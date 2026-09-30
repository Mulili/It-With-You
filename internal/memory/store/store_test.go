package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/memory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 契约测试用的人格 ID。
//
// 与 history/store 的测试跑在**同一个库**上，所以 ID 刻意错开（那边是 a001/b001/c001）：
// `go test ./...` 会并行跑不同的包，撞上就会互相清掉对方的数据。
const (
	testPersonaA = "00000000-0000-0000-0000-00000000a002"
	testPersonaB = "00000000-0000-0000-0000-00000000b002"
)

// testContentPrefix 是所有测试记忆共有的前缀。
//
// 用途只有一个：PG 侧跑完按它精确清掉本次写入的数据。不能只靠删人格来级联——
// 测试里会写**公共记忆**（persona_id 为 NULL），它们没有主人可以级联。
const testContentPrefix = "【记忆测试】"

// embedDim 必须与 schema 里的 vector(1024) 一致，否则 PG 直接拒绝这次写入。
const embedDim = 1024

// makeVec 造一个只有单个分量非零的 embedDim 维向量。
//
// 用"单分量"是为了让相似度完全可预测：同 index → 余弦 1，不同 index → 余弦 0。
// 这样契约测试不依赖嵌入服务，在任何机器上都能跑。
func makeVec(index int, scale float32) []float32 {
	v := make([]float32, embedDim)
	v[index] = scale
	return v
}

// makeVecPair 造"第 i 个分量 = a、第 j 个分量 = b"的向量，用来构造**连续可分**的相似度：
// 与查询方向越接近，余弦越大（(1,0) vs (1,0)=1.0、(1,1)=0.707、(1,3)=0.316）。
//
// 分量下标从 20 起：前面那些子测试用的是 0~8，避开它们，排序断言才不会被旧数据干扰。
func makeVecPair(i, j int, a, b float32) []float32 {
	v := make([]float32, embedDim)
	v[i], v[j] = a, b
	return v
}

// testChunk 是片索引测试要用的那一行。
//
// 为什么要把 id 传进契约测试：chunk_index 有外键挂在 session_chunks 上，
// PG 侧必须先真的建出"会话 → 片"这条链，而内存实现没有外键、随便给个 uuid 就行。
type testChunk struct {
	ChunkID   string
	SessionID string
}

