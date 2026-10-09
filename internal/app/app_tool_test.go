package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"agent-for-you-love/internal/history"
	historystore "agent-for-you-love/internal/history/store"
	"agent-for-you-love/internal/llm"
)

// scriptedProvider 按"这一轮给没给工具"决定怎么答，用来把工具循环跑完整。
//
// 为什么按 opts.Tools 而不是按轮次编号决定：那正好模拟真实模型的行为——
// 有工具可用时它才会要求调用；到上限后我们不再给工具，它自然就改成说话了。
// 于是这个假 provider 同时验证了"循环能收住"。
type scriptedProvider struct {
	mu sync.Mutex
	// toolRounds 是前几轮要求调用工具（0 = 从不要求）
	toolRounds int
	// toolName 是它要求调用的工具名（空 = 查时间）；填一个不存在的名字用来测失败路径
	toolName string

	opts []llm.ChatOptions // 每轮的 options（断言"到上限后不带工具"）
	seen [][]llm.Message   // 每轮的输入（断言"工具结果回灌了"）
}

func (p *scriptedProvider) ChatStream(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (<-chan llm.Chunk, error) {
	p.mu.Lock()
	p.opts = append(p.opts, opts)
	p.seen = append(p.seen, msgs)
	round := len(p.opts)
	p.mu.Unlock()

	name := p.toolName
	if name == "" {
		name = "get_current_time"
	}
	askTool := len(opts.Tools) > 0 && round <= p.toolRounds

	ch := make(chan llm.Chunk, 8)
	go func() {
		defer close(ch)
		if askTool {
			ch <- llm.Chunk{Content: "我看一眼时间。"}
			tc := llm.ToolCall{ID: fmt.Sprintf("call_%d", round), Type: "function"}
			tc.Function.Name = name
			tc.Function.Arguments = "{}"
			// 思维链跟着收尾帧一起过来（真实协议里也是攒到最后这一帧）
			ch <- llm.Chunk{Reasoning: "得先知道现在几点", Done: true, ToolCalls: []llm.ToolCall{tc}}
			return
		}
		ch <- llm.Chunk{Content: "现在是十点。"}
		ch <- llm.Chunk{Done: true}
	}()
	return ch, nil
}

func (p *scriptedProvider) Chat(context.Context, []llm.Message, llm.ChatOptions) (string, error) {
	return "", nil
}

// roundOptions / roundMessages 取出某一轮的记录（越界直接失败，省得每个用例都判一次）。
func (p *scriptedProvider) roundOptions(t *testing.T, round int) llm.ChatOptions {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if round >= len(p.opts) {
		t.Fatalf("只跑了 %d 轮，取不到第 %d 轮", len(p.opts), round+1)
	}
	return p.opts[round]
}

func (p *scriptedProvider) roundMessages(t *testing.T, round int) []llm.Message {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if round >= len(p.seen) {
		t.Fatalf("只跑了 %d 轮，取不到第 %d 轮", len(p.seen), round+1)
	}
	return p.seen[round]
}

func (p *scriptedProvider) rounds() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.opts)
}

// newToolApp 组一个只跑对话链路的 App：历史用内存实现，记忆与嵌入都停用。
//
// 刻意不接记忆：这一组测的是工具循环，而结算与检索各有自己的测试。
// 把它们拖进来只会让失败原因变模糊（比如"正文没落库"其实是结算卡住了）。
func newToolApp(t *testing.T, p llm.Provider) (*App, history.Store, string, string) {
	t.Helper()
	hist := historystore.NewMemoryStore()
	app := NewApp(p, nil, hist, nil, nil, nil)
	// ctx 非 nil 才能跑 stream（它要拿它做取消）；win 仍为 nil，
	// 于是所有推事件都会被 emit 丢掉——见 emit 上那段说明
	app.ctx = context.Background()

	const personaID = "00000000-0000-0000-0000-0000000000a1"
	sess, err := hist.EnsureSession(personaID)
	if err != nil {
		t.Fatalf("准备会话失败: %v", err)
	}
	chunk, err := hist.EnsureChunk(sess.ID, history.ChunkMaxRunes, history.ChunkMaxMessages)
	if err != nil {
		t.Fatalf("准备分片失败: %v", err)
	}
	return app, hist, sess.ID, chunk.ID
}

// assistantText 取出这一片里落库的 assistant 正文（应当只有一条）。
func assistantText(t *testing.T, hist history.Store, sessionID string) string {
	t.Helper()
	msgs, err := hist.SessionMessages(sessionID, 50)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			return msgs[i].Content
		}
	}
	t.Fatal("历史里没有 assistant 消息")
	return ""
}

// toolDeclared 判断声明表里有没有某个工具。用"包含"而不是"只有它"：
// 内置工具集里除了查时间还有联网那两个（联网是否可用取决于设置，不该钉死在测试里）。
func toolDeclared(specs []llm.Tool, name string) bool {
	for _, s := range specs {
		if s.Function.Name == name {
			return true
		}
	}
	return false
}

// 只有耗时工具才提示——查时间是零延迟的，给它也发提示只会让界面闪一下。
func TestToolNoticeOnlyForSlowTools(t *testing.T) {
	if toolNotice("get_current_time") != "" {
		t.Error("查时间不该有提示（零延迟，提示只会闪一下）")
	}
	if toolNotice("web_search") == "" || toolNotice("read_page") == "" {
		t.Error("联网搜索与读网页要提示，否则界面看着像卡住了")
	}
}

