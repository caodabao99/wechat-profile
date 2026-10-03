package main

// 数据访问抽象层：根据 isRemoteMode 自动切换本地 SQLite 或远程 API

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// uiTaskTimeout 桌面端一次 LLM 任务的最长等待时间。
// 必须大于 LLM 客户端的重试窗口（60s×2 + 2s ≈ 122s），
// 否则会在服务端还在正常重试时就把请求掐断。
const uiTaskTimeout = 3 * time.Minute

func taskContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), uiTaskTimeout)
}

// parseAPIMsgTime 解析 bot 端 API 返回的消息时间。
//
// API 用的是 "2006-01-02 15:04:05"（服务端本地时间，不带时区），
// 这里必须用 ParseInLocation 按本地时区还原，否则 time.Parse 会当成 UTC，
// 界面上所有消息都比实际早 8 小时。解析失败返回零值而不是 0001-01-01 的脏数据。
func parseAPIMsgTime(raw string) time.Time {
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.Local); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	slog.Warn("无法解析消息时间", "raw", raw)
	return time.Time{}
}

// fetchContacts 获取联系人列表（自动适配本地/远程）
func fetchContacts(includeMerged bool) ([]Contact, error) {
	if isRemoteMode {
		return fetchContactsRemote(includeMerged)
	}
	return GetAllContacts(db, includeMerged)
}

func fetchContactsRemote(includeMerged bool) ([]Contact, error) {
	apiList, err := remoteClient.GetContacts(includeMerged)
	if err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(apiList))
	for _, ac := range apiList {
		out = append(out, contactFromAPI(ac))
	}
	return out, nil
}

func contactFromAPI(ac ContactJSON) Contact {
	return Contact{
		ID:             ac.ID,
		Name:           ac.Name,
		Remark:         ac.Remark,
		ProfileJSON:    ac.ProfileJSON,
		ProfileSummary: ac.ProfileSummary,
		OtherMsgCount:  ac.OtherMsgCount,
		LastUpdated:    ac.LastUpdated,
		CreatedAt:      ac.CreatedAt,
		MergedInto:     ac.MergedInto,
		MergeCount:     ac.MergeCount,
		Aliases:        ac.Aliases,
	}
}

// fetchContactByID 获取单个联系人（自动适配本地/远程）
func fetchContactByID(id int64) (*Contact, error) {
	if isRemoteMode {
		ac, err := remoteClient.GetContact(id)
		if err != nil {
			return nil, err
		}
		c := contactFromAPI(*ac)
		return &c, nil
	}
	return GetContactByIDWithMerged(db, id)
}

// fetchMessagesPage 获取消息分页（自动适配本地/远程）
func fetchMessagesPage(contactID int64, offset, limit int) ([]Message, error) {
	if isRemoteMode {
		apiMsgs, err := remoteClient.GetMessages(contactID, offset, limit)
		if err != nil {
			return nil, err
		}
		out := make([]Message, 0, len(apiMsgs))
		for _, am := range apiMsgs {
			ts := parseAPIMsgTime(am.MsgTime)
			out = append(out, Message{
				Sender:    am.Sender,
				Content:   am.Content,
				Timestamp: ts,
			})
		}
		return out, nil
	}
	return GetMessagesPage(db, contactID, offset, limit)
}

// fetchHistory 获取画像历史（自动适配本地/远程）
func fetchHistory(contactID int64, limit int) ([]ProfileHistory, error) {
	if isRemoteMode {
		apiHist, err := remoteClient.GetHistory(contactID, limit)
		if err != nil {
			return nil, err
		}
		out := make([]ProfileHistory, 0, len(apiHist))
		for _, ah := range apiHist {
			out = append(out, ProfileHistory{
				ID:            ah.ID,
				ContactID:     ah.ContactID,
				ProfileJSON:   ah.ProfileJSON,
				ChangeSummary: ah.ChangeSummary,
				CreatedAt:     ah.CreatedAt,
			})
		}
		return out, nil
	}
	return GetProfileHistory(db, contactID, limit)
}

// fetchStats 获取统计信息（自动适配本地/远程）
func fetchStats(contactID int64) (ContactStats, error) {
	if isRemoteMode {
		apiStats, err := remoteClient.GetStats(contactID)
		if err != nil {
			return ContactStats{}, err
		}
		return ContactStats{
			Total:     apiStats.Total,
			Mine:      apiStats.Mine,
			Other:     apiStats.Other,
			FirstTime: apiStats.FirstTime,
			LastTime:  apiStats.LastTime,
		}, nil
	}
	return GetContactStats(db, contactID)
}

