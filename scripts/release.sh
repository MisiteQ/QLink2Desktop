#!/usr/bin/env bash
#
# QLink2Desktop —— 发版脚本
#
# 一次做完三件事：
#   1. 读 VERSION 文件，取其中的值作为**本次**发版的版本号；
#   2. 用它一次性构建 x86 / arm 两个包（同一版本号）；
#   3. 把 VERSION 文件写回 +1 的值，供下次发版使用。
#
# 为什么版本号递增放在这里，而不是放在 build-fpk.sh 里：
#   双架构要跑两次 build-fpk.sh，递增若写在里面，x86 和 arm 会拿到
#   两个不同的版本号 —— 同一批发版出现两个版本，设备上的版本比较
#   和"哪个包是最新的"就全乱了。
#
# 用法：
#   ./scripts/release.sh            # 构建双架构
#   ./scripts/release.sh x86        # 只构建 x86（版本号同样会递增一次）
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

# patch 位 +1：1.0.0 → 1.0.1。缺段自动补 0（1 → 1.0.1，1.0 → 1.0.1）。
bump_patch() {
    local major minor patch
    IFS='.' read -r major minor patch <<< "$1"
    major="${major:-0}"
    minor="${minor:-0}"
    patch="${patch:-0}"
    printf '%s.%s.%s\n' "${major}" "${minor}" "$((patch + 1))"
}

NEXT="$(bump_patch "${CURRENT}")"

echo "=== 发版版本号: ${CURRENT}（完成后 VERSION 文件写回 ${NEXT}）==="

for arch in "${ARCHES[@]}"; do
    VERSION="${CURRENT}" "${SCRIPT_DIR}/build-fpk.sh" "${arch}"
done

printf '%s\n' "${NEXT}" > "${VERSION_FILE}"

echo
echo "[OK] 发版完成: ${CURRENT}"
echo "     下次发版将使用: ${NEXT}"
echo "     产物:"
ls -lh "${ROOT_DIR}/${APP_NAME}-${CURRENT}"-*.fpk 2>/dev/null | sed 's/^/       /' || true
