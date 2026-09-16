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
