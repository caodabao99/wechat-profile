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
# 发布包一律附带纳入版本库的配置模板，避免把本机 gitignore 的旧 config.json
# （可能缺新字段、甚至含真实 apiKey）打进 Release 导致用户配置项缺失
cp config.example.json dist/config.json
cp app.manifest dist/ 2>/dev/null || true

echo "完成。dist/ 目录内容："
ls -la dist/
