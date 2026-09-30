"""交付包完整性复验：把 fpk 解开，逐项核对"包装进去的东西"与"源码里的东西"一致。

检查项都是真机上真的踩过或差一点踩到的：
版本号、架构、包结构、相对路径守卫、看门狗、import 图自检、
版本占位符、以及 app.js 的 import 布局。

两个刻意的写法，避免把"正确行为"误判成缺陷：

1. `?v=__V__` 是**请求时**由 handleSPA 替换的（见 internal/httpapi/server.go），
   嵌在二进制里的 index.html 本来就该保留占位符。所以这里校验的是
   "占位符在、且结构完好"，运行时是否真的被替换由网关等效 E2E 负责验证。

2. 可执行位从 **tar 归档成员**里读，而不是 os.stat()：
   Windows 上 os.stat 不体现 x 位，用它判断必然全部失败。
"""

import argparse
import glob
import os
import re
import sys
import tarfile

CASES = (("x86", 62), ("arm", 183))

FPK_RE = re.compile(r"^qlink2desktop-(?P<ver>[0-9][^-]*)-(?P<arch>x86|arm)\.fpk$")


def resolve_version(explicit: str | None) -> str:
    """确定要复验的版本号。

    刻意**不**从 VERSION 文件读：发版脚本构建完就把 VERSION 写回成下一个待用
    版本，此时按 VERSION 去找包必然找不到（这个坑真的踩过）。这里按目录里
    实际存在的产物反推；若同时存在多个版本，要求显式指定，避免验错包。
    """
    if explicit:
        return explicit
    found = {}
    for path in glob.glob(os.path.join(ROOT, "qlink2desktop-*.fpk")):
        m = FPK_RE.match(os.path.basename(path))
        if m:
            found.setdefault(m.group("ver"), set()).add(m.group("arch"))
    if not found:
        print("!! 仓库根目录下找不到 qlink2desktop-<版本>-<架构>.fpk，", file=sys.stderr)
        print("   请先执行 make release，或用 --version 指定版本。", file=sys.stderr)
        sys.exit(2)
    if len(found) > 1:
        print(f"!! 存在多个版本的产物：{sorted(found)}", file=sys.stderr)
        print("   请用 --version 指定要复验哪一个。", file=sys.stderr)
        sys.exit(2)
    return next(iter(found))


parser = argparse.ArgumentParser(description="QLink2Desktop 交付包复验")
parser.add_argument("--version", help="要复验的版本号（默认从现有产物反推）")
args = parser.parse_args()

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
VERSION = resolve_version(args.version)
print(f"复验版本：{VERSION}\n")


def modes(archive_path: str) -> dict[str, int]:
    """返回归档内 名字 -> 权限位（含目录，去掉 ./ 前缀）。"""
    with tarfile.open(archive_path) as tar:
        return {m.name.lstrip("./"): m.mode for m in tar.getmembers()}


