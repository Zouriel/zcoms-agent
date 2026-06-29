#!/usr/bin/env bash
# Cross-compile zcoms-agent for release. Pure-Go (modernc.org/sqlite) — no cgo. Asset
# names match the installer's platformAsset() (amd64→x64). Outputs to dist/.
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=zcoms-agent; PKG=./cmd/zcoms-agent
mkdir -p dist
for t in linux/amd64/x64 windows/amd64/x64 darwin/arm64/arm64; do
  IFS=/ read -r os arch asset <<<"$t"
  out="dist/${BIN}-${os}-${asset}"
  [ "$os" = "windows" ] && out="${out}.exe"
  echo "→ $out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$out" "$PKG"
done
echo "done. artifacts in dist/"
