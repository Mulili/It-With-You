package history

import (
	"strings"
	"testing"

	"agent-for-you-love/internal/llm"
)

func TestParseTopicVerdict(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"判为结束", `{"topicEnded": true, "reason": "用户说不聊了"}`, true},
		{"判为继续", `{"topicEnded": false, "reason": "只是附和"}`, false},
		{"缺 reason 字段也能解析", `{"topicEnded": true}`, true},
		{"裸 JSON 之外的空白不影响", "\n  {\"topicEnded\": false}\n", false},
		// 提示词里明确要求不要包代码块，但模型仍会偶发——这层容错不做就会白丢一次判断
		{"容忍代码块包裹", "```json\n{\"topicEnded\": true}\n```", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := ParseTopicVerdict(c.raw)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if v.Ended != c.want {
				t.Errorf("Ended = %v，期望 %v", v.Ended, c.want)
			}
		})
	}
}

// 解析不了就该报错，让调用方按"未结束"处理并记日志——
// 静默当成 false 会把"模型输出坏了"和"话题确实没结束"混成一件事，排查时无从下手。
func TestParseTopicVerdictRejectsGarbage(t *testing.T) {
	if _, err := ParseTopicVerdict("我觉得这个话题差不多结束了"); err == nil {
		t.Fatal("非 json 输入应当报错")
	}
}

// 提示词里**必须出现 "json" 字样**，否则 OpenAI / DeepSeek 的 json_object 模式会直接报错。
// 人格指令抽取那边踩过这个坑，这里也钉一条。
func TestTopicPromptMentionsJSON(t *testing.T) {
	system, user := TopicPrompt([]llm.Message{{Role: llm.RoleUser, Content: "在吗"}})
	if !strings.Contains(strings.ToLower(system+user), "json") {
		t.Error("提示词里必须出现 json 字样（json_object 模式的要求）")
	}
}

// 对话要带上角色，否则模型分不清哪句是用户说的、哪句是自己说的——
// 而"最后一条用户消息"正是它要判断的对象。
func TestTopicPromptRendersRoles(t *testing.T) {
	_, user := TopicPrompt([]llm.Message{
		{Role: llm.RoleUser, Content: "帮我看看这个函数"},
		{Role: llm.RoleAssistant, Content: "好的，贴上来"},
		{Role: llm.RoleUser, Content: "算了，搞定了"},
	})

	for _, want := range []string{"用户：帮我看看这个函数", "你：好的，贴上来", "用户：算了，搞定了"} {
		if !strings.Contains(user, want) {
			t.Errorf("提示词里应当包含 %q，实际：\n%s", want, user)
		}
	}
	// 顺序也要对：模型判断的是"最后一条"，顺序错了对象就错了
	if strings.Index(user, "算了，搞定了") < strings.Index(user, "帮我看看这个函数") {
		t.Error("对话应当按时间正序渲染")
	}
}
