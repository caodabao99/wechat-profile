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

// Config 程序总配置
type Config struct {
	MyName  string        `json:"myName"` // 我自己的微信昵称，用于区分消息发送方
	LLM     LLMConfig     `json:"llm"`
	Profile ProfileConfig `json:"profile"`
}

// config 全局配置单例
var config *Config

// 首次运行自动生成的配置模板（带注释说明每个配置项用途，JSON 解析时 _comment 键会被忽略）
const defaultConfigTemplate = `{
  "_comment1": "myName: 你的微信昵称，必须与微信里显示的昵称完全一致，用于区分聊天记录中哪些是你发的",
  "myName": "你的微信昵称",
  "_comment2": "llm: 大模型配置。apiKey 填入你的密钥；baseURL 为 OpenAI 兼容接口地址；model 为模型名称；disableThinking 默认 true 关闭思考/推理模式——本程序不需要推理，开着只会拖慢响应、多耗 token。deepseek-v4-flash、qwen3.8-flash 等默认开思考的模型必须保持 true；不支持该参数的接口会自动忽略",
  "llm": {
    "apiKey": "sk-xxx",
    "baseURL": "https://api.deepseek.com",
    "model": "deepseek-chat",
    "disableThinking": true
  },
  "_comment3": "profile: 画像生成规则。coldStartCount 表示累计对方消息达到该条数后首次生成画像；updateInterval 表示画像生成后，对方消息每新增该条数就自动更新一次画像",
  "profile": {
    "coldStartCount": 20,
    "updateInterval": 10
  }
}
`

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
			if werr := os.WriteFile(p, []byte(defaultConfigTemplate), 0644); werr != nil {
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

	config = &c
	return &c, nil
}
