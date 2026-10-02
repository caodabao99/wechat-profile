package main

import (
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// Message 表示解析出来的一条聊天消息
type Message struct {
	Sender     string    // "me" 或 "other"
	SenderName string    // 发送方昵称
	Content    string    // 消息正文（多行用 \n 连接）
	Timestamp  time.Time // 消息时间
}

var (
	// reHeader 匹配「昵称 + 日期时间」同一行的消息头，兼容多种微信版本/语言：
	//   张三 2025年6月10日 10:23:45
	//   张三 2025/6/10 10:23:45
	//   张三 2025-6-10 10:23:45
	//   张三 2025/6/10 10:23 AM
	// 秒可有可无，AM/PM、上午/下午可有可无。
	reHeader = regexp.MustCompile(
		`^(\S.*?)\s+(\d{4}\s*[年/-]\s*\d{1,2}\s*[月/-]\s*\d{1,2}\s*日?\s+\d{1,2}:\d{2}(?::\d{2})?(?:\s*(?:AM|PM|上午|下午))?)\s*$`)

	// reDateOnly 匹配单独成行的日期时间（新版微信 4.x 复制格式中，昵称在上一行、时间单独一行）
	reDateOnly = regexp.MustCompile(
		`^\d{4}\s*[年/-]\s*\d{1,2}\s*[月/-]\s*\d{1,2}\s*日?\s+\d{1,2}:\d{2}(?::\d{2})?(?:\s*(?:AM|PM|上午|下午))?\s*$`)

	// reWithColon 匹配无时间头的降级格式：昵称: 内容 / 昵称：内容
	reWithColon = regexp.MustCompile(`^(.+?)[:：]\s*(.+)$`)

	// reTimeSep / reDateSep 匹配微信多选记录里单独成行的时间/日期分隔标记，
	// 例如「13:30」「13:30:05」「昨天 13:30」「2026年9月30日」，它们不是消息内容。
	reTimeSep = regexp.MustCompile(
		`^(?:昨天|前天|今日|今天)?\s*\d{1,2}:\d{2}(?::\d{2})?(?:\s*(?:AM|PM|上午|下午))?$`)
	reDateSep = regexp.MustCompile(
		`^\d{4}\s*[年/-]\s*\d{1,2}\s*[月/-]\s*\d{1,2}\s*日?\s*(?:星期[一二三四五六日天])?$`)
)

// timeLayouts 支持解析的时间格式（归一化中文日期后依次尝试）
var timeLayouts = []string{
	"2006/1/2 15:04:05",
	"2006/1/2 15:04",
	"2006/1/2 03:04:05 PM",
	"2006/1/2 03:04 PM",
}

// classifySender 根据昵称判断消息是谁发的
func classifySender(name, myName string) string {
	name = strings.TrimSpace(name)
	if name == strings.TrimSpace(myName) || name == "我" || name == "Me" || name == "me" {
		return "me"
	}
	return "other"
}

// parseTime 解析消息时间，归一化中文「年月日」后按多种布局尝试。
//
// 解析失败时仍回落当前时间（保持导入流程不中断），但必须打印告警：
// 静默回落会让消息时间整体错乱且无从排查——两个调用点都由正则筛过，
// 走到这里说明是 timeLayouts 没覆盖的新格式，需要补布局而不是忽略。
func parseTime(s string) time.Time {
	raw := s
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "年", "/")
	s = strings.ReplaceAll(s, "月", "/")
	s = strings.ReplaceAll(s, "日", "")
	s = strings.TrimSpace(s)
	// 折叠数字与符号之间的空白
	s = regexp.MustCompile(`\s*/\s*`).ReplaceAllString(s, "/")
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")

	for _, layout := range timeLayouts {
		// 微信文本里的时间就是本地时间，必须按本地时区解析，
		// 否则 time.Parse 会按 UTC 处理，显示时再转本地会差 8 小时
		if ts, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return ts
		}
	}
	slog.Warn("无法解析消息时间，已回落为当前时间", "raw", raw, "normalized", s)
	return time.Now()
}

// looksLikeStandaloneName 粗判一行是否可能是单独成行的昵称：
// 短、不带句末标点、不含冒号/网址。仅作为下一行是时间行时的临时猜测。
func looksLikeStandaloneName(line string) bool {
	r := []rune(strings.TrimSpace(line))
	if len(r) == 0 || len(r) > 20 {
		return false
	}
	if strings.ContainsAny(line, "：:。！？!?，,.、~…\"“”'") || strings.Contains(line, "http") {
		return false
	}
	return true
}