// 同一套断言跑两种实现：内存与 PG 的去重语义必须完全一致。
//
// 尤其"可见范围"那条——私有记忆只看得到"公共 + 自己"，两个实现若在这上面分叉，
// 用户在两种存储下会遇到完全不同的记忆行为，而且这种分叉不会报错，只会"记的东西不一样"。
func runStoreContract(t *testing.T, s memory.Store, chunk testChunk) {
	t.Helper()

	save := func(t *testing.T, m memory.Memory, vec []float32) memory.SaveResult {
		t.Helper()
		res, err := s.Save(m, vec, memory.DefaultDedupThreshold)
		if err != nil {
			t.Fatalf("保存记忆失败: %v", err)
		}
		if res.ID == "" {
			t.Fatal("保存后应当返回 ID")
		}
		return res
	}

	t.Run("新记忆会被插入", func(t *testing.T) {
		res := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact,
			Content: testContentPrefix + "用户养了一只叫团子的猫",
		}, makeVec(0, 1))
		if res.Updated {
			t.Error("全新的一条不该被标成更新")
		}
	})

	t.Run("相似且同类型 → 更新已有那条、重要性取大", func(t *testing.T) {
		first := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact, Importance: 3,
			Content: testContentPrefix + "用户养了猫",
		}, makeVec(1, 1))

		// 同方向 → 余弦 1.0，必然命中阈值
		second := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact, Importance: 5,
			Content: testContentPrefix + "用户有一只猫叫团子",
		}, makeVec(1, 2))
		if !second.Updated {
			t.Error("相似且同类型应当更新而不是新增")
		}
		if second.ID != first.ID {
			t.Errorf("应当更新同一条：%s → %s", first.ID, second.ID)
		}

		list, err := s.List(testPersonaA, 100)
		if err != nil {
			t.Fatalf("读列表失败: %v", err)
		}
		var seen bool
		for _, m := range list {
			if m.ID != first.ID {
				continue
			}
			seen = true
			if m.Importance != 5 {
				t.Errorf("重要性应当取两者较大值，实际 %d", m.Importance)
			}
			if m.Content != testContentPrefix+"用户有一只猫叫团子" {
				t.Errorf("内容应当换成新的，实际 %q", m.Content)
			}
		}
		if !seen {
			t.Error("更新后的那条应当还在列表里")
		}
	})

	t.Run("相似但类型不同 → 各自独立", func(t *testing.T) {
		fact := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact,
			Content: testContentPrefix + "用户喜欢猫（事实）",
		}, makeVec(2, 1))
		pref := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindPreference,
			Content: testContentPrefix + "用户喜欢猫（偏好）",
		}, makeVec(2, 1))
		if pref.ID == fact.ID {
			t.Error("类型不同不该合并：相似度高也不是同一件事")
		}
	})

	t.Run("不相似 → 新增", func(t *testing.T) {
		a := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact,
			Content: testContentPrefix + "甲",
		}, makeVec(3, 1))
		b := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact,
			Content: testContentPrefix + "乙",
		}, makeVec(4, 1)) // 与上一条正交 → 余弦 0
		if b.ID == a.ID {
			t.Error("不相似不该合并")
		}
	})

	t.Run("本人格的私有记忆会与公共记忆去重", func(t *testing.T) {
		pub := save(t, memory.Memory{
			Kind:    memory.KindFact, // PersonaID 留空 = 公共
			Content: testContentPrefix + "用户有只猫",
		}, makeVec(5, 1))
		own := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact,
			Content: testContentPrefix + "他又提到那只猫",
		}, makeVec(5, 1))
		if !own.Updated || own.ID != pub.ID {
			t.Error("本人格应当看得见公共记忆，并能与它去重")
		}
	})

	t.Run("跨人格的私有记忆互不可见", func(t *testing.T) {
		a := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindEvent,
			Content: testContentPrefix + "A 的私事",
		}, makeVec(6, 1))
		b := save(t, memory.Memory{
			PersonaID: testPersonaB, Kind: memory.KindEvent,
			Content: testContentPrefix + "B 的私事",
		}, makeVec(6, 1))
		if b.ID == a.ID {
			t.Error("A 的私事不该被 B 看到并合并")
		}
	})

	t.Run("List 只返回可见的，且按时间倒序", func(t *testing.T) {
		list, err := s.List(testPersonaA, 100)
		if err != nil {
			t.Fatalf("读列表失败: %v", err)
		}
		for _, m := range list {
			if m.PersonaID != "" && m.PersonaID != testPersonaA {
				t.Errorf("列表里出现了别人格的私有记忆：%+v", m)
			}
		}
		for i := 1; i < len(list); i++ {
			if list[i-1].CreatedAt < list[i].CreatedAt {
				t.Error("列表应当按创建时间倒序")
			}
		}
	})

	// ---- 检索（第 4 步）----
	//
	// 这一组用"余弦 1.0 / 0.707 / 0.316"三个连续可分的相似度：
	// 断言排序才有意义，而"完全相同的相似度"在 PG 与内存之间**本来就不保证顺序**（见接口注释）。

	t.Run("Search 按相似度倒序取候选", func(t *testing.T) {
		query := makeVec(20, 1)
		for _, tc := range []struct {
			suffix string
			vec    []float32
		}{
			{"最像的", makeVec(20, 1)},
			{"有点像的", makeVecPair(20, 21, 1, 1)},
			{"不太像的", makeVecPair(20, 22, 1, 3)},
		} {
			save(t, memory.Memory{
				PersonaID: testPersonaA, Kind: memory.KindFact,
				Content: testContentPrefix + tc.suffix,
			}, tc.vec)
		}

		hits, err := s.Search(testPersonaA, query, 3)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		if len(hits) != 3 {
			t.Fatalf("应当取回 3 条，实际 %d 条", len(hits))
		}
		for i, want := range []string{"最像的", "有点像的", "不太像的"} {
			if !strings.HasSuffix(hits[i].Memory.Content, want) {
				t.Errorf("第 %d 条应当是 %q，实际 %q", i, want, hits[i].Memory.Content)
			}
		}
		if !(hits[0].Score > hits[1].Score && hits[1].Score > hits[2].Score) {
			t.Errorf("应当按相似度倒序，实际 %.3f / %.3f / %.3f",
				hits[0].Score, hits[1].Score, hits[2].Score)
		}
	})

	t.Run("Search 不会返回别人格的私有记忆", func(t *testing.T) {
		query := makeVec(20, 1)
		// B 名下放一条**同向**的：过滤若没做，它必然是 A 眼里的第一名
		save(t, memory.Memory{
			PersonaID: testPersonaB, Kind: memory.KindFact,
			Content: testContentPrefix + "B 的私事",
		}, makeVec(20, 1))

		hits, err := s.Search(testPersonaA, query, 20)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		for _, h := range hits {
			if h.Memory.PersonaID != "" && h.Memory.PersonaID != testPersonaA {
				t.Errorf("检索结果里出现了别人格的私有记忆：%+v", h.Memory)
			}
		}

		// 反过来，B 自己搜得到它（否则说明是"谁都搜不到"，而不是"隔离正确"）
		own, err := s.Search(testPersonaB, query, 1)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		if len(own) != 1 || !strings.HasSuffix(own[0].Memory.Content, "B 的私事") {
			t.Errorf("B 应当搜得到自己的私事，实际 %+v", own)
		}
	})

	t.Run("SearchChunks 只返回本人格的片索引", func(t *testing.T) {
		query := makeVec(20, 1)
		if err := s.IndexChunk(memory.ChunkIndex{
			ChunkID: chunk.ChunkID, SessionID: chunk.SessionID, PersonaID: testPersonaA,
			Summary: testContentPrefix + "主题：聊过键盘",
		}, makeVec(20, 1)); err != nil {
			t.Fatalf("写片索引失败: %v", err)
		}

		hits, err := s.SearchChunks(testPersonaA, query, 3)
		if err != nil {
			t.Fatalf("检索片索引失败: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("应当命中 1 条片索引，实际 %d 条", len(hits))
		}
		if hits[0].Chunk.ChunkID != chunk.ChunkID {
			t.Errorf("命中的片不对：%s", hits[0].Chunk.ChunkID)
		}
		if hits[0].Score < 0.99 {
			t.Errorf("同向向量应当几乎完全相似，实际 %.3f", hits[0].Score)
		}

		// 片索引是"片属于谁"的严格归属，不像记忆那样有公共概念
		none, err := s.SearchChunks(testPersonaB, query, 3)
		if err != nil {
			t.Fatalf("检索片索引失败: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("别人格不该搜到这段往事，实际 %d 条", len(none))
		}
	})

	t.Run("提过的记忆照样返回，只是带着会话与计数", func(t *testing.T) {
		const (
			sessionA = "00000000-0000-0000-0000-0000000000a1"
			sessionB = "00000000-0000-0000-0000-0000000000b1"
		)
		query := makeVec(41, 1)
		m := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact,
			Content: testContentPrefix + "会话计数用的那条",
		}, makeVec(41, 1))

		// ⚠️ 断言"含我那条"而不是"恰好 1 条"：契约测试共用同一个人格，
		// 库里还有别的子测试写下的数据，而 Search **不做阈值过滤**（阈值是上层的策略）。
		mine := func(t *testing.T) (memory.MemoryHit, bool) {
			t.Helper()
			hits, err := s.Search(testPersonaA, query, 5)
			if err != nil {
				t.Fatalf("检索失败: %v", err)
			}
			for _, h := range hits {
				if h.Memory.ID == m.ID {
					return h, true
				}
			}
			return memory.MemoryHit{}, false
		}

		if h, ok := mine(t); !ok || h.Memory.RecallCount != 0 {
			t.Fatalf("未标记前应当搜得到且计数为 0，实际 %+v", h.Memory)
		}

		// ⚠️ 这个测试的核心：**无论提过几次，它都必须返回**。
		// 曾经做过"提过太多次就不再返回"，被一个反例推翻了——连着问"草莓 / 芒果 / 菠萝
		// 喜不喜欢"，第三次起若不再注入，她会"不知道"，然后开始编。
		// 所以抑制**只改措辞，不决定给不给**（见 memory.Memory.RecallCount）。
		for i := 1; i <= 3; i++ {
			if err := s.MarkRecalled([]string{m.ID}, sessionA); err != nil {
				t.Fatalf("第 %d 次标记失败: %v", i, err)
			}
			h, ok := mine(t)
			if !ok {
				t.Fatalf("提过 %d 次之后**也必须返回**（丢了她就只能编）", i)
			}
			if h.Memory.LastRecalledSession != sessionA || h.Memory.RecallCount != i {
				t.Errorf("第 %d 次标记后应当是 %s/%d，实际 %s/%d",
					i, sessionA, i, h.Memory.LastRecalledSession, h.Memory.RecallCount)
			}
		}

		// 换一段对话：计数从 1 重新开始（"上次我们聊过…"是期望行为）
		if err := s.MarkRecalled([]string{m.ID}, sessionB); err != nil {
			t.Fatalf("换会话标记失败: %v", err)
		}
		if h, _ := mine(t); h.Memory.LastRecalledSession != sessionB || h.Memory.RecallCount != 1 {
			t.Errorf("换会话后计数应当重置为 1，实际 %s/%d",
				h.Memory.LastRecalledSession, h.Memory.RecallCount)
		}
	})

	t.Run("见过的往事照样返回，只是带着会话与计数", func(t *testing.T) {
		const (
			sessionA = "00000000-0000-0000-0000-0000000000c3"
			sessionB = "00000000-0000-0000-0000-0000000000d3"
		)
		query := makeVec(22, 1)
		if err := s.IndexChunk(memory.ChunkIndex{
			ChunkID: chunk.ChunkID, SessionID: chunk.SessionID, PersonaID: testPersonaA,
			Summary: testContentPrefix + "主题：会话计数用的往事",
		}, makeVec(22, 1)); err != nil {
			t.Fatalf("写片索引失败: %v", err)
		}

		mine := func(t *testing.T) (memory.ChunkHit, bool) {
			t.Helper()
			hits, err := s.SearchChunks(testPersonaA, query, 5)
			if err != nil {
				t.Fatalf("检索片索引失败: %v", err)
			}
			for _, h := range hits {
				if h.Chunk.ChunkID == chunk.ChunkID {
					return h, true
				}
			}
			return memory.ChunkHit{}, false
		}

		// ⚠️ **本段对话自己的片也照样返回**：它的摘要并不在上下文里（上下文只发当前片），
		// 丢掉她就会在"你上次说的那件事"面前哑口无言。同理，无论提过几次都不能丢。
		for i := 1; i <= 3; i++ {
			if err := s.MarkChunksRecalled([]string{chunk.ChunkID}, sessionA); err != nil {
				t.Fatalf("第 %d 次标记失败: %v", i, err)
			}
			h, ok := mine(t)
			if !ok {
				t.Fatalf("提过 %d 次之后**也必须返回**（丢了她就只能编）", i)
			}
			if h.Chunk.LastRecalledSession != sessionA || h.Chunk.RecallCount != i {
				t.Errorf("第 %d 次标记后应当是 %s/%d，实际 %s/%d",
					i, sessionA, i, h.Chunk.LastRecalledSession, h.Chunk.RecallCount)
			}
		}

		// 换一段对话：计数从 1 重新开始
		if err := s.MarkChunksRecalled([]string{chunk.ChunkID}, sessionB); err != nil {
			t.Fatalf("换会话标记失败: %v", err)
		}
		if h, _ := mine(t); h.Chunk.LastRecalledSession != sessionB || h.Chunk.RecallCount != 1 {
			t.Errorf("换会话后计数应当重置为 1，实际 %s/%d",
				h.Chunk.LastRecalledSession, h.Chunk.RecallCount)
		}
	})

	t.Run("MarkRecalled 只写下被想起来的那些", func(t *testing.T) {
		if err := s.MarkRecalled(nil, ""); err != nil {
			t.Fatalf("空列表不该报错: %v", err)
		}

		recalled := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindEvent,
			Content: testContentPrefix + "刚刚被想起来的事",
		}, makeVec(30, 1))
		untouched := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindEvent,
			Content: testContentPrefix + "没被想起来的事",
		}, makeVec(31, 1))

		if err := s.MarkRecalled([]string{recalled.ID}, ""); err != nil {
			t.Fatalf("标记失败: %v", err)
		}

		list, err := s.List(testPersonaA, 200)
		if err != nil {
			t.Fatalf("读列表失败: %v", err)
		}
		for _, m := range list {
			switch m.ID {
			case recalled.ID:
				if m.LastRecalledAt == 0 {
					t.Error("被想起来的记忆应当写上 last_recalled_at")
				}
			case untouched.ID:
				if m.LastRecalledAt != 0 {
					t.Error("没被想起来的记忆不该被动到")
				}
			}
		}
	})

	t.Run("Delete 只删掉指定那一条", func(t *testing.T) {
		keep := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindPreference,
			Content: testContentPrefix + "留着的那条",
		}, makeVec(32, 1))
		gone := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindPreference,
			Content: testContentPrefix + "要删掉的那条",
		}, makeVec(33, 1))

		if err := s.Delete(gone.ID); err != nil {
			t.Fatalf("删除失败: %v", err)
		}

		list, err := s.List(testPersonaA, 200)
		if err != nil {
			t.Fatalf("读列表失败: %v", err)
		}
		var sawKeep, sawGone bool
		for _, m := range list {
			switch m.ID {
			case keep.ID:
				sawKeep = true
			case gone.ID:
				sawGone = true
			}
		}
		if !sawKeep {
			t.Error("删一条不该动到别的")
		}
		if sawGone {
			t.Error("被删的那条还在")
		}

		// 删两次是同一次的结果：界面上的重复点击不该报错（见接口注释）
		if err := s.Delete(gone.ID); err != nil {
			t.Errorf("删已经不存在的不该报错: %v", err)
		}
	})

	t.Run("DeletePersona 只删私有、公共保留", func(t *testing.T) {
		pubID := save(t, memory.Memory{
			Kind: memory.KindFact, Content: testContentPrefix + "该留下的公共事实",
		}, makeVec(7, 1)).ID
		privID := save(t, memory.Memory{
			PersonaID: testPersonaA, Kind: memory.KindFact,
			Content: testContentPrefix + "该被删掉的私事",
		}, makeVec(8, 1)).ID

		if err := s.DeletePersona(testPersonaA); err != nil {
			t.Fatalf("删除失败: %v", err)
		}

		list, err := s.List(testPersonaA, 100)
		if err != nil {
			t.Fatalf("读列表失败: %v", err)
		}
		var pubKept bool
		for _, m := range list {
			if m.ID == privID {
				t.Error("该人格的私有记忆应当被删掉")
			}
			if m.ID == pubID {
				pubKept = true
			}
		}
		if !pubKept {
			t.Error("公共记忆不该被删：人格没了，但事实仍然成立")
		}
	})
}

