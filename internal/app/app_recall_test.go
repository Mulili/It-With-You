package app

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-for-you-love/internal/history"
	historystore "agent-for-you-love/internal/history/store"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	memorystore "agent-for-you-love/internal/memory/store"
	"agent-for-you-love/internal/persona/store"

	"github.com/google/uuid"
)

// newRecallApp 组一个带记忆库与假嵌入器的 App。
//
// 全用内存实现：这一组测的是"检索 → 注入"这条链怎么串，而不是 PG 本身
// （PG 那条路径由 internal/memory/store 的契约测试覆盖）。
func newRecallApp(t *testing.T) (*App, *memorystore.MemoryStore, string) {
	t.Helper()
	personas := store.NewMemoryStore(testBuiltins(), true)
	mems := memorystore.NewMemoryStore()
	app := NewApp(nil, personas, historystore.NewMemoryStore(), mems, &fakeEmbedder{}, nil)
	app.ctx = context.Background() // 真运行时由 Wails 注入；recall 要用它做超时
	return app, mems, personas.Snapshot().ActiveID
}

// seedMemory 往记忆库里塞一条记忆，并指定它的向量落在哪个分量上。
//
// 假嵌入器对单条文本固定返回 e0（见 fakeEmbedder），所以：
// 分量 0 → 与查询相似度 1（必过阈值）；分量 1 → 0（必被滤掉）。
// 这样"阈值到底有没有在起作用"是可以直接断言出来的，不依赖任何模型。
func seedMemory(t *testing.T, s memory.Store, personaID, content string, component int) string {
	t.Helper()
	vec := make([]float32, llm.DefaultEmbedDim)
	vec[component] = 1
	res, err := s.Save(memory.Memory{
		PersonaID: personaID, Kind: memory.KindFact, Content: content,
	}, vec, memory.DefaultDedupThreshold)
	if err != nil {
		t.Fatalf("准备记忆失败: %v", err)
	}
	return res.ID
}

func lastRecalledAt(t *testing.T, s memory.Store, personaID, id string) int64 {
	t.Helper()
	list, err := s.List(personaID, 100)
	if err != nil {
		t.Fatalf("读记忆列表失败: %v", err)
	}
	for _, m := range list {
		if m.ID == id {
			return m.LastRecalledAt
		}
	}
	t.Fatalf("列表里找不到 %s", id)
	return 0
}

// 命中就注入。⚠️ 但**注入本身不该动计数**——计数归口在 markUsed，只有她真的用到了才 +1。
//
// 理由见 recall 里那段注释：一条被看了十轮、一次都没提的记忆，按"注入就记"会把门槛顶到最高，
// 结果**你以后问起它反而拿不到了**。用户的原话是"按照采取次数累加"。
func TestRecallInjectsHitWithoutCounting(t *testing.T) {
	app, mems, personaID := newRecallApp(t)

	hit := seedMemory(t, mems, personaID, "用户有一把 HHKB 键盘", 0)
	miss := seedMemory(t, mems, personaID, "用户养了一只猫", 1)

	got := app.recallText(personaID, "", []llm.Message{{Role: llm.RoleUser, Content: "我那个键盘到了"}})
	if !strings.Contains(got, "用户有一把 HHKB 键盘") {
		t.Errorf("相关的记忆应当被注入，实际：\n%s", got)
	}
	// 相似度 0（正交）远低于阈值：它不该出现。阈值这个旋钮就是靠这条钉住的
	if strings.Contains(got, "用户养了一只猫") {
		t.Errorf("低于阈值的记忆不该被注入，实际：\n%s", got)
	}
	// 注入块必须写明「可能无关、没关系就别提」，否则她会把每条都当任务汇报
	if !strings.Contains(got, "没关系就当没想起来") {
		t.Errorf("注入块缺少「允许忽略」的说明，实际：\n%s", got)
	}
	// 还要教她自标用了哪几条——那是计数归口的唯一依据
	if !strings.Contains(got, "[[used:") {
		t.Errorf("注入块缺少「自标用到了哪几条」的说明，实际：\n%s", got)
	}

	if lastRecalledAt(t, mems, personaID, hit) != 0 {
		t.Error("**注入本身不该动计数**——否则看了十轮、一次没提的也会被顶到最高门槛")
	}
	if lastRecalledAt(t, mems, personaID, miss) != 0 {
		t.Error("没被注入的记忆不该被动到")
	}
}

