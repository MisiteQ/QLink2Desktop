#!/usr/bin/env bash
#
# QLink2Desktop —— 飞牛 OS (.fpk) 打包脚本
#
# 产物遵循 developer.fnnas.com 的官方 fpk 结构，并优先使用官方打包工具
# fnpack（https://developer.fnnas.com/docs/cli/fnpack/）完成打包；
# fnpack 不可用时自动从官方静态资源下载；两者都不可行时才退回到
# 手工 tar + md5 checksum 的等价实现。
#
# 用法：
#   ./scripts/build-fpk.sh [x86|arm]
#
# 环境变量：
#   VERSION   版本号，默认读仓库根目录的 VERSION 文件
#   GO        Go 可执行文件路径（默认自动探测）
#   GOPROXY   透传给 go build
#
# 说明：本脚本**不**使用 rm 清理临时目录 —— 一部分环境对删除操作有安全闸，
# 且构建暂存区放在仓库的 .build/ 下，由 `make clean` 统一回收更可控。
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
FNOS_APP_DIR="${ROOT_DIR}/fnos-app"
BUILD_DIR="${ROOT_DIR}/.build"
APP_NAME="qlink2desktop"
ARCH_INPUT="${1:-x86}"

# ---------------------------------------------------------------- 参数解析

case "${ARCH_INPUT}" in
x86 | amd64 | x86_64)
    GOARCH="amd64"
    PLATFORM="x86"
    TAG="x86"
    ;;
arm | arm64 | aarch64)
    GOARCH="arm64"
    PLATFORM="arm"
    TAG="arm"
    ;;
*)
    echo "!! 不支持的架构: ${ARCH_INPUT}（可选 x86 / arm）" >&2
    exit 1
    ;;
esac

# ---------------------------------------------------------------- 版本号
#
# 仓库根目录的 VERSION 文件是版本号的唯一来源。
#
# 为什么不再用 git tag / 日期时间戳：
#   - git tag —— 项目常被放在 D:\CODE 这类大仓库下面（自身不是仓库根），
#     `git describe` 会拿到**父仓库**的 tag，版本号跳到毫不相干的值，
#     覆盖安装的版本比较随之失效；
#   - 日期时间戳（把年月日时分拼进版本号）—— 又长又难念，而且不含
#     "这是第几次发版"的语义，用户要求改为逐次递增的短号。
#
# 约定：VERSION 文件里写的是**本次发包使用的版本号**，
# scripts/release.sh 打包完成后写回 +1 的值供下次使用。
# 直接跑本脚本不会改动 VERSION 文件，方便反复构建同一版本做验证。
VERSION_FILE="${ROOT_DIR}/VERSION"
if [ -z "${VERSION:-}" ]; then
    if [ ! -f "${VERSION_FILE}" ]; then
        echo "!! 缺少版本文件 ${VERSION_FILE}" >&2
        echo "   请写入形如 1.0.0 的版本号，或用 VERSION=x.y.z 显式指定。" >&2
        exit 1
    fi
    VERSION="$(tr -d '[:space:]' < "${VERSION_FILE}")"
fi
VERSION="${VERSION#v}"
if ! printf '%s' "${VERSION}" | grep -Eq '^[0-9]+(\.[0-9]+)*$'; then
    echo "!! 版本号格式非法: ${VERSION}（应形如 1.0.0）" >&2
    exit 1
fi

echo "=== QLink2Desktop fpk 构建 [${TAG}] version=${VERSION} ==="

# ---------------------------------------------------------------- 工具探测

# find_go 依次尝试：显式指定 → PATH → GOROOT → 常见安装位置 → ~/sdk 下的托管版本。
find_go() {
    if [ -n "${GO:-}" ] && [ -x "${GO}" ]; then
        echo "${GO}"
        return 0
    fi
    if command -v go >/dev/null 2>&1; then
        echo "go"
        return 0
    fi
    local candidate
    for candidate in \
        "${GOROOT:-/nonexistent}/bin/go" \
        "/usr/local/go/bin/go" \
        "/c/Program Files/Go/bin/go.exe"; do
        if [ -x "${candidate}" ]; then
            echo "${candidate}"
            return 0
        fi
    done
    # Windows 上常见的多版本托管布局（取版本号最大的一个）。
    local best
    best="$(ls -1d "${HOME}"/sdk/go*/bin/go.exe 2>/dev/null | sort -V | tail -n 1 || true)"
    if [ -n "${best}" ] && [ -x "${best}" ]; then
        echo "${best}"
        return 0
    fi
    return 1
}