// fetchAliases 获取别名列表（自动适配本地/远程）
func fetchAliases(contactID int64) ([]string, error) {
	if isRemoteMode {
		ac, err := remoteClient.GetContact(contactID)
		if err != nil {
			return nil, err
		}
		return ac.Aliases, nil
	}
	return GetAliases(db, contactID)
}

// fetchMergeLogs 获取合并日志（自动适配本地/远程）
func fetchMergeLogs(limit int) ([]MergeLogEntry, error) {
	if isRemoteMode {
		apiLogs, err := remoteClient.GetMergeLogs(limit)
		if err != nil {
			return nil, err
		}
		out := make([]MergeLogEntry, 0, len(apiLogs))
		for _, al := range apiLogs {
			out = append(out, mergeLogFromAPI(al))
		}
		return out, nil
	}
	return GetMergeLogs(db, limit)
}

// fetchMergeLogsForTarget 获取指定目标联系人的合并日志（自动适配本地/远程）
func fetchMergeLogsForTarget(targetID int64) ([]MergeLogEntry, error) {
	if isRemoteMode {
		apiLogs, err := remoteClient.GetMergeLogsForTarget(targetID)
		if err != nil {
			return nil, err
		}
		out := make([]MergeLogEntry, 0, len(apiLogs))
		for _, al := range apiLogs {
			out = append(out, mergeLogFromAPI(al))
		}
		return out, nil
	}
	return GetMergeLogsForTarget(db, targetID)
}

func mergeLogFromAPI(al MergeLogJSON) MergeLogEntry {
	return MergeLogEntry{
		ID:         al.ID,
		SourceID:   al.SourceID,
		TargetID:   al.TargetID,
		SourceName: al.SourceName,
		TargetName: al.TargetName,
		CreatedAt:  al.CreatedAt,
		UndoneAt:   al.UndoneAt,
	}
}

// doMerge 执行合并（自动适配本地/远程）
func doMerge(sourceID, targetID int64, useSourceName, regenerate bool) (MergeResult, error) {
	if isRemoteMode {
		apiResult, err := remoteClient.MergeContacts(sourceID, targetID, useSourceName, regenerate)
		if err != nil {
			return MergeResult{}, err
		}
		return MergeResult{
			MovedMessages: apiResult.MovedMessages,
			MovedHistory:  apiResult.MovedHistory,
			MergeLogID:    apiResult.MergeLogID,
			ProfileCopied: apiResult.ProfileCopied,
		}, nil
	}
	return MergeContacts(db, sourceID, targetID, MergeOptions{
		UseSourceNameAsDisplay: useSourceName,
		RegenerateProfile:      regenerate,
	})
}

// doUndoMerge 撤销合并（自动适配本地/远程）
func doUndoMerge(logID int64) error {
	if isRemoteMode {
		return remoteClient.UndoMerge(logID)
	}
	return UndoMerge(db, logID)
}

func doEditProfile(contactID int64, profile Profile, base string) error {
	if isRemoteMode {
		return remoteClient.EditProfile(contactID, profile, base)
	}
	return EditProfile(db, contactID, profile, base)
}

// doSupplement 补充画像（自动适配本地/远程）
func doSupplement(contactID int64, note string) error {
	if isRemoteMode {
		return remoteClient.SupplementProfile(contactID, note)
	}
	ctx, cancel := taskContext()
	defer cancel()
	return SupplementProfile(ctx, db, llmClient, contactID, "", note)
}

// doRegenerate 重新生成画像（自动适配本地/远程）
func doRegenerate(contactID int64) error {
	if isRemoteMode {
		_, err := remoteClient.RegenerateProfile(contactID)
		return err
	}
	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return err
	}
	msgs, err := GetAllMessages(db, contactID)
	if err != nil {
		return err
	}
	ctx, cancel := taskContext()
	defer cancel()
	return GenerateOrUpdateProfile(ctx, db, llmClient, contactID, contact.Name, msgs)
}

// doSetRemark 设置备注（自动适配本地/远程）
func doSetRemark(contactID int64, remark string) error {
	if isRemoteMode {
		return remoteClient.SetRemark(contactID, remark)
	}
	return UpdateContactRemark(db, contactID, remark)
}

// doDeleteContact 删除联系人（自动适配本地/远程）
func doDeleteContact(contactID int64) error {
	if isRemoteMode {
		return remoteClient.DeleteContact(contactID)
	}
	return DeleteContactByID(db, contactID)
}

