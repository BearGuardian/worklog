#!/usr/bin/env bash
# worklog 构建脚本(Git Bash)
#
# 封装了本机环境的几个坑,与 PasteWin 一致:
#   1. Go 不在 PATH 里,在 .tools/go
#   2. proxy.golang.org 不通,必须走 goproxy.cn
#   3. CGO_ENABLED=0,SQLite 用纯 Go 驱动
#   4. wails build 的绑定生成极慢(要用 go/packages 加载整个依赖图,
#      modernc.org/sqlite 是 C 转译产物、包数极多),默认跳过
#
# 前端直接调运行时注入的 window.go.main.App.Xxx(),不依赖生成的 wailsjs 文件,
# 所以 -skipbindings 是安全的。只有需要 IDE 类型提示时才跑 --full。
set -euo pipefail

cd "$(dirname "$0")"

export PATH="/d/code-env/.tools/go/bin:/c/Users/suyz2/go/bin:$PATH"
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off
export CGO_ENABLED=0

BUILD_FLAGS=(-skipbindings -nosyncgomod)
if [[ "${1:-}" == "--full" ]]; then
    echo "完整构建(含绑定生成,可能耗时十余分钟)…"
    BUILD_FLAGS=()
fi

echo "==> 运行测试"
go test ./internal/... 2>&1 | grep -v "no test files" || true

echo "==> 静态检查"
go vet ./...

echo "==> 构建应用"
wails build "${BUILD_FLAGS[@]}"

echo
echo "完成: build/bin/worklog.exe"
ls -la build/bin/worklog.exe
