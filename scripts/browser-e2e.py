#!/usr/bin/env python3
"""QLink2Desktop —— 真浏览器启动期验证。

为什么还需要它：网关等效 E2E（e2e-gateway-check.py）只能断言「服务端送出了
什么」，而真机上翻车的往往是「浏览器拿它做了什么」。本脚本用真实
Chromium 系浏览器（Edge 即可）打开页面，等看门狗窗口过去，再读回页面
**实际显示**的内容：版本号、失败资源清单、有没有停在启动态。

它专门覆盖一次真实事故：新包装上后浏览器仍在跑上一版 app.js，
模块全部 404，界面停在启动页，而服务端一切正常。

用法：
    python scripts/browser-e2e.py --exe .build/dev/qlink2desktop.exe --expect ok
    python scripts/browser-e2e.py --exe .build/dev/qlink2d-degraded.exe --expect diagnostic

    --expect ok         期望正常进入应用（启动页被隐藏）
    --expect diagnostic 期望看门狗把失败证据写到页面上
                        （用于验证「诊断能力本身」不是空壳）

依赖：node + playwright-core，以及本机 Chromium 系浏览器。
缺依赖时脚本明确报错，不静默跳过。
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NODE_SCRIPT = os.path.join(ROOT, "scripts", "browser-boot-check.js")
E2E_SCRIPT = os.path.join(ROOT, "scripts", "e2e-gateway-check.py")

# 看门狗在 12 秒触发，浏览器侧等 15 秒留出余量。
BROWSER_WAIT_MS = 15000

GREEN = "\033[32m"
RED = "\033[31m"
BOLD = "\033[1m"
RESET = "\033[0m"


def load_e2e_module():
    """复用 e2e-gateway-check.py 里的网关模拟器，避免两处实现各自漂移。"""
    spec = importlib.util.spec_from_file_location("qlink_e2e", E2E_SCRIPT)
    if spec is None or spec.loader is None:  # pragma: no cover - 环境异常
        raise RuntimeError(f"无法加载 {E2E_SCRIPT}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def find_node() -> str | None:
    node = shutil.which("node")
    if node:
        return node
    # 托管运行时（本机默认布局）兜底。
    base = os.path.join(os.path.expanduser("~"), ".workbuddy", "binaries", "node", "versions")
    if os.path.isdir(base):
        for name in sorted(os.listdir(base), reverse=True):
            cand = os.path.join(base, name, "node.exe")
            if os.path.isfile(cand):
                return cand
    return None


def node_env() -> dict[str, str]:
    env = dict(os.environ)
    workspace = os.path.join(
        os.path.expanduser("~"), ".workbuddy", "binaries", "node", "workspace", "node_modules"
    )
    if os.path.isdir(workspace):
        existing = env.get("NODE_PATH", "")
        env["NODE_PATH"] = workspace + (os.pathsep + existing if existing else "")
    return env


def check(ok: bool, label: str, detail: str = "") -> bool:
    mark = f"{GREEN}PASS{RESET}" if ok else f"{RED}FAIL{RESET}"
    print(f"  {mark}  {label}" + (f"    {detail}" if detail and not ok else ""))
    return ok


def main() -> int:
    parser = argparse.ArgumentParser(description="QLink2Desktop 真浏览器启动期验证")
    parser.add_argument("--exe", default=".build/dev/qlink2desktop.exe", help="后端可执行文件路径")
    parser.add_argument(
        "--expect",
        choices=("ok", "diagnostic"),
        default="ok",
        help="ok=应正常进入应用；diagnostic=应把失败证据显示在启动页上",
    )
    args = parser.parse_args()

    exe = os.path.abspath(args.exe)
    if not os.path.isfile(exe):
        print(f"!! 找不到可执行文件：{exe}", file=sys.stderr)
        return 2

    node = find_node()
    if not node:
        print("!! 找不到 node，无法驱动浏览器验证。", file=sys.stderr)
        return 2

    e2e = load_e2e_module()
    data_dir = tempfile.mkdtemp(prefix="qlink2d-browser-")

    print(f"{BOLD}QLink2Desktop 真浏览器启动期验证{RESET}")
    print(f"exe = {exe}\n期望 = {args.expect}\n")

    backend_port = e2e.free_port()
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

    gw = None
    failures = 0
    try:
        if not e2e.wait_backend(backend_port):
            print("!! 后端 20 秒内未就绪，放弃。", file=sys.stderr)
            return 2

        gw = e2e.GatewaySimulator(backend_port, strip_prefix=True)
        gw.start()

        # 故意不带尾斜杠：复刻真机上「网关吃掉配置里的尾斜杠」的形态，
        # 这样连前端的尾斜杠守卫一起验了。
        url = f"http://127.0.0.1:{gw.port}{e2e.GATEWAY_PREFIX}"
        print(f"打开 {url}（不带尾斜杠，模拟网关形态）\n")

        result = subprocess.run(
            [node, NODE_SCRIPT, url, str(BROWSER_WAIT_MS)],
            capture_output=True,
            text=True,
            env=node_env(),
            timeout=120,
        )
        if result.returncode != 0:
            print(result.stdout)
            print(result.stderr, file=sys.stderr)
            print("!! 浏览器验证脚本执行失败。", file=sys.stderr)
            return 2

        snap = None
        for chunk in (result.stdout,):
            start = chunk.find("{")
            if start >= 0:
                snap = json.loads(chunk[start:])
        if snap is None:
            print(result.stdout)
            print("!! 无法解析浏览器输出。", file=sys.stderr)
            return 2

        print(f"最终路径      : {snap['finalUrl']}")
        print(f"启动页版本号  : {snap['versionText']!r}")
        print(f"进入应用      : {snap['shellVisible']}")
        print(f"停在启动页    : {snap['bootVisible']}")
        print(f"诊断详情可见  : {snap['detailVisible']}")
        print(f"重试按钮可见  : {snap['retryVisible']}")
        print(
            f"计算样式      : boot={snap['bootDisplay']} app={snap['appDisplay']} "
            f"retry={snap['retryDisplay']} spinner={snap['spinnerDisplay']}"
        )
        print(f"hidden 是否真的隐藏 : {snap['hiddenCssWorks']}")
        print(f"看门狗判定    : 入口模块失败={snap['diagEntryFailed']} 其他资源失败={snap['diagFailed']}")
        print(f"HTTP 4xx/5xx  : {snap['httpErrors']}")
        if snap["detailText"]:
            print("--- 页面上显示的内容 ---")
            print(snap["detailText"])
            print("------------------------")
        print()

        v = snap["versionText"] or ""
        if not check(v.startswith("v") and v not in ("v", "v__V__"), "启动页显示了真实版本号", f"版本号={v!r}"):
            failures += 1

        # 这条是"页面自身是否健康"的底线：hidden 属性必须真的能隐藏元素。
        # [hidden] 的 display:none 来自浏览器默认样式表，作者样式里任何一条
        # 设置 display 的规则都会把它压过去（.boot{display:flex} 尤其致命，
        # 遮罩会永远盖着应用）。只看 el.hidden 属性是查不出这个问题的。
        if not check(
            snap["hiddenCssWorks"] is True,
            "hidden 属性真的能隐藏元素（页面有 [hidden] 兜底规则）",
            f"bootDisplay={snap['bootDisplay']} appDisplay={snap['appDisplay']}",
        ):
            failures += 1

        if args.expect == "ok":
            # 必须按**计算样式**判断：属性为 true 但元素仍在屏幕上显示过，
            # 正是「应用其实跑起来了却一直看到加载页」的那次事故。
            if not check(
                snap["shellVisible"] and not snap["bootVisible"],
                "正常进入应用（启动遮罩真的消失了）",
                f"bootDisplay={snap['bootDisplay']} shellVisible={snap['shellVisible']} bootVisible={snap['bootVisible']}",
            ):
                failures += 1
            if not check(
                not snap["retryVisible"] and snap["spinnerHidden"],
                "重试按钮与转圈处于初始隐藏态",
                f"retryVisible={snap['retryVisible']} spinnerHidden={snap['spinnerHidden']}",
            ):
                failures += 1
            if not check(not snap["detailVisible"], "没有误报诊断信息", f"detail={snap['detailText']!r}"):
                failures += 1
        else:
            if not check(snap["bootVisible"] and not snap["shellVisible"], "停在启动页（复刻故障场景）"):
                failures += 1
            if not check(snap["detailVisible"] and bool(snap["detailText"]), "看门狗把证据显示在了页面上"):
                failures += 1
            text = snap["detailText"] or ""
            if not check(
                snap["diagEntryFailed"] or "没有捕获到资源加载失败" in text,
                "看门狗判定了失败性质（入口模块未执行 / 无资源失败）",
                f"entryFailed={snap['diagEntryFailed']}",
            ):
                failures += 1
            # 这是本脚本存在的核心理由：浏览器自己不会告诉你「哪个文件 404」，
            # 看门狗必须把 import 图里取不到的文件一个个列出来。
            if not check(
                "取不到的文件" in text and "404" in text,
                "详情里列出了具体取不到的文件（含 404 状态）",
                f"detail={text!r}",
            ):
                failures += 1

    finally:
        if gw is not None:
            try:
                gw.stop()
            except Exception:  # noqa: BLE001
                pass
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:  # pragma: no cover
            proc.kill()

    print()
    if failures:
        print(f"{RED}{BOLD}失败 {failures} 项{RESET}")
        return 1
    print(f"{GREEN}{BOLD}全部通过{RESET}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
