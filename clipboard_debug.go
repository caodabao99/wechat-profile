package main

import (
	"os"
	"path/filepath"
	"time"
)

// DumpClipboardForDebug 把剪贴板原文写到程序同目录的 clipboard_debug.txt，
// 用于在解析失败时排查微信复制格式变化。返回文件路径。
func DumpClipboardForDebug(text string) string {
	name := "clipboard_debug.txt"
	if exe, err := os.Executable(); err == nil {
		name = filepath.Join(filepath.Dir(exe), name)
	}
	content := time.Now().Format("2006-01-02 15:04:05") + "\n" + text
	_ = os.WriteFile(name, []byte(content), 0644)
	return name
}
