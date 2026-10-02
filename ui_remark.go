package main

import (
	"fmt"
	"strings"

	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// showRemarkDialog 弹出备注编辑对话框
func showRemarkDialog(contact Contact, onDone func()) {
	var dlg *walk.Dialog
	var remarkLE *walk.LineEdit

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    "编辑备注",
		MinSize:  dl.Size{Width: 360, Height: 180},
		Layout:   dl.VBox{Spacing: 8},
		Children: []dl.Widget{
			dl.Label{
				Text: fmt.Sprintf("微信昵称：%s", contact.Name),
				Font: fontSection,
			},
			dl.Label{
				Text: "备注仅用于你个人区分，不影响聊天记录识别匹配。",
				Font: fontHint,
			},
			dl.LineEdit{
				AssignTo:  &remarkLE,
				Text:      contact.Remark,
				CueBanner: "输入备注（留空=取消备注）",
			},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						Text:    "保存",
						MinSize: dl.Size{Width: 80, Height: 32},
						OnClicked: func() {
							// 必须 TrimSpace：输入框里只打了几个空格会被当成有效备注存下来，
							// 列表里显示成「（昵称）」这种看不出所以然的样子
							remark := strings.TrimSpace(remarkLE.Text())
							if err := doSetRemark(contact.ID, remark); err != nil {
								showError("保存备注失败: " + err.Error())
								return
							}
							dlg.Accept()
							if onDone != nil {
								onDone()
							}
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
		showError("创建备注对话框失败: " + err.Error())
		return
	}
	setTopMost(dlg.Handle())
	dlg.Run()
}
