package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type mergeMessageSnapshot struct {
	ID                    int64
	Sender, Content, Hash string
	Time, Captured        sql.NullString
}

type mergeProfileSnapshot struct {
	JSON, Summary, Updated sql.NullString
	Count                  int
}

// MergeOptions 合并选项
type MergeOptions struct {
	UseSourceNameAsDisplay bool // 显示名改用新昵称（源昵称）
	// RegenerateProfile 仅用于画像历史的文案描述。MergeContacts 处在数据层，
	// 拿不到 LLMClient，自身不会重生成画像；真正的重生成由调用方负责
	// （bot 端 api.go hMerge / bot.go mergeContacts，桌面端 ui_merge.go）。
	RegenerateProfile bool
}

// MergeResult 合并结果
type MergeResult struct {
	MovedMessages int
	MovedHistory  int
	MergeLogID    int64
	// ProfileCopied 为 true 表示本次合并把 source 的画像拷贝给了 target。
	// 撤销合并时必须据此清空 target 的画像，否则 target 会永久留着一份
	// 基于「source+target 合并消息集」生成的画像。
	ProfileCopied bool
}

// isSelfName 判断一个联系人名是不是代表「我本人」。
// 把别人合并进「我」会让 other_msg_count 归零、画像彻底失真，必须拦掉。
func isSelfName(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	if n == "我" || strings.EqualFold(n, "me") {
		return true
	}
	if me := strings.TrimSpace(config.MyName); me != "" && n == me {
		return true
	}
	return false
}

