#!/usr/bin/env bash
#
# 构建 go-music-dl 的飞牛 fnOS 应用包（.fpk）。
#
# 用法：
#   packaging/fnos/build.sh                      # 构建 x86 与 arm 两个架构
#   TARGETS="linux/amd64" packaging/fnos/build.sh # 只构建指定架构
#
# 依赖：Go 工具链、fnpack（https://developer.fnnas.com/docs/cli/fnpack 下载后加入 PATH）

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PACK_DIR="${ROOT_DIR}/packaging/fnos/go-music-dl"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist/fnos}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64}"

if ! command -v fnpack >/dev/null 2>&1; then
    echo "缺少 fnpack，请先从 https://developer.fnnas.com/docs/cli/fnpack 下载并加入 PATH" >&2
    exit 1
fi

if [ ! -f "${PACK_DIR}/manifest" ]; then
    echo "找不到应用包目录：${PACK_DIR}" >&2
    exit 1
fi

APPNAME="$(sed -n 's/^appname[[:space:]]*=[[:space:]]*//p' "${PACK_DIR}/manifest" | head -n 1 | tr -d '[:space:]')"
MANIFEST_VERSION="$(sed -n 's/^version[[:space:]]*=[[:space:]]*//p' "${PACK_DIR}/manifest" | head -n 1 | tr -d '[:space:]')"
APPNAME="${APPNAME:-go-music-dl}"

# 版本号优先级：PKG_VERSION 环境变量 > core/version.go 的 AppVersion > manifest。
# 需要发布只包含打包或修复改动、且能覆盖安装的升级包时，用 PKG_VERSION 指定更高版本。
APP_VERSION="$(sed -n 's/.*AppVersion[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "${ROOT_DIR}/core/version.go" | head -n 1)"
VERSION="${PKG_VERSION:-${APP_VERSION:-${MANIFEST_VERSION:-0.0.0}}}"

mkdir -p "${DIST_DIR}"

for target in ${TARGETS}; do
    goos="${target%%/*}"
    goarch="${target##*/}"

    case "${goarch}" in
    amd64) fn_platform="x86" ;;
    arm64) fn_platform="arm" ;;
    *)
        echo "不支持的架构：${goarch}" >&2
        exit 1
        ;;
    esac

    echo "==> 构建 ${goos}/${goarch}（fnOS platform=${fn_platform}）"
    build_dir="$(mktemp -d "${TMPDIR:-/tmp}/fnos-${APPNAME}-${goarch}.XXXXXX")"

    # 复制应用包骨架到临时目录，构建产物不写回源码目录。
    cp -R "${PACK_DIR}/." "${build_dir}/"
    mkdir -p "${build_dir}/app/bin" "${build_dir}/wizard"

    # 桌面入口走 Unix Socket（由统一网关转发）；另有直连 TCP 端口供安卓等
    # 客户端使用，端口在运行时由 cmd/main 读取 ${TRIM_PKGVAR}/web-port.conf 决定。
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
        go build -trimpath -ldflags "-s -w" \
        -o "${build_dir}/app/bin/music-dl" "${ROOT_DIR}/cmd/music-dl"

    # 架构相关字段写入 manifest：amd64 -> x86，arm64 -> arm。
    sed -i.bak "s/^platform\([[:space:]]*=[[:space:]]*\).*/platform\1${fn_platform}/" "${build_dir}/manifest"
    sed -i.bak "s/^version\([[:space:]]*=[[:space:]]*\).*/version\1${VERSION}/" "${build_dir}/manifest"
    rm -f "${build_dir}/manifest.bak"

    (cd "${build_dir}" && fnpack build)

    fpk_file="$(find "${build_dir}" "${build_dir}/.." -maxdepth 1 -name '*.fpk' -print 2>/dev/null | head -n 1)"
    if [ -z "${fpk_file}" ]; then
        echo "未找到 fnpack 生成的 fpk 文件，构建目录：${build_dir}" >&2
        exit 1
    fi

    target_fpk="${DIST_DIR}/${APPNAME}-${VERSION}-${fn_platform}.fpk"
    cp "${fpk_file}" "${target_fpk}"
    rm -rf "${build_dir}"
    echo "==> 已生成 ${target_fpk}"
done
