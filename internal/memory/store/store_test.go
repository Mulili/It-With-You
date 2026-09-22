package store

import (
	"context"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/memory"

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

// 同一套断言跑两种实现：内存与 PG 的去重语义必须完全一致。
//
// 尤其"可见范围"那条——私有记忆只看得到"公共 + 自己"，两个实现若在这上面分叉，
// 用户在两种存储下会遇到完全不同的记忆行为，而且这种分叉不会报错，只会"记的东西不一样"。
func runStoreContract(t *testing.T, s memory.Store) {
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
	runStoreContract(t, NewMemoryStore())
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
	runStoreContract(t, st)
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
