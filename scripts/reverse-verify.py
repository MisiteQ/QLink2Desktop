#!/usr/bin/env python3
"""反向验证：把每条新守卫对应的事实改坏，确认检查真的会 FAIL。

「通过」和「没在测」长得一模一样 —— 这个脚本存在的唯一目的就是把这两者分开。
每个场景独立打补丁、单独跑一遍静态守卫、统计 FAIL 条数，然后无条件还原。

注意一个本项目踩过的坑：e2e-gateway-check.py 会去找可执行文件，**找不到就直接
退出、一条断言都不跑** —— 那看起来和"全绿"完全一样。所以这里必须确认产物存在，
并且把"跑了多少条"和"失败多少条"一起打出来。

用法：
    make reverse-verify            静态守卫的反向验证（21 个场景）
    make reverse-verify-browser    真视口守卫的反向验证（4 个场景，跑得慢）
    python scripts/reverse-verify.py <场景名>...            只跑指定场景

为什么把它固化进仓库、而不是当一次性脚本用掉就删：
「改坏一份确认检查会 FAIL」如果每次都要现场手写，就一定会有人省掉它 ——
而这个项目已经不止一次遇到「守卫全绿，但用户报的问题依然存在」。
"""

from __future__ import annotations

import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE_CANDIDATES = [
    os.path.join(ROOT, "qlink2desktop.exe"),
    os.path.join(ROOT, ".build", "dev", "qlink2desktop.exe"),
]

