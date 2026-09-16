// Package llm 是大模型接入层（阶段2 落地）。
//
// 设计目标是把「用哪家模型」和「业务怎么用」解耦：对外只暴露 Provider 一个接口，
// DeepSeek / OpenAI / 本地推理各写一个实现即可，App 层不需要跟着改。
//
// 流式输出的通路（阶段2 的关键决策）：Wails 前端拿不到 Go 的流，所以 ChatStream 返回通道，
// App 层每收到一个 chunk 就用 runtime.EventsEmit 往前端推一次，前端用 EventsOn 增量渲染。
// 事件名统一定义在 internal/ui/events.go，两端共用。
package llm

import "context"

// Message 是一条对话消息。
type Message struct {
	Role    string `json:"role"` // system / user / assistant
	Content string `json:"content"`
}

// 角色常量。写字符串容易拼错，而拼错不会报错，只会让模型行为变得诡异。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Chunk 是流式回复中的一个片段。
type Chunk struct {
	Content string
	// Done 表示本轮正常结束。
	Done bool
	// Err 非 nil 表示本轮以错误结束，此后通道不会再有内容。
	Err error
}

// Provider 是模型接入层对外的唯一抽象，换供应商只换实现，上层不动。
//
// 约定（实现方与调用方都要遵守，否则会 goroutine 泄漏）：
//   - ChatStream 立即返回通道，不阻塞等待模型响应；
//   - 实现方负责在所有返回路径上关闭通道；
//   - 调用方必须把通道读到底（for range），不能中途 return 走人。
type Provider interface {
	// ChatStream 以流式方式返回回复，chunk 通道关闭表示本轮结束。
	ChatStream(ctx context.Context, messages []Message) (<-chan Chunk, error)
}
