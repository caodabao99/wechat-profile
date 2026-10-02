package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// setupLogging 初始化日志：写入 wechat-profile.log（与数据库同目录）。
// 桌面端是 GUI 程序没有控制台，日志文件是排查问题的唯一途径。
// 返回的文件的关闭时机由 main 的 defer 负责；打开失败时静默退回无日志运行。
func setupLogging() *os.File {
	logPath := filepath.Join(filepath.Dir(dbPath()), "wechat-profile.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(f), nil)))
	slog.Info("日志已初始化", "file", logPath)
	return f
}
