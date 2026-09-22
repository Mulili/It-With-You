package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"agent-for-you-love/internal/history"
	historystore "agent-for-you-love/internal/history/store"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	memorystore "agent-for-you-love/internal/memory/store"
)

// 收尾结算的人格 ID。内存实现没有外键，所以随便一个 uuid 都行。
const testSettlePersona = "00000000-0000-0000-0000-0000000000e1"

// settleGoodJSON 是一份"模型正常发挥"的输出：两条要点、两条事实（一条公共一条私有）。
const settleGoodJSON = `{
  "title": "聊新买的键盘",
  "topic": "这段主要在聊新买的 HHKB 键盘",
  "key_points": [
    {"point": "用户买了 HHKB", "quote": "我上周末买了个 HHKB，静电容的"}
  ],
  "facts": [
    {"content": "用户有一把 HHKB 键盘", "about": "user", "kind": "fact", "importance": 3,
     "quote": "我上周末买了个 HHKB，静电容的"},
    {"content": "用户答应过下次一起挑键帽", "about": "persona", "kind": "promise", "importance": 4,
     "quote": "敲起来那种顿挫感我挺喜欢的"}
  ],
  "unresolved": []
}`

// fakeProvider 依次返回预置的输出（用完之后重复最后一个），并记录调用次数——
// 调用次数是"这一轮到底有没有去调模型"的唯一可观测证据。
type fakeProvider struct {
	mu    sync.Mutex
	outs  []string
	calls int
}

func (p *fakeProvider) Chat(context.Context, []llm.Message, llm.ChatOptions) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	if len(p.outs) == 0 {
		return "", errors.New("测试没有准备输出")
	}
	out := p.outs[0]
	if len(p.outs) > 1 {
		p.outs = p.outs[1:]
	}
	return out, nil
}

func (p *fakeProvider) ChatStream(context.Context, []llm.Message, llm.ChatOptions) (<-chan llm.Chunk, error) {
	return nil, errors.New("收尾结算走的是非流式的 Chat")
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// fakeEmbedder 用"第 i 条文本在第 i 个分量上为 1"的向量：既满足维度要求，
// 又让不同条目天然不相似（否则会被记忆去重合并成一条，测试就看不出写了几条）。
type fakeEmbedder struct {
	mu    sync.Mutex
	fail  bool
	calls int
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.calls++
	if e.fail {
		return nil, errors.New("嵌入服务不可用")
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, llm.DefaultEmbedDim)
		v[i] = 1
		out[i] = v
	}
	return out, nil
}

func (e *fakeEmbedder) Dim() int { return llm.DefaultEmbedDim }

// recordingStore 把片索引的写入记下来。
//
// 为什么需要它：memory.Store 目前没有"读片索引"的方法（检索是第 4 步的事），
// 所以只能从写入侧观察"到底写没写"。
type recordingStore struct {
	memory.Store
	mu     sync.Mutex
	chunks []memory.ChunkIndex
}

func (s *recordingStore) IndexChunk(ci memory.ChunkIndex, vec []float32) error {
	s.mu.Lock()
	s.chunks = append(s.chunks, ci)
	s.mu.Unlock()
	return s.Store.IndexChunk(ci, vec)
}

func (s *recordingStore) indexed() []memory.ChunkIndex {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]memory.ChunkIndex(nil), s.chunks...)
}

