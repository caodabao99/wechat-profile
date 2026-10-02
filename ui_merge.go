package main

import (
	"fmt"
	"strings"

	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// mergeTargetModel 合并对话框目标联系人列表模型
// 注意：walk 的 ListBox 模型方法只能定义在包级类型上，局部类型无法定义方法会导致反射建型失败
type mergeTargetModel struct {
	walk.ListModelBase
	items []Contact
}

func (m *mergeTargetModel) ItemCount() int { return len(m.items) }

func (m *mergeTargetModel) Value(index int) interface{} {
	if index < 0 || index >= len(m.items) {
		return nil
	}
	return m.items[index].Name
}

// showMergeDialog 显示合并联系人对话框
// sourceID: 要合并掉的联系人（新昵称）
// sourceName: 源联系人昵称
// onDone: 合并完成后的回调（刷新列表等）
func showMergeDialog(sourceID int64, sourceName string, onDone func()) {
	var dlg *walk.Dialog
	var targetLE *walk.LineEdit
	var targetLB *walk.ListBox
	var previewLabel *walk.Label
	var keepTargetRB, useSourceRB *walk.RadioButton
	var regenChk *walk.CheckBox
	var confirmBtn *walk.PushButton

	candidates, err := doGetMergeCandidates(sourceID)
	if err != nil {
		showError("读取候选联系人失败: " + err.Error())
		return
	}
	if len(candidates) == 0 {
		showError("没有可合并的目标联系人")
		return
	}

	// 过滤后的候选列表
	var filtered []Contact
	filtered = candidates

	tm := &mergeTargetModel{items: filtered}

	// 预览里已经查过统计，确认框直接复用这两个值，不再重复查一遍
	var sourceTotal, targetTotal int64

	// 更新预览
	updatePreview := func() {
		idx := targetLB.CurrentIndex()
		if idx < 0 || idx >= len(tm.items) {
			previewLabel.SetText("请选择目标联系人")
			confirmBtn.SetEnabled(false)
			return
		}
		target := tm.items[idx]

		sourceStats, serr := fetchStats(sourceID)
		targetStats, terr := fetchStats(target.ID)
		if serr != nil || terr != nil {
			// 统计取不到就不能让用户在「合并后共 0 条」的假数字上点确认
			err := serr
			if err == nil {
				err = terr
			}
			previewLabel.SetText("读取消息统计失败: " + err.Error())
			confirmBtn.SetEnabled(false)
			return
		}
		sourceTotal, targetTotal = sourceStats.Total, targetStats.Total

		previewLabel.SetText(fmt.Sprintf(
			"合并后共 %d 条消息（源 %d + 目标 %d，重复消息自动去重）",
			sourceTotal+targetTotal, sourceTotal, targetTotal))
		confirmBtn.SetEnabled(true)

		// 默认勾选重生成画像（如果目标消息数达到阈值）
		if target.OtherMsgCount >= config.Profile.ColdStartCount {
			regenChk.SetChecked(true)
		}
	}

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    "关联昵称",
		MinSize:  dl.Size{Width: 480, Height: 420},
		Layout:   dl.VBox{Spacing: 8},
		Children: []dl.Widget{
			dl.Label{
				Text: fmt.Sprintf("把「%s」的数据合并到下方选择的旧联系人。\n此操作会搬移全部消息与画像历史，可在历史页撤销。", sourceName),
				Font: fontHint,
			},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true, Spacing: 8},
				Children: []dl.Widget{
					dl.Label{Text: "新昵称（源）:", Font: fontSection},
					dl.Label{Text: sourceName, Font: fontBody},
				},
			},
			dl.Label{Text: "旧联系人（目标，保留的联系人）:", Font: fontSection},
			dl.LineEdit{
				AssignTo:  &targetLE,
				CueBanner: "搜索目标联系人昵称",
				OnTextChanged: func() {
					// 与联系人列表的搜索规则保持一致：忽略大小写，备注和历史昵称也参与匹配
					q := strings.ToLower(strings.TrimSpace(targetLE.Text()))
					filtered = nil
					for _, c := range candidates {
						if q == "" || matchContact(c, q) {
							filtered = append(filtered, c)
						}
					}
					tm.items = filtered
					tm.PublishItemsReset()
				},
			},
			dl.ListBox{
				AssignTo: &targetLB,
				Model:    tm,
				MinSize:  dl.Size{Width: 400, Height: 120},
			},
			dl.Label{AssignTo: &previewLabel, Text: "请选择目标联系人", Font: fontHint},
			dl.Composite{
				Layout: dl.VBox{MarginsZero: true, Spacing: 4},
				Children: []dl.Widget{
					dl.RadioButton{
						AssignTo: &useSourceRB,
						Text:     fmt.Sprintf("用新昵称「%s」作为显示名（推荐）", sourceName),
					},
					dl.RadioButton{
						AssignTo: &keepTargetRB,
						Text:     "用旧联系人的原昵称作为显示名",
					},
				},
			},
			dl.CheckBox{
				AssignTo: &regenChk,
				Text:     "合并完成后重新生成画像",
			},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						AssignTo: &confirmBtn,
						Text:     "确认合并",
						MinSize:  dl.Size{Width: 100, Height: 32},
						Enabled:  false,
						OnClicked: func() {
							idx := targetLB.CurrentIndex()
							if idx < 0 || idx >= len(tm.items) {
								return
							}
							target := tm.items[idx]
							if target.ID == sourceID {
								showError("不能把联系人合并到它自己")
								return
							}

							// 二次确认（finalName 不叫 displayName，避免遮蔽同名的包级函数）
							finalName := target.Name
							if useSourceRB.Checked() {
								finalName = sourceName
							}
							msg := fmt.Sprintf(
								"确认把「%s」合并到「%s」？\n\n"+
									"消息数：源 %d 条 → 目标 %d 条\n"+
									"最终显示名：%s\n"+
									"合并后「%s」的历史昵称将自动归位到该联系人。",
								sourceName, target.Name,
								sourceTotal, targetTotal,
								finalName, sourceName)
							if walk.MsgBox(dlg, "确认合并", msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
								return
							}

							// 执行合并
							result, err := doMerge(sourceID, target.ID, useSourceRB.Checked(), regenChk.Checked())
							if err != nil {
								showError("合并失败: " + err.Error())
								return
							}

							doneMsg := fmt.Sprintf("已搬移 %d 条消息、%d 条画像历史。",
								result.MovedMessages, result.MovedHistory)
							if result.ProfileCopied {
								doneMsg += "\n目标联系人原本没有画像，已把源联系人的画像拷过来。"
							}
							if regenChk.Checked() {
								doneMsg += "\n画像正在后台重新生成，稍后到画像页查看。"
							}
							walk.MsgBox(dlg, "合并完成", doneMsg, walk.MsgBoxIconInformation)

							// 异步重新生成画像（合并后没有"本次复制"语境，用目标联系人全部消息）。
							// 远程模式不用在这里做：doMerge 传的 regenerate=true 已经让
							// 服务端自己异步重生成了，这里再跑一遍是白跑，
							// 还要多拉一整轮分页消息。
							if regenChk.Checked() && !isRemoteMode {
								go func() {
									msgs, merr := doGetAllMessages(target.ID)
									if merr != nil {
										mainWindow.Synchronize(func() {
											showError("合并后重生成画像失败：读取消息出错 " + merr.Error())
										})
										return
									}
									if len(msgs) == 0 {
										return
									}
									if gerr := doGenerateOrUpdateProfile(target.ID, finalName, msgs); gerr != nil {
										_ = doSaveProfileHistory(target.ID, "{}", "画像生成失败: "+gerr.Error())
										mainWindow.Synchronize(func() {
											showError("合并后重生成画像失败: " + gerr.Error())
										})
									}
								}()
							}

							dlg.Accept()
							if onDone != nil {
								onDone()
							}
						},
					},
					dl.PushButton{
						Text:      "取消",
						MinSize:   dl.Size{Width: 80, Height: 32},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}).Create(mainWindow); err != nil {
		showError("创建合并对话框失败: " + err.Error())
		return
	}
	setTopMost(dlg.Handle())

	// 选中目标时更新预览
	targetLB.CurrentIndexChanged().Attach(updatePreview)

	// 默认选中「使用新昵称」
	useSourceRB.SetChecked(true)

	dlg.Run()
}

