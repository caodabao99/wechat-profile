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
	// 不叫 statusLabel：那是主窗口状态栏的包级变量，遮蔽之后很容易改错地方
	var hintLabel *walk.Label
	var submitBtn, cancelBtn *walk.PushButton

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
			dl.Label{AssignTo: &hintLabel, Text: "", Font: fontHint},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						AssignTo: &submitBtn,
						Text:     "提交",
						MinSize:  dl.Size{Width: 80, Height: 32},
						OnClicked: func() {
							note := strings.TrimSpace(noteTE.Text())
							if note == "" {
								showError("请先输入要补充的信息")
								return
							}
							// 一次补充要跑一轮 LLM（最长约两分钟），期间必须锁住按钮：
							// 否则连点几下就会并发改同一条画像，历史记录里全是重复版本。
							submitBtn.SetEnabled(false)
							cancelBtn.SetEnabled(false)
							hintLabel.SetText("正在合并到画像…")
							go func() {
								err := doSupplement(contact.ID, note)
								mainWindow.Synchronize(func() {
									// 请求还在飞的时候用户可能已经关掉了对话框，
									// 再操作已销毁的控件会崩
									if dlg.IsDisposed() {
										return
									}
									if err != nil {
										submitBtn.SetEnabled(true)
										cancelBtn.SetEnabled(true)
										hintLabel.SetText("")
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
						AssignTo:  &cancelBtn,
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
