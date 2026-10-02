package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ImportantDates 重要日子列表。模型可能返回 []string，也可能返回 map（如 {"生日":"5月20日"}），
// 自定义 Unmarshal 统一转成 "key: value" 字符串数组。
type ImportantDates []string

func (d *ImportantDates) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*d = arr
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err == nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			*d = append(*d, k+": "+m[k])
		}
		return nil
	}
	// 实在解析不了就置空，避免整个画像解析失败
	*d = nil
	return nil
}

// BasicInfo 基本信息
type BasicInfo struct {
	Occupation     string         `json:"occupation"`      // 职业
	Location       string         `json:"location"`        // 所在城市/地区
	ImportantDates ImportantDates `json:"important_dates"` // 重要日子（生日、纪念日等）
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

// ErrProfileBusy 表示该联系人的画像正在生成中。
// 画像是「读旧 JSON → 调 LLM → 写回」的非原子过程，同一联系人并发跑两次时，
// 后写的一方会用自己那份旧 JSON 覆盖前者的结果，用户手动补充的信息可能凭空消失。
var ErrProfileBusy = errors.New("该联系人的画像正在生成中，请稍后再试")

var (
	// profileGuard 只保护 profileLocks 这张表本身，持锁时间极短，
	// 且一定在 dbMu 之外获取（先 profileGuard 后 dbMu），不存在反向持锁的路径。
	profileGuard sync.Mutex
	profileLocks = make(map[int64]*sync.Mutex)
)

// tryLockProfile 尝试为某个联系人取画像生成锁。
// 成功时返回解锁函数；已被占用时返回 false，调用方应把 ErrProfileBusy 反馈给用户。
//
// 这里刻意用 TryLock 而不是阻塞等锁：一次画像生成最坏要等 LLM 两轮超时（~122s），
// 让用户请求干等这么久不如立刻告知「正在生成」。
// 锁表按 contactID 常驻不回收，条目数等于联系人数，量级可忽略。
func tryLockProfile(contactID int64) (func(), bool) {
	profileGuard.Lock()
	mu, ok := profileLocks[contactID]
	if !ok {
		mu = &sync.Mutex{}
		profileLocks[contactID] = mu
	}
	profileGuard.Unlock()

	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// getContactProfileFields 读取联系人的画像字段（含上次生成画像时的对方消息数）
func getContactProfileFields(db *sql.DB, contactID int64) (otherCount int, profileJSON string, profileMsgCount int, err error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	var pj sql.NullString
	err = db.QueryRow(
		`SELECT other_msg_count, COALESCE(profile_json, ''), COALESCE(profile_msg_count, 0) FROM contacts WHERE id = ?`,
		contactID).Scan(&otherCount, &pj, &profileMsgCount)
	if err != nil {
		return 0, "", 0, err
	}
	return otherCount, pj.String, profileMsgCount, nil
}

// ShouldGenerateProfile 是否到了首次生成画像的时机
func ShouldGenerateProfile(db *sql.DB, contactID int64) bool {
	count, pj, _, err := getContactProfileFields(db, contactID)
	if err != nil {
		return false
	}
	pj = strings.TrimSpace(pj)
	return count >= config.Profile.ColdStartCount && (pj == "" || pj == "{}")
}

// ShouldUpdateProfile 是否到了周期性更新画像的时机：
// 距离上次生成画像，对方消息新增达到 updateInterval 条即触发（不再用整除判断，避免跳点漏更新）
func ShouldUpdateProfile(db *sql.DB, contactID int64) bool {
	count, pj, lastCount, err := getContactProfileFields(db, contactID)
	if err != nil {
		return false
	}
	pj = strings.TrimSpace(pj)
	interval := config.Profile.UpdateInterval
	if interval <= 0 {
		return false
	}
	return count >= config.Profile.ColdStartCount && pj != "" && pj != "{}" && count-lastCount >= interval
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

// maxPromptMessageRunes 聊天记录拼进 prompt 的长度上限（按 rune 计）。
// 多选复制上千条消息时，早先这里会把全部内容原样塞进 prompt：
// 一是必然超过模型上下文窗口直接 400，二是 token 费用无上限。
// 12000 个 rune 约等于 1.2 万汉字，对主流模型的上下文足够安全。
const maxPromptMessageRunes = 12000

// formatMessagesForPromptLimited 与 formatMessagesForPrompt 相同，但带长度保护。
// 超限时保留**最近**的消息（越靠近当下的对话越能反映这个人现在的状态），
// 并在开头注明省略了多少条，避免模型把残缺对话误当成全部事实。
// 按整条消息丢弃，不会截断出半句话。
func formatMessagesForPromptLimited(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	start := 0
	for {
		s := formatMessagesForPrompt(messages[start:])
		if utf8.RuneCountInString(s) <= maxPromptMessageRunes || start >= len(messages)-1 {
			if start > 0 {
				return fmt.Sprintf("（较早的 %d 条消息因长度限制已省略）\n%s", start, s)
			}
			return s
		}
		// 每轮丢掉最早的四分之一，总开销约 4n，避免逐条试探的 O(n²)
		cut := (len(messages) - start) / 4
		if cut < 1 {
			cut = 1
		}
		start += cut
	}
}

// GenerateOrUpdateProfile 生成或更新联系人画像，并写入历史。
// messages 为本次用于生成画像的聊天记录（通常是本次复制的消息；合并重生成时为该联系人全部消息）。
// 同一联系人已有画像任务在跑时返回 ErrProfileBusy，调用方应原样反馈给用户。
func GenerateOrUpdateProfile(ctx context.Context, db *sql.DB, llmClient *LLMClient, contactID int64, contactName string, messages []Message) error {
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

	// 必须放在重定向之后：锁要锁在「最终真正被写入的那个联系人」上，
	// 否则两个分别指向同一 target 的 source 仍能并发改写 target 的画像。
	unlock, ok := tryLockProfile(contactID)
	if !ok {
		return ErrProfileBusy
	}
	defer unlock()

	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return fmt.Errorf("读取联系人失败: %w", err)
	}
	// 调用方未提供名字时用库里存的名字，避免 prompt 里「联系人：」为空影响 LLM 判断
	if strings.TrimSpace(contactName) == "" {
		contactName = contact.Name
	}
	oldJSON := strings.TrimSpace(contact.ProfileJSON)
	if oldJSON == "" {
		oldJSON = "{}"
	}
	if len(messages) == 0 {
		return fmt.Errorf("没有可用于生成画像的聊天记录")
	}

	prompt := fmt.Sprintf(`你是人物画像分析助手。请根据【旧画像】和【新增聊天记录】，更新该联系人的人物画像。
要求：
1. 只基于聊天记录中的证据，不要编造。
2. 如果新信息与旧画像冲突，以新信息为准。
3. 输出完整 JSON，结构同旧画像。
4. intent_patterns 的键（意图名称）必须用中文，如"分享资源"、"技术支持"、"闲聊问候"。
5. summary 字段用 100 字以内概括这个人的核心特征。

画像 JSON 结构如下：
{
  "basic_info": {"occupation": "", "location": "", "important_dates": []},
  "personality": [],
  "communication_style": {"reply_length": "", "tone": "", "frequent_phrases": [], "emoji_usage": "", "initiative": ""},
  "interests": [],
  "emotional_patterns": {"stressors": [], "comfort_topics": [], "when_upset": ""},
  "relationship": {"closeness": "", "recent_events": [], "interaction_pattern": ""},
  "intent_patterns": {"中文意图名称": "描述该意图的典型表现"},
  "important_facts": [],
  "summary": ""
}

联系人：%s
【旧画像】
%s
【新增聊天记录】
%s
请只输出 JSON，不要其他内容。`, contactName, oldJSON, formatMessagesForPromptLimited(messages))

	raw, err := llmClient.CallContext(ctx, prompt)
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
	changeSummary := summarizeProfileChange(ctx, llmClient, oldJSON, newJSON)

	if err := SaveProfile(db, contactID, newJSON, profile.Summary, changeSummary); err != nil {
		return fmt.Errorf("保存画像失败: %w", err)
	}
	return nil
}

// summarizeProfileChange 让模型用一句话概括画像变化；任何失败都返回兜底文案
func summarizeProfileChange(ctx context.Context, llmClient *LLMClient, oldJSON, newJSON string) string {
	prompt := fmt.Sprintf(`对比下面两份人物画像 JSON，用一句中文（30 字以内）概括新画像相对旧画像的主要变化。
如果除了首次生成外没有实质变化，也请简述新增了哪些信息。
只输出 JSON：{"change_summary": "一句话"}
【旧画像】
%s
【新画像】
%s`, oldJSON, newJSON)

	raw, err := llmClient.CallContext(ctx, prompt)
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

// SupplementProfile 把用户手动提供的信息补充进画像。
// 同一联系人已有画像任务在跑时返回 ErrProfileBusy：手动补充的信息是用户第一手资料，
// 一旦被并发的自动生成覆盖就再也找不回来，宁可让用户稍后重试。
func SupplementProfile(ctx context.Context, db *sql.DB, llmClient *LLMClient, contactID int64, contactName string, userNote string) error {
	unlock, ok := tryLockProfile(contactID)
	if !ok {
		return ErrProfileBusy
	}
	defer unlock()

	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return fmt.Errorf("读取联系人失败: %w", err)
	}
	// 调用方未提供名字时回落到库里的名字，与 GenerateOrUpdateProfile 一致：
	// prompt 里「联系人：」为空会让模型丢失最基本的主体信息，影响字段归属判断。
	if strings.TrimSpace(contactName) == "" {
		contactName = contact.Name
	}
	oldJSON := strings.TrimSpace(contact.ProfileJSON)
	if oldJSON == "" {
		oldJSON = "{}"
	}

	prompt := fmt.Sprintf(`你是人物画像分析助手。用户手动提供了关于联系人的新信息，请把这些信息合并到现有画像中。
要求：
1. 用户手动提供的信息是准确的第一手资料，优先级最高，直接更新到画像对应字段。
2. 不要删除原有画像中没有被新信息覆盖的内容。
3. 日期类信息（如生日、纪念日）严格按照用户提供的精度记录：提供了年月日就记年月日，只提供月日就只记月日，不要自行补全或猜测缺失的部分。
4. 输出完整 JSON，结构同旧画像。
5. summary 字段用 100 字以内概括这个人的核心特征。

联系人：%s
【旧画像】
%s
【用户手动补充的信息】
%s
请只输出 JSON，不要其他内容。`, contactName, oldJSON, strings.TrimSpace(userNote))

	raw, err := llmClient.CallContext(ctx, prompt)
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

	// 完整记录用户补充的内容，不做截断（列表显示时再截断）
	changeSummary := "手动补充: " + strings.TrimSpace(userNote)

	if err := SaveProfile(db, contactID, newJSON, profile.Summary, changeSummary); err != nil {
		return fmt.Errorf("保存画像失败: %w", err)
	}
	return nil
}

// AnalyzeIntent 结合画像与本次复制的对话，分析对方最新消息的意图。
// messages 为本次多选复制解析出的消息（不限条数），不按历史累计加载。
func AnalyzeIntent(ctx context.Context, db *sql.DB, llmClient *LLMClient, contactID int64, newMessage string, messages []Message) (map[string]interface{}, error) {
	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, fmt.Errorf("读取联系人失败: %w", err)
	}
	profileSummary := strings.TrimSpace(contact.ProfileSummary)
	if profileSummary == "" {
		profileSummary = "（暂无画像，消息积累到一定数量后会自动生成）"
	}

	prompt := fmt.Sprintf(`你是聊天分析助手。下面是联系人的人物画像和本次对话，请分析对方最新消息的意图。
【人物画像】
%s
【本次对话】
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
		profileSummary, formatMessagesForPromptLimited(messages), strings.TrimSpace(newMessage))

	raw, err := llmClient.CallContext(ctx, prompt)
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

// displayTime 把数据库里的时间字符串统一成可读的本地时间显示。
// 数据库时间列统一存 RFC3339（如 2026-10-01T14:45:54+08:00），驱动读出转 UTC 后
// 用 time.Parse + Local() 还原成本地时间；纯文本日期则直接按本地时区解析。
func displayTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 标准 RFC3339（含 Z 或 +08:00）→ 正确解析后转本地
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02 15:04:05")
	}
	// 纯文本日期（如 2026-10-01 14:45:54）→ 直接按本地时区解析
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
		return t.Format("2006-01-02 15:04:05")
	}
	return s
}
