package main

import (
	"fmt"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// Personal Relationship OS 2.0 桌面端集成：关系状态机 / 重新认识 TA / 今天值得做。
//
// 这三项均为服务端能力（statemachine / replay / decision 只存在于 bot 仓），
// 桌面端不重复实现，只在「远程模式」经 REST API 呈现；本地模式下这些入口会给出
// 明确提示而非静默失败。弹层范式完全沿用 ui_assistance.go：只读 TextEdit + 异步
// fetch + mainWindow.Synchronize + setTopMost + makeDialogResizable。

// os2RemoteOnlyHint 本地模式下的统一提示语（蓝图 §23.2：功能要求服务端时必须明说，禁止静默失败）。
const os2RemoteOnlyHint = "此功能需要连接 Relationship OS Server 才能使用（本地模式不提供）。\n请在 config.json 中启用远程模式：remote.enabled=true 且填写 remote.apiURL。"

// 状态枚举中文映射（与服务端 statemachine.go 常量一致）。
var (
	baseStateLabel = map[string]string{
		"unknown":    "未知",
		"introduced": "已认识",
		"familiar":   "熟悉",
		"stable":     "稳定",
		"close":      "亲密",
		"core":       "核心",
	}
	dynamicStateLabel = map[string]string{
		"stable":       "平稳",
		"warming":      "升温",
		"cooling":      "降温",
		"at_risk":      "有风险",
		"dormant":      "沉寂",
		"reconnecting": "重新连接",
	}
	trendStateLabel = map[string]string{
		"new":     "尚无互动",
		"warming": "升温",
		"cooling": "降温",
		"dormant": "长期沉寂",
		"stable":  "平稳",
		"":        "—",
	}
	alertLabel = map[string]string{
		"urgent":   "断点风险高",
		"watching": "需留意降温",
		"normal":   "正常",
		"":         "无",
	}
	riskLabel = map[int]string{0: "低", 1: "关注", 2: "高"}
)

func label(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	if k == "" {
		return "—"
	}
	return k
}

// fetchRelationshipStateText 拉取并渲染关系状态快照为文本（自动适配本地/远程）。
func fetchRelationshipStateText(id int64) (string, error) {
	if !isRemoteMode {
		return "", fmt.Errorf("%s", os2RemoteOnlyHint)
	}
	st, err := remoteClient.ContactState(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	name := st.Name
	if name == "" {
		name = "该联系人"
	}
	fmt.Fprintf(&b, "关系状态 · %s\n\n", name)
	fmt.Fprintf(&b, "基础状态（亲疏）：%s\n", label(baseStateLabel, st.BaseState))
	fmt.Fprintf(&b, "当下动态（温度）：%s\n", label(dynamicStateLabel, st.DynamicState))
	fmt.Fprintf(&b, "亲密度：%d / 100\n", st.Intimacy)
	fmt.Fprintf(&b, "互动趋势：%s\n", label(trendStateLabel, st.TrendState))
	fmt.Fprintf(&b, "预警：%s\n", label(alertLabel, st.Alert))
	fmt.Fprintf(&b, "关系健康度：%d\n", st.Health)
	if st.Reason != "" {
		fmt.Fprintf(&b, "\n判定依据：%s\n", st.Reason)
	}
	if st.ChangedAt != "" {
		fmt.Fprintf(&b, "最近转态：%s", st.ChangedAt)
	}
	if st.ComputedAt != "" {
		fmt.Fprintf(&b, "\n计算时间：%s", st.ComputedAt)
	}
	return strings.TrimSpace(b.String()), nil
}

// fetchReplayText 拉取「重新认识 TA」回放文本（服务端已渲染；自动适配本地/远程）。
func fetchReplayText(id int64) (string, error) {
	if !isRemoteMode {
		return "", fmt.Errorf("%s", os2RemoteOnlyHint)
	}
	txt, err := remoteClient.ContactReplay(id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(txt) == "" {
		return "暂无可回放的内容（该联系人记忆样本不足）。", nil
	}
	return txt, nil
}

// fetchTodayDecisionsText 拉取并渲染「今天值得做」决策清单为文本（全局，自动适配本地/远程）。
func fetchTodayDecisionsText(top int) (string, error) {
	if !isRemoteMode {
		return "", fmt.Errorf("%s", os2RemoteOnlyHint)
	}
	list, err := remoteClient.TodayDecisions(top)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "今天没有需要主动投入的关系行动，一切平稳。", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "今天最值得投入的关系行动（确定性排序，Top %d）\n", len(list))
	for i, d := range list {
		fmt.Fprintf(&b, "\n%d. %s", i+1, d.Name)
		if d.State != "" {
			fmt.Fprintf(&b, " · %s", d.State)
		} else {
			fmt.Fprintf(&b, " · %s/%s", label(baseStateLabel, d.BaseState), label(dynamicStateLabel, d.DynamicState))
		}
		fmt.Fprintf(&b, "　[风险：%s", riskText(d.Risk))
		if d.Opportunity > 0 {
			b.WriteString("，有机会")
		}
		b.WriteString("]")
		if len(d.WhyNow) > 0 {
			fmt.Fprintf(&b, "\n   为什么是现在：%s", strings.Join(d.WhyNow, "；"))
		}
		if d.Action != "" {
			fmt.Fprintf(&b, "\n   建议行动：%s", d.Action)
		}
		if d.Followup != "" {
			fmt.Fprintf(&b, "\n   待跟进：%s", d.Followup)
		}
		if d.Goal != "" {
			fmt.Fprintf(&b, "\n   相关目标：%s", d.Goal)
		}
		if d.Topic != "" {
			fmt.Fprintf(&b, "\n   话题线索：%s", d.Topic)
		}
		if d.BestTime != "" {
			fmt.Fprintf(&b, "\n   最佳时段：%s", d.BestTime)
		}
		if d.Confidence != "" {
			fmt.Fprintf(&b, "\n   置信度：%s", d.Confidence)
		}
	}
	fmt.Fprintf(&b, "\n\n说明：以上由服务端确定性汇聚状态/健康度/待办/目标/重要日子得出，仅供参考。")
	return b.String(), nil
}

func riskText(r int) string {
	if v, ok := riskLabel[r]; ok {
		return v
	}
	return "低"
}

// fetchMemoryReviewText 拉取并渲染「待确认记忆」队列为文本（全局，自动适配本地/远程）。
func fetchMemoryReviewText(limit int) (string, error) {
	if !isRemoteMode {
		return "", fmt.Errorf("%s", os2RemoteOnlyHint)
	}
	list, err := remoteClient.MemoryReview(limit)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "没有需要确认的记忆，画像事实都很稳。", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "待确认记忆（系统识别但尚未确认、或出现冲突需复核的事实，Top %d）\n", len(list))
	for i, it := range list {
		fmt.Fprintf(&b, "\n%d. %s", i+1, it.ContactName)
		if it.ContactName == "" {
			fmt.Fprintf(&b, "联系人#%d", it.ContactID)
		}
		fmt.Fprintf(&b, " · %s：%s", it.FactType, it.FactValue)
		if it.HasConflict {
			b.WriteString("　[⚠ 有冲突]")
		}
		if it.Reason != "" {
			fmt.Fprintf(&b, "\n   为何需确认：%s", it.Reason)
		}
		if it.Status != "" {
			fmt.Fprintf(&b, "\n   当前状态：%s", it.Status)
		}
		if it.Confidence > 0 {
			fmt.Fprintf(&b, "\n   置信度：%.2f", it.Confidence)
		}
	}
	fmt.Fprintf(&b, "\n\n说明：以上为服务端识别的记忆，确认前不作为既定事实；请到对应联系人页核对。")
	return b.String(), nil
}

// showMemoryReviewDialog 待确认记忆弹层（只读 + 刷新 + 复制）。
func showMemoryReviewDialog() {
	var dlg *walk.Dialog
	var output *walk.TextEdit
	var refresh *walk.PushButton
	load := func() {
		refresh.SetEnabled(false)
		go func() {
			out, err := fetchMemoryReviewText(50)
			mainWindow.Synchronize(func() {
				if dlg.IsDisposed() {
					return
				}
				refresh.SetEnabled(true)
				if err != nil {
					output.SetText(err.Error())
					return
				}
				output.SetText(strings.ReplaceAll(out, "\n", "\r\n"))
			})
		}()
	}
	err := (dl.Dialog{AssignTo: &dlg, Title: "待确认记忆 · Memory Review",
		MinSize: dl.Size{Width: 520, Height: 420}, Size: dl.Size{Width: 720, Height: 640},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: "服务端从对话中识别出、但尚未被确认或与现有事实冲突的记忆条目，逐条核对后才写为既定事实（只读）。"},
			dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 420}},
			dl.Composite{Layout: dl.HBox{MarginsZero: true, Spacing: 6}, Children: []dl.Widget{
				dl.PushButton{AssignTo: &refresh, Text: "刷新队列", OnClicked: load},
				dl.PushButton{Text: "复制", OnClicked: func() { _ = clipboard.WriteAll(output.Text()) }},
			}},
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

// showRelationshipStateDialog 关系状态快照弹层（只读 + 刷新 + 复制）。
func showRelationshipStateDialog(id int64) {
	if id <= 0 {
		return
	}
	var dlg *walk.Dialog
	var output *walk.TextEdit
	var refresh *walk.PushButton
	load := func() {
		refresh.SetEnabled(false)
		go func() {
			out, err := fetchRelationshipStateText(id)
			mainWindow.Synchronize(func() {
				if dlg.IsDisposed() {
					return
				}
				refresh.SetEnabled(true)
				if err != nil {
					output.SetText(err.Error())
					return
				}
				output.SetText(strings.ReplaceAll(out, "\n", "\r\n"))
			})
		}()
	}
	err := (dl.Dialog{AssignTo: &dlg, Title: "关系状态",
		MinSize: dl.Size{Width: 460, Height: 360}, Size: dl.Size{Width: 620, Height: 560},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: "关系状态机为该联系人派生的基态 / 动态 / 亲密度 / 趋势 / 预警快照（服务端计算，只读）。"},
			dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 340}},
			dl.Composite{Layout: dl.HBox{MarginsZero: true, Spacing: 6}, Children: []dl.Widget{
				dl.PushButton{AssignTo: &refresh, Text: "刷新状态", OnClicked: load},
				dl.PushButton{Text: "复制", OnClicked: func() { _ = clipboard.WriteAll(output.Text()) }},
			}},
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

// showReplayDialog 「重新认识 TA」Memory Replay 回放弹层（只读 + 刷新 + 复制）。
func showReplayDialog(id int64) {
	if id <= 0 {
		return
	}
	var dlg *walk.Dialog
	var output *walk.TextEdit
	var refresh *walk.PushButton
	load := func() {
		refresh.SetEnabled(false)
		go func() {
			out, err := fetchReplayText(id)
			mainWindow.Synchronize(func() {
				if dlg.IsDisposed() {
					return
				}
				refresh.SetEnabled(true)
				if err != nil {
					output.SetText(err.Error())
					return
				}
				output.SetText(strings.ReplaceAll(out, "\n", "\r\n"))
			})
		}()
	}
	err := (dl.Dialog{AssignTo: &dlg, Title: "重新认识 TA · Memory Replay",
		MinSize: dl.Size{Width: 480, Height: 400}, Size: dl.Size{Width: 720, Height: 680},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: "从原始消息重新回放对 TA 的记忆与可信事实，帮助重新认识（服务端渲染，只读）。"},
			dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 440}},
			dl.Composite{Layout: dl.HBox{MarginsZero: true, Spacing: 6}, Children: []dl.Widget{
				dl.PushButton{AssignTo: &refresh, Text: "重新回放", OnClicked: load},
				dl.PushButton{Text: "复制", OnClicked: func() { _ = clipboard.WriteAll(output.Text()) }},
			}},
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

// showTodayDecisionsDialog 「今天值得做」全局决策面板（只读 + 刷新 + 复制）。
func showTodayDecisionsDialog() {
	var dlg *walk.Dialog
	var output *walk.TextEdit
	var refresh *walk.PushButton
	load := func() {
		refresh.SetEnabled(false)
		go func() {
			out, err := fetchTodayDecisionsText(10)
			mainWindow.Synchronize(func() {
				if dlg.IsDisposed() {
					return
				}
				refresh.SetEnabled(true)
				if err != nil {
					output.SetText(err.Error())
					return
				}
				output.SetText(strings.ReplaceAll(out, "\n", "\r\n"))
			})
		}()
	}
	err := (dl.Dialog{AssignTo: &dlg, Title: "今天值得做 · 关系决策",
		MinSize: dl.Size{Width: 520, Height: 420}, Size: dl.Size{Width: 760, Height: 680},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: "服务端决策引擎汇聚状态 / 健康度 / 待办 / 目标 / 重要日子，确定性给出今天最值得投入的关系行动（非模型排序，只读）。"},
			dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 420}},
			dl.Composite{Layout: dl.HBox{MarginsZero: true, Spacing: 6}, Children: []dl.Widget{
				dl.PushButton{AssignTo: &refresh, Text: "刷新决策", OnClicked: load},
				dl.PushButton{Text: "复制", OnClicked: func() { _ = clipboard.WriteAll(output.Text()) }},
			}},
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