# (场景名, 相对路径, 原文, 改成)
SCENARIOS: list[tuple[str, str, str, str]] = [
    # --- 解析链结构 ---
    (
        "ResolveIcon 不再委派给 ResolveIconEx（守卫会盯着没人调的壳）",
        "internal/fnos/icon.go",
        "func ResolveIcon(req IconRequest) IconSet { return ResolveIconEx(req).IconSet }",
        "func ResolveIcon(req IconRequest) IconSet { return DefaultIconSet() }",
    ),
    (
        "favicon 那一档挪到官方图标库之前",
        "internal/fnos/icon.go",
        """		if set, keyword, ok := matchOfficialIconKeyword(req.Candidates); ok {
			return done(set, IconSourceOfficial,
				fmt.Sprintf("命中官方图标库（关键词 %s）", keyword))
		}
		if set, detail, ok := siteFaviconLookup(req.SiteURL, req.SkipTLSVerify); ok {
			return done(set, IconSourceFavicon, detail)
		}""",
        """		if set, detail, ok := siteFaviconLookup(req.SiteURL, req.SkipTLSVerify); ok {
			return done(set, IconSourceFavicon, detail)
		}
		if set, keyword, ok := matchOfficialIconKeyword(req.Candidates); ok {
			return done(set, IconSourceOfficial,
				fmt.Sprintf("命中官方图标库（关键词 %s）", keyword))
		}""",
    ),
    (
        "favicon 抓取不再受「允许远端图标」管辖（偷偷发内网请求）",
        "internal/fnos/icon.go",
        "	if req.AllowRemote {\n		if img := loadFromInline(req.Icon.Ref); img != nil {",
        "	if true {\n		if img := loadFromInline(req.Icon.Ref); img != nil {",
    ),
    (
        "官方图标库匹配去掉总预算（最坏挂几分钟）",
        "internal/fnos/icon.go",
        "	ctx, cancel := context.WithTimeout(context.Background(), officialIconBudget)",
        "	ctx, cancel := context.WithCancel(context.Background())",
    ),
    (
        "官方图标库匹配不再回报命中的关键词",
        "internal/fnos/icon.go",
        "				return set, name, true",
        "				return set, \"\", true",
    ),
    # --- 过程可见 ---
    (
        "解析结果只剩图标，不再带来源与说明",
        "internal/fnos/icon.go",
        "	Source IconSource\n	// Detail 是一句给用户看的话：命中了什么关键词、问了哪个地址、为什么没找到。\n	Detail string",
        "	Origin string",
    ),
    (
        "抓取失败不再说明问了哪个地址",
        "internal/fnos/icon.go",
        "		return done(DefaultIconSet(), IconSourceDefault, faviconMissDetail(req.SiteURL))",
        "		return done(DefaultIconSet(), IconSourceDefault, \"没找到图标\")",
    ),
    (
        "离线模式不再说明是开关关掉了抓取",
        "internal/fnos/icon.go",
        "		\"远端图标已关闭，未去站点查找；如需自动抓取请在设置里打开「允许远端图标」\")",
        "		\"没找到图标\")",
    ),
    (
        "解析接口不再宽松解码（链接多一个字段就把预览打挂）",
        "internal/httpapi/icons.go",
        "	if !decodeJSONLenient(w, r, &req) {",
        "	if !decodeJSON(w, r, &req) {",
    ),
    (
        "解析接口不再回预览图",
        "internal/httpapi/icons.go",
        '		body["preview"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(res.Large)',
        '		_ = res.Large',
    ),
    (
        "解析接口的响应丢掉 message / detail",
        "internal/httpapi/icons.go",
        '		"detail":     res.Detail,',
        '		"x-detail":   res.Detail,',
    ),
    (
        "前端不再调用解析接口（接口写了没人用）",
        "internal/webui/web/assets/modules/views/editor.js",
        "      const res = await api.icons.resolve(toPayload(model));",
        "      const res = { source: 'default', message: '未找到图标', detail: '' };",
    ),
    (
        "编辑器不再显示「正在查找」",
        "internal/webui/web/assets/modules/views/editor.js",
        "    h('span', { class: 'tag busy' }, [h('span', { class: 'spinner-sm' }), '正在查找图标…']),",
        "    h('span', { class: 'tag busy' }, ['…']),",
    ),
    (
        "状态行建了却没挂进界面",
        "internal/webui/web/assets/modules/views/editor.js",
        "          statusBox,\n        ]),",
        "        ]),",
    ),
    (
        "改地址不再触发重新查找",
        "internal/webui/web/assets/modules/views/editor.js",
        "    retarget: scheduleLookup,",
        "    retarget: () => {},",
    ),
    (
        "状态行样式被删掉",
        "internal/webui/web/assets/style.css",
        ".icon-status { display: inline-flex; align-items: center; gap: 8px; flex-wrap: wrap; }",
        "",
    ),
    # --- 列表缩略图 ---
    (
        "自动图标不再落盘（列表拿不到真实图标）",
        "internal/fnos/service.go",
        "	res := s.ResolveLinkIcon(l)\n\n	changed := s.writeAutoIcon(dir, name, res)",
        "	res := s.ResolveLinkIcon(l)\n\n	changed := false\n	_ = s.writeAutoIcon",
    ),
    (
        "视图不再带上自动图标文件名",
        "internal/httpapi/server.go",
        "		views[i].IconAuto = s.deps.Service.AutoIconFile(l)",
        "		views[i].IconAuto = \"\"",
    ),
    (
        "列表缩略图不再用解析出来的图标",
        "internal/webui/web/assets/modules/views/desktop.js",
        "  if (view.icon_auto) return api.icons.url(view.icon_auto);",
        "  if (view.icon_auto) return api.icons.defaultURL(64);",
    ),
    (
        "自动图标混进用户图标库",
        "internal/httpapi/icons.go",
        "		if fnos.IsAutoIconFile(e.Name()) {\n			continue\n		}",
        "",
    ),
    (
        "浏览器回归脚本被摘掉（过程可见失去真视口证据）",
        "scripts/browser-e2e.py",
        "                [node, ICON_SCRIPT, url],",
        "                [node, MOBILE_SCRIPT, url],",
    ),
]


