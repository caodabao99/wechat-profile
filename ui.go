package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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

// spiGetWorkArea 对应 Win32 的 SPI_GETWORKAREA（lxn/win 未导出该常量）
const spiGetWorkArea = 0x0030

// makeDialogResizable 让固定大小的 walk 对话框支持拖拽边框缩放和最大化，
// 并把初始尺寸钳制在屏幕工作区内、重新居中。
// 编辑画像/画像变化/意图分析这类弹窗固定 680~820px 高，在 1366×768 的笔记本
// 或开启 125%/150% DPI 缩放的屏幕上会超出屏幕，底部的保存/刷新按钮被切掉点不到；
// 开启缩放后用户可以把窗口拉到舒服的尺寸，内部 ScrollView/TextEdit 会自动重排。
func makeDialogResizable(dlg *walk.Dialog) {
	hwnd := dlg.Handle()
	style := win.GetWindowLong(hwnd, win.GWL_STYLE)
	win.SetWindowLong(hwnd, win.GWL_STYLE, style|win.WS_THICKFRAME|win.WS_MAXIMIZEBOX)
	win.SetWindowPos(hwnd, 0, 0, 0, 0, 0,
		win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_NOACTIVATE|win.SWP_FRAMECHANGED)

	// 工作区（排除任务栏），物理像素
	var rc win.RECT
	win.SystemParametersInfo(spiGetWorkArea, 0, unsafe.Pointer(&rc), 0)
	const margin = 40 // 与屏幕边缘留出的余量，避免宽高正好等于工作区时贴边
	maxW := int(rc.Right-rc.Left) - margin
	maxH := int(rc.Bottom-rc.Top) - margin
	// 顶层对话框的 BoundsPixels 即屏幕坐标物理像素
	b := dlg.BoundsPixels()
	w, h := b.Width, b.Height
	if w > maxW {
		w = maxW
	}
	if h > maxH {
		h = maxH
	}
	if w != b.Width || h != b.Height || b.X < int(rc.Left) || b.Y < int(rc.Top) {
		x := int(rc.Left) + (int(rc.Right-rc.Left)-w)/2
		y := int(rc.Top) + (int(rc.Bottom-rc.Top)-h)/2
		_ = dlg.SetBoundsPixels(walk.Rectangle{X: x, Y: y, Width: w, Height: h})
	}
}

// createFitDialog 创建「可滚动内容 + 固定底部按钮行」的弹窗骨架。
// ScrollView 关闭横向滚动（HorizontalFixed）：内容永远按可用宽度换行，
// 不会再冒出横向滚动条；宽度超出屏幕的极端情况下才可能裁切。
// 配合 CompactHeight 的 TextEdit，内容会按文字行数撑高。
// 返回 dlg 与 sv，调用方接着调 armAutoFitScroll，最后 dlg.Run()。
func createFitDialog(title string, width96, height96 int, body, bottomBar []dl.Widget) (*walk.Dialog, *walk.ScrollView) {
	var dlg *walk.Dialog
	var sv *walk.ScrollView
	children := []dl.Widget{
		dl.ScrollView{
			AssignTo:        &sv,
			HorizontalFixed: true,
			Layout:          dl.VBox{Spacing: 8},
			Children:        body,
		},
	}
	if len(bottomBar) > 0 {
		children = append(children, dl.Composite{
			Layout:   dl.HBox{MarginsZero: true, Spacing: 8},
			Children: bottomBar,
		})
	}
	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    title,
		// 高度下限给小一点：开屏实际高度由 armAutoFitScroll 按内容测量后决定
		MinSize:  dl.Size{Width: width96, Height: 240},
		Size:     dl.Size{Width: width96, Height: height96},
		Layout:   dl.VBox{Spacing: 8},
		Children: children,
	}).Create(mainWindow); err != nil {
		showError("创建窗口失败: " + err.Error())
		return nil, nil
	}
	return dlg, sv
}

