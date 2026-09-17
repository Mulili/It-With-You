package ui

import (
	"context"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Window 封装主窗口的显示与隐藏。
//
// 为什么要自己维护可见状态：Wails v2 只提供 WindowShow / WindowHide，
// 没有「查询窗口是否可见」的 API，而托盘上的「显示 / 隐藏」需要一个方向正确的开关。
//
// 约定：前端的显示 / 隐藏也必须走这里（App.ShowWindow / HideWindow），
// 不要在 JS 里直接调 runtime 的 WindowShow / WindowHide，否则状态会与实际不一致。
type Window struct {
	ctx context.Context

	mu      sync.Mutex
	visible bool
	// expanded 记录菜单是否处于展开态；baseW / baseH 是展开前的窗口尺寸。
	expanded     bool
	baseW, baseH int
}

// NewWindow 创建窗口控制器。窗口在 Wails 启动后默认是可见的。
func NewWindow(ctx context.Context) *Window {
	return &Window{ctx: ctx, visible: true}
}

// Visible 返回当前记录的可见状态。
func (w *Window) Visible() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.visible
}

// Show 显示窗口（幂等）。
func (w *Window) Show() {
	w.mu.Lock()
	w.visible = true
	w.mu.Unlock()
	runtime.WindowShow(w.ctx)
}

// Hide 隐藏窗口（幂等），窗口从任务栏消失但进程与托盘仍在。
func (w *Window) Hide() {
	w.mu.Lock()
	w.visible = false
	w.mu.Unlock()
	runtime.WindowHide(w.ctx)
}

// Toggle 切换显示 / 隐藏，返回切换后的状态。
func (w *Window) Toggle() bool {
	if w.Visible() {
		w.Hide()
		return false
	}
	w.Show()
	return true
}

// menuExtraHeight 是历史菜单展开时额外增加的窗口高度。
const menuExtraHeight = 260

// SetMenuOpen 展开 / 收起历史菜单时临时调整窗口尺寸。
//
// 向上扩展而不是向下：桌宠贴着窗口底部，若向下扩展，桌宠在屏幕上的位置会跟着下移、
// 收起时又弹回来，看着像窗口在跳。保持「下边缘不动」则桌宠纹丝不动，菜单从上方长出来。
//
// 已知限制：Wails v2 的 runtime.Screen 只提供屏幕尺寸、不提供屏幕坐标，
// 因此无法判断窗口是否贴着屏幕顶边，钳制不了。若贴顶时观感不好，
// 把下面两行 WindowSetPosition 删掉即可退化成「向下扩展」（无需钳制，代价是桌宠会下移）。
func (w *Window) SetMenuOpen(open bool) {
	w.mu.Lock()
	if w.expanded == open {
		w.mu.Unlock()
		return
	}
	w.expanded = open
	w.mu.Unlock()

	if open {
		// 记下展开前的尺寸，收起时原样还原——而不是硬编码回 340×460：
		// 用户可能自己拖拽缩放过窗口，把他调好的尺寸重置掉是那种很难复现的体验问题。
		baseW, baseH := runtime.WindowGetSize(w.ctx)
		w.mu.Lock()
		w.baseW, w.baseH = baseW, baseH
		w.mu.Unlock()

		runtime.WindowSetSize(w.ctx, baseW, baseH+menuExtraHeight)
		x, y := runtime.WindowGetPosition(w.ctx)
		runtime.WindowSetPosition(w.ctx, x, y-menuExtraHeight)
		return
	}

	w.mu.Lock()
	baseW, baseH := w.baseW, w.baseH
	w.mu.Unlock()

	runtime.WindowSetSize(w.ctx, baseW, baseH)
	x, y := runtime.WindowGetPosition(w.ctx)
	runtime.WindowSetPosition(w.ctx, x, y+menuExtraHeight)
}
