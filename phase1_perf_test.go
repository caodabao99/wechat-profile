package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestDesktopPhase1MsgUnixMigration 校验桌面本地库 v6→v7：
// SaveMessages 写 msg_unix；老库降级后 migrate 自动回填 + 建复合索引 + 版本到 7；重复迁移幂等。
// 说明：桌面端依赖 Windows-only 的 lxn/walk，本测试仅在 GOOS=windows 下编译/执行。
func TestDesktopPhase1MsgUnixMigration(t *testing.T) {
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, err := GetOrCreateContact(db, "桌面老数据")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2025, 4, 9, 16, 30, 0, 0, time.Local)
	if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "带时间消息", Timestamp: ts}}); err != nil {
		t.Fatal(err)
	}
	// 写入路径应立即填 msg_unix。
	var got sql.NullInt64
	if err := db.QueryRow(`SELECT msg_unix FROM messages WHERE content='带时间消息'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != ts.Unix() {
		t.Fatalf("msg_unix 应=%d, got %+v", ts.Unix(), got)
	}

	// 模拟未迁移的老库：清空 msg_unix、删索引、降版本。
	if _, err := db.Exec(`UPDATE messages SET msg_unix = NULL`); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []string{"idx_messages_contact_id", "idx_messages_contact_unix"} {
		if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != 7 {
		t.Fatalf("user_version 应为 7, got %d err=%v", ver, err)
	}
	var idxCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_messages_contact_id','idx_messages_contact_unix')`).Scan(&idxCount); err != nil || idxCount != 2 {
		t.Fatalf("复合索引应建立 2 个, got %d err=%v", idxCount, err)
	}
	var bad int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE msg_unix IS NULL AND msg_time IS NOT NULL AND msg_time != ''`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("应全部回填, 剩余 %d err=%v", bad, err)
	}
	// 幂等重跑。
	if err := migrate(db); err != nil {
		t.Fatalf("重复迁移不应报错: %v", err)
	}
}
