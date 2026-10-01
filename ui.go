package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/atotto/clipboard"
	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procSetLayered  = user32.NewProc("SetLayeredWindowAttributes")
	procAppendMenuW = user32.NewProc("AppendMenuW")
)

const lwaAlpha = 0x2

func setLayeredWindowAttributes(hwnd win.HWND, alpha byte) {
	procSetLayered.Call(uintptr(hwnd), 0, uintptr(alpha), lwaAlpha)
}

// setTopMost 把窗口设为置顶，不被其他程序覆盖
func setTopMost(hwnd win.HWND) {
	win.SetWindowPos(hwnd, win.HWND_TOPMOST, 0, 0, 0, 0,
		win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOACTIVATE)
}

func appendPopupItem(hMenu win.HMENU, id uintptr, text string) {
	ptr, _ := syscall.UTF16PtrFromString(text)
	procAppendMenuW.Call(uintptr(hMenu), 0x0, id, uintptr(unsafe.Pointer(ptr)))
}

func appendPopupSeparator(hMenu win.HMENU) {
	procAppendMenuW.Call(uintptr(hMenu), 0x800, 0, 0)
}

// 全局 UI 状态
var (
	mainWindow  *walk.MainWindow // 悬浮主窗
	statusLabel *walk.Label      // 悬浮窗状态文字
	clickLock   sync.Mutex       // 防止识别按钮连点
	db          *sql.DB          // 数据库
	llmClient   *LLMClient       // 大模型客户端
)

// 公共字体样式
var (
	fontTitle   = dl.Font{Family: "Microsoft YaHei UI", PointSize: 12, Bold: true}
	fontSection = dl.Font{Family: "Microsoft YaHei UI", PointSize: 10, Bold: true}
	fontBody    = dl.Font{Family: "Microsoft YaHei UI", PointSize: 9}
	fontHint    = dl.Font{Family: "Microsoft YaHei UI", PointSize: 8}
)

// SetupFloatingWindow 创建置顶无边框悬浮窗
func SetupFloatingWindow() error {
	var identifyBtn, profileBtn *walk.PushButton
	if err := (dl.MainWindow{
		AssignTo: &mainWindow,
		Title:    "画像助手",
		Size:     dl.Size{Width: 150, Height: 96},
		MinSize:  dl.Size{Width: 150, Height: 96},
		MaxSize:  dl.Size{Width: 150, Height: 96},
		Layout:   dl.VBox{MarginsZero: true, Spacing: 2},
		Children: []dl.Widget{
			dl.PushButton{
				AssignTo:  &identifyBtn,
				Text:      "识 别",
				Font:      fontTitle,
				MinSize:   dl.Size{Width: 140, Height: 36},
				OnClicked: onIdentifyClicked,
			},
			dl.PushButton{
				AssignTo: &profileBtn,
				Text:     "画 像",
				Font:     fontBody,
				MinSize:  dl.Size{Width: 140, Height: 28},
				OnClicked: func() {
					ShowProfileWindow(0)
				},
			},
			dl.Label{
				AssignTo:      &statusLabel,
				Text:          "复制后点识别",
				Font:          fontHint,
				TextAlignment: dl.AlignCenter,
			},
		},
	}).Create(); err != nil {
		return err
	}

	applyFloatingStyle(mainWindow.Handle())
	bindFloatingEvents()

	// 定时检查置顶状态，仅在被其他程序顶掉时才重申
	// （无脑每秒 SetWindowPos 会导致分层窗口闪烁，还会把结果窗/画像窗压到浮窗下面）
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			mainWindow.Synchronize(func() {
				hwnd := mainWindow.Handle()
				if win.GetWindowLong(hwnd, win.GWL_EXSTYLE)&win.WS_EX_TOPMOST == 0 {
					setTopMost(hwnd)
				}
			})
		}
	}()
	return nil
}

