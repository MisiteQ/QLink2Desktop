#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""QLink2Desktop —— 飞牛统一网关等效端到端验证。

为什么需要这个脚本
------------------
QLink2Desktop 在真机上无法访问时，最典型的失败表现是「页面卡在『正在加载界面…』」。
这类问题在本机用 `curl localhost:5900/` 是**测不出来**的，因为飞牛统一网关
并不是简单的反向代理，它有三个会改变请求语义的行为：

  1. 请求会被加上 `/app/<appname>` 前缀转发（部分版本转发时**带上**前缀，
     部分版本**剥掉**前缀，两种都要兼容）；
  2. `ui/config` 里 url 字段的尾斜杠会被吃掉，iframe 文档 URL 变成
     `/app/qlink2desktop`（无斜杠）——此时前端所有相对路径都会以 `/app/`
     为基准解析，全部 404；
  3. 并发转发不可靠（偶发挂住），所以首屏必须收敛成单个请求。

本脚本用两个 HTTP 代理精确复刻上述 1 的两种模式，然后逐条断言前端在
"网关之后"能否正常跑起来：入口重定向、文档、每个静态资源、递归 import
图、首屏接口、鉴权、深链接回退。

用法
----
    python scripts/e2e-gateway-check.py [--exe .build/dev/qlink2desktop.exe]

