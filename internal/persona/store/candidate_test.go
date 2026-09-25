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
}

func TestMemoryStoreCandidates(t *testing.T) {
	runCandidateContract(t, newTestStore())
}

func TestPgStoreCandidates(t *testing.T) {
	runCandidateContract(t, openTestPgStore(t))
}