// 计数只在"她真的用到了"时才涨，而且要能穿过编号对照表落到正确的那条记忆上。
//
// 另一件事同样重要：**编号越界、写成 0 或负数，都只该被忽略**，不能 panic、
// 更不能记到别人头上——那都是模型的小失误，不该影响这一轮。
//
// ⚠️ 这里只放一条记忆：两条向量相同的记忆会被 Save 的去重（阈值 0.92）合并成一条，
// 那样"第 2 条"根本不存在，测不出"没标到的没被动"。
func TestMarkUsedCountsOnlyWhatSheUsed(t *testing.T) {
	app, mems, personaID := newRecallApp(t)

	id := seedMemory(t, mems, personaID, "用户有一把 HHKB 键盘", 0)
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "我那个键盘到了"}}

	rec := app.recall(personaID, "", msgs)
	if len(rec.Refs) != 1 {
		t.Fatalf("应当恰好一条编号对照，实际 %d 条：%+v", len(rec.Refs), rec.Refs)
	}
	if rec.Refs[0].MemoryID != id {
		t.Fatalf("编号 1 应当指向刚才那条记忆，实际 %+v", rec.Refs[0])
	}

	// 标了"用到了第 1 条" → 记一次
	app.markUsed(rec.Refs, []int{1}, "")
	if lastRecalledAt(t, mems, personaID, id) == 0 {
		t.Error("标了用到的应当记一次")
	}

	// 越界 / 非法编号：忽略即可（这里只验它不 panic、不把编号算错）
	app.markUsed(rec.Refs, []int{99, 0, -1}, "")
}

// 标记的剥离：模型写了就照它记，没写、写坏了都不能影响正文。
func TestStripRecallUsage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		body string
		used []int
	}{
		{"没写标记", "今天天气不错", "今天天气不错", nil},
		{"写在末尾", "好呀\n[[used:1,3]]", "好呀", []int{1, 3}},
		{"标记后还有话也一并丢掉", "好呀\n[[used:2]] 对了", "好呀", []int{2}},
		{"只有一个编号", "嗯\n[[used:2]]", "嗯", []int{2}},
		{"非数字忽略", "嗯\n[[used:a,2]]", "嗯", []int{2}},
		{"没闭合按没标处理", "嗯\n[[used:1", "嗯", nil},
		{"正文里出现半截不算标记", "他说 [[used 是个标记", "他说 [[used 是个标记", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, used := stripRecallUsage(c.in)
			if body != c.body {
				t.Errorf("正文应当是 %q，实际 %q", c.body, body)
			}
			if len(used) != len(c.used) {
				t.Fatalf("编号应当是 %v，实际 %v", c.used, used)
			}
			for i := range used {
				if used[i] != c.used[i] {
					t.Fatalf("编号应当是 %v，实际 %v", c.used, used)
				}
			}
		})
	}
}

