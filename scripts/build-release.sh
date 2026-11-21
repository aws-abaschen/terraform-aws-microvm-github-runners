#!/usr/bin/env bash
# Reproducible release build used by CI. Mirrors `just release-assets`.
# Inputs: RUNNER_VERSION, SHA_ARM64, SHA_X64.
set -euo pipefail

: "${RUNNER_VERSION:?}" "${SHA_ARM64:?}" "${SHA_X64:?}"
dist=dist
flags=(-trimpath -buildvcs=false -ldflags "-s -w -buildid=")
rm -rf "$dist" && mkdir -p "$dist/lambda" "$dist/cache"

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build "${flags[@]}" -o "$dist/lambda/bootstrap" ./cmd/lambda
touch -d @0 "$dist/lambda/bootstrap"
(cd "$dist/lambda" && zip -q -X -D ../lambda.zip bootstrap)

version=$(git describe --tags --always)
for arch in arm64 x64; do
  if [ "$arch" = arm64 ]; then goarch=arm64 sha=$SHA_ARM64; else goarch=amd64 sha=$SHA_X64; fi
  out="$dist/image-$arch"; mkdir -p "$out"
  tgz="$dist/cache/actions-runner-linux-$arch-$RUNNER_VERSION.tar.gz"
  curl -fsSL -o "$tgz" "https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/actions-runner-linux-$arch-$RUNNER_VERSION.tar.gz"
  echo "$sha  $tgz" | sha256sum -c -
  cp "$tgz" "$out/actions-runner.tar.gz"
  CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build "${flags[@]}" -o "$out/supervisor" ./cmd/supervisor
  cp image/Dockerfile "$out/Dockerfile"
  echo "runner=$RUNNER_VERSION supervisor=$version" > "$out/VERSION"
  find "$out" -exec touch -d @0 {} +
  (cd "$out" && zip -q -X -D "../image-$arch.zip" Dockerfile supervisor actions-runner.tar.gz VERSION)
done

(cd "$dist" && sha256sum lambda.zip image-arm64.zip image-x64.zip > SHA256SUMS && cat SHA256SUMS)