# 真视口那一层单独验：这里的事实只有在浏览器里才看得见。
# 每条跑一次完整 browser-e2e（约两三分钟），所以只放最要紧的几条。
# (场景名, 相对路径, 原文, 改成, 是否要重新编译)
BROWSER_SCENARIOS: list[tuple[str, str, str, str, bool]] = [
    # 注意：下面的 need_build 一律为 True。
    #
    # 前端资源是被 //go:embed 编进二进制的，改了磁盘上的 js/css **不重新编译
    # 等于没改** —— 浏览器跑的仍是旧代码。第一次跑这几条时全部 MISS，
    # 差一步就得出"浏览器守卫形同虚设"的错误结论，实际只是补丁没进二进制。
    # （同一个道理，出包之后必须解包 grep 确认新代码真的在里面。）
    (
        "编辑器不再显示「正在查找图标…」（用户要的那句话没了）",
        "internal/webui/web/assets/modules/views/editor.js",
        "'正在查找图标…'",
        "'请稍候'",
        True,
    ),
    (
        "状态行建了却没挂进界面",
        "internal/webui/web/assets/modules/views/editor.js",
        "          statusBox,\n        ]),",
        "        ]),",
        True,
    ),
    (
        "「没找到」被画成成功色（比不提示更糟）",
        "internal/webui/web/assets/modules/views/editor.js",
        "class: `tag ${ok ? 'ok' : 'warn'}`",
        "class: `tag ${ok ? 'ok' : 'ok'}`",
        True,
    ),
    (
        "解析接口改回严格解码（真机上就是这么挂的）",
        "internal/httpapi/icons.go",
        "	if !decodeJSONLenient(w, r, &req) {",
        "	if !decodeJSON(w, r, &req) {",
        True,
    ),
]


def pick_exe() -> str | None:
    for p in EXE_CANDIDATES:
        if os.path.isfile(p):
            return p
    return None


def run_static(exe: str) -> tuple[int, int, str]:
    # 必须显式指定子进程的输出编码：Windows 上默认是 GBK，
    # 按 utf-8 读回来中文会全变成替换字符，汇总行就再也匹配不上 ——
    # 表现是"基线都没跑起来"，与"守卫真的坏了"完全分不清。
    env = dict(os.environ, PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        [sys.executable, os.path.join(ROOT, "scripts", "e2e-gateway-check.py"), "--exe", exe],
        capture_output=True, text=True, cwd=ROOT,
        encoding="utf-8", errors="replace", env=env,
    )
    out = proc.stdout + proc.stderr
    # 汇总行里夹着 ANSI 颜色码（`通过 219 项<ESC>[0m，失败 0 项`）。
    # 不剥掉的话正则永远匹配不上，脚本会一口咬定"基线没跑起来" ——
    # 又一个"看起来在测、其实一条都没测"的同型陷阱。
    clean = re.sub(r"\x1b\[[0-9;]*m", "", out)
    m = re.search(r"通过\s*(\d+)\s*项，失败\s*(\d+)\s*项", clean)
    if not m:
        return -1, -1, out
    return int(m.group(1)), int(m.group(2)), out


def run_browser(exe: str) -> tuple[int, str]:
    """跑一遍浏览器端到端，返回 (FAIL 条数, 原始输出)。"""
    env = dict(os.environ, PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        [sys.executable, os.path.join(ROOT, "scripts", "browser-e2e.py"),
         "--exe", exe, "--expect", "ok"],
        capture_output=True, text=True, cwd=ROOT,
        encoding="utf-8", errors="replace", env=env, timeout=900,
    )
    out = proc.stdout + proc.stderr
    clean = re.sub(r"\x1b\[[0-9;]*m", "", out)
    fails = [ln.strip() for ln in clean.splitlines() if ln.strip().startswith("FAIL")]
    return len(fails), out


def rebuild(exe: str) -> bool:
    """改了 Go 代码就得重新编译 —— 否则浏览器验的还是旧二进制。"""
    env = dict(os.environ, PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        ["go", "build", "-trimpath", "-ldflags", "-s -w -X main.version=1.2.6",
         "-o", exe, "./cmd/server"],
        capture_output=True, text=True, cwd=ROOT, env=env, timeout=600,
    )
    if proc.returncode != 0:
        print(proc.stdout, proc.stderr)
    return proc.returncode == 0


def failing_names(out: str) -> list[str]:
    names = []
    in_list = False
    out = re.sub(r"\x1b\[[0-9;]*m", "", out)
    for line in out.splitlines():
        if "失败明细" in line:
            in_list = True
            continue
        if in_list:
            s = line.strip()
            if s.startswith("- "):
                names.append(s[2:].split(" ::")[0].strip())
            elif s:
                in_list = False
    return names


