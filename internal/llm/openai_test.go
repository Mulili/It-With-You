package llm

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
)

// 这两个测试会**真的调用一次模型**，属于集成测试：没配 API Key 时自动跳过。
//
// 为什么不做成纯单元测试：本步要验证的是"服务端确实接受 response_format: json_object
// 并返回可解析的 JSON"，这件事只有打一次真请求才能确认，mock 掉就没有意义了。
//
// 运行方式（在项目根目录，一条命令即可）：
//
//	go test ./internal/llm -run TestChat -v -count=1
//
// Key 从哪来：`go test` 的工作目录是**包目录**，而 .env 在项目根，所以要向上找一次
// （config.LoadDotEnvUpward）。已经设好的环境变量仍然优先，因为它不覆盖已有值。
//
// -count=1 是为了绕开 go test 的结果缓存，否则第二次跑会直接复用上次结论、根本没发请求。

// newTestProvider 返回一个可用的 provider；没有 Key 时跳过测试。
func newTestProvider(t *testing.T) (*OpenAIProvider, context.Context) {
	t.Helper()
	// 顺序不能反：先补 .env，再读配置
	config.LoadDotEnvUpward()
	cfg := ConfigFromEnv()
	if cfg.APIKey == "" {
		t.Skip("未配置 COMPANION_LLM_API_KEY（.env 或环境变量），跳过集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return NewOpenAIProvider(cfg), ctx
}

// 分片的 tool_calls 必须在流末被拼成**一次完整调用**。
//
// 这是这一层最容易写错的地方：协议把一次调用切成好几帧——id 与 name 只在第一片出现，
// 后面几片只续 arguments 的一半。拼错的表现是"参数是半截 JSON"，而模型拿到半截 JSON
// 只会瞎猜，比不调工具更糟；而拼错的那半边**恰好**是最长的参数（模型会把它切很多片）。
//
// 这个测试不打网络：pump 吃的是 io.Reader，喂一段假的 SSE 就能验完整条解析。
func TestPumpAssemblesToolCallFragments(t *testing.T) {
	// 帧的形状照抄真实协议：注意第二帧起 id 与 name 都不再出现
	frames := []string{
		`data: {"choices":[{"delta":{"content":"我看一眼。"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_current_time","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"asia/shanghai\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"我得先知道几点。"}}]}`,
		": keep-alive",
		`data: [DONE]`,
	}
	p := &OpenAIProvider{}
	ch := make(chan Chunk, 64)
	go func() {
		defer close(ch)
		p.pump(context.Background(), strings.NewReader(strings.Join(frames, "\n")), ch)
	}()

	var text strings.Builder
	var last Chunk
	count := 0
	for c := range ch {
		count++
		text.WriteString(c.Content)
		last = c
	}
	if count == 0 {
		t.Fatal("一帧都没解析出来")
	}
	if got := text.String(); got != "我看一眼。" {
		t.Errorf("正文解析不对：%q", got)
	}
	if !last.Done {
		t.Fatal("最后一片应当是收尾帧（工具调用挂在它上面）")
	}
	if last.Reasoning != "我得先知道几点。" {
		t.Errorf("思维链应当被攒下来：%q", last.Reasoning)
	}
	if len(last.ToolCalls) != 1 {
		t.Fatalf("应当拼出 1 次调用，实际 %d 次：%+v", len(last.ToolCalls), last.ToolCalls)
	}
	tc := last.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_current_time" {
		t.Errorf("id / name 应当来自第一片：%+v", tc)
	}
	if want := `{"tz":"asia/shanghai"}`; tc.Function.Arguments != want {
		t.Errorf("分片的 arguments 没拼对：\n实际 %q\n期望 %q", tc.Function.Arguments, want)
	}
}

// 多个工具调用（并行）也要按 index 各自收拢，且顺序稳定。
//
// 顺序为什么重要：调用方按顺序执行、按顺序回结果，而 map 的遍历顺序在 Go 里是随机的——
// 那会让"同一份输出跑出不同顺序的调用"变成偶发现象，事后极难复现。
func TestPumpAssemblesParallelToolCallsInOrder(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"second","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"first","arguments":"{}"}}]}}]}`,
		`data: [DONE]`,
	}
	p := &OpenAIProvider{}
	ch := make(chan Chunk, 8)
	go func() {
		defer close(ch)
		p.pump(context.Background(), strings.NewReader(strings.Join(frames, "\n")), ch)
	}()

	var last Chunk
	for c := range ch {
		last = c
	}
	if len(last.ToolCalls) != 2 {
		t.Fatalf("应当拼出 2 次调用，实际 %d 次", len(last.ToolCalls))
	}
	if last.ToolCalls[0].Function.Name != "first" || last.ToolCalls[1].Function.Name != "second" {
		t.Errorf("应当按 index 升序（与协议给的顺序一致），实际：%+v", last.ToolCalls)
	}
}