// 流式推送时，标记不能被"提前"送出去——它是逐块来的，可能正好把 "[[used:" 切开。
func TestSafePrefixLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"hello", 5},         // 没标记：全推
		{"hi[[used:1]]", 2},  // 见到完整标记：只推它之前
		{"hi[[use", 2},       // 尾巴可能是标记前缀：扣住
		{"hi[", 2},           // 一个 "[" 也要扣
		{"hi", 2},            // 与标记无关：全推
		{"x[[used:2]]yz", 1}, // 标记之后的内容一律不推
	}
	for _, c := range cases {
		if got := safePrefixLen(c.in); got != c.want {
			t.Errorf("safePrefixLen(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 片索引线：命中的是"你们以前聊过什么"，注入的是抽取式摘要，并且**带时间**——
// 摘要本身不带时间，而"上次""前几天"这类说法全靠它。
func TestRecallInjectsPastConversationWithDate(t *testing.T) {
	app, mems, personaID := newRecallApp(t)

	vec := make([]float32, llm.DefaultEmbedDim)
	vec[0] = 1
	if err := mems.IndexChunk(memory.ChunkIndex{
		ChunkID: uuid.NewString(), SessionID: uuid.NewString(), PersonaID: personaID,
		Summary: "主题：聊新买的键盘\n- 用户买了 HHKB：「我上周末买了个 HHKB」",
	}, vec); err != nil {
		t.Fatalf("准备片索引失败: %v", err)
	}

	got := app.recallText(personaID, "", []llm.Message{{Role: llm.RoleUser, Content: "键盘用得怎么样"}})
	for _, want := range []string{"以前聊过", "主题：聊新买的键盘", "我上周末买了个 HHKB", "今天"} {
		if !strings.Contains(got, want) {
			t.Errorf("注入块里应当包含 %q，实际：\n%s", want, got)
		}
	}
}

// 什么都不够像时**不注入**（也不写回想时刻）：宁可这一轮不想，也别硬塞无关内容。
func TestRecallReturnsEmptyWhenNothingPasses(t *testing.T) {
	app, mems, personaID := newRecallApp(t)

	miss := seedMemory(t, mems, personaID, "完全无关的事", 1)
	if got := app.recallText(personaID, "", []llm.Message{{Role: llm.RoleUser, Content: "在吗"}}); got != "" {
		t.Errorf("没有候选过阈值时不该注入，实际：\n%s", got)
	}
	if lastRecalledAt(t, mems, personaID, miss) != 0 {
		t.Error("没注入就不该动 last_recalled_at")
	}
}

// 嵌入服务不可用时记忆功能整体停用：不报错、不注入，对话照常。
func TestRecallDisabledWithoutEmbedder(t *testing.T) {
	app := NewApp(nil, nil, historystore.NewMemoryStore(), memorystore.NewMemoryStore(), nil, nil)
	app.ctx = context.Background()

	if got := app.recallText("00000000-0000-0000-0000-000000000001", "",
		[]llm.Message{{Role: llm.RoleUser, Content: "在吗"}}); got != "" {
		t.Errorf("没有嵌入服务时不该注入，实际：%q", got)
	}
}

// recallText 是测试用的薄封装：这些用例只关心"注入了什么文本"，
// 不关心"编号 → ID"那张对照表（那是 markUsed 的事）。
func (a *App) recallText(personaID, sessionID string, msgs []llm.Message) string {
	return a.recall(personaID, sessionID, msgs).Text
}

// 门槛的曲线形状（"先陡后缓"）以及它与基础阈值的关系。
//
// ⚠️ recallCountPenalty 只有 0.06 —— 上限被实测卡死（"他直接问起"也只有 0.53）。
// 所以这条曲线**只挡得住"话题近邻"那一档**，详见那个常量的注释。
func TestRecallThresholdRisesWithRecallCount(t *testing.T) {
	const session = "00000000-0000-0000-0000-0000000000a1"

	// 这两条与 penalty 取值无关，任何时候都成立：没提过、换了会话 → 基础阈值
	if got := recallThreshold("", 0, session); got != recallMinScore {
		t.Errorf("没提过时应当用基础阈值，实际 %.2f", got)
	}
	if got := recallThreshold(session, 5, "00000000-0000-0000-0000-0000000000b1"); got != recallMinScore {
		t.Errorf("换一段对话应当用基础阈值，实际 %.2f", got)
	}

	if recallCountPenalty == 0 {
		// 万一有人把 penalty 归零（比如换了模型后想先关掉）：门槛必须退回基础值，
		// 而不是留下"提过就永远抬着"的残迹。
		for n := 1; n <= 10; n++ {
			if got := recallThreshold(session, n, session); got != recallMinScore {
				t.Errorf("penalty 为 0 时门槛不该变：提过 %d 次得到 %.3f", n, got)
			}
		}
		return
	}

	// 以下是 penalty 非零时的形状，供将来启用时守着
	prev := recallThreshold(session, 0, session)
	for n := 1; n <= 10; n++ {
		th := recallThreshold(session, n, session)
		if th <= prev {
			t.Fatalf("门槛必须随次数上升：第 %d 次 %.3f 没有超过上一次 %.3f", n, th, prev)
		}
		if th > recallMinScore+recallCountPenalty+0.001 {
			t.Fatalf("门槛不该超过上限：第 %d 次 %.3f", n, th)
		}
		prev = th
	}

	// "先陡后缓"：第 1 次抬的幅度必须大于第 2 次
	first := recallThreshold(session, 1, session) - recallMinScore
	second := recallThreshold(session, 2, session) - recallThreshold(session, 1, session)
	if first <= second {
		t.Errorf("应当先陡后缓：第 1 次抬了 %.3f，第 2 次只抬 %.3f", first, second)
	}
}

// 用到过之后，同一段对话里再注入就**降级为极弱措辞**——内容照给，但别主动说。
//
// 起因是一次真实观察：连着聊同一个话题时，她**每一句都回扣**那件事，很刻意。
// 但"提过就不再注入"是错的：那会让她**不知道**，然后开始编。
// 用户给过反例——连着问"草莓 / 芒果 / 菠萝三种口味喜不喜欢"，第三次起若不再注入，
// 她会答"喜欢"。**编出来的答案比重复提一句有害得多。**
func TestRecallDowngradesRatherThanForgets(t *testing.T) {
	app, mems, personaID := newRecallApp(t)
	seedMemory(t, mems, personaID, "用户有一把 HHKB 键盘", 0)
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "我那个键盘到了"}}

	const (
		sessionA = "00000000-0000-0000-0000-0000000000a1"
		sessionB = "00000000-0000-0000-0000-0000000000b1"
	)

	// 第一次：正常档。她还没用到过，措辞就不该降级
	first := app.recall(personaID, sessionA, msgs)
	if !strings.Contains(first.Text, "HHKB") {
		t.Fatalf("第一次应当注入，实际：\n%s", first.Text)
	}
	if strings.Contains(first.Text, "只当背景资料") {
		t.Errorf("还没被用到过，不该降档，实际：\n%s", first.Text)
	}

	// 她标了"用到了"——计数归口在这里
	app.markUsed(first.Refs, []int{1}, sessionA)

	// 之后：仍**带着内容**（他问起时要答得上），但降到"只当背景资料"
	second := app.recallText(personaID, sessionA, msgs)
	if !strings.Contains(second, "HHKB") {
		t.Errorf("降档后仍须带上内容——否则他问起时她答不上来，实际：\n%s", second)
	}
	if !strings.Contains(second, "只当背景资料") || !strings.Contains(second, "只有他明确问到时") {
		t.Errorf("用到过之后应当降档，实际：\n%s", second)
	}

	// ⚠️ **反复追问也必须一直看得到内容**（用户举的正是这个场景：草莓 / 芒果 / 菠萝）
	for i := 0; i < 4; i++ {
		r := app.recall(personaID, sessionA, msgs)
		if !strings.Contains(r.Text, "HHKB") {
			t.Fatalf("第 %d 次追问仍须带内容（丢了她就只能编），实际：\n%s", i+3, r.Text)
		}
		app.markUsed(r.Refs, []int{1}, sessionA) // 模拟她每一轮都真的用到了
	}
	// 到"反复"门槛之后会多出那句标记
	if final := app.recallText(personaID, sessionA, msgs); !strings.Contains(final, "反复提过很多次") {
		t.Errorf("反复用到之后应当带上那句标记，实际：\n%s", final)
	}

	// 换一段对话回到正常档（"上次我们聊过…"是期望行为）
	third := app.recallText(personaID, sessionB, msgs)
	if !strings.Contains(third, "HHKB") || strings.Contains(third, "只当背景资料") {
		t.Errorf("换一段对话应当回到正常注入，实际：\n%s", third)
	}
}

// 往事那一侧同样：用到过之后降级为极弱措辞，但**内容永不丢弃**。
//
// ⚠️ 包括"本段对话自己的片"——它的摘要并不在上下文里（上下文只发当前片），
// 丢掉她就会在"你上次说的那件事"面前哑口无言、甚至开始编。
func TestRecallDowngradesPastConversationInSameSession(t *testing.T) {
	app, mems, personaID := newRecallApp(t)

	vec := make([]float32, llm.DefaultEmbedDim)
	vec[0] = 1
	if err := mems.IndexChunk(memory.ChunkIndex{
		ChunkID:   "00000000-0000-0000-0000-0000000000f1",
		SessionID: "00000000-0000-0000-0000-0000000000f2", // 刻意不等于下面两个会话
		PersonaID: personaID,
		Summary:   "主题：聊过西瓜味冰棍",
	}, vec); err != nil {
		t.Fatalf("准备片索引失败: %v", err)
	}
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "我那个键盘到了"}}

	const (
		sessionA = "00000000-0000-0000-0000-0000000000a1"
		sessionB = "00000000-0000-0000-0000-0000000000b1"
	)

	first := app.recall(personaID, sessionA, msgs)
	if !strings.Contains(first.Text, "西瓜味冰棍") || strings.Contains(first.Text, "只当背景资料") {
		t.Fatalf("第一次应当正常注入这段往事，实际：\n%s", first.Text)
	}
	app.markUsed(first.Refs, []int{1}, sessionA)

	second := app.recallText(personaID, sessionA, msgs)
	if !strings.Contains(second, "西瓜味冰棍") {
		t.Errorf("降档后仍须带上往事内容（他问起时要答得上），实际：\n%s", second)
	}
	if !strings.Contains(second, "只当背景资料") {
		t.Errorf("用到过之后应当降档，实际：\n%s", second)
	}

	third := app.recallText(personaID, sessionB, msgs)
	if !strings.Contains(third, "西瓜味冰棍") || strings.Contains(third, "只当背景资料") {
		t.Errorf("换一段对话应当回到正常注入，实际：\n%s", third)
	}
}

