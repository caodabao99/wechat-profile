package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LLMConfig 大模型接口配置
type LLMConfig struct {
	ApiKey          string `json:"apiKey"`          // API Key
	BaseURL         string `json:"baseURL"`         // 接口地址，如 https://api.deepseek.com
	Model           string `json:"model"`           // 模型名称
	DisableThinking bool   `json:"disableThinking"` // 关闭推理思考模式（适用于 deepseek-v4/qwen3 等默认开启思考的模型）
}

// ProfileConfig 画像生成策略
type ProfileConfig struct {
	ColdStartCount int `json:"coldStartCount"` // 累计多少条对方消息后首次生成画像
	UpdateInterval int `json:"updateInterval"` // 之后每新增多少条对方消息更新一次画像
}

// ContextConfig 意图分析时携带的上下文条数
type ContextConfig struct {
	RecentMessageCount int `json:"recentMessageCount"` // 近期对话条数
}

// Config 程序总配置
type Config struct {
	MyName  string        `json:"myName"` // 我自己的微信昵称，用于区分消息发送方
	LLM     LLMConfig     `json:"llm"`
	Profile ProfileConfig `json:"profile"`
	Context ContextConfig `json:"context"`
}

// config 全局配置单例
var config *Config

// defaultConfig 返回内置默认配置
func defaultConfig() *Config {
	return &Config{
		MyName: "我的微信昵称",
		LLM: LLMConfig{
			ApiKey:  "sk-xxx",
			BaseURL: "https://api.deepseek.com",
			Model:   "deepseek-chat",
		},
		Profile: ProfileConfig{
			ColdStartCount: 20,
			UpdateInterval: 10,
		},
		Context: ContextConfig{
			RecentMessageCount: 30,
		},
	}
}

// configPath 返回程序同目录下的 config.json 路径
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

// LoadConfig 读取程序同目录下的 config.json；
// 文件不存在时自动生成一份默认配置并返回错误，提示用户填写后重启。
func LoadConfig() (*Config, error) {
	p := configPath()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			c := defaultConfig()
			if werr := writeConfig(c); werr != nil {
				return nil, fmt.Errorf("配置文件不存在，且自动创建失败: %w", werr)
			}
			return nil, fmt.Errorf("配置文件不存在，已在程序目录生成默认配置：\n%s\n\n请填写 myName 和 llm.apiKey 后重新启动程序", p)
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("解析 config.json 失败: %w", err)
	}

	// 兜底默认值，避免用户把某些数字字段删空
	if c.Profile.ColdStartCount <= 0 {
		c.Profile.ColdStartCount = 20
	}
	if c.Profile.UpdateInterval <= 0 {
		c.Profile.UpdateInterval = 10
	}
	if c.Context.RecentMessageCount <= 0 {
		c.Context.RecentMessageCount = 30
	}

	config = &c
	return &c, nil
}

// writeConfig 以缩进 JSON 写回配置文件
func writeConfig(c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0644)
}