// applyFloatingStyle 去掉标题栏与边框、半透明、置顶并放到屏幕右侧三分之二处
func applyFloatingStyle(hwnd win.HWND) {
	style := win.GetWindowLong(hwnd, win.GWL_STYLE)
	style &^= win.WS_CAPTION | win.WS_THICKFRAME
	win.SetWindowLong(hwnd, win.GWL_STYLE, style)

	exStyle := win.GetWindowLong(hwnd, win.GWL_EXSTYLE)
	exStyle |= win.WS_EX_LAYERED | win.WS_EX_TOPMOST
	win.SetWindowLong(hwnd, win.GWL_EXSTYLE, exStyle)
	setLayeredWindowAttributes(hwnd, 230)

	win.SetWindowPos(hwnd, win.HWND(0), 0, 0, 0, 0,
		win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_FRAMECHANGED)

	const winW, winH = 150, 96
	cx := win.GetSystemMetrics(win.SM_CXSCREEN)
	cy := win.GetSystemMetrics(win.SM_CYSCREEN)
	x := cx - winW - 20
	y := cy * 2 / 3
	if y+winH > cy {
		y = cy - winH - 20
	}
	if y < 0 {
		y = 0
	}
	win.SetWindowPos(hwnd, win.HWND_TOPMOST, x, y, winW, winH, win.SWP_NOACTIVATE)
}

// bindFloatingEvents 左键拖动 + 右键菜单
func bindFloatingEvents() {
	mainWindow.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			win.ReleaseCapture()
			win.SendMessage(mainWindow.Handle(), win.WM_NCLBUTTONDOWN, win.HTCAPTION, 0)
		}
	})

	mainWindow.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button != walk.RightButton {
			return
		}
		hMenu := win.CreatePopupMenu()
		appendPopupItem(hMenu, 1, "查看画像")
		appendPopupItem(hMenu, 2, "查看联系人")
		appendPopupSeparator(hMenu)
		appendPopupItem(hMenu, 3, "设置")
		appendPopupItem(hMenu, 4, "退出")
		win.SetForegroundWindow(mainWindow.Handle())
		var pt win.POINT
		win.GetCursorPos(&pt)
		cmd := win.TrackPopupMenuEx(hMenu,
			win.TPM_RETURNCMD|win.TPM_RIGHTBUTTON,
			pt.X, pt.Y, mainWindow.Handle(), nil)
		switch cmd {
		case 1, 2:
			ShowProfileWindow(0)
		case 3:
			walk.MsgBox(mainWindow, "设置",
				"请编辑程序同目录下的 config.json，填写 myName 与 llm.apiKey，\n保存后重新启动程序生效。\n\n注意：聊天内容会发送到配置的云端大模型接口，请注意隐私。",
				walk.MsgBoxIconInformation)
		case 4:
			walk.App().Exit(0)
		}
	})
}

// onIdentifyClicked 识别按钮：读剪贴板 → 解析 → 存储 → 意图分析
func onIdentifyClicked() {
	if !clickLock.TryLock() {
		return
	}
	statusLabel.SetText("识别中...")

	go func() {
		defer clickLock.Unlock()
		finalStatus := "复制后点识别"
		defer func() {
			mainWindow.Synchronize(func() { statusLabel.SetText(finalStatus) })
		}()

		// 1. 读剪贴板
		text, err := ReadClipboard()
		if err != nil {
			if errors.Is(err, ErrClipboardUnchanged) {
				finalStatus = "内容未变化，请重新复制"
				return
			}
			if errors.Is(err, ErrClipboardEmpty) {
				finalStatus = "剪贴板为空"
				return
			}
			mainWindow.Synchronize(func() { showError(err.Error()) })
			return
		}

		// 2. 解析聊天记录
		messages := ParseClipboard(text, config.MyName)
		if len(messages) == 0 {
			finalStatus = "未解析到消息"
			return
		}

		// 3. 推断联系人
		contactName := InferContactName(messages, config.MyName)
		if strings.TrimSpace(contactName) == "" {
			dumpPath := DumpClipboardForDebug(text)
			preview := strings.TrimSpace(text)
			if r := []rune(preview); len(r) > 120 {
				preview = string(r[:120]) + "..."
			}
			mainWindow.Synchronize(func() {
				showError(fmt.Sprintf(
					"无法识别对方昵称，可能原因：\n"+
						"1. 复制的不是带昵称的完整聊天记录；\n"+
						"2. config.json 中的 myName 未改成你自己的微信昵称。\n\n"+
						"已把本次剪贴板内容保存到：\n%s\n\n"+
						"识别到的前 120 字：\n%s\n\n"+
						"请检查 config.json 的 myName，或把上面的文件发给开发者适配新版格式。",
					dumpPath, preview))
			})
			return
		}

		// 4. 落库（三级解析：精确匹配→别名→新建）
		contactID, viaAlias, err := ResolveContactID(db, contactName)
		if err != nil {
			mainWindow.Synchronize(func() { showError("写入联系人失败: " + err.Error()) })
			return
		}
		if viaAlias {
			finalStatus = "旧昵称已归位"
		}
		newCount, err := SaveMessages(db, contactID, messages)
		if err != nil {
			mainWindow.Synchronize(func() { showError("保存消息失败: " + err.Error()) })
			return
		}

		// 5. 取对方最后一条消息做意图分析
		latestOther := ""
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Sender == "other" {
				latestOther = messages[i].Content
				break
			}
		}
		if latestOther == "" {
			latestOther = messages[len(messages)-1].Content
		}

		result, analysisErr := AnalyzeIntent(db, llmClient, contactID, latestOther)
		mainWindow.Synchronize(func() {
			ShowResultWindow(contactID, contactName, newCount, viaAlias, result, analysisErr)
		})

		// 6. 异步生成/更新画像，不阻塞结果展示
		go func() {
			if ShouldGenerateProfile(db, contactID) || ShouldUpdateProfile(db, contactID) {
				_ = GenerateOrUpdateProfile(db, llmClient, contactID, contactName)
			}
		}()
	}()
}

