package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

// LLMClient 封装 OpenAI 兼容的 chat/completions 调用
type LLMClient struct {
	apiKey          string
	baseURL         string
	model           string
	disableThinking bool
	http            *resty.Client
}

// NewLLMClient 根据配置创建 LLM 客户端
func NewLLMClient(cfg *Config) *LLMClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.LLM.BaseURL), "/")
	return &LLMClient{
		apiKey:          cfg.LLM.ApiKey,
		baseURL:         base,
		model:           cfg.LLM.Model,
		disableThinking: cfg.LLM.DisableThinking,
		http: resty.New().
			SetTimeout(60*time.Second).
			SetHeader("Content-Type", "application/json"),
	}
}

// chatResponse 是接口返回中我们关心的字段
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Call 发送一次对话请求，要求模型输出 JSON。
// 网络或接口失败时自动重试一次。
func (c *LLMClient) Call(prompt string) (string, error) {
	return c.CallContext(context.Background(), prompt)
}

// isRetryableStatus 判断 HTTP 状态码是否值得重试。
// 401/403/404/400 属于配置或请求本身的问题，重试必然得到同样的结果；
// 早先这里对所有 IsError() 一律重试，导致每次配置错误都要白等 2 秒再叠加一次
// 完整超时（客户端超时 60s，最坏一次调用 ~122s 才返回错误）。
// 只有 429（限流）和 5xx（服务端故障）才可能有瞬态性。
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// CallContext 与 Call 相同，但接受 ctx 用于取消/超时控制。
// HTTP 请求走 resty 的 SetContext，重试等待也感知 ctx，
// 避免调用方已经放弃后仍在后台占用 LLM 配额。
func (c *LLMClient) CallContext(ctx context.Context, prompt string) (string, error) {
	body := map[string]interface{}{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.3,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}
	// 关闭推理思考：百炼兼容模式认 enable_thinking，DeepSeek 官方/V4 认 thinking.type，
	// 两个参数一起发，不支持的平台会忽略。
	if c.disableThinking {
		body["enable_thinking"] = false
		body["thinking"] = map[string]string{"type": "disabled"}
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}

		resp, err := c.http.R().
			SetContext(ctx).
			SetHeader("Authorization", "Bearer "+c.apiKey).
			SetBody(body).
			Post(c.baseURL + "/chat/completions")
		if err != nil {
			lastErr = fmt.Errorf("请求模型接口失败: %w", err)
			continue
		}
		if resp.IsError() {
			lastErr = fmt.Errorf("模型接口返回 %d: %s", resp.StatusCode(),
				strings.TrimSpace(string(resp.Body())))
			if !isRetryableStatus(resp.StatusCode()) {
				return "", lastErr
			}
			continue
		}

		var out chatResponse
		if err := json.Unmarshal(resp.Body(), &out); err != nil {
			lastErr = fmt.Errorf("解析模型返回失败: %w", err)
			continue
		}
		if out.Error != nil {
			lastErr = errors.New("模型接口报错: " + out.Error.Message)
			continue
		}
		if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
			lastErr = errors.New("模型返回内容为空")
			continue
		}
		return out.Choices[0].Message.Content, nil
	}
	return "", lastErr
}

// ExtractJSON 从模型返回文本中提取第一个 { 到最后一个 } 的内容，
// 容忍 ```json 代码块包裹和多余解释文字。
func ExtractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 去掉常见的 markdown 代码块围栏
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