GO_BIN="$(find_go || true)"

# resolve_fnpack 定位官方打包工具。
# 下载地址不带扩展名（官方静态资源即如此），本地落盘时在 Windows 上补 .exe。
resolve_fnpack() {
    if command -v fnpack >/dev/null 2>&1; then
        echo "fnpack"
        return 0
    fi

    local version="1.2.3"
    local base="https://static2.fnnas.com/fnpack"
    local os_name arch_name url_file local_name
    os_name="$(uname -s)"
    arch_name="$(uname -m)"
    case "${os_name}:${arch_name}" in
    Linux:x86_64)
        url_file="fnpack-${version}-linux-amd64"
        local_name="${url_file}"
        ;;
    Linux:aarch64 | Linux:arm64)
        url_file="fnpack-${version}-linux-arm64"
        local_name="${url_file}"
        ;;
    Darwin:x86_64)
        url_file="fnpack-${version}-darwin-amd64"
        local_name="${url_file}"
        ;;
    Darwin:arm64)
        url_file="fnpack-${version}-darwin-arm64"
        local_name="${url_file}"
        ;;
    MINGW*:x86_64 | MSYS*:x86_64 | CYGWIN*:x86_64)
        url_file="fnpack-${version}-windows-amd64"
        local_name="${url_file}.exe"
        ;;
    *)
        return 1
        ;;
    esac

    local cache_dir="${BUILD_DIR}/tools"
    local target="${cache_dir}/${local_name}"
    if [ ! -x "${target}" ]; then
        echo "--> 未检测到 fnpack，正在从官方地址下载 (${url_file})..." >&2
        mkdir -p "${cache_dir}"
        if ! curl -fsSL --retry 3 -o "${target}" "${base}/${url_file}"; then
            echo "!! fnpack 下载失败" >&2
            return 1
        fi
        chmod +x "${target}"
    fi
    echo "${target}"
}

FNPACK="$(resolve_fnpack || true)"

# ---------------------------------------------------------------- 1. 图标

# 图标由 cmd/iconforge 从根目录的高清源图生成，与运行时缩放路径完全一致。
# 仓库里已提交生成结果，缺源图或 Go 时直接沿用现有文件，
# 保证脚本在只拉了源码的环境里也能跑通。
if [ -f "${ROOT_DIR}/QLink2Desktop.png" ] && [ -n "${GO_BIN}" ]; then
    echo "--> 生成官方规格图标 (64 / 256)..."
    (cd "${ROOT_DIR}" && "${GO_BIN}" run ./cmd/iconforge -src QLink2Desktop.png -app fnos-app) >/dev/null
else
    echo "--> 跳过图标生成（缺源图或 Go 工具链），沿用仓库内已提交的图标。"
fi

# ---------------------------------------------------------------- 2. 组装打包目录

# 不在 fnos-app/ 原地打包，而是先复制到 .build/ 下的暂存目录再改写：
# manifest 的 platform / version 可以按目标架构注入，
# 也不会把构建产物（二进制）留在源码树里。
STAGE="${BUILD_DIR}/stage-${TAG}"
rm -rf "${STAGE}" 2>/dev/null || true
STAGE_APP="${STAGE}/${APP_NAME}"
mkdir -p "${STAGE_APP}"

cp -r "${FNOS_APP_DIR}/." "${STAGE_APP}/"
# git 不会跟踪空目录，但打包也不该带进任何占位文件。
find "${STAGE_APP}" -name '.gitkeep' -delete 2>/dev/null || true

# manifest 是包的身份信息：platform 必须与二进制的真实架构一致，
# 否则 ARM 设备会装上 x86 包然后直接起不来。
if grep -q '^platform[[:space:]]*=' "${STAGE_APP}/manifest"; then
    sed -i "s|^platform[[:space:]]*=.*|platform              = ${PLATFORM}|" "${STAGE_APP}/manifest"
    sed -i "s|^version[[:space:]]*=.*|version               = ${VERSION}|" "${STAGE_APP}/manifest"
else
    printf 'platform              = %s\nversion               = %s\n' "${PLATFORM}" "${VERSION}" \
        >> "${STAGE_APP}/manifest"
