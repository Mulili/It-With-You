// Package tool 是**可被模型调用的工具**（阶段4.5）。
//
// 为什么单独成包：一个工具由两半组成——"给模型看的声明"与"真正执行的那段代码"，
// 而这两半分散在不同的地方（声明要进请求体，执行在对话循环里）。放在一个包里，
// 就能让同一张表同时管住它们：不会出现"告诉她能调某工具、但没人实现"
// （模型每次调都失败），也不会出现"实现了却没告诉她"（永远用不上）。
//
// 与 llm 包的分工：llm 只管协议（Tool/ToolCall 的形状），本包管"有哪些工具、怎么执行"。
package tool

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/web"
)

// Tool 是一个可被模型调用的工具。
//
// 实现方要保证 Run 的两件事：
//   - **不 panic、不长阻塞**：它跑在一轮对话的关键路径上（用户正等着回复）；
//   - 失败时返回 `error`，由调用方翻译成"给模型看的一句话"——
//     让模型自己决定怎么办（换个说法、或者老实说查不到），比我们抛错断在半截好。
type Tool interface {
	// Spec 是给模型看的声明。Name 必须稳定：模型按名字调用，改名等于换了一个工具。
	Spec() llm.Tool
	// Run 执行一次调用，返回**给模型看的文本结果**。
	// args 是模型给的原始参数 JSON（可能为空串，也可能是它写坏的 JSON）。
	Run(ctx context.Context, args string) (string, error)
}

// Availability 是工具可选实现的一个小接口：报告"我现在能不能用"。
//
// 为什么需要它：本轮这批工具里有需要外部服务的（联网搜索要本机那个搜索服务在跑、
// 读网页要解析器起来）。**不可用时不该让模型看见这个工具**——否则她会拿一个必然失败的工具
// 反复试，用户看到的是"她老说查不到"。而"看得见却调不动"比"看不见"更糟：
// 模型会以为是自己参数写错了，于是反复重试。
//
// 判定必须是**廉价的**（读一个标志，不要发网络请求）：Specs 每一轮都会被调用一次。
type Availability interface {
	Available() bool
}

// usable 判断一个工具现在能不能用（没实现 Availability 的永远可用）。
func usable(t Tool) bool {
	if av, ok := t.(Availability); ok {
		return av.Available()
	}
	return true
}

// Registry 是"名字 → 工具"的注册表，也是给模型的那份工具清单的唯一来源。
//
// 空注册表是合法状态：Specs() 返回 nil，于是 ChatOptions.Tools 为空、
// 请求里连 tools 字段都不带（对不支持它的服务端很要紧，见 llm.ChatOptions.Tools）。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 建一个注册表。
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		r.tools[t.Spec().Function.Name] = t
	}
	return r
}

// Builtins 返回内置工具集（阶段4.5 起）。
//
// 现在两件：查时间（零依赖）与上网（搜索 + 读网页）。
// 加第三个工具时，往这里添一项 + 一个文件即可——搜索与读页是同一套链路的延伸。
func Builtins(search *web.Searcher, reader *web.Reader, searchEnabled func() bool) *Registry {
	return NewRegistry(
		Clock{},
		WebSearch{Searcher: search, Enabled: searchEnabled},
		ReadPage{Reader: reader, Enabled: searchEnabled},
	)
}

// Specs 返回给模型看的工具声明，按名字排序（顺序稳定，便于比对两次请求的差异）。
//
// **跳过当前不可用的工具**（见 Availability）：让她看不见一个必然失败的工具。
func (r *Registry) Specs() []llm.Tool {
	if r == nil || len(r.tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.tools))
	for name, t := range r.tools {
		if !usable(t) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]llm.Tool, 0, len(names))
	for _, name := range names {
		out = append(out, r.tools[name].Spec())
	}
	return out
}

// Empty 表示没有任何工具可给（调用方据此决定要不要带 tools 字段）。
func (r *Registry) Empty() bool { return r == nil || len(r.tools) == 0 }

// Call 按名字执行一次调用。
//
// 名字不存在时返回 error 而不是 panic：模型偶尔会**编**一个不存在的工具名
// （尤其在工具列表刚变过的时候）。那属于"它的小失误"，不该让这一轮崩掉——
// 调用方会把这句话回灌给它，它自己就纠正了。
func (r *Registry) Call(ctx context.Context, name, args string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("当前没有可用的工具")
	}
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("没有叫 %q 的工具（可用：%s）", name, strings.Join(r.names(), "、"))
	}
	// 不可用的工具要拦在这里：它可能上一秒还可用（服务挂了、开关被关），
	// 而模型是照着上一轮的工具声明来调的
	if !usable(t) {
		return "", fmt.Errorf("工具 %q 现在不可用（外部服务没起来，或者这个能力被关掉了）", name)
	}
	return t.Run(ctx, args)
}

// names 返回排序后的工具名，只用于错误信息。
func (r *Registry) names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
