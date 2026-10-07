//go:build windows

package main

// 单实例保护。
//
// 为什么必须做：桌面端是常驻托盘 + 剪贴板监听工具，双击第二次启动会造成
//   - 两个进程同时监听剪贴板 → 同一条聊天记录被识别两次、重复消耗大模型额度；
//   - 两进程各开一条连接写同一个 wechat_profile.db（WAL + SetMaxOpenConns(1)）→ 偶发 SQLITE_BUSY；
//   - 双份托盘图标/悬浮窗，用户分不清哪个是真的，进而误判「数据丢了」。
//
// 实现要点：
//   - 用 Windows 命名互斥体（CreateMutexW + ERROR_ALREADY_EXISTS）判定，进程退出后自动释放，
//     不留锁文件、不会被 kill -9 卡死（这是比 pidfile 更稳的方案）；
//   - 句柄故意不 CloseHandle，留到进程结束由系统回收——提前释放会让第三个实例误判为「无人运行」；
//   - 已存在实例时，尽力把它的主窗口带回前台，让用户立刻看到反馈，然后提示并退出；
//   - 任何一步失败都放行本次启动：保护机制绝不能反过来阻止正常使用（宁可放行，不可误杀）。
//
// Local\ 前缀 = 每个登录会话一个实例（多用户各自登录 Windows 时可同时使用，符合本机工具定位）。

import (
	"os"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
)

const singleInstanceMutexName = `Local\WeChatProfileBot.SingleInstance`

var (
	modkernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW      = modkernel32.NewProc("CreateMutexW")
	procFindWindowW       = user32.NewProc("FindWindowW")
	procSetForegroundWind = user32.NewProc("SetForegroundWindow")
	procShowWindowSW      = user32.NewProc("ShowWindow")
)

// singleInstanceHandle 持有互斥体句柄直到进程退出（不可提前 CloseHandle）。
var singleInstanceHandle uintptr

// ensureSingleInstance 必须在 main 最开头调用：早于建日志、开数据库、注册托盘，
// 这样被拒之门外的第二个进程不会留下任何副作用。
func ensureSingleInstance() {
	name, err := syscall.UTF16PtrFromString(singleInstanceMutexName)
	if err != nil {
		return
	}
	h, _, e := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return // 连句柄都拿不到：放行，不让保护逻辑挡住正常使用
	}
	singleInstanceHandle = h

	if errno, ok := e.(syscall.Errno); !ok || errno != syscall.ERROR_ALREADY_EXISTS {
		return // 首个实例：正常启动
	}

	activateExistingWindow()
	walk.MsgBox(nil, "微信人物画像助手",
		"程序已经在运行了，请看屏幕右下角的系统托盘。\n\n"+
			"重复启动会让同一条聊天记录被识别两次、重复消耗大模型额度，\n"+
			"并可能同时写入同一个数据库，因此本次启动已自动退出。",
		walk.MsgBoxIconInformation)
	os.Exit(0)
}

// activateExistingWindow 尝试把已运行实例的主窗口前置并还原；找不到就算了（不打扰用户）。
func activateExistingWindow() {
	title := "画像助手 " + appVersion
	t, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(t)))
	if hwnd == 0 {
		return
	}
	const swRestore = 9
	procShowWindowSW.Call(hwnd, swRestore)
	procSetForegroundWind.Call(hwnd)
}
