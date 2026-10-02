package main

// 备份密码输入对话框。
//
// walk 没有内置的单行输入弹窗，这里按 ui_remark.go 的写法自建一个：
// 一个密码框（默认打码）+「显示密码」勾选 + 确定/取消。

import (
	"strings"

	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// promptBackupPassword 弹出备份密码输入框。
//
// hint 为顶部说明文字；okText 为确定按钮文字。
// 返回 (密码, 是否点了确定)。用户取消时返回 ("", false)；
// 确定但留空时返回 ("", true)——留空即不加密。
func promptBackupPassword(title, hint, okText string) (string, bool) {
	var dlg *walk.Dialog
	var pwdLE *walk.LineEdit
	var showCB *walk.CheckBox
	accepted := false
	var password string

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    title,
		MinSize:  dl.Size{Width: 420, Height: 240},
		Layout:   dl.VBox{Spacing: 8},
		Children: []dl.Widget{
			dl.TextEdit{Text: hint, ReadOnly: true, MinSize: dl.Size{Height: 96}, Font: fontHint},
			dl.LineEdit{AssignTo: &pwdLE, PasswordMode: true, CueBanner: "输入备份密码（留空=不加密）"},
			dl.CheckBox{
				AssignTo: &showCB,
				Text:     "显示密码",
				OnCheckedChanged: func() {
					pwdLE.SetPasswordMode(!showCB.Checked())
				},
			},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						Text:    okText,
						MinSize: dl.Size{Width: 80, Height: 32},
						OnClicked: func() {
							password = strings.TrimSpace(pwdLE.Text())
							accepted = true
							dlg.Accept()
						},
					},
					dl.PushButton{
						Text:    "取消",
						MinSize: dl.Size{Width: 60, Height: 32},
						OnClicked: func() {
							dlg.Cancel()
						},
					},
				},
			},
		},
	}).Create(mainWindow); err != nil {
		showError("创建密码输入框失败: " + err.Error())
		return "", false
	}

	setTopMost(dlg.Handle())
	dlg.Run()
	if !accepted {
		return "", false
	}
	return password, true
}
