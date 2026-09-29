package store

import (
	"testing"
	"time"

	"agent-for-you-love/internal/persona"
)

// 候选区（隐式演化的待办）的契约测试：同一套断言跑内存与 PG 两种实现。
//
// 为什么要两边都跑：候选区的判重键（persona + slot + value）在两个实现里是**分别**实现的
// （内存是自己遍历，PG 靠唯一索引 + ON CONFLICT），这正是最容易分叉的地方——
// 一旦分叉，开发期用内存、用户侧用 PG 就成了两个系统，而表现只是"它有时候会重复学同一件事"。
func runCandidateContract(t *testing.T, s persona.Store) {
	t.Helper()

	// 候选按人格隔离，所以每个子测试都自建一个人格：共用一个会互相干扰，
	// 而且"删人格连带清候选"那条必须能真的删掉一个人格
	newPersona := func(t *testing.T) string {
		t.Helper()
		name := "候选测试 " + time.Now().Format("0102-150405.000000")
		id, err := s.CreatePersona(name, "")
		if err != nil {
			t.Fatalf("新建人格失败: %v", err)
		}
		t.Cleanup(func() { _ = s.DeletePersona(id) }) // 不清会留垃圾数据
		return id
	}

	t.Run("写入后能列出，且新的在前", func(t *testing.T) {
		pid := newPersona(t)
		base := time.Now().UnixMilli()
		// 显式错开 created_at：同一毫秒内的相对顺序两种实现都不保证，
		// 那样"新的在前"这条断言就成了掷骰子（与 history 的 created_at 是同一个坑）
		if err := s.AddCandidates([]persona.Candidate{
			{PersonaID: pid, Slot: "catchphrase", Value: "好耶", Evidence: "好耶！", CreatedAt: base - 1000},
			{PersonaID: pid, Slot: "tone", Value: "更活泼一点", Evidence: "你能不能活泼点", CreatedAt: base},
		}); err != nil {
			t.Fatalf("写候选失败: %v", err)
		}

		got, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("应当有 2 条候选，实际 %d 条：%+v", len(got), got)
		}
		if got[0].Value != "更活泼一点" {
			t.Errorf("新的应当排在前，实际第一条是 %q", got[0].Value)
		}
		for _, c := range got {
			if c.ID == "" {
				t.Error("候选没有 ID：调用方（界面）要靠它删除")
			}
			if c.PersonaID != pid {
				t.Errorf("候选归属错了：%s，期望 %s", c.PersonaID, pid)
			}
		}
	})

	t.Run("同一条候选重复写入是幂等的", func(t *testing.T) {
		pid := newPersona(t)
		c := persona.Candidate{PersonaID: pid, Slot: "catchphrase", Value: "好耶"}

		// 结算是每段会话都跑一次，同一件事必然被反复抽到；结算失败重放还会整批再写一遍
		for i := 0; i < 2; i++ {
			if err := s.AddCandidates([]persona.Candidate{c}); err != nil {
				t.Fatalf("第 %d 次写候选失败: %v", i+1, err)
			}
		}

		got, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("同一条候选应当只有 1 条，实际 %d 条：%+v", len(got), got)
		}
	})

	t.Run("DeleteCandidate 只删指定那条、重复删不报错", func(t *testing.T) {
		pid := newPersona(t)
		if err := s.AddCandidates([]persona.Candidate{
			{PersonaID: pid, Slot: "catchphrase", Value: "好耶"},
			{PersonaID: pid, Slot: "tone", Value: "更活泼一点"},
		}); err != nil {
			t.Fatalf("写候选失败: %v", err)
		}

		all, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("应当有 2 条候选，实际 %d 条", len(all))
		}

		if err := s.DeleteCandidate(all[0].ID); err != nil {
			t.Fatalf("删候选失败: %v", err)
		}
		left, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if len(left) != 1 {
			t.Fatalf("应当只剩 1 条，实际 %d 条", len(left))
		}
		if left[0].ID == all[0].ID {
			t.Error("删错了那条")
		}

		// 采纳与丢弃都会删它，界面重复点一下不该变成错误弹窗
		if err := s.DeleteCandidate(all[0].ID); err != nil {
			t.Errorf("删已经不存在的不该报错: %v", err)
		}
	})

	t.Run("删掉人格时它的候选一起消失", func(t *testing.T) {
		pid := newPersona(t)
		if err := s.AddCandidates([]persona.Candidate{
			{PersonaID: pid, Slot: "catchphrase", Value: "好耶"},
		}); err != nil {
			t.Fatalf("写候选失败: %v", err)
		}

		if err := s.DeletePersona(pid); err != nil {
			t.Fatalf("删人格失败: %v", err)
		}

		got, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("人格都没了，候选不该还在：%+v", got)
		}
	})

	t.Run("PruneStaleCandidates：缓冲队列没满就一条都不删，且不碰规则", func(t *testing.T) {
		pid := newPersona(t)
		// 用"1970 年"这种极端时间戳，before 也取极小值：契约测试连的是**真实库**，
		// 只要两头的量级都远离当下，就不可能误伤库里任何真实数据
		if err := s.AddCandidates([]persona.Candidate{
			{PersonaID: pid, Slot: "address_user", Value: "老板", CreatedAt: 1},
			{PersonaID: pid, Slot: "tone", Value: "别太正经", CreatedAt: time.Now().UnixMilli()},
		}); err != nil {
			t.Fatalf("写候选失败: %v", err)
		}
		if _, err := s.SaveRule(persona.PersonaRule{
			PersonaID: pid, Slot: "catchphrase", Value: "好耶",
			Source: persona.SourceManual, Tier: persona.TierRecent,
			Kind: persona.KindVolatile, Enabled: true,
		}); err != nil {
			t.Fatalf("写规则失败: %v", err)
		}

		// keep=50：过期的只有 1 条，远没堆满缓冲 → 什么都不删。
		// 这一条是"缓冲区"的核心：过期只是**进队资格**，不是判决
		gone, err := s.PruneStaleCandidates(1000, 50)
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if len(gone) != 0 {
			t.Errorf("缓冲队列没满时不该删任何东西，实际删了 %d 条：%+v", len(gone), gone)
		}

		left, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if len(left) != 2 {
			t.Errorf("两条都该还在（一条过期但队没满、一条没过期），实际 %d 条", len(left))
		}

		// 规则一条都不能少：懒归档**只清候选**。规则那侧没有无界增长的来源
		// （单值槽位写入即覆盖、多值槽位靠用户明说或亲手提升），而按时间淘汰它们
		// 会造成"用户明说了'叫我主人'，30 天后她忘了"这种伤害。见 persona.Store 的注释
		rules, err := s.RulesOf(pid)
		if err != nil {
			t.Fatalf("读规则失败: %v", err)
		}
		if len(rules) != 1 {
			t.Errorf("清理候选不该动到规则，实际规则数 %d", len(rules))
		}
	})

	t.Run("PruneStaleCandidates：队列满了只删最久没被提起的那些", func(t *testing.T) {
		pid := newPersona(t)
		// 造 keep+2 条过期候选，最后提起时间依次递增 → 该删的是最早的那两条。
		// 同一个槽位放三条是刻意的：多值槽位（口头禅）本来就该攒好几条
		if err := s.AddCandidates([]persona.Candidate{
			{PersonaID: pid, Slot: "catchphrase", Value: "最早", CreatedAt: 100, LastUsedAt: 100},
			{PersonaID: pid, Slot: "catchphrase", Value: "第二早", CreatedAt: 200, LastUsedAt: 200},
			{PersonaID: pid, Slot: "catchphrase", Value: "较新", CreatedAt: 300, LastUsedAt: 300},
			{PersonaID: pid, Slot: "tone", Value: "最新", CreatedAt: 400, LastUsedAt: 400},
		}); err != nil {
			t.Fatalf("写候选失败: %v", err)
		}

		gone, err := s.PruneStaleCandidates(1000, 2)
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if len(gone) != 2 {
			t.Fatalf("留 2 条、共 4 条过期，应当删 2 条，实际 %d 条：%+v", len(gone), gone)
		}
		// 返回的必须是**真删掉的那些**：调用方要直接拿它写 `[archive]` 日志
		for _, c := range gone {
			if c.Value != "最早" && c.Value != "第二早" {
				t.Errorf("删错了：%q（该删最久没被提起的两条）", c.Value)
			}
		}

		left, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if len(left) != 2 {
			t.Fatalf("应当留下 2 条，实际 %d 条：%+v", len(left), left)
		}
		for _, c := range left {
			if c.Value != "较新" && c.Value != "最新" {
				t.Errorf("留下的是错的：%q（该留被提起得最晚的两条）", c.Value)
			}
		}

		// 再跑一次：队列已经只剩 2 条（正好等于 keep），不该再删
		gone, err = s.PruneStaleCandidates(1000, 2)
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if len(gone) != 0 {
			t.Errorf("第二次清理应当清 0 条，实际 %d 条", len(gone))
		}
	})

	t.Run("TouchCandidates 刷新最后提起时间，删过的 ID 不报错", func(t *testing.T) {
		pid := newPersona(t)
		if err := s.AddCandidates([]persona.Candidate{
			{PersonaID: pid, Slot: "address_user", Value: "老板", CreatedAt: 1, LastUsedAt: 1},
		}); err != nil {
			t.Fatalf("写候选失败: %v", err)
		}
		all, err := s.ListCandidates(pid, 10)
		if err != nil || len(all) != 1 {
			t.Fatalf("列候选失败或条数不对: %v %+v", err, all)
		}
		if all[0].LastUsedAt != 1 {
			t.Fatalf("准备数据不对：LastUsedAt 应当是 1，实际 %d", all[0].LastUsedAt)
		}

		// 写库时 LastUsedAt 缺省会取 CreatedAt（不是 0）——0 会让它在第一次清理时
		// 就被当成"1970 年就没再用过"，那是升级时最容易踩的坑
		if all[0].CreatedAt != 1 {
			t.Errorf("缺省时间戳不对：%+v", all[0])
		}

		if err := s.TouchCandidates([]string{all[0].ID}, 9999); err != nil {
			t.Fatalf("刷新失败: %v", err)
		}
		after, err := s.ListCandidates(pid, 10)
		if err != nil {
			t.Fatalf("列候选失败: %v", err)
		}
		if after[0].LastUsedAt != 9999 {
			t.Errorf("最后提起时间应当被刷成 9999，实际 %d", after[0].LastUsedAt)
		}
		if after[0].CreatedAt != 1 {
			t.Errorf("刷新不该动 CreatedAt，实际 %d", after[0].CreatedAt)
		}

		// 刷新过的这条不该再被当成过期（before=5000 时它还在窗口内）
		gone, err := s.PruneStaleCandidates(5000, 0)
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if len(gone) != 0 {
			t.Errorf("刚刷新过的不该被清掉，实际 %+v", gone)
		}

		// 找不到的 ID（刚被删或刚被提升）不该报错
		if err := s.TouchCandidates([]string{"00000000-0000-0000-0000-000000000000"}, 1); err != nil {
			t.Errorf("刷新不存在的候选不该报错: %v", err)
		}
	})
}

func TestMemoryStoreCandidates(t *testing.T) {
	runCandidateContract(t, newTestStore())
}

func TestPgStoreCandidates(t *testing.T) {
	runCandidateContract(t, openTestPgStore(t))
}