// 工具循环的正脸：她要调工具 → 执行 → 结果回灌 → 她接着说 → 收尾。
//
// 这条路上有四件事必须同时成立，缺一件工具就等于没用：
//  1. 请求里真的带上了工具声明（否则模型永远不知道有它）；
//  2. 工具结果以 role=tool + tool_call_id 回灌（协议要求配对，配错服务端会报错）；
//  3. **做过工具调用的那轮 assistant 必须回传思维链**（官方硬规则，只开思考模式才踩得到，
//     而那时报的错与"模型不支持工具"看起来一模一样）；
//  4. 两轮的话拼成一段落库（用户看到的与历史里存的必须一致）。
func TestToolLoopFeedsResultBack(t *testing.T) {
	p := &scriptedProvider{toolRounds: 1}
	app, hist, sessID, chunkID := newToolApp(t, p)

	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "你是 Lapwing。"},
		{Role: llm.RoleUser, Content: "现在几点了"},
	}
	app.stream(context.Background(), "m1", sessID, chunkID, "", msgs, true, recallResult{})

	if got := p.rounds(); got != 2 {
		t.Fatalf("她要一次工具、再收尾，应当正好 2 轮，实际 %d 轮", got)
	}
	if specs := p.roundOptions(t, 0).Tools; len(specs) == 0 {
		t.Fatal("第一轮该带上工具声明（否则她永远不知道有哪些工具）")
	} else if !toolDeclared(specs, "get_current_time") {
		t.Fatalf("工具声明里该有查时间，实际 %+v", specs)
	}

	// ② 工具结果回灌
	second := p.roundMessages(t, 1)
	var toolMsg, asstMsg *llm.Message
	for i := range second {
		switch {
		case second[i].Role == llm.RoleTool:
			toolMsg = &second[i]
		case second[i].Role == llm.RoleAssistant && len(second[i].ToolCalls) == 1:
			asstMsg = &second[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("工具结果没有被回灌——她会看不到任何结果，只能瞎说")
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("结果必须带上它回的是哪一次调用，实际 %q", toolMsg.ToolCallID)
	}
	if !strings.Contains(toolMsg.Content, "现在是") {
		t.Errorf("回灌的该是工具的真实输出：%q", toolMsg.Content)
	}

	// ③ 思维链必须回传（官方硬规则）
	if asstMsg == nil {
		t.Fatal("assistant 那条工具调用消息也要回灌，否则协议不完整")
	}
	if asstMsg.ReasoningContent == "" {
		t.Error("做过工具调用的 assistant 必须回传 reasoning_content——不回传服务端会直接报错")
	}

	// ④ 落库的是两轮拼起来的那段话，而且**没有协议痕迹**
	body := assistantText(t, hist, sessID)
	want := "我看一眼时间。\n现在是十点。"
	if body != want {
		t.Errorf("落库正文不对：\n实际 %q\n期望 %q", body, want)
	}
	if strings.Contains(body, "tool_call") || strings.Contains(body, "{") {
		t.Errorf("历史里不该出现协议 JSON（用户可见）：%q", body)
	}
}

// 到上限就不再给工具：否则模型可能陷进"调工具 → 拿到结果 → 再调同一个"的循环，
// 而每一轮都是一次完整的模型调用（几秒 + 钱）。
func TestToolLoopStopsAtRoundLimit(t *testing.T) {
	p := &scriptedProvider{toolRounds: 99} // 只要有工具它就要求调
	app, hist, sessID, chunkID := newToolApp(t, p)

	msgs := []llm.Message{{Role: llm.RoleUser, Content: "现在几点了"}}
	app.stream(context.Background(), "m1", sessID, chunkID, "", msgs, true, recallResult{})

	// maxToolRounds 轮给了工具，之后那一轮不给（于是她收尾）
	if got, want := p.rounds(), maxToolRounds+1; got != want {
		t.Fatalf("到上限后应当只多跑一轮收尾，实际 %d 轮（期望 %d）", got, want)
	}
	if specs := p.roundOptions(t, maxToolRounds).Tools; len(specs) != 0 {
		t.Errorf("到上限后不该再给工具，实际 %+v", specs)
	}
	// 最要紧的一条：无论如何都要有正文落库，不能"查了半天一个字没说"
	if body := assistantText(t, hist, sessID); strings.TrimSpace(body) == "" {
		t.Error("跑满上限之后仍然要有正文——否则这一轮在历史里就是空的")
	}
}

// 工具执行失败也要回灌**一句话**，而不是让整轮断掉。
//
// 这是 function calling 的常规姿态：模型拿到「这个工具失败了」能自己决定怎么办
// （换个说法、或者老实说查不到）。我们抛错的话，用户看到的是半截回复 + 一个错误气泡。
func TestToolFailureIsFedBackAsText(t *testing.T) {
	p := &scriptedProvider{toolRounds: 1, toolName: "no_such_tool"}
	app, hist, sessID, chunkID := newToolApp(t, p)

	msgs := []llm.Message{{Role: llm.RoleUser, Content: "现在几点了"}}
	app.stream(context.Background(), "m1", sessID, chunkID, "", msgs, true, recallResult{})

	if got := p.rounds(); got != 2 {
		t.Fatalf("工具失败之后也该继续这一轮（让她自己收场），实际 %d 轮", got)
	}
	var toolMsg *llm.Message
	for i := range p.roundMessages(t, 1) {
		if m := p.roundMessages(t, 1)[i]; m.Role == llm.RoleTool {
			toolMsg = &m
		}
	}
	if toolMsg == nil {
		t.Fatal("失败的调用也要回灌一条结果，否则协议里这次调用没人应答")
	}
	if !strings.Contains(toolMsg.Content, "调用失败") {
		t.Errorf("失败原因该以一句话告诉模型：%q", toolMsg.Content)
	}
	if body := assistantText(t, hist, sessID); strings.TrimSpace(body) == "" {
		t.Error("工具失败不该让这一轮没有正文")
	}
}