ok = True
for tag, want_machine in CASES:
    fpk = os.path.join(ROOT, f"qlink2desktop-{VERSION}-{tag}.fpk")
    if not os.path.isfile(fpk):
        print(f"!! 缺少产物：{fpk}", file=sys.stderr)
        sys.exit(2)
    out = os.path.join(ROOT, ".build", "final", tag)
    os.makedirs(out, exist_ok=True)
    with tarfile.open(fpk) as tar:
        tar.extractall(out, filter="data")

    manifest = open(os.path.join(out, "manifest"), encoding="utf-8").read()
    ver = re.search(r"^version\s*=\s*(\S+)", manifest, re.M).group(1)
    plat = re.search(r"^platform\s*=\s*(\S+)", manifest, re.M).group(1)

    payload = os.path.join(out, "payload")
    with tarfile.open(os.path.join(out, "app.tgz")) as tar:
        tar.extractall(payload, filter="data")
    raw = open(os.path.join(payload, "qlink2desktop"), "rb").read()
    machine = int.from_bytes(raw[18:20], "little")

    anchor = raw.find(b'<p id="boot-version"')
    start = raw.rfind(b"<!DOCTYPE html>", 0, anchor)
    end = raw.find(b"</html>", anchor)
    html = raw[start : end + 7].decode("utf-8", "replace")

    fpk_modes = modes(fpk)
    app_modes = modes(os.path.join(out, "app.tgz"))
    cmd_main_mode = fpk_modes.get("cmd/main", 0)
    binary_mode = app_modes.get("qlink2desktop", 0)

    checks = [
        ("manifest 版本号 = " + VERSION, ver == VERSION, ver),
        ("manifest platform 与目标架构一致", plat == tag, plat),
        ("ELF 架构正确", machine == want_machine, f"{machine}（期望 {want_machine}）"),
        ("产物文件名带版本号", os.path.isfile(fpk), fpk),
        ("相对路径守卫进包（同步注入 base）", "createElement('base')" in html, ""),
        ("启动期看门狗进包", "__qlinkDiag" in html, ""),
        ("import 图自检进包", "取不到的文件" in html, ""),
        ("启动页版本号占位符完好（运行时替换）", 'id="boot-version"' in html and "v__V__" in html, ""),
        ("资源 URL 版本戳占位符完好", 'href="assets/style.css?v=__V__"' in html, ""),
        ("无根绝对路径资源", not re.search(r'"(?:src|href)="/(?!/)', html), ""),
        ("app.js 使用新 import 布局", b"from './modules/dom.js'" in raw, ""),
        ("app.js 不含旧扁平 import", b"from './dom.js'" not in raw, ""),
        # [hidden] 兜底规则必须进包。少了它，启动遮罩 .boot 会因为
        # 作者样式里的 display:flex 而永远藏不掉 —— 应用其实启动了，
        # 用户却一直看到加载页（真机事故，控制台干净、无从下手）。
        ("样式表含 [hidden] 兜底规则", b"[hidden] { display: none !important; }" in raw, ""),
    ]

    print(f"--- {fpk} ---")
    for name, good, extra in checks:
        print(f"  {'OK  ' if good else 'FAIL'} {name}" + (f"   [{extra}]" if extra else ""))
        ok = ok and good

    # 可执行位：归档里记的是 0666。
    #
    # 成因是构建宿主 —— Windows 的 NTFS 没有 x 位，fnpack 只能记 0666，
    # 脚本里的 chmod +x 在 NTFS 上不生效。在本仓库历史上每个包都是这样，
    # 而它们在真机上都能正常安装并启动（应用界面能打开就是 cmd/main 跑过的证据），
    # 说明飞牛侧并不依赖这个位。
    #
    # 因此：POSIX 宿主机上它是**真缺陷**（fnpack 本该记 0755），必须报错；
    # Windows 上只作提示，不判失败。
    exec_bits_ok = bool(cmd_main_mode & 0o111) and bool(binary_mode & 0o111)
    exec_detail = f"cmd/main={oct(cmd_main_mode)} payload={oct(binary_mode)}"
    if exec_bits_ok:
        print(f"  OK   cmd/main 与 payload 二进制带可执行位   [{exec_detail}]")
    elif os.name == "posix":
        print(f"  FAIL cmd/main 与 payload 二进制缺少可执行位   [{exec_detail}]")
        ok = False
    else:
        print(f"  WARN cmd/main 与 payload 二进制记录为 0666   [{exec_detail}]")
        print("       （Windows 构建宿主的必然结果：NTFS 无 x 位。真机实测不影响安装与启动）")

    files = ["manifest", "app.tgz", "ICON.PNG", "ICON_256.PNG"]
    dirs = ["cmd", "config", "wizard"]
    struct = all(os.path.getsize(os.path.join(out, f)) > 0 for f in files) and all(
        os.path.isdir(os.path.join(out, d)) for d in dirs
    )
    for name, good in (
        ("包结构（manifest/app.tgz/cmd/config/wizard/图标）", struct),
        ("manifest 含 checksum", "checksum" in manifest),
    ):
        print(f"  {'OK  ' if good else 'FAIL'} {name}")
        ok = ok and good
    print()

print("=== 全部通过 ===" if ok else "=== 存在失败项 ===")
sys.exit(0 if ok else 1)