func TestMemoryStoreContract(t *testing.T) {
	// 内存实现没有外键，片 id 随便给一组 uuid 就行
	runStoreContract(t, NewMemoryStore(), testChunk{ChunkID: uuid.NewString(), SessionID: uuid.NewString()})
	// 规则索引同理：内存实现没有 persona_rules 可挂
	runRuleIndexContract(t, NewMemoryStore(), testRule{RuleID: uuid.NewString(), PersonaID: testPersonaA})
}

// testRule 是规则索引测试要用的那一行。
//
// rule_index 有外键挂在 persona_rules 上，所以 PG 侧必须先真的建出这条规则；
// 内存实现没有外键，随便给个 uuid 就行（与 testChunk 同一个套路）。
type testRule struct {
	RuleID    string
	PersonaID string
}

// runRuleIndexContract 是**归档规则索引**的契约：写入/覆盖/删除、版本表、按人格隔离。
//
// 为什么它值得一套契约：这是"降层不等于删除"唯一的落点。索引一旦分叉，
// 用户在 PG 上"想得起来"、在内存里想不起来（或反过来），而症状只是
// "她有时候记得、有时候不记得"——最难定位的那一类。
//
// 三个子测试按顺序依赖同一条索引（与片索引那组同一姿态）。
func runRuleIndexContract(t *testing.T, s memory.Store, r testRule) {
	t.Helper()

	t.Run("规则索引：写入后能按人格检索到，别的人格看不到", func(t *testing.T) {
		if err := s.IndexRule(memory.RuleIndex{
			RuleID: r.RuleID, PersonaID: r.PersonaID,
			Content: "语气：别太正经", SourceUpdatedAt: 100,
		}, makeVec(0, 1)); err != nil {
			t.Fatalf("写规则索引失败: %v", err)
		}

		hits, err := s.SearchRules(r.PersonaID, makeVec(0, 1), 5)
		if err != nil {
			t.Fatalf("检索规则索引失败: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("应当命中 1 条，实际 %d 条：%+v", len(hits), hits)
		}
		if hits[0].Rule.Content != "语气：别太正经" {
			t.Errorf("命中的内容不对：%q", hits[0].Rule.Content)
		}
		// 同方向单分量 → 余弦 1。分数低于这个说明口径与 PG 的 <=> 不一致
		if hits[0].Score < 0.99 {
			t.Errorf("同向向量应当几乎完全相似，实际 %.3f", hits[0].Score)
		}

		// 规则索引按人格**严格隔离**（不像记忆可以公共）
		other, err := s.SearchRules(testPersonaB, makeVec(0, 1), 5)
		if err != nil {
			t.Fatalf("检索别的人格失败: %v", err)
		}
		if len(other) != 0 {
			t.Errorf("别的人格不该检索到这条索引：%+v", other)
		}

		// 版本表：调用方靠它判断"这条索引是不是落后于规则了"
		versions, err := s.RuleIndexVersions(r.PersonaID)
		if err != nil {
			t.Fatalf("读索引版本失败: %v", err)
		}
		if v, ok := versions[r.RuleID]; !ok || v != 100 {
			t.Errorf("版本表应当是 %s → 100，实际 %+v", r.RuleID, versions)
		}
	})

	t.Run("规则索引：重复写是覆盖，内容与版本都换新", func(t *testing.T) {
		if err := s.IndexRule(memory.RuleIndex{
			RuleID: r.RuleID, PersonaID: r.PersonaID,
			Content: "语气：还是正经一点", SourceUpdatedAt: 200,
		}, makeVec(1, 1)); err != nil {
			t.Fatalf("覆盖规则索引失败: %v", err)
		}

		// 主键是 rule_id，所以同一条规则只会有一条索引
		hits, err := s.SearchRules(r.PersonaID, makeVec(1, 1), 5)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("同一条规则应当只有一条索引，实际 %d 条", len(hits))
		}
		if hits[0].Rule.Content != "语气：还是正经一点" || hits[0].Rule.SourceUpdatedAt != 200 {
			t.Errorf("覆盖后应当是新的内容与版本：%+v", hits[0].Rule)
		}

		versions, err := s.RuleIndexVersions(r.PersonaID)
		if err != nil {
			t.Fatalf("读索引版本失败: %v", err)
		}
		if versions[r.RuleID] != 200 {
			t.Errorf("版本应当跟到 200，实际 %d", versions[r.RuleID])
		}
	})

	t.Run("规则索引：删掉之后检索不到，重复删不报错", func(t *testing.T) {
		if err := s.DropRuleIndex(r.RuleID); err != nil {
			t.Fatalf("删除规则索引失败: %v", err)
		}

		hits, err := s.SearchRules(r.PersonaID, makeVec(0, 1), 5)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		if len(hits) != 0 {
			t.Errorf("删掉之后不该还能检索到：%+v", hits)
		}
		versions, err := s.RuleIndexVersions(r.PersonaID)
		if err != nil {
			t.Fatalf("读索引版本失败: %v", err)
		}
		if _, ok := versions[r.RuleID]; ok {
			t.Errorf("删掉之后版本表里不该还有它：%+v", versions)
		}

		// 用户在界面上的动作与回复那一轮是并发的，重复删很常见
		if err := s.DropRuleIndex(r.RuleID); err != nil {
			t.Errorf("删一条不存在的索引不该报错: %v", err)
		}
	})
}

