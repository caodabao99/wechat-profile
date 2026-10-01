package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// BasicInfo 基本信息
type BasicInfo struct {
	Occupation     string   `json:"occupation"`      // 职业
	Location       string   `json:"location"`        // 所在城市/地区
	ImportantDates []string `json:"important_dates"` // 重要日子（生日、纪念日等）
}

// CommunicationStyle 沟通风格
type CommunicationStyle struct {
	ReplyLength     string   `json:"reply_length"`     // 回复长短倾向
	Tone            string   `json:"tone"`             // 语气
	FrequentPhrases []string `json:"frequent_phrases"` // 口头禅/高频表达
	EmojiUsage      string   `json:"emoji_usage"`      // 表情使用习惯
	Initiative      string   `json:"initiative"`       // 主动程度
}

// EmotionalPatterns 情绪模式
type EmotionalPatterns struct {
	Stressors     []string `json:"stressors"`      // 压力源/雷点
	ComfortTopics []string `json:"comfort_topics"` // 安慰有效话题
	WhenUpset     string   `json:"when_upset"`     // 不高兴时的表现
}

// Relationship 与"我"的关系
type Relationship struct {
	Closeness          string   `json:"closeness"`           // 亲密程度
	RecentEvents       []string `json:"recent_events"`       // 近期共同事件
	InteractionPattern string   `json:"interaction_pattern"` // 互动模式
}

// Profile 人物画像完整结构
type Profile struct {
	BasicInfo          BasicInfo          `json:"basic_info"`
	Personality        []string           `json:"personality"` // 性格特征
	CommunicationStyle CommunicationStyle `json:"communication_style"`
	Interests          []string           `json:"interests"` // 兴趣爱好
	EmotionalPatterns  EmotionalPatterns  `json:"emotional_patterns"`
	Relationship       Relationship       `json:"relationship"`
	IntentPatterns     map[string]string  `json:"intent_patterns"` // 典型意图模式
	ImportantFacts     []string           `json:"important_facts"` // 重要事实
	Summary            string             `json:"summary"`         // 100 字内核心概括
}

// getContactProfileFields 读取联系人的画像字段
func getContactProfileFields(db *sql.DB, contactID int64) (otherCount int, profileJSON string, err error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	var pj sql.NullString
	err = db.QueryRow(
		`SELECT other_msg_count, COALESCE(profile_json, '') FROM contacts WHERE id = ?`,
		contactID).Scan(&otherCount, &pj)
	if err != nil {
		return 0, "", err
	}
	return otherCount, pj.String, nil
}

// ShouldGenerateProfile 是否到了首次生成画像的时机
func ShouldGenerateProfile(db *sql.DB, contactID int64) bool {
	count, pj, err := getContactProfileFields(db, contactID)
	if err != nil {
		return false
	}
	pj = strings.TrimSpace(pj)
	return count >= config.Profile.ColdStartCount && (pj == "" || pj == "{}")
}

// ShouldUpdateProfile 是否到了周期性更新画像的时机
func ShouldUpdateProfile(db *sql.DB, contactID int64) bool {
	count, pj, err := getContactProfileFields(db, contactID)
	if err != nil {
		return false
	}
	pj = strings.TrimSpace(pj)
	interval := config.Profile.UpdateInterval
	if interval <= 0 {
		return false
	}
	return count >= config.Profile.ColdStartCount && pj != "" && pj != "{}" && count%interval == 0
}

// formatMessagesForPrompt 把消息列表拼成给模型看的对话文本
func formatMessagesForPrompt(messages []Message) string {
	var b strings.Builder
	for _, m := range messages {
		who := "对方"
		if m.Sender == "me" {
			who = "我"
		}
		name := m.SenderName
		if name != "" {
			who = name
		}
		fmt.Fprintf(&b, "%s: %s\n", who, strings.TrimSpace(m.Content))
	}
	return b.String()
}

// GenerateOrUpdateProfile 生成或更新联系人画像，并写入历史
func GenerateOrUpdateProfile(db *sql.DB, llmClient *LLMClient, contactID int64, contactName string) error {
	// 防护：如果联系人已被合并，重定向到目标联系人
	merged, targetID, err := IsMerged(db, contactID)
	if err != nil {
		return fmt.Errorf("检查合并状态失败: %w", err)
	}
	if merged {
		contactID = targetID
		// 读取目标联系人名
		target, err := GetContactByID(db, targetID)
		if err != nil {
			return fmt.Errorf("读取目标联系人失败: %w", err)
		}
		contactName = target.Name
	}

	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return fmt.Errorf("读取联系人失败: %w", err)
	}
	oldJSON := strings.TrimSpace(contact.ProfileJSON)
	if oldJSON == "" {
		oldJSON = "{}"
	}

	messages, err := GetRecentMessages(db, contactID, 50)
	if err != nil {
		return fmt.Errorf("读取聊天记录失败: %w", err)
	}

	prompt := fmt.Sprintf(`你是人物画像分析助手。请根据【旧画像】和【新增聊天记录】，更新该联系人的人物画像。
要求：
1. 只基于聊天记录中的证据，不要编造。
2. 如果新信息与旧画像冲突，以新信息为准。
3. 输出完整 JSON，结构同旧画像。
4. summary 字段用 100 字以内概括这个人的核心特征。

画像 JSON 结构如下：
{
  "basic_info": {"occupation": "", "location": "", "important_dates": []},
  "personality": [],
  "communication_style": {"reply_length": "", "tone": "", "frequent_phrases": [], "emoji_usage": "", "initiative": ""},
  "interests": [],
  "emotional_patterns": {"stressors": [], "comfort_topics": [], "when_upset": ""},
  "relationship": {"closeness": "", "recent_events": [], "interaction_pattern": ""},
  "intent_patterns": {},
  "important_facts": [],
  "summary": ""
}

联系人：%s
【旧画像】
%s
【新增聊天记录】
%s
请只输出 JSON，不要其他内容。`, contactName, oldJSON, formatMessagesForPrompt(messages))

	raw, err := llmClient.Call(prompt)
	if err != nil {
		return fmt.Errorf("生成画像失败: %w", err)
	}

	var profile Profile
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &profile); err != nil {
		return fmt.Errorf("解析画像 JSON 失败: %w", err)
	}

	newJSONBytes, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	newJSON := string(newJSONBytes)

	// 用模型生成一句话的本次变化说明（失败不影响主流程）
	changeSummary := summarizeProfileChange(llmClient, oldJSON, newJSON)

	if err := SaveProfile(db, contactID, newJSON, profile.Summary, changeSummary); err != nil {
		return fmt.Errorf("保存画像失败: %w", err)
	}
	return nil
}

