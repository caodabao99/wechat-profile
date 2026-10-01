package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// MergeOptions 合并选项
type MergeOptions struct {
	UseSourceNameAsDisplay bool // 显示名改用新昵称（源昵称）
	RegenerateProfile      bool // 合并完成后异步重生成画像
}

// MergeResult 合并结果
type MergeResult struct {
	MovedMessages int
	MovedHistory  int
	MergeLogID    int64
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
	if sourceName == config.MyName || sourceName == "我" || sourceName == "Me" || sourceName == "me" {
		return result, fmt.Errorf("不能合并自己")
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
	var targetProfileJSON string
	err = tx.QueryRow(`SELECT COALESCE(profile_json, '') FROM contacts WHERE id = ?`, targetID).
		Scan(&targetProfileJSON)
	if err != nil {
		return result, err
	}
	if targetProfileJSON == "" || targetProfileJSON == "{}" {
		var sourceProfileJSON, sourceProfileSummary string
		err = tx.QueryRow(`SELECT COALESCE(profile_json, ''), COALESCE(profile_summary, '') FROM contacts WHERE id = ?`, sourceID).
			Scan(&sourceProfileJSON, &sourceProfileSummary)
		if err != nil {
			return result, err
		}
		if sourceProfileJSON != "" && sourceProfileJSON != "{}" {
			if _, err := tx.Exec(`UPDATE contacts SET profile_json = ?, profile_summary = ?, last_updated = CURRENT_TIMESTAMP WHERE id = ?`,
				sourceProfileJSON, sourceProfileSummary, targetID); err != nil {
				return result, err
			}
		}
	}

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

	// 8. 登记别名（source 旧名和 target 当前名）
	// 注意：如果用了 UseSourceNameAsDisplay，target 当前名已变成 sourceName，要记录的是 target 原来的名字
	aliasSourceName := sourceName
	aliasTargetName := targetName
	if opts.UseSourceNameAsDisplay {
		// 此时 target 的 name 已变成 sourceName，aliasTargetName 应该是 target 原来的名字
		// 但 targetName 变量存的就是原来的名字，所以是对的
	}

	for _, alias := range []string{aliasSourceName, aliasTargetName} {
		if alias == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO contact_aliases (contact_id, alias) VALUES (?, ?)
			ON CONFLICT(alias) DO UPDATE SET contact_id = excluded.contact_id`,
			targetID, alias); err != nil {
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
	res, err = tx.Exec(`INSERT INTO merge_log (source_id, target_id, source_name, target_name, moved_message_ids, moved_history_ids)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sourceID, targetID, sourceName, targetName, string(msgIDsJSON), string(historyIDsJSON))
	if err != nil {
		return result, err
	}
	result.MergeLogID, _ = res.LastInsertId()

	if err := tx.Commit(); err != nil {
		return result, err
	}
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
		 ORDER BY last_updated DESC, id DESC`, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Contact
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
		UndoneAt        sql.NullString
	}
	err := db.QueryRow(`SELECT source_id, target_id, source_name, target_name, moved_message_ids, moved_history_ids, undone_at
		FROM merge_log WHERE id = ?`, mergeLogID).
		Scan(&log.SourceID, &log.TargetID, &log.SourceName, &log.TargetName,
			&log.MovedMessageIDs, &log.MovedHistoryIDs, &log.UndoneAt)
	if err != nil {
		return fmt.Errorf("合并日志不存在: %w", err)
	}
	if log.UndoneAt.Valid {
		return fmt.Errorf("该合并已撤销")
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

	// 3. 搬回历史
	for _, histID := range historyIDs {
		if _, err := tx.Exec(`UPDATE profile_history SET contact_id = ? WHERE id = ?`, log.SourceID, histID); err != nil {
			return err
		}
	}

	// 4. 恢复 source 的名字（如果之前被改过）
	if _, err := tx.Exec(`UPDATE contacts SET name = ? WHERE id = ?`, log.SourceName, log.SourceID); err != nil {
		return err
	}

	// 5. 重算双方计数
	for _, cid := range []int64{log.SourceID, log.TargetID} {
		if _, err := tx.Exec(`UPDATE contacts SET other_msg_count = (
			SELECT COUNT(*) FROM messages WHERE contact_id = ? AND sender = 'other'
		) WHERE id = ?`, cid, cid); err != nil {
			return err
		}
	}

	// 6. 标记撤销时间
	if _, err := tx.Exec(`UPDATE merge_log SET undone_at = CURRENT_TIMESTAMP WHERE id = ?`, mergeLogID); err != nil {
		return err
	}

	return tx.Commit()
}

// GetAliases 获取联系人的已确认别名列表
func GetAliases(db *sql.DB, contactID int64) ([]string, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(`SELECT alias FROM contact_aliases WHERE contact_id = ? ORDER BY created_at`, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
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

	var out []MergeLogEntry
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
	rows, err := db.Query(`SELECT alias FROM contact_aliases WHERE contact_id = ? ORDER BY created_at`, id)
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
func UpdateContactName(db *sql.DB, contactID int64, newName string) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("名称不能为空")
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

	_, err = db.Exec(`UPDATE contacts SET name = ? WHERE id = ?`, newName, contactID)
	return err
}
