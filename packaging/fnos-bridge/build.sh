#!/usr/bin/env bash
#
# Build the standalone root FPK that runs fnos-bridge directly from the fnOS
# application lifecycle.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PACK_DIR="${ROOT_DIR}/packaging/fnos-bridge/go-music-dl-fnos-bridge"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist/fnos-bridge}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64}"
FNPACK_VERSION="${FNPACK_VERSION:-1.2.3}"

APP_VERSION="$(sed -n 's/.*AppVersion[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "${ROOT_DIR}/core/version.go" | head -n 1)"
VERSION="${PKG_VERSION:-${APP_VERSION:-0.0.0}}"

ensure_fnpack() {
    if [ -n "${FNPACK:-}" ]; then
        [ -x "${FNPACK}" ] || {
            echo "FNPACK is not executable: ${FNPACK}" >&2
            return 1
        }
        printf '%s\n' "${FNPACK}"
        return
    fi
    if command -v fnpack >/dev/null 2>&1; then
        command -v fnpack
        return
    fi
    local os arch cache platform
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m)"
    case "${arch}" in
    x86_64|amd64) platform="amd64" ;;
    arm64|aarch64) platform="arm64" ;;
    *) echo "Unsupported host architecture for fnpack: ${arch}" >&2; return 1 ;;
    esac
    case "${os}" in
    darwin|linux) ;;
    *) echo "Unsupported host OS for automatic fnpack download: ${os}" >&2; return 1 ;;
    esac
    cache="${XDG_CACHE_HOME:-${HOME}/.cache}/go-music-dl/tools/fnpack-${FNPACK_VERSION}-${os}-${platform}"
    if [ ! -x "${cache}" ]; then
        mkdir -p "$(dirname "${cache}")"
        echo "==> downloading fnpack ${FNPACK_VERSION}" >&2
        curl -fL --retry 3 \
            "https://static2.fnnas.com/fnpack/fnpack-${FNPACK_VERSION}-${os}-${platform}" \
            -o "${cache}.tmp"
        chmod 0755 "${cache}.tmp"
        mv -f "${cache}.tmp" "${cache}"
    fi
    printf '%s\n' "${cache}"
}

FNPACK_BIN="$(ensure_fnpack)"
mkdir -p "${DIST_DIR}"

for target in ${TARGETS}; do
    goos="${target%%/*}"
    goarch="${target##*/}"
    case "${goarch}" in
    amd64) fn_platform="x86" ;;
    arm64) fn_platform="arm" ;;
    *) echo "Unsupported target: ${target}" >&2; exit 1 ;;
    esac
    [ "${goos}" = "linux" ] || {
        echo "fnOS packages only support Linux targets, got ${goos}" >&2
        exit 1
    }

    build_dir="$(mktemp -d "${TMPDIR:-/tmp}/fnos-bridge-${goarch}.XXXXXX")"
    trap 'rm -rf -- "${build_dir}"' EXIT
    cp -R "${PACK_DIR}/." "${build_dir}/"
    mkdir -p "${build_dir}/app/bin"

    echo "==> building bridge binary ${goos}/${goarch}"
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
        go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o "${build_dir}/app/bin/fnos-bridge" "${ROOT_DIR}/cmd/fnos-bridge"

    find "${build_dir}/app/bin" "${build_dir}/cmd" -type f \
        ! -name '.gitkeep' -exec chmod 0755 {} +
    sed -i.bak "s/^platform\([[:space:]]*=[[:space:]]*\).*/platform\1${fn_platform}/" "${build_dir}/manifest"
    sed -i.bak "s/^version\([[:space:]]*=[[:space:]]*\).*/version\1${VERSION}/" "${build_dir}/manifest"
    rm -f "${build_dir}/manifest.bak"

    (cd "${build_dir}" && "${FNPACK_BIN}" build)
    fpk_file="$(find "${build_dir}" -maxdepth 2 -name '*.fpk' -print | head -n 1)"
    [ -n "${fpk_file}" ] || {
        echo "fnpack did not produce an FPK in ${build_dir}" >&2
        exit 1
    }
    target_fpk="${DIST_DIR}/go-music-dl-fnos-bridge-${VERSION}-${fn_platform}.fpk"
    cp "${fpk_file}" "${target_fpk}"
    echo "==> created ${target_fpk}"

    rm -rf -- "${build_dir}"
    trap - EXIT
done