// summarizeProfileChange 让模型用一句话概括画像变化；任何失败都返回兜底文案
func summarizeProfileChange(llmClient *LLMClient, oldJSON, newJSON string) string {
	prompt := fmt.Sprintf(`对比下面两份人物画像 JSON，用一句中文（30 字以内）概括新画像相对旧画像的主要变化。
如果除了首次生成外没有实质变化，也请简述新增了哪些信息。
只输出 JSON：{"change_summary": "一句话"}
【旧画像】
%s
【新画像】
%s`, oldJSON, newJSON)

	raw, err := llmClient.Call(prompt)
	if err != nil {
		return "画像已更新"
	}
	var out struct {
		ChangeSummary string `json:"change_summary"`
	}
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &out); err != nil || strings.TrimSpace(out.ChangeSummary) == "" {
		return "画像已更新"
	}
	return strings.TrimSpace(out.ChangeSummary)
}

// SupplementProfile 把用户手动提供的信息补充进画像
func SupplementProfile(db *sql.DB, llmClient *LLMClient, contactID int64, contactName string, userNote string) error {
	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return fmt.Errorf("读取联系人失败: %w", err)
	}
	oldJSON := strings.TrimSpace(contact.ProfileJSON)
	if oldJSON == "" {
		oldJSON = "{}"
	}

	prompt := fmt.Sprintf(`你是人物画像分析助手。用户手动提供了关于联系人的新信息，请把这些信息合并到现有画像中。
要求：
1. 用户手动提供的信息是准确的第一手资料，优先级最高，直接更新到画像对应字段。
2. 不要删除原有画像中没有被新信息覆盖的内容。
3. 输出完整 JSON，结构同旧画像。
4. summary 字段用 100 字以内概括这个人的核心特征。

联系人：%s
【旧画像】
%s
【用户手动补充的信息】
%s
请只输出 JSON，不要其他内容。`, contactName, oldJSON, strings.TrimSpace(userNote))

	raw, err := llmClient.Call(prompt)
	if err != nil {
		return fmt.Errorf("补充画像失败: %w", err)
	}

	var profile Profile
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &profile); err != nil {
		return fmt.Errorf("解析画像 JSON 失败: %w", err)
	}

	newJSONBytes, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	newJSON := string(newJSONBytes)

	changeSummary := "手动补充: " + strings.TrimSpace(userNote)
	if r := []rune(changeSummary); len(r) > 60 {
		changeSummary = string(r[:60]) + "..."
	}

	if err := SaveProfile(db, contactID, newJSON, profile.Summary, changeSummary); err != nil {
		return fmt.Errorf("保存画像失败: %w", err)
	}
	return nil
}

// AnalyzeIntent 结合画像与近期对话分析对方最新消息的意图
func AnalyzeIntent(db *sql.DB, llmClient *LLMClient, contactID int64, newMessage string) (map[string]interface{}, error) {
	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, fmt.Errorf("读取联系人失败: %w", err)
	}
	profileSummary := strings.TrimSpace(contact.ProfileSummary)
	if profileSummary == "" {
		profileSummary = "（暂无画像，消息积累到一定数量后会自动生成）"
	}

	limit := config.Context.RecentMessageCount
	if limit <= 0 {
		limit = 30
	}
	messages, err := GetRecentMessages(db, contactID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取近期对话失败: %w", err)
	}

	prompt := fmt.Sprintf(`你是聊天分析助手。下面是联系人的人物画像和近期对话，请分析对方最新消息的意图。
【人物画像】
%s
【近期对话】
%s
【当前对方最新消息】
对方：%s
请输出 JSON：
{
  "surface": "表面意思",
  "intent": "潜在意图，从[邀约/试探/求安慰/敷衍/婉拒/分享/日常寒暄/其他]中选择",
  "emotion": "情绪状态",
  "subtext": "潜台词",
  "suggested_reply": "建议回复",
  "confidence": 0.0
}
只输出 JSON，不要其他内容。`,
		profileSummary, formatMessagesForPrompt(messages), strings.TrimSpace(newMessage))

	raw, err := llmClient.Call(prompt)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &result); err != nil {
		return nil, fmt.Errorf("解析意图分析结果失败: %w", err)
	}
	return result, nil
}

// orUnknown 空值显示为"暂无"
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "暂无"
	}
	return s
}