// 回忆的位置：**插在最后一条消息之前**，而不是接在人格提示词后面。
//
// 这条盯的是前缀缓存：人格 + 历史才是"每轮都一样"的前缀，
// 回忆每轮都在变，接在前面等于把整个前缀作废（4.7 记过这个坑）。
func TestBuildMessagesPutsRecallBeforeLastMessage(t *testing.T) {
	app, _, personaID := newRecallApp(t)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "第一句"},
		{Role: llm.RoleAssistant, Content: "第一答"},
		{Role: llm.RoleUser, Content: "这一轮说的"},
	}
	got := app.buildMessages(personaID, msgs, recallResult{Text: "（回忆块）"})

	if len(got) != 5 {
		t.Fatalf("应当是 人格 + 2 条历史 + 回忆 + 最后一条 = 5 条，实际 %d 条：%+v", len(got), got)
	}
	if got[0].Role != llm.RoleSystem {
		t.Errorf("第一条应当是人格提示词，实际 %s", got[0].Role)
	}
	if got[3].Role != llm.RoleSystem || got[3].Content != "（回忆块）" {
		t.Errorf("回忆应当插在最后一条之前，实际第 4 条是 %+v", got[3])
	}
	if got[4].Content != "这一轮说的" {
		t.Errorf("最后一条应当还是用户刚说的话，实际 %+v", got[4])
	}

	// 没有回忆时不该多出任何一条消息
	if plain := app.buildMessages(personaID, msgs, recallResult{}); len(plain) != 4 {
		t.Errorf("没有回忆时应当只有 4 条，实际 %d 条", len(plain))
	}
}

