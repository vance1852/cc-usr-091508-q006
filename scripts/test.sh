#!/usr/bin/env bash
# 运行全部 Go 测试（CGO 使用仓库内 third_party 的 sqlite3 头文件）。
set -euo pipefail
# shellcheck source=scripts/env.sh
source "$(dirname "$0")/env.sh"

go test -mod=vendor ./... "$@"
