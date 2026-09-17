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
	// records 是会话历史的唯一真源，阶段2.5 只放内存：隐藏窗口不销毁进程，
	// 所以"关闭窗口不丢历史"这条验收成立；进程重启即丢，持久化留到阶段3 / 阶段4。
	//
	// 两个出口都从它投影而来，不另存副本——两份数据要成对 append，
	// 漏一处就会永久不同步，而症状是"菜单里少一条"这种极难定位的问题：
	//   - 发给模型：projectMessages（阶段4 的上下文截断加在那里）
	//   - 给前端菜单：History（截到最近 ui.HistoryDisplayLimit 条）
	records []record
	// prevCancel 是上一轮流的取消函数。context.CancelFunc 可重复调用，所以不必清理，
	// 每次新请求直接覆盖即可——省掉了"谁负责清空"的并发问题。
	prevCancel context.CancelFunc
	msgSeq     int
}

// record 是内部记录：除了 llm.Message 还带上展示需要的编号、时间与状态。
//
// 这些字段必须留在这里，不能加进 llm.Message——后者会被整段序列化进请求体，
// 加什么字段都会原样发给模型。
type record struct {
	id      string
	message llm.Message
	at      int64
	status  string
}

// nextID 返回进程内自增的记录编号。调用方必须持有 a.mu。
func (a *App) nextID() string {
	a.msgSeq++
	return fmt.Sprintf("m%d", a.msgSeq)
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
	// 这一轮的 ID 同时就是这条 user 记录的 ID，流式事件也用它，前端据此过滤片段
	id := a.nextID()
	a.records = append(a.records, record{
		id:      id,
		message: llm.Message{Role: llm.RoleUser, Content: text},
		at:      time.Now().UnixMilli(),
		status:  ui.StatusOK, // 用户这句话在发出时就是完整的
	})
	// 拷贝一份给 goroutine：之后 records 还会被 append，共享底层数组会读到意料之外的内容。
	msgs := projectMessages(a.records)
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

// projectMessages 把内部记录投影成发给模型的消息。
//
// 现在只是把 message 摘出来，但它有意做成一函数而不是一段内联代码：
// 阶段3 要在头部拼人格 System Prompt、阶段4 要「只发最近 N 轮」，
// 那两件事都加在这里，而不是去动 records 本身。
//
// 纯函数、不碰锁：调用方持有 a.mu 时使用。
func projectMessages(records []record) []llm.Message {
	msgs := make([]llm.Message, 0, len(records))
	for _, r := range records {
		msgs = append(msgs, r.message)
	}
	return msgs
}

// History 返回最近的历史对话，供前端菜单展示（只读，拉模式）。
//
// 返回的是新切片，与内部 records 不共享底层数组，前端随便改都影响不到真源。
func (a *App) History() []ui.HistoryItem {
	a.mu.Lock()
	defer a.mu.Unlock()

	start := len(a.records) - ui.HistoryDisplayLimit
	if start < 0 {
		start = 0
	}
	items := make([]ui.HistoryItem, 0, len(a.records)-start)
	for _, r := range a.records[start:] {
		items = append(items, ui.HistoryItem{
			ID:     r.id,
			Role:   r.message.Role,
			Text:   r.message.Content,
			At:     r.at,
			Status: r.status,
		})
	}
	return items
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
				// 已收到的半截内容照样进历史，但标成 canceled：菜单要能把它和正常回复区分开。
				a.appendAssistant(full.String(), ui.StatusCanceled)
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

	a.appendAssistant(full.String(), ui.StatusOK)
	runtime.EventsEmit(a.ctx, ui.EventChatDone, ui.ChatDonePayload{ID: id})
}

// appendAssistant 把这一轮的回复写进历史。status 区分"正常说完"与"被用户打断"。
func (a *App) appendAssistant(content, status string) {
	if content == "" {
		return
	}
	a.mu.Lock()
	a.records = append(a.records, record{
		id:      a.nextID(),
		message: llm.Message{Role: llm.RoleAssistant, Content: content},
		at:      time.Now().UnixMilli(),
		status:  status,
	})
	a.mu.Unlock()
}

// ShowWindow 显示窗口。
func (a *App) ShowWindow() { a.win.Show() }

// HideWindow 隐藏窗口（收进托盘）。
func (a *App) HideWindow() { a.win.Hide() }

// ToggleWindow 在显示 / 隐藏之间切换。
func (a *App) ToggleWindow() { a.win.Toggle() }

// SetMenuOpen 由前端在打开 / 关闭历史菜单时调用，用于临时加高窗口。
func (a *App) SetMenuOpen(open bool) { a.win.SetMenuOpen(open) }

// Quit 退出应用。
func (a *App) Quit() { runtime.Quit(a.ctx) }
