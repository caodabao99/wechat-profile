package main

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/go-sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// dbMu 串行化所有数据库操作（配合 WAL，避免 database is locked）
var dbMu sync.Mutex

// profileEpoch is guarded by dbMu; it is never restored from a backup.
var profileEpoch uint64

func currentProfileEpoch() uint64 {
	dbMu.Lock()
	defer dbMu.Unlock()
	return profileEpoch
}

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
	// WAL 下 synchronous=NORMAL 既安全又显著降低写盘放大；temp_store=MEMORY、
	// cache_size(-8000≈8MB) 属保守调优，不会在低内存机器上造成问题。
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-8000)"
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

	if version < 1 {
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
		if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
			return err
		}
	}

	if version < 2 {
		// v2: contacts 增加 profile_msg_count（上次生成画像时的对方消息数），
		// 用于「距离上次画像新增 N 条」的更新判断，替代原来的整除判断
		var colCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('contacts') WHERE name='profile_msg_count'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(
				`ALTER TABLE contacts ADD COLUMN profile_msg_count INTEGER DEFAULT 0`); err != nil {
				return err
			}
			// 已有画像的联系人：以当前消息数为基线，避免升级后立即触发一次多余更新
			if _, err := db.Exec(
				`UPDATE contacts SET profile_msg_count = other_msg_count
				 WHERE profile_json IS NOT NULL AND profile_json != '' AND profile_json != '{}'`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
			return err
		}
	}

	if version < 3 {
		// v3: 一次性自清洗。测试期曾用 v3 迁移把 last_updated 错误 +8 小时，
		// 导致出现"未来时间"（如 22:45 比当前还晚）。所有 last_updated 超过当前
		// 时间的值都是这种脏数据，统一重置为当前时间。
		// 正常写入的时间永远不会是未来时间，所以这个条件只命中脏数据。
		//
		// 必须挂在版本号下：早先它无条件跑在每次 InitDB 里，库变大后每次启动
		// 都要全表扫描一次 contacts，且迁移语义被冲淡。
		now := time.Now().Format(time.RFC3339)
		if _, err := db.Exec(`UPDATE contacts SET last_updated = ? WHERE last_updated > ?`, now, now); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
			return err
		}
	}

	if version < 4 {
		// v4: merge_log 增加 profile_copied，记录本次合并是否把 source 的画像
		// 拷贝给了 target。撤销合并时必须据此把 target 的画像清空，
		// 否则 target 会永久留着一份基于「source+target 合并消息集」生成的画像，
		// 而它的消息集在撤销后已经变回去了。
		var colCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('merge_log') WHERE name='profile_copied'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(
				`ALTER TABLE merge_log ADD COLUMN profile_copied INTEGER DEFAULT 0`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
			return err
		}
	}

	if version < 5 {
		// v5: 备份/恢复操作日志。只追加不修改，恢复备份时本表不参与整体替换，
		// 保证「这台机器上发生过什么备份操作」的历史不会因恢复旧备份而丢失。
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS backup_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		action TEXT NOT NULL CHECK(action IN ('export', 'import')),
		source TEXT NOT NULL DEFAULT '',
		filename TEXT DEFAULT '',
		size_bytes INTEGER DEFAULT 0,
		detail TEXT DEFAULT '',
		success INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
			return err
		}
	}
	if version < 6 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, stmt := range []string{
			`ALTER TABLE merge_log ADD COLUMN deleted_messages TEXT DEFAULT '[]'`,
			`ALTER TABLE merge_log ADD COLUMN target_profile TEXT DEFAULT ''`,
			`PRAGMA user_version = 6`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 7 {
		// v7: 数据库性能升级（与服务端 wechat-profile-bot v8 对齐）。为 messages 新增
		// msg_unix（Unix 秒）并建立覆盖“按联系人 + 时间/分页”的复合索引；旧数据分批回填。
		// 桌面本地库无归档表，故只处理 messages。
		// 幂等：列存在则跳过 ALTER；回填只补 msg_unix IS NULL 的行；任一步失败都在
		// PRAGMA user_version=7 之前返回，下次启动整体重跑。
		var colCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='msg_unix'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN msg_unix INTEGER`); err != nil {
				return err
			}
		}
		if err := backfillMsgUnix(db); err != nil {
			return err
		}
		for _, idx := range []string{
			`CREATE INDEX IF NOT EXISTS idx_messages_contact_id ON messages(contact_id, id DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_contact_unix ON messages(contact_id, msg_unix DESC, id DESC)`,
		} {
			if _, err := db.Exec(idx); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
			return err
		}
	}
	return nil
}

// backfillMsgUnix 分批把 msg_time 换算成 Unix 秒写入 msg_unix（仅补 NULL 行）。
// 按 id 窗口分批自动提交，避免百万级数据落入一个超长事务；首尾打印进度日志。
func backfillMsgUnix(db *sql.DB) error {
	var minID, maxID sql.NullInt64
	if err := db.QueryRow(`SELECT MIN(id), MAX(id) FROM messages`).Scan(&minID, &maxID); err != nil {
		return err
	}
	if !maxID.Valid {
		return nil // 空表
	}
	const batch = int64(20000)
	start, end := minID.Int64, maxID.Int64
	var total int64
	slog.Info("msg_unix 回填开始", "table", "messages", "minID", start, "maxID", end)
	for lo := start; lo <= end; lo += batch {
		hi := lo + batch - 1
		res, err := db.Exec(
			`UPDATE messages SET msg_unix = CAST(strftime('%s', msg_time) AS INTEGER)
			 WHERE id >= ? AND id <= ? AND msg_unix IS NULL
			   AND msg_time IS NOT NULL AND msg_time != ''`, lo, hi)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			total += n
		}
	}
	slog.Info("msg_unix 回填完成", "table", "messages", "updated", total)
	return nil
}

// messageHash 计算消息去重哈希：发送方 + 时间 + 正文。
//
// ts 必须由调用方传入（即最终写入 msg_time 的那个值）。早先这里在 ts 为零值时
// 自己调 time.Now()，与 SaveMessages 里的 time.Now() 是两个不同时刻，导致
// hash 每次都不一样、UNIQUE(contact_id, msg_hash) 去重彻底失效，且 hash 里的
// 时间与库里的 msg_time 对不上。
func messageHash(m Message, ts time.Time) string {
	sum := md5.Sum([]byte(m.Sender + ":" + ts.Format(time.RFC3339) + ":" + m.Content))
	return hex.EncodeToString(sum[:])
}

// parseMsgTime 解析库中存储的 msg_time（统一为 RFC3339）。
// 解析失败时回落当前时间，但会打印告警：静默改写时间会让数据错乱无从排查。
func parseMsgTime(raw string) time.Time {
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	slog.Warn("msg_time 不是合法的 RFC3339，已回落为当前时间", "raw", raw)
	return time.Now()
}

// SaveMessages 保存一批消息（INSERT OR IGNORE 去重）。
// 返回实际新增条数；新增的对方消息同时累加 contacts.other_msg_count。
func SaveMessages(db *sql.DB, contactID int64, messages []Message) (int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	// 整批一个事务提交（过去每条一次隐式提交，粘贴一大片时写盘放大严重）；
	// other_msg_count 累加后末尾写一次。整批原子：中途出错一律回滚，不留“半批已入库”。
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	newCount := 0
	newOther := 0
	for _, m := range messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		ts := m.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}

		res, err := tx.Exec(
			`INSERT OR IGNORE INTO messages (contact_id, sender, content, msg_hash, msg_time, msg_unix)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			contactID, m.Sender, content, messageHash(m, ts), ts.Format(time.RFC3339), ts.Unix())
		if err != nil {
			return 0, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			newCount++
			if m.Sender == "other" {
				newOther++
			}
		}
	}
	if newOther > 0 {
		if _, err := tx.Exec(
			`UPDATE contacts SET other_msg_count = other_msg_count + ? WHERE id = ?`,
			newOther, contactID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newCount, nil
}

// scanMessages 从 rows 扫描消息并按时间正序（ASC）返回。
//
// 契约：传入的查询**必须**按 id DESC 排序，本函数负责把它反转成正序。
// 所有调用方都要遵守这一点，否则净结果会变成倒序（GetAllMessages 曾经就是这样，
// 用 ASC 查询再被这里反转，等于把最新在前的对话喂给了 LLM）。
func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var reversed []Message
	for rows.Next() {
		var sender, content, msgTime string
		if err := rows.Scan(&sender, &content, &msgTime); err != nil {
			return nil, err
		}
		reversed = append(reversed, Message{
			Sender:          sender,
			Content:         content,
			Timestamp:       parseMsgTime(msgTime),
			profileEpoch:    profileEpoch,
			profileSnapshot: true,
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

// GetAllMessages 取该联系人全部消息（按时间正序 ASC），供画像生成 / 合并后重生成使用。
//
// 注意查询必须是 id DESC：scanMessages 会无条件反转，ASC 查询 + 反转 = 倒序，
// 这正是修复前的 bug（LLM 拿到的是最新在前的对话，画像质量受损，且与桌面端
// 远程模式 doGetAllMessages 的正序结果方向相反）。
func GetAllMessages(db *sql.DB, contactID int64) ([]Message, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(
		`SELECT sender, content, msg_time FROM messages
		 WHERE contact_id = ? ORDER BY id DESC`,
		contactID)
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
	out := []Message{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
	for rows.Next() {
		var sender, content, msgTime string
		if err := rows.Scan(&sender, &content, &msgTime); err != nil {
			return nil, err
		}
		out = append(out, Message{Sender: sender, Content: content, Timestamp: parseMsgTime(msgTime)})
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

	query := `SELECT c.id, c.name, COALESCE(c.remark,''), COALESCE(c.profile_json,'{}'),
		        COALESCE(c.profile_summary,''), c.other_msg_count,
		        COALESCE(c.last_updated,''), c.created_at,
		        COALESCE(c.merged_into, 0),
		        (SELECT COUNT(*) FROM merge_log ml WHERE ml.target_id = c.id AND ml.undone_at IS NULL)
		 FROM contacts c`
	if !includeMerged {
		query += ` WHERE c.merged_into IS NULL`
	}
	// 用 strftime('%s', ...) 转成 epoch 再排序：last_updated 是 RFC3339 字符串，
	// 直接按字典序比较只在所有记录时区偏移相同时才成立。一旦混入 +08:00 与 Z
	// （例如库在容器里以 UTC 写过一段），字典序就会给出错误的先后关系。
	// 无法解析/NULL 的值 strftime 返回 NULL，在 DESC 下排最后，与原 '' 行为一致。
	query += ` ORDER BY strftime('%s', c.last_updated) DESC, c.id DESC`

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Contact{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.Name, &c.Remark, &c.ProfileJSON,
			&c.ProfileSummary, &c.OtherMsgCount, &c.LastUpdated, &c.CreatedAt,
			&c.MergedInto, &c.MergeCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetContactsPage 分页查询联系人，返回当前页数据与符合条件的总数。
// q 非空时按昵称/备注/别名模糊匹配（LIKE 忽略 ASCII 大小写，中文天然精确）。
// GetAllContacts 保持全量语义不动（桌面端远程模式在用），网页端改用本函数避免全量渲染。
func GetContactsPage(db *sql.DB, includeMerged bool, q string, offset, limit int) ([]Contact, int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200 // 硬上限，避免一个请求把整库拉走
	}
	if offset < 0 {
		offset = 0
	}

	where := ""
	var args []interface{}
	if !includeMerged {
		where = ` WHERE c.merged_into IS NULL`
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + q + "%"
		cond := `(c.name LIKE ? OR COALESCE(c.remark,'') LIKE ?
			OR EXISTS (SELECT 1 FROM contact_aliases ca WHERE ca.contact_id = c.id AND ca.alias LIKE ?))`
		if where == "" {
			where = ` WHERE ` + cond
		} else {
			where += ` AND ` + cond
		}
		args = append(args, like, like, like)
	}

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts c`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT c.id, c.name, COALESCE(c.remark,''), COALESCE(c.profile_json,'{}'),
		        COALESCE(c.profile_summary,''), c.other_msg_count,
		        COALESCE(c.last_updated,''), c.created_at,
		        COALESCE(c.merged_into, 0),
		        (SELECT COUNT(*) FROM merge_log ml WHERE ml.target_id = c.id AND ml.undone_at IS NULL)
		 FROM contacts c` + where +
		// 排序依据同 GetAllContacts：strftime 转 epoch 再比，避免混入不同时区偏移时字典序出错
		` ORDER BY strftime('%s', c.last_updated) DESC, c.id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Contact{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.Name, &c.Remark, &c.ProfileJSON,
			&c.ProfileSummary, &c.OtherMsgCount, &c.LastUpdated, &c.CreatedAt,
			&c.MergedInto, &c.MergeCount); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
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

	out := []ProfileHistory{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
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
	return saveProfileAtEpoch(db, contactID, profileJSON, summary, changeSummary, currentProfileEpoch())
}

func saveProfileAtEpoch(db *sql.DB, contactID int64, profileJSON, summary, changeSummary string, epoch uint64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if epoch != profileEpoch {
		return ErrProfileStale
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var merged sql.NullInt64
	if err := tx.QueryRow(`SELECT merged_into FROM contacts WHERE id = ?`, contactID).Scan(&merged); err != nil {
		return err
	}
	if merged.Valid {
		return ErrProfileStale
	}

	// 统一用本地时间（不用 SQLite CURRENT_TIMESTAMP，避免存成 UTC 慢 8 小时）
	// 同时记录本次生成画像时的对方消息数，作为下次周期更新的基线
	now := time.Now().Format(time.RFC3339)
	if _, err := tx.Exec(`UPDATE contacts
		 SET profile_json = ?, profile_summary = ?, last_updated = ?,
		     profile_msg_count = other_msg_count
		 WHERE id = ?`, profileJSON, summary, now, contactID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO profile_history (contact_id, profile_json, change_summary, created_at)
		 VALUES (?, ?, ?, ?)`, contactID, profileJSON, changeSummary, now); err != nil {
		return err
	}
	return tx.Commit()
}

// saveProfileHistory 只写一条历史记录（不改 contacts 当前画像），用于记录生成失败等事件
func saveProfileHistory(db *sql.DB, contactID int64, profileJSON, changeSummary string) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if strings.TrimSpace(profileJSON) == "" {
		profileJSON = "{}"
	}
	now := time.Now().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO profile_history (contact_id, profile_json, change_summary, created_at)
		 VALUES (?, ?, ?, ?)`, contactID, profileJSON, changeSummary, now); err != nil {
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
	// 首/末消息时间不能用 MIN/MAX(msg_time)：msg_time 是 RFC3339 字符串，
	// MIN/MAX 走的是字典序，只在所有记录时区偏移一致时才等价于时间序。
	// 改为按 strftime('%s', msg_time) 排序取端点，返回的仍是原始字符串。
	err := db.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN sender='me' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN sender='other' THEN 1 ELSE 0 END), 0),
		        COALESCE((SELECT msg_time FROM messages
		                  WHERE contact_id = ? AND msg_time IS NOT NULL AND msg_time != ''
		                  ORDER BY strftime('%s', msg_time) ASC, id ASC LIMIT 1), ''),
		        COALESCE((SELECT msg_time FROM messages
		                  WHERE contact_id = ? AND msg_time IS NOT NULL AND msg_time != ''
		                  ORDER BY strftime('%s', msg_time) DESC, id DESC LIMIT 1), '')
		 FROM messages WHERE contact_id = ?`,
		contactID, contactID, contactID, contactID).
		Scan(&s.Total, &s.Mine, &s.Other, &first, &last)
	if err != nil {
		return s, err
	}
	s.FirstTime = first.String
	s.LastTime = last.String
	return s, nil
}
