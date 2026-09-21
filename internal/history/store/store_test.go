package store

import (
	"context"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/llm"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 契约测试用的固定人格 ID。
//
// PG 那边 messages.persona_id 有外键约束，所以集成测试必须先让这些人格真实存在。
// 用固定值而不是新建 uuid：跑完能干净删掉，失败时也不会在库里留一堆垃圾。
const (
	testPersonaA = "00000000-0000-0000-0000-00000000a001"
	testPersonaB = "00000000-0000-0000-0000-00000000b001"
	testPersonaC = "00000000-0000-0000-0000-00000000c001"
)

// 同一套断言跑两种实现：内存与 PG 的行为必须一致。
//
// 这不是为了覆盖率——两个实现一旦分叉，"开发时用内存、用户侧用 PG"就成了两个不同的系统，
// 而分叉点往往正是难查的地方：顺序、过滤、边界条数。
func runStoreContract(t *testing.T, s history.Store) {
	t.Helper()

	// 片相关的子测试都要"建会话 → 取当前片 → 往里写"，抽出来免得每处重复
	newChunk := func(t *testing.T, personaID string) (history.Session, history.Chunk) {
		t.Helper()
		sess, err := s.EnsureSession(personaID)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		c, err := s.EnsureChunk(sess.ID, history.ChunkMaxRunes)
		if err != nil {
			t.Fatalf("取当前片失败: %v", err)
		}
		return sess, c
	}
	// 显式把每条消息的时间戳错开。
	//
	// 为什么需要：created_at 是毫秒精度，测试里连着写几条会落在**同一毫秒**，
	// 而 PG 对"排序键相同"的行的相对顺序是不保证的（内存实现则恰好按插入顺序）——
	// 于是两个实现在"取最近 N 条"上会出现差异。这不是实现的 bug，是测试数据本身有歧义。
	// 真实场景撞不上：用户敲字与模型生成之间隔着好几秒。
	var timeShift int64
	appendTo := func(t *testing.T, c history.Chunk, text string) {
		t.Helper()
		timeShift++
		if _, err := s.AppendMessage(history.Message{
			SessionID: c.SessionID, ChunkID: c.ID, PersonaID: c.PersonaID,
			Role: llm.RoleUser, Content: text, Status: history.StatusOK,
			CreatedAt: history.NowMillis() + timeShift,
		}); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
	}

	t.Run("EnsureSession 复用同一段未收尾的会话", func(t *testing.T) {
		first, err := s.EnsureSession(testPersonaA)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		again, err := s.EnsureSession(testPersonaA)
		if err != nil {
			t.Fatalf("再取会话失败: %v", err)
		}
		// 这条是"重启后接着上一段聊"的全部依据：没有它，每次发消息都会开一段新会话，
		// 上下文永远是空的，而症状只是"它好像什么都不记得了"
		if again.ID != first.ID {
			t.Errorf("未收尾的会话应当被复用：第一次 %s，第二次 %s", first.ID, again.ID)
		}
	})

	t.Run("不同人格各自独立的会话", func(t *testing.T) {
		a, err := s.EnsureSession(testPersonaA)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		b, err := s.EnsureSession(testPersonaB)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		if a.ID == b.ID {
			t.Error("不同人格不该共用会话：历史按人格隔离是「独立个体」的前提")
		}
	})

	t.Run("消息按时间正序、且按片隔离", func(t *testing.T) {
		_, c := newChunk(t, testPersonaA)
		for _, text := range []string{"第一句", "第二句", "第三句"} {
			appendTo(t, c, text)
		}

		msgs, err := s.ChunkMessages(c.ID)
		if err != nil {
			t.Fatalf("读消息失败: %v", err)
		}
		if len(msgs) != 3 {
			t.Fatalf("应当读到 3 条，实际 %d 条", len(msgs))
		}
		// 正序是拼上下文的前提：反了模型会看到"先答后问"
		if msgs[0].Content != "第一句" || msgs[2].Content != "第三句" {
			t.Errorf("消息顺序不对：%s / %s / %s", msgs[0].Content, msgs[1].Content, msgs[2].Content)
		}
		// 每条都要带上片 ID：没有它，消息归不到任何一片里，拼上下文时就永远读不到
		if msgs[0].ChunkID != c.ID {
			t.Errorf("消息应当带上片 ID %s，实际 %q", c.ID, msgs[0].ChunkID)
		}

		// 片隔离：另一个人格的片里不该有这些消息
		_, other := newChunk(t, testPersonaB)
		others, err := s.ChunkMessages(other.ID)
		if err != nil {
			t.Fatalf("读消息失败: %v", err)
		}
		if len(others) != 0 {
			t.Errorf("另一片不该读到别人的消息，实际 %d 条", len(others))
		}
	})

	t.Run("EnsureChunk 未到阈值时复用同一片", func(t *testing.T) {
		sess, first := newChunk(t, testPersonaA)
		appendTo(t, first, "随便说点什么")

		again, err := s.EnsureChunk(sess.ID, history.ChunkMaxRunes)
		if err != nil {
			t.Fatalf("取当前片失败: %v", err)
		}
		if again.ID != first.ID {
			t.Errorf("没到阈值不该换片：%s → %s", first.ID, again.ID)
		}
	})

	t.Run("EnsureChunk 到阈值时换新片、并把旧片收尾", func(t *testing.T) {
		sess, first := newChunk(t, testPersonaC)
		appendTo(t, first, "这句话的存在就是为了让片超过阈值")

		// 阈值传 1：任何内容都超标，于是必然触发换片
		second, err := s.EnsureChunk(sess.ID, 1)
		if err != nil {
			t.Fatalf("取当前片失败: %v", err)
		}
		if second.ID == first.ID {
			t.Fatal("超过阈值后应当另起一片")
		}
		if second.Seq != 2 {
			t.Errorf("新片序号应当是 2，实际 %d", second.Seq)
		}

		chunks, err := s.ListChunks(sess.ID)
		if err != nil {
			t.Fatalf("读片列表失败: %v", err)
		}
		if len(chunks) != 2 {
			t.Fatalf("该会话应当有 2 片，实际 %d 片", len(chunks))
		}
		// 切走的片必须被收尾：否则"当前片"会有两个，而收尾懒结算正是靠这个状态找候选
		if chunks[0].EndedAt == 0 {
			t.Error("被切走的旧片应当有 ended_at")
		}
		if chunks[1].EndedAt != 0 {
			t.Error("新片应当是未收尾状态")
		}
	})

	t.Run("RecentMessages 取最近的 limit 条并保持正序", func(t *testing.T) {
		_, c := newChunk(t, testPersonaB)
		for _, text := range []string{"一", "二", "三", "四", "五"} {
			appendTo(t, c, text)
		}

		got, err := s.RecentMessages(testPersonaB, 2)
		if err != nil {
			t.Fatalf("读历史失败: %v", err)
		}
		// 这条挡的是最容易写错、且错了不报错的地方：SQL 里写成 ASC LIMIT n
		// 会取到**开头**的两条（"一、二"），而历史界面要看的是最近的
		if len(got) != 2 {
			t.Fatalf("应当返回 2 条，实际 %d 条", len(got))
		}
		if got[0].Content != "四" || got[1].Content != "五" {
			t.Errorf("应当取最近的 2 条并按正序（四、五），实际（%s、%s）", got[0].Content, got[1].Content)
		}
	})

	t.Run("读不存在的片返回空而不是错误", func(t *testing.T) {
		msgs, err := s.ChunkMessages("00000000-0000-0000-0000-00000000dead")
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("应当为空，实际 %d 条", len(msgs))
		}
	})

	t.Run("DeletePersona 清掉它的历史且不误伤别人", func(t *testing.T) {
		sess, c := newChunk(t, testPersonaC)
		appendTo(t, c, "要一起被删掉的")

		// 先确认片也在：删人格要连片一起清掉，否则会留下没有主人的片
		if chunks, err := s.ListChunks(sess.ID); err != nil {
			t.Fatalf("读片列表失败: %v", err)
		} else if len(chunks) == 0 {
			t.Fatal("前置条件不满足：该会话应当有片")
		}

		if err := s.DeletePersona(testPersonaC); err != nil {
			t.Fatalf("删历史失败: %v", err)
		}
		gone, err := s.RecentMessages(testPersonaC, 10)
		if err != nil {
			t.Fatalf("读历史失败: %v", err)
		}
		if len(gone) != 0 {
			t.Errorf("删除后应当读不到消息，实际 %d 条", len(gone))
		}

		// 别人格不受影响（A 前面写过 3 条）
		kept, err := s.RecentMessages(testPersonaA, 10)
		if err != nil {
			t.Fatalf("读历史失败: %v", err)
		}
		if len(kept) == 0 {
			t.Error("别人格的历史被误删了")
		}
	})

	// 这条放在最后：它会把 A 的当前会话结束掉，而前面几个子测试依赖"会话可复用"
	t.Run("EndSession 后不再复用这段会话，重复调用也无副作用", func(t *testing.T) {
		sess, _ := newChunk(t, testPersonaA)

		if err := s.EndSession(sess.ID); err != nil {
			t.Fatalf("结束会话失败: %v", err)
		}
		// 幂等：判定是异步且启发式的，同一个话题可能被连续两轮都判成"结束"
		if err := s.EndSession(sess.ID); err != nil {
			t.Fatalf("重复结束应当无害: %v", err)
		}

		// 结束之后必须给一段**新**会话——这正是"话题切换"落地的方式
		next, err := s.EnsureSession(testPersonaA)
		if err != nil {
			t.Fatalf("取会话失败: %v", err)
		}
		if next.ID == sess.ID {
			t.Error("已结束的会话不该被复用")
		}
	})
}

