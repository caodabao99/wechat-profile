package main

// 备份/恢复的本地/远程适配（与其他 do* 函数同一层）。

import (
	"fmt"
	"os"
	"path/filepath"
)

// desktopSidecarFiles 桌面备份随带的旁路文件：只带 config.json
// （ilink 凭据、totp_secret 是 bot 服务端独有的）。
var desktopSidecarFiles = []string{"config.json"}

// doExportBackup 导出备份到用户选择的本地文件路径（自动适配本地/远程模式）。
// password 非空时把 zip 内的密钥文件（桌面端是 config.json）加密，聊天数据仍为明文。
func doExportBackup(destPath, password string) error {
	if isRemoteMode {
		data, _, err := remoteClient.DownloadBackup(password)
		if err != nil {
			return err
		}
		// 远程模式：服务端在 hBackupExport 里已记过日志，这里不重复
		return os.WriteFile(destPath, data, 0600)
	}
	dir := filepath.Dir(dbPath())
	zipPath, cleanup, err := BuildBackupZipWithPassword(db, dir, desktopSidecarFiles, password)
	if err != nil {
		LogBackupAction(db, "export", "desktop-local", "", 0, err.Error(), false)
		return err
	}
	defer cleanup()
	if err := copyFile(zipPath, destPath); err != nil {
		LogBackupAction(db, "export", "desktop-local", filepath.Base(destPath), 0, err.Error(), false)
		return err
	}
	detail := ""
	if password != "" {
		detail = "密钥文件已加密"
	}
	if fi, err := os.Stat(destPath); err == nil {
		LogBackupAction(db, "export", "desktop-local", filepath.Base(destPath), fi.Size(), detail, true)
	}
	return nil
}

// doImportBackup 从本地备份文件恢复（自动适配本地/远程模式）。
// password 用于解密备份内被加密的密钥文件；备份未加密时传空串。
// 本地模式：数据即时生效，config.json 需重启程序生效。
// 远程模式：恢复发生在服务端，服务端配置/凭据需重启服务生效。
func doImportBackup(srcPath, password string) (*BackupSummary, error) {
	if isRemoteMode {
		// 远程模式：服务端在 hBackupImport 里记日志
		return remoteClient.UploadBackup(srcPath, password)
	}
	fi, _ := os.Stat(srcPath)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	summary, err := RestoreBackupZipWithPassword(db, srcPath, filepath.Dir(dbPath()), true, password)
	if err != nil {
		LogBackupAction(db, "import", "desktop-local", filepath.Base(srcPath), size, err.Error(), false)
		return nil, err
	}
	detail := ""
	if summary != nil {
		detail = fmt.Sprintf("联系人 %d，消息 %d，画像历史 %d",
			summary.Contacts, summary.Messages, summary.Histories)
		if password != "" {
			detail += "（密钥文件已解密）"
		}
	}
	LogBackupAction(db, "import", "desktop-local", filepath.Base(srcPath), size, detail, true)
	return summary, nil
}
