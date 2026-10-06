package main

// V7 §23 Desktop v4.0 —— 三个核心入口 + 首页 Today Top 3。
//
// 蓝图铁律「不复制完整 Web」：桌面端不重复实现任何服务端智能，只把三项服务端能力经
// REST API 呈现为一等入口，并严守 §23.2 本地/远程分叉——本地模式只调本地已有能力，
// 要求服务端的功能一律给出「此功能需要连接 Relationship OS Server」的明确提示，禁止静默失败。
//
// 弹层范式完全沿用 ui_relationship.go / ui_assistance.go：只读 TextEdit + 异步 fetch +
// mainWindow.Synchronize + setTopMost + makeDialogResizable，本地模式 fetch 返回提示文本入框。

import (
	"fmt"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// §23 三个核心入口 + 首页：
//   1. Contact Brief      联系前简报（服务端 §14，纯确定性）
//   2. Relationship Session 关系会话（服务端 §7，只读编排）
//   3. Memory Maintenance  记忆维护（服务端 §3/§17 待确认记忆队列，全局）
//      首页 Today Top 3    今天最值得做的 3 条（服务端确定性排序，非 LLM）

// ---- 渲染：把服务端 JSON 拍平成只读文本 ----

// renderBriefFacts 渲染 BeforeBrief 事实全景（Contact Brief 与 Relationship Session 共用）。
func renderBriefFacts(b briefBeforeJSON, sb *strings.Builder) {
	fmt.Fprintf(sb, "关系状态：%s\n", orDash(b.RelationshipState))
	fmt.Fprintf(sb, "关系健康度：%d\n", b.Health)
	if b.Change30d != "" {
		fmt.Fprintf(sb, "近 30 天变化：%s\n", b.Change30d)
	}
	writeListSection(sb, "重要事实", b.ImportantFacts)
	writeListSection(sb, "冲突事实（需确认）", b.ConflictFacts)
	writeListSection(sb, "未完成事项", b.OpenItems)
	writeListSection(sb, "相关目标", b.Goals)
	writeListSection(sb, "相关项目", b.Projects)
	writeListSection(sb, "风险", b.Risks)
	writeListSection(sb, "机会", b.Opportunities)
	if b.LastAction != "" || b.LastActionAt != "" {
		fmt.Fprintf(sb, "\n上次行动：%s %s\n", orDash(b.LastAction), b.LastActionAt)
	}
}

// renderStrategy 渲染推荐策略（SessionStrategy）。
func renderStrategy(s strategyJSON, sb *strings.Builder) {
	sb.WriteString("\n—— 推荐策略 ——\n")
	fmt.Fprintf(sb, "首选：%s\n", orDash(s.Recommended))
	if s.Backup != "" {
		fmt.Fprintf(sb, "备选：%s\n", s.Backup)
	}
	if len(s.Why) > 0 {
		fmt.Fprintf(sb, "推荐理由：%s\n", strings.Join(s.Why, "；"))
	}
	if len(s.DataBasis) > 0 {
		fmt.Fprintf(sb, "数据依据：%s\n", strings.Join(s.DataBasis, "；"))
	}
	if s.Confidence != "" {
		fmt.Fprintf(sb, "置信度：%s\n", s.Confidence)
	}
	if s.HistEffectiveness != "" {
		fmt.Fprintf(sb, "历史有效性：%s\n", s.HistEffectiveness)
	}
	if s.BestTime != "" {
		fmt.Fprintf(sb, "最佳时段：%s\n", s.BestTime)
	}
}

func writeListSection(sb *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n%s：\n", title)
	for _, it := range items {
		fmt.Fprintf(sb, "  · %s\n", it)
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// ---- 入口 1：Contact Brief 联系前简报（§14）----

// fetchContactBriefText 拉取并渲染联系前简报（本地模式返回 §23.2 明确提示）。
func fetchContactBriefText(id int64) (string, error) {
	if !isRemoteMode {
		return "", fmt.Errorf("%s", os2RemoteOnlyHint)
	}
	b, err := remoteClient.ContactBrief(id)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	name := orDash(b.Name)
	fmt.Fprintf(&sb, "联系前简报 · %s\n", name)
	fmt.Fprintf(&sb, "（数据来源：%s · 纯确定性，不依赖大模型）\n\n", orDash(b.Layer))
	renderBriefFacts(b.Brief, &sb)
	writeListSection(&sb, "最近谈过什么", b.RecentTopics)
	writeListSection(&sb, "此刻应避免", b.AvoidItems)
	renderStrategy(b.Strategy, &sb)
	sb.WriteString("\n说明：以上由服务端 Context Engine 确定性汇聚，联系前一眼看清。")
	return sb.String(), nil
}

// ---- 入口 2：Relationship Session 关系会话（§7）----

// fetchRelationshipSessionText 拉取并渲染一屏关系会话（本地模式返回 §23.2 明确提示）。
func fetchRelationshipSessionText(id int64) (string, error) {
	if !isRemoteMode {
		return "", fmt.Errorf("%s", os2RemoteOnlyHint)
	}
	s, err := remoteClient.RelationshipSession(id)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "关系会话 · %s\n\n", orDash(s.Name))
	renderBriefFacts(s.Brief, &sb)
	renderStrategy(s.Strategy, &sb)
	if len(s.ObservationDays) > 0 {
		days := make([]string, 0, len(s.ObservationDays))
		for _, d := range s.ObservationDays {
			days = append(days, fmt.Sprintf("%d 天", d))
		}
		fmt.Fprintf(&sb, "\n长期回看计划：%s 后观察效果\n", strings.Join(days, " / "))
	}
	sb.WriteString("\n说明：本屏为服务端只读编排（Brief + 策略 + 观察计划），不改动任何数据。" +
		"\n对话预演（Rehearsal）请到网页端进行。")
	return sb.String(), nil
}

// ---- 首页：Today Top 3 ----

// fetchTodayTop3Text 拉取并渲染今天最值得做的 Top 3（本地模式返回 §23.2 明确提示）。
func fetchTodayTop3Text() (string, error) {
	if !isRemoteMode {
		return "", fmt.Errorf("%s", os2RemoteOnlyHint)
	}
	list, err := remoteClient.TodayDecisions(3)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "今天没有需要主动投入的关系行动，一切平稳。", nil
	}
	var sb strings.Builder
	sb.WriteString("Today · 今天最值得做的 3 件事\n")
	for i, d := range list {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, orDash(d.Name))
		if d.Action != "" {
			fmt.Fprintf(&sb, "：%s", d.Action)
		}
		if len(d.WhyNow) > 0 {
			fmt.Fprintf(&sb, "\n   为什么是现在：%s", strings.Join(d.WhyNow, "；"))
		}
	}
	sb.WriteString("\n\n说明：以上由服务端确定性排序（非大模型），按优先级取前 3。")
	return sb.String(), nil
}

