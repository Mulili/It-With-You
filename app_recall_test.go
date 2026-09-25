package main

import (
	"context"
	"strings"
	"testing"
	"time"

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
	app := NewApp(nil, personas, historystore.NewMemoryStore(), mems, &fakeEmbedder{})
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

// 命中就注入，并且**记下"刚被想起过"**——那是"别反复提同一件事"的唯一依据。
func TestRecallInjectsHitAndMarksIt(t *testing.T) {
	app, mems, personaID := newRecallApp(t)

	hit := seedMemory(t, mems, personaID, "用户有一把 HHKB 键盘", 0)
	miss := seedMemory(t, mems, personaID, "用户养了一只猫", 1)

	got := app.recall(personaID, []llm.Message{{Role: llm.RoleUser, Content: "我那个键盘到了"}})
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

	if lastRecalledAt(t, mems, personaID, hit) == 0 {
		t.Error("被注入的记忆应当写上 last_recalled_at")
	}
	if lastRecalledAt(t, mems, personaID, miss) != 0 {
		t.Error("没被注入的记忆不该被动到")
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

	got := app.recall(personaID, []llm.Message{{Role: llm.RoleUser, Content: "键盘用得怎么样"}})
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
	if got := app.recall(personaID, []llm.Message{{Role: llm.RoleUser, Content: "在吗"}}); got != "" {
		t.Errorf("没有候选过阈值时不该注入，实际：\n%s", got)
	}
	if lastRecalledAt(t, mems, personaID, miss) != 0 {
		t.Error("没注入就不该动 last_recalled_at")
	}
}

// 嵌入服务不可用时记忆功能整体停用：不报错、不注入，对话照常。
func TestRecallDisabledWithoutEmbedder(t *testing.T) {
	app := NewApp(nil, nil, historystore.NewMemoryStore(), memorystore.NewMemoryStore(), nil)
	app.ctx = context.Background()

	if got := app.recall("00000000-0000-0000-0000-000000000001",
		[]llm.Message{{Role: llm.RoleUser, Content: "在吗"}}); got != "" {
		t.Errorf("没有嵌入服务时不该注入，实际：%q", got)
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
	got := app.buildMessages(personaID, msgs, "（回忆块）")

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
	if plain := app.buildMessages(personaID, msgs, ""); len(plain) != 4 {
		t.Errorf("没有回忆时应当只有 4 条，实际 %d 条", len(plain))
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
