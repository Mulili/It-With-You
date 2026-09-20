package main

import (
	"testing"

	"agent-for-you-love/internal/history"
	historystore "agent-for-you-love/internal/history/store"
	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/persona"
	"agent-for-you-love/internal/persona/builtin"
	"agent-for-you-love/internal/persona/store"
)

// 这一个内置人格够用：验证"删除自建人格后历史怎么清"不需要更多数据。
func testBuiltins() []builtin.Entry {
	return []builtin.Entry{{
		Persona: persona.Persona{
			ID: "builtin:a", Name: "甲", SeedText: "测试用种子",
			Origin: persona.OriginBuiltin, IsBuiltin: true,
		},
		Rules: []persona.PersonaRule{{
			ID: "builtin:a#0", PersonaID: "builtin:a", Slot: "address_user", Value: "你",
			Source: persona.SourceManual, Tier: persona.TierCore,
		}},
	}}
}

// 这些测试不需要 Wails 运行时：事件发送有 a.ctx == nil 的保护，只会走日志。
//
// 历史用内存实现：这一组关心的是"人格与历史怎么联动"，而不是 PG 本身
// （PG 那条路径由 internal/history/store 的集成测试覆盖）。
func newPersonaApp(t *testing.T) (*App, persona.Store, history.Store) {
	t.Helper()
	st := store.NewMemoryStore(testBuiltins(), true)
	hist := historystore.NewMemoryStore()
	return NewApp(nil, st, hist), st, hist
}

// 删除人格必须同时清掉它的对话历史：那些消息的 personaID 已失效，
// 留着既不显示、又永远占空间——就是孤儿数据。
func TestDeletePersonaClearsItsHistory(t *testing.T) {
	app, st, hist := newPersonaApp(t)

	id, err := st.CreatePersona("临时人格", "")
	if err != nil {
		t.Fatalf("新建人格失败: %v", err)
	}

	// 给两个人格各写一条历史（各用各自的会话）
	seed := func(personaID, text string) history.Session {
		t.Helper()
		sess, err := hist.EnsureSession(personaID)
		if err != nil {
			t.Fatalf("建会话失败: %v", err)
		}
		if _, err := hist.AppendMessage(history.Message{
			SessionID: sess.ID, PersonaID: personaID,
			Role: llm.RoleUser, Content: text, Status: history.StatusOK,
		}); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
		return sess
	}
	seed("builtin:a", "甲的对话")
	sess := seed(id, "临时的对话")

	if err := app.DeletePersona(id); err != nil {
		t.Fatalf("删除人格失败: %v", err)
	}

	gone, err := hist.RecentMessages(id, 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("删除后仍有它的历史：%+v", gone)
	}

	kept, err := hist.RecentMessages("builtin:a", 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(kept) != 1 || kept[0].PersonaID != "builtin:a" {
		t.Fatalf("别的人格的历史被误伤了：%+v", kept)
	}

	// 删除时被掐掉的那一轮可能在删除之后才收尾，这条"迟到的半截回复"不能再进历史
	app.appendAssistant(sess.ID, id, "迟到的半截回复", history.StatusCanceled)
	after, err := hist.RecentMessages(id, 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("已删除人格的回复不该再进历史：%+v", after)
	}
}

// 内置人格只读：存储层就该拦住，别指望 UI 一定先判一次。
func TestDeleteBuiltinPersonaRefused(t *testing.T) {
	app, _, _ := newPersonaApp(t)
	if err := app.DeletePersona("builtin:a"); err == nil {
		t.Fatal("内置人格应当不可删除")
	}
}

// 删除当前人格后，存储层会回退到第一个内置人格；回执里要能说出切到了谁。
func TestDeleteActivePersonaFallsBack(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("会被删掉的", "builtin:a")
	if err != nil {
		t.Fatalf("复制人格失败: %v", err)
	}
	if err := st.SetActivePersona(id); err != nil {
		t.Fatalf("切换失败: %v", err)
	}

	if err := app.DeletePersona(id); err != nil {
		t.Fatalf("删除当前人格失败: %v", err)
	}
	if got := app.activePersonaID(); got != "builtin:a" {
		t.Fatalf("删除当前人格后应回退到内置人格，实际 %q", got)
	}
}

// 设置页要能改"非当前人格"，所以 SaveSeedText 必须带 personaID 而不是只看当前人格。
func TestSaveSeedTextTargetsGivenPersona(t *testing.T) {
	app, st, _ := newPersonaApp(t)

	id, err := st.CreatePersona("另一个", "builtin:a")
	if err != nil {
		t.Fatalf("复制人格失败: %v", err)
	}
	if err := app.SaveSeedText(id, "只改它"); err != nil {
		t.Fatalf("保存主体文本失败: %v", err)
	}

	snap := st.Snapshot()
	for _, p := range snap.Personas {
		if p.ID == id && p.SeedText != "只改它" {
			t.Fatalf("目标人格的主体文本没被改成「只改它」，实际 %q", p.SeedText)
		}
		if p.ID == "builtin:a" && p.SeedText != "测试用种子" {
			t.Fatalf("内置人格被连带改了：%q", p.SeedText)
		}
	}
	if err := app.SaveSeedText("builtin:a", "试着改内置"); err == nil {
		t.Fatal("内置人格应当只读")
	}
}