// ---- 通用只读弹层工厂（沿用 ui_relationship.go 范式，避免重复样板）----

// showFetchDialog 打开一个只读 TextEdit 弹层，异步调用 fetch 渲染文本，附「刷新 / 复制」。
func showFetchDialog(title, intro string, fetch func() (string, error)) {
	var dlg *walk.Dialog
	var output *walk.TextEdit
	var refresh *walk.PushButton
	load := func() {
		refresh.SetEnabled(false)
		go func() {
			out, err := fetch()
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
	err := (dl.Dialog{AssignTo: &dlg, Title: title,
		MinSize: dl.Size{Width: 520, Height: 440}, Size: dl.Size{Width: 760, Height: 680},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: intro},
			dl.TextEdit{AssignTo: &output, ReadOnly: true, VScroll: true, MinSize: dl.Size{Height: 440}},
			dl.Composite{Layout: dl.HBox{MarginsZero: true, Spacing: 6}, Children: []dl.Widget{
				dl.PushButton{AssignTo: &refresh, Text: "刷新", OnClicked: load},
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

// showContactBriefDialog 入口 1：联系前简报（需选中联系人）。
func showContactBriefDialog(id int64) {
	if id <= 0 {
		showError("请先在列表中选择一个联系人")
		return
	}
	showFetchDialog("联系前简报 · Contact Brief",
		"联系 TA 之前一眼看清当前状态 / 变化 / 事实 / 冲突 / 未完成 / 风险 / 机会 / 推荐策略（服务端纯确定性，只读）。",
		func() (string, error) { return fetchContactBriefText(id) })
}

// showRelationshipSessionDialog 入口 2：关系会话（需选中联系人）。
func showRelationshipSessionDialog(id int64) {
	if id <= 0 {
		showError("请先在列表中选择一个联系人")
		return
	}
	showFetchDialog("关系会话 · Relationship Session",
		"「开始处理这段关系」的一屏只读编排：事实全景 + 推荐策略 + 长期观察计划（服务端计算，不改数据）。",
		func() (string, error) { return fetchRelationshipSessionText(id) })
}

// showTodayTop3Dialog 首页：Today Top 3（全局）。
func showTodayTop3Dialog() {
	showFetchDialog("首页 · Today Top 3",
		"今天最值得主动投入的 3 段关系行动（服务端确定性排序，非大模型，只读）。",
		fetchTodayTop3Text)
}