func doRewriteReply(id int64, text, style string) (string, error) {
	if isRemoteMode {
		return remoteClient.RewriteReply(id, text, style)
	}
	ctx, cancel := taskContext()
	defer cancel()
	return RewriteReply(ctx, db, llmClient, id, text, style)
}
func doReviewDraft(id int64, text string) (*DraftReview, error) {
	if isRemoteMode {
		return remoteClient.ReviewDraft(id, text)
	}
	ctx, cancel := taskContext()
	defer cancel()
	return ReviewDraft(ctx, db, llmClient, id, text)
}
func fetchProfileChanges(id int64) (ProfileChanges, error) {
	if isRemoteMode {
		return remoteClient.ProfileChanges(id)
	}
	return GetProfileChanges(db, id)
}

// doAnalyzeIntent 意图分析（自动适配本地/远程）
func doAnalyzeIntent(contactID int64, message string) (map[string]interface{}, error) {
	if isRemoteMode {
		return remoteClient.AnalyzeIntent(contactID, message)
	}
	msgs, err := GetRecentMessages(db, contactID, 50)
	if err != nil {
		return nil, err
	}
	ctx, cancel := taskContext()
	defer cancel()
	return AnalyzeIntent(ctx, db, llmClient, contactID, message, msgs)
}

// doResolveContact 解析联系人名称（自动适配本地/远程）
func doResolveContact(name string) (int64, bool, error) {
	if isRemoteMode {
		return remoteClient.ResolveContact(name)
	}
	return ResolveContactID(db, name)
}

// doSaveMessages 保存消息（自动适配本地/远程）
func doSaveMessages(contactID int64, messages []Message) (int, error) {
	if isRemoteMode {
		// 远程模式：通过 ingest 接口提交，这里只做解析后存库
		// 实际上远程模式下消息已经在 bot 端存好了，这里不需要重复存
		// 但为了兼容本地逻辑，返回 0 即可
		return 0, nil
	}
	return SaveMessages(db, contactID, messages)
}

// doShouldGenerateProfile 检查是否应该生成画像（自动适配本地/远程）
func doShouldGenerateProfile(contactID int64) bool {
	if isRemoteMode {
		// 远程模式下由 bot 端判断，这里返回 false 避免重复触发
		return false
	}
	return ShouldGenerateProfile(db, contactID)
}

// doShouldUpdateProfile 检查是否应该更新画像（自动适配本地/远程）
func doShouldUpdateProfile(contactID int64) bool {
	if isRemoteMode {
		// 远程模式下由 bot 端判断，这里返回 false 避免重复触发
		return false
	}
	return ShouldUpdateProfile(db, contactID)
}

// doGenerateOrUpdateProfile 生成或更新画像（自动适配本地/远程）
func doGenerateOrUpdateProfile(contactID int64, contactName string, messages []Message) error {
	if isRemoteMode {
		return nil // 远程模式下由 bot 端处理
	}
	ctx, cancel := taskContext()
	defer cancel()
	return GenerateOrUpdateProfile(ctx, db, llmClient, contactID, contactName, messages)
}

// doSaveProfileHistory 保存画像历史（自动适配本地/远程）
func doSaveProfileHistory(contactID int64, profileJSON, changeSummary string) error {
	if isRemoteMode {
		return nil // 远程模式下由 bot 端处理
	}
	return saveProfileHistory(db, contactID, profileJSON, changeSummary)
}

// doGetContactByID 获取联系人（自动适配本地/远程）
func doGetContactByID(id int64) (*Contact, error) {
	if isRemoteMode {
		ac, err := remoteClient.GetContact(id)
		if err != nil {
			return nil, err
		}
		c := contactFromAPI(*ac)
		return &c, nil
	}
	return GetContactByID(db, id)
}

// doGetAllMessages 获取全部消息（自动适配本地/远程）
func doGetAllMessages(contactID int64) ([]Message, error) {
	if isRemoteMode {
		// 远程模式：分页拉取全部消息
		var all []Message
		offset := 0
		// 必须和服务端的 maxMessagesPageLimit 一致：服务端会把超额的 limit 截到 500，
		// 这里若写 1000，第一页拿回 500 条就满足 len(page) < limit 而提前退出，
		// 结果只导入前 500 条消息且毫无提示。
		limit := 500
		for {
			page, err := remoteClient.GetMessages(contactID, offset, limit)
			if err != nil {
				return nil, err
			}
			if len(page) == 0 {
				break
			}
			for _, am := range page {
				ts := parseAPIMsgTime(am.MsgTime)
				all = append(all, Message{
					Sender:    am.Sender,
					Content:   am.Content,
					Timestamp: ts,
				})
			}
			if len(page) < limit {
				break
			}
			offset += limit
		}
		// 反转为时间正序
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
		return all, nil
	}
	return GetAllMessages(db, contactID)
}

// doGetOrCreateContact 获取或创建联系人（自动适配本地/远程）
func doGetOrCreateContact(name string) (int64, error) {
	if isRemoteMode {
		return remoteClient.CreateContact(name)
	}
	return GetOrCreateContact(db, name)
}