// armAutoFitScroll 让弹窗在 Run/Show 开屏时自动开到「刚好完整显示内容」的高度：
//  1. 先借 walk 在 WM_ENTERSIZEMOVE 期间的同步布局通道强制布局一遍
//     （此刻窗口还没显示，用户看不到过程），拿到 CompactHeight 控件按文字
//     换行后的真实位置；
//  2. 据内容总高设置窗口最小高度——Dialog.Show 会把窗口开到这个高度，
//     屏幕放不下时钳制到工作区高度（内部滚动保留）；
//  3. 首次 SizeChanged 时重新居中，并解除最小高度锁定，之后用户可自由拖小。
//
// 没有这个机制时，ScrollView 不向父布局汇报内容高度，Dialog.Show 会把窗口
// 缩到 MinSize，底部按钮和单选框被挤出可视区，用户每次都得手动拖长窗口。
func armAutoFitScroll(dlg *walk.Dialog, sv *walk.ScrollView, width96, floorH96 int) {
	if dlg == nil || sv == nil {
		return
	}
	hwnd := dlg.Handle()
	style := win.GetWindowLong(hwnd, win.GWL_STYLE)
	win.SetWindowLong(hwnd, win.GWL_STYLE, style|win.WS_THICKFRAME|win.WS_MAXIMIZEBOX)
	win.SetWindowPos(hwnd, 0, 0, 0, 0, 0,
		win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_NOACTIVATE|win.SWP_FRAMECHANGED)

	// walk 在收到 WM_ENTERSIZEMOVE 后会把布局结果改为同步等待，
	// 这样下面的 SetBoundsPixels 返回时内部布局已经算完，可以直接读坐标。
	// 尺寸必须真的变化才会触发 WM_WINDOWPOSCHANGED 里的布局，所以先撑 2px 再还原。
	win.SendMessage(hwnd, win.WM_ENTERSIZEMOVE, 0, 0)
	b0 := dlg.BoundsPixels()
	dlg.SetBoundsPixels(walk.Rectangle{X: b0.X, Y: b0.Y, Width: b0.Width, Height: b0.Height + 2})
	dlg.SetBoundsPixels(b0)
	win.SendMessage(hwnd, win.WM_EXITSIZEMOVE, 0, 0)

	svClient := sv.ClientBoundsPixels()

	// 内容总高：滚动区里最后一个控件的底边 + VBox 下侧留白（默认 9 设计像素）
	contentBottom := svClient.Y
	kids := sv.Children()
	for i := 0; i < kids.Len(); i++ {
		b := kids.At(i).BoundsPixels()
		if b.Y+b.Height > contentBottom {
			contentBottom = b.Y + b.Height
		}
	}
	contentH := contentBottom - svClient.Y + sv.IntFrom96DPI(9)

	// 工作区（排除任务栏），PerMonitorV2 进程拿到的是物理像素
	var rc win.RECT
	win.SystemParametersInfo(spiGetWorkArea, 0, unsafe.Pointer(&rc), 0)
	const edge96 = 24
	edge := dlg.IntFrom96DPI(edge96)
	maxW := int(rc.Right-rc.Left) - edge
	maxH := int(rc.Bottom-rc.Top) - edge

	cur := dlg.BoundsPixels()
	wantH := cur.Height + (contentH - svClient.Height)
	if floor := dlg.IntFrom96DPI(floorH96); wantH < floor {
		wantH = floor
	}
	if wantH > maxH {
		wantH = maxH
	}
	wantW := cur.Width
	if wantW > maxW {
		wantW = maxW
	}

	// Dialog.Show 取 max(布局最小尺寸, MinSizePixels)；ScrollView 不汇报高度，
	// 所以把开屏高度作为最小高度喂给它，宽度同样以设计宽度为下限
	dlg.SetMinMaxSizePixels(walk.Size{Width: dlg.IntFrom96DPI(width96), Height: wantH}, walk.Size{})

	armed := true
	dlg.SizeChanged().Attach(func() {
		if !armed {
			return
		}
		armed = false
		b := dlg.BoundsPixels()
		w, h := b.Width, b.Height
		if w > maxW {
			w = maxW
		}
		if h > maxH {
			h = maxH
		}
		x := int(rc.Left) + (int(rc.Right-rc.Left)-w)/2
		y := int(rc.Top) + (int(rc.Bottom-rc.Top)-h)/2
		// 开屏高度锁定解除：允许用户之后把窗口拖小，内容由 ScrollView 兜底
		dlg.SetMinMaxSizePixels(walk.Size{
			Width:  dlg.IntFrom96DPI(480),
			Height: dlg.IntFrom96DPI(floorH96),
		}, walk.Size{})
		if w != b.Width || h != b.Height || x != b.X || y != b.Y {
			_ = dlg.SetBoundsPixels(walk.Rectangle{X: x, Y: y, Width: w, Height: h})
		}
	})
}