// 顶部状态条的数字必须与**真的发出去的东西**对得上。
//
// 这条同时钉住"按字符数、不按字节数"：中文写成字节数会整体大三倍，各段比例就全错了。
func TestContextStatMatchesWhatWasSent(t *testing.T) {
	app, _, personaID := newRecallApp(t)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "第一句"},
		{Role: llm.RoleAssistant, Content: "第一答"},
		{Role: llm.RoleUser, Content: "这一轮说的"},
	}
	rec := recallResult{Text: "（回忆块）", Facts: 2, Chunks: 1}
	app.buildMessages(personaID, msgs, rec)

	stat := app.ContextStat()
	if stat.Persona == 0 {
		t.Error("人格那一段不该为 0")
	}
	// "第一句" + "第一答" = 6 个字符（写成字节数会是 18）
	if stat.History != 6 {
		t.Errorf("历史那一段应当是 6 个字符，实际 %d", stat.History)
	}
	if stat.Prompt != 5 {
		t.Errorf("本轮那句话应当是 5 个字符，实际 %d", stat.Prompt)
	}
	if stat.Recall != utf8.RuneCountInString(rec.Text) {
		t.Errorf("回忆那一段应当按字符数算，实际 %d", stat.Recall)
	}
	if stat.RecallFacts != 2 || stat.RecallChunks != 1 {
		t.Errorf("回忆条数应当照抄 recallResult，实际 %d 条事实 / %d 段往事", stat.RecallFacts, stat.RecallChunks)
	}
	if stat.Total != stat.Persona+stat.History+stat.Recall+stat.Prompt {
		t.Errorf("合计应当等于四段之和，实际 %d", stat.Total)
	}
	if stat.Capacity != history.ChunkMaxRunes || stat.MessageLimit != history.ContextMessagesLimit {
		t.Errorf("「满」的参照应当是片的两个上限，实际 %d 字 / %d 条", stat.Capacity, stat.MessageLimit)
	}
	if stat.Messages != len(msgs) {
		t.Errorf("条数应当是 %d，实际 %d", len(msgs), stat.Messages)
	}
}