// fieldString 从分析结果 map 中安全取字段
func fieldString(m map[string]interface{}, key string) string {
	if m == nil {
		return "暂无"
	}
	if v, ok := m[key]; ok && v != nil {
		s := strings.TrimSpace(fmt.Sprintf("%v", v))
		if s != "" {
			return s
		}
	}
	return "暂无"
}

// ShowResultWindow 弹出意图分析结果窗口（卡片式布局）
func ShowResultWindow(contactID int64, contactName string, newCount int, viaAlias bool, result map[string]interface{}, analysisErr error) {
	var dlg *walk.Dialog
	var summaryLabel *walk.Label
	var suggestionTE *walk.TextEdit
	var confidencePB *walk.ProgressBar
	var confidenceLabel *walk.Label

	var headerText string
	if viaAlias {
		headerText = fmt.Sprintf("%s · 本次新增 %d 条 · 旧昵称已归位", contactName, newCount)
	} else {
		headerText = fmt.Sprintf("%s · 本次新增 %d 条", contactName, newCount)
	}

	children := []dl.Widget{
		dl.Label{
			AssignTo: &summaryLabel,
			Text:     headerText,
			Font:     fontTitle,
		},
	}

	if analysisErr != nil {
		children = append(children,
			dl.GroupBox{
				Title:  "分析失败",
				Layout: dl.VBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.TextEdit{
						ReadOnly: true,
						VScroll:  true,
						Text:     analysisErr.Error(),
						MinSize:  dl.Size{Width: 480, Height: 200},
					},
				},
			},
		)
	} else {
		// 五个固定卡片
		cardKeys := []struct {
			title string
			key   string
		}{
			{"表面意思", "surface"},
			{"潜在意图", "intent"},
			{"情绪状态", "emotion"},
			{"潜台词", "subtext"},
			{"建议回复", "suggested_reply"},
		}

		for _, c := range cardKeys {
			content := fieldString(result, c.key)
			if c.key == "suggested_reply" {
				children = append(children,
					dl.GroupBox{
						Title:  c.title,
						Layout: dl.VBox{MarginsZero: true, Spacing: 4},
						Children: []dl.Widget{
							dl.Composite{
								Layout: dl.HBox{MarginsZero: true, Spacing: 4},
								Children: []dl.Widget{
									dl.TextEdit{
										AssignTo: &suggestionTE,
										ReadOnly: true,
										VScroll:  true,
										Text:     content,
										MinSize:  dl.Size{Width: 380, Height: 60},
									},
									dl.PushButton{
										Text:    "复制",
										MinSize: dl.Size{Width: 60, Height: 28},
										OnClicked: func() {
											if suggestionTE != nil {
												_ = clipboard.WriteAll(suggestionTE.Text())
											}
										},
									},
								},
							},
						},
					},
				)
			} else {
				children = append(children,
					dl.GroupBox{
						Title:  c.title,
						Layout: dl.VBox{MarginsZero: true},
						Children: []dl.Widget{
							dl.TextEdit{
								ReadOnly: true,
								VScroll:  true,
								Text:     content,
								MinSize:  dl.Size{Width: 480, Height: 50},
							},
						},
					},
				)
			}
		}

		// 置信度
		confidenceStr := fieldString(result, "confidence")
		children = append(children,
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true, Spacing: 8},
				Children: []dl.Widget{
					dl.Label{Text: "置信度:", Font: fontSection},
					dl.ProgressBar{
						AssignTo: &confidencePB,
						MinSize:  dl.Size{Width: 200, Height: 20},
					},
					dl.Label{AssignTo: &confidenceLabel, Text: confidenceStr, Font: fontBody},
				},
			},
		)
	}

	children = append(children,
		dl.Composite{
			Layout: dl.HBox{MarginsZero: true},
			Children: []dl.Widget{
				dl.HSpacer{},
				dl.PushButton{
					Text:    "查看完整画像",
					MinSize: dl.Size{Width: 120, Height: 32},
					OnClicked: func() {
						dlg.Cancel()
						ShowProfileWindow(contactID)
					},
				},
				dl.PushButton{
					Text:      "关闭",
					MinSize:   dl.Size{Width: 80, Height: 32},
					OnClicked: func() { dlg.Cancel() },
				},
			},
		},
	)

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    "意图分析结果",
		MinSize:  dl.Size{Width: 520, Height: 480},
		Layout:   dl.VBox{Spacing: 8},
		Children: children,
	}).Create(mainWindow); err != nil {
		showError("创建结果窗口失败: " + err.Error())
		return
	}
	setTopMost(dlg.Handle())

	// 设置置信度进度条
	if confidencePB != nil {
		confidenceStr := fieldString(result, "confidence")
		confidenceVal := 0
		fmt.Sscanf(confidenceStr, "%d", &confidenceVal)
		if confidenceVal < 0 {
			confidenceVal = 0
		}
		if confidenceVal > 100 {
			confidenceVal = 100
		}
		confidencePB.SetValue(confidenceVal)
	}
	dlg.Run()
}

