package main

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/atotto/clipboard"
)

var (
	// ErrClipboardEmpty 剪贴板为空
	ErrClipboardEmpty = errors.New("剪贴板为空，请先在微信中选中消息并按 Ctrl+C 复制")
	// ErrClipboardUnchanged 剪贴板内容与上次读取一致
	ErrClipboardUnchanged = errors.New("剪贴板内容与上次相同，请重新复制聊天记录后再点击识别")
)

var (
	lastClipboardHash string
	clipboardMu       sync.Mutex
)

// ReadClipboard 读取系统剪贴板文本。
// 内容为空返回 ErrClipboardEmpty；与上次读取内容相同返回 ErrClipboardUnchanged。
func ReadClipboard() (string, error) {
	text, err := clipboard.ReadAll()
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrClipboardEmpty
	}

	sum := md5.Sum([]byte(text))
	hash := hex.EncodeToString(sum[:])

	clipboardMu.Lock()
	defer clipboardMu.Unlock()
	if hash == lastClipboardHash {
		return "", ErrClipboardUnchanged
	}
	lastClipboardHash = hash
	return text, nil
}

// ResetClipboardHash 清空去重记录（目前未使用，保留给后续"强制重新识别"场景）
func ResetClipboardHash() {
	clipboardMu.Lock()
	defer clipboardMu.Unlock()
	lastClipboardHash = ""
}