// 不带工具时，请求体里**不能出现 tools 字段**。
//
// 这条不是洁癖：对不支持 tools 的服务端（某些本地推理框架、老的兼容网关），
// 多一个未知字段会直接 400——那会让整个对话挂掉，而我们的工具本来是可选的。
func TestRequestOmitsToolsUnlessAsked(t *testing.T) {
	p := NewOpenAIProvider(Config{BaseURL: "https://example.com/v1", Model: "m", APIKey: "k"})
	msgs := []Message{{Role: RoleUser, Content: "在吗"}}

	bodyOf := func(t *testing.T, opts ChatOptions) string {
		t.Helper()
		req, err := p.newRequest(context.Background(), msgs, true, opts)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("读请求体失败: %v", err)
		}
		return string(raw)
	}

	if body := bodyOf(t, ChatOptions{}); strings.Contains(body, `"tools"`) {
		t.Errorf("没给工具时不该带 tools 字段：%s", body)
	}
	if body := bodyOf(t, ChatOptions{}); strings.Contains(body, `"tool_choice"`) {
		t.Errorf("我们不用 tool_choice，也更不该凭空带上：%s", body)
	}

	withTool := bodyOf(t, ChatOptions{Tools: []Tool{{
		Type:     "function",
		Function: ToolFunction{Name: "get_current_time", Description: "查时间", Parameters: map[string]any{"type": "object"}},
	}}})
	if !strings.Contains(withTool, `"tools"`) || !strings.Contains(withTool, "get_current_time") {
		t.Errorf("给了工具就该出现在请求体里：%s", withTool)
	}
}

// 工具调用的**真机**验证：确认服务端真的接受 tools、真的把分片的 tool_calls 回给我们、
// 而且**回灌结果并把思维链一起回传**之后它愿意开口。
//
// 为什么非打一次真请求不可：这一条链路的细节（分片怎么拼、reasoning_content 要不要回传、
// 空 parameters 收不收）全是从文档读来的，而文档与真实响应之间经常差一点——
// 差的那一点只有发一次才知道。operation.md 里那些"已核实的前提"也是这么来的。
func TestChatStreamToolCallRoundTrip(t *testing.T) {
	p, ctx := newTestProvider(t)

	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "get_current_time",
			Description: "查询当前的日期、星期与时间。用户问到今天是几号、现在几点时使用。",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
		},
	}}
	base := []Message{
		{Role: RoleSystem, Content: "你在扮演一个助手。需要知道时间时必须调用工具，不要凭空猜。"},
		{Role: RoleUser, Content: "现在几点了？"},
	}

	// 第一跳：它应当要求调用工具
	ch, err := p.ChatStream(ctx, base, ChatOptions{Tools: tools})
	if err != nil {
		t.Fatalf("发起请求失败: %v", err)
	}
	var text, reasoning string
	var calls []ToolCall
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("流式出错: %v", c.Err)
		}
		text += c.Content
		if c.Done {
			calls, reasoning = c.ToolCalls, c.Reasoning
		}
	}
	if len(calls) == 0 {
		t.Fatalf("模型没有要求调用工具（正文：%q）——要么 tools 没被接受，要么描述不够明确", text)
	}
	tc := calls[0]
	if tc.Function.Name != "get_current_time" {
		t.Errorf("调用的工具名不对：%q", tc.Function.Name)
	}
	if tc.ID == "" {
		t.Error("工具调用必须带 id：结果要靠它配对，缺了就回不去")
	}

	// 第二跳：把结果**与思维链一起**回灌（回传 reasoning 是官方硬规则，不回传会报错）
	msgs := append(append([]Message{}, base...),
		Message{Role: RoleAssistant, Content: text, ToolCalls: calls, ReasoningContent: reasoning},
		Message{Role: RoleTool, ToolCallID: tc.ID, Content: "现在是 2026-10-08 21:45（星期四）"},
	)
	ch2, err := p.ChatStream(ctx, msgs, ChatOptions{Tools: tools})
	if err != nil {
		t.Fatalf("回灌之后发起请求失败（这一条最可能踩到 reasoning_content 的硬规则）: %v", err)
	}
	var final strings.Builder
	for c := range ch2 {
		if c.Err != nil {
			t.Fatalf("回灌后的流式出错: %v", c.Err)
		}
		final.WriteString(c.Content)
	}
	if strings.TrimSpace(final.String()) == "" {
		t.Fatal("拿到工具结果之后应当给出一句回答")
	}
	t.Logf("工具链路真机正常：调用 %s（思维链 %d 字），最终回答 %q",
		tc.Function.Name, len([]rune(reasoning)), final.String())
}

// 验证 JSON 模式：这是阶段3 抽取链路的地基，必须确认服务端真的返回可解析的 JSON。
func TestChatJSONMode(t *testing.T) {
	p, ctx := newTestProvider(t)

	// 提示词里必须出现 "json" 字样，否则服务端会拒绝 response_format（OpenAI 与 DeepSeek 都如此）
	out, err := p.Chat(ctx, []Message{
		{Role: RoleSystem, Content: "你是信息抽取器，只输出 json，不要任何解释文字。"},
		{Role: RoleUser, Content: `从这句话里抽取「称呼」相关信息，以 json 输出，字段为 slot 与 value。句子：以后呢喊你Lapwing怎么样`},
	}, ChatOptions{JSON: true})
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}

	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("返回内容不是合法 JSON: %v\n原始内容: %s", err, out)
	}
	if len(v) == 0 {
		t.Fatalf("返回的是空 JSON 对象: %s", out)
	}
	t.Logf("JSON 模式正常，模型输出: %s", out)
}

// 验证非 JSON 模式：确认抽出来的公用请求构造没有把普通问答带坏
// （例如误加了 response_format，或 Accept 头改错了）。
func TestChatPlainText(t *testing.T) {
	p, ctx := newTestProvider(t)

	out, err := p.Chat(ctx, []Message{
		{Role: RoleUser, Content: "只回复两个字：你好"},
	}, ChatOptions{})
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("返回内容为空")
	}
	// 普通模式不该被强制成 JSON：这里只要求它不是纯 JSON 对象即可
	var v map[string]any
	if json.Unmarshal([]byte(out), &v) == nil && len(v) > 0 && !strings.Contains(out, "你好") {
		t.Fatalf("普通模式疑似被强加了 JSON 输出: %s", out)
	}
	t.Logf("非流式普通回复正常，模型输出: %s", out)
}
