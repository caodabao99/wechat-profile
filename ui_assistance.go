package main

import (
	"github.com/atotto/clipboard"
	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
	"strings"
)

func rewriteControls(id int64, text **walk.TextEdit) dl.Widget {
	var style *walk.ComboBox
	var button *walk.PushButton
	return dl.Composite{Layout: dl.HBox{MarginsZero: true}, Children: []dl.Widget{
		dl.ComboBox{AssignTo: &style, Model: rewriteStyles, CurrentIndex: 1},
		dl.PushButton{AssignTo: &button, Text: "换个说法", OnClicked: func() {
			original := (*text).Text()
			chosen := style.Text()
			button.SetEnabled(false)
			style.SetEnabled(false)
			go func() {
				out, err := doRewriteReply(id, original, chosen)
				mainWindow.Synchronize(func() {
					if button.IsDisposed() {
						return
					}
					button.SetEnabled(true)
					style.SetEnabled(true)
					if err != nil {
						showError("改写失败：" + err.Error())
						return
					}
					if !(*text).IsDisposed() && (*text).Text() == original {
						(*text).SetText(out)
					}
				})
			}()
		}},
	}}
}

func showDraftDialog(id int64) {
	if id <= 0 {
		return
	}
	var dlg *walk.Dialog
	var input, output *walk.TextEdit
	var check *walk.PushButton
	err := (dl.Dialog{AssignTo: &dlg, Title: "回复前帮我看看", Size: dl.Size{Width: 560, Height: 520}, Layout: dl.VBox{}, Children: []dl.Widget{
		dl.Label{Text: "粘贴准备发送的话（最多4000字）。结合画像和最近上下文检查，仅展示和复制。"},
		dl.TextEdit{AssignTo: &input, VScroll: true, MinSize: dl.Size{Height: 130}},
		dl.PushButton{AssignTo: &check, Text: "帮我看看", OnClicked: func() {
			original := input.Text()
			check.SetEnabled(false)
			input.SetReadOnly(true)
			go func() {
				out, err := doReviewDraft(id, original)
				mainWindow.Synchronize(func() {
					if dlg.IsDisposed() {
						return
					}
					check.SetEnabled(true)
					input.SetReadOnly(false)
					if err != nil {
						showError("检查失败：" + err.Error())
						return
					}
					if input.Text() == original {
						output.SetText(strings.ReplaceAll(formatDraftReview(out), "\n", "\r\n"))
					}
				})
			}()
		}},
		dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 180}},
		dl.PushButton{Text: "复制检查结果", OnClicked: func() { _ = clipboard.WriteAll(output.Text()) }},
	}}).Create(mainWindow)
	if err != nil {
		showError(err.Error())
		return
	}
	dlg.Run()
}

func showChangesDialog(id int64) {
	if id <= 0 {
		return
	}
	var dlg *walk.Dialog
	var output *walk.TextEdit
	var refresh *walk.PushButton
	load := func() {
		refresh.SetEnabled(false)
		go func() {
			out, err := fetchProfileChanges(id)
			mainWindow.Synchronize(func() {
				if dlg.IsDisposed() {
					return
				}
				refresh.SetEnabled(true)
				if err != nil {
					output.SetText("读取失败：" + err.Error())
					return
				}
				output.SetText(strings.ReplaceAll(formatProfileChanges(out), "\n", "\r\n"))
			})
		}()
	}
	err := (dl.Dialog{AssignTo: &dlg, Title: "画像变化 · 新增 / 修改 / 删除", Size: dl.Size{Width: 600, Height: 500}, Layout: dl.VBox{}, Children: []dl.Widget{
		dl.Label{Text: "当前画像与上一历史画像的结构化比较，不调用模型。"},
		dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true},
		dl.PushButton{AssignTo: &refresh, Text: "刷新变化", OnClicked: load},
	}}).Create(mainWindow)
	if err != nil {
		showError(err.Error())
		return
	}
	load()
	dlg.Run()
}
