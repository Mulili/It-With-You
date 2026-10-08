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
//
// 阶段4.5 起它还承担工具调用的三种新形态（OpenAI 兼容协议）：
//
//	assistant + ToolCalls  → 她这一轮要求调用这些工具
//	tool + ToolCallID      → 工具的结果，回给**那一次**调用
//
// 加上原本的 system / user / assistant 纯文本，一共五种组合。之所以不另立结构体：
// 协议就是这么定义的（同一个 messages 数组里混排），拆成两套反而要来回转换。
type Message struct {
	Role    string `json:"role"` // system / user / assistant / tool
	Content string `json:"content"`
	// ToolCalls 只在 assistant 消息上出现：模型这一轮想调的工具（可能多个，可并行）。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 只在 role=tool 的消息上出现：这条结果回的是哪一次调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ReasoningContent 是思考模式的思维链，**做过工具调用的那一轮必须原样回传**。
	//
	// 官方硬规则：两个 user 消息之间，若 assistant 做过工具调用，它的 reasoning_content
	// 必须回传；没做过则可省略（传了也会被忽略）。不回传会直接报错——而这正是
	// "工具一次就废"的那种坑：只有真开了思考模式才会踩到。
	//
	// 我们**不显示也不落库**它（用户不该看到思维链），只在工具循环内部回传。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// 角色常量。写字符串容易拼错，而拼错不会报错，只会让模型行为变得诡异。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	// RoleTool 承载一次工具调用的结果，必须与 ToolCallID 配对出现。
	RoleTool = "tool"
)

// ToolCall 是模型要求执行的一次工具调用。
//
// 注意 Arguments 是**模型给的原始 JSON 字符串**，不是解析好的参数：
// 它由模型生成，可能是截断的、类型错的、甚至不是合法 JSON——解析与校验是执行方的事，
// 本层只负责把协议字段搬运过来（与 Provider 接口"只做接入、不做业务"的分工一致）。
type ToolCall struct {
	// ID 由服务端生成，回结果时要用它配对
	ID   string `json:"id"`
	Type string `json:"type"` // 固定 "function"
	// Function 是对函数的说明。用匿名结构体而不是另立类型：它只有名字与参数两个字段，
	// 而且**只在这里出现**（执行方看到的是 Name + Arguments）。
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool 是发给模型的工具声明（告诉它"你能调什么"）。
type Tool struct {
	Type     string       `json:"type"` // 固定 "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction 是一个工具的自描述。
type ToolFunction struct {
	// Name 是模型调用时用的标识，必须与注册表里的键一致
	Name string `json:"name"`
	// Description 决定模型会不会用它、用得对不对，是这三个字段里最要紧的一个
	Description string `json:"description"`
	// Parameters 是 JSON Schema（type: object + properties + required）
	Parameters any `json:"parameters"`
}

// Chunk 是流式回复中的一个片段。
type Chunk struct {
	Content string
	// Reasoning 是思考模式的思维链，与 ToolCalls 一样**只在流结束时那一个 chunk 上非空**。
	// 它不推给前端（用户不该看到思维链），只在工具循环里原样回传给服务端
	//（见 Message.ReasoningContent：那是官方硬规则，不回传会直接报错）。
	Reasoning string
	// ToolCalls 只在**流结束时**那一个 chunk 上非空，且已经是拼好的完整结果。
	//
	// 为什么不在每个 delta 上透传分片：协议里 tool_calls 是按 index 分片的
	// （id / name / arguments 各自被切成好几帧），拼接是**协议细节**，属于本层；
	// 上层只该看到"她要调这几个工具、参数分别是这些"。
	ToolCalls []ToolCall
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

	// Tools 非空时把工具声明发给模型（function calling）。
	//
	// 与 Thinking 同一姿态：不带就是不带（omitempty），因为对不支持 tools 的服务端
	// 多一个未知字段会直接 400。空切片与 nil 都表示"这次不给它工具"。
	Tools []Tool
}
