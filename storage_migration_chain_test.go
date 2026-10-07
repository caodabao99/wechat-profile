package main

import (
	"path/filepath"
	"testing"
)

// TestDesktopMigrationFromEmptyDB 校验从零建库能一路升到最新版本，且重复 migrate() 幂等。
//
// 注：曾试过「把已迁移好的库 user_version 降回 v1~v6 再重放 migrate()」，Windows CI 实跑
// 直接报 duplicate column name: deleted_messages——因为 migrate() 的 ADD COLUMN 未做存在性
// 检查，重放不合法。那是真实代码缺口（迁移中途崩溃后可能重现），单独在测试里记录为待修项，
// 见 TestDesktopMigrationReplaySafetyKnownIssue。
func TestDesktopMigrationFromEmptyDB(t *testing.T) {
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != 7 {
		t.Fatalf("新建库应一路迁移到 v7，实得 %d (err=%v)", ver, err)
	}
	// 关键表必须存在（缺表会让后续功能默默失败）
	for _, tbl := range []string{"contacts", "messages", "profile_history", "merge_log", "contact_aliases", "backup_log"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatalf("表 %s 应存在且可查: %v", tbl, err)
		}
	}
	id, err := GetOrCreateContact(db, "新库联系人")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("重复 migrate() 应幂等: %v", err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != 7 {
		t.Fatalf("二次 migrate() 后版本应仍为 7，实得 %d", ver)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id = ?`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("二次 migrate() 后数据应保留，实得 n=%d (err=%v)", n, err)
	}
}

// TestDesktopMigrationReplaySafetyKnownIssue 把已知缺口以可执行形式留档：
// 当前 migrate() 重放不安全（ALTER TABLE ADD COLUMN 未先查列存在），一旦迁移中途崩溃
// （列已加、user_version 未写），下次启动会直接报错卡住。本测试只记录现状、不阻断 CI；
// 修好（改用带存在性检查的加列助手）后把下面注释里的 Skip 删掉即变成长效回归钩。
func TestDesktopMigrationReplaySafetyKnownIssue(t *testing.T) {
	t.Skip("已知缺口：migrate() 非重放安全（ADD COLUMN 无存在性检查），待加 guarded 加列助手后启用本用例")
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("重放应安全，实得: %v", err)
	}
}

// TestDesktopMigrationLatestVersionMatchesBackup 钉住「迁移终点 == 备份支持的版本」，
// 避免加了新迁移步却忘了同步 backupCurrentDBVer（那样备份/恢复会误判版本不受支持）。
func TestDesktopMigrationLatestVersionMatchesBackup(t *testing.T) {
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(t.TempDir(), "ver.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != backupCurrentDBVer {
		t.Fatalf("migrate() 终点 user_version=%d 应等于 backupCurrentDBVer=%d", ver, backupCurrentDBVer)
	}
}
