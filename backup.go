package main

// 数据备份与恢复（桌面端与 bot 服务端共享）。
//
// 备份物是一个 zip：
//   data.db            —— SQLite 一致性快照（VACUUM INTO 生成的单文件，已合并 WAL）
//   MANIFEST.json      —— 备份格式与时间戳
//   config.json 等     —— 可选的旁路文件（由调用方通过 sidecarFiles 指定），
//                          换机器时连模型 Key / 登录凭据 / 2FA 密钥一起带走
//
// 恢复采用 ATTACH + 事务逐表拷贝（只拷两边都存在的同名列），不需要重启即可让聊天数据
// 即时生效；旁路文件落盘后需要重启才生效，返回值 NeedRestart 会标明。
// 恢复前自动生成一份「恢复前自动备份」，防止误操作。

import (
	"archive/zip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	backupFormatName    = "wechat-profile-backup"
	backupFormatVersion = 1
	backupDBEntry       = "data.db"
	backupManifestName  = "MANIFEST.json"
	backupCurrentDBVer  = 5 // 当前程序支持的最高 SQLite user_version
	backupMaxUnzipBytes = int64(512 << 20)
	backupMaxEntries    = 20
)

// 备份加密相关常量。
//
// 只对「旁路文件」（config.json / ilink_credentials.json / totp_secret.json 这几个
// 真正含密钥的文件）加密，data.db 与 MANIFEST.json 保持明文：
//   - zip 本身仍是标准格式，Windows 资源管理器、手机文件管理器都能直接打开查看
//   - 数据库明文才能被 sqlite3 等工具直接检视，也让「恢复前自动备份」链路不受影响
//   - 加密后旁路文件在 zip 内以 <原名>.enc 存放，恢复端自动识别并解密
const (
	backupEncSuffix     = ".enc"  // zip 内加密文件的后缀
	backupEncMagic      = "WPE1"  // 加密块魔数，用于识别格式与版本
	backupEncSaltLen    = 16      // PBKDF2 盐长度
	backupEncNonceLen   = 12      // GCM 标准 nonce 长度
	backupEncIterations = 120_000 // PBKDF2-SHA256 迭代次数
	backupEncKeyLen     = 32      // AES-256
	backupEncTagLen     = 16      // GCM 认证标签长度，用于最小长度校验
)

// backupTables 参与整库拷贝的业务表（外键顺序）。
// 删除顺序与插入顺序相反，sqlite_sequence 永远最后处理。
var backupTables = []string{
	"contacts",
	"contact_aliases",
	"messages",
	"profile_history",
	"merge_log",
}

// BackupLog 一次备份/恢复操作的记录（backup_log 表，append-only）
type BackupLog struct {
	ID        int64  `json:"id"`
	Action    string `json:"action"`   // export / import
	Source    string `json:"source"`   // web / wechat / desktop
	Filename  string `json:"filename"` // 备份文件名（导入时为上传的文件名）
	SizeBytes int64  `json:"sizeBytes"`
	Detail    string `json:"detail"` // 恢复摘要或失败原因
	Success   bool   `json:"success"`
	CreatedAt string `json:"createdAt"`
}