// presentResultDialog 意图分析结果窗公共外壳（本地/远程两种数据来源共用）。
// body 为滚动区内的卡片控件；confidenceVal < 0 表示不显示置信度行。
func presentResultDialog(contactID int64, body []dl.Widget, confidenceVal int) {
	var dlg *walk.Dialog
	var confidencePB *walk.ProgressBar

	if confidenceVal >= 0 {
		body = append(body, dl.Composite{
			Layout: dl.HBox{MarginsZero: true, Spacing: 8},
			Children: []dl.Widget{
				dl.Label{Text: "置信度:", Font: fontSection},
				dl.ProgressBar{AssignTo: &confidencePB, MinSize: dl.Size{Width: 200, Height: 20}},
				dl.Label{Text: confidenceText(confidenceVal), Font: fontBody},
			},
		})
	}

	bottomBar := []dl.Widget{
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
	}

	var sv *walk.ScrollView
	dlg, sv = createFitDialog("意图分析结果", 680, 820, body, bottomBar)
	if dlg == nil {
		return
	}
	setTopMost(dlg.Handle())
	armAutoFitScroll(dlg, sv, 680, 360)

	if confidencePB != nil && confidenceVal > 0 {
		confidencePB.SetValue(confidenceVal)
	}
	dlg.Run()
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
		// 浮窗刻意做小：120×80（96dpi 逻辑尺寸，高 DPI 屏由 walk 自动放大）
		Size:    dl.Size{Width: 120, Height: 80},
		MinSize: dl.Size{Width: 120, Height: 80},
		MaxSize: dl.Size{Width: 120, Height: 80},
		Layout:  dl.VBox{MarginsZero: true, Spacing: 2},
		Children: []dl.Widget{
			dl.PushButton{
				AssignTo:  &identifyBtn,
				Text:      "识 别",
				Font:      fontSection,
				MinSize:   dl.Size{Width: 110, Height: 26},
				OnClicked: onIdentifyClicked,
			},
			dl.PushButton{
				AssignTo: &profileBtn,
				Text:     "画 像",
				Font:     fontBody,
				MinSize:  dl.Size{Width: 110, Height: 22},
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
	//
	// 窗口销毁时必须停下：否则这个 goroutine 会一直往已经退出的消息循环里
	// Synchronize，操作一个已释放的 HWND。
	stopTopMost := make(chan struct{})
	mainWindow.Disposing().Attach(func() { close(stopTopMost) })
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTopMost:
				return
			case <-ticker.C:
			}
			if mainWindow.IsDisposed() {
				return
			}
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

	// 逻辑尺寸（96dpi）换算到当前显示器的物理像素；
	// DPI 感知生效前进程会被系统位图拉伸，窗口会异常巨大且发虚，
	// 声明 PerMonitorV2 后这里拿到的才是真实物理分辨率。
	const winW96, winH96, margin96 = 120, 80, 20
	winW := mainWindow.IntFrom96DPI(winW96)
	winH := mainWindow.IntFrom96DPI(winH96)
	margin := mainWindow.IntFrom96DPI(margin96)
	cx := int(win.GetSystemMetrics(win.SM_CXSCREEN))
	cy := int(win.GetSystemMetrics(win.SM_CYSCREEN))
	x := cx - winW - margin
	y := cy * 2 / 3
	if y+winH > cy {
		y = cy - winH - margin
	}
	if y < 0 {
		y = 0
	}
	win.SetWindowPos(hwnd, win.HWND_TOPMOST, int32(x), int32(y), int32(winW), int32(winH), win.SWP_NOACTIVATE)
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

		if isRemoteMode {
			onIdentifyRemote(text, &finalStatus)
			return
		}
		onIdentifyLocal(text, &finalStatus)
	}()
}

// onIdentifyRemote 远程模式：全部交给 bot 服务端处理
func onIdentifyRemote(text string, finalStatus *string) {
	result, err := remoteClient.Ingest(text, true)
	if err != nil {
		// 网络/服务端问题，重试同样的内容有意义：放开去重，不用再复制一遍
		ResetClipboardHash()
		mainWindow.Synchronize(func() { showError("识别失败: " + err.Error()) })
		return
	}

	if result.IntentError != "" {
		ResetClipboardHash()
	}

	if result.ViaAlias {
		*finalStatus = "旧昵称已归位"
	}
	if result.ProfileTriggered {
		*finalStatus = "已识别，画像更新中"
	}

	mainWindow.Synchronize(func() {
		ShowResultWindowRemote(result)
	})
}

