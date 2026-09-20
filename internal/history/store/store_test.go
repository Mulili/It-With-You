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

	t.Run("消息按时间正序、且按会话隔离", func(t *testing.T) {
		sess, err := s.EnsureSession(testPersonaA)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		for _, text := range []string{"第一句", "第二句", "第三句"} {
			if _, err := s.AppendMessage(history.Message{
				SessionID: sess.ID, PersonaID: testPersonaA,
				Role: llm.RoleUser, Content: text, Status: history.StatusOK,
			}); err != nil {
				t.Fatalf("写消息失败: %v", err)
			}
		}

		msgs, err := s.MessagesOf(sess.ID)
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

		// 会话隔离：拿别的人格去读这段会话（它自己另有一段）
		other, err := s.EnsureSession(testPersonaB)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		others, err := s.MessagesOf(other.ID)
		if err != nil {
			t.Fatalf("读消息失败: %v", err)
		}
		if len(others) != 0 {
			t.Errorf("另一段会话不该读到别人的消息，实际 %d 条", len(others))
		}
	})

	t.Run("RecentMessages 取最近的 limit 条并保持正序", func(t *testing.T) {
		sess, err := s.EnsureSession(testPersonaB)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		for _, text := range []string{"一", "二", "三", "四", "五"} {
			if _, err := s.AppendMessage(history.Message{
				SessionID: sess.ID, PersonaID: testPersonaB,
				Role: llm.RoleUser, Content: text, Status: history.StatusOK,
			}); err != nil {
				t.Fatalf("写消息失败: %v", err)
			}
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

	t.Run("读不存在的会话返回空而不是错误", func(t *testing.T) {
		msgs, err := s.MessagesOf("00000000-0000-0000-0000-00000000dead")
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("应当为空，实际 %d 条", len(msgs))
		}
	})

	t.Run("DeletePersona 清掉它的历史且不误伤别人", func(t *testing.T) {
		sess, err := s.EnsureSession(testPersonaC)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		if _, err := s.AppendMessage(history.Message{
			SessionID: sess.ID, PersonaID: testPersonaC,
			Role: llm.RoleUser, Content: "要一起被删掉的", Status: history.StatusOK,
		}); err != nil {
			t.Fatalf("写消息失败: %v", err)
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