退出码 0 表示全部通过。
"""

from __future__ import annotations

import argparse
import http.client
import json
import os
import re
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

APP_SLUG = "qlink2desktop"
GATEWAY_PREFIX = f"/app/{APP_SLUG}"

# ---------------------------------------------------------------------------
# 断言与统计
# ---------------------------------------------------------------------------

PASSED: list[str] = []
FAILED: list[str] = []


def check(name: str, ok: bool, detail: str = "") -> bool:
    if ok:
        PASSED.append(name)
        print(f"  \033[32mPASS\033[0m  {name}")
    else:
        FAILED.append(f"{name} :: {detail}")
        print(f"  \033[31mFAIL\033[0m  {name}\n        {detail}")
    return ok


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


# ---------------------------------------------------------------------------
# 网关模拟器
# ---------------------------------------------------------------------------

class GatewaySimulator:
    """复刻飞牛统一网关的路径改写行为。

    strip_prefix=True  —— 网关剥掉 /app/<app> 前缀后再转发（本项目主路径）；
    strip_prefix=False —— 网关把前缀一并转发，后端需要自己 StripAppPrefix。
    两种都必须能跑通，因为不同飞牛版本行为不一致。
    """

    def __init__(self, backend_port: int, strip_prefix: bool):
        self.backend_port = backend_port
        self.strip_prefix = strip_prefix
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args):  # 静音
                pass

            def _proxy(self, method: str) -> None:
                path = self.path
                if outer.strip_prefix:
                    if path == GATEWAY_PREFIX:
                        # 真机网关会吃掉配置里的尾斜杠，文档 URL 变成无斜杠形态。
                        path = "/"
                    elif path.startswith(GATEWAY_PREFIX + "/"):
                        path = path[len(GATEWAY_PREFIX):]
                    elif path.startswith(GATEWAY_PREFIX):
                        path = "/" + path[len(GATEWAY_PREFIX):].lstrip("/")

                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length) if length else None

                # 跳过后端不认识的 hop-by-hop 头。
                headers = {
                    k: v
                    for k, v in self.headers.items()
                    if k.lower() not in ("host", "connection", "accept-encoding")
                }
                headers["Host"] = f"127.0.0.1:{outer.backend_port}"

                conn = http.client.HTTPConnection("127.0.0.1", outer.backend_port, timeout=30)
                try:
                    conn.request(method, path, body=body, headers=headers)
                    resp = conn.getresponse()
                    payload = resp.read()
                except Exception as exc:  # noqa: BLE001
                    self.send_error(502, f"gateway: {exc}")
                    return
                finally:
                    conn.close()

                self.send_response(resp.status)
                for k, v in resp.getheaders():
                    if k.lower() in ("connection", "transfer-encoding", "content-length"):
                        continue
                    self.send_header(k, v)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                if method != "HEAD":
                    self.wfile.write(payload)

            def do_GET(self):  # noqa: N802
                self._proxy("GET")

            def do_HEAD(self):  # noqa: N802
                self._proxy("HEAD")

            def do_POST(self):  # noqa: N802
                self._proxy("POST")

            def do_PUT(self):  # noqa: N802
                self._proxy("PUT")

            def do_DELETE(self):  # noqa: N802
                self._proxy("DELETE")

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.port = self.server.server_address[1]

    def start(self) -> None:
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()


# ---------------------------------------------------------------------------
# HTTP 客户端 + import 图爬取
# ---------------------------------------------------------------------------

def fetch(
    port: int,
    path: str,
    method: str = "GET",
    accept: str | None = None,
    body: bytes | None = None,
    content_type: str | None = None,
    cookie: str | None = None,
):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        headers: dict[str, str] = {}
        if accept:
            headers["Accept"] = accept
        if content_type:
            headers["Content-Type"] = content_type
        if cookie:
            headers["Cookie"] = cookie
        conn.request(method, path, body=body, headers=headers)
        resp = conn.getresponse()
        payload = resp.read()
        return resp.status, dict(resp.getheaders()), payload
    finally:
        conn.close()


def fetch_follow(port: int, path: str, accept: str = "text/html", max_hops: int = 5):
    """跟随重定向地取一个页面，尽量贴近浏览器行为。"""
    status, headers, body = fetch(port, path, accept=accept)
    hops = 0
    while status in (301, 302, 303, 307, 308) and hops < max_hops:
        loc = headers.get("Location", "")
        if not loc:
            break
        path = loc
        status, headers, body = fetch(port, path, accept=accept)
        hops += 1
    return status, headers, body, path


IMPORT_RE = re.compile(
    r"""(?:^|[^\w$])(?:import|export)\b[^;\n]*?from\s*['"]([^'"]+)['"]"""
    r"""|(?:^|[^\w$])import\s*\(\s*['"]([^'"]+)['"]\s*\)""",
    re.MULTILINE,
)


def crawl_module_graph(port: int, entry_url: str) -> tuple[int, list[str]]:
    """从入口模块出发递归拉取全部相对 import，返回 (拉取数量, 失败列表)。

    真机上「首页能打开但交互全无」的典型原因是某条 import 路径写错
    （例如 app.js 里写 './dom.js' 而文件其实在 './modules/dom.js'）。
    单看 HTTP 状态码发现不了——只有把整张图爬下来逐个请求才会暴露。
    """
    seen: set[str] = set()
    queue = [entry_url]
    failures: list[str] = []

    while queue:
        url = queue.pop()
        if url in seen:
            continue
        seen.add(url)

        parsed = urllib.parse.urlsplit(url)
        status, _headers, body = fetch(port, parsed.path + (("?" + parsed.query) if parsed.query else ""))
        if status != 200:
            failures.append(f"{url} → HTTP {status}")
            continue

        # 只爬 JS 模块。
        if not parsed.path.endswith(".js"):
            continue

        text = body.decode("utf-8", "replace")
        for match in IMPORT_RE.finditer(text):
            spec = match.group(1) or match.group(2)
            if not spec or not (spec.startswith(".") or spec.startswith("/")):
                continue  # 裸模块名（本项目不用打包器，不应出现）
            nxt = urllib.parse.urljoin(url, spec)
            if nxt not in seen:
                queue.append(nxt)

    return len(seen), failures


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------

def wait_backend(port: int, timeout: float = 20.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            status, _h, _b = fetch(port, "/api/health")
            if status == 200:
                return True
        except Exception:  # noqa: BLE001
            pass
        time.sleep(0.25)
    return False


def run_checks(label: str, gw: GatewaySimulator, forwards_prefix: bool) -> None:
    """在某一台模拟网关之后跑完整断言。

    forwards_prefix=True  —— 网关把 /app/<name> 一并转发，后端能自己 302 补斜杠；
    forwards_prefix=False —— 网关剥掉前缀，后端只看到 "/"，
                            只能靠前端守卫脚本自我纠正。
    """
    print(f"\n\033[1m== 网关模式：{label} ==\033[0m")
    port = gw.port
    base = GATEWAY_PREFIX

    # --- 1. 入口尾斜杠 ----------------------------------------------------
    status, headers, body = fetch(port, base, accept="text/html")
    if forwards_prefix:
        loc = headers.get("Location", "")
        check(
            "入口无斜杠 → 302 到目录形态（后端兜底）",
            status == 302 and loc.endswith(base + "/"),
            f"status={status} location={loc!r}",
        )
    else:
        html_entry = body.decode("utf-8", "replace")
        check(
            "入口无斜杠 → 后端只能返回文档，由前端守卫自我纠正",
            status == 200 and "location.replace" in html_entry,
            f"status={status}（文档里必须带尾斜杠守卫）",
        )

    # --- 2. 文档本体 ----------------------------------------------------
    status, headers, body = fetch(port, base + "/")
    html = body.decode("utf-8", "replace")
    check(
        "文档 /app/qlink2desktop/ → 200 text/html",
        status == 200 and "text/html" in headers.get("Content-Type", ""),
        f"status={status} ctype={headers.get('Content-Type')!r}",
    )
    check(
        "文档为 SPA 外壳（含 app.js 与挂载点）",
        'src="assets/app.js' in html and 'id="app"' in html,
        "index.html 内容不符合预期",
    )
    check(
        "文档不引用根绝对路径（网关前缀下会 404）",
        not re.search(r'(?:src|href)="/(?!/)', html),
        "检测到 src=/... 或 href=/... 形式的根绝对路径",
    )

    # --- 3. 关键静态资源 -------------------------------------------------
    for path, want_ctype in (
        ("/assets/app.js", "javascript"),
        ("/assets/style.css", "text/css"),
        ("/icons/default-64.png", "image/png"),
    ):
        status, headers, _ = fetch(port, base + path)
        check(
            f"静态资源 {path} → 200",
            status == 200 and want_ctype in headers.get("Content-Type", ""),
            f"status={status} ctype={headers.get('Content-Type')!r}",
        )

    # --- 4. 递归 import 图 ----------------------------------------------
    count, failures = crawl_module_graph(port, base + "/assets/app.js")
    check(
        f"ES 模块 import 图完整（共 {count} 个模块）",
        not failures,
        "; ".join(failures[:8]),
    )

    # --- 5. 首屏单请求 ---------------------------------------------------
    status, _headers, body = fetch(port, base + "/api/bootstrap")
    payload = {}
    try:
        payload = json.loads(body.decode("utf-8"))
    except Exception:  # noqa: BLE001
        pass
    version = payload.get("system", {}).get("version") if isinstance(payload, dict) else None
    check(
        "GET /api/bootstrap → 200 JSON（首屏单请求）",
        status == 200 and bool(version),
        f"status={status} system.version={version!r} body={body[:160]!r}",
    )
    check(
        "bootstrap 携带 links / settings / system 三块数据",
        all(k in payload for k in ("links", "settings", "system")),
        f"keys={sorted(payload.keys()) if isinstance(payload, dict) else payload!r}",
    )
    check(
        "bootstrap 不泄露口令摘要与盐",
        "auth_hash" not in body.decode("utf-8", "replace")
        and "auth_salt" not in body.decode("utf-8", "replace"),
        "响应体里出现了 auth_hash / auth_salt 字段",
    )

    # --- 6. 未知接口 -----------------------------------------------------
    status, headers, _ = fetch(port, base + "/api/does-not-exist")
    check(
        "未知 API → 404 JSON（不是 SPA HTML）",
        status == 404 and "json" in headers.get("Content-Type", ""),
        f"status={status} ctype={headers.get('Content-Type')!r}",
    )

    # --- 7. 前端深链接回退（只对浏览器导航生效） --------------------------
    status, headers, _ = fetch(port, base + "/some/deep/route", accept="text/html")
    check(
        "浏览器深链接 → 200 HTML（SPA 回退）",
        status == 200 and "text/html" in headers.get("Content-Type", ""),
        f"status={status} ctype={headers.get('Content-Type')!r}",
    )
    status, headers, _ = fetch(port, base + "/some/deep/route", accept="application/json")
    check(
        "非浏览器请求深链接 → 404 JSON",
        status == 404 and "json" in headers.get("Content-Type", ""),
        f"status={status} ctype={headers.get('Content-Type')!r}",
    )

    # --- 8. 尾斜杠自愈（真机白屏的头号成因） ------------------------------
    # 浏览器在无斜杠文档下会把 assets/app.js 解析成 /app/assets/app.js。
    # 这个路径不匹配网关前缀，必然 404 —— 这正是「卡在正在加载界面」的机理。
    status, _h, _b = fetch(port, "/app/assets/app.js")
    check(
        "无斜杠基准下的错误资源路径确实 404（证明守卫是必需的）",
        status == 404,
        f"status={status}（若为 200，说明本断言的前提已变化，请复核）",
    )
    status, _headers, body, final_path = fetch_follow(port, base)
    html_noslash = body.decode("utf-8", "replace")
    if forwards_prefix:
        check(
            "跟随重定向后拿到文档，且最终 URL 以斜杠结尾",
            status == 200 and final_path.endswith("/"),
            f"status={status} final_path={final_path!r}",
        )
    else:
        # 网关剥掉前缀时后端只看到 "/"，无从得知对外前缀，
        # 因此这里看到无斜杠的最终 URL 是正常的 —— 由下面的前端守卫接手。
        check(
            "剥前缀模式下文档可正常送达（尾斜杠交给前端守卫）",
            status == 200 and bool(html_noslash),
            f"status={status} final_path={final_path!r}",
        )
    # 守卫的主手段是**同步注入 <base>**：location.replace 是异步的，
    # 解析器会继续把后面那批相对资源按旧基准发出去，先炸一串 404 再跳转回来，
    # 控制台上看起来就像应用坏了。保留 location.replace 作为取不到 head 时的兜底。
    check(
        "文档内置相对路径基准守卫（无斜杠时自我修正）",
        "location.pathname" in html_noslash
        and "createElement('base')" in html_noslash
        and "location.replace" in html_noslash,
        "index.html 里找不到基准守卫，真机将白屏",
    )
    # 注意用「带属性的真实引用」定位，不能只搜文件名：
    # 守卫脚本自己的注释里就提到了 assets/app.js，朴素查找会命中注释而误判。
    guard_pos = html_noslash.find("createElement('base')")
    ref_positions = [p for p in (
        html_noslash.find('href="assets/style.css'),
        html_noslash.find('src="assets/app.js'),
    ) if p >= 0]
    check(
        "守卫脚本位于任何相对资源之前",
        bool(ref_positions) and all(guard_pos < p for p in ref_positions),
        f"guard={guard_pos} refs={ref_positions}",
    )
    check(
        "资源版本占位符已被后端替换（无 __V__ 残留）",
        "__V__" not in html_noslash,
        "index.html 里的 __V__ 没被替换成版本号",
    )
    m = re.search(r'href="assets/style\.css\?v=([^"]+)"', html_noslash)
    check(
        "静态资源 URL 带版本号（升级后不会命中旧缓存）",
        bool(m) and m.group(1) not in ("", "__V__"),
        f"匹配结果={m.group(0) if m else None!r}",
    )

    # --- 8.5 启动期自诊断与缓存策略 ---------------------------------------
    #
    # 这两组断言对应一次真机事故：新包装上后，浏览器仍在跑上一版 app.js
    # （它按旧布局 import ./dom.js），所有模块 404，界面停在启动页，
    # 而服务端一切正常 —— 排查方向被彻底带偏。
    #   ① 缓存策略必须让"新版本一定跑新资源"；
    #   ② 万一带不到新资源，页面必须自己把证据摆出来。
    check(
        "启动页内置看门狗（app.js 加载失败时也能自诊断）",
        "__qlinkDiag" in html_noslash
        and "boot-detail" in html_noslash
        and "boot-retry" in html_noslash,
        "index.html 里找不到看门狗，模块加载失败时页面只会一直转圈",
    )
    m = re.search(r'<p id="boot-version"[^>]*>v([^<]+)</p>', html_noslash)
    check(
        "启动页常显版本号（截图即可自证装的是哪一版）",
        bool(m) and m.group(1) not in ("", "__V__"),
        f"匹配结果={m.group(0) if m else None!r}",
    )
    status, headers, _b = fetch(port, base + "/")
    cc = headers.get("Cache-Control", "")
    check(
        "入口文档 no-store（升级后不会被旧 HTML 钉住）",
        status == 200 and "no-store" in cc,
        f"status={status} cache-control={cc!r}",
    )
    status, headers, _b = fetch(port, base + "/assets/app.js")
    cc = headers.get("Cache-Control", "")
    check(
        "静态资源 no-store（升级后不会复用旧 JS）",
        status == 200 and "no-store" in cc,
        f"status={status} cache-control={cc!r}",
    )

    # --- 8.5 层叠陷阱：hidden 属性必须真的能隐藏元素 ---------------------
    #
    # [hidden] { display: none } 来自浏览器**默认**样式表，作者样式里任何一条
    # 设置 display 的规则都会把它压过去。真机事故就出在这里：应用其实已经
    # 正常启动，但 .boot{display:flex} 让 boot.hidden = true 变成空操作，
    # 启动遮罩永远盖在应用上面 —— 用户看到的就是"一直打不开"，
    # 而服务端、前端逻辑、控制台全都查不出毛病（控制台是干净的）。
    # 这种缺陷只能在计算样式层面暴露，所以必须有人在发版前守住这条规则。
    status, _h, css_body = fetch(port, base + "/assets/style.css")
    # 先剥掉注释再匹配：CSS 里解释这条规则的注释同样写着
    # "[hidden] { display: none }"，不剥离的话规则被删掉了也照样"通过"。
    css_code = re.sub(r"/\*.*?\*/", "", css_body.decode("utf-8", "replace"), flags=re.S)
    css_flat = re.sub(r"\s+", "", css_code)
    check(
        "样式表含 [hidden] 兜底规则（否则 el.hidden=true 是空操作）",
        status == 200 and "[hidden]{display:none" in css_flat,
        f"status={status} —— 样式表里找不到 [hidden]{{display:none}} 兜底规则",
    )

    # --- 9. 鉴权边界（放在最后，因为下面会给应用设上口令） ---------------
    status, _h, _b = fetch(port, base + "/api/links")
    check("未设口令时 /api/links 放行（家用 NAS 的默认姿态）", status == 200, f"status={status}")

    pw = json.dumps({"password": "qlink2d-e2e-pass"}).encode()
    status, _h, body = fetch(port, base + "/api/settings", method="POST", body=pw, content_type="application/json")
    check(
        "设置口令成功",
        status == 200,
        f"status={status} body={body[:200]!r}",
    )

    status, _h, _b = fetch(port, base + "/api/links")
    check("设口令后未登录 /api/links → 401", status == 401, f"status={status}")
    status, _h, _b = fetch(port, base + "/api/bootstrap")
    check("设口令后未登录 /api/bootstrap → 401", status == 401, f"status={status}")

    login = json.dumps({"password": "qlink2d-e2e-pass"}).encode()
    status, headers, body = fetch(port, base + "/api/auth/login", method="POST", body=login, content_type="application/json")
    cookie = headers.get("Set-Cookie", "").split(";")[0]
    check(
        "登录成功并下发会话 Cookie",
        status == 200 and cookie.startswith("qlink_token="),
        f"status={status} set-cookie={headers.get('Set-Cookie')!r}",
    )
    status, _h, _b = fetch(port, base + "/api/links")
    check("仍不带 Cookie 时依然 401", status == 401, f"status={status}")
    status, _h, body = fetch(port, base + "/api/links", cookie=cookie)
    check(
        "带上会话 Cookie 后放行",
        status == 200 and b'"items"' in body,
        f"status={status} body={body[:160]!r}",
    )

    # 复原：清掉口令，让下一种网关模式的断言从同一个初始状态出发。
    clear = json.dumps({"clear_password": True, "current_password": "qlink2d-e2e-pass"}).encode()
    status, _h, body = fetch(
        port, base + "/api/settings", method="POST", body=clear, content_type="application/json", cookie=cookie
    )
    check("清除口令（复原初始状态）", status == 200, f"status={status} body={body[:200]!r}")
    status, _h, _b = fetch(port, base + "/api/links")
    check("清口令后恢复放行", status == 200, f"status={status}")

    # ---- 图标地址必须是相对路径 -------------------------------------------
    #
    # 真机上图标库整片裂图，根因就是这里：/api/icons 返回了 "/icons/x.png"，
    # 而应用挂在统一网关的 /app/qlink2desktop/ 之下，根绝对路径会解析到
    # NAS 根目录，必然 404。桌面列表里的缩略图走的是相对路径，所以当时
    # 只有图标库是坏的——这种"一半好一半坏"的现象最容易把人带偏。
    status, _h, body = fetch(port, base + "/api/icons")
    rel_ok, detail = True, ""
    if status == 200:
        try:
            items = json.loads(body).get("items") or []
        except Exception as exc:  # noqa: BLE001
            items, rel_ok, detail = [], False, f"响应不是合法 JSON: {exc}"
        for it in items:
            url = it.get("url") or ""
            if url.startswith("/") or url.startswith("http"):
                rel_ok = False
                detail = f"{it.get('name')} 的 url = {url!r}（应形如 icons/xxx.png）"
                break
        else:
            detail = f"{len(items)} 个图标，地址均为相对路径"
    else:
        rel_ok, detail = False, f"status={status}"
    check("图标库返回的是相对地址（网关前缀下才不裂图）", rel_ok, detail)

    # ---- 图标目录可配置 ---------------------------------------------------
    #
    # 图标目录是用户在设置页指定的，因此**运行时**会变。这里断言两件最容易
    # 出错的事：非法路径必须被拒绝（不能写进去一个用不了的目录），
    # 以及合法的改动要真正生效（曾经的问题是接口层与 fnos 层各记一份目录，
    # 改完只有一半生效）。
    bad = json.dumps({"icons_dir": "relative/icons"}).encode()
    status, _h, body = fetch(port, base + "/api/settings", method="POST",
                             body=bad, content_type="application/json")
    check("相对路径的图标目录被拒绝（400）", status == 400, f"status={status} body={body[:160]!r}")

    icons_dir = os.path.join(tempfile.gettempdir(), f"qlink2d-e2e-icons-{os.getpid()}")
    good = json.dumps({"icons_dir": icons_dir}).encode()
    status, _h, body = fetch(port, base + "/api/settings", method="POST",
                             body=good, content_type="application/json")
    check("绝对路径的图标目录被接受", status == 200, f"status={status} body={body[:200]!r}")

    def reported_icons_dir() -> str:
        """读回后端当前生效的图标目录。

        必须解析 JSON 再比：Windows 路径里的反斜杠在 JSON 里是转义的
        （C:\\Users\\...），按原始字节找子串会永远找不到，那种断言
        在失败时看起来像功能坏了，其实是断言本身在骗人。
        """
        st, _hd, bd = fetch(port, base + "/api/system")
        if st != 200:
            return f"<status {st}>"
        try:
            return json.loads(bd).get("icons_dir") or ""
        except Exception as exc:  # noqa: BLE001
            return f"<parse error: {exc}>"

    check("系统信息回报的是新目录（改动真的生效了）",
          os.path.normcase(reported_icons_dir()) == os.path.normcase(icons_dir),
          f"期望 {icons_dir!r}，实际 {reported_icons_dir()!r}")

    # 复原：留空表示跟随应用数据目录。留着改过的值会让下一次运行从脏状态出发。
    restore = json.dumps({"icons_dir": ""}).encode()
    status, _h, body = fetch(port, base + "/api/settings", method="POST",
                             body=restore, content_type="application/json")
    check("留空可恢复默认图标目录", status == 200, f"status={status} body={body[:200]!r}")
    check("恢复后不再指向临时目录",
          os.path.normcase(reported_icons_dir()) != os.path.normcase(icons_dir),
          f"实际仍为 {reported_icons_dir()!r}")
    import shutil as _shutil

    _shutil.rmtree(icons_dir, ignore_errors=True)

    # ---- 异步受理：写操作不得在请求路径里等 appcenter-cli -------------------
    #
    # 真机事故：用户在面板上点删除，15 秒后弹出「请求超时」，但服务端其实
    # 还在正常跑 appcenter-cli（单条命令最长 3 分钟，删除含 stop+uninstall
    # 最坏 6 分钟）。那条报错是假的——真正的问题在响应模型：
    # 把分钟级的外部命令放进了秒级的请求路径。
    #
    # 这组断言守的是契约本身：写操作**受理即返回**，
    # 落盘的部分同步做完，碰 appcenter-cli 的部分交给后台队列。
    link_payload = json.dumps({
        "name": "E2E 异步受理探针",
        "kind": "shortcut",
        "path": "https://example.com",
        "ui": "iframe",
    }).encode()
    t0 = time.time()
    status, _h, body = fetch(port, base + "/api/links", method="POST",
                             body=link_payload, content_type="application/json")
    elapsed = time.time() - t0
    try:
        created = json.loads(body)
    except Exception:  # noqa: BLE001
        created = {}
    link_id = (created.get("link") or {}).get("id") or ""
    check(
        "新建链接 → 200 且立刻返回（不在请求里等注册）",
        status == 200 and bool(link_id) and elapsed < 10,
        f"status={status} elapsed={elapsed:.2f}s id={link_id!r} body={body[:160]!r}",
    )
    phase = (created.get("status") or {}).get("phase")
    check(
        "新建后立刻带上阶段（用户看得到进度，而不是一片空白）",
        phase in ("pending", "installing", "installed", "failed"),
        f"status.phase={phase!r}",
    )

    def accepted_body(path: str) -> tuple[int, dict]:
        st, _hd, bd = fetch(port, base + path, method="POST",
                            body=b"{}", content_type="application/json")
        try:
            return st, json.loads(bd)
        except Exception:  # noqa: BLE001
            return st, {}

    if link_id:
        enc = urllib.parse.quote(link_id, safe="")
        st, acc = accepted_body(f"/api/links/{enc}/sync")
        check(
            "重新同步 → 202 已受理（不阻塞到安装完成）",
            st == 202 and acc.get("accepted") is True,
            f"status={st} body={acc!r}",
        )

        st, acc = accepted_body("/api/system/reconcile")
        check(
            "立即对账 → 202 已受理（不再同步返回全量结果）",
            st == 202 and acc.get("accepted") is True,
            f"status={st} body={acc!r}",
        )

        st, acc = accepted_body("/api/system/cleanup-orphans")
        check(
            "清理遗留图标 → 202 已受理（卸载搬到后台执行）",
            st == 202 and acc.get("accepted") is True,
            f"status={st} body={acc!r}",
        )

        st, _hd, bd = fetch(port, base + "/api/system")
        try:
            sysinfo = json.loads(bd)
        except Exception:  # noqa: BLE001
            sysinfo = {}
        check(
            "系统信息暴露后台队列深度（让「排队中」可被观测）",
            st == 200 and "pending_jobs" in sysinfo,
            f"status={st} 缺少 pending_jobs 字段",
        )

        t0 = time.time()
        st, _hd, bd = fetch(port, base + f"/api/links/{enc}", method="DELETE")
        del_elapsed = time.time() - t0
        check(
            "删除链接 → 200 且立刻返回（不等图标注销）",
            st == 200 and del_elapsed < 10,
            f"status={st} elapsed={del_elapsed:.2f}s body={bd[:160]!r}",
        )
        st, _hd, bd = fetch(port, base + "/api/links")
        try:
            ids = [(it.get("link") or {}).get("id") for it in json.loads(bd).get("items") or []]
        except Exception:  # noqa: BLE001
            ids = []
        check(
            "删除后链接立刻从列表消失（落盘与注销解耦）",
            st == 200 and link_id not in ids,
            f"status={st} 仍存在={link_id in ids}",
        )

    # ---- 局域网扫描：必须是后台任务，而不是一个同步接口 -----------------------
    #
    # 真机形态：一个 /24 网段 × 46 个常见端口 = 上万个 TCP 连接，十几秒起步，
    # 必然撞上前端 15 秒的请求预算。用户看到的是「请求超时」，而扫描其实还在跑、
    # 结果永远回不来。这里断言的是契约本身：
    #   POST 受理即返回（202 + 可轮询的句柄）→ GET 读进度 → DELETE 可取消，
    #   范围非法时给 400 明确拒绝，而不是悄悄只扫一部分。
    status, _h, body = fetch(port, base + "/api/discovery/subnets")
    try:
        net = json.loads(body)
    except Exception:  # noqa: BLE001
        net = {}
    check(
        "GET /api/discovery/subnets → 200 且带网段信息",
        status == 200 and all(k in net for k in ("primary", "subnets", "parent", "limit")),
        f"status={status} keys={sorted(net) if isinstance(net, dict) else body[:160]!r}",
    )
    # 虚拟网卡（docker0 / br-xxxx）绝不能当成本机网段：扫容器网络等于一台
    # 真实设备都扫不到 —— 这正是「扫描有结果但全是空的」的由来。
    docker_bridge = ("172.17.", "172.18.", "172.19.", "172.20.")
    subs = [str(s) for s in (net.get("subnets") or [])]
    real = [s for s in subs if not s.startswith(docker_bridge)]
    check(
        "存在实体网卡网段时，首选网段不是 Docker/桥接虚拟网段",
        (not real) or ((net.get("primary") or "") in real),
        f"primary={net.get('primary')!r} subnets={subs}",
    )
    check(
        "上级网段是字符串（为空是事实、不是失败；为空时前端要能解释清楚）",
        isinstance(net.get("parent"), str),
        f"parent={net.get('parent')!r}",
    )
    check(
        "网段接口同时给出常见端口表与探活端口表（前端据此解释扫描会做什么）",
        status == 200
        and isinstance(net.get("ports"), list)
        and isinstance(net.get("probe_ports"), list)
        and len(net.get("ports") or []) > 0,
        f"ports={net.get('ports')!r} probe_ports={net.get('probe_ports')!r}",
    )

    # 只扫 127.0.0.1 一台：无论如何都跑得快，也不会碰到真实网络。
    t0 = time.time()
    status, _h, body = fetch(
        port, base + "/api/discovery/scan", method="POST",
        body=json.dumps({"mode": "custom", "spec": "127.0.0.1"}).encode(),
        content_type="application/json",
    )
    started = time.time() - t0
    try:
        job = json.loads(body)
    except Exception:  # noqa: BLE001
        job = {}
    check(
        "POST /api/discovery/scan → 202 且立刻返回（不在请求里等扫描）",
        status == 202 and started < 10 and bool(job.get("id")),
        f"status={status} elapsed={started:.2f}s id={job.get('id')!r} body={body[:200]!r}",
    )
    check(
        "扫描受理回执带范围与目标数（用户立刻知道扫的是什么）",
        bool(job.get("range")) and job.get("hosts_total") == 1,
        f"range={job.get('range')!r} hosts_total={job.get('hosts_total')!r}",
    )

    # 轮询到落终态：这一条同时证明 GET 能读到进度，且任务真的会自己结束。
    final: dict = {}
    deadline = time.time() + 25
    while time.time() < deadline:
        st, _hd, bd = fetch(port, base + "/api/discovery/scan")
        if st != 200:
            break
        try:
            final = json.loads(bd)
        except Exception:  # noqa: BLE001
            final = {}
            break
        if not final.get("running"):
            break
        time.sleep(0.3)
    check(
        "GET /api/discovery/scan → 200 且最终落终态（进度可被轮询）",
        bool(final.get("id")) and final.get("running") is False and final.get("done") is True,
        f"id={final.get('id')!r} running={final.get('running')!r} done={final.get('done')!r} "
        f"canceled={final.get('canceled')!r} found={final.get('found')!r}",
    )
    check(
        "终态带耗时与结果集合（前端据此渲染结果表）",
        "elapsed_ms" in final and isinstance(final.get("items"), list),
        f"keys={sorted(final)[:14]}",
    )

    st, _hd, bd = fetch(port, base + "/api/discovery/scan", method="DELETE")
    check(
        "DELETE /api/discovery/scan → 200（取消是幂等的，没任务在跑也不报错）",
        st == 200,
        f"status={st} body={bd[:160]!r}",
    )

    for label, payload in (
        ("范围为空", {"mode": "custom", "spec": ""}),
        ("范围超过上限", {"mode": "custom", "spec": "10.0.0.0/16"}),
    ):
        st, _hd, bd = fetch(
            port, base + "/api/discovery/scan", method="POST",
            body=json.dumps(payload).encode(), content_type="application/json",
        )
        check(
            f"{label} → 400（明确拒绝，而不是悄悄只扫一部分）",
            st == 400,
            f"status={st} body={bd[:200]!r}",
        )

    # ---- 帮助页资源可达 ---------------------------------------------------
    #
    # 说明写进界面里才有机会被看到，而它首先得能被取到：
    # 少一个 import、写错一个相对路径，用户点开的就是一片空白。
    status, headers, _b = fetch(port, base + "/assets/modules/views/help.js")
    check(
        "使用帮助模块可通过网关取到（说明写在界面里，必须先取得到）",
        status == 200 and "javascript" in headers.get("Content-Type", ""),
        f"status={status} ctype={headers.get('Content-Type')!r}",
    )


# ---------------------------------------------------------------------------
# 静态守卫：禁止把根绝对路径拼进资源地址
# ---------------------------------------------------------------------------

# 只盯「拼接出 URL」这一种写法：
#   URL: "/icons/" + name      ← 浏览器侧的基准，必错
#
# 路由注册（HandleFunc("GET /icons/{name}")）与鉴权白名单
# （HasPrefix(p, "/icons/")）用的是合法的绝对形式 —— 它们面对的是网关
# **剥掉前缀之后**的路径，和浏览器解析相对地址的基准不是同一回事。
# 正则要求引号后紧跟加号，天然把这两类放过去。
ROOT_ABS_PATTERNS = (
    re.compile(r'"/icons/"\s*\+'),
    re.compile(r"'/icons/'\s*\+"),
    re.compile(r'"/assets/"\s*\+'),
    re.compile(r"'/assets/'\s*\+"),
)
SOURCE_SUFFIXES = (".go", ".js", ".mjs", ".html")
SKIP_DIRS = {"node_modules", ".git", ".build", "vendor", "__pycache__", "data"}


def read_text(path: str) -> str:
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            return fh.read()
    except OSError:
        return ""


def extract_fn_body(src: str, signature: str) -> str:
    """截取某个函数的函数体（从签名到它自己的收尾花括号）。

    用于把断言限定在**一个函数内部**：整文件搜 "s.cli.Stop(" 会命中
    PruneOrphans / Uninstall 这些完全合法的调用，那样守卫就形同虚设 ——
    它要守的正是"RestartSelf 自己不许直接 stop"。
    """
    start = src.find(signature)
    if start < 0:
        return ""
    brace = src.find("{", start)
    if brace < 0:
        return ""
    depth = 0
    for i in range(brace, len(src)):
        if src[i] == "{":
            depth += 1
        elif src[i] == "}":
            depth -= 1
            if depth == 0:
                return src[brace : i + 1]
    return ""


def find_root_absolute_assets(root: str) -> list[str]:
    """返回所有把根绝对路径拼接成资源地址的位置。"""
    offenders: list[str] = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            if not name.endswith(SOURCE_SUFFIXES):
                continue
            path = os.path.join(dirpath, name)
            try:
                with open(path, "r", encoding="utf-8", errors="replace") as fh:
                    lines = fh.read().splitlines()
            except OSError:
                continue
            for no, line in enumerate(lines, 1):
                if any(p.search(line) for p in ROOT_ABS_PATTERNS):
                    offenders.append(f"{os.path.relpath(path, root)}:{no}: {line.strip()}")
    return offenders


def main() -> int:
    parser = argparse.ArgumentParser(description="QLink2Desktop 飞牛网关等效端到端验证")
    parser.add_argument("--exe", default=".build/dev/qlink2desktop.exe", help="后端可执行文件路径")
    parser.add_argument("--keep", action="store_true", help="结束后保留临时数据目录，便于排查")
    args = parser.parse_args()

    exe = os.path.abspath(args.exe)
    if not os.path.isfile(exe):
        print(f"!! 找不到可执行文件：{exe}\n   先运行：go build -o {args.exe} ./cmd/server", file=sys.stderr)
        return 2

    print(f"\033[1mQLink2Desktop 网关等效端到端验证\033[0m\nexe = {exe}")

    repo_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    offenders = find_root_absolute_assets(repo_root)
    check(
        "源码里没有把根绝对路径拼成资源地址（/icons/、/assets/）",
        not offenders,
        "；".join(offenders[:6]),
    )

    # 前端超时语义的静态守卫。
    #
    # 后端改成受理即返回之后，前端还有一半责任：写操作要有**独立的**超时预算，
    # 且超时后必须回查真实状态，不能把「可能已成功」报成硬失败。
    # 这两条都是纯前端行为，运行时断言抓不到，只能靠静态守卫。
    api_js = os.path.join(repo_root, "internal", "webui", "web", "assets", "modules", "api.js")
    api_src = read_text(api_js)
    check(
        "写操作有独立的超时预算（不再与只读请求共用 15 秒）",
        "MUTATION_TIMEOUT_MS" in api_src and "isTimeout" in api_src,
        "api.js 缺少 MUTATION_TIMEOUT_MS / isTimeout",
    )
    for name, rel in (
        ("删除超时后回查列表", os.path.join("views", "desktop.js")),
        ("保存超时后回查列表", os.path.join("views", "editor.js")),
        ("对账/清理超时后如实提示", os.path.join("views", "settings.js")),
    ):
        src = read_text(os.path.join(repo_root, "internal", "webui", "web", "assets", "modules", rel))
        check(
            name,
            "isTimeout" in src,
            f"{rel} 里没有 isTimeout 分支（超时会被当成硬失败甩给用户）",
        )

    # 前端扫描交互的静态守卫：后端异步化只完成了一半，
    # 前端必须真的走「受理 → 轮询 → 可取消」，否则用户点下去还是等一个
    # 永远不返回的请求。这类契约运行时断言抓不到（要看 DOM 才知道），
    # 静态守卫至少能守住"三个方法都在用、三种范围都在界面上"。
    web_root = os.path.join(repo_root, "internal", "webui", "web", "assets")
    disc_src = read_text(os.path.join(web_root, "modules", "views", "discovery.js"))
    check(
        "扫描前端已异步化（受理 / 轮询 / 取消三件事都在用）",
        all(s in disc_src for s in ("api.discovery.scan(", "api.discovery.scanStatus()", "api.discovery.cancelScan()")),
        "discovery.js 仍在等一个同步响应，真机上必然超时",
    )
    check(
        "扫描目标给出本机网段 / 上级网段 / 自定义范围三种选择",
        all(s in disc_src for s in ("本机网段", "上级网段", "自定义范围")),
        "扫描界面没有可选择的范围，用户只能盲扫",
    )
    check(
        "扫描状态存在全局 state 里（重绘不丢进度）",
        "state.scan" in disc_src,
        "扫描进度若留在视图闭包里，任何一次重绘都会清空结果",
    )

    # 统一帮助页的静态守卫：说明必须在侧边导航里找得到，
    # 否则它只是一份没人会打开的文档。
    app_src = read_text(os.path.join(web_root, "app.js"))
    check(
        "侧边导航含统一「使用帮助」入口",
        "views/help.js" in app_src and "使用帮助" in app_src and "key: 'help'" in app_src,
        "app.js 里找不到 help 视图的注册（说明页没人能打开）",
    )
    check(
        "帮助页覆盖远程主机与局域网扫描（用户最容易困惑的两块）",
        all(s in read_text(os.path.join(web_root, "modules", "views", "help.js"))
            for s in ("远程主机", "局域网扫描", "网址快捷方式")),
        "help.js 缺少远程主机 / 局域网扫描 / 快捷方式的说明",
    )
    check(
        "帮助页解释了外观主题与重启服务（新功能必须在帮助里找得到）",
        all(s in read_text(os.path.join(web_root, "modules", "views", "help.js"))
            for s in ("界面主题", "重启本应用服务", "localStorage")),
        "help.js 没有说明主题存在哪里 / 重启期间断开是预期行为",
    )
    check(
        "帮助页含「关于」：开发者与源码地址",
        all(s in read_text(os.path.join(web_root, "modules", "views", "help.js"))
            for s in ("关于", "Misite齊", "github.com/MisiteQ")),
        "help.js 缺少关于卡片（开发者 / 源码地址）",
    )

    # ---- 主题（明暗 × 色系）的静态守卫 ----------------------------------
    #
    # 主题最容易坏在两个地方，而且两处**都不会报错**，只会让界面"看起来不对"：
    #   1. index.html 的预应用脚本与 theme.js 读的不是同一个 localStorage 键 ——
    #      首帧按默认值画，进了应用才跳成用户选的主题（深色用户每次闪一下白）；
    #   2. theme.js 里加了色系、style.css 里没加对应变量段 —— 选了没反应，
    #      变量悄悄回落成蓝色。
    # 运行时断言抓不到这两类问题（要看首帧像素才知道），只能静态守。
    index_html = read_text(os.path.join(repo_root, "internal", "webui", "web", "index.html"))
    theme_src = read_text(os.path.join(web_root, "modules", "theme.js"))
    css_src = read_text(os.path.join(web_root, "style.css"))

    m_key = re.search(r"export const STORAGE_KEY\s*=\s*'([^']+)'", theme_src)
    key = m_key.group(1) if m_key else ""
    check("theme.js 声明了 localStorage 键", bool(key), "theme.js 里找不到 STORAGE_KEY")
    check(
        "index.html 的预应用脚本与 theme.js 用的是同一个键",
        bool(key) and f"'{key}'" in index_html,
        f"键名不一致（theme.js={key!r}）：首帧会按默认值画再跳一次，深色用户看到闪白",
    )
    check(
        "主题在样式表**之前**同步写入 data-theme（无 FOUC）",
        0 <= index_html.find("setAttribute('data-theme'") < index_html.find("assets/style.css"),
        "预应用脚本不在 <link rel=stylesheet> 之前，深色模式会先闪一帧浅色",
    )

    def const_values(name: str) -> list[str]:
        m = re.search(rf"export const {name} = \[(.*?)\];", theme_src, re.S)
        return re.findall(r"value:\s*'([^']+)'", m.group(1)) if m else []

    modes, accents = const_values("MODES"), const_values("ACCENTS")
    check(
        "主题提供白天 / 暗黑 / 跟随系统三种模式",
        {"light", "dark", "auto"} <= set(modes),
        f"MODES 不全：{modes}",
    )
    check(
        "色系不少于四套（否则「多色系」名不副实）",
        len(accents) >= 4,
        f"ACCENTS 只有 {len(accents)} 套：{accents}",
    )
    missing_css = [a for a in accents if f'data-accent="{a}"' not in css_src]
    check(
        "每个色系在 style.css 里都有对应变量段",
        not missing_css,
        f"style.css 缺少这些色系：{missing_css}（选了没反应，会静默回落成蓝色）",
    )
    check(
        "暗黑模式有独立的变量段",
        ':root[data-theme="dark"]' in css_src,
        "style.css 没有 data-theme=dark 段，切换后只是换了个属性名",
    )
    m_idx = re.search(r"var ACCENTS = \[([^\]]*)\]", index_html)
    idx_accents = re.findall(r"'([^']+)'", m_idx.group(1)) if m_idx else []
    check(
        "index.html 的色系白名单与 theme.js 完全一致",
        idx_accents == accents,
        f"不一致：index.html={idx_accents} theme.js={accents}（新增色系在首帧会被丢弃）",
    )
    check(
        "启动时应用主题并监听系统偏好变化",
        "initTheme()" in app_src and "watchSystemTheme(" in app_src,
        "app.js 没有接 initTheme / watchSystemTheme，跟随系统会失效",
    )
    # 主题有**两处入口**（顶栏明暗按钮 + 设置页色卡），而这两处本身都要显示
    # 当前主题。早期实现是"每个入口自己重画自己"，结果在设置页切了暗黑、
    # 顶栏的月亮图标不变（用户真机上就是这么发现的）。现在统一成事件驱动：
    # 入口只改状态，重画由 app.js 的监听器负责。
    settings_src = read_text(os.path.join(web_root, "modules", "views", "settings.js"))
    check(
        "两处主题入口共用同一个通知（不会各自重画导致不同步）",
        "THEME_EVENT" in theme_src
        and "THEME_EVENT" in app_src
        and "setTheme(" in theme_src
        and "setTheme(" in settings_src
        and "applyTheme(" not in settings_src,
        "设置页仍在用 applyTheme 自己重画：顶栏按钮与设置页色卡会停在不同状态",
    )

    # ---- 重启应用服务的静态守卫 ------------------------------------------
    #
    # 这个功能最容易做错的不是后端，而是前端的**误报**：停止命令会杀掉执行
    # 它的进程，请求本来就等不到响应。把它当成失败报出去，用户会以为没成功
    # 然后再点一次。所以这里守的是"前端确实进入了等待轮询"，而不只是
    # "接口存在"。
    server_go = read_text(os.path.join(repo_root, "internal", "httpapi", "server.go"))
    sys_src = read_text(os.path.join(web_root, "modules", "views", "system.js"))
    check(
        "后端注册了重启接口",
        '"POST /api/system/restart"' in server_go,
        "server.go 里找不到 POST /api/system/restart",
    )
    check(
        "前端封装并使用了重启接口",
        "api/system/restart" in api_src and "api.system.restart(" in sys_src,
        "api.js / system.js 没有接上重启接口（只有后端等于没有这个功能）",
    )
    check(
        "重启后进入等待轮询，而不是把断连报成失败",
        "api.health(" in sys_src and "err.status < 500" in sys_src and "location.reload()" in sys_src,
        "system.js 没有探活 / 没有区分「被拒绝」与「已开始」，用户会以为重启失败",
    )
    dom_src = read_text(os.path.join(web_root, "modules", "dom.js"))
    check(
        "不可关闭的弹窗真的没有出口（× / Esc / 背景三条都要堵住）",
        "dismissible: false" in sys_src
        and "options.dismissible === false ? null :" in dom_src
        and "options.dismissible !== false" in dom_src,
        "dismissible:false 只管住了 Esc 与背景，右上角的 × 仍能关掉重启等待层",
    )
    check(
        "主动停服时先关掉 SSE，不把故意的断开显示为故障",
        # 必须匹配**带引号的完整事件名**：只搜 qlink:restarting 这个裸子串，
        # 拼错成 qlink:restarting-x 也会命中，等于没在测。
        "'qlink:restarting'" in sys_src and "'qlink:restarting'" in app_src,
        "缺少 qlink:restarting 协作：重启期间界面会飘一个假的「实时已断开」",
    )

    # 真机事故：RestartSelf 原本写成「本进程里 stop，再 start」。
    # appcenter-cli stop 是同步的（它等本进程退出才返回），所以"再 start"
    # 那几行永远执行不到 —— 结果是点一次重启，应用就永久停在停止态。
    # 这几条守的是"那条链必须交给独立会话"，是整个功能能不能用的分界线。
    service_go = read_text(os.path.join(repo_root, "internal", "fnos", "service.go"))
    restart_body = extract_fn_body(service_go, "func (s *Service) RestartSelf(")
    check(
        "重启走独立会话，而不是在本进程里 stop 再 start（否则应用会被停死）",
        bool(restart_body)
        and "spawnDetachedShell" in restart_body
        and "buildRestartScript" in restart_body
        and "s.cli.Stop(" not in restart_body
        and "s.cli.Start(" not in restart_body,
        "RestartSelf 又回到本进程内 stop→start：stop 是同步的，start 永远执行不到",
    )
    detach_src = read_text(os.path.join(repo_root, "internal", "fnos", "detach_unix.go"))
    check(
        "独立会话真的另立了会话（setsid）",
        "Setsid: true" in detach_src and "func detachSysProcAttr" in detach_src,
        "detachSysProcAttr 没有 setsid：脚本仍属于本进程会话，会被 stop 一起带走",
    )
    check(
        "重启脚本里 stop 失败也继续 start",
        "; else echo" in service_go and "仍继续" in service_go,
        "stop 一旦失败整条链就断掉，用户点一次重启就永久失去服务",
    )
    main_go = read_text(os.path.join(repo_root, "cmd", "server", "main.go"))
    check(
        "退出时先关 SSE 再 Shutdown（顺序反了会等满超时再被 SIGKILL）",
        "CloseEvents()" in main_go
        and main_go.index("CloseEvents()") < main_go.index("srv.Shutdown("),
        "SSE 是长连接永不变空闲，Shutdown 排在前面会永远等不到，每次停止都耗满宽限",
    )

    # ---- 语义色按钮的静态守卫 --------------------------------------------
    # 用户提的是"界面太素雅"，所以按钮不能全是同一个灰底。
    # 守的是"语义色真的有人用"，而不只是"CSS 里定义了"。
    view_src = "".join(
        read_text(os.path.join(web_root, "modules", "views", f))
        for f in os.listdir(os.path.join(web_root, "modules", "views"))
    )
    check(
        "按钮用上了语义色（不只是定义了样式）",
        all(c in css_src for c in (".btn.accent", ".btn.ok", ".btn.warn", ".btn.danger"))
        and all(c in view_src for c in ("btn accent", "btn warn", "btn danger")),
        "语义色要么没定义、要么没人用，界面仍旧一片灰",
    )

    data_dir = tempfile.mkdtemp(prefix="qlink2d-e2e-")
    backend_port = free_port()
    # 显式指定套接字路径，把运行痕迹全部关在临时目录里，
    # 不要污染系统 /tmp。Windows 上创建 Unix 套接字会失败，
    # 服务会自行降级为「仅 TCP」——这正是本脚本依赖的路径。
    proc = subprocess.Popen(
        [
            exe,
            "-port", str(backend_port),
            "-data", data_dir,
            "-log-dir", os.path.join(data_dir, "logs"),
            "-socket", os.path.join(data_dir, "app.sock"),
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    gateways: list[GatewaySimulator] = []
    try:
        if not wait_backend(backend_port):
            print("!! 后端 20 秒内未就绪，放弃。", file=sys.stderr)
            return 2
        print(f"后端已就绪（127.0.0.1:{backend_port}），数据目录 {data_dir}")

        for label, strip in (
            ("剥离前缀（真机网关的实际行为）", True),
            ("转发时保留前缀（兼容模式）", False),
        ):
            gw = GatewaySimulator(backend_port, strip_prefix=strip)
            gw.start()
            gateways.append(gw)
            run_checks(label, gw, forwards_prefix=not strip)
            gw.stop()

    finally:
        for gw in gateways:
            try:
                gw.stop()
            except Exception:  # noqa: BLE001
                pass
        proc.terminate()
        try:
            proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            proc.kill()
        if not args.keep:
            import shutil

            shutil.rmtree(data_dir, ignore_errors=True)
        else:
            print(f"临时数据目录已保留：{data_dir}")

    print("\n" + "=" * 62)
    print(f"\033[1m通过 {len(PASSED)} 项\033[0m，失败 \033[31m{len(FAILED)}\033[0m 项")
    if FAILED:
        print("\n失败明细：")
        for f in FAILED:
            print(f"  - {f}")
        return 1
    print("\033[32m全部通过：在飞牛统一网关的两种转发模式下均可正常打开。\033[0m")
    return 0


if __name__ == "__main__":
    sys.exit(main())