def run_browser_scenarios(exe: str, only: set[str]) -> int:
    scenarios = [s for s in BROWSER_SCENARIOS if not only or s[0] in only]

    base_fails, out = run_browser(exe)
    clean = re.sub(r"\x1b\[[0-9;]*m", "", out)
    if "全部通过" not in clean and base_fails == 0:
        print("!! 浏览器基线没跑起来（既没报全部通过，也没有 FAIL 行）。")
        print(clean[-800:])
        return 2
    print(f"浏览器基线：FAIL {base_fails} 条\n")

    bad: list[str] = []
    for name, rel, old, new, need_build in scenarios:
        path = os.path.join(ROOT, rel)
        with open(path, "rb") as f:
            original = f.read()
        text = original.decode("utf-8")
        if old not in text:
            print(f"  SKIP  {name}\n        补丁未命中（原文对不上）：{rel}")
            bad.append(f"{name}（补丁未命中）")
            continue

        try:
            with open(path, "wb") as f:
                f.write(text.replace(old, new, 1).encode("utf-8"))
            if need_build and not rebuild(exe):
                print(f"  SKIP  {name}\n        重新编译失败")
                bad.append(f"{name}（编译失败）")
                continue
            n_fail, bout = run_browser(exe)
            bclean = re.sub(r"\x1b\[[0-9;]*m", "", bout)
            fails = [ln.strip() for ln in bclean.splitlines() if ln.strip().startswith("FAIL")]
            print(f"  {'OK   ' if n_fail > base_fails else 'MISS '} {name}")
            print(f"        FAIL {base_fails} → {n_fail}")
            for f_ in fails[:3]:
                print(f"        · {f_}")
            if n_fail <= base_fails:
                bad.append(name)
        finally:
            with open(path, "wb") as f:
                f.write(original)
            if need_build:
                rebuild(exe)

    print()
    if bad:
        print(f"以下场景没能让浏览器守卫 FAIL（{len(bad)} 条）：")
        for b in bad:
            print(f"  - {b}")
        return 1
    print(f"全部 {len(scenarios)} 个真视口场景都让守卫真的 FAIL 了。")
    return 0


def main() -> int:
    args = [a for a in sys.argv[1:]]
    browser_mode = "--browser" in args
    only = {a for a in args if not a.startswith("--")}

    exe = pick_exe()
    if not exe:
        print("!! 找不到可执行文件，先 `make build`。")
        print("   注意：找不到产物时守卫会直接退出、一条都不跑 —— 那看起来和全绿一模一样。")
        return 2
    print(f"产物：{exe}\n")

    if browser_mode:
        return run_browser_scenarios(exe, only)

    scenarios = [s for s in SCENARIOS if not only or s[0] in only]
    total, failed_total, out = run_static(exe)
    if total <= 0:
        print("!! 基线都没跑起来，先修好再谈反向验证。")
        print("--- 原始输出（末尾 800 字） ---")
        print(out[-800:])
        print("--- 结束 ---")
        return 2
    print(f"基线：通过 {total} 项，失败 {failed_total} 项\n")

    bad: list[str] = []
    for name, rel, old, new in scenarios:
        path = os.path.join(ROOT, rel)
        with open(path, "rb") as f:
            original = f.read()
        text = original.decode("utf-8")
        if old not in text:
            print(f"  SKIP  {name}\n        补丁未命中（原文对不上）：{rel}")
            bad.append(f"{name}（补丁未命中）")
            continue

        try:
            # 一律二进制回写：本项目所有文件都是 LF，文本模式会把 \n 变 \r\n，
            # shell 脚本会变成 bad interpreter、Go 文件会被 gofmt 判为未格式化。
            with open(path, "wb") as f:
                f.write(text.replace(old, new, 1).encode("utf-8"))
            n_pass, n_fail, out = run_static(exe)
            names = failing_names(out)
            hit = any(name[:12] in n for n in names) or n_fail > failed_total
            mark = "OK   " if n_fail > failed_total else "MISS "
            print(f"  {mark} {name}\n        FAIL {failed_total} → {n_fail}（通过 {n_pass}）")
            for n in names[:3]:
                print(f"        · {n}")
            if n_fail <= failed_total:
                bad.append(name)
        finally:
            with open(path, "wb") as f:
                f.write(original)
            os.remove(backup)

    print()
    if bad:
        print(f"以下场景没能让守卫 FAIL（{len(bad)} 条）—— 守卫可能是形同虚设的：")
        for b in bad:
            print(f"  - {b}")
        return 1
    print(f"全部 {len(scenarios)} 个场景都让守卫真的 FAIL 了。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