// mergeLogModel 合并记录列表模型
type mergeLogModel struct {
	walk.ListModelBase
	items []MergeLogEntry
}

func (m *mergeLogModel) ItemCount() int { return len(m.items) }

func (m *mergeLogModel) Value(index int) interface{} {
	e := m.items[index]
	return fmt.Sprintf("%s  «%s» 并入（%s）", displayTime(e.CreatedAt), e.SourceName, e.TargetName)
}

// showMergeHistoryDialog 显示当前联系人的合并记录，支持撤销（拆分）
func showMergeHistoryDialog(targetContact Contact, onDone func()) {
	var dlg *walk.Dialog
	var logLB *walk.ListBox
	var undoBtn *walk.PushButton

	logs, err := fetchMergeLogsForTarget(targetContact.ID)
	if err != nil {
		showError("读取合并记录失败: " + err.Error())
		return
	}
	if len(logs) == 0 {
		walk.MsgBox(mainWindow, "合并记录",
			fmt.Sprintf("「%s」没有已合并进来的昵称。", targetContact.Name),
			walk.MsgBoxIconInformation)
		return
	}

	lm := &mergeLogModel{items: logs}

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    fmt.Sprintf("合并记录 - %s", targetContact.Name),
		MinSize:  dl.Size{Width: 460, Height: 320},
		Layout:   dl.VBox{Spacing: 8},
		Children: []dl.Widget{
			dl.Label{
				Text: "以下昵称已合并到该联系人。选中一条点「撤销合并」可将其消息与画像历史拆分回去。",
				Font: fontHint,
			},
			dl.ListBox{
				AssignTo: &logLB,
				Model:    lm,
				MinSize:  dl.Size{Width: 420, Height: 140},
			},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						AssignTo: &undoBtn,
						Text:     "撤销合并",
						MinSize:  dl.Size{Width: 100, Height: 32},
						OnClicked: func() {
							idx := logLB.CurrentIndex()
							if idx < 0 || idx >= len(lm.items) {
								return
							}
							entry := lm.items[idx]
							if walk.MsgBox(dlg, "确认撤销",
								fmt.Sprintf("确认把「%s」从「%s」中拆分出来？\n该联系人的消息与画像历史将搬回原联系人。",
									entry.SourceName, entry.TargetName),
								walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
								return
							}
							if err := doUndoMerge(entry.ID); err != nil {
								showError("撤销失败: " + err.Error())
								return
							}
							walk.MsgBox(dlg, "已撤销", "合并已撤销，数据已搬回原联系人。", walk.MsgBoxIconInformation)
							// 刷新列表
							newLogs, err := fetchMergeLogsForTarget(targetContact.ID)
							if err != nil {
								showError("刷新合并记录失败: " + err.Error())
								return
							}
							lm.items = newLogs
							lm.PublishItemsReset()
							if len(newLogs) == 0 {
								dlg.Accept()
							}
							if onDone != nil {
								onDone()
							}
						},
					},
					dl.PushButton{
						Text:      "关闭",
						MinSize:   dl.Size{Width: 80, Height: 32},
						OnClicked: func() { dlg.Accept() },
					},
				},
			},
		},
	}).Create(mainWindow); err != nil {
		showError("创建合并记录对话框失败: " + err.Error())
		return
	}
	setTopMost(dlg.Handle())
	dlg.Run()
}