// ---------------- 画像主窗口的表格模型 ----------------

// contactTableModel 联系人列表模型
type contactTableModel struct {
	walk.TableModelBase
	items []Contact
}

func (m *contactTableModel) RowCount() int { return len(m.items) }

func (m *contactTableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	c := m.items[row]
	switch col {
	case 0:
		return displayName(&c)
	case 1:
		return c.OtherMsgCount
	case 2:
		return c.LastUpdated
	}
	return nil
}

func (m *contactTableModel) set(items []Contact) { m.items = items }

// messageTableModel 消息列表模型
type messageTableModel struct {
	walk.TableModelBase
	items []Message
}

func (m *messageTableModel) RowCount() int { return len(m.items) }

func (m *messageTableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	msg := m.items[row]
	switch col {
	case 0:
		if msg.Sender == "me" {
			return "我"
		}
		return "对方"
	case 1:
		return msg.Content
	case 2:
		if msg.Timestamp.IsZero() {
			return ""
		}
		return msg.Timestamp.Format("2006-01-02 15:04:05")
	}
	return nil
}

func (m *messageTableModel) set(items []Message) { m.items = items }

// historyListModel 画像变更历史模型
type historyListModel struct {
	walk.ListModelBase
	items []ProfileHistory
}

func (m *historyListModel) ItemCount() int { return len(m.items) }

func (m *historyListModel) Value(index int) interface{} {
	if index < 0 || index >= len(m.items) {
		return nil
	}
	h := m.items[index]
	return fmt.Sprintf("[%s] %s", h.CreatedAt, h.ChangeSummary)
}

func (m *historyListModel) set(items []ProfileHistory) { m.items = items }

// formatList 把字符串列表格式化成多行（每项一行，带 - 前缀）
func formatList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		fmt.Fprintf(&b, "- %s\r\n", item)
	}
	return b.String()
}

