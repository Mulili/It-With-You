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