// 内存实现的片索引是 map，PG 那边靠 chunk_id 主键——两者都要满足"一片只有一条"。
// PG 那侧因为 chunk_index 有外键挂在 session_chunks 上（需要一段真实历史），
// 放在 history/store 的测试里验（TestPgIndexChunkUpsert）。
func TestMemoryStoreIndexChunkUpsert(t *testing.T) {
	s := NewMemoryStore()
	ci := memory.ChunkIndex{
		ChunkID:   "00000000-0000-0000-0000-0000000000c1",
		SessionID: "00000000-0000-0000-0000-0000000000s1",
		PersonaID: testPersonaA,
		Summary:   "主题：第一版",
	}
	if err := s.IndexChunk(ci, makeVec(0, 1)); err != nil {
		t.Fatalf("写片索引失败: %v", err)
	}
	firstAt := s.chunkIndex[ci.ChunkID].ci.CreatedAt

	// 结算失败重放时会重复写同一片，所以这里必须是覆盖而不是新增
	ci.Summary = "主题：第二版"
	if err := s.IndexChunk(ci, makeVec(1, 1)); err != nil {
		t.Fatalf("覆盖片索引失败: %v", err)
	}

	if len(s.chunkIndex) != 1 {
		t.Fatalf("同一片应当只有一条索引，实际 %d 条", len(s.chunkIndex))
	}
	got := s.chunkIndex[ci.ChunkID]
	if got.ci.Summary != "主题：第二版" {
		t.Errorf("重复写应当覆盖摘要，实际 %q", got.ci.Summary)
	}
	// created_at 保留第一次的值：它表示"这片是什么时候被索引的"，
	// 而重放不该把它刷新成现在（与 PG 的 ON CONFLICT DO UPDATE 对齐）
	if got.ci.CreatedAt != firstAt {
		t.Errorf("重复写不该刷新创建时间：%d → %d", firstAt, got.ci.CreatedAt)
	}
	if len(got.vec) != embedDim {
		t.Errorf("向量应当被一起换掉，实际维度 %d", len(got.vec))
	}
}

