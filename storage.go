package main

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/go-sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// dbMu 串行化所有数据库操作（配合 WAL，避免 database is locked）
var dbMu sync.Mutex

// ProfileHistory 画像变更历史
type ProfileHistory struct {
	ID            int64
	ContactID     int64
	ProfileJSON   string
	ChangeSummary string
	CreatedAt     string
}

// ContactStats 联系人统计信息
type ContactStats struct {
	Total     int64
	Mine      int64
	Other     int64
	FirstTime string
	LastTime  string
}

// InitDB 打开（不存在则创建）SQLite 数据库并建表
func InitDB(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接 + WAL，手机/桌面混用场景最稳
	db.SetMaxOpenConns(1)

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS contacts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			remark TEXT,
			profile_json TEXT DEFAULT '{}',
			profile_summary TEXT DEFAULT '',
			other_msg_count INTEGER DEFAULT 0,
			last_updated DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			sender TEXT NOT NULL CHECK(sender IN ('me', 'other')),
			content TEXT NOT NULL,
			msg_hash TEXT NOT NULL,
			msg_time DATETIME,
			captured_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(contact_id, msg_hash),
			FOREIGN KEY (contact_id) REFERENCES contacts(id)
		)`,
		`CREATE TABLE IF NOT EXISTS profile_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			profile_json TEXT NOT NULL,
			change_summary TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (contact_id) REFERENCES contacts(id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate 执行数据库版本迁移（幂等）
func migrate(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 1 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. contacts 增加 merged_into 列
	var colCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('contacts') WHERE name='merged_into'`).
		Scan(&colCount); err != nil {
		return err
	}
	if colCount == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE contacts ADD COLUMN merged_into INTEGER DEFAULT NULL`); err != nil {
			return err
		}
	}

	// 2. 别名表
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS contact_aliases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL REFERENCES contacts(id),
		alias TEXT NOT NULL UNIQUE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_aliases_contact ON contact_aliases(contact_id)`); err != nil {
		return err
	}

	// 3. 合并日志表
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS merge_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL,
		target_id INTEGER NOT NULL,
		source_name TEXT NOT NULL,
		target_name TEXT NOT NULL,
		moved_message_ids TEXT DEFAULT '[]',
		moved_history_ids TEXT DEFAULT '[]',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		undone_at DATETIME DEFAULT NULL
	)`); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	// PRAGMA user_version 必须在事务外执行
	_, err = db.Exec(`PRAGMA user_version = 1`)
	return err
}

// messageHash 计算消息去重哈希：发送方 + 时间 + 正文
func messageHash(m Message) string {
	ts := m.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	sum := md5.Sum([]byte(m.Sender + ":" + ts.Format(time.RFC3339) + ":" + m.Content))
	return hex.EncodeToString(sum[:])
}

// SaveMessages 保存一批消息（INSERT OR IGNORE 去重）。
// 返回实际新增条数；新增的对方消息同时累加 contacts.other_msg_count。
func SaveMessages(db *sql.DB, contactID int64, messages []Message) (int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	newCount := 0
	for _, m := range messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		ts := m.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}

		res, err := db.Exec(
			`INSERT OR IGNORE INTO messages (contact_id, sender, content, msg_hash, msg_time)
			 VALUES (?, ?, ?, ?, ?)`,
			contactID, m.Sender, content, messageHash(m), ts.Format(time.RFC3339))
		if err != nil {
			return newCount, err
		}
		affected, _ := res.RowsAffected()
		if affected > 0 {
			newCount++
			if m.Sender == "other" {
				if _, err := db.Exec(
					`UPDATE contacts SET other_msg_count = other_msg_count + 1 WHERE id = ?`,
					contactID); err != nil {
					return newCount, err
				}
			}
		}
	}
	return newCount, nil
}

