#!/bin/sh
# Builds self-contained aisw binaries for every platform into dist/.
set -eu
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(date +%Y.%m.%d)}"
rm -rf dist && mkdir dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}; arch=${target#*/}
  out="dist/aisw-$os-$arch"; [ "$os" = windows ] && out="$out.exe"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/aisw
  echo "built $out"
done
(cd dist && shasum -a 256 aisw-* > checksums.txt)
echo "wrote dist/checksums.txt"
