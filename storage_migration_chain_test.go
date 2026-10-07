package main

import (
	"path/filepath"
	"strconv"
	"testing"
)

// TestDesktopMigrationChainFromEveryStep 校验 v1→v7 **整条**迁移链。
//
// 为什么需要：此前只有 phase1_perf_test.go 测了 v6→v7 这一步，老库（v1~v5）升级路径
// 从未被执行过——而桌面端马上要正式投入使用，用户手里的库可能停在任意历史版本。
// 做法：把一个已建到最新版本(v7)的库依次「降版本」到 1..6，再各跑一次 migrate()：
// 每一步都必须收敛回 7、可重复执行（幂等）、且已有联系人数据不丢。
//
// 注：桌面端依赖 Windows-only 的 lxn/walk，本测试仅在 GOOS=windows 下编译/执行，
// 由仓库内的 Windows CI 工作流负责真正跑起来。
func TestDesktopMigrationChainFromEveryStep(t *testing.T) {
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, err := GetOrCreateContact(db, "迁移链联系人")
	if err != nil {
		t.Fatal(err)
	}

	for step := 1; step <= 6; step++ {
		if _, err := db.Exec(`PRAGMA user_version = ` + strconv.Itoa(step)); err != nil {
			t.Fatalf("设置 v%d 失败: %v", step, err)
		}
		if err := migrate(db); err != nil {
			t.Fatalf("从 v%d 迁移失败（老库升上来会踩同一个坑）: %v", step, err)
		}
		var ver int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != 7 {
			t.Fatalf("从 v%d 迁移后 user_version 应为 7，实得 %d (err=%v)", step, ver, err)
		}
		// 幂等：紧接着再迁移一次，必须仍然成功且版本不变
		if err := migrate(db); err != nil {
			t.Fatalf("从 v%d 二次迁移应幂等: %v", step, err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id = ?`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("从 v%d 迁移后联系人数据应保留，实得 n=%d (err=%v)", step, n, err)
		}
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
