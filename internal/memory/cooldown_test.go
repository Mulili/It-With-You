package memory

import "testing"

// 冷却窗口的边界要钉死：它是"每 CooldownWindow 最多主动提一次"的节流阀，
// 差一毫秒就变成"永远不提"或"完全不冷却"，两种都很难从日志里看出来。
func TestInCooldown(t *testing.T) {
	now := int64(1_800_000_000_000)

	cases := []struct {
		name   string
		last   int64
		want   bool
		reason string
	}{
		{"从未注入", 0, false, "零值必须算 fresh，否则老数据全被冻住"},
		{"刚刚注入", now, true, ""},
		{"窗口内最后一毫秒", now - CooldownWindow + 1, true, ""},
		{"正好到期", now - CooldownWindow, false, "窗口是左闭右开：到点即解冻"},
		{"早已过期", now - 30*24*60*60*1000, false, ""},
		{"时钟回拨（未来时间）", now + 5*60*1000, true, "按刚注入处理：宁可少提，也别因时钟跳变而疯狂复读"},
	}
	for _, c := range cases {
		if got := InCooldown(c.last, now); got != c.want {
			t.Errorf("%s: InCooldown(%d) = %v，期望 %v。%s", c.name, c.last, got, c.want, c.reason)
		}
	}
}

// 硬窗是窗口的子集：它只负责"连着两三轮别再说同一件事"。
func TestJustInjected(t *testing.T) {
	now := int64(1_800_000_000_000)

	if JustInjected(0, now) {
		t.Error("从未注入不该算「刚刚才提过」")
	}
	if !JustInjected(now-1, now) {
		t.Error("一毫秒前注入应当算「刚刚才提过」")
	}
	if JustInjected(now-CooldownHard, now) {
		t.Error("硬窗到点即解除")
	}
	if !JustInjected(now-CooldownHard+1, now) {
		t.Error("硬窗内应当算「刚刚才提过」")
	}
	if CooldownHard >= CooldownWindow {
		t.Errorf("硬窗（%d）必须短于冷却窗（%d），否则「刚刚才提过」就失去意义", CooldownHard, CooldownWindow)
	}
}

// SaidBefore 是两个来源的并集：本段对话里真的用过，或还在时间冷却里。
// 这张表就是"什么时候该用弱化措辞"的完整定义，改任何一支都应先改它。
func TestSaidBefore(t *testing.T) {
	now := int64(1_800_000_000_000)
	m := Memory{LastRecalledSession: "s1", RecallCount: 2}

	if !m.SaidBefore("s1", now) {
		t.Error("本段对话里用过 → 算说过")
	}
	if m.SaidBefore("s2", now) {
		t.Error("只换了个会话、也没在冷却里 → 应当允许重新提一次")
	}
	if m.SaidBefore("", now) {
		t.Error("会话未知时不拿「提过」当理由——那是确定不了的事实")
	}

	m2 := Memory{LastInjectedAt: now - 60_000}
	if !m2.SaidBefore("", now) {
		t.Error("冷却窗内应当算说过（跨会话也有效，这是它比 session 判据强的地方）")
	}
	if m2.SaidBefore("", now+CooldownWindow) {
		t.Error("冷却到期应当自动回到 fresh")
	}
}