// scanMessages 从 rows 扫描消息并按时间正序返回（传入的查询需按 id DESC）
func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var reversed []Message
	for rows.Next() {
		var sender, content, msgTime string
		if err := rows.Scan(&sender, &content, &msgTime); err != nil {
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339, msgTime)
		if err != nil {
			ts = time.Now()
		}
		reversed = append(reversed, Message{
			Sender:    sender,
			Content:   content,
			Timestamp: ts,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转为时间正序
	out := make([]Message, len(reversed))
	for i, m := range reversed {
		out[len(reversed)-1-i] = m
	}
	return out, nil
}

// GetRecentMessages 取最近 limit 条消息，返回时按时间正序排列
func GetRecentMessages(db *sql.DB, contactID int64, limit int) ([]Message, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(
		`SELECT sender, content, msg_time FROM messages
		 WHERE contact_id = ? ORDER BY id DESC LIMIT ?`,
		contactID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// GetMessagesPage 分页取消息（offset 从 0 开始，按时间倒序），用于画像窗口翻页
func GetMessagesPage(db *sql.DB, contactID int64, offset, limit int) ([]Message, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(
		`SELECT sender, content, msg_time FROM messages
		 WHERE contact_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		contactID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var sender, content, msgTime string
		if err := rows.Scan(&sender, &content, &msgTime); err != nil {
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339, msgTime)
		if err != nil {
			ts = time.Now()
		}
		out = append(out, Message{Sender: sender, Content: content, Timestamp: ts})
	}
	return out, rows.Err()
}

// GetContactByID 按 id 查询联系人
func GetContactByID(db *sql.DB, id int64) (*Contact, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	var c Contact
	var remark, profileJSON, profileSummary, lastUpdated sql.NullString
	err := db.QueryRow(
		`SELECT id, name, remark, profile_json, profile_summary,
		        other_msg_count, last_updated, created_at
		 FROM contacts WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &remark, &profileJSON, &profileSummary,
			&c.OtherMsgCount, &lastUpdated, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	c.Remark = remark.String
	c.ProfileJSON = profileJSON.String
	c.ProfileSummary = profileSummary.String
	c.LastUpdated = lastUpdated.String
	return &c, nil
}

// GetAllContacts 返回联系人列表，按最近更新时间倒序。
// includeMerged=true 时包含已合并的联系人。
func GetAllContacts(db *sql.DB, includeMerged bool) ([]Contact, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	query := `SELECT id, name, COALESCE(remark,''), COALESCE(profile_json,'{}'),
		        COALESCE(profile_summary,''), other_msg_count,
		        COALESCE(last_updated,''), created_at
		 FROM contacts`
	if !includeMerged {
		query += ` WHERE merged_into IS NULL`
	}
	query += ` ORDER BY last_updated DESC, id DESC`

	rows, err := db.Query(query)
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

// GetProfileHistory 取画像变更历史（最新在前）
func GetProfileHistory(db *sql.DB, contactID int64, limit int) ([]ProfileHistory, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(
		`SELECT id, contact_id, profile_json, COALESCE(change_summary,''), created_at
		 FROM profile_history WHERE contact_id = ?
		 ORDER BY id DESC LIMIT ?`, contactID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProfileHistory
	for rows.Next() {
		var h ProfileHistory
		if err := rows.Scan(&h.ID, &h.ContactID, &h.ProfileJSON,
			&h.ChangeSummary, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SaveProfile 持久化最新画像并写入一条历史记录
func SaveProfile(db *sql.DB, contactID int64, profileJSON, summary, changeSummary string) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	if _, err := db.Exec(
		`UPDATE contacts
		 SET profile_json = ?, profile_summary = ?, last_updated = CURRENT_TIMESTAMP
		 WHERE id = ?`, profileJSON, summary, contactID); err != nil {
		return err
	}
	if _, err := db.Exec(
		`INSERT INTO profile_history (contact_id, profile_json, change_summary)
		 VALUES (?, ?, ?)`, contactID, profileJSON, changeSummary); err != nil {
		return err
	}
	return nil
}

// GetContactStats 返回统计页需要的计数与时间跨度
func GetContactStats(db *sql.DB, contactID int64) (ContactStats, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	var s ContactStats
	var first, last sql.NullString
	err := db.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN sender='me' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN sender='other' THEN 1 ELSE 0 END), 0),
		        COALESCE(MIN(msg_time), ''), COALESCE(MAX(msg_time), '')
		 FROM messages WHERE contact_id = ?`, contactID).
		Scan(&s.Total, &s.Mine, &s.Other, &first, &last)
	if err != nil {
		return s, err
	}
	s.FirstTime = first.String
	s.LastTime = last.String
	return s, nil
}
