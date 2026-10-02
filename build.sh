#!/bin/bash
# 编译并打包到 dist/
set -e
cd "$(dirname "$0")"

echo "编译中..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -H windowsgui" -o wechat-profile.exe .

echo "复制到 dist/..."
mkdir -p dist
cp wechat-profile.exe dist/
# Releases 页面用的是带平台后缀的名字，一并产出
cp wechat-profile.exe dist/wechat-profile-windows-amd64.exe
# config.json 已在 .gitignore 中（含 API Key），新克隆的仓库没有此文件，
# 首次运行程序会自动生成模板，所以拷贝失败不算错误
cp config.json dist/ 2>/dev/null || echo "跳过 config.json（不存在，程序首次运行会自动生成）"
cp app.manifest dist/ 2>/dev/null || true

echo "完成。dist/ 目录内容："
ls -la dist/
