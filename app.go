package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-for-you-love/internal/llm"
	errorcode "agent-for-you-love/internal/pkg/errorCode"
	"agent-for-you-love/internal/ui"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx      context.Context
	win      *ui.Window
	provider llm.Provider

	mu sync.Mutex
	// history 阶段2 只放内存。隐藏窗口不销毁进程，所以"关闭窗口不丢历史"这条验收成立；
	// 进程重启即丢，持久化留到阶段3 / 阶段4。
	history []llm.Message
	// prevCancel 是上一轮流的取消函数。context.CancelFunc 可重复调用，所以不必清理，
	// 每次新请求直接覆盖即可——省掉了"谁负责清空"的并发问题。
	prevCancel context.CancelFunc
	msgSeq     int
}

func NewApp(provider llm.Provider) *App {
	return &App{provider: provider}
}

// startup 由 Wails 在应用启动时调用，此后 runtime 才可用。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.win = ui.NewWindow(ctx)

	ui.StartTray(ui.TrayOptions{
		Icon:           trayIcon,
		Tooltip:        "AI 桌面伴侣",
		OnToggleWindow: a.ToggleWindow,
		OnSay:          func() { a.Say("你好，我是你的桌面伴侣～") },
		OnQuit:         a.Quit,
	})

	log.Println("[app] 启动完成：窗口 + 系统托盘就绪")
}

// shutdown 由 Wails 在应用退出时调用。
func (a *App) shutdown(ctx context.Context) {
	ui.StopTray()
	log.Println("[app] 已退出")
}

// Say 让桌宠说一句话。
//
// 文本通过 Wails Events 推给前端，由 Bubble.vue 渲染成气泡。
// 之所以不用「返回值」而用「事件」：阶段2 接入 LLM 后，说话会由后端流式主动发起，
// 方向天然是 Go → 前端。这里先用同一条通路，后面接 LLM 时不必改架构。
func (a *App) Say(text string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, ui.EventSay, ui.SayPayload{
		Text: text,
		At:   time.Now().UnixMilli(),
	})
}

// Ask 把用户这句话交给模型，然后立刻返回本轮的消息 ID；
// 回复内容通过 chat:chunk / chat:done / chat:error 三个事件流式推给前端。
//
// 为什么必须立刻返回：绑定给前端的方法，前端 await 它时是在等返回值。
// 在这里等模型说完，界面就一直是卡住的——这正是流式输出要走事件的原因。
func (a *App) Ask(text string) (string, error) {
	if a.ctx == nil {
		return "", errorcode.New(errorcode.BadRequest, "应用尚未准备就绪")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}

	ctx, cancel := context.WithCancel(a.ctx)

	a.mu.Lock()
	if a.prevCancel != nil {
		a.prevCancel() // 掐掉上一轮，保证同时只有一个流
	}
	a.prevCancel = cancel
	a.history = append(a.history, llm.Message{Role: llm.RoleUser, Content: text})
	// 拷贝一份给 goroutine：之后 history 还会被 append，共享底层数组会读到意料之外的内容。
	msgs := append([]llm.Message(nil), a.history...)
	a.msgSeq++
	id := fmt.Sprintf("m%d", a.msgSeq)
	a.mu.Unlock()

	go a.stream(ctx, id, msgs)
	return id, nil
}

// Cancel 中止正在进行的回复（前端「停止」按钮）。
func (a *App) Cancel() {
	a.mu.Lock()
	cancel := a.prevCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// stream 消费 Provider 的通道，边收边推事件。
func (a *App) stream(ctx context.Context, id string, msgs []llm.Message) {
	ch, err := a.provider.ChatStream(ctx, msgs)
	if err != nil {
		// 走到这里说明请求还没发出去（缺 Key、网络不通、4xx）。历史里保留用户这句，方便重试。
		runtime.EventsEmit(a.ctx, ui.EventChatError, ui.ChatErrorPayload{ID: id, Message: err.Error()})
		return
	}

	var full strings.Builder
	for chunk := range ch { // 必须读到底：提前 return 会让生产端 goroutine 永久阻塞
		if chunk.Err != nil {
			if errors.Is(chunk.Err, context.Canceled) {
				// 用户点了停止，或发了新问题把这一轮掐掉——这不是错误，安静收场。
				a.appendAssistant(full.String())
				return
			}
			runtime.EventsEmit(a.ctx, ui.EventChatError, ui.ChatErrorPayload{ID: id, Message: chunk.Err.Error()})
			return
		}
		if chunk.Content == "" {
			continue
		}
		full.WriteString(chunk.Content)
		runtime.EventsEmit(a.ctx, ui.EventChatChunk, ui.ChatChunkPayload{ID: id, Delta: chunk.Content})
	}

	a.appendAssistant(full.String())
	runtime.EventsEmit(a.ctx, ui.EventChatDone, ui.ChatDonePayload{ID: id})
}

func (a *App) appendAssistant(content string) {
	if content == "" {
		return
	}
	a.mu.Lock()
	a.history = append(a.history, llm.Message{Role: llm.RoleAssistant, Content: content})
	a.mu.Unlock()
}

// ShowWindow 显示窗口。
func (a *App) ShowWindow() { a.win.Show() }

// HideWindow 隐藏窗口（收进托盘）。
func (a *App) HideWindow() { a.win.Hide() }

// ToggleWindow 在显示 / 隐藏之间切换。
func (a *App) ToggleWindow() { a.win.Toggle() }

// Quit 退出应用。
func (a *App) Quit() { runtime.Quit(a.ctx) }