// newSettleApp 造一个"刚聊完一段、还没结算"的 App。
//
// 会话与片都由 EndSession 收尾（它顺手收尾当前片），于是这一片正好落在
// 待结算的条件里：已收尾、没摘要、里面有消息。
func newSettleApp(t *testing.T, outs ...string) (*App, *fakeProvider, *fakeEmbedder, *recordingStore, history.Store, history.Chunk) {
	t.Helper()

	hist := historystore.NewMemoryStore()
	mems := &recordingStore{Store: memorystore.NewMemoryStore()}
	prov := &fakeProvider{outs: outs}
	emb := &fakeEmbedder{}

	// 人格传 nil：这一组不关心人格，而 NewApp 对它是判空之后才用的
	app := NewApp(prov, nil, hist, mems, emb)
	// 真运行时由 Wails 注入；settlePending 会判空，所以这里必须给一个
	app.ctx = context.Background()

	sess, err := hist.EnsureSession(testSettlePersona)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	chunk, err := hist.EnsureChunk(sess.ID, history.ChunkMaxRunes)
	if err != nil {
		t.Fatalf("取当前片失败: %v", err)
	}
	for i, text := range []string{"我上周末买了个 HHKB，静电容的", "敲起来那种顿挫感我挺喜欢的"} {
		if _, err := hist.AppendMessage(history.Message{
			SessionID: sess.ID, ChunkID: chunk.ID, PersonaID: testSettlePersona,
			Role: llm.RoleUser, Content: text, Status: history.StatusOK,
			// 显式错开时间戳：毫秒精度下连着写会落在同一毫秒，而"同毫秒内的顺序"没有保证
			CreatedAt: history.NowMillis() + int64(i),
		}); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
	}
	if err := hist.EndSession(sess.ID); err != nil {
		t.Fatalf("结束会话失败: %v", err)
	}
	return app, prov, emb, mems, hist, chunk
}

// chunkSummaryOf 读回某一片当前的摘要。
func chunkSummaryOf(t *testing.T, hist history.Store, chunk history.Chunk) string {
	t.Helper()
	chunks, err := hist.ListChunks(chunk.SessionID)
	if err != nil {
		t.Fatalf("读片列表失败: %v", err)
	}
	for _, c := range chunks {
		if c.ID == chunk.ID {
			return c.Summary
		}
	}
	t.Fatal("列表里找不到那片")
	return ""
}

