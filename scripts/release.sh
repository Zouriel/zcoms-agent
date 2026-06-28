#!/usr/bin/env bash
# Cross-compile zcoms-agent for release. Pure-Go (modernc.org/sqlite) — no cgo, no
# bundled libs, so AOT cross-compilation stays clean. Outputs to dist/.
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=zcoms-agent
PKG=./cmd/zcoms-agent
mkdir -p dist
targets=("linux/amd64" "windows/amd64" "darwin/arm64")
for t in "${targets[@]}"; do
  os="${t%/*}"; arch="${t#*/}"
  out="dist/${BIN}-${os}-${arch}"
  [ "$os" = "windows" ] && out="${out}.exe"
  echo "→ $out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$out" "$PKG"
done
echo "done. artifacts in dist/"
