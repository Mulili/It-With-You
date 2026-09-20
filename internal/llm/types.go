// Package llm 是大模型接入层（阶段2 落地）。
//
// 设计目标是把「用哪家模型」和「业务怎么用」解耦：对外只暴露 Provider 一个接口，
// DeepSeek / OpenAI / 本地推理各写一个实现即可，App 层不需要跟着改。
//
// 流式输出的通路（阶段2 的关键决策）：Wails 前端拿不到 Go 的流，所以 ChatStream 返回通道，
// App 层每收到一个 chunk 就用 runtime.EventsEmit 往前端推一次，前端用 EventsOn 增量渲染。
// 除流式外，这里还提供非流式的 Chat：抽取类场景（从对话里抽槽位、抽事实）需要一整块
// 可解析的 JSON，半个 JSON 没法解析，所以不能用流式。阶段3 的人格抽取与阶段4 的记忆抽取都走它。
//
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
	ChatStream(ctx context.Context, messages []Message, opts ChatOptions) (<-chan Chunk, error)

	// Chat 非流式地要一次完整回复，返回模型输出的正文。
	//
	// 与 ChatStream 的分工：流式给人看（逐字上屏），非流式给机器用
	// （从对话里抽槽位、抽事实这类场景要一整块可解析的结果，半个 JSON 没法解析）。
	// 返回的是模型原样输出的字符串，本层不假设它是 JSON，解析由调用方负责。
	Chat(ctx context.Context, messages []Message, opts ChatOptions) (string, error)
}

// ChatOptions 是一次调用的可选项。零值表示"普通文本回复"。
type ChatOptions struct {
	// JSON 为 true 时要求服务端输出可解析的 JSON 对象（response_format: json_object）。
	//
	// 注意：OpenAI 与 DeepSeek 都要求**提示词里出现 "json" 字样**才会接受这个参数，
	// 否则会直接报错。这是调用方的责任，本层不做校验也不做改写。
	//
	// 只对非流式的 Chat 有效：JSON 要求一次性给出完整对象，与逐帧流出天然矛盾。
	JSON bool

	// DisableThinking 为 true 时显式要求服务端关闭思考模式。
	//
	// 为什么需要这个开关：DeepSeek 的思考模式**默认开启**，而它对闲聊只有副作用——
	// 最终回答之前要先算完思维链（表现为长时间不吐字）、思考 token 按输出价计费，
	// 而且思考模式下 temperature / top_p / presence_penalty / frequency_penalty
	// 会**静默失效**（设置了不报错、也不生效）。
	//
	// 零值必须是"不发送这个字段"：它是 DeepSeek 的扩展而非 OpenAI 标准，
	// 对不认识它的服务端（OpenAI 官方、某些本地推理框架）会直接报未知参数。
	// 所以只有调用方明确要求时才带上，默认路径的行为与加这个字段之前完全一致。
	DisableThinking bool
}
