#!/bin/bash
# 编译并打包到 dist/
set -e
cd "$(dirname "$0")"

echo "编译中..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-H windowsgui" -o wechat-profile.exe .

echo "复制到 dist/..."
mkdir -p dist
cp wechat-profile.exe dist/
cp config.json dist/
cp app.manifest dist/ 2>/dev/null || true

echo "完成。dist/ 目录内容："
ls -la dist/