// ShowProfileWindow 展示完整画像：左侧联系人，右侧画像/消息/历史/统计
func ShowProfileWindow(targetContactID int64) {
	var dlg *walk.Dialog
	var searchLE *walk.LineEdit
	var contactsTV *walk.TableView
	var summaryLabel *walk.Label
	// 画像页：单页结构化全量展示，无需点击
	var sectionTE *walk.TextEdit
	var messagesTV *walk.TableView
	var msgDetailTE *walk.TextEdit
	var historyLV *walk.ListBox
	var historyDetailTE *walk.TextEdit
	var statTotalVal, statMineVal, statOtherVal *walk.Label
	var statFirstVal, statLastVal, statProfileVal *walk.Label
	var statAliasVal *walk.Label
	var prevBtn, nextBtn *walk.PushButton
	var pageLabel *walk.Label
	var mergeBtn *walk.PushButton
	var showMergedChk *walk.CheckBox

	contacts, err := GetAllContacts(db, false)
	if err != nil {
		showError("读取联系人失败: " + err.Error())
		return
	}
	cm := &contactTableModel{items: contacts}
	mm := &messageTableModel{}
	hm := &historyListModel{}

	const pageSize = 50
	offset := 0
	var currentID int64 = -1
	var currentContact *Contact

	// loadMessages 按当前页重新加载消息
	var loadMessages func()
	loadMessages = func() {
		if currentID <= 0 {
			return
		}
		page, err := GetMessagesPage(db, currentID, offset, pageSize)
		if err != nil {
			return
		}
		mm.set(page)
		mm.PublishRowsReset()
		pageLabel.SetText(fmt.Sprintf("第 %d 页（最新在前）", offset/pageSize+1))
		prevBtn.SetEnabled(offset > 0)
		nextBtn.SetEnabled(len(page) >= pageSize)
	}

	// renderProfile 渲染当前联系人的完整画像（全部分区一次铺开）
	renderProfile := func() {
		if currentContact == nil {
			sectionTE.SetText("请选择左侧联系人")
			return
		}
		pj := strings.TrimSpace(currentContact.ProfileJSON)
		if pj == "" || pj == "{}" {
			sectionTE.SetText(fmt.Sprintf("暂无画像\r\n\r\n对方消息积累到 %d 条后自动生成画像\r\n当前已积累 %d 条",
				config.Profile.ColdStartCount, currentContact.OtherMsgCount))
			return
		}
		var p Profile
		if jsonErr := json.Unmarshal([]byte(pj), &p); jsonErr != nil {
			sectionTE.SetText("画像数据解析失败：\r\n" + pj)
			return
		}
		sectionTE.SetText(profileFullText(&p))
	}

	// renderHistory 解析一条历史画像并全量结构化显示
	renderHistory := func(h ProfileHistory) {
		var p Profile
		if jsonErr := json.Unmarshal([]byte(h.ProfileJSON), &p); jsonErr != nil {
			historyDetailTE.SetText("历史画像解析失败：\r\n" + h.ProfileJSON)
			return
		}
		historyDetailTE.SetText(profileFullText(&p))
	}

	// loadContact 刷新右侧全部页签
	loadContact := func(id int64) {
		currentID = id
		contact, err := GetContactByIDWithMerged(db, id)
		if err != nil {
			return
		}
		currentContact = contact

		// 概要条：短标题防裁剪，完整概括在画像页【概要】分区
		dispName := displayName(contact)
		if strings.TrimSpace(contact.ProfileSummary) != "" {
			summaryLabel.SetText(fmt.Sprintf("%s · 对方消息 %d 条", dispName, contact.OtherMsgCount))
		} else if contact.OtherMsgCount >= config.Profile.ColdStartCount {
			summaryLabel.SetText(dispName + " · 画像生成中，稍后点击查看")
		} else {
			summaryLabel.SetText(fmt.Sprintf("%s · 积累中 %d/%d 条",
				dispName, contact.OtherMsgCount, config.Profile.ColdStartCount))
		}

		// 画像页：全量结构化展示
		renderProfile()

		// 消息页
		offset = 0
		loadMessages()
		msgDetailTE.SetText("")

		// 历史页
		history, _ := GetProfileHistory(db, id, 100)
		hm.set(history)
		hm.PublishItemsReset()
		if len(history) > 0 {
			historyLV.SetCurrentIndex(0)
			renderHistory(history[0])
		} else {
			historyDetailTE.SetText("暂无画像历史")
		}

		// 统计页
		stats, _ := GetContactStats(db, contact.ID)
		aliases, _ := GetAliases(db, contact.ID)
		aliasStr := strings.Join(aliases, "、")
		if len(aliases) == 0 {
			aliasStr = "无"
		}
		statTotalVal.SetText(fmt.Sprintf("%d 条", stats.Total))
		statMineVal.SetText(fmt.Sprintf("%d 条", stats.Mine))
		statOtherVal.SetText(fmt.Sprintf("%d 条", stats.Other))
		statFirstVal.SetText(orUnknown(stats.FirstTime))
		statLastVal.SetText(orUnknown(stats.LastTime))
		statProfileVal.SetText(orUnknown(contact.LastUpdated))
		statAliasVal.SetText(aliasStr)
	}

	// 刷新联系人列表
	refreshContacts := func() {
		showMerged := showMergedChk.Checked()
		contacts, err := GetAllContacts(db, showMerged)
		if err != nil {
			return
		}
		cm.set(contacts)
		cm.PublishRowsReset()
	}

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    "人物画像",
		Size:     dl.Size{Width: 640, Height: 860},
		MinSize:  dl.Size{Width: 520, Height: 600},
		Layout:   dl.VBox{Spacing: 6},
		Children: []dl.Widget{
			dl.HSplitter{
				Children: []dl.Widget{
					// 左栏：搜索 + 联系人列表 + 工具条
					dl.Composite{
						Layout: dl.VBox{Spacing: 4},
						Children: []dl.Widget{
							dl.LineEdit{
								AssignTo:  &searchLE,
								CueBanner: "搜索联系人昵称",
							},
							dl.TableView{
								AssignTo: &contactsTV,
								Model:    cm,
								Columns: []dl.TableViewColumn{
									{Title: "昵称", Width: 110},
									{Title: "对方消息", Width: 55},
								},
							},
							dl.Composite{
								Layout: dl.HBox{MarginsZero: true, Spacing: 4},
								Children: []dl.Widget{
									dl.PushButton{
										Text:    "补充画像…",
										MinSize: dl.Size{Width: 80, Height: 28},
										OnClicked: func() {
											idx := contactsTV.CurrentIndex()
											if idx < 0 || idx >= len(cm.items) {
												return
											}
											showSupplementDialog(cm.items[idx], func() {
												refreshContacts()
												loadContact(currentID)
											})
										},
									},
									dl.PushButton{
										Text:    "改备注…",
										MinSize: dl.Size{Width: 70, Height: 28},
										OnClicked: func() {
											idx := contactsTV.CurrentIndex()
											if idx < 0 || idx >= len(cm.items) {
												return
											}
											showRemarkDialog(cm.items[idx], func() {
												refreshContacts()
												loadContact(currentID)
											})
										},
									},
									dl.PushButton{
										AssignTo: &mergeBtn,
										Text:     "关联昵称…",
										MinSize:  dl.Size{Width: 100, Height: 28},
										OnClicked: func() {
											idx := contactsTV.CurrentIndex()
											if idx < 0 || idx >= len(cm.items) {
												return
											}
											source := cm.items[idx]
											showMergeDialog(source.ID, source.Name, func() {
												refreshContacts()
												for i, c := range cm.items {
													if c.ID == currentID {
														contactsTV.SetCurrentIndex(i)
														break
													}
												}
											})
										},
									},
									dl.CheckBox{
										AssignTo: &showMergedChk,
										Text:     "显示已合并",
										OnClicked: func() {
											refreshContacts()
										},
									},
								},
							},
						},
					},
					// 右栏：概要 + 四个页签
					dl.Composite{
						Layout: dl.VBox{Spacing: 6},
						Children: []dl.Widget{
							dl.Label{AssignTo: &summaryLabel, Text: "请选择左侧联系人", Font: fontTitle},
							dl.TabWidget{
								Pages: []dl.TabPage{
									{
										Title:  "画像",
										Layout: dl.VBox{MarginsZero: true},
										Children: []dl.Widget{
											dl.TextEdit{
												AssignTo: &sectionTE,
												ReadOnly: true,
												VScroll:  true,
												Font:     fontBody,
											},
										},
									},
									{
										Title:  "消息",
										Layout: dl.VBox{MarginsZero: true, Spacing: 4},
										Children: []dl.Widget{
											dl.Composite{
												Layout: dl.HBox{MarginsZero: true, Spacing: 6},
												Children: []dl.Widget{
													dl.Label{AssignTo: &pageLabel, Text: ""},
													dl.HSpacer{},
													dl.PushButton{AssignTo: &prevBtn, Text: "上一页",
														OnClicked: func() {
															if offset >= pageSize {
																offset -= pageSize
																loadMessages()
															}
														}},
													dl.PushButton{AssignTo: &nextBtn, Text: "下一页",
														OnClicked: func() {
															offset += pageSize
															loadMessages()
														}},
												},
											},
											dl.VSplitter{
												Children: []dl.Widget{
													dl.TableView{
														AssignTo: &messagesTV,
														Model:    mm,
														Columns: []dl.TableViewColumn{
															{Title: "发送方", Width: 50},
															{Title: "内容", Width: 260},
															{Title: "时间", Width: 120},
														},
													},
													dl.GroupBox{
														Title:  "完整内容",
														Layout: dl.VBox{MarginsZero: true},
														Children: []dl.Widget{
															dl.TextEdit{
																AssignTo: &msgDetailTE,
																ReadOnly: true,
																VScroll:  true,
																Font:     fontBody,
															},
														},
													},
												},
											},
										},
									},
									{
										Title:  "历史",
										Layout: dl.VBox{MarginsZero: true},
										Children: []dl.Widget{
											dl.VSplitter{
												Children: []dl.Widget{
													dl.ListBox{AssignTo: &historyLV, Model: hm},
													dl.TextEdit{
														AssignTo: &historyDetailTE,
														ReadOnly: true,
														VScroll:  true,
														Font:     fontBody,
													},
												},
											},
										},
									},
									{
										Title:  "统计",
										Layout: dl.VBox{MarginsZero: true, Spacing: 6},
										Children: []dl.Widget{
											dl.GroupBox{
												Title:  "消息统计",
												Layout: dl.VBox{Spacing: 4},
												Children: []dl.Widget{
													statRow("消息总数", &statTotalVal),
													statRow("我发送", &statMineVal),
													statRow("对方发送", &statOtherVal),
												},
											},
											dl.GroupBox{
												Title:  "时间范围",
												Layout: dl.VBox{Spacing: 4},
												Children: []dl.Widget{
													statRow("最早消息", &statFirstVal),
													statRow("最近消息", &statLastVal),
													statRow("画像上次更新", &statProfileVal),
												},
											},
											dl.GroupBox{
												Title:  "昵称",
												Layout: dl.VBox{Spacing: 4},
												Children: []dl.Widget{
													statRow("历史昵称（别名）", &statAliasVal),
												},
											},
											dl.VSpacer{},
										},
									},
								},
							},
						},
					},
				},
			},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						Text:      "关闭",
						MinSize:   dl.Size{Width: 80, Height: 30},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}).Create(mainWindow); err != nil {
		showError("创建画像窗口失败: " + err.Error())
		return
	}
	setTopMost(dlg.Handle())

	// 选中联系人变化时刷新右侧
	contactsTV.CurrentIndexChanged().Attach(func() {
		idx := contactsTV.CurrentIndex()
		if idx < 0 || idx >= len(cm.items) {
			return
		}
		loadContact(cm.items[idx].ID)
	})

	// 历史页：版本切换
	historyLV.CurrentIndexChanged().Attach(func() {
		idx := historyLV.CurrentIndex()
		if idx < 0 || idx >= len(hm.items) {
			return
		}
		renderHistory(hm.items[idx])
	})

	// 消息页：选中某条消息时下方显示完整内容
	messagesTV.CurrentIndexChanged().Attach(func() {
		idx := messagesTV.CurrentIndex()
		if idx < 0 || idx >= len(mm.items) {
			msgDetailTE.SetText("")
			return
		}
		m := mm.items[idx]
		who := "对方"
		if m.Sender == "me" {
			who = "我"
		}
		ts := ""
		if !m.Timestamp.IsZero() {
			ts = m.Timestamp.Format("2006-01-02 15:04:05")
		}
		msgDetailTE.SetText(fmt.Sprintf("%s  %s\r\n\r\n%s", who, ts, m.Content))
	})

	// 搜索过滤
	searchLE.TextChanged().Attach(func() {
		q := strings.TrimSpace(searchLE.Text())
		var shown []Contact
		for _, c := range contacts {
			if q == "" || strings.Contains(c.Name, q) {
				shown = append(shown, c)
			}
		}
		cm.set(shown)
		cm.PublishRowsReset()
	})

	// 默认选中指定联系人（否则选第一个）
	selectIdx := 0
	if targetContactID > 0 {
		selectIdx = -1
		for i, c := range cm.items {
			if c.ID == targetContactID {
				selectIdx = i
				break
			}
		}
		if selectIdx < 0 && len(cm.items) > 0 {
			selectIdx = 0
		}
	}
	if selectIdx >= 0 && selectIdx < len(cm.items) {
		contactsTV.SetCurrentIndex(selectIdx)
	}

	dlg.Run()
}

