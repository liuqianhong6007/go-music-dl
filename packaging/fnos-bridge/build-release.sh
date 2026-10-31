#!/usr/bin/env bash
#
# Build standalone fnOS FPKs and their checksums.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist/fnos-bridge}"
VERSION="${PKG_VERSION:-$(sed -n 's/.*AppVersion[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "${ROOT_DIR}/core/version.go" | head -n 1)}"
VERSION="${VERSION:-0.0.0}"

mkdir -p "${DIST_DIR}"
rm -f \
    "${DIST_DIR}"/go-music-dl-fnos-bridge-*.fpk \
    "${DIST_DIR}"/go-music-dl-fnos-bridge-*.tar.gz \
    "${DIST_DIR}/SHA256SUMS"

checksum() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$@"
    else
        shasum -a 256 "$@"
    fi
}

DIST_DIR="${DIST_DIR}" PKG_VERSION="${VERSION}" \
    "${ROOT_DIR}/packaging/fnos-bridge/build.sh"

(
    cd "${DIST_DIR}"
    checksum go-music-dl-fnos-bridge-* > SHA256SUMS
)

echo "==> release assets:"
find "${DIST_DIR}" -maxdepth 1 -type f -print | sort
