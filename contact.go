package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
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
	MergeCount     int      // 作为目标吸收过多少个联系人（未撤销的合并数）
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
	if name == "" {
		return 0, fmt.Errorf("联系人名称不能为空")
	}

	var id int64
	err := db.QueryRow(`SELECT id FROM contacts WHERE name = ?`, name).Scan(&id)
	if err == nil {
		// 这个名字可能挂在一条已被合并掉的记录上。直接往里写消息，
		// 这些消息会因为 merged_into 非空而在联系人列表和画像里全部隐身，
		// 必须跟着合并链走到仍然存活的那个联系人。
		return followMergedLocked(db, id)
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	res, err := db.Exec(`INSERT INTO contacts (name, created_at) VALUES (?, ?)`, name,
		time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// followMergedLocked 沿 merged_into 链找到仍然存活的联系人 id（调用方必须已持有 dbMu）。
// 正常数据不会成环（MergeContacts 会校验），但为了防御脏数据仍设一个跳数上限，
// 免得陷入死循环把进程挂住。
func followMergedLocked(db *sql.DB, id int64) (int64, error) {
	for i := 0; i < 32; i++ {
		var next sql.NullInt64
		if err := db.QueryRow(`SELECT merged_into FROM contacts WHERE id = ?`, id).Scan(&next); err != nil {
			return 0, err
		}
		if !next.Valid || next.Int64 == 0 {
			return id, nil
		}
		id = next.Int64
	}
	return 0, fmt.Errorf("合并链过长（停在联系人 %d），数据可能已损坏", id)
}

// UpdateContactRemark 更新联系人备注。
// 备注有两个用途：① 列表显示「备注（昵称）」② 管理命令可用昵称或备注定位（见 FindContactID）。
// 但备注不参与聊天记录归属识别——粘贴的记录里只有微信昵称，归属仍走 name/alias。
func UpdateContactRemark(db *sql.DB, contactID int64, remark string) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`UPDATE contacts SET remark = ? WHERE id = ?`,
		strings.TrimSpace(remark), contactID)
	return err
}

// DeleteContactByID 删除联系人及其消息、画像历史、别名，
// 并删除 merge_log 中涉及该联系人的记录（source_id/target_id 是 NOT NULL，不能置空）
func DeleteContactByID(db *sql.DB, contactID int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM messages WHERE contact_id = ?`, contactID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM profile_history WHERE contact_id = ?`, contactID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM contact_aliases WHERE contact_id = ?`, contactID); err != nil {
		return err
	}
	// 清理 merge_log：source_id/target_id 建表时是 NOT NULL，置空会触发约束错误导致整个事务失败，
	// 因此直接删除涉及该联系人的合并记录（联系人已删除，其合并历史也没有保留意义）
	if _, err := tx.Exec(`DELETE FROM merge_log WHERE source_id = ? OR target_id = ?`,
		contactID, contactID); err != nil {
		return err
	}
	// 解除其他联系人对该联系人的合并引用
	if _, err := tx.Exec(`UPDATE contacts SET merged_into = NULL WHERE merged_into = ?`, contactID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM contacts WHERE id = ?`, contactID); err != nil {
		return err
	}
	return tx.Commit()
}

// displayName 列表/标题显示用：有备注时显示「备注（昵称）」
func displayName(c *Contact) string {
	if strings.TrimSpace(c.Remark) != "" {
		return c.Remark + "（" + c.Name + "）"
	}
	return c.Name
}

// findContactIDLocked 查找联系人 ID（调用方必须已持有 dbMu）：
// 1. contacts.name 精确匹配（未合并）；2. contact_aliases.alias 匹配（viaAlias=true）
// 未命中返回 sql.ErrNoRows，绝不新建。
func findContactIDLocked(db *sql.DB, name string) (int64, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false, sql.ErrNoRows
	}

	var id int64
	err := db.QueryRow(
		`SELECT id FROM contacts WHERE name = ? AND merged_into IS NULL`, name).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}

	err = db.QueryRow(
		`SELECT contact_id FROM contact_aliases WHERE alias = ?`, name).Scan(&id)
	if err == nil {
		// 别名指向的联系人自己也可能后来被合并掉了，同样要跟到存活的那个
		live, ferr := followMergedLocked(db, id)
		if ferr != nil {
			return 0, false, ferr
		}
		return live, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	return 0, false, sql.ErrNoRows
}

// findManageContactIDLocked 管理/查询类命令的联系人解析（调用方必须已持有 dbMu）：
// 1. contacts.name 精确匹配（未合并）
// 2. contacts.remark 精确匹配（未合并）——设过备注后，命令里用昵称或备注都行（或的关系）
// 3. contact_aliases.alias 匹配（历史昵称）
// 未命中返回 sql.ErrNoRows，绝不新建。
//
// 注意：备注匹配只在管理命令路径生效，不进 ResolveContactID——
// 聊天记录归属识别只能用微信昵称，否则备注会把同名的另一个人的消息错误归并。
func findManageContactIDLocked(db *sql.DB, name string) (int64, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false, sql.ErrNoRows
	}

	var id int64
	err := db.QueryRow(
		`SELECT id FROM contacts WHERE name = ? AND merged_into IS NULL`, name).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}

	// 备注精确匹配（只命中存活联系人）。
	// 加 remark != '' 防止空备注被意外匹配（虽然参数本身非空，属于双保险）
	err = db.QueryRow(
		`SELECT id FROM contacts WHERE remark = ? AND remark != '' AND merged_into IS NULL`, name).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}

	// 历史别名
	err = db.QueryRow(
		`SELECT contact_id FROM contact_aliases WHERE alias = ?`, name).Scan(&id)
	if err == nil {
		// 别名指向的联系人自己也可能后来被合并掉了，同样要跟到存活的那个
		live, ferr := followMergedLocked(db, id)
		if ferr != nil {
			return 0, false, ferr
		}
		return live, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	return 0, false, sql.ErrNoRows
}

// FindContactID 只查不建地解析联系人，未找到返回 sql.ErrNoRows。
// 查询/管理类命令（画像、备注、补充、统计、删除、合并、撤销合并）必须用它：
// 昵称、备注、历史别名任一精确命中即可；用 ResolveContactID 的话，
// 用户输错昵称会静默创建一个空联系人再对它操作。
func FindContactID(db *sql.DB, name string) (int64, bool, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return findManageContactIDLocked(db, name)
}

// ResolveContactID 三级精确解析联系人：
// 1. contacts.name 精确匹配（未合并）→ 直接返回
// 2. contact_aliases.alias 匹配 → 返回其 contact_id（viaAlias=true）
// 3. 都未命中 → 新建（不做模糊匹配）
// 仅用于导入聊天记录场景，查询/管理命令请用 FindContactID
func ResolveContactID(db *sql.DB, name string) (int64, bool, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	name = strings.TrimSpace(name)
	id, viaAlias, err := findContactIDLocked(db, name)
	if err == nil {
		return id, viaAlias, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	if name == "" {
		return 0, false, sql.ErrNoRows
	}

	// 3. 新建
	res, err := db.Exec(`INSERT INTO contacts (name, created_at) VALUES (?, ?)`, name,
		time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, false, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return newID, false, nil
}