// onIdentifyLocal 本地模式：走本地 SQLite + 本地 LLM
func onIdentifyLocal(text string, finalStatus *string) {
	// 2. 解析聊天记录
	messages := ParseClipboard(text, config.MyName)
	if len(messages) == 0 {
		*finalStatus = "未解析到消息"
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
	contactID, viaAlias, err := doResolveContact(contactName)
	if err != nil {
		ResetClipboardHash() // 落库失败（磁盘/网络），重试同样内容有意义
		mainWindow.Synchronize(func() { showError("写入联系人失败: " + err.Error()) })
		return
	}
	if viaAlias {
		*finalStatus = "旧昵称已归位"
	}
	newCount, err := doSaveMessages(contactID, messages)
	if err != nil {
		ResetClipboardHash()
		mainWindow.Synchronize(func() { showError("保存消息失败: " + err.Error()) })
		return
	}

	// 5. 画像生成/更新与意图分析并行：画像异步后台跑，不拖慢结果弹出。
	if doShouldGenerateProfile(contactID) || doShouldUpdateProfile(contactID) {
		go func() {
			if err := doGenerateOrUpdateProfile(contactID, contactName, messages); err != nil {
				_ = doSaveProfileHistory(contactID, "{}", "画像生成失败: "+err.Error())
			}
		}()
	}

	// 6. 取对方最后一条消息做意图分析（本次复制的消息）
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

	result, analysisErr := doAnalyzeIntent(contactID, latestOther)
	if analysisErr != nil {
		ResetClipboardHash()
	}
	mainWindow.Synchronize(func() {
		ShowResultWindow(contactID, contactName, newCount, viaAlias, result, analysisErr)
	})
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

// parseConfidence 把模型返回的置信度统一成 0~100 的整数。
//
// 模型有时给 0.85，有时给 85，两种都归一成 85；解析不出来返回 -1，
// 界面显示"暂无"而不是一个误导性的 0%。
func parseConfidence(raw string) int {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%g", &f); err != nil {
		return -1
	}
	if f > 0 && f <= 1.0 {
		f *= 100
	}
	v := int(f + 0.5)
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return v
}

// confidenceText 置信度的界面文案
func confidenceText(v int) string {
	if v < 0 {
		return "暂无"
	}
	return fmt.Sprintf("%d%%", v)
}

// ShowResultWindow 弹出意图分析结果窗口（卡片式布局）。
// 卡片 TextEdit 全部 CompactHeight：按文字行数自己撑高，窗口再由
// presentResultDialog 按内容总高开到刚好显示全，不再需要手动拖长。
func ShowResultWindow(contactID int64, contactName string, newCount int, viaAlias bool, result map[string]interface{}, analysisErr error) {
	// -1 表示这次没有置信度可显示（分析失败或模型没给）
	confidenceVal := -1

	var headerText string
	if viaAlias {
		headerText = fmt.Sprintf("%s · 本次新增 %d 条 · 旧昵称已归位", contactName, newCount)
	} else {
		headerText = fmt.Sprintf("%s · 本次新增 %d 条", contactName, newCount)
	}

	body := []dl.Widget{
		dl.Label{Text: headerText, Font: fontTitle},
	}

	if analysisErr != nil {
		body = append(body,
			dl.GroupBox{
				Title:  "分析失败",
				Layout: dl.VBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.TextEdit{
						ReadOnly:      true,
						CompactHeight: true,
						Text:          analysisErr.Error(),
					},
				},
			},
		)
		presentResultDialog(contactID, body, confidenceVal)
		return
	}

	// 四个分析卡片 + N 条建议回复卡片
	cardKeys := []struct {
		title string
		key   string
	}{
		{"表面意思", "surface"},
		{"潜在意图", "intent"},
		{"情绪状态", "emotion"},
		{"潜台词", "subtext"},
	}
	for i, reply := range suggestedReplyItems(result) {
		key := fmt.Sprintf("suggested_reply_%d", i)
		result[key] = reply.Text
		cardKeys = append(cardKeys, struct{ title, key string }{reply.Style, key})
	}

	for _, c := range cardKeys {
		content := fieldString(result, c.key)
		if strings.HasPrefix(c.key, "suggested_reply_") {
			var suggestionTE *walk.TextEdit
			controls := rewriteControls(contactID, &suggestionTE)
			body = append(body, controls,
				dl.GroupBox{
					Title:  c.title,
					Layout: dl.VBox{MarginsZero: true, Spacing: 4},
					Children: []dl.Widget{
						dl.Composite{
							Layout: dl.HBox{MarginsZero: true, Spacing: 4},
							Children: []dl.Widget{
								dl.TextEdit{
									AssignTo:      &suggestionTE,
									ReadOnly:      true,
									CompactHeight: true,
									Text:          content,
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
			body = append(body,
				dl.GroupBox{
					Title:  c.title,
					Layout: dl.VBox{MarginsZero: true},
					Children: []dl.Widget{
						dl.TextEdit{
							ReadOnly:      true,
							CompactHeight: true,
							Text:          content,
						},
					},
				},
			)
		}
	}

	// 置信度：标签初值就必须是格式化好的百分比，
	// 否则模型没给置信度时界面上会一直停着原始的 "0.85"。
	confidenceVal = parseConfidence(fieldString(result, "confidence"))
	presentResultDialog(contactID, body, confidenceVal)
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
		// 合并状态：被吸收 / 吸收过别人
		if c.MergedInto != 0 {
			return "已并入"
		}
		if c.MergeCount > 0 {
			return fmt.Sprintf("含%d人", c.MergeCount)
		}
		return ""
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
	summary := h.ChangeSummary
	// 列表只显示一行，超长的截断显示，完整内容在详情面板里看
	if r := []rune(summary); len(r) > 40 {
		summary = string(r[:40]) + "…"
	}
	// CreatedAt 存的是 RFC3339（2026-10-01T14:03:22+08:00），直接拼进列表
	// 又长又带时区，用 displayTime 统一成 "2026-10-01 14:03:22"
	return fmt.Sprintf("[%s] %s", orUnknown(displayTime(h.CreatedAt)), summary)
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

// matchContact 搜索框的匹配规则：q 必须已经小写化。
// 昵称、备注、历史昵称（别名）任一命中即算匹配。
func matchContact(c Contact, q string) bool {
	if strings.Contains(strings.ToLower(c.Name), q) ||
		strings.Contains(strings.ToLower(c.Remark), q) {
		return true
	}
	for _, a := range c.Aliases {
		if strings.Contains(strings.ToLower(a), q) {
			return true
		}
	}
	return false
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

	contacts, err := fetchContacts(false)
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
		page, err := fetchMessagesPage(currentID, offset, pageSize)
		if err != nil {
			// 远程模式下这就是网络/鉴权失败，静默 return 会让界面停在
			// 上一个联系人的消息上，用户以为「这个人没有聊天记录」
			showError("读取消息失败: " + err.Error())
			return
		}
		mm.set(page)
		mm.PublishRowsReset()
		pageLabel.SetText(fmt.Sprintf("第 %d 页（最新在前）", offset/pageSize+1))
		prevBtn.SetEnabled(offset > 0)
		nextBtn.SetEnabled(len(page) >= pageSize)
	}

	// applyFilter 按搜索框内容过滤联系人列表（同时匹配昵称和备注）。
	// 在 UI 构建时赋值，refreshContacts 也会调用它，保证刷新后搜索条件不丢失。
	var applyFilter func()

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
		// 有些历史记录只是「事件日志」，没有画像快照（比如画像生成失败、
		// 改名、合并等），ProfileJSON 是空串或 "{}"。这种要直接说明，
		// 否则会渲染出一整页「暂无记录」，看着像数据坏了。
		pj := strings.TrimSpace(h.ProfileJSON)
		if pj == "" || pj == "{}" {
			historyDetailTE.SetText("该版本没有画像快照\r\n\r\n" + orUnknown(h.ChangeSummary))
			return
		}
		var p Profile
		if jsonErr := json.Unmarshal([]byte(pj), &p); jsonErr != nil {
			historyDetailTE.SetText("历史画像解析失败：\r\n" + pj)
			return
		}
		historyDetailTE.SetText(profileFullText(&p))
	}

	clearContact := func() {
		currentID = 0
		currentContact = nil
		offset = 0
		summaryLabel.SetText("请选择左侧联系人")
		renderProfile()
		mm.set(nil)
		mm.PublishRowsReset()
		msgDetailTE.SetText("")
		hm.set(nil)
		hm.PublishItemsReset()
		historyDetailTE.SetText("")
		pageLabel.SetText("")
		prevBtn.SetEnabled(false)
		nextBtn.SetEnabled(false)
		for _, lb := range []*walk.Label{statTotalVal, statMineVal, statOtherVal,
			statFirstVal, statLastVal, statProfileVal, statAliasVal} {
			lb.SetText("-")
		}
	}

	// loadContact 刷新右侧全部页签
	loadContact := func(id int64) {
		clearContact()
		if id <= 0 {
			return
		}
		currentID = id
		contact, err := fetchContactByID(id)
		if err != nil {
			showError("读取联系人详情失败: " + err.Error())
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
		history, herr := fetchHistory(id, 100)
		hm.set(history)
		hm.PublishItemsReset()
		switch {
		case herr != nil:
			// 不能和「暂无画像历史」混为一谈：那是没数据，这是取数据失败
			historyDetailTE.SetText("读取画像历史失败: " + herr.Error())
		case len(history) > 0:
			historyLV.SetCurrentIndex(0)
			renderHistory(history[0])
		default:
			historyDetailTE.SetText("暂无画像历史")
		}

		// 统计页。统计只是辅助信息，取失败时不弹模态框打断选人的操作，
		// 直接在面板上写明，避免显示成一排 "0 条 / 暂无" 让人误判。
		stats, serr := fetchStats(contact.ID)
		aliases, aerr := fetchAliases(contact.ID)
		if serr != nil || aerr != nil {
			for _, lb := range []*walk.Label{statTotalVal, statMineVal, statOtherVal,
				statFirstVal, statLastVal, statProfileVal, statAliasVal} {
				if lb != nil {
					lb.SetText("读取失败")
				}
			}
			return
		}
		aliasStr := strings.Join(aliases, "、")
		if len(aliases) == 0 {
			aliasStr = "无"
		}
		statTotalVal.SetText(fmt.Sprintf("%d 条", stats.Total))
		statMineVal.SetText(fmt.Sprintf("%d 条", stats.Mine))
		statOtherVal.SetText(fmt.Sprintf("%d 条", stats.Other))
		statFirstVal.SetText(orUnknown(displayTime(stats.FirstTime)))
		statLastVal.SetText(orUnknown(displayTime(stats.LastTime)))
		statProfileVal.SetText(orUnknown(displayTime(contact.LastUpdated)))
		statAliasVal.SetText(aliasStr)
	}

	// 刷新联系人列表：更新闭包共享的 contacts，再重新应用搜索过滤
	// （旧实现用 := 声明了同名局部变量，外层 contacts 不变，导致搜索基于初始快照，
	//  刚合并掉的联系人会在搜索结果里复活）
	refreshContacts := func() {
		showMerged := showMergedChk.Checked()
		list, err := fetchContacts(showMerged)
		if err != nil {
			// 静默 return 会让列表停在合并/改名之前的旧快照上，
			// 刚被合并掉的联系人看着像「没合并成功」
			showError("刷新联系人失败: " + err.Error())
			return
		}
		contacts = list
		applyFilter()
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
									{Title: "合并", Width: 45},
								},
							},
							dl.Composite{
								Layout: dl.VBox{MarginsZero: true, Spacing: 2},
								Children: []dl.Widget{
									dl.Composite{
										Layout: dl.HBox{MarginsZero: true, Spacing: 4},
										Children: []dl.Widget{
											dl.PushButton{Text: "回复前帮我看看", OnClicked: func() { showDraftDialog(currentID) }},
											dl.PushButton{Text: "画像变化", OnClicked: func() { showChangesDialog(currentID) }},
											dl.PushButton{
												Text: "编辑画像…",
												OnClicked: func() {
													idx := contactsTV.CurrentIndex()
													if idx < 0 || idx >= len(cm.items) {
														return
													}
													showEditProfileDialog(cm.items[idx].ID, func() { refreshContacts(); loadContact(currentID) })
												},
											},
											dl.PushButton{
												Text:    "补充画像…",
												MinSize: dl.Size{Width: 70, Height: 28},
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
												MinSize: dl.Size{Width: 60, Height: 28},
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
										},
									},
									dl.Composite{
										Layout: dl.HBox{MarginsZero: true, Spacing: 4},
										Children: []dl.Widget{
											dl.PushButton{
												AssignTo: &mergeBtn,
												Text:     "关联昵称…",
												MinSize:  dl.Size{Width: 70, Height: 28},
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
											dl.PushButton{
												Text:    "合并记录…",
												MinSize: dl.Size{Width: 70, Height: 28},
												OnClicked: func() {
													idx := contactsTV.CurrentIndex()
													if idx < 0 || idx >= len(cm.items) {
														return
													}
													showMergeHistoryDialog(cm.items[idx], func() {
														refreshContacts()
													})
												},
											},
											dl.Composite{
												Layout: dl.HBox{MarginsZero: true, Spacing: 4},
												Children: []dl.Widget{
													dl.PushButton{
														Text:    "删除联系人…",
														MinSize: dl.Size{Width: 82, Height: 28},
														OnClicked: func() {
															idx := contactsTV.CurrentIndex()
															if idx < 0 || idx >= len(cm.items) {
																return
															}
															c := cm.items[idx]
															// 二次确认：删除会把消息、画像、画像历史、别名、合并记录一起清掉，不可恢复
															if walk.MsgBox(dlg, "删除联系人",
																fmt.Sprintf("确定删除联系人「%s」吗？\n该联系人的消息、画像、画像历史、别名、合并记录都会被删除，不可恢复。", displayName(&c)),
																walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
																return
															}
															if err := doDeleteContact(c.ID); err != nil {
																walk.MsgBox(dlg, "删除失败", err.Error(), walk.MsgBoxIconError)
																return
															}
															// 删除后右栏还显示着被删联系人的画像，清掉避免误导
															clearContact()
															refreshContacts()
														},
													},
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
									// 第三行：备份 / 恢复（全局操作，与当前选中联系人无关）
									dl.Composite{
										Layout: dl.HBox{MarginsZero: true, Spacing: 4},
										Children: []dl.Widget{
											dl.PushButton{
												Text:    "备份…",
												MinSize: dl.Size{Width: 60, Height: 28},
												OnClicked: func() {
													fd := walk.FileDialog{
														Title:    "保存备份文件",
														Filter:   "备份文件 (*.zip)|*.zip",
														FilePath: BackupFileName(time.Now()),
													}
													ok, err := fd.ShowSave(dlg)
													if err != nil {
														walk.MsgBox(dlg, "备份失败", err.Error(), walk.MsgBoxIconError)
														return
													}
													if !ok || fd.FilePath == "" {
														return
													}
													dest := fd.FilePath
													if !strings.HasSuffix(strings.ToLower(dest), ".zip") {
														dest += ".zip"
													}
													// 可选口令：填了就把 zip 里的密钥文件加密
													pwd, ok := promptBackupPassword("设置备份密码（可留空）",
														"· 填了密码：配置里的模型 API Key 会用 AES-256 加密后放进 zip（文件名带 .enc），导入时需填同一密码\n"+
															"· 留空：和以前一样明文保存，任何机器都能直接导入\n"+
															"· 聊天数据始终是明文，zip 也仍是标准格式，可用解压软件打开查看\n"+
															"· 密码程序不会保存，忘了就再也解不开被加密的文件",
														"开始备份")
													if !ok {
														return
													}
													go func() {
														err := doExportBackup(dest, pwd)
														dlg.Synchronize(func() {
															if err != nil {
																walk.MsgBox(dlg, "备份失败", err.Error(), walk.MsgBoxIconError)
																return
															}
															msg := "已导出到：\n" + dest +
																"\n\n备份含全部聊天画像数据和配置（模型 Key），请妥善保管，换电脑时在新机导入即可。"
															if pwd != "" {
																msg += "\n\n本次已用密码加密配置文件，导入时必须填写同一密码。"
															}
															walk.MsgBox(dlg, "备份完成", msg, walk.MsgBoxIconInformation)
														})
													}()
												},
											},
											dl.PushButton{
												Text:    "恢复…",
												MinSize: dl.Size{Width: 60, Height: 28},
												OnClicked: func() {
													fd := walk.FileDialog{
														Title:  "选择备份文件",
														Filter: "备份文件 (*.zip)|*.zip",
													}
													ok, err := fd.ShowOpen(dlg)
													if err != nil {
														walk.MsgBox(dlg, "恢复失败", err.Error(), walk.MsgBoxIconError)
														return
													}
													if !ok || fd.FilePath == "" {
														return
													}
													src := fd.FilePath
													// 备份里的密钥文件若是加密的，必须先拿到密码才谈得上恢复
													var pwd string
													if BackupNeedsPassword(src) {
														p, ok := promptBackupPassword("输入备份密码",
															"这份备份导出时对密钥文件（模型 API Key 等）做了加密，请输入当时设置的密码。\n\n"+
																"· 密码不对会直接中止恢复，现有数据不会受影响\n"+
																"· 只想恢复聊天数据、不需要密钥文件时，也可以先取消，把 zip 里的 .enc 文件删掉再导入",
															"继续")
														if !ok {
															return
														}
														if p == "" {
															walk.MsgBox(dlg, "需要密码", "这份备份已加密，必须填写导出时设置的密码。", walk.MsgBoxIconWarning)
															return
														}
														pwd = p
													}
													extra := ""
													if pwd != "" {
														extra = "\n· 本次将用你输入的密码解开备份中的密钥文件"
													}
													if walk.MsgBox(dlg, "从备份恢复",
														"将用备份文件「"+filepath.Base(src)+"」整体替换当前所有联系人、消息和画像数据。\n\n"+
															"· 恢复前会自动在程序目录留一份「恢复前自动备份」\n"+
															"· 配置文件恢复后需重启程序生效"+extra+"\n\n确定继续吗？",
														walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
														return
													}
													go func() {
														summary, err := doImportBackup(src, pwd)
														dlg.Synchronize(func() {
															if err != nil {
																walk.MsgBox(dlg, "恢复失败", err.Error(), walk.MsgBoxIconError)
																return
															}
															msg := fmt.Sprintf(
																"恢复完成：\n联系人 %d 个、消息 %d 条、画像历史 %d 条、合并记录 %d 条",
																summary.Contacts, summary.Messages, summary.Histories, summary.MergeLogs)
															if len(summary.Files) > 0 {
																msg += "\n\n配置文件已恢复，重启程序后生效"
															}
															if isRemoteMode {
																msg += "\n\n数据在服务端已即时生效；服务端配置/登录凭据需重启服务"
															}
															walk.MsgBox(dlg, "恢复完成", msg, walk.MsgBoxIconInformation)
															clearContact()
															refreshContacts()
														})
													}()
												},
											},
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
														AssignTo:      &messagesTV,
														Model:         mm,
														StretchFactor: 4,
														Columns: []dl.TableViewColumn{
															{Title: "发送方", Width: 50},
															{Title: "内容", Width: 260},
															{Title: "时间", Width: 120},
														},
													},
													dl.GroupBox{
														Title:         "完整内容（单条消息，需要时可拖动上方分隔线加高）",
														StretchFactor: 1,
														Layout:        dl.VBox{MarginsZero: true},
														Children: []dl.Widget{
															dl.TextEdit{
																AssignTo: &msgDetailTE,
																ReadOnly: true,
																VScroll:  true,
																Font:     fontBody,
																MinSize:  dl.Size{Height: 76},
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
			clearContact()
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

	// 搜索过滤：昵称、备注、历史昵称都参与匹配，且忽略大小写。
	// 微信昵称里英文大小写很随意（"Amy" / "amy"），别名更是改名前的旧昵称，
	// 只按当前昵称精确大小写匹配会让人以为「这个人不在列表里」。
	applyFilter = func() {
		q := strings.ToLower(strings.TrimSpace(searchLE.Text()))
		var shown []Contact
		for _, c := range contacts {
			if q == "" || matchContact(c, q) {
				shown = append(shown, c)
			}
		}
		cm.set(shown)
		cm.PublishRowsReset()
		if idx := contactsTV.CurrentIndex(); idx < 0 || idx >= len(cm.items) {
			clearContact()
		}
	}
	searchLE.TextChanged().Attach(applyFilter)

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
	} else {
		clearContact()
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

// statRow 统计页的一行：左标签固定宽 + 右值固定宽，确保多行左对齐
func statRow(label string, assignTo **walk.Label) dl.Composite {
	return dl.Composite{
		Layout: dl.HBox{MarginsZero: true, Spacing: 4},
		Children: []dl.Widget{
			dl.Label{Text: label + "：", Font: fontSection, MinSize: dl.Size{Width: 110}},
			dl.Label{AssignTo: assignTo, Text: "-", Font: fontBody, MinSize: dl.Size{Width: 80}, TextAlignment: dl.AlignNear},
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
