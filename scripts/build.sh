#!/usr/bin/env bash
# Builds plugins/darwin/arm64/soonest-reset.dylib after vet and the race tests pass.
set -euo pipefail

cd "$(dirname "$0")/.."

out=plugins/darwin/arm64/soonest-reset.dylib

go vet ./...
go test -race ./...

mkdir -p "$(dirname "$out")"
# Without a minimum version clang stamps the dylib with this Mac's macOS
# version, and an older Mac refuses to load it. 13.0 is Go's own floor.
min="-mmacosx-version-min=${MACOS_MIN:-13.0}"
CGO_CFLAGS="$min" CGO_LDFLAGS="$min" CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
	go build -buildmode=c-shared -trimpath -o "$out" .
rm -f "${out%.dylib}.h"

otool -l "$out" | grep -q "minos ${MACOS_MIN:-13.0}" || { echo "unexpected minimum macOS in $out" >&2; exit 1; }
echo "built $out"
