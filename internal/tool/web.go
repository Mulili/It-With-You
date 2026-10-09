package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/web"
)

// WebSearch 是「上网查一下」。
//
// 用不用它、什么时候用，全在 Description 里（模型只看得到这段话）：**问到现实世界的
// 时效信息**时用——新闻、天气、价格、比赛结果、最近发布了什么；她自己就知道的常识、
// 或用户只是在闲聊时，别用。
//
// ⚠️ 它**可能查不到**：本机的搜索服务没起来、目标引擎被反爬拦了、或者网络不通。
// 那时 Run 返回错误，调用方会把"调用失败：…"回灌给她——由她决定怎么说，
// 而不是由我们抛错把这一轮打断。
type WebSearch struct {
	Searcher *web.Searcher
	// Enabled 是"允许她上网"的总开关（设置里那个勾）。nil = 一直允许。
	//
	// 用函数而不是 bool：设置改了就立刻生效，不用重启应用——
	// 而"改了没反应"是最难查的那类问题。
	Enabled func() bool
}

// Spec 实现 Tool。
//
// Description 里刻意写了"什么时候**别**用"：function calling 里模型靠描述决定调不调，
// 只写"可以联网搜索"它会每轮都搜一下（那是真机上很容易被用户看出来的毛病）。
func (WebSearch) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name: "web_search",
			Description: "上网搜索最新的信息。当用户问到现实世界里会变化的东西——" +
				"新闻、天气、价格、赛事、某个人或产品最近怎么样了、你不确定或可能过时的事实时用它。" +
				"凭你自己的知识就能回答的（常识、聊天、算数、写东西）不要用。" +
				"查到的只是资料，可能有错，要挑真正相关的说。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "搜索关键词。用最能命中结果的几个词，不要写成一整句话，也不要加引号或搜索语法。",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// Available 实现 Availability：没配地址、或用户关掉了联网，就不该让她看见这个工具。
func (w WebSearch) Available() bool {
	if w.Searcher == nil || !w.Searcher.Available() {
		return false
	}
	return w.Enabled == nil || w.Enabled()
}

// Run 实现 Tool。
func (w WebSearch) Run(ctx context.Context, args string) (string, error) {
	query, err := stringArg(args, "query")
	if err != nil {
		return "", err
	}
	results, err := w.Searcher.Search(ctx, query)
	if err != nil {
		return "", err
	}
	return web.Format(query, results), nil
}

// ReadPage 是「打开这一页仔细看看」。
//
// 为什么与搜索分成两个工具：正文很贵（一页几千字，见 web.MaxPageRunes）。
// 模型拿到搜索结果的标题与摘要通常已经够回答，**只在摘要不够、或用户明确要看那篇的细节时**
// 才该付这个钱。合成一个工具的话，每次搜索都会顺手把正文灌进上下文。
type ReadPage struct {
	Reader *web.Reader
	// Enabled 是"允许她上网"的总开关（设置里那个勾）。nil = 一直允许。
	//
	// 与 WebSearch 共用同一个开关：读一页也是"上网"，用户关掉联网时不该还剩一个
	// 能打开外部网页的工具在。
	Enabled func() bool
}

// Spec 实现 Tool。
func (ReadPage) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name: "read_page",
			Description: "打开一个网页，读它的正文。当你已经从搜索结果里拿到链接、" +
				"但标题和摘要不足以回答问题（需要具体数字、细节、原文说法）时用它。" +
				"只读一个你确实要知道内容的页面；只为了确认某个标题就没必要读。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "要读的网页地址（http/https）。应当是搜索结果里给出的链接。",
					},
				},
				"required": []string{"url"},
			},
		},
	}
}

// Available 实现 Availability：解析器起不来（初始化失败）、或联网被关掉，就别给她这个工具。
func (p ReadPage) Available() bool {
	if p.Reader == nil {
		return false
	}
	return p.Enabled == nil || p.Enabled()
}

// Run 实现 Tool。
func (p ReadPage) Run(ctx context.Context, args string) (string, error) {
	rawURL, err := stringArg(args, "url")
	if err != nil {
		return "", err
	}
	return p.Reader.Read(ctx, rawURL)
}

// stringArg 从模型给的参数 JSON 里取一个字符串字段。
//
// 为什么容错要求低、报错要求高：参数是**模型写的**，它偶尔会把字段名写错、
// 或者干脆给个不合法的 JSON。这时把"应该怎么写"告诉它，它下一轮就改对了；
// 而我们自己 panic 或抛一个空洞的错，只会让这一轮白费。
func stringArg(args, field string) (string, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "", fmt.Errorf("缺少参数 %s（应当形如 {\"%s\": \"...\"}）", field, field)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return "", fmt.Errorf("参数不是合法的 JSON（应当形如 {\"%s\": \"...\"}）：%v", field, err)
	}
	v, _ := m[field].(string)
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("参数 %s 是空的（应当形如 {\"%s\": \"...\"}）", field, field)
	}
	return v, nil
}
