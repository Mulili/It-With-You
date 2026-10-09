package ui

// 事件名统一定义在这里，Go 侧用 runtime.EventsEmit 推送、前端用 EventsOn(<常量值>, ...) 监听。
// 两端共用同一份名字，避免字符串写错导致事件收不到。
const (
	// EventSay 是「让桌宠说话」，阶段1 的单句气泡。
	EventSay = "bubble:say"

	// 下面三个是阶段2 的对话流：逐段正文、本轮正常结束、本轮出错。
	EventChatChunk = "chat:chunk"
	EventChatDone  = "chat:done"
	EventChatError = "chat:error"

	// EventPersonaChanged 是人格被改动的通知（阶段3）。
	// 用途有二：显式指令写入后的回执（让用户知道"它记住了"）、设置浮层开着时刷新。
	EventPersonaChanged = "persona:changed"

	// EventContextStat 在**每轮拼完上下文之后**发出，负载是 ui.ContextStat。
	//
	// 为什么用事件而不是让前端主动来问：这个数字只有在"刚拼完上下文"那一刻才准
	// （它就是那一轮真的发出去的东西）。前端每轮问一次等于多一次往返，还可能问到旧值。
	EventContextStat = "context:stat"

	// EventWindowHidden 在窗口被隐藏时发出。
	//
	// 为什么需要它：隐藏可能由**托盘**发起（Go 侧直接调 Hide），而"菜单开没开"的状态在前端。
	// 不通知的话前端会以为菜单还开着——下次显示窗口时，「加高后的尺寸 + 打开的面板」一起回来。
	EventWindowHidden = "window:hidden"

	// EventToolRunning 在她**开始执行一个耗时工具**时发出（负载 ui.ToolRunningPayload）。
	//
	// 为什么需要它：查时间几乎瞬间返回，但联网搜索要几秒、读一页更久。中间没有提示，
	// 界面就只是停在半截回复上——用户会以为卡死了，然后去点停止。
	// 零延迟的工具**不发**这个事件（见 app.announceTool）：发了只会让提示闪一下。
	EventToolRunning = "tool:running"
)

// SayPayload 是 EventSay 事件的负载。
type SayPayload struct {
	Text string `json:"text"`
	// At 是 Unix 毫秒时间戳。
	// 阶段1 只是留给前端做去重 / 排序；接 LLM 后可用于对齐流式片段的先后顺序。
	At int64 `json:"at"`
}

// ChatChunkPayload 是一段流式片段。
// ID 标识本轮回复，前端据此把片段追加到同一条消息上，两个请求交错时不会串台。
type ChatChunkPayload struct {
	ID    string `json:"id"`
	Delta string `json:"delta"`
}

// ChatDonePayload 表示本轮正常结束。
type ChatDonePayload struct {
	ID string `json:"id"`
}

// ChatErrorPayload 表示本轮以错误结束。
type ChatErrorPayload struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// PersonaChangedPayload 是一条人格变更通知。
//
// Summary 是后端组装好的中文短句，前端可以直接显示（例如"已记住：称呼 → 主人"）——
// 把文案留给后端，是为了让"有没有真的记住、记的是什么"只有一处真相。
type PersonaChangedPayload struct {
	PersonaID string `json:"personaId"`
	// Action 形如 create / update / delete / enable / disable（见 persona 包的 Action* 常量）
	Action  string `json:"action"`
	Slot    string `json:"slot"`
	Value   string `json:"value"`
	Summary string `json:"summary"`
}

// ToolRunningPayload 是一次耗时工具调用开始执行的通知。
//
// Label 是给人看的那句话（"正在上网查…"），由**后端**组装：让"哪个工具叫什么提示"
// 只有一处真相，前端不必认识 web_search 这种英文标识。
type ToolRunningPayload struct {
	// ID 是本轮回复的轮次 ID，前端据此把提示挂到正确的那条消息上。
	ID string `json:"id"`
	// Tool 是工具名（web_search / read_page），留给调试与将来做更细的图标。
	Tool string `json:"tool"`
	// Label 是直接显示的中文提示。
	Label string `json:"label"`
}
