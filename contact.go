package main

import (
	"database/sql"
	"strings"
)

// Contact 对应 contacts 表的一行
type Contact struct {
	ID             int64
	Name           string
	Remark         string
	ProfileJSON    string
	ProfileSummary string
	OtherMsgCount  int
	LastUpdated    string
	CreatedAt      string
	MergedInto     int64    // 已合并到的目标联系人 id，0 表示未合并
	Aliases        []string // 已确认的历史昵称列表（按需加载）
}

// InferContactName 从解析出的消息中推断联系人（对方）昵称：
// 统计所有非我发送且带昵称的消息，返回出现次数最多的；
// 次数相同取最先出现的，保证结果稳定。
func InferContactName(messages []Message, myName string) string {
	counts := map[string]int{}
	var order []string
	for _, m := range messages {
		if m.Sender != "me" {
			name := strings.TrimSpace(m.SenderName)
			if name == "" {
				continue
			}
			if _, seen := counts[name]; !seen {
				order = append(order, name)
			}
			counts[name]++
		}
	}

	best := ""
	bestCount := 0
	for _, name := range order {
		if counts[name] > bestCount {
			best = name
			bestCount = counts[name]
		}
	}
	return best
}

// GetOrCreateContact 按昵称查询联系人，不存在则插入，返回其 id。
func GetOrCreateContact(db *sql.DB, name string) (int64, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	name = strings.TrimSpace(name)
	var id int64
	err := db.QueryRow(`SELECT id FROM contacts WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	res, err := db.Exec(`INSERT INTO contacts (name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateContactRemark 更新联系人备注（仅用于显示区分，识别匹配仍用微信昵称）
func UpdateContactRemark(db *sql.DB, contactID int64, remark string) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`UPDATE contacts SET remark = ? WHERE id = ?`,
		strings.TrimSpace(remark), contactID)
	return err
}

// displayName 列表/标题显示用：有备注时显示「备注（昵称）」
func displayName(c *Contact) string {
	if strings.TrimSpace(c.Remark) != "" {
		return c.Remark + "（" + c.Name + "）"
	}
	return c.Name
}

// ResolveContactID 三级精确解析联系人：
// 1. contacts.name 精确匹配（未合并）→ 直接返回
// 2. contact_aliases.alias 匹配 → 返回其 contact_id（viaAlias=true）
// 3. 都未命中 → GetOrCreateContact 新建（不做模糊匹配）
func ResolveContactID(db *sql.DB, name string) (int64, bool, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false, sql.ErrNoRows
	}

	// 1. 精确匹配未合并的联系人
	var id int64
	err := db.QueryRow(
		`SELECT id FROM contacts WHERE name = ? AND merged_into IS NULL`, name).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}

	// 2. 别名匹配
	err = db.QueryRow(
		`SELECT contact_id FROM contact_aliases WHERE alias = ?`, name).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}

	// 3. 新建
	res, err := db.Exec(`INSERT INTO contacts (name) VALUES (?)`, name)
	if err != nil {
		return 0, false, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return newID, false, nil
}