// profileFullText 把画像渲染成结构化全量文本：所有分区一次铺开，一眼看全
func profileFullText(p *Profile) string {
	var b strings.Builder
	writeSection := func(title, content string) {
		if strings.TrimSpace(content) == "" {
			content = "暂无记录"
		}
		fmt.Fprintf(&b, "【%s】\r\n%s\r\n\r\n", title, content)
	}
	writeSection("概要", p.Summary)
	writeSection("基本信息", formatBasicInfo(p.BasicInfo))
	writeSection("性格特征", formatList(p.Personality))
	writeSection("沟通风格", formatCommunicationStyle(p.CommunicationStyle))
	writeSection("兴趣爱好", formatList(p.Interests))
	writeSection("情绪模式", formatEmotionalPatterns(p.EmotionalPatterns))
	writeSection("关系", formatRelationship(p.Relationship))
	writeSection("典型意图", formatIntentPatterns(p.IntentPatterns))
	writeSection("重要事实", formatList(p.ImportantFacts))
	return b.String()
}

// statRow 统计页的一行：左标签右值
func statRow(label string, assignTo **walk.Label) dl.Composite {
	return dl.Composite{
		Layout: dl.HBox{MarginsZero: true, Spacing: 4},
		Children: []dl.Widget{
			dl.Label{Text: label + "：", Font: fontSection, MinSize: dl.Size{Width: 110}},
			dl.Label{AssignTo: assignTo, Text: "-", Font: fontBody},
		},
	}
}

