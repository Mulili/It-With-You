package ui

import (
	"runtime"

	"github.com/energye/systray"
)

// TrayOptions 描述托盘图标、提示文案与菜单回调。
//
// 回调都在托盘自己的 goroutine 中触发，实现方需保证并发安全
// （App 侧的方法最终只调用 Wails runtime，本身是并发安全的）。
type TrayOptions struct {
	// Icon 是图标字节内容。Windows 下必须是 .ico，png 会加载失败。
	Icon    []byte
	Tooltip string

	OnToggleWindow func()
	OnSay          func()
	OnQuit         func()
}

// StartTray 在独立 goroutine 中启动系统托盘，立即返回。
//
// 这里有两个容易踩的坑，都是有意为之：
//
//  1. 托盘必须跑在后台 goroutine。Wails 和 systray 都想要主线程（各自的 OS 消息循环），
//     串行调用会让程序在启动时卡死。
//
//  2. 必须 LockOSThread。systray 在 Windows 上的流程是「先在当前线程 CreateWindowEx
//     建一个隐藏窗口，再用 GetMessage(0, ...) 收消息」，而 GetMessage 只收取
//     *本线程* 的消息队列。Go 的 goroutine 随时可能被调度到另一个 OS 线程上，
//     一旦建窗口和收消息落在不同线程，就会出现「图标显示正常，但右键菜单点不动」的
//     诡异现象。锁定线程后，二者必定在同一个线程上。
//
// 托盘 goroutine 会一直持有该 OS 线程直到进程退出，属预期行为。
func StartTray(opt TrayOptions) {
	go func() {
		runtime.LockOSThread()
		systray.Run(func() { trayReady(opt) }, func() {})
	}()
}

// StopTray 关闭托盘：移除图标并结束其消息循环。可安全重复调用。
func StopTray() {
	systray.Quit()
}

func trayReady(opt TrayOptions) {
	systray.SetIcon(opt.Icon)
	systray.SetTooltip(opt.Tooltip)

	// 左键单击图标：直接切换窗口显隐，比右键进菜单更快
	systray.SetOnClick(func(systray.IMenu) {
		if opt.OnToggleWindow != nil {
			opt.OnToggleWindow()
		}
	})

	// 右键菜单：不设置 SetOnRClick，库会默认弹出下面的菜单
	menuToggle := systray.AddMenuItem("显示 / 隐藏", "显示或隐藏桌面伴侣窗口")
	menuToggle.Click(func() {
		if opt.OnToggleWindow != nil {
			opt.OnToggleWindow()
		}
	})

	systray.AddSeparator()

	menuSay := systray.AddMenuItem("让它说句话", `触发一次 Say("你好")`)
	menuSay.Click(func() {
		if opt.OnSay != nil {
			opt.OnSay()
		}
	})

	systray.AddSeparator()

	menuQuit := systray.AddMenuItem("退出", "退出桌面伴侣")
	menuQuit.Click(func() {
		if opt.OnQuit != nil {
			opt.OnQuit()
		}
	})
}