func TestPgStoreContract(t *testing.T) {
	st, pool := openTestPgStore(t)
	for _, id := range []string{testPersonaA, testPersonaB} {
		ensureTestPersona(t, pool, id)
	}
	// 按内容前缀清掉本次写入的记忆。这一步不能省：测试里写了公共记忆，
	// 它们没有 persona 可以级联，不清就会留在库里
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM memories WHERE content LIKE $1`, testContentPrefix+"%")
	})
	// chunk_index 有外键挂在 session_chunks 上，所以片索引那几条断言必须先造出真实的片
	chunk := createTestChunk(t, pool, testPersonaA)
	runStoreContract(t, st, chunk)
	// rule_index 同理工挂在 persona_rules 上
	runRuleIndexContract(t, st, createTestRule(t, pool, testPersonaA))
}

// createTestRule 造一行最小的人格规则：rule_index 的外键要求它真实存在。
// 跑完删掉规则——外键级联会把它的索引一起带走。
func createTestRule(t *testing.T, pool *pgxpool.Pool, personaID string) testRule {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	r := testRule{RuleID: uuid.NewString(), PersonaID: personaID}
	if _, err := pool.Exec(ctx, `
		INSERT INTO persona_rules (id, persona_id, slot, value, source, evidence, tier, kind, priority, enabled, created_at, updated_at)
		VALUES ($1, $2, 'tone', '别太正经', 'manual', '', 'archived', 'volatile', 0, true, $3, $3)`,
		r.RuleID, personaID, now); err != nil {
		t.Fatalf("准备测试规则失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM persona_rules WHERE id = $1`, r.RuleID)
	})
	return r
}

