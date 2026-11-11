#!/usr/bin/env bash
# Builds the Lambda package into build/: a static linux/arm64 binary named
# "bootstrap" (what the provided.al2023 runtime runs) plus the config file.
set -euo pipefail
cd "$(dirname "$0")/.."

rm -rf build
mkdir -p build

GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o build/bootstrap ./cmd/lambda

# config.local.yaml (real channel IDs, gitignored) wins over the template.
if [[ -f config.local.yaml ]]; then
  cp config.local.yaml build/config.yaml
  src=config.local.yaml
else
  cp config.yaml build/config.yaml
  src=config.yaml
fi

echo "built build/bootstrap ($(du -h build/bootstrap | cut -f1)) with $src"
