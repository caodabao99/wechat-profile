package main

import (
	"strings"

	"github.com/lxn/walk"
)

func main() {
	// 1. 加载配置（首次运行会自动生成 config.json）
	cfg, err := LoadConfig()
	if err != nil {
		walk.MsgBox(nil, "微信人物画像助手", err.Error(), walk.MsgBoxIconWarning)
		return
	}
	if strings.TrimSpace(cfg.LLM.ApiKey) == "" || cfg.LLM.ApiKey == "sk-xxx" {
		walk.MsgBox(nil, "微信人物画像助手",
			"请先编辑程序同目录下的 config.json，\n填写 myName（你的微信昵称）和 llm.apiKey（大模型密钥）后重新启动。",
			walk.MsgBoxIconWarning)
		return
	}

	// 2. 初始化数据库
	db, err = InitDB("wechat_profile.db")
	if err != nil {
		walk.MsgBox(nil, "微信人物画像助手", "初始化数据库失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}

	// 3. 创建大模型客户端
	llmClient = NewLLMClient(cfg)

	// 4. 创建悬浮窗
	if err := SetupFloatingWindow(); err != nil {
		walk.MsgBox(nil, "微信人物画像助手", "创建悬浮窗失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}

	// 5. 进入消息循环
	mainWindow.Run()
}