// createTestChunk 造"会话 → 片"这条最小链路，返回它们的 id。
//
// 只给 PG 实现用：内存实现没有外键，契约测试那边直接给 uuid。
// 跑完删掉会话——外键级联会把片与片索引一起带走，测试不在库里留垃圾。
func createTestChunk(t *testing.T, pool *pgxpool.Pool, personaID string) testChunk {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	c := testChunk{ChunkID: uuid.NewString(), SessionID: uuid.NewString()}
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (id, persona_id, title, summary, started_at, ended_at, created_at, updated_at)
		VALUES ($1, $2, '', '', $3, NULL, $3, $3)`,
		c.SessionID, personaID, now); err != nil {
		t.Fatalf("准备测试会话失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_chunks (id, session_id, persona_id, seq, started_at, ended_at, summary, created_at, updated_at)
		VALUES ($1, $2, $3, 1, $4, NULL, '', $4, $4)`,
		c.ChunkID, c.SessionID, personaID, now); err != nil {
		t.Fatalf("准备测试片失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = $1`, c.SessionID)
	})
	return c
}

// openTestPgStore 连库并返回 PG 实现 + 连接池。
// 与 history/store 的测试同构（两个包无法共享未导出的测试助手，这点重复是为了简单）。
func openTestPgStore(t *testing.T) (*PgStore, *pgxpool.Pool) {
	t.Helper()
	config.LoadDotEnvUpward()
	dsn := db.DSNFromEnv()
	if dsn == "" {
		t.Skip("未配置 COMPANION_PG_DSN，跳过 PG 集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewPgStore(pool), pool
}

// ensureTestPersona 建一行最小的人格记录：memories 的外键要求它真实存在。
// 结束时会删掉它，级联带走它的私有记忆。
func ensureTestPersona(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if _, err := pool.Exec(ctx, `
		INSERT INTO personas (id, name, seed_text, origin, avatar_path, created_at, updated_at)
		VALUES ($1, '记忆集成测试用', '', 'user', '', $2, $2)
		ON CONFLICT (id) DO NOTHING`, id, now); err != nil {
		t.Fatalf("准备测试人格失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM memories WHERE persona_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM personas WHERE id = $1`, id)
	})
}