// clip 是给日志用的：压成一行 + 按**字符**截断。
//
// 单独测它是因为"按字符"这三个字：写成按字节截断的话，中文会被从中间劈开，
// 日志里就是乱码——而日志恰恰是排查时才看的东西，最不该在那种时候坏掉。
func TestClipKeepsRunesIntact(t *testing.T) {
	if got := clip("今天好累啊", 10); got != "今天好累啊" {
		t.Errorf("不足上限时应当原样返回，实际 %q", got)
	}
	// 5 个中文字 = 15 字节；按字节截断会得到半个字（乱码）
	if got := clip("今天好累啊", 3); got != "今天好…" {
		t.Errorf("应当按字符截断，实际 %q", got)
	}
	if got := clip("第一行\n第二行", 40); got != "第一行 第二行" {
		t.Errorf("换行应当压成空格（否则一条日志会散成好几行），实际 %q", got)
	}
}

// 时间要按**自然日**说人话：昨晚聊的，今天早上提该是"昨天"而不是"今天"。
func TestRelativeDay(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"今天", now, "今天"},
		{"昨天", now.AddDate(0, 0, -1), "昨天"},
		{"三天前", now.AddDate(0, 0, -3), "3 天前"},
		{"一周以上给日期", now.AddDate(0, 0, -30), now.AddDate(0, 0, -30).Format("2006-01-02")},
		{"没有时间", time.Time{}, "以前"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeDay(tc.at.UnixMilli()); got != tc.want {
				t.Errorf("relativeDay = %q，期望 %q", got, tc.want)
			}
		})
	}
}