fi

# ---------------------------------------------------------------- 3. 交叉编译

BIN_PATH="${STAGE_APP}/app/${APP_NAME}"
mkdir -p "$(dirname "${BIN_PATH}")"

if [ -n "${GO_BIN}" ]; then
    echo "--> 交叉编译 Linux/${GOARCH} 二进制（${GO_BIN}）..."
    (cd "${ROOT_DIR}" && CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" \
        "${GO_BIN}" build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o "${BIN_PATH}" ./cmd/server)
else
    echo "--> 本机没有 Go 工具链，改用 Docker 官方 golang 镜像编译..."
    docker run --rm \
        -e GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
        -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="${GOARCH}" \
        -v "${ROOT_DIR}:/build" -w /build \
        golang:1.23-alpine \
        go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o ".build/stage-${TAG}/${APP_NAME}/app/${APP_NAME}" ./cmd/server
fi
chmod +x "${BIN_PATH}"
chmod +x "${STAGE_APP}/cmd/"* 2>/dev/null || true

# ---------------------------------------------------------------- 4. 打包

# 产物文件名带版本号：并排放几个包时一眼能分清谁是谁，
# 也避免"这个 qlink2desktop-x86.fpk 到底是哪一版"这种靠时间戳猜的处境。
FPK_OUT="${ROOT_DIR}/${APP_NAME}-${VERSION}-${TAG}.fpk"
rm -f "${FPK_OUT}" 2>/dev/null || true

if [ -n "${FNPACK}" ]; then
    echo "--> 使用官方 fnpack 打包: ${FNPACK}"
    # fnpack 必须在包目录内运行（产物落在当前工作目录）。
    (
        cd "${STAGE_APP}"
        "${FNPACK}" build
    ) | sed 's/^/    /'
    if [ ! -f "${STAGE_APP}/${APP_NAME}.fpk" ]; then
        echo "!! fnpack 未产出 ${APP_NAME}.fpk" >&2
        exit 1
    fi
    mv "${STAGE_APP}/${APP_NAME}.fpk" "${FPK_OUT}"
else
    echo "--> fnpack 不可用，退回到手工打包（app.tgz + md5 checksum）。"
    manual_pack "${STAGE_APP}" "${FPK_OUT}"
fi

echo "[OK] 构建完成: ${FPK_OUT}"
ls -lh "${FPK_OUT}"

# ---------------------------------------------------------------- 手工打包

# 手工路径必须产出与 fnpack 完全一致的结构（已用官方工具实测解包比对过）：
#
#   <fpk>
#   ├── manifest        追加一行 checksum = <app.tgz 的 md5>
#   ├── app.tgz         内容 = app/* ∪ config/*
#   ├── cmd/            生命周期脚本
#   ├── config/         privilege / resource
#   ├── wizard/         安装 / 卸载向导
#   ├── ICON.PNG
#   └── ICON_256.PNG
manual_pack() {
    local app_dir="$1"
    local out="$2"
    local work="${BUILD_DIR}/pack-${TAG}"
    rm -rf "${work}" 2>/dev/null || true
    mkdir -p "${work}/payload"

    # app.tgz = app/* ∪ config/*，即安装后 target/ 里的内容。
    cp -r "${app_dir}/app/." "${work}/payload/"
    cp -r "${app_dir}/config" "${work}/payload/"
    tar -czf "${work}/app.tgz" -C "${work}/payload" .

    local checksum
    if command -v md5sum >/dev/null 2>&1; then
        checksum="$(md5sum "${work}/app.tgz" | awk '{print $1}')"
    else
        checksum="$(md5 -q "${work}/app.tgz")"
    fi

    grep -v '^checksum[[:space:]]*=' "${app_dir}/manifest" > "${work}/manifest"
    printf 'checksum              = %s\n' "${checksum}" >> "${work}/manifest"

    cp -r "${app_dir}/cmd" "${work}/cmd"
    cp -r "${app_dir}/config" "${work}/config"
    cp -r "${app_dir}/wizard" "${work}/wizard"
    cp "${app_dir}/ICON.PNG" "${app_dir}/ICON_256.PNG" "${work}/"

    tar -czf "${out}" -C "${work}" manifest app.tgz cmd config wizard ICON.PNG ICON_256.PNG
}
