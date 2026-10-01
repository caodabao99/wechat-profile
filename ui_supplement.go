package main

import (
	"fmt"
	"strings"

	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// showSupplementDialog 弹出手动补充画像信息的对话框
func showSupplementDialog(contact Contact, onDone func()) {
	var dlg *walk.Dialog
	var noteTE *walk.TextEdit
	var statusLabel *walk.Label

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    "补充画像信息",
		MinSize:  dl.Size{Width: 420, Height: 320},
		Layout:   dl.VBox{Spacing: 6},
		Children: []dl.Widget{
			dl.Label{
				Text: fmt.Sprintf("给「%s」手动补充信息", displayName(&contact)),
				Font: fontSection,
			},
			dl.Label{
				Text: "比如：生日 5月20日、性别男、星座金牛、手机号 138xxx、职业设计师…",
				Font: fontHint,
			},
			dl.TextEdit{
				AssignTo: &noteTE,
				VScroll:  true,
				Font:     fontBody,
				MinSize:  dl.Size{Width: 380, Height: 140},
			},
			dl.Label{AssignTo: &statusLabel, Text: "", Font: fontHint},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						Text:    "提交",
						MinSize: dl.Size{Width: 80, Height: 32},
						OnClicked: func() {
							note := strings.TrimSpace(noteTE.Text())
							if note == "" {
								showError("请先输入要补充的信息")
								return
							}
							statusLabel.SetText("正在合并到画像…")
							go func() {
								err := SupplementProfile(db, llmClient, contact.ID, contact.Name, note)
								mainWindow.Synchronize(func() {
									if err != nil {
										statusLabel.SetText("")
										showError("补充画像失败: " + err.Error())
										return
									}
									dlg.Accept()
									if onDone != nil {
										onDone()
									}
								})
							}()
						},
					},
					dl.PushButton{
						Text:      "取消",
						MinSize:   dl.Size{Width: 60, Height: 32},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}).Create(mainWindow); err != nil {
		showError("创建补充画像对话框失败: " + err.Error())
		return
	}
	setTopMost(dlg.Handle())
	dlg.Run()
}