// ParseClipboard 解析微信复制的聊天记录文本。
// 兼容三种形态：
//  1. 昵称与时间同一行：「昵称 2025年6月10日 10:23:45」后跟若干行内容
//  2. 昵称、时间各占一行（新版微信）：昵称行 → 时间行 → 内容
//  3. 降级格式：单行「昵称: 内容」
//
// 若完全无法解析，则把整段文本作为一条对方消息返回，避免微信改版后完全失效。
func ParseClipboard(text, myName string) []Message {
	// 统一换行符
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	var out []Message
	var current *Message
	var pendingName string // 暂时猜测为「单独成行昵称」的上一行

	flush := func() {
		if current == nil {
			return
		}
		current.Content = strings.TrimSpace(current.Content)
		if current.Content != "" {
			out = append(out, *current)
		}
		current = nil
	}

	// commitPending 把之前暂存的昵称行按普通内容消化掉（因为下一行并不是时间行）
	commitPending := func() {
		if pendingName == "" {
			return
		}
		line := pendingName
		pendingName = ""
		if current != nil {
			current.Content += "\n" + line
			return
		}
		// 与主流程一致：尝试冒号格式，否则作为一条对方消息的开头
		if m := reWithColon.FindStringSubmatch(line); m != nil {
			name := strings.TrimSpace(m[1])
			out = append(out, Message{
				Sender:     classifySender(name, myName),
				SenderName: name,
				Content:    strings.TrimSpace(m[2]),
				Timestamp:  time.Now(),
			})
			return
		}
		current = &Message{Sender: "other", SenderName: "", Timestamp: time.Now()}
		current.Content = line
	}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		// 跨时间段多选时微信插入的独立时间/日期分隔行，直接忽略，不当作消息内容
		if reTimeSep.MatchString(line) || reDateSep.MatchString(line) {
			continue
		}

		// 形态 1：昵称 + 时间同一行
		if m := reHeader.FindStringSubmatch(line); m != nil {
			commitPending()
			flush()
			name := strings.TrimSpace(m[1])
			current = &Message{
				Sender:     classifySender(name, myName),
				SenderName: name,
				Timestamp:  parseTime(m[2]),
			}
			continue
		}

		// 形态 2：时间单独成行，且上一行被猜测为昵称 —— 上一条消息在此收尾
		if reDateOnly.MatchString(line) {
			if pendingName != "" {
				flush()
				name := strings.TrimSpace(pendingName)
				pendingName = ""
				current = &Message{
					Sender:     classifySender(name, myName),
					SenderName: name,
					Timestamp:  parseTime(line),
				}
				continue
			}
			// 没有待确认昵称，裸时间行按普通内容处理（落入下方正文逻辑）
		} else if looksLikeStandaloneName(line) {
			// 短行先挂起：下一行是时间则判定为昵称，否则按普通内容补录。
			// 即使正在累积上一条消息也允许挂起，以支持「昵称/时间分行」的多消息记录。
			commitPending()
			pendingName = line
			continue
		}

		// 不是任何已知头部，之前的昵称猜测作废（按普通内容补录）
		commitPending()

		// 形态 3：还没有当前消息时，尝试「昵称: 内容」
		if current == nil {
			if m := reWithColon.FindStringSubmatch(line); m != nil {
				name := strings.TrimSpace(m[1])
				out = append(out, Message{
					Sender:     classifySender(name, myName),
					SenderName: name,
					Content:    strings.TrimSpace(m[2]),
					Timestamp:  time.Now(),
				})
				continue
			}
			// 普通文本：作为一条对方消息的正文开始累积
			current = &Message{Sender: "other", SenderName: "", Timestamp: time.Now()}
		}

		// 普通正文行，多行用 \n 连接
		if current.Content != "" {
			current.Content += "\n"
		}
		current.Content += line
	}
	commitPending()
	flush()

	// 最终降级：整段文本作为一条对方消息
	if len(out) == 0 {
		return []Message{{
			Sender:     "other",
			SenderName: "",
			Content:    strings.TrimSpace(text),
			Timestamp:  time.Now(),
		}}
	}
	return out
}