// formatBasicInfo 格式化基本信息
func formatBasicInfo(b BasicInfo) string {
	var parts []string
	if b.Occupation != "" {
		parts = append(parts, "职业："+b.Occupation)
	}
	if b.Location != "" {
		parts = append(parts, "所在地："+b.Location)
	}
	if len(b.ImportantDates) > 0 {
		parts = append(parts, "重要日子：\r\n"+formatList(b.ImportantDates))
	}
	return strings.Join(parts, "\r\n")
}

// formatCommunicationStyle 格式化沟通风格
func formatCommunicationStyle(c CommunicationStyle) string {
	var parts []string
	if c.ReplyLength != "" {
		parts = append(parts, "回复长短："+c.ReplyLength)
	}
	if c.Tone != "" {
		parts = append(parts, "语气："+c.Tone)
	}
	if len(c.FrequentPhrases) > 0 {
		parts = append(parts, "口头禅：\r\n"+formatList(c.FrequentPhrases))
	}
	if c.EmojiUsage != "" {
		parts = append(parts, "表情使用："+c.EmojiUsage)
	}
	if c.Initiative != "" {
		parts = append(parts, "主动程度："+c.Initiative)
	}
	return strings.Join(parts, "\r\n")
}

// formatEmotionalPatterns 格式化情绪模式
func formatEmotionalPatterns(e EmotionalPatterns) string {
	var parts []string
	if len(e.Stressors) > 0 {
		parts = append(parts, "压力源：\r\n"+formatList(e.Stressors))
	}
	if len(e.ComfortTopics) > 0 {
		parts = append(parts, "安慰有效话题：\r\n"+formatList(e.ComfortTopics))
	}
	if e.WhenUpset != "" {
		parts = append(parts, "不高兴时的表现："+e.WhenUpset)
	}
	return strings.Join(parts, "\r\n")
}

// formatRelationship 格式化关系
func formatRelationship(r Relationship) string {
	var parts []string
	if r.Closeness != "" {
		parts = append(parts, "亲密程度："+r.Closeness)
	}
	if len(r.RecentEvents) > 0 {
		parts = append(parts, "近期共同事件：\r\n"+formatList(r.RecentEvents))
	}
	if r.InteractionPattern != "" {
		parts = append(parts, "互动模式："+r.InteractionPattern)
	}
	return strings.Join(parts, "\r\n")
}

// formatIntentPatterns 格式化意图模式
func formatIntentPatterns(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	// 按 key 排序保证输出稳定
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s：%s\r\n", k, m[k])
	}
	return b.String()
}

// showError 弹出错误提示框
func showError(msg string) {
	walk.MsgBox(mainWindow, "错误", msg, walk.MsgBoxIconError)
}
