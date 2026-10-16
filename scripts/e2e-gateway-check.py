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

        class Server(ThreadingHTTPServer):
            # 浏览器会主动掐掉它不再需要的请求（预扫描器的预取、被取消的 fetch），
            # socketserver 默认会为每一次这样的断开往 stderr 打一整段 traceback。
            # 那不是故障，但混在真实失败里会让"这轮到底有没有问题"变得难判 ——
            # 而这个项目已经吃够了"噪声与失败同形"的亏。
            def handle_error(self, request, client_address):
                pass

        self.server = Server(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.port = self.server.server_address[1]

    def start(self) -> None:
        self._thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self.server.shutdown()
        # 等 serve_forever 真的退出再关套接字：不等的话它会带着一个已经关掉的
        # 监听套接字继续转一圈，在解释器收尾阶段碰到已经失效的 stderr ——
        # 结果是"全部通过"打完了，退出码却是非零（表现为 make 目标失败）。
        self._thread.join(timeout=5)
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
# 扫描仓库源码时一律跳过这些目录。
# `.workbuddy/` 也在这里：它是助手的工作目录（在 .gitignore 第 28 行），
# 不属于交付内容 —— 里面的工作笔记会引用"用户要求不体现的那个名字"，
# 那是在记录要求本身，不该被当成仓库里残留了它。
SKIP_DIRS = {"node_modules", ".git", ".build", ".workbuddy", "vendor", "__pycache__", "data"}


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

    # ---- 端口转发（四层）--------------------------------------------------
    fwd_dir = os.path.join(repo_root, "internal", "forward")
    fwd_mgr = read_text(os.path.join(fwd_dir, "manager.go"))
    fwd_tcp = read_text(os.path.join(fwd_dir, "tcp.go"))
    fwd_udp = read_text(os.path.join(fwd_dir, "udp.go"))
    fwd_acl = read_text(os.path.join(fwd_dir, "acl.go"))
    dom_fwd = read_text(os.path.join(repo_root, "internal", "domain", "forward.go"))
    api_fwd = read_text(os.path.join(repo_root, "internal", "httpapi", "forwards.go"))
    store_fwd = read_text(os.path.join(repo_root, "internal", "store", "forwards.go"))
    fwd_view = read_text(os.path.join(web_root, "modules", "views", "forward.js"))
    help_js = read_text(os.path.join(web_root, "modules", "views", "help.js"))

    check(
        "转发监听只在 forward 包里建立，接口层不许自己 net.Listen",
        "net.Listen" not in api_fwd and "net.ListenUDP" not in api_fwd,
        "接口层直接监听端口 = 分层被打破，状态会散落在两个地方",
    )
    # 自转发（A→A）是最容易顺手配出来的错误：不会报错，只会让每个请求
    # 被反复转发给本机，直到 fd 耗尽。
    check(
        "目标为本机同一个端口时必须拒绝（自转发）",
        "func (f Forward) IsSelfForward()" in dom_fwd
        and "IsSelfForward()" in dom_fwd.replace("func (f Forward) IsSelfForward()", "")
        and "IsUnspecified" in dom_fwd,
        "自转发检测缺失：8080→127.0.0.1:8080 会让流量在端口上无限自我转发",
    )
    check(
        "两条规则互为转发目标时必须拒绝（两跳环）",
        "func (m *Manager) checkLoopLocked(" in fwd_mgr
        and "互为转发目标" in fwd_mgr,
        "缺少环检测：A→B→A 会让数据包在两个端口间无限弹射，几秒内吃满 CPU",
    )
    check(
        "UDP 会话必须被回收（UDP 没有 FIN，不回收就是泄漏）",
        "func (u *udpFwd) reap(" in fwd_udp
        and "func (u *udpFwd) reapLoop(" in fwd_udp
        and "udpIdleTimeout" in fwd_udp,
        "UDP 会话表没有回收逻辑：功能一切正常，fd 与内存却在一直涨",
    )
    check(
        "TCP 转发双向搬运，且支持半关闭",
        "io.Copy(up, src)" in fwd_tcp
        and "io.Copy(src, up)" in fwd_tcp
        and "CloseWrite" in fwd_tcp,
        "少一个方向或漏了半关闭，表现是'连接正常但响应永远不回来'",
    )
    check(
        "访问控制支持 IP 与 CIDR，并归一化 IPv4-mapped 地址",
        "ParseCIDR" in fwd_acl and "func normalizeIP(" in fwd_acl and "To4()" in fwd_acl,
        "不做归一化的话，双栈下同一条白名单会时灵时不灵",
    )
    # 端口被自家另一条规则占用属于确定性错误：让它落盘就会留下一条
    # "保存了却永远起不来"的规则。必须在写盘之前拦住。
    check(
        "保存前先挡掉确定性冲突（Check），而不是落盘后才发现起不来",
        "func (m *Manager) Check(" in fwd_mgr
        and "s.checkForward(f)" in api_fwd,
        "缺少落盘前的冲突检查：规则会长期停在'存下来了但不工作'的状态",
    )
    check(
        "端口被外部程序占用时规则照常保存，但要给出告警",
        "warning" in api_fwd and "Warning" in api_fwd,
        "外部占用是暂时状态，不该阻止保存；但不告警用户就以为一切正常",
    )
    check(
        "转发接口已注册（列表 / 增删改 / 启停 / 探测 / 连接记录）",
        all(s in server_go for s in (
            'GET /api/forwards"', 'POST /api/forwards"',
            'PUT /api/forwards/{id}"', 'DELETE /api/forwards/{id}"',
            'POST /api/forwards/{id}/enabled"',
            'POST /api/forwards/{id}/probe"',
            'GET /api/forwards/{id}/connections"',
        )),
        "转发接口注册不全",
    )
    check(
        "转发规则持久化到独立文件，不与链接混在一起",
        "fileForwards" in store_fwd
        and 'fileForwards = "forwards.json"' in read_text(
            os.path.join(repo_root, "internal", "store", "store.go")),
        "转发规则应与 links.json 分开，避免一处损坏连带丢掉另一类数据",
    )
    # 前端：视图注册、导航入口、事件订阅三处都要有。
    # 单测覆盖不到 import 路径写错，而那类错误的表现正是
    # "页面能开、接口全通、点了没反应"。
    check(
        "前端注册了端口转发视图与导航入口",
        "modules/views/forward.js" in app_src
        and "key: 'forward'" in app_src
        and "forward," in app_src.split("const VIEWS")[1][:80],
        "app.js 没有把 forward 接进 VIEWS / NAV",
    )
    check(
        "转发状态变化会广播给前端，前端订阅了该事件",
        "EventForwardChanged" in read_text(os.path.join(repo_root, "internal", "httpapi", "events.go"))
        and "'forward'" in read_text(os.path.join(web_root, "modules", "api.js"))
        and "case 'forward'" in app_src,
        "转发状态变化没有推给前端，界面会停在旧状态直到手动刷新",
    )
    check(
        "转发视图提供了规则编辑、启停、目标探测与连接记录",
        all(s in fwd_view for s in
            ("openEditor", "api.forwards.probe", "api.forwards.connections",
             "api.forwards.setEnabled")),
        "转发视图缺少关键操作入口",
    )
    check(
        "帮助页讲清了端口转发与端口映射的区别",
        "端口转发" in help_js and "端口映射" in help_js,
        "两个能力名字相近、层级不同，帮助页不讲清楚用户会选错",
    )

    # 界面文案走的是纯文本节点，**不解析 markdown**。写成 **重点** 会把两颗
    # 星号原样显示给用户 —— 这个 bug 真实存在过（帮助页"那是**预期行为**"
    # 与设置页"当前**没有**口令保护"），只是没人盯着这两个字看过。
    md_hits: list[str] = []
    views_dir = os.path.join(web_root, "modules", "views")
    for _fn in sorted(os.listdir(views_dir)):
        if not _fn.endswith(".js"):
            continue
        for _i, _line in enumerate(read_text(os.path.join(views_dir, _fn)).splitlines(), 1):
            _s = _line.strip()
            if _s.startswith("//") or _s.startswith("/*") or _s.startswith("*"):
                continue  # 注释里用 ** 无所谓，它不会显示
            if "**" in _line:
                md_hits.append("%s:%d" % (_fn, _i))
    check(
        "界面文案里没有混进 markdown 星号",
        not md_hits,
        "视图文本是纯文本，'**重点**'会原样冒出两颗星号：" + ", ".join(md_hits[:5]),
    )

    # 代码与文档里不留外部参考实现的名字（用户明确要求）。
    # 词刻意拆成两半拼：这个脚本自己要能搜它，又不能让自己变成命中项。
    needle = "luc" + "ky"
    ref_hits: list[str] = []
    for _dirpath, _dirnames, _filenames in os.walk(repo_root):
        _dirnames[:] = [d for d in _dirnames if d not in SKIP_DIRS]
        for _fn in _filenames:
            if not _fn.endswith((".go", ".js", ".md", ".py", ".sh", ".html", ".css")):
                continue
            _rel = os.path.relpath(os.path.join(_dirpath, _fn), repo_root)
            if _rel == "README.md":
                continue  # 项目来源只写在 README 一处，这是既有约定
            if needle in read_text(os.path.join(_dirpath, _fn)).lower():
                ref_hits.append(_rel)
    check(
        "代码与文档里不留外部参考实现的名字",
        not ref_hits,
        "用户明确要求不体现它：" + ", ".join(ref_hits[:5]),
    )

    # ---- 窄屏导航：一个「元素存在」查不出来的可见性故障 ----------------------
    # 用户报「手机模式下左侧的侧边栏没有了，不能切换功能区」。根因是一条按
    # 标签名排除某个 class 的窄屏规则（`span:not(...)`）—— 本意「只留图标」，
    # 但图标本身也是 span，于是图标与文字一起被藏掉，七个功能项变成
    # 看不见、仅 37×14px 的空白按钮。
    # 静态守卫在这里格外关键：图标一直在 DOM 里、`hidden` 属性从未被设置过，
    # 任何「元素存在吗」的断言都看不见它。
    style_css = read_text(os.path.join(web_root, "style.css"))
    app_js = read_text(os.path.join(web_root, "app.js"))
    # 剥掉注释再判断 —— 这条坑项目里踩过一次（见上面 [hidden] 那条）：解释旧写法的
    # 注释里同样写着旧规则原文，不剥离的话"规则早删了"也会被判成"还在"。
    style_code = re.sub(r"/\*.*?\*/", "", style_css, flags=re.S)
    narrow_css = extract_fn_body(style_code, "@media (max-width: 860px)")

    check(
        "窄屏样式块存在",
        bool(narrow_css),
        "没有窄屏适配，手机上就是一坨挤在一起的桌面布局",
    )
    check(
        "不再用「元素类型 + :not(class)」隐藏导航内容",
        "span:not(" not in style_code,
        "图标本身也是 span —— 这种写法会把图标连文字一起藏掉（用户报过的故障）",
    )
    # 断言限定在 renderNav 的**函数体**里，并且带上 `class:` 前缀。
    # 整文件搜 "nav-label" 是骗得过去的：改名成 'xnav-label' 仍含这个子串，
    # 注释里提一句也能让它通过 —— 「名字出现」不等于「真的用上了」
    # （这条坑本项目今天在 CheckListen / 外部参考名两处各踩过一次）。
    nav_body = extract_fn_body(app_js, "function renderNav(")
    check(
        "要隐藏的文字有自己的 class，而不是「所有 span」",
        "class: 'nav-label'" in nav_body and ".nav-label" in style_code,
        "按语义给 class 才不会误伤同一元素类型的兄弟节点；"
        "且必须确认它真的挂在导航项的文字节点上，而不只是名字出现在文件里",
    )
    check(
        "窄屏下导航图标与功能名都没被 display:none 掉",
        not [
            ln for ln in narrow_css.splitlines()
            if "display: none" in ln and ("nav-icon" in ln or "nav-label" in ln)
        ],
        "窄屏只该去掉品牌名与状态文字，图标和功能名必须留着",
    )
    check(
        "窄屏下导航改为底部固定条（拇指够得到）",
        "position: fixed" in narrow_css and "bottom: 0" in narrow_css,
        "飞牛 App 自带标题栏已占掉顶部两行，导航再放顶部就又挤又难按",
    )
    check(
        "触摸目标不小于 44px",
        "min-height: 44px" in narrow_css or "min-height: 46px" in narrow_css,
        "37×14px 的按钮手指点不准 ——「不能切换」的真实体感就是这个",
    )
    check(
        "内容区给底部条留了空间，并避开全面屏手势条",
        "env(safe-area-inset-bottom" in narrow_css and ".content" in narrow_css,
        "底部条是 fixed、不占文档流：不留白，最后一张卡片永远滚不到",
    )

    # 底部条的尺寸必须**两处同源**。早先是 .sidebar 写 46、.content 写 78 两个
    # 互不相干的数字；改了其中一处，另一处就静默失配 —— 表现是"最后一张卡片
    # 被压在栏下面"或"底部空一大截"，而单看任意一边都是对的。
    # 静态这里守"同源"，真浏览器那边守"渲染出来确实够大"（见 browser-mobile-check.js）。
    sidebar_rule = extract_fn_body(narrow_css, ".sidebar {")
    content_rule = extract_fn_body(narrow_css, ".content {")
    check(
        "底部条高度与内容区留白共用同一个变量（不要两个写死的数字）",
        "--navbar-h" in narrow_css
        and "var(--navbar-h)" in sidebar_rule
        and "var(--navbar-h)" in content_rule,
        "两处写死的数字迟早不同步：改了条高忘了 content，卡片就被压在栏下面",
    )
    # 图标大小同理：只断言"图标元素在 DOM 里"抓不住"图标被改小"。
    # 反向验证时把 22px 改成 14px，除了这条没有任何一条会 FAIL。
    icon_rule = extract_fn_body(narrow_css, ".nav-item .nav-icon svg {")
    icon_px = re.search(r"width:\s*(\d+)px", icon_rule)
    check(
        "窄屏导航图标不小于 20px",
        icon_px is not None and int(icon_px.group(1)) >= 20,
        f"当前 {icon_px.group(1) + 'px' if icon_px else '取不到'} —— "
        "图标小到看不清时，「可见 / 能点」类断言仍然全绿，只有量尺寸能发现",
    )
    # 侧边栏**只放导航**：用户要求把那一排「数字 + 圆圈」整个去掉。
    # 断言"节点根本不存在"而不是"被 CSS 藏起来" —— 留着 DOM 换个断点就会
    # 重新冒出来，而且数据还得有人去拉（那正是上一轮为徽标改出来的逻辑）。
    check(
        "侧边栏不再有计数徽标（数字与圆圈整个去掉）",
        "nav-badge" not in app_js and "nav-badge" not in style_code,
        "用户明确要求去掉，app.js / style.css 里都不该再有它",
    )
    # 窄屏把状态点**整格**撤掉：那里没有文字，只剩一枚看不出含义的绿点。
    check(
        "窄屏撤掉状态点（只有圆点、没有文字的指示灯没有信息量）",
        re.search(r"\.sidebar-foot\s*\{[^}]*display:\s*none", narrow_css, re.S) is not None,
        "窄屏必须把 .sidebar-foot 整格隐藏；只藏文字会留下一个光秃秃的点",
    )
    check(
        "窄屏导航有真浏览器回归（规矩不能只写在注释里）",
        os.path.isfile(os.path.join(repo_root, "scripts", "browser-mobile-check.js"))
        and "browser-mobile-check.js" in read_text(os.path.join(repo_root, "scripts", "browser-e2e.py")),
        "静态规则守不住「图标被藏」，得有人在真视口里量尺寸、按真实坐标点",
    )
    # 「正在查找…」是一个**只在请求进行中存在的瞬时状态**：静态守卫能查到
    # 代码里有这行字，查不到它有没有被画到屏幕上、有没有被样式压没。
    # 所以"过程可见"这件事必须有人在真浏览器里盯着看。
    # 光有文件名常量不算接上：必须真的被**当成参数传给 node**。
    # 反向验证时把调用换回 mobile 脚本，常量声明还在，只查文件名是抓不住的。
    check(
        "图标查找过程有真浏览器回归（找一个瞬时状态，静态断言守不住）",
        os.path.isfile(os.path.join(repo_root, "scripts", "browser-icon-check.js"))
        and "[node, ICON_SCRIPT, url]" in read_text(os.path.join(repo_root, "scripts", "browser-e2e.py")),
        "「有提示」的对立面是「节点建了却没挂进界面」，只有渲染出来才看得见",
    )

    # ---- 「自动」图标：真的去抓站点自己的 favicon ---------------------------
    # 用户报的是「自动获取图标，能不能直接自动抓取，一般就是通过直接抓 url 的
    # favicon 来获得的」。这一档的价值在于**内网服务**：公开图标库里不会有
    # 用户自家 NAS 上跑的那个服务，只有它自己的 favicon 认得出来。
    icon_go = read_text(os.path.join(repo_root, "internal", "fnos", "icon.go"))
    favicon_go = read_text(os.path.join(repo_root, "internal", "fnos", "favicon.go"))
    service_go = read_text(os.path.join(repo_root, "internal", "fnos", "service.go"))
    # 解析链的实现只在一处（ResolveIconEx），ResolveIcon 必须委派过去。
    #
    # 这条守卫是**被自己坑出来的**：下面那几条断言原先盯着 ResolveIcon 的函数体，
    # 拆分之后它们就变成了在检查一个没人调用的壳 —— 而"守卫盯着错的函数"
    # 与"守卫全绿"长得一模一样。所以先钉死这层委派关系。
    resolve_body = extract_fn_body(icon_go, "func ResolveIcon(")
    resolve_ex_body = extract_fn_body(icon_go, "func ResolveIconEx(")
    check(
        "图标解析链只有一个实现，ResolveIcon 只是它的薄壳",
        bool(resolve_body) and "ResolveIconEx(req)" in resolve_body and bool(resolve_ex_body),
        "两处各写一份必然会漂移；而守卫盯着不执行的那一份，等于没守",
    )
    check(
        "favicon 这一档接进了图标解析，且排在官方图标库之后",
        resolve_ex_body.count("siteFaviconLookup(") == 1
        and resolve_ex_body.find("matchOfficialIconKeyword(") < resolve_ex_body.find("siteFaviconLookup("),
        "顺序反了的话，每个链接都会先去打一次站点请求，还会拿一张 16×16 的 "
        "favicon 顶掉人工挑选的官方图标",
    )
    # 离线模式（--no-remote-icon）下不许发请求：内网地址看起来无害，
    # 但那仍然是本进程主动建立的连接。
    remote_block = extract_fn_body(resolve_ex_body or icon_go, "if req.AllowRemote {")
    check(
        "favicon 抓取受「允许远端图标」开关管辖",
        bool(remote_block) and "siteFaviconLookup(" in remote_block,
        "关掉远端图标后，这一步必须整体跳过（不许偷偷发内网请求）",
    )
    check(
        "站点地址与证书开关由链路形态推导后传入",
        "SiteURL:       iconSiteURL(l)" in service_go or "SiteURL: iconSiteURL(l)" in service_go,
        "拿不到地址就应传空串，让这一档自动跳过；不能凭空造一个地址出来",
    )
    # 官方图标库那一档是个乘法：候选 × 关键词 × 4 个 CDN 节点。国内访问
    # jsDelivr 经常整体不通，每次都要等满单请求超时，最坏能把「保存」挂几分钟。
    check(
        "官方图标库匹配也有总时间预算（不是逐个请求各自等超时）",
        "officialIconBudget" in icon_go
        and "context.WithTimeout" in extract_fn_body(icon_go, "func matchOfficialIconKeyword("),
        "候选数 × 关键词数 × 4 个 CDN 的乘法，最坏几十次 8 秒超时，而用户在等这一刻",
    )
    check(
        "官方图标库匹配会回报命中的关键词（否则界面说不出「命中什么」）",
        "return set, name, true" in icon_go,
        "只回一枚图标的话，界面只能写「已找到」，说不出是凭什么找到的",
    )
    # ICO 是这条路上最容易「看起来没生效」的地方：标准库没有 ICO 解码器，
    # 而 /favicon.ico 绝大多数就是 ICO —— 不自己解的话整条路会全程失败。
    decode_body = extract_fn_body(favicon_go, "func decodeIconImage(")
    check(
        "站点图标解码先走 ICO 分支（标准库解不了 ICO）",
        decode_body.find("decodeICO(") >= 0
        and decode_body.find("decodeICO(") < decode_body.find("decodeImage("),
        "image.Decode 对 ICO 直接报 unknown format，而 /favicon.ico 绝大多数是 ICO",
    )
    check(
        "ICO 解码覆盖「PNG 内嵌」与「DIB 位图」两种内容",
        "func decodeICOEntry(" in favicon_go and "func decodeDIB(" in favicon_go
        and re.search(r"bpp\s*!=\s*1\s*&&\s*bpp\s*!=\s*4\s*&&\s*bpp\s*!=\s*8\s*&&\s*bpp\s*!=\s*24\s*&&\s*bpp\s*!=\s*32", favicon_go) is not None,
        "现代 favicon 是 PNG-in-ICO，老站点是 1/4/8/24/32 位的 DIB —— 两种都要能解",
    )
    # 抓取必须**有总预算**：这段代码跑在「保存 / 放到桌面」这条用户正在等的
    # 路径上，逐个候选设超时的话最坏会拖到二十几秒。
    check(
        "抓取有总时间预算，而不是每个候选各自计时",
        "context.WithTimeout" in favicon_go and "faviconBudget" in favicon_go,
        "逐个候选设超时的话，试满 4 个候选最坏要二十几秒，而用户在等这一刻",
    )

    # ---- 图标解析的过程必须对用户可见 ----------------------------------------
    #
    # 用户的原话是「这个图标也没有自动搜索百度的图标啊，这个过程有个什么提示吗
    # 或者能让人知道搜索中或者搜索状态」，随后补充「不光是快捷方式，本机端口和
    # 端口映射也有这样的问题……过程也没有提示」。
    #
    # 追下来真正的病根不在抓取本身（后端其实抓到了），而在**结果与过程都不回传**：
    # 编辑器那一档永远画着内置默认图，"还没找""找了没找到""根本没找"三种情况
    # 在界面上完全同形。所以这组守卫盯的是"有没有把话讲出来"，
    # 而不是"有没有抓到" —— 后者早就绿了，用户依然报问题。
    icons_api = read_text(os.path.join(repo_root, "internal", "httpapi", "icons.go"))
    server_go = read_text(os.path.join(repo_root, "internal", "httpapi", "server.go"))
    api_js = read_text(os.path.join(web_root, "modules", "api.js"))
    editor_js = read_text(os.path.join(web_root, "modules", "views", "editor.js"))
    desktop_js = read_text(os.path.join(web_root, "modules", "views", "desktop.js"))

    check(
        "解析结果带回来源与一句可展示的说明（否则界面只能显示一张图）",
        "type IconResolution struct" in icon_go
        and "Source IconSource" in icon_go
        and "Detail string" in icon_go,
        "只回一枚图标的话，「还没找」和「找了没找到」在界面上分不出来",
    )
    check(
        "抓取失败时说明「问了哪个地址、为什么没有」",
        "func faviconMissDetail(" in icon_go
        and "faviconMissDetail(req.SiteURL)" in resolve_ex_body,
        "一句「没找到」和「压根没去问」对用户毫无区别",
    )
    check(
        "离线模式也说明是开关关掉了抓取（而不是含糊的「没找到」）",
        "允许远端图标" in resolve_ex_body,
        "用户关掉了远端图标，界面却暗示「找过了」，会让人以为功能坏了",
    )
    check(
        "有「现场解析一次」的接口，且不依赖链接已经落库",
        'api.HandleFunc("POST /api/icons/resolve", s.handleResolveIcon)' in server_go
        and "func (s *Server) handleResolveIcon(" in icons_api,
        "编辑器里用户常常还没填完就想看图标，接口必须能吃下未落库的草案",
    )
    # 这条守的是一个真实踩过的坑：图标解析接口收的是**整条链接**的表单数据
    # （前端直接回传 toPayload 的结果），而它自己只用得上几个字段。
    # 一开始用了严格解码，第一次真机验证就撞上 `json: unknown field "desc"` ——
    # 报错内容和图标毫无关系，界面上只表现为"查找失败"。
    check(
        "解析接口对请求体宽松解码（它收的是别人的 DTO）",
        "decodeJSONLenient(w, r, &req)" in icons_api
        and "func decodeJSONLenient(" in read_text(os.path.join(repo_root, "internal", "httpapi", "respond.go")),
        "严格解码会让链接以后每加一个字段都把这个预览打挂",
    )
    check(
        "解析接口把预览图与来源一起回给前端",
        'body["preview"]' in icons_api
        and '"message":' in icons_api
        and '"detail":' in icons_api
        and '"elapsed_ms":' in icons_api,
        "没有预览图，前端就只能继续画内置默认图 —— 那正是用户看到的现象",
    )
    check(
        "前端确实调用了这个接口（名字出现 ≠ 真的用上）",
        "api.icons.resolve(toPayload(model))" in editor_js,
        "接口写好了没人调，与没写毫无区别",
    )
    auto_body = extract_fn_body(editor_js, "function buildIconSection(")
    check(
        "编辑器「自动」档会显示查找中 / 已找到 / 没找到三种状态",
        "正在查找图标" in auto_body and "lookupAuto" in auto_body and "statusBox" in auto_body,
        "这正是用户问的「这个过程有个什么提示吗」",
    )
    check(
        "状态行真的被挂进了界面，不是建了对象就完事",
        re.search(r"append\(fields,.*statusBox", auto_body, re.S) is not None
        or re.search(r"statusBox\s*\]", auto_body) is not None,
        "建了 DOM 节点却没 append，界面上什么都看不到（本项目踩过同类坑）",
    )
    check(
        "改地址 / 换形态会重新查找（否则一直挂着上一个地址的结果）",
        "onTargetChange()" in editor_js and "retarget: scheduleLookup" in editor_js,
        "用户改了端口但图标还是旧地址的 —— 会被当成「图标不更新」",
    )
    # 匹配必须落在**声明块**上，不能只搜类名。
    # 反向验证时把 `.icon-status { ... }` 整条规则删掉，只留下 `.icon-status:empty`
    # 之类的选择器，`.icon-status` in style_code 依然为真 —— 守卫照样全绿。
    check(
        "状态行有对应样式，且进行中/成功/失败三者同形",
        re.search(r"\.icon-status\s*\{[^}]*display:\s*inline-flex", style_code) is not None
        and re.search(r"\.icon-status:empty\s*\{[^}]*display:\s*none", style_code) is not None
        and re.search(r"\.tag\.busy\s*\{[^}]*background:", style_code) is not None,
        "缺样式的话标签会挤成一坨；空状态不收起则会留一条莫名其妙的空白",
    )

    # 列表缩略图：它是用户看到"图标没找着"的**第一现场**。
    #
    # 后端本来就会在对账时把图标装到桌面，但面板列表一直只认 Link.Icon 里
    # 用户填的东西 —— 自动档于是永远显示内置默认图，用户据此判断"没找到"。
    # 断言必须落在「真的写了盘」这一步上。只检查函数存在的话，
    # 把落盘调用摘掉、函数留成空壳，守卫照样全绿（反向验证时就是这样）。
    ensure_body = extract_fn_body(service_go, "func (s *Service) EnsureAutoIcon(")
    write_body = extract_fn_body(service_go, "func (s *Service) writeAutoIcon(")
    check(
        "自动解析出的图标会落盘，并被列表读到",
        "s.writeAutoIcon(dir, name, res)" in ensure_body
        and bool(write_body) and "os.WriteFile(path, res.Small" in write_body
        and "func (s *Service) AutoIconFile(" in service_go,
        "不落盘的话，列表就得为每条链接并发打一个可能访问外网的请求",
    )
    check(
        "视图带上自动图标的文件名与来历",
        "IconAuto string" in read_text(os.path.join(repo_root, "internal", "domain", "status.go"))
        and "views[i].IconAuto = s.deps.Service.AutoIconFile(l)" in server_go
        and "views[i].IconAutoNote = note.detail" in server_go,
        "少了这个字段，列表就无从显示真实图标，只能一律画默认图",
    )
    # 断言要落在"拿它当 src"上：写成 `if (view.icon_auto) return api.icons.defaultURL(64)`
    # 时字段名仍然出现，但图标永远还是那张默认图。
    check(
        "列表缩略图优先用解析出来的图标",
        "api.icons.url(view.icon_auto)" in extract_fn_body(desktop_js, "function thumbSrc("),
        "这是用户看到「图标没自动找到」的第一现场",
    )
    check(
        "程序生成的自动图标不混进用户图标库",
        "fnos.IsAutoIconFile(" in icons_api,
        "自动图标是程序产物，混进「我的图标」会让用户以为是自己传的",
    )

    # ---- 图标缩放：别把 logo 压缩太狠 ------------------------------------------
    #
    # 用户换了一版 logo，随后补的要求是「logo 的图别压缩太狠，有用户反映图标
    # 都不清楚了」。图标只有 64 / 256 两档，而来源普遍是 512～2048 见方 ——
    # 也就是说这条链上做的**全是缩小**，缩得越狠越糊，病根就在这里。
    #
    # 追下来是两层：旧的「反复折半 + 收尾双线性」本身就是一层近似（多一层
    # 近似多一层模糊），而面积平均又是个低通滤波 —— 少了后一道锐化，
    # 边缘必然被摊平。这组守卫盯的就是这两件事有没有真的落在代码里。
    imageutil_go = read_text(os.path.join(repo_root, "internal", "fnos", "imageutil.go"))
    hq_body = extract_fn_body(imageutil_go, "func ResizeHighQuality(")

    # 断言落在**调用点**上：只查「有个叫 ResizeArea 的函数」的话，把调用换回
    # 双线性插值、把函数留成没人用的死代码，守卫照样全绿（本项目踩过这种坑）。
    check(
        "缩小时走精确面积平均，而不是「折半近似 + 插值」",
        "ResizeArea(square, dstW, dstH)" in hq_body,
        "双线性每个输出像素只采样 4 个点，缩 32 倍时 99.6% 的源像素根本没被读到",
    )
    # 面积平均是低通滤波，边缘一定会被摊平，缩得越狠摊得越平 ——
    # 这道锐化就是「别压缩太狠」的落点。反向验证时把这一行换成 `return out`：
    # 静态断言与行为断言会同时 FAIL，缺了它这条修复就只是换了个近似算法。
    check(
        "缩到 2× 以上会补一道锐化（否则边缘会被平均摊平）",
        "sharpenPremultiplied(out, sharpenAmount)" in hq_body
        and re.search(r"sw\s*>=\s*dstW\s*\*\s*sharpenMinRatio", hq_body) is not None,
        "缩得越狠边缘越平，用户看到的正是「图标不清楚」",
    )
    # 按覆盖面积加权才叫「面积平均」。只算一个方向的重叠、或者干脆等权，
    # 都能跑、都能出图，只是缩小时会丢一多半像素。
    area_body = extract_fn_body(imageutil_go, "func ResizeArea(")
    check(
        "面积平均真的按覆盖面积加权（横竖两个方向都要算重叠）",
        "cx := math.Min(right, float64(sx+1)) - math.Max(left, float64(sx))" in area_body
        and "cy := math.Min(bottom, float64(sy+1)) - math.Max(top, float64(sy))" in area_body
        and "w := cx * cy" in area_body,
        "等权平均就退化成「取一个代表点」，细线条还是会断",
    )
    # 颜色运算必须在预乘 alpha 空间：全透明像素的 RGB 没有任何意义，
    # 不预乘就等于让这些「不存在的颜色」按同样的权重参与平均与锐化 ——
    # 表现出来是图标外圈糊出一圈脏描边、边缘泛彩色噪点。
    bilinear_body = extract_fn_body(imageutil_go, "func ResizeBilinear(")
    check(
        "颜色运算在预乘 alpha 空间做（放大与锐化都要）",
        bilinear_body.count("premultiplied(srcRGBA.RGBAAt(") == 4
        and "float64(c.R) * a / 255" in extract_fn_body(imageutil_go, "func premultiplied("),
        "全透明像素的颜色参与平均，会给图标外圈套上一圈脏描边",
    )
    check(
        "缩放只有一份实现（旧的「反复折半」近似不许以第二条路径回来）",
        "func BoxDownscale(" not in imageutil_go,
        "两套缩放并存时产物走哪一条取决于调用点 —— 改了 A 看起来「没生效」",
    )
    # 运行时生成的默认图标（favicon、面板品牌图）走的是另一条调用链，
    # 必须与产品图标汇到同一个入口，否则会出现「应用中心图标清楚了、
    # 桌面上的还是糊的」。
    check(
        "运行时默认图标与产品图标走同一条重采样路径",
        "ResizeHighQuality(logo, size, size)" in extract_fn_body(icon_go, "func DefaultIcon("),
        "两条路径各写一份的话，改了其中一条只有一半的图标会变",
    )
    # 「改了 logo 只重新生成一部分」不会有任何报错，只表现为有的地方图标换了、
    # 有的没换 —— 浏览器标签与面板里的品牌图标走的正是内嵌那张，最容易漏。
    # 字节级的比对只能由测试来做，所以这里守的是"那条测试还在、且产物清单齐全"。
    iconforge_go = read_text(os.path.join(repo_root, "cmd", "iconforge", "main.go"))
    artifacts_test = os.path.join(repo_root, "internal", "fnos", "icon_assets_test.go")
    check(
        "logo 的 5 个派生产物由同一处生成，且有测试钉住它们彼此一致",
        iconforge_go.count("set.Small") == 2
        and iconforge_go.count("set.Large") == 3
        and "TestShippedIconArtifactsStayInSync" in read_text(artifacts_test),
        "少重新生成任何一处都不会报错，只表现为「桌面图标换了、页面里没换」",
    )

    # ---- 从图标库里挑一枚已有图标 ---------------------------------------------
    #
    # 需求原话：「在添加图标的时候可以选择图标库里面的已有图标作为显示的图标」。
    # 在此之前图标库只能往里传、不能从里选 —— 想复用一枚传过的图标，唯一的
    # 办法是把那张图再从本机找出来重传一遍，于是同一个图标在 icons/ 目录里
    # 堆成 miwifi.png / miwifi-1.png / miwifi-2.png 好几份，谁也说不清
    # 哪份是哪条链接在用的。
    #
    # 这里守三件事，每件都对应一种"看起来做了、其实没做"：
    #   1. 档位真的在；
    #   2. 它**复用 upload 这一档**（文件名 + icons/ 目录），不新造 source ——
    #      新造一个后端不认识的 source 不会有任何报错：表单照收、链接照存，
    #      然后在解析链上被判成未知来源，一路降级到内置默认图标，而界面上
    #      看起来一切正常；
    #   3. 选中态真的画得出来。只把 class 加上、CSS 里没有对应规则，
    #      用户点完还是不知道选上没有 —— "点了没反应"正是这一档最容易出的问题。
    editor_js = read_text(os.path.join(web_root, "modules", "views", "editor.js"))
    picker_body = extract_fn_body(editor_js, "function buildIconLibraryPicker(")

    check(
        "图标卡片里有「图标库」这一档",
        "{ value: 'library', label: '图标库' }" in editor_js,
        "没有这一档，用户只能把同一张图反复重传，图标目录里堆出一串副本",
    )
    check(
        "「图标库」档复用 upload 来源，不新造后端不认识的 source",
        re.search(r"model\.icon\.source = v === 'library' \? 'upload' : v;", editor_js) is not None,
        "新造的 source 会一路穿过表单落进链接定义，再在解析链上悄悄退回默认图标",
    )
    check(
        "从图标库选中的文件名写进 icon.ref（与「上传」档同构）",
        "model.icon.ref = item.name;" in picker_body,
        "写到别处的话，列表缩略图 / 桌面图标 / 打包三条已有路径都读不到它",
    )
    # 反向验证时把 `api.icons.list()` 换成一句空数组：静态与真视口会同时 FAIL。
    check(
        "「图标库」档真的去读图标库列表（不是画一个空网格）",
        "api.icons.list()" in picker_body,
        "读不到列表就只是一个空网格，用户会以为图标库是空的",
    )
    library_css = re.sub(r"/\*.*?\*/", "", read_text(os.path.join(web_root, "style.css")), flags=re.S)
    check(
        "选中态真的画得出来（class 与样式两条都要在）",
        "classList.toggle('selected', on)" in picker_body
        and re.search(r"\.icon-tile\.pick\.selected\s*\{", library_css) is not None,
        "只加 class 不写样式 = 用户点了没反应，而这正是这一档最容易出的问题",
    )
    # 磁贴整块是一个按钮 —— 只让里面的小图可点会让点击目标缩到 60px 见方，
    # 手机上基本点不准（本项目在窄屏导航上栽过一次同型的坑）。
    check(
        "整块磁贴都是点击目标，不是只有中间那张小图",
        re.search(r"'icon-tile pick'", picker_body) is not None
        or re.search(r'"icon-tile pick"', picker_body) is not None,
        "点击目标只有 60px 见方，手机上点不准",
    )

    # ---- 服务发现：同一个服务的多个套接字必须收成一行 -------------------------
    #
    # 用户的原话是「为什么扫描出来的端口是重复的，有 0.0.0.0 的有不带的，
    # 然后窗口最大化之后又再重复出来了很多」。
    #
    # 追下来病根有两层，而且都不在"扫描"上：
    #   1. 内核按**套接字**记账（IPv4/IPv6 通配各一份、tcp/udp 各一份，
    #      nmbd 这类还在每个网卡地址上各绑一份），而界面按**服务**展示；
    #   2. 界面把协议当成"第二行为空时的兜底"来显示 —— 于是同地址同端口的
    #      一对 tcp/udp 套接字渲染成了两条**一模一样**的行。
    # 所以这组守卫盯两件事：后端真的合并了，前端真的把协议画出来了。
    group_go = read_text(os.path.join(repo_root, "internal", "discovery", "group.go"))
    scanner_go = read_text(os.path.join(repo_root, "internal", "discovery", "scanner.go"))
    port_types_go = read_text(os.path.join(repo_root, "internal", "discovery", "types.go"))
    ports_api_go = read_text(os.path.join(repo_root, "internal", "httpapi", "discovery.go"))
    discovery_js = read_text(os.path.join(web_root, "modules", "views", "discovery.js"))

    # 断言必须落在**调用点**上。只检查 "func GroupPorts(" 存在的话，
    # 把调用摘掉、把函数留成没人用的死代码，守卫照样全绿。
    listening_body = extract_fn_body(scanner_go, "func (s *Scanner) Listening(")
    check(
        "扫描结果在出口前会把同一服务的多个套接字合并成一行",
        "out = GroupPorts(out)" in listening_body,
        "不合并的话，同一个 sshd 只因为 IPv4/IPv6 各绑一份就显示成两条",
    )
    check(
        "合并排在容器关联之后（容器是按端口+协议索引的，合并后就只剩一个主协议）",
        listening_body.find("Enrich(") >= 0
        and listening_body.find("Enrich(") < listening_body.find("GroupPorts("),
        "先合并再关联容器，udp 那一半就再也匹配不上容器信息了",
    )
    # 「同一个服务」的身份只能是进程 + 端口。把协议或地址算进身份，
    # 同一服务的 tcp/udp 就永远合不到一起 —— 那正是用户看到的重复。
    key_body = extract_fn_body(group_go, "func groupKey(")
    check(
        "同一服务的身份是「进程 + 端口」，协议与地址都不算身份",
        bool(key_body) and "Protos" not in key_body and "Address" not in key_body
        and "p.Port" in key_body,
        "把协议算进身份，同地址同端口的一对 tcp/udp 就永远合不到一起",
    )
    # 「所有网卡 / 仅本机」是一个**判断**，必须只写一处。前端若自己去比
    # 字符串 "0.0.0.0"，两侧判据迟早不一致；而且 :: 在界面上会被当成"没有地址"。
    #
    # 引号形态两种都要拦：第一版只找双引号，而反向验证时用的补丁写的是
    # `=== '0.0.0.0'`（单引号）—— 守卫照样全绿，这条补丁白跑了。
    # 断言落成"文件里不许出现这两个地址的字面量"，与人家用什么引号无关。
    check(
        "「所有网卡 / 仅本机」的判断只在 Go 侧做一次（前端不许自己比字符串）",
        "func IsWildcardAddress(" in group_go
        and "IsWildcardAddress(a)" in extract_fn_body(group_go, "func ClassifyScope(")
        and re.search(r"""["'](?:0\.0\.0\.0|::)["']""", discovery_js) is None,
        "前后端各写一份判据，界面上那两种说法迟早对不上",
    )
    check(
        "合并结果把完整绑定地址与协议集合一起带出去",
        'json:"addresses,omitempty"' in port_types_go
        and 'json:"protos,omitempty"' in port_types_go
        and 'json:"scope,omitempty"' in port_types_go,
        "只给一个代表地址的话，界面永远说不出「这个服务对谁可见」——"
        "而 nmbd 那种绑了十一个网卡地址的服务，挑哪一个当代表都是误导",
    )
    # 协议必须**永远**显示出来。早先它只是第二行为空时的兜底，于是同地址同
    # 端口的一对 tcp/udp 渲染成一模一样的行。反向验证时把 `text: protoLabel(p)`
    # 换成 `text: ''`：类名还在、节点还在，只搜类名的守卫照样全绿 ——
    # 所以断言落在"真的把值传进去了"这一步。
    check(
        "协议被真的画进了端口号那一格（不是建了个空标签）",
        "h('span', { class: 'port-proto', text: protoLabel(p) })" in discovery_js,
        "同地址同端口的一对 tcp/udp 除了协议之外没有任何区别，协议藏起来就是"
        "「两条一模一样的行」；放在第二行末尾还会被 truncate 砍掉",
    )
    # 匹配落在**声明块**上，不能只搜类名：整条规则删掉、只留个 :empty 选择器，
    # `.port-proto` in style_code 依然为真。
    check(
        "协议标签有自己的样式，且为空时不占位",
        re.search(r"\.port-proto\s*\{[^}]*font-size:", style_code) is not None
        and re.search(r"\.port-proto:empty\s*\{[^}]*display:\s*none", style_code) is not None,
        "缺样式会把每一行撑开；空标签不收起则每行下面多出一条空白",
    )
    # 断言落在"返回值真的被用了"上：`ports, _ := ...Listening(...)` 时
    # 套接字数会被直接丢掉，而方法名、字段名都还在。
    check(
        "服务数与合并前的套接字一起报出来（少了一大截要能解释）",
        "Sockets int" in ports_api_go
        and re.search(r"ports,\s*sockets\s*:=\s*s\.deps\.Scanner\.Listening\(", ports_api_go) is not None
        and re.search(r"Sockets:\s*sockets", ports_api_go) is not None
        and "state.discovery.sockets" in discovery_js,
        "101 个套接字显示成二十来个服务，不说清楚会被当成「漏了一堆端口」",
    )
    # 「两行长得一模一样」是视觉事实，静态断言守不住 —— 得有人在真视口里
    # 把每一行的"身份"算出来去重。而且"改视口后重复变多"更是只有改视口
    # 再数一遍才能证伪。光有文件名常量不算接上：必须真的被当成参数传给 node。
    check(
        "端口行有没有重复由真浏览器回归守着（静态断言只能查到有没有标签）",
        os.path.isfile(os.path.join(repo_root, "scripts", "browser-discovery-check.js"))
        and "[node, DISCOVERY_SCRIPT, url]" in read_text(os.path.join(repo_root, "scripts", "browser-e2e.py")),
        "「协议藏起来了」和「协议显示了」在静态文本里都能找到类名，"
        "只有渲染出来才能判断两行是不是真的一样",
    )

    # ---- 打包脚本：两个「名字撞车 / 顺序」陷阱 --------------------------------
    # 这两条都是升版本号时才炸的雷，而且炸的方式是"打包直接挂了"，
    # 很难联想到版本号 —— 值得静态守一下。
    build_sh = read_text(os.path.join(repo_root, "scripts", "build-fpk.sh"))
    check(
        "打包脚本没有 CRLF 行尾",
        b"\r\n" not in open(os.path.join(repo_root, "scripts", "build-fpk.sh"), "rb").read(),
        "Windows 上用文本模式改 shell 脚本会写入 CRLF，真机 `#!/bin/sh\\r` 直接 "
        "bad interpreter —— 构建与安装钩子全挂",
    )
    check(
        "第三方工具 fnpack 的版本号独立成变量，不跟随本应用版本号",
        'FNPACK_VERSION="' in build_sh
        and 'local version="${FNPACK_VERSION}"' in build_sh,
        "两者曾经恰好都是 1.2.3 而写成同一个字面量；按「改版本号」批量替换后 "
        "fnpack 1.2.4 在官方静态资源上不存在 → 下载 404 → 打包整体失败",
    )
    # POSIX sh 是边读边执行的：函数定义排在调用点之后，调用处直接 command not found。
    # 这条兜底路径平时不走（fnpack 一直可用），于是一直没被发现是坏的。
    _def_at = build_sh.find("manual_pack() {")
    _call_at = build_sh.find('manual_pack "${STAGE_APP}"')
    check(
        "手工打包函数定义在调用点之前（兜底路径不能只是写着看）",
        _def_at >= 0 and _call_at >= 0 and _def_at < _call_at,
        f"定义于 {_def_at}、调用于 {_call_at} —— POSIX sh 边读边执行，"
        "定义在末尾会导致 command not found。兜底路径不试一次，就等于没有兜底",
    )

    # ---- 绑定形态与占用者诊断 --------------------------------------------
    # 这一组守的是一个真实故障："同样的设置，别的工具能跑，这里报端口被占用"。
    # 根因是绑定形态写死成 `:port`（Linux 上要求 IPv4 与 IPv6 两侧都空闲）。
    listen_go = read_text(os.path.join(repo_root, "internal", "forward", "listen.go"))
    tcp_go = read_text(os.path.join(repo_root, "internal", "forward", "tcp.go"))
    udp_go = read_text(os.path.join(repo_root, "internal", "forward", "udp.go"))
    procnet_go = read_text(os.path.join(repo_root, "internal", "forward", "procnet.go"))
    who_go = read_text(os.path.join(repo_root, "internal", "forward", "who_linux.go"))
    probe_go = read_text(os.path.join(repo_root, "internal", "forward", "probe.go"))
    manager_go = read_text(os.path.join(repo_root, "internal", "forward", "manager.go"))

    check(
        "留空监听地址时会退让绑定形态（双栈 → 仅 IPv4 → 仅 IPv6）",
        all(s in listen_go for s in ('proto + "4"', 'proto + "6"', "bindV4", "bindV6")),
        "只剩双栈一种候选时，端口被占住一侧就整体启动失败 —— 这正是用户报的那个故障",
    )
    check(
        "TCP / UDP 都走统一的绑定入口，不各自写裸 net.Listen",
        "listenTCP(" in tcp_go and "listenUDP(" in udp_go
        and 'net.Listen("tcp"' not in tcp_go and "net.ListenUDP(" not in udp_go,
        "绕过 listenTCP/listenUDP 就等于绕过了退让逻辑与占用者诊断",
    )
    check(
        "用户显式指定的监听地址不退让",
        "bindFixed" in listen_go and "host != \"\"" in listen_go,
        "替他改地址比如实报错更糟：他会以为自己配的地址生效了",
    )
    check(
        "失败时会查出占用者（Linux 直读 /proc，不依赖 ss / lsof）",
        "whoHoldsPort" in manager_go and "/proc/net/tcp" in who_go and "/proc/net/udp" in who_go,
        "只报「已被其它程序占用」，用户在遇到本应用自己占用时会去找一个不存在的「其它程序」",
    )
    check(
        "占用者若是本进程，提示必须指向本应用而不是 PID",
        "Self" in who_go and "占用者是本应用自己" in manager_go,
        "对用户报自己进程的 PID 等于没给线索",
    )
    check(
        "抓 /proc 里的端口必须补零成四位十六进制",
        'fmt.Sprintf("%04X", port)' in procnet_go,
        "内核把 53 写成 0035，按 %X 拼成 35 会永远匹配不上（这个 bug 被单测抓过）",
    )
    check(
        "退化为单栈时会告诉用户（状态里带 bind_mode）",
        "BindMode" in manager_go and "bind_mode" in fwd_view,
        "`[::]:18080` 在双栈与仅 IPv6 下字符串一样，不说出来用户会以为两侧都能连",
    )
    check(
        "端口占用检测：接口已注册且前端有入口",
        "GET /api/forwards/port-check" in server_go
        and "checkPort" in read_text(os.path.join(web_root, "modules", "api.js"))
        and "检查端口" in fwd_view,
        "用户需要能事先问「这个端口能不能用」，而不是等保存完再看一条失败提示",
    )
    check(
        "Manager 提供了只做冲突判断的入口 CheckListen",
        "func (m *Manager) CheckListen(" in manager_go,
        "Check 面向「保存一条完整规则」，会先要求名称非空，不能用来探测端口",
    )
    check(
        "端口检测只做冲突判断，不跑完整规则校验",
        # 必须限定在 handler 的**函数体**里断言。
        # 第一版写的是 `"CheckListen" in api_fwd`（整文件搜），结果反向验证
        # 时没有 FAIL —— 因为注释里也写着"这里刻意用 CheckListen"，
        # 换个调用方式注释还在，守卫照样通过。名字出现不等于调用发生。
        "CheckListen(" in extract_fn_body(api_fwd, "func (s *Server) handleCheckForwardPort(")
        and "checkForward(" not in extract_fn_body(api_fwd, "func (s *Server) handleCheckForwardPort("),
        "用 Check 会先要求规则名非空，探测一个端口号时返回「规则名称不能为空」"
        "（这个 bug 是探测接口的真实验证抓出来的）",
    )
    check(
        "探测结果会区分「双栈可用」与「只能单栈」",
        "Degraded" in probe_go and "只能退化为仅" in api_fwd,
        "无条件报「可以监听」会让用户以为两侧访客都能连上",
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
