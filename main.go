package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxn/walk"
)

var (
	isRemoteMode bool          // true: 远程 API 模式；false: 本地 SQLite 模式
	remoteClient *RemoteClient // 远程客户端（仅远程模式）
)

// appVersion 桌面端版本单一事实来源。
// 与 git tag 同步；package_release.sh 的 DESK_VER 默认取此处，避免多版本号。
const appVersion = "v3.1.0"

// dbPath 返回程序同目录下的数据库路径。
//
// 不能用相对路径 "wechat_profile.db"：那会落在「当前工作目录」，
// 从快捷方式、命令行或别的目录启动时工作目录各不相同，
// 用户会遇到「联系人全都不见了」——其实是新建了一个空库。
func dbPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "wechat_profile.db"
	}
	return filepath.Join(filepath.Dir(exe), "wechat_profile.db")
}

func main() {
	// 0. 日志（GUI 程序没有控制台，wechat-profile.log 是排查问题的唯一途径）
	logFile := setupLogging()
	if logFile != nil {
		defer logFile.Close()
	}
	slog.Info("程序启动")

	// 1. 加载配置（首次运行会自动生成 config.json）
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("加载配置失败", "err", err)
		walk.MsgBox(nil, "微信人物画像助手", err.Error(), walk.MsgBoxIconWarning)
		return
	}

	// 2. 初始化数据源：本地 SQLite 或远程 API
	if cfg.Remote.Enabled && cfg.Remote.APIURL != "" {
		// 远程模式：连接 bot 服务端
		apiURL, err := ParseAPIURL(cfg.Remote.APIURL)
		if err != nil {
			slog.Error("远程地址无效", "apiURL", cfg.Remote.APIURL, "err", err)
			walk.MsgBox(nil, "微信人物画像助手", "远程地址无效: "+err.Error(), walk.MsgBoxIconError)
			return
		}
		remoteClient = NewRemoteClient(apiURL, cfg.Remote.APIToken)
		if err := remoteClient.Ping(); err != nil {
			slog.Error("无法连接到 bot 服务端", "apiURL", apiURL, "err", err)
			walk.MsgBox(nil, "微信人物画像助手",
				fmt.Sprintf("无法连接到 bot 服务端 (%s):\n%v\n\n请检查服务端是否已启动，或修改 config.json 中的 remote.apiURL", apiURL, err),
				walk.MsgBoxIconError)
			return
		}
		isRemoteMode = true
		slog.Info("远程模式已连接", "apiURL", apiURL)
		// 远程模式下 LLM 由服务端处理，桌面端不需要 apiKey
	} else {
		// 本地模式：初始化 SQLite
		if strings.TrimSpace(cfg.LLM.ApiKey) == "" || cfg.LLM.ApiKey == "sk-xxx" {
			slog.Warn("本地模式缺少 llm.apiKey，启动被拦截")
			walk.MsgBox(nil, "微信人物画像助手",
				"本地模式需要填写 llm.apiKey。\n如需远程模式，请在 config.json 中设置 remote.enabled=true 和 remote.apiURL",
				walk.MsgBoxIconWarning)
			return
		}
		// myName 没填的话，解析聊天记录时无法区分哪些消息是自己发的，
		// 自己的话会被当成对方的，画像整个跑偏。启动时就拦下来。
		if n := strings.TrimSpace(cfg.MyName); n == "" || n == "你的微信昵称" {
			slog.Warn("本地模式缺少 myName，启动被拦截")
			walk.MsgBox(nil, "微信人物画像助手",
				"本地模式需要填写 myName（你自己的微信昵称，必须与微信里显示的完全一致）。\n"+
					"否则无法区分聊天记录中哪些是你发的，画像结果会严重失真。",
				walk.MsgBoxIconWarning)
			return
		}
		db, err = InitDB(dbPath())
		if err != nil {
			slog.Error("初始化数据库失败", "err", err)
			walk.MsgBox(nil, "微信人物画像助手", "初始化数据库失败: "+err.Error(), walk.MsgBoxIconError)
			return
		}
		defer db.Close()
		isRemoteMode = false
		slog.Info("本地模式已就绪", "db", dbPath())
	}

	// 3. 创建大模型客户端（远程模式下不会被使用）
	llmClient = NewLLMClient(cfg)

	// 4. 创建悬浮窗
	if err := SetupFloatingWindow(); err != nil {
		slog.Error("创建悬浮窗失败", "err", err)
		walk.MsgBox(nil, "微信人物画像助手", "创建悬浮窗失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}

	// 5. 进入消息循环
	mainWindow.Run()
}
