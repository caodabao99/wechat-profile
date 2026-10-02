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

// ResetClipboardHash 清空去重记录，让下一次 ReadClipboard 接受同样的内容。
//
// ReadClipboard 一读到内容就更新了 lastClipboardHash，但后面的落库/分析可能失败
// （网络抖动、服务端 5xx）。不调用它的话，用户必须重新复制一遍才能重试。
// 只在「重试同样内容有可能成功」的失败路径上调用；解析不出消息这种确定性失败不要调。
func ResetClipboardHash() {
	clipboardMu.Lock()
	defer clipboardMu.Unlock()
	lastClipboardHash = ""
}
