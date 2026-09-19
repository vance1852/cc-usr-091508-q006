#!/usr/bin/env bash
# 构建样品链路服务。
set -euo pipefail
# shellcheck source=scripts/env.sh
source "$(dirname "$0")/env.sh"

mkdir -p bin
go build -mod=vendor -o bin/samplechain ./cmd/samplechain
echo "built bin/samplechain"
