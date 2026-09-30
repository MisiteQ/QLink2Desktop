#!/usr/bin/env python3
"""校验前端 ES Module 的 import 路径，以及 index.html 内联脚本的语法。

两件事，都对应真机踩过的坑：

1. import 路径：node --check 只验证语法，不解析 import 目标 —— app.js 曾把
   './modules/dom.js' 写成 './dom.js'，语法全对、真机全 404。本脚本解析所有
   .js 里的静态 import / export from，逐条验证相对路径在磁盘上真实存在。

2. index.html 的第一段内联脚本：它是「相对路径基准守卫 + 启动期看门狗」，
   刻意不依赖任何外部文件（它要处理的故障恰恰是外部文件加载不到）。
   反过来说，它自己有语法错误就没有任何兜底 —— 守卫和看门狗会一起失效，
   页面变回只转圈、什么都不说。常规的 .js 语法检查覆盖不到内联脚本，
   所以在这里单独把它抽出来交给 node --check。
"""
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

WEB = Path(__file__).resolve().parent.parent / "internal" / "webui" / "web"
ROOT = WEB
IMPORT_RE = re.compile(
    r"""\b(?:import|export)\s+[^'"]*?from\s+['"]([^'"]+)['"]"""
    r"""|\bimport\s*\(\s*['"]([^'"]+)['"]\s*\)"""
    r"""|\bimport\s+['"]([^'"]+)['"]"""
)

failures = 0
checked = 0
for js in sorted(ROOT.rglob("*.js")):
    text = js.read_text(encoding="utf-8")
    for m in IMPORT_RE.finditer(text):
        spec = next(g for g in m.groups() if g)
        if not spec.startswith("."):
            continue  # 裸模块名（CDN 等）跳过
        checked += 1
        target = (js.parent / spec).resolve()
        if not target.is_file():
            print(f"[缺失] {js.relative_to(ROOT)} -> {spec}")
            failures += 1

print(f"校验 {checked} 条相对 import")

# ---- index.html 内联脚本语法 ------------------------------------------------
inline_checked = 0
node = shutil.which("node")
index_html = ROOT / "index.html"
if not node:
    print("(未找到 node，跳过内联脚本语法校验)")
elif not index_html.is_file():
    print(f"[缺失] {index_html} 不存在")
    failures += 1
else:
    html = index_html.read_text(encoding="utf-8")
    blocks = re.findall(r"<script>(.*?)</script>", html, re.S)
    if not blocks:
        print("[缺失] index.html 里找不到内联脚本（相对路径基准守卫+看门狗）")
        failures += 1
    for i, js in enumerate(blocks):
        inline_checked += 1
        with tempfile.NamedTemporaryFile("w", suffix=".js", delete=False, encoding="utf-8") as fh:
            fh.write(js)
            tmp = fh.name
        try:
            proc = subprocess.run([node, "--check", tmp], capture_output=True, text=True)
        finally:
            Path(tmp).unlink(missing_ok=True)
        if proc.returncode != 0:
            print(f"[语法错误] index.html 第 {i + 1} 段内联脚本：")
            print(proc.stderr.strip())
            failures += 1
    print(f"校验 {inline_checked} 段 index.html 内联脚本")

if failures:
    print(f"FAIL: {failures} 项不通过")
    sys.exit(1)
print("OK: import 路径与内联脚本均有效")
