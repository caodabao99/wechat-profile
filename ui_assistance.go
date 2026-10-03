package main

import (
	"strings"

	"github.com/atotto/clipboard"
	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// rewriteControls 生成「改成（先选风格再点换个说法）：」+「○稳妥得体 ○简洁直接 ○亲切热情 ○委婉留余地 [换个说法]」两行控件。
// 四个 RadioButton 在同一容器内自动互斥；默认一个都不选，必须先点风格再改写。
func rewriteControls(id int64, text **walk.TextEdit) dl.Widget {
	radios := make([]*walk.RadioButton, len(replyStyles))
	var button *walk.PushButton

	radiosDL := make([]dl.Widget, 0, len(replyStyles)+1)
	for i := range replyStyles {
		radiosDL = append(radiosDL, dl.RadioButton{
			AssignTo: &radios[i],
			Text:     replyStyles[i],
		})
	}
	radiosDL = append(radiosDL, dl.PushButton{AssignTo: &button, Text: "换个说法", MinSize: dl.Size{Width: 90, Height: 28}, OnClicked: func() {
		chosen := ""
		for i, r := range radios {
			if r != nil && r.Checked() {
				chosen = replyStyles[i]
				break
			}
		}
		if chosen == "" {
			showError("请先点选一种回复风格（稳妥得体 / 简洁直接 / 亲切热情 / 委婉留余地）")
			return
		}
		original := (*text).Text()
		button.SetEnabled(false)
		for _, r := range radios {
			r.SetEnabled(false)
		}
		go func() {
			out, err := doRewriteReply(id, original, chosen)
			mainWindow.Synchronize(func() {
				if button.IsDisposed() {
					return
				}
				button.SetEnabled(true)
				for _, r := range radios {
					if !r.IsDisposed() {
						r.SetEnabled(true)
					}
				}
				if err != nil {
					showError("改写失败：" + err.Error())
					return
				}
				if !(*text).IsDisposed() && (*text).Text() == original {
					(*text).SetText(out)
				}
			})
		}()
	}})

	return dl.Composite{
		Layout: dl.VBox{MarginsZero: true, Spacing: 4},
		Children: []dl.Widget{
			dl.Label{Text: "改成（先选风格，再点换个说法）:"},
			dl.Composite{Layout: dl.HBox{MarginsZero: true, Spacing: 6}, Children: radiosDL},
		},
	}
}

func showDraftDialog(id int64) {
	if id <= 0 {
		return
	}
	var dlg *walk.Dialog
	var input, output *walk.TextEdit
	var check *walk.PushButton
	err := (dl.Dialog{AssignTo: &dlg, Title: "回复前帮我看看",
		MinSize: dl.Size{Width: 480, Height: 400}, Size: dl.Size{Width: 680, Height: 620},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: "粘贴准备发送的话（最多4000字）。结合画像和最近上下文检查，仅展示和复制。"},
			dl.TextEdit{AssignTo: &input, VScroll: true, MinSize: dl.Size{Height: 140}},
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
			dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 260}},
			dl.PushButton{Text: "复制检查结果", OnClicked: func() { _ = clipboard.WriteAll(output.Text()) }},
		}}).Create(mainWindow)
	if err != nil {
		showError(err.Error())
		return
	}
	setTopMost(dlg.Handle())
	makeDialogResizable(dlg)
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
	err := (dl.Dialog{AssignTo: &dlg, Title: "画像变化 · 新增 / 修改 / 删除",
		MinSize: dl.Size{Width: 480, Height: 360}, Size: dl.Size{Width: 720, Height: 680},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: "当前画像与上一历史画像的结构化比较，不调用模型。"},
			dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 420}},
			dl.PushButton{AssignTo: &refresh, Text: "刷新变化", OnClicked: load},
		}}).Create(mainWindow)
	if err != nil {
		showError(err.Error())
		return
	}
	setTopMost(dlg.Handle())
	makeDialogResizable(dlg)
	load()
	dlg.Run()
}
