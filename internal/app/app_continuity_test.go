package app

import (
	"strings"
	"testing"

	"agent-for-you-love/internal/history"
	historystore "agent-for-you-love/internal/history/store"
	"agent-for-you-love/internal/llm"
)

// 开场窗口与"摘要只补一次"。
//
// 顺序是有意义的：前两次摘要是空的（用原文兜底），第三次摘要就绪（补上并记住），
// 第四次即使还在窗口内也不再重复。
func TestContinuityState(t *testing.T) {
	a := &App{}

	if !a.shouldInjectContinuity("c1", 1, false) {
		t.Fatal("摘要没就绪时应当先用原文兜底")
	}
	if !a.shouldInjectContinuity("c1", 3, false) {
		t.Fatal("摘要还没好时应当继续兜底")
	}
	if !a.shouldInjectContinuity("c1", 5, true) {
		t.Fatal("摘要就绪后应当补上")
	}
	if a.shouldInjectContinuity("c1", 7, true) {
		t.Fatal("摘要只该注入一次")
	}

	// 换一片 → 状态重置
	if !a.shouldInjectContinuity("c2", 1, false) {
		t.Fatal("新片应当重新开始衔接")
	}

	// 超过窗口不再注入（哪怕摘要就绪）
	if a.shouldInjectContinuity("c3", continuityWindowMessages+1, true) {
		t.Fatal("超过窗口不该再注入")
	}
}

// 有摘要时用摘要，而且不能退化成原文。
func TestContinuityBlockUsesSummary(t *testing.T) {
	a := &App{}
	prev := history.Chunk{Summary: "主题：聊 TTS 声线\n- 要清亮微哑、语速偏快"}

	got := a.continuityBlock(prev)
	if !strings.Contains(got, "接着往下说") {
		t.Errorf("缺少衔接说明:\n%s", got)
	}
	if !strings.Contains(got, "清亮微哑") {
		t.Errorf("摘要没被带进来:\n%s", got)
	}
	if strings.Contains(got, "他：") || strings.Contains(got, "你：") {
		t.Errorf("有摘要时不该退化去取原文:\n%s", got)
	}
}

// 摘要还没结算出来时，退化成上一片的最后两句话（而不是什么都不注入）。
func TestContinuityBlockFallsBackToTailMessages(t *testing.T) {
	st := historystore.NewMemoryStore()
	a := &App{history: st}

	sess, err := st.EnsureSession("persona-1")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	c, err := st.EnsureChunk(sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("建片失败: %v", err)
	}
	for _, m := range []history.Message{
		{SessionID: sess.ID, ChunkID: c.ID, PersonaID: "persona-1", Role: llm.RoleUser, Content: "上一段最后一句"},
		{SessionID: sess.ID, ChunkID: c.ID, PersonaID: "persona-1", Role: llm.RoleAssistant, Content: "上一段的最后一句回复"},
	} {
		if _, err := st.AppendMessage(m); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
	}

	got := a.continuityBlock(c)
	if !strings.Contains(got, "他：上一段最后一句") {
		t.Errorf("退化时应当带上最后一句用户话:\n%s", got)
	}
	if !strings.Contains(got, "你：上一段的最后一句回复") {
		t.Errorf("退化时应当带上最后一句回复:\n%s", got)
	}
}

// 既没摘要、也没消息：返回空串，调用方据此不插入。
func TestContinuityBlockEmptyWhenNothingToSay(t *testing.T) {
	st := historystore.NewMemoryStore()
	a := &App{history: st}

	sess, err := st.EnsureSession("persona-1")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	c, err := st.EnsureChunk(sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("建片失败: %v", err)
	}

	if got := a.continuityBlock(c); got != "" {
		t.Errorf("什么都没有时应当返回空串，实际 %q", got)
	}
}