func TestMemoryStoreContract(t *testing.T) {
	runStoreContract(t, NewMemoryStore())
}

func TestPgStoreContract(t *testing.T) {
	st, pool := openTestPgStore(t)
	for _, id := range []string{testPersonaA, testPersonaB, testPersonaC} {
		ensureTestPersona(t, pool, id)
	}
	runStoreContract(t, st)
}

// openTestPgStore 连库并返回 PG 实现 + 连接池（池给测试准备前置数据用）。
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

// ensureTestPersona 建一行最小的人格记录：messages 的外键要求它真实存在。
//
// 跑完删掉它——外键级联会把属于它的会话与消息一起带走，所以测试不在库里留垃圾。
func ensureTestPersona(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if _, err := pool.Exec(ctx, `
		INSERT INTO personas (id, name, seed_text, origin, avatar_path, created_at, updated_at)
		VALUES ($1, '集成测试用', '', 'user', '', $2, $2)
		ON CONFLICT (id) DO NOTHING`, id, now); err != nil {
		t.Fatalf("准备测试人格失败: %v", err)
	}
	t.Cleanup(func() {
		// 先删历史再删人格：虽然外键会级联，但显式删一次能让失败信息更清楚
		_, _ = pool.Exec(context.Background(), `DELETE FROM sessions WHERE persona_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM personas WHERE id = $1`, id)
	})
}