// 一次结算该产出的三样东西：片摘要、片索引、待存事实。
func TestSettlePendingWritesSummaryIndexAndFacts(t *testing.T) {
	app, prov, emb, mems, hist, chunk := newSettleApp(t, settleGoodJSON)
	app.settlePending()

	summary := chunkSummaryOf(t, hist, chunk)
	if strings.TrimSpace(summary) == "" {
		t.Fatal("片摘要没写进去：结算等于没发生")
	}
	if !strings.Contains(summary, "主题：") || !strings.Contains(summary, "我上周末买了个 HHKB") {
		t.Errorf("摘要应当是抽取式渲染（主题 + 带原话的要点），实际：\n%s", summary)
	}

	indexed := mems.indexed()
	if len(indexed) != 1 {
		t.Fatalf("应当写 1 条片索引，实际 %d 条", len(indexed))
	}
	if indexed[0].ChunkID != chunk.ID || indexed[0].Summary != summary {
		t.Errorf("片索引的摘要必须与片里存的那份完全一致（命中后注入的就是它）：%+v", indexed[0])
	}

	facts, err := mems.List(testSettlePersona, 10)
	if err != nil {
		t.Fatalf("读记忆失败: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("应当存下 2 条事实，实际 %d 条", len(facts))
	}
	var public, private int
	for _, f := range facts {
		// 依据必须是原话：将来核对"这条记忆从哪儿来"时，只有原话能回到原文里对
		if f.Evidence == "" {
			t.Errorf("记忆没有留下原话依据：%+v", f)
		}
		if f.PersonaID == "" {
			public++
		} else if f.PersonaID == testSettlePersona {
			private++
		} else {
			t.Errorf("记忆归属了错误的人格：%q", f.PersonaID)
		}
	}
	if public != 1 || private != 1 {
		t.Errorf("about=user 的该写公共、about=persona 的该写私有，实际公共 %d 私有 %d", public, private)
	}

	if prov.callCount() != 1 {
		t.Errorf("一次结算只该调一次模型，实际 %d 次", prov.callCount())
	}
	// 摘要与全部事实要一次嵌完：逐条发请求就是十几次网络往返
	if emb.calls != 1 {
		t.Errorf("摘要与事实应当一次嵌入算完，实际调了 %d 次", emb.calls)
	}
}

// 写库顺序：嵌入失败时**什么都不该落**，且这一片要留在待结算里等下一轮重放。
//
// 这条钉的正是"摘要是提交点、必须最后写"：若摘要先写，这一片就再也扫不到了，
// 那些事实**永久丢失而且无声**——这是本项目里最不能接受的一类错。
func TestSettleChunkIsReplayableWhenEmbeddingFails(t *testing.T) {
	app, _, emb, mems, hist, chunk := newSettleApp(t, settleGoodJSON)

	emb.fail = true
	app.settlePending()

	if got := chunkSummaryOf(t, hist, chunk); got != "" {
		t.Fatalf("嵌入失败时不该写下摘要，否则这一片再也扫不到。实际写入了：%q", got)
	}
	if facts, err := mems.List(testSettlePersona, 10); err != nil {
		t.Fatalf("读记忆失败: %v", err)
	} else if len(facts) != 0 {
		t.Errorf("嵌入失败时不该留下记忆，实际 %d 条", len(facts))
	}

	// 服务恢复后重放：整轮补齐（去重让重放是幂等的，不会存重）
	emb.fail = false
	app.settlePending()

	if got := chunkSummaryOf(t, hist, chunk); strings.TrimSpace(got) == "" {
		t.Fatal("重放之后摘要应当补上")
	}
	facts, err := mems.List(testSettlePersona, 10)
	if err != nil {
		t.Fatalf("读记忆失败: %v", err)
	}
	if len(facts) != 2 {
		t.Errorf("重放之后应当有 2 条事实（而不是 4 条），实际 %d 条", len(facts))
	}
}

// 输出不可用（不是 json）：不落任何东西，留在待结算里；但**本进程内不再重试**——
// 这类失败是确定的（同样的输入还是同一份垃圾输出），每轮重试只是白烧调用。
func TestSettleGivesUpOnUnusableOutput(t *testing.T) {
	app, prov, _, mems, hist, chunk := newSettleApp(t, "我觉得这段聊得挺好")

	app.settlePending()
	if prov.callCount() != 1 {
		t.Fatalf("第一次应当调一次模型，实际 %d 次", prov.callCount())
	}

	app.settlePending()
	if prov.callCount() != 1 {
		t.Errorf("同一片不该反复重试，实际调了 %d 次", prov.callCount())
	}
	if got := chunkSummaryOf(t, hist, chunk); got != "" {
		t.Errorf("输出不可用时不该写摘要，实际 %q", got)
	}
	if indexed := mems.indexed(); len(indexed) != 0 {
		t.Errorf("输出不可用时不该写片索引，实际 %d 条", len(indexed))
	}
}

// 嵌入服务整体不可用时，结算直接不启动：抽了却嵌不出向量，等于白花一次调用。
func TestSettleDisabledWithoutEmbedder(t *testing.T) {
	hist := historystore.NewMemoryStore()
	mems := &recordingStore{Store: memorystore.NewMemoryStore()}
	prov := &fakeProvider{outs: []string{settleGoodJSON}}

	app := NewApp(prov, nil, hist, mems, nil) // embedder = nil
	app.ctx = context.Background()

	sess, err := hist.EnsureSession(testSettlePersona)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	chunk, err := hist.EnsureChunk(sess.ID, history.ChunkMaxRunes)
	if err != nil {
		t.Fatalf("取当前片失败: %v", err)
	}
	if _, err := hist.AppendMessage(history.Message{
		SessionID: sess.ID, ChunkID: chunk.ID, PersonaID: testSettlePersona,
		Role: llm.RoleUser, Content: "随便说点什么", Status: history.StatusOK,
	}); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	if err := hist.EndSession(sess.ID); err != nil {
		t.Fatalf("结束会话失败: %v", err)
	}

	app.settlePending()

	if prov.callCount() != 0 {
		t.Errorf("没有嵌入服务时不该调模型，实际 %d 次", prov.callCount())
	}
	if got := chunkSummaryOf(t, hist, chunk); got != "" {
		t.Errorf("没有嵌入服务时不该写摘要，实际 %q", got)
	}
}