// LogBackupAction 追加一条备份/恢复日志。只记录不返回错误：
// 日志写失败不应影响备份主流程，调用方也不用判错。
func LogBackupAction(db *sql.DB, action, source, filename string, sizeBytes int64, detail string, success bool) {
	if action != "export" && action != "import" {
		return
	}
	succ := 0
	if success {
		succ = 1
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(
		`INSERT INTO backup_log (action, source, filename, size_bytes, detail, success) VALUES (?,?,?,?,?,?)`,
		action, source, filename, sizeBytes, detail, succ); err != nil {
		slog.Warn("写备份日志失败", "err", err)
	}
}

// ListBackupLogs 取最近的备份/恢复记录（新的在前）
func ListBackupLogs(db *sql.DB, limit int) ([]BackupLog, error) {
	if limit <= 0 {
		limit = 50
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(
		`SELECT id, action, source, filename, size_bytes, detail, success, created_at
		 FROM backup_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupLog{}
	for rows.Next() {
		var l BackupLog
		var succ int
		if err := rows.Scan(&l.ID, &l.Action, &l.Source, &l.Filename,
			&l.SizeBytes, &l.Detail, &succ, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.Success = succ != 0
		out = append(out, l)
	}
	return out, rows.Err()
}

// BackupManifest zip 内的清单
type BackupManifest struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"formatVersion"`
	CreatedAt     string `json:"createdAt"`
	DBUserVersion int    `json:"dbUserVersion"`
}

// BackupSummary 恢复结果（也作为 HTTP 接口的 JSON 响应体）
type BackupSummary struct {
	Contacts     int      `json:"contacts"`
	Aliases      int      `json:"aliases"`
	Messages     int      `json:"messages"`
	Histories    int      `json:"histories"`
	MergeLogs    int      `json:"mergeLogs"`
	Files        []string `json:"files"`
	NeedRestart  bool     `json:"needRestart"`
	SafetyBackup string   `json:"safetyBackup,omitempty"`
}

// BackupFileName 标准备份文件名（本地时间）
func BackupFileName(t time.Time) string {
	return "wechat-profile-backup-" + t.Format("20060102-150405") + ".zip"
}

// quoteSQLString 转义 SQL 字符串字面量中的单引号
func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ---- 备份内密钥文件的口令加密（AES-256-GCM + PBKDF2-SHA256）----

// deriveBackupKey 由口令派生 AES 密钥（PBKDF2-HMAC-SHA256，RFC 8018）。
//
// 标准库的 crypto/pbkdf2 需要模块声明 go1.24，而桌面端 go.mod 是 go1.21；
// 提升语言版本会连带改变 1.22 起的循环变量语义，属于「影响现有行为」。
// 这里按公开标准自行实现，只有二十来行，且两端共用同一份代码，零第三方依赖。
func deriveBackupKey(password string, salt []byte) []byte {
	prf := hmac.New(sha256.New, []byte(password))
	size := prf.Size()
	blocks := (backupEncKeyLen + size - 1) / size

	dk := make([]byte, 0, blocks*size)
	t := make([]byte, size)
	u := make([]byte, size)
	idx := make([]byte, 4)
	for i := 1; i <= blocks; i++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(idx, uint32(i))
		prf.Write(idx)
		copy(t, prf.Sum(nil))
		copy(u, t)
		for j := 1; j < backupEncIterations; j++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for k := range t {
				t[k] ^= u[k]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:backupEncKeyLen]
}

// encryptBackupBlob 加密一段数据，输出格式：magic(4) || salt(16) || nonce(12) || 密文+GCM标签
func encryptBackupBlob(plain []byte, password string) ([]byte, error) {
	salt := make([]byte, backupEncSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("生成加密盐失败: %w", err)
	}
	block, err := aes.NewCipher(deriveBackupKey(password, salt))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成加密随机数失败: %w", err)
	}
	out := make([]byte, 0, len(backupEncMagic)+len(salt)+len(nonce)+len(plain)+gcm.Overhead())
	out = append(out, backupEncMagic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plain, nil), nil
}

// decryptBackupBlob 解密 encryptBackupBlob 的输出。口令不对时 GCM 校验失败，
// 返回的错误信息会直接展示给用户，因此不能暴露内部细节。
func decryptBackupBlob(blob []byte, password string) ([]byte, error) {
	head := len(backupEncMagic) + backupEncSaltLen + backupEncNonceLen
	if len(blob) < head+backupEncTagLen || string(blob[:len(backupEncMagic)]) != backupEncMagic {
		return nil, fmt.Errorf("加密数据格式不正确或已被破坏")
	}
	salt := blob[len(backupEncMagic) : len(backupEncMagic)+backupEncSaltLen]
	nonce := blob[len(backupEncMagic)+backupEncSaltLen : head]
	ct := blob[head:]
	block, err := aes.NewCipher(deriveBackupKey(password, salt))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("密码不正确，或加密数据已被破坏")
	}
	return plain, nil
}

// BuildBackupZip 生成备份 zip 到临时文件，返回文件路径与清理函数。
// 调用方负责在发送/另存完成后调用 cleanup 删除临时目录。
//
// sidecarDir 为旁路文件所在目录（通常就是数据库所在目录），
// sidecarFiles 为允许打包的白名单文件名，文件不存在则跳过。
//
// 不加密旁路文件；需要加密请用 BuildBackupZipWithPassword。
func BuildBackupZip(db *sql.DB, sidecarDir string, sidecarFiles []string) (string, func(), error) {
	return BuildBackupZipWithPassword(db, sidecarDir, sidecarFiles, "")
}

// BuildBackupZipWithPassword 同 BuildBackupZip，password 非空时把旁路文件加密后
// 以 <原名>.enc 放入 zip（data.db 与 MANIFEST.json 始终明文）。
func BuildBackupZipWithPassword(db *sql.DB, sidecarDir string, sidecarFiles []string, password string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "wp-backup-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	// 1. VACUUM INTO 生成一致性快照（含已提交的 WAL 内容），失败时整个备份中止
	snapPath := filepath.Join(tmpDir, backupDBEntry)
	if _, err := db.Exec("VACUUM INTO " + quoteSQLString(snapPath)); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("生成数据库快照失败: %w", err)
	}

	var dbVersion int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&dbVersion); err != nil {
		cleanup()
		return "", nil, err
	}

	// 2. 组装 zip
	zipPath := filepath.Join(tmpDir, "backup.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	zw := zip.NewWriter(zf)

	addFile := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	snapBytes, err := os.ReadFile(snapPath)
	if err != nil {
		zf.Close()
		cleanup()
		return "", nil, err
	}
	if err := addFile(backupDBEntry, snapBytes); err != nil {
		zf.Close()
		cleanup()
		return "", nil, err
	}

	for _, name := range sidecarFiles {
		// 只允许纯文件名，防止目录穿越
		if name != filepath.Base(name) || strings.Contains(name, "/") || strings.Contains(name, "\\") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sidecarDir, name))
		if err != nil {
			continue // 不存在就跳过
		}
		entryName := name
		if password != "" {
			enc, err := encryptBackupBlob(b, password)
			if err != nil {
				zf.Close()
				cleanup()
				return "", nil, fmt.Errorf("加密 %s 失败: %w", name, err)
			}
			b = enc
			entryName = name + backupEncSuffix
		}
		if err := addFile(entryName, b); err != nil {
			zf.Close()
			cleanup()
			return "", nil, err
		}
	}

	manifest := BackupManifest{
		Format:        backupFormatName,
		FormatVersion: backupFormatVersion,
		CreatedAt:     time.Now().Format(time.RFC3339),
		DBUserVersion: dbVersion,
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := addFile(backupManifestName, mb); err != nil {
		zf.Close()
		cleanup()
		return "", nil, err
	}
	if err := zw.Close(); err != nil {
		zf.Close()
		cleanup()
		return "", nil, err
	}
	if err := zf.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return zipPath, cleanup, nil
}