// MergeContacts 把 sourceID 合并到 targetID，单事务完成
func MergeContacts(db *sql.DB, sourceID, targetID int64, opts MergeOptions) (MergeResult, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	var result MergeResult

	// 前置校验
	if sourceID == targetID {
		return result, fmt.Errorf("不能合并到自身")
	}

	var sourceName, targetName string
	var sourceMerged, targetMerged sql.NullInt64
	err := db.QueryRow(`SELECT name, merged_into FROM contacts WHERE id = ?`, sourceID).
		Scan(&sourceName, &sourceMerged)
	if err != nil {
		return result, fmt.Errorf("源联系人不存在: %w", err)
	}
	err = db.QueryRow(`SELECT name, merged_into FROM contacts WHERE id = ?`, targetID).
		Scan(&targetName, &targetMerged)
	if err != nil {
		return result, fmt.Errorf("目标联系人不存在: %w", err)
	}
	if sourceMerged.Valid {
		return result, fmt.Errorf("源联系人已被合并")
	}
	if targetMerged.Valid {
		return result, fmt.Errorf("目标联系人已被合并")
	}
	if isSelfName(sourceName) {
		return result, fmt.Errorf("不能合并自己")
	}
	if isSelfName(targetName) {
		return result, fmt.Errorf("不能把其他联系人合并进自己")
	}

	tx, err := db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	// 1. 记录要搬移的消息 ID（用于 merge_log）
	var msgIDs []int64
	rows, err := tx.Query(`SELECT id FROM messages WHERE contact_id = ?`, sourceID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		msgIDs = append(msgIDs, id)
	}
	rows.Close()

	// 2. 搬移消息（UPDATE OR IGNORE 处理 UNIQUE(contact_id, msg_hash) 冲突）
	res, err := tx.Exec(`UPDATE OR IGNORE messages SET contact_id = ? WHERE contact_id = ?`, targetID, sourceID)
	if err != nil {
		return result, err
	}
	movedMsgs, _ := res.RowsAffected()
	result.MovedMessages = int(movedMsgs)

	var deleted []mergeMessageSnapshot
	rows, err = tx.Query(`SELECT id, sender, content, msg_hash, msg_time, captured_at FROM messages WHERE contact_id = ?`, sourceID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var m mergeMessageSnapshot
		if err := rows.Scan(&m.ID, &m.Sender, &m.Content, &m.Hash, &m.Time, &m.Captured); err != nil {
			rows.Close()
			return result, err
		}
		deleted = append(deleted, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	deletedJSON, err := json.Marshal(deleted)
	if err != nil {
		return result, err
	}
	var before mergeProfileSnapshot
	if err := tx.QueryRow(`SELECT profile_json, profile_summary, last_updated, COALESCE(profile_msg_count,0) FROM contacts WHERE id = ?`, targetID).Scan(&before.JSON, &before.Summary, &before.Updated, &before.Count); err != nil {
		return result, err
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		return result, err
	}

	// 3. 清理源联系人残留的重复消息（撞唯一键没被搬走的）
	if _, err := tx.Exec(`DELETE FROM messages WHERE contact_id = ?`, sourceID); err != nil {
		return result, err
	}

	// 4. 搬移画像历史
	var historyIDs []int64
	rows, err = tx.Query(`SELECT id FROM profile_history WHERE contact_id = ?`, sourceID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		historyIDs = append(historyIDs, id)
	}
	rows.Close()

	res, err = tx.Exec(`UPDATE profile_history SET contact_id = ? WHERE contact_id = ?`, targetID, sourceID)
	if err != nil {
		return result, err
	}
	movedHist, _ := res.RowsAffected()
	result.MovedHistory = int(movedHist)

	// 5. 画像迁移：target 为空且 source 有画像 → 拷贝
	var targetProfileJSON, targetProfileSummary string
	err = tx.QueryRow(`SELECT COALESCE(profile_json, ''), COALESCE(profile_summary, '') FROM contacts WHERE id = ?`, targetID).
		Scan(&targetProfileJSON, &targetProfileSummary)
	if err != nil {
		return result, err
	}
	profileCopied := false
	if targetProfileJSON == "" || targetProfileJSON == "{}" {
		var sourceProfileJSON, sourceProfileSummary string
		err = tx.QueryRow(`SELECT COALESCE(profile_json, ''), COALESCE(profile_summary, '') FROM contacts WHERE id = ?`, sourceID).
			Scan(&sourceProfileJSON, &sourceProfileSummary)
		if err != nil {
			return result, err
		}
		if sourceProfileJSON != "" && sourceProfileJSON != "{}" {
			if _, err := tx.Exec(`UPDATE contacts SET profile_json = ?, profile_summary = ?, last_updated = ? WHERE id = ?`,
				sourceProfileJSON, sourceProfileSummary, time.Now().Format(time.RFC3339), targetID); err != nil {
				return result, err
			}
			// 记下拷贝事实：撤销合并时要靠它把 target 的画像清空
			profileCopied = true
			targetProfileJSON, targetProfileSummary = sourceProfileJSON, sourceProfileSummary
		}
	}
	result.ProfileCopied = profileCopied

	// 6. 重算 target 的 other_msg_count（全量，不能累加）
	if _, err := tx.Exec(`UPDATE contacts SET other_msg_count = (
		SELECT COUNT(*) FROM messages WHERE contact_id = ? AND sender = 'other'
	) WHERE id = ?`, targetID, targetID); err != nil {
		return result, err
	}

	// 7. 显示名处理
	if opts.UseSourceNameAsDisplay {
		// 先把 source 改名避开 UNIQUE 冲突
		tmpName := sourceName + "#merged-" + fmt.Sprint(sourceID)
		if _, err := tx.Exec(`UPDATE contacts SET name = ? WHERE id = ?`, tmpName, sourceID); err != nil {
			return result, err
		}
		// 再把 target 改成 source 旧名
		if _, err := tx.Exec(`UPDATE contacts SET name = ? WHERE id = ?`, sourceName, targetID); err != nil {
			return result, err
		}
	}

	// 8. 登记别名（source 旧名和 target 原名的旧值）
	// targetName 变量在函数开头就取好了，是 target 改名前的名字，
	// 因此即使 UseSourceNameAsDisplay 把 target 改成了 sourceName，
	// 这里登记的两条别名依然是「source 原名」和「target 原名」。
	for _, alias := range []string{sourceName, targetName} {
		if alias == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO contact_aliases (contact_id, alias, created_at) VALUES (?, ?, ?)
			ON CONFLICT(alias) DO UPDATE SET contact_id = excluded.contact_id`,
			targetID, alias, time.Now().Format(time.RFC3339)); err != nil {
			return result, err
		}
	}

	// 9. 标记 source 已合并
	if _, err := tx.Exec(`UPDATE contacts SET merged_into = ? WHERE id = ?`, targetID, sourceID); err != nil {
		return result, err
	}

	// 10. 写 merge_log
	msgIDsJSON, _ := json.Marshal(msgIDs)
	historyIDsJSON, _ := json.Marshal(historyIDs)
	res, err = tx.Exec(`INSERT INTO merge_log (source_id, target_id, source_name, target_name, moved_message_ids, moved_history_ids, profile_copied, created_at, deleted_messages, target_profile)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceID, targetID, sourceName, targetName, string(msgIDsJSON), string(historyIDsJSON),
		profileCopied, time.Now().Format(time.RFC3339), string(deletedJSON), string(beforeJSON))
	if err != nil {
		return result, err
	}
	result.MergeLogID, _ = res.LastInsertId()

	// 11. 合并事件写入目标的画像历史（必须在事务内写——saveProfileHistory 会再加 dbMu 锁，
	// 而本函数持有锁未释放，事务外调用会死锁，表现为点击确认后程序卡死）
	// profile_json 存合并后 target 的真实画像快照，而不是空对象 "{}"：
	// 否则历史页会出现一条「有摘要但画像内容全空」的记录，看起来像数据丢失。
	summary := fmt.Sprintf("合并联系人「%s」：搬移 %d 条消息、%d 条画像历史",
		sourceName, result.MovedMessages, result.MovedHistory)
	if profileCopied {
		summary += "，并沿用源联系人的画像"
	}
	if opts.RegenerateProfile {
		summary += "，画像将在后台重新生成"
	}
	snapshot := strings.TrimSpace(targetProfileJSON)
	if snapshot == "" {
		snapshot = "{}"
	}
	if _, err := tx.Exec(`INSERT INTO profile_history (contact_id, profile_json, change_summary, created_at)
		VALUES (?, ?, ?, ?)`, targetID, snapshot, summary, time.Now().Format(time.RFC3339)); err != nil {
		return result, err
	}

	if err := tx.Commit(); err != nil {
		return result, err
	}

	profileEpoch++
	return result, nil
}

// RecomputeOtherMsgCount 全量重算联系人的对方消息数
func RecomputeOtherMsgCount(db *sql.DB, contactID int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	_, err := db.Exec(`UPDATE contacts SET other_msg_count = (
		SELECT COUNT(*) FROM messages WHERE contact_id = ? AND sender = 'other'
	) WHERE id = ?`, contactID, contactID)
	return err
}

// GetMergeCandidates 获取可合并的目标联系人列表（排除自己和已合并的）
func GetMergeCandidates(db *sql.DB, excludeID int64) ([]Contact, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(
		`SELECT id, name, COALESCE(remark,''), COALESCE(profile_json,'{}'),
		        COALESCE(profile_summary,''), other_msg_count,
		        COALESCE(last_updated,''), created_at
		 FROM contacts WHERE merged_into IS NULL AND id != ?
		 ORDER BY strftime('%s', last_updated) DESC, id DESC`, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Contact{} // 空结果也返回 [] 而非 nil（JSON null）
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.Name, &c.Remark, &c.ProfileJSON,
			&c.ProfileSummary, &c.OtherMsgCount, &c.LastUpdated, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UndoMerge 撤销合并（按 merge_log 逆向搬回）
func UndoMerge(db *sql.DB, mergeLogID int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	var log struct {
		SourceID        int64
		TargetID        int64
		SourceName      string
		TargetName      string
		MovedMessageIDs string
		MovedHistoryIDs string
		ProfileCopied   int
		UndoneAt        sql.NullString
		DeletedMessages string
		TargetProfile   string
	}
	err := db.QueryRow(`SELECT source_id, target_id, source_name, target_name, moved_message_ids, moved_history_ids,
		COALESCE(profile_copied, 0), undone_at, COALESCE(deleted_messages,'[]'), COALESCE(target_profile,'')
		FROM merge_log WHERE id = ?`, mergeLogID).
		Scan(&log.SourceID, &log.TargetID, &log.SourceName, &log.TargetName,
			&log.MovedMessageIDs, &log.MovedHistoryIDs, &log.ProfileCopied, &log.UndoneAt, &log.DeletedMessages, &log.TargetProfile)
	if err != nil {
		return fmt.Errorf("合并日志不存在: %w", err)
	}
	if log.UndoneAt.Valid {
		return fmt.Errorf("该合并已撤销")
	}

	var dependent int
	if err := db.QueryRow(`SELECT COUNT(*) FROM merge_log WHERE id > ? AND undone_at IS NULL AND (source_id IN (?,?) OR target_id IN (?,?))`, mergeLogID, log.SourceID, log.TargetID, log.SourceID, log.TargetID).Scan(&dependent); err != nil {
		return err
	}
	if dependent != 0 {
		return fmt.Errorf("请先撤销涉及这些联系人的后续合并")
	}

	var msgIDs, historyIDs []int64
	if err := json.Unmarshal([]byte(log.MovedMessageIDs), &msgIDs); err != nil {
		return fmt.Errorf("解析消息ID列表失败: %w", err)
	}
	if err := json.Unmarshal([]byte(log.MovedHistoryIDs), &historyIDs); err != nil {
		return fmt.Errorf("解析历史ID列表失败: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 恢复 source 的 merged_into 标记
	if _, err := tx.Exec(`UPDATE contacts SET merged_into = NULL WHERE id = ?`, log.SourceID); err != nil {
		return err
	}

	// 2. 搬回消息
	for _, msgID := range msgIDs {
		if _, err := tx.Exec(`UPDATE messages SET contact_id = ? WHERE id = ?`, log.SourceID, msgID); err != nil {
			return err
		}
	}

	var deleted []mergeMessageSnapshot
	if err := json.Unmarshal([]byte(log.DeletedMessages), &deleted); err != nil {
		return fmt.Errorf("解析重复消息快照失败: %w", err)
	}
	for _, m := range deleted {
		if _, err := tx.Exec(`INSERT INTO messages (id, contact_id, sender, content, msg_hash, msg_time, captured_at) VALUES (?,?,?,?,?,?,?)`, m.ID, log.SourceID, m.Sender, m.Content, m.Hash, m.Time, m.Captured); err != nil {
			return err
		}
	}

	// 3. 搬回历史
	for _, histID := range historyIDs {
		if _, err := tx.Exec(`UPDATE profile_history SET contact_id = ? WHERE id = ?`, log.SourceID, histID); err != nil {
			return err
		}
	}

	// 4. 恢复名称：
	//    - target 仅当初用 source 名作显示（当前名 == log.SourceName）时才改回 log.TargetName，
	//      否则保留用户合并后可能改过的新名字；
	//    - log.TargetName 已被别的联系人占用时，退而用 "log.TargetName#undo-<logID>" 这个唯一
	//      回退名。目的是腾出 log.SourceName：source 的名字是后续消息匹配用的身份，
	//      比 target 的显示名更重要，不能让它永久卡在 "xxx#merged-<id>" 临时名上；
	//    - source 从 "xxx#merged-<id>" 临时名改回 log.SourceName；
	//    - 任何情况下都不让 UNIQUE 冲突中断撤销。
	nameTaken := func(name string, exceptID int64) bool {
		var id int64
		err := tx.QueryRow(`SELECT id FROM contacts WHERE name = ?`, name).Scan(&id)
		return err == nil && id != exceptID
	}

	var curTargetName, curSourceName string
	if err := tx.QueryRow(`SELECT name FROM contacts WHERE id = ?`, log.TargetID).Scan(&curTargetName); err == nil {
		if curTargetName == log.SourceName {
			newName := log.TargetName
			if nameTaken(newName, log.TargetID) {
				newName = fmt.Sprintf("%s#undo-%d", log.TargetName, mergeLogID)
			}
			if newName != curTargetName && !nameTaken(newName, log.TargetID) {
				if _, err := tx.Exec(`UPDATE contacts SET name = ? WHERE id = ?`, newName, log.TargetID); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.QueryRow(`SELECT name FROM contacts WHERE id = ?`, log.SourceID).Scan(&curSourceName); err == nil {
		if curSourceName != log.SourceName && !nameTaken(log.SourceName, log.SourceID) {
			if _, err := tx.Exec(`UPDATE contacts SET name = ? WHERE id = ?`, log.SourceName, log.SourceID); err != nil {
				return err
			}
		}
	}

	// 4b. 回收别名：合并时把 source 名和 target 名都登记成了 target 的别名，
	//     撤销后 source 名必须归还给 source，否则 FindContactID 会解析到错误的联系人。
	if _, err := tx.Exec(`UPDATE contact_aliases SET contact_id = ? WHERE alias = ? AND contact_id = ?`,
		log.SourceID, log.SourceName, log.TargetID); err != nil {
		return err
	}
	// 别名与自己当前名字相同的记录是冗余的（名字匹配优先于别名），顺手清掉
	for _, cid := range []int64{log.SourceID, log.TargetID} {
		if _, err := tx.Exec(
			`DELETE FROM contact_aliases WHERE contact_id = ? AND alias = (SELECT name FROM contacts WHERE id = ?)`,
			cid, cid); err != nil {
			return err
		}
	}

	// 5. 重算双方计数
	for _, cid := range []int64{log.SourceID, log.TargetID} {
		if _, err := tx.Exec(`UPDATE contacts SET other_msg_count = (
			SELECT COUNT(*) FROM messages WHERE contact_id = ? AND sender = 'other'
		) WHERE id = ?`, cid, cid); err != nil {
			return err
		}
	}

	// 5b. 回滚画像拷贝：合并时若把 source 的画像拷给了 target，撤销后 target 的消息集
	//     已经变回去了，那份画像（尤其是合并后又基于合并消息集重新生成过的）不再可信，
	//     必须清空，让它下次重新生成。
	if log.TargetProfile != "" {
		var before mergeProfileSnapshot
		if err := json.Unmarshal([]byte(log.TargetProfile), &before); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE contacts SET profile_json=?, profile_summary=?, last_updated=?, profile_msg_count=? WHERE id=?`, before.JSON, before.Summary, before.Updated, before.Count, log.TargetID); err != nil {
			return err
		}
	} else {
		// Legacy logs have no trustworthy pre-merge profile snapshot.
		if _, err := tx.Exec(`UPDATE contacts SET profile_json='{}', profile_summary='', profile_msg_count=0 WHERE id=?`, log.TargetID); err != nil {
			return err
		}
	}

	// 6. 标记撤销时间（本地时间，不用 CURRENT_TIMESTAMP）
	if _, err := tx.Exec(`UPDATE merge_log SET undone_at = ? WHERE id = ?`,
		time.Now().Format(time.RFC3339), mergeLogID); err != nil {
		return err
	}

	// 7. 撤销事件写入双方的画像历史（必须在事务内——saveProfileHistory 会再加 dbMu 锁，
	// 本函数持锁未释放时调用会死锁）
	// profile_json 存撤销后各自的真实画像快照，而不是硬编码的 "{}"，
	// 否则历史页会出现「有摘要但内容全空」的记录。
	summary := fmt.Sprintf("撤销合并「%s」：消息与画像历史已搬回", log.SourceName)
	now := time.Now().Format(time.RFC3339)
	for _, cid := range []int64{log.SourceID, log.TargetID} {
		snapshot := "{}"
		if err := tx.QueryRow(`SELECT COALESCE(profile_json, '{}') FROM contacts WHERE id = ?`, cid).
			Scan(&snapshot); err != nil && err != sql.ErrNoRows {
			return err
		}
		if strings.TrimSpace(snapshot) == "" {
			snapshot = "{}"
		}
		if _, err := tx.Exec(`INSERT INTO profile_history (contact_id, profile_json, change_summary, created_at)
			VALUES (?, ?, ?, ?)`, cid, snapshot, summary, now); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	profileEpoch++
	return nil
}

// GetAliases 获取联系人的已确认别名列表
func GetAliases(db *sql.DB, contactID int64) ([]string, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	// created_at 在老库里混有 CURRENT_TIMESTAMP（UTC）与 RFC3339（本地时间）两种格式，
	// 按它排序结果不可靠；id 单调递增，用它保证「先登记的别名排前面」。
	rows, err := db.Query(`SELECT alias FROM contact_aliases WHERE contact_id = ? ORDER BY id`, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{} // 空结果也返回 [] 而非 nil（JSON null）
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		out = append(out, alias)
	}
	return out, rows.Err()
}

// GetMergeLogs 获取合并日志（用于历史页显示和撤销）
func GetMergeLogs(db *sql.DB, limit int) ([]MergeLogEntry, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(
		`SELECT id, source_id, target_id, source_name, target_name, created_at, undone_at
		 FROM merge_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 初始化为空切片而不是 nil：无记录时 JSON 序列化成 [] 而不是 null。
	// 网页端拿到 null 后访问 .length 会抛 TypeError 导致整个页面渲染崩溃白屏。
	out := []MergeLogEntry{}
	for rows.Next() {
		var e MergeLogEntry
		var undoneAt sql.NullString
		if err := rows.Scan(&e.ID, &e.SourceID, &e.TargetID, &e.SourceName, &e.TargetName, &e.CreatedAt, &undoneAt); err != nil {
			return nil, err
		}
		e.UndoneAt = undoneAt.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// MergeLogEntry 合并日志条目
type MergeLogEntry struct {
	ID         int64
	SourceID   int64
	TargetID   int64
	SourceName string
	TargetName string
	CreatedAt  string
	UndoneAt   string
}

// GetMergeLogsForTarget 查询指定联系人作为 target 且未撤销的合并记录
func GetMergeLogsForTarget(db *sql.DB, targetID int64) ([]MergeLogEntry, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(
		`SELECT id, source_id, target_id, source_name, target_name, created_at, undone_at
		 FROM merge_log WHERE target_id = ? AND undone_at IS NULL ORDER BY id DESC`,
		targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 初始化为空切片而不是 nil：无记录时 JSON 序列化成 [] 而不是 null。
	// 网页端拿到 null 后访问 .length 会抛 TypeError 导致整个页面渲染崩溃白屏。
	out := []MergeLogEntry{}
	for rows.Next() {
		var e MergeLogEntry
		var undoneAt sql.NullString
		if err := rows.Scan(&e.ID, &e.SourceID, &e.TargetID, &e.SourceName, &e.TargetName, &e.CreatedAt, &undoneAt); err != nil {
			return nil, err
		}
		e.UndoneAt = undoneAt.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// IsMerged 检查联系人是否已被合并
func IsMerged(db *sql.DB, contactID int64) (bool, int64, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	var mergedInto sql.NullInt64
	err := db.QueryRow(`SELECT merged_into FROM contacts WHERE id = ?`, contactID).Scan(&mergedInto)
	if err != nil {
		return false, 0, err
	}
	return mergedInto.Valid, mergedInto.Int64, nil
}

// GetContactByIDWithMerged 按 id 查询联系人（含 merged_into 和 aliases）
func GetContactByIDWithMerged(db *sql.DB, id int64) (*Contact, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	var c Contact
	var remark, profileJSON, profileSummary, lastUpdated sql.NullString
	var mergedInto sql.NullInt64
	err := db.QueryRow(
		`SELECT id, name, remark, profile_json, profile_summary,
		        other_msg_count, last_updated, created_at, merged_into
		 FROM contacts WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &remark, &profileJSON, &profileSummary,
			&c.OtherMsgCount, &lastUpdated, &c.CreatedAt, &mergedInto)
	if err != nil {
		return nil, err
	}
	c.Remark = remark.String
	c.ProfileJSON = profileJSON.String
	c.ProfileSummary = profileSummary.String
	c.LastUpdated = lastUpdated.String
	if mergedInto.Valid {
		c.MergedInto = mergedInto.Int64
	}

	// 内联查询别名（不能调 GetAliases，会重复加 dbMu 锁导致死锁）
	// 同 GetAliases：created_at 格式不统一，用 id 排序才可靠
	rows, err := db.Query(`SELECT alias FROM contact_aliases WHERE contact_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		c.Aliases = append(c.Aliases, alias)
	}
	return &c, rows.Err()
}

// UpdateContactName 更新联系人显示名（同时处理 UNIQUE 冲突）
//
// 改名后必须把旧名字登记成别名：消息入库时是按「名字 → 别名 → 新建」三级匹配的，
// 如果旧名字没留成别名，之后仍带旧昵称发来的历史消息会被建出一个新的重复联系人。
func UpdateContactName(db *sql.DB, contactID int64, newName string) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("名称不能为空")
	}
	if isSelfName(newName) {
		return fmt.Errorf("不能使用自己的名字作为联系人名称")
	}

	// 检查新名字是否已被占用
	var existingID int64
	err := db.QueryRow(`SELECT id FROM contacts WHERE name = ? AND id != ?`, newName, contactID).Scan(&existingID)
	if err == nil {
		return fmt.Errorf("名称已被其他联系人使用")
	}
	if err != sql.ErrNoRows {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var oldName string
	if err := tx.QueryRow(`SELECT name FROM contacts WHERE id = ?`, contactID).Scan(&oldName); err != nil {
		return fmt.Errorf("联系人不存在: %w", err)
	}
	if oldName == newName {
		return tx.Commit()
	}

	if _, err := tx.Exec(`UPDATE contacts SET name = ?, last_updated = ? WHERE id = ?`,
		newName, time.Now().Format(time.RFC3339), contactID); err != nil {
		return err
	}

	// 新名字既然已经成了本联系人的正式名字，就不该再留一条同名别名
	if _, err := tx.Exec(`DELETE FROM contact_aliases WHERE alias = ? AND contact_id = ?`,
		newName, contactID); err != nil {
		return err
	}

	// 旧名字转成别名（旧名字可能已挂在别的联系人上，用 upsert 抢过来）
	if _, err := tx.Exec(`INSERT INTO contact_aliases (contact_id, alias, created_at) VALUES (?, ?, ?)
		ON CONFLICT(alias) DO UPDATE SET contact_id = excluded.contact_id`,
		contactID, oldName, time.Now().Format(time.RFC3339)); err != nil {
		return err
	}

	return tx.Commit()
}
