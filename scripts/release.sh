#!/usr/bin/env bash
#
# QLink2Desktop —— 发版脚本
#
# 读 VERSION 文件，取其中的值作为**本次**发版的版本号，
# 用它一次性构建 x86 / arm 两个包（同一版本号）。
#
# 版本号策略（与 github-plus-plus / fnmonitor / GStats 等项目一致）：
#   VERSION 是版本号的唯一来源，脚本**只读不写**——发版后不会自动 +1。
#   要发新版本时，先手动把 VERSION（以及 manifest 等展示位）改成目标版本号，
#   再跑本脚本。这样仓库里不会出现「代码还没开发到、VERSION 却提前跳号」。
#
# 为什么双架构必须共用一个版本号：
#   x86 和 arm 属于同一批发版，版本号必须一致，
#   否则设备上的版本比较和「哪个包是最新的」就全乱了。
#
# 用法：
#   ./scripts/release.sh            # 构建双架构
#   ./scripts/release.sh x86        # 只构建 x86
#   ./scripts/release.sh x86 arm
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION_FILE="${ROOT_DIR}/VERSION"
APP_NAME="qlink2desktop"

ARCHES=("$@")
if [ ${#ARCHES[@]} -eq 0 ]; then
    ARCHES=(x86 arm)
fi

if [ ! -f "${VERSION_FILE}" ]; then
    echo "!! 缺少版本文件 ${VERSION_FILE}" >&2
    exit 1
fi

CURRENT="$(tr -d '[:space:]' < "${VERSION_FILE}")"

echo "=== 发版版本号: ${CURRENT}（VERSION 只读，发版后保持不变）==="

for arch in "${ARCHES[@]}"; do
    VERSION="${CURRENT}" "${SCRIPT_DIR}/build-fpk.sh" "${arch}"
done

echo
echo "[OK] 发版完成: ${CURRENT}"
echo "     下次发版请先手动修改 VERSION 文件"
echo "     产物:"
ls -lh "${ROOT_DIR}/${APP_NAME}-${CURRENT}"-*.fpk 2>/dev/null | sed 's/^/       /' || true