// doUpdateContactName 更新联系人名称（自动适配本地/远程）
func doUpdateContactName(contactID int64, newName string) error {
	if isRemoteMode {
		return remoteClient.SetName(contactID, newName)
	}
	return UpdateContactName(db, contactID, newName)
}

// doIsMerged 检查联系人是否已合并（自动适配本地/远程）
func doIsMerged(contactID int64) (bool, int64, error) {
	if isRemoteMode {
		ac, err := remoteClient.GetContact(contactID)
		if err != nil {
			return false, 0, err
		}
		return ac.MergedInto != 0, ac.MergedInto, nil
	}
	return IsMerged(db, contactID)
}

// doGetMergeCandidates 获取合并候选列表（自动适配本地/远程）
func doGetMergeCandidates(excludeID int64) ([]Contact, error) {
	if isRemoteMode {
		// 远程模式：获取全部联系人，排除已合并的
		apiList, err := remoteClient.GetContacts(false)
		if err != nil {
			return nil, err
		}
		out := make([]Contact, 0, len(apiList))
		for _, ac := range apiList {
			if ac.ID != excludeID && ac.MergedInto == 0 {
				out = append(out, contactFromAPI(ac))
			}
		}
		return out, nil
	}
	return GetMergeCandidates(db, excludeID)
}

// ShowResultWindowRemote 远程模式的结果窗口（从 API 返回的数据直接展示）。
// 布局与本地 ShowResultWindow 一致：CompactHeight 卡片 + 按内容自动开窗。
func ShowResultWindowRemote(result *IngestResult) {
	// -1 表示这次没有置信度可显示
	confidenceVal := -1

	headerText := result.DisplayName
	if result.NewCount > 0 {
		headerText += fmt.Sprintf(" · 本次新增 %d 条", result.NewCount)
	}
	if result.ViaAlias {
		headerText += " · 旧昵称已归位"
	}
	if result.ProfileTriggered {
		headerText += " · 画像更新中"
	}

	body := []dl.Widget{
		dl.Label{Text: headerText, Font: fontTitle},
	}

	// 意图分析结果
	if result.IntentError != "" {
		body = append(body,
			dl.GroupBox{
				Title:  "分析失败",
				Layout: dl.VBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.TextEdit{
						ReadOnly:      true,
						CompactHeight: true,
						Text:          result.IntentError,
					},
				},
			},
		)
		presentResultDialog(result.ContactID, body, confidenceVal)
		return
	}

	if result.Intent == nil {
		presentResultDialog(result.ContactID, body, confidenceVal)
		return
	}

	cardKeys := []struct {
		title string
		key   string
	}{
		{"表面意思", "surface"},
		{"潜在意图", "intent"},
		{"情绪状态", "emotion"},
		{"潜台词", "subtext"},
	}
	for i, reply := range suggestedReplyItems(result.Intent) {
		key := fmt.Sprintf("suggested_reply_%d", i)
		result.Intent[key] = reply.Text
		cardKeys = append(cardKeys, struct{ title, key string }{reply.Style, key})
	}

	for _, c := range cardKeys {
		content := fieldString(result.Intent, c.key)
		if strings.HasPrefix(c.key, "suggested_reply_") {
			var suggestionTE *walk.TextEdit
			controls := rewriteControls(result.ContactID, &suggestionTE)
			body = append(body, controls,
				dl.GroupBox{
					Title:  c.title,
					Layout: dl.VBox{MarginsZero: true, Spacing: 4},
					Children: []dl.Widget{
						dl.Composite{
							Layout: dl.HBox{MarginsZero: true, Spacing: 4},
							Children: []dl.Widget{
								dl.TextEdit{
									AssignTo:      &suggestionTE,
									ReadOnly:      true,
									CompactHeight: true,
									Text:          content,
								},
								dl.PushButton{
									Text:    "复制",
									MinSize: dl.Size{Width: 60, Height: 28},
									OnClicked: func() {
										if suggestionTE != nil {
											_ = clipboard.WriteAll(suggestionTE.Text())
										}
									},
								},
							},
						},
					},
				},
			)
		} else {
			body = append(body,
				dl.GroupBox{
					Title:  c.title,
					Layout: dl.VBox{MarginsZero: true},
					Children: []dl.Widget{
						dl.TextEdit{
							ReadOnly:      true,
							CompactHeight: true,
							Text:          content,
						},
					},
				},
			)
		}
	}

	// 置信度：标签初值就是格式化好的百分比，别把原始的 "0.85" 露给用户
	confidenceVal = parseConfidence(fieldString(result.Intent, "confidence"))
	presentResultDialog(result.ContactID, body, confidenceVal)
}