// extractBackupZip 校验并解压备份到临时目录，返回 data.db 路径、清单、找到的旁路文件表。
func extractBackupZip(zipPath string) (string, BackupManifest, map[string]string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", BackupManifest{}, nil, fmt.Errorf("不是有效的备份文件（无法打开 zip）: %w", err)
	}
	defer zr.Close()

	tmpDir, err := os.MkdirTemp("", "wp-restore-*")
	if err != nil {
		return "", BackupManifest{}, nil, err
	}
	fail := func(err error) (string, BackupManifest, map[string]string, error) {
		os.RemoveAll(tmpDir)
		return "", BackupManifest{}, nil, err
	}

	var manifest BackupManifest
	sidecars := map[string]string{}
	var total int64
	dbPath := ""

	if len(zr.File) > backupMaxEntries {
		return fail(fmt.Errorf("备份内文件数超过上限 %d", backupMaxEntries))
	}
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		// 拒绝目录、路径穿越、非普通文件
		if f.FileInfo().IsDir() || name != f.Name || name == "." || name == ".." || strings.ContainsAny(f.Name, `/\`) {
			return fail(fmt.Errorf("备份内存在非法路径: %s", f.Name))
		}
		total += int64(f.UncompressedSize64)
		if total > backupMaxUnzipBytes {
			return fail(fmt.Errorf("备份解压后体积超过上限 %dMB", backupMaxUnzipBytes>>20))
		}
		rc, err := f.Open()
		if err != nil {
			return fail(err)
		}
		out, err := os.OpenFile(filepath.Join(tmpDir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			rc.Close()
			return fail(err)
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return fail(err)
		}
		out.Close()
		rc.Close()

		switch name {
		case backupDBEntry:
			dbPath = filepath.Join(tmpDir, name)
		case backupManifestName:
			b, _ := os.ReadFile(filepath.Join(tmpDir, name))
			_ = json.Unmarshal(b, &manifest)
		default:
			sidecars[name] = filepath.Join(tmpDir, name)
		}
	}
	if dbPath == "" {
		return fail(fmt.Errorf("备份中缺少 %s，文件可能已损坏", backupDBEntry))
	}
	if manifest.Format != backupFormatName {
		return fail(fmt.Errorf("不是本程序的备份文件（清单标记为 %q）", manifest.Format))
	}
	return dbPath, manifest, sidecars, nil
}

// RestoreBackupZip 从备份 zip 恢复。
//
// sidecarDir 为旁路文件目标目录（通常就是数据库所在目录）；
// makeSafety=true 时恢复前自动在该目录生成一份「恢复前自动备份」zip。
//
// 不能恢复含加密旁路文件的备份；这类备份请用 RestoreBackupZipWithPassword。
func RestoreBackupZip(db *sql.DB, zipPath, sidecarDir string, makeSafety bool) (*BackupSummary, error) {
	return RestoreBackupZipWithPassword(db, zipPath, sidecarDir, makeSafety, "")
}

// RestoreBackupZipWithPassword 同 RestoreBackupZip，password 用于解密 zip 内的 .enc 旁路文件。
// 备份未加密时传空串即可；备份加密了却没传密码会明确报错，不会静默丢文件。
func RestoreBackupZipWithPassword(db *sql.DB, zipPath, sidecarDir string, makeSafety bool, password string) (*BackupSummary, error) {
	snapPath, _, sidecars, err := extractBackupZip(zipPath)
	if err != nil {
		return nil, err
	}
	tmpRoot := filepath.Dir(snapPath)
	defer os.RemoveAll(tmpRoot)

	// 0. 先把加密的旁路文件解密。放在动数据库之前：密码错误时整个恢复直接失败，
	//    不会出现「聊天数据已经换掉了、密钥文件却没恢复」的半吊子状态。
	sidecars, err = decryptSidecars(sidecars, password)
	if err != nil {
		return nil, err
	}

	// 1. 打开快照做完整性与版本校验
	snapDSN := snapPath + "?_pragma=busy_timeout(5000)"
	srcDB, err := sql.Open("sqlite", snapDSN)
	if err != nil {
		return nil, err
	}
	srcDB.SetMaxOpenConns(1)
	defer srcDB.Close()

	var integrity string
	if err := srcDB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return nil, fmt.Errorf("备份数据库完整性检查失败: %w", err)
	}
	if integrity != "ok" {
		return nil, fmt.Errorf("备份数据库已损坏（integrity_check=%s）", integrity)
	}
	var srcVer int
	if err := srcDB.QueryRow(`PRAGMA user_version`).Scan(&srcVer); err != nil {
		return nil, err
	}
	if srcVer < 1 || srcVer > backupCurrentDBVer {
		return nil, fmt.Errorf("备份的数据库版本 %d 不受支持（本程序支持 1～%d；版本过高可能来自更新的程序）", srcVer, backupCurrentDBVer)
	}
	var tableCount int
	if err := srcDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='contacts'`).Scan(&tableCount); err != nil {
		return nil, err
	}
	if tableCount == 0 {
		return nil, fmt.Errorf("备份数据库中没有 contacts 表，不是有效的数据备份")
	}
	if err := srcDB.Close(); err != nil {
		return nil, err
	}

	// 2. 恢复前自动备份（防误操作，长期保留）
	summary := &BackupSummary{Files: []string{}}
	if makeSafety {
		safetyPath, safetyCleanup, err := BuildBackupZip(db, sidecarDir, nil)
		if err != nil {
			return nil, fmt.Errorf("生成恢复前自动备份失败: %w", err)
		}
		dest := filepath.Join(sidecarDir, "auto-backup-pre-restore-"+time.Now().Format("20060102-150405")+".zip")
		if err := copyFile(safetyPath, dest); err != nil {
			safetyCleanup()
			return nil, err
		}
		safetyCleanup()
		summary.SafetyBackup = dest
	}

	// 3. ATTACH 快照，在单个连接 + 单事务内整库替换
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	dbMu.Lock()
	defer dbMu.Unlock()

	exec := func(q string) error {
		_, err := conn.ExecContext(ctx, q)
		return err
	}
	if err := exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return nil, err
	}
	if err := exec(`ATTACH DATABASE ` + quoteSQLString(snapPath) + ` AS bak`); err != nil {
		return nil, fmt.Errorf("挂载备份数据库失败: %w", err)
	}
	defer exec(`DETACH DATABASE bak`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 先清空目标（子表→父表）
	for i := len(backupTables) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+quoteIdent(backupTables[i])); err != nil {
			return nil, fmt.Errorf("清空 %s 失败: %w", backupTables[i], err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM main.sqlite_sequence`); err != nil {
		// 全新库可能还没有 sqlite_sequence，报错可忽略
		if !strings.Contains(err.Error(), "no such table") {
			return nil, err
		}
	}

	// 逐表按同名列拷贝（父表→子表）
	counts := map[string]int{}
	for _, t := range backupTables {
		cols, err := commonColumns(ctx, tx, t)
		if err != nil {
			return nil, err
		}
		if len(cols) == 0 {
			continue
		}
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = quoteIdent(c)
		}
		q := fmt.Sprintf(`INSERT INTO main.%s (%s) SELECT %s FROM bak.%s`,
			quoteIdent(t), strings.Join(quoted, ","), strings.Join(quoted, ","), quoteIdent(t))
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("写入 %s 失败: %w", t, err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.`+quoteIdent(t)).Scan(&n); err != nil {
			return nil, err
		}
		counts[t] = n
	}

	// 自增序列一起搬，保证恢复后新行的 id 不与历史 id 冲突
	if seqCols, _ := commonColumns(ctx, tx, "sqlite_sequence"); len(seqCols) == 2 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO main.sqlite_sequence(name,seq) SELECT name,seq FROM bak.sqlite_sequence`); err != nil {
			if !strings.Contains(err.Error(), "no such table") {
				return nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := exec(`DETACH DATABASE bak`); err != nil {
		return nil, err
	}
	if err := exec(`PRAGMA foreign_keys=ON`); err != nil {
		return nil, err
	}

	summary.Contacts = counts["contacts"]
	summary.Aliases = counts["contact_aliases"]
	summary.Messages = counts["messages"]
	summary.Histories = counts["profile_history"]
	summary.MergeLogs = counts["merge_log"]

	// 4. 旁路文件落盘（旧文件先改名 .bak-时间戳），这些文件重启后才生效
	allowed := map[string]bool{
		"config.json":            true,
		"ilink_credentials.json": true,
		"totp_secret.json":       true,
	}
	names := make([]string, 0, len(sidecars))
	for n := range sidecars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !allowed[name] {
			continue
		}
		dest := filepath.Join(sidecarDir, name)
		if _, err := os.Stat(dest); err == nil {
			bak := dest + ".bak-" + time.Now().Format("20060102-150405")
			if err := os.Rename(dest, bak); err != nil {
				return nil, fmt.Errorf("备份旧 %s 失败: %w", name, err)
			}
		}
		if err := copyFile(sidecars[name], dest); err != nil {
			return nil, fmt.Errorf("写入 %s 失败: %w", name, err)
		}
		os.Chmod(dest, 0600)
		summary.Files = append(summary.Files, name)
	}
	summary.NeedRestart = len(summary.Files) > 0
	return summary, nil
}

// decryptSidecars 把解压出来的旁路文件统一转成明文视图：
// 以 .enc 结尾的用 password 解密后写到同目录的 .dec 文件，其余原样返回。
// 返回的 map 以真实文件名（已去掉 .enc 后缀）为键。
func decryptSidecars(sidecars map[string]string, password string) (map[string]string, error) {
	out := make(map[string]string, len(sidecars))
	for name, path := range sidecars {
		if !strings.HasSuffix(name, backupEncSuffix) {
			out[name] = path
			continue
		}
		realName := strings.TrimSuffix(name, backupEncSuffix)
		if realName == "" || realName == name {
			continue // 只有 ".enc" 这种畸形名字，直接忽略
		}
		if password == "" {
			return nil, fmt.Errorf("备份中的 %s 已加密，请在恢复时填写导出时设置的密码", realName)
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", name, err)
		}
		plain, err := decryptBackupBlob(blob, password)
		if err != nil {
			return nil, fmt.Errorf("解密 %s 失败：%w", realName, err)
		}
		decPath := path + ".dec"
		if err := os.WriteFile(decPath, plain, 0600); err != nil {
			return nil, err
		}
		out[realName] = decPath
	}
	return out, nil
}

// BackupNeedsPassword 判断备份 zip 内是否有加密的旁路文件（恢复时必须提供密码）。
// 供前端在提交前提示用户，避免恢复了一半才报密码错误。
func BackupNeedsPassword(zipPath string) bool {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return false
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, backupEncSuffix) {
			return true
		}
	}
	return false
}

// quoteIdent 包裹 SQL 标识符（列名/表名来自 pragma，正常只含字母下划线，这里再兜底）
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// commonColumns 返回 main 与 bak 两边都存在的列名（按 main 的列顺序）。
// tx 同时能看到 main（目标库）和 bak（ATTACH 的快照库）。
func commonColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT m.name FROM pragma_table_info(?, 'main') m
		 WHERE EXISTS (SELECT 1 FROM pragma_table_info(?, 'bak') b WHERE b.name = m.name)
		 ORDER BY m.cid`, table, table)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 列信息失败: %w", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
