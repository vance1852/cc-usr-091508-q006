#!/usr/bin/env bash
# Go + CGO 构建环境，被 build.sh / test.sh source。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v go >/dev/null 2>&1; then
  : "${GOROOT:=/home/node/goroot/usr/lib/go-1.23}"
  export GOROOT
  export PATH="$GOROOT/bin:$PATH"
fi
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export CGO_ENABLED=1

# 用系统 libsqlite3.so.0 生成开发链接（不把本机绝对路径提交进仓库）。
mkdir -p third_party/lib
if [ ! -e third_party/lib/libsqlite3.so ]; then
  for cand in /usr/lib/x86_64-linux-gnu/libsqlite3.so.0 /usr/lib/libsqlite3.so.0 /usr/lib64/libsqlite3.so.0; do
    if [ -e "$cand" ]; then ln -sf "$cand" third_party/lib/libsqlite3.so; break; fi
  done
fi
if [ -e third_party/lib/libsqlite3.so ]; then
  export CGO_LDFLAGS="-L${ROOT}/third_party/lib -lm ${CGO_LDFLAGS:-}"
else
  export CGO_LDFLAGS="-lm ${CGO_LDFLAGS:-}"
fi
export CGO_CFLAGS="-I${ROOT}/third_party/cgo-include ${CGO_CFLAGS:-}"
