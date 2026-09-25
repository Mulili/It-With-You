package store

import (
	"context"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
	"agent-for-you-love/internal/db"
	"agent-for-you-love/internal/history"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
	memorystore "agent-for-you-love/internal/memory/store"

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
	// D 只服务于"条数到阈值也换片"那条：它要断言"新片序号是 2"，
	// 所以必须用一个**前面没用过**的人格——复用的话会接上别人已经建好的会话与片，序号就不从 1 开始了
	testPersonaD = "00000000-0000-0000-0000-00000000d001"
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
		c, err := s.EnsureChunk(sess.ID, history.ChunkMaxRunes, history.ChunkMaxMessages)
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

	// 待结算列表按片 ID 判断"在不在里面"：同一时刻可能有好几片挂在里面
	//（前面的子测试会留下已收尾的片），断言具体条数会互相干扰
	pendingHas := func(t *testing.T, chunkID string) bool {
		t.Helper()
		list, err := s.PendingChunks(100)
		if err != nil {
			t.Fatalf("读待结算列表失败: %v", err)
		}
		for _, c := range list {
			if c.ID == chunkID {
				return true
			}
		}
		return false
	}
	// freshChunk 收掉当前的会话，再开一段干净的（片里没有历史消息）——
	// 需要精确计数的子测试用它，否则前面写进去的消息会让断言变得含糊
	freshChunk := func(t *testing.T, personaID string) (history.Session, history.Chunk) {
		t.Helper()
		cur, err := s.EnsureSession(personaID)
		if err != nil {
			t.Fatalf("取会话失败: %v", err)
		}
		if err := s.EndSession(cur.ID); err != nil {
			t.Fatalf("结束会话失败: %v", err)
		}
		return newChunk(t, personaID)
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

		msgs, err := s.ChunkMessages(c.ID, history.ContextMessagesLimit)
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
		others, err := s.ChunkMessages(other.ID, history.ContextMessagesLimit)
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

		again, err := s.EnsureChunk(sess.ID, history.ChunkMaxRunes, history.ChunkMaxMessages)
		if err != nil {
			t.Fatalf("取当前片失败: %v", err)
		}
		if again.ID != first.ID {
			t.Errorf("没到阈值不该换片：%s → %s", first.ID, again.ID)
		}
	})

	t.Run("EnsureChunk 条数到阈值也换片", func(t *testing.T) {
		// 这一条盯的是"落在缝里的短消息"：短消息字符数几乎不涨，
		// 若只按字符切，一屏"嗯""哈哈"能把片撑到上千条——而拼上下文有 500 条的兜底上限，
		// 被裁掉的那部分既不在上下文里、又因为片没收尾而进不了结算，等于静默丢失
		sess, first := newChunk(t, testPersonaD)
		for i := 0; i < 3; i++ {
			appendTo(t, first, "嗯")
		}

		// 字符上限给足（不设限），只让条数上限生效：3 条 ≥ 2 就该切
		second, err := s.EnsureChunk(sess.ID, 0, 2)
		if err != nil {
			t.Fatalf("取当前片失败: %v", err)
		}
		if second.ID == first.ID {
			t.Fatal("条数到上限后应当另起一片")
		}
		if second.Seq != 2 {
			t.Errorf("新片序号应当是 2，实际 %d", second.Seq)
		}
	})

	t.Run("EnsureChunk 到阈值时换新片、并把旧片收尾", func(t *testing.T) {
		sess, first := newChunk(t, testPersonaC)
		appendTo(t, first, "这句话的存在就是为了让片超过阈值")

		// 字符阈值传 1：任何内容都超标，于是必然触发换片
		second, err := s.EnsureChunk(sess.ID, 1, 0)
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

	t.Run("SessionMessages 只取该会话的消息、超出取尾部", func(t *testing.T) {
		// 复用 B 现有的会话：不断言总条数（前面几个子测试可能也往它里面写过），
		// 只断言"不串会话"与"尾部两条的顺序"这两个真正要守的不变式
		sess, c := newChunk(t, testPersonaB)
		appendTo(t, c, "会话消息甲")
		appendTo(t, c, "会话消息乙")

		all, err := s.SessionMessages(sess.ID, 0)
		if err != nil {
			t.Fatalf("读会话消息失败: %v", err)
		}
		for _, m := range all {
			if m.SessionID != sess.ID {
				t.Fatalf("混进了别的会话的消息：%s", m.SessionID)
			}
		}
		if len(all) < 2 {
			t.Fatalf("至少该有刚写的两条，实际 %d 条", len(all))
		}
		last := all[len(all)-2:]
		if last[0].Content != "会话消息甲" || last[1].Content != "会话消息乙" {
			t.Errorf("应当按正序收尾（甲、乙），实际（%s、%s）", last[0].Content, last[1].Content)
		}

		// limit 是"最多几条"且取尾部：给 1 应当只剩最后那条
		one, err := s.SessionMessages(sess.ID, 1)
		if err != nil {
			t.Fatalf("读会话消息失败: %v", err)
		}
		if len(one) != 1 || one[0].Content != "会话消息乙" {
			t.Errorf("limit=1 应当只给最后那条（乙），实际 %+v", one)
		}
	})

	t.Run("读不存在的片返回空而不是错误", func(t *testing.T) {
		msgs, err := s.ChunkMessages("00000000-0000-0000-0000-00000000dead", history.ContextMessagesLimit)
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

	// limit <= 0 是给收尾结算用的：它要整片原文。这不是"多取几条"的细节——
	// 摘要的保真度上限就是原文的完整度，短句闲聊很容易在 2 万字符里塞下 500 条以上消息，
	// 若沿用拼上下文那个上限，摘要会从中间开始、开头的内容永远进不了记忆
	t.Run("ChunkMessages 的 limit<=0 表示取回整片", func(t *testing.T) {
		_, c := freshChunk(t, testPersonaA)
		for _, text := range []string{"一", "二", "三"} {
			appendTo(t, c, text)
		}

		all, err := s.ChunkMessages(c.ID, 0)
		if err != nil {
			t.Fatalf("读消息失败: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("limit<=0 应当取回整片（3 条），实际 %d 条", len(all))
		}

		tail, err := s.ChunkMessages(c.ID, 2)
		if err != nil {
			t.Fatalf("读消息失败: %v", err)
		}
		if len(tail) != 2 || tail[0].Content != "二" || tail[1].Content != "三" {
			t.Errorf("给了 limit 应当取**尾部**若干条，实际 %d 条", len(tail))
		}
	})

	// 会话的最后一片必须跟着收尾：否则它永远停在"未收尾"，而懒结算扫的正是
	// "已收尾但没摘要"的片——那片就永远拿不到摘要、也永远不会被抽成事实
	t.Run("EndSession 会顺手收尾当前片", func(t *testing.T) {
		sess, c := freshChunk(t, testPersonaB)
		appendTo(t, c, "这句话要在会话收尾之后还能被结算到")

		// 还没收尾 → 不在待结算里
		if pendingHas(t, c.ID) {
			t.Error("会话还没结束，当前片不该出现在待结算里")
		}

		if err := s.EndSession(sess.ID); err != nil {
			t.Fatalf("结束会话失败: %v", err)
		}
		chunks, err := s.ListChunks(sess.ID)
		if err != nil {
			t.Fatalf("读片列表失败: %v", err)
		}
		for _, ch := range chunks {
			if ch.EndedAt == 0 {
				t.Error("会话结束后不该还有未收尾的片")
			}
		}
		if !pendingHas(t, c.ID) {
			t.Error("片收尾之后就该出现在待结算里")
		}

		// 摘要一写，它就该从待结算里消失——这就是"结算完成"的判据
		if err := s.SetChunkSummary(c.ID, "主题：随便聊聊"); err != nil {
			t.Fatalf("写片摘要失败: %v", err)
		}
		if pendingHas(t, c.ID) {
			t.Error("写过摘要的片不该继续留在待结算里：那会每轮都被重新结算一遍")
		}
	})

	// 空片结算不出任何东西。把它排除在待结算之外，是为了不被"每轮扫到、每轮无事可做"
	// 反复打扰——它也永远不会因为写了摘要而消失
	t.Run("没有消息的片不算待结算", func(t *testing.T) {
		sess, c := freshChunk(t, testPersonaC)
		if err := s.EndSession(sess.ID); err != nil {
			t.Fatalf("结束会话失败: %v", err)
		}
		if pendingHas(t, c.ID) {
			t.Error("空片不该出现在待结算里")
		}
	})

	// "一次会话一个题目"由存储层保证：后来的片即使也产出了标题，也改不掉已经写下的那个
	t.Run("会话标题只写第一次", func(t *testing.T) {
		sess, _ := freshChunk(t, testPersonaA)

		if err := s.SetSessionTitleIfEmpty(sess.ID, "第一次的标题"); err != nil {
			t.Fatalf("写会话标题失败: %v", err)
		}
		if err := s.SetSessionTitleIfEmpty(sess.ID, "后来的标题"); err != nil {
			t.Fatalf("写会话标题失败: %v", err)
		}

		list, err := s.ListSessions(testPersonaA, history.DefaultSessionLimit)
		if err != nil {
			t.Fatalf("读会话列表失败: %v", err)
		}
		for _, got := range list {
			if got.ID != sess.ID {
				continue
			}
			if got.Title != "第一次的标题" {
				t.Errorf("标题应当停在第一次写入的那个，实际 %q", got.Title)
			}
			return
		}
		t.Fatal("列表里找不到刚建的会话")
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
	for _, id := range []string{testPersonaA, testPersonaB, testPersonaC, testPersonaD} {
		ensureTestPersona(t, pool, id)
	}
	runStoreContract(t, st)
}

// 片索引写在记忆库里（chunk_index 与 memories 同属 schema_vector.sql），
// 但它挂在 session_chunks 上，而造一段真实历史要用本包的 Store——
// 所以这条测试放在这里（memory/store 的测试没有 history 的夹具）。
//
// 验的是那一句 ON CONFLICT：结算的写库顺序靠它才能"整体重放"，
// 若它写成普通 INSERT，重放时会撞主键，于是每次重试都在原地失败。
func TestPgIndexChunkUpsert(t *testing.T) {
	st, pool := openTestPgStore(t)
	ensureTestPersona(t, pool, testPersonaA)

	sess, err := st.EnsureSession(testPersonaA)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	chunk, err := st.EnsureChunk(sess.ID, history.ChunkMaxRunes, history.ChunkMaxMessages)
	if err != nil {
		t.Fatalf("取当前片失败: %v", err)
	}

	memStore := memorystore.NewPgStore(pool)
	ci := memory.ChunkIndex{
		ChunkID: chunk.ID, SessionID: sess.ID, PersonaID: testPersonaA,
		Summary: "主题：聊新买的键盘",
	}
	if err := memStore.IndexChunk(ci, makeVec(0)); err != nil {
		t.Fatalf("写片索引失败: %v", err)
	}

	ci.Summary = "主题：又聊了一次键盘"
	if err := memStore.IndexChunk(ci, makeVec(1)); err != nil {
		t.Fatalf("覆盖片索引失败: %v", err)
	}

	var count int
	var summary string
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(MAX(summary), '') FROM chunk_index WHERE chunk_id = $1`,
		chunk.ID).Scan(&count, &summary); err != nil {
		t.Fatalf("查片索引失败: %v", err)
	}
	if count != 1 {
		t.Errorf("同一片应当只有一条索引，实际 %d 条", count)
	}
	if summary != ci.Summary {
		t.Errorf("重复写应当覆盖摘要，实际 %q", summary)
	}
}

// 片索引的向量列维度必须与建表时一致（vector(1024)），否则 PG 直接拒绝写入。
const embedDim = 1024

// makeVec 造一个只有单个分量非零的向量：与 memory/store 的契约测试同一个套路，
// 不依赖嵌入服务。
func makeVec(index int) []float32 {
	v := make([]float32, embedDim)
	v[index] = 1
	return v
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
