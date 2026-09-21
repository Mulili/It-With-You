package history

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-for-you-love/internal/llm"
)

// 本文件是「话题边界」判断：每轮问一次模型"刚才这句是不是结束了当前话题"。
//
// 为什么每轮都要问、而不能像人格指令那样先做本地粗筛："话题结束"没有任何可靠的词面特征——
// 一句"嗯"可能是敷衍，也可能是收尾。所以这里没有粗筛，每轮调一次。
// 成本可忽略（几百 token 输入 + 十几 token 输出），换来的是会话边界不靠猜。

// TopicJudgeContextLimit 是判断话题边界时带上的最近消息条数。
//
// 只给最后一句是判断不了的："嗯"到底算敷衍还是收尾，要看上文在聊什么。
// 给太多也没用——话题边界是局部现象，十几条足够，反而更省 token、干扰更少。
const TopicJudgeContextLimit = 12

// TopicVerdict 是话题边界判断的结果。
type TopicVerdict struct {
	// Ended 为 true 表示最后一条用户消息结束了当前话题
	Ended bool `json:"topicEnded"`
	// Reason 是模型给出的一句话依据，只用于日志（不展示给用户）
	Reason string `json:"reason"`
}

// TopicPrompt 生成判断话题边界的提示词。
//
// recent 是当前会话最近若干条消息，**必须包含刚写进去的那一条用户消息**——
// 判断的对象就是"最后一条用户消息是否收尾"。
func TopicPrompt(recent []llm.Message) (system, user string) {
	var b strings.Builder
	b.WriteString("你在判断一段对话的「话题边界」。\n\n")
	b.WriteString("下面给你最近几轮对话。请判断：**最后一条用户消息**是否意味着当前话题已经结束。\n\n")
	b.WriteString("算结束的情形：\n")
	b.WriteString("- 用户明确表示不聊了（「好，就这样吧」「不说了」「我去睡了」）\n")
	b.WriteString("- 用户开启了一个与上文无关的新话题（「对了，我昨天买了个键盘」）\n")
	b.WriteString("- 一件具体的事办完了、且没有自然延续（「搞定了，谢谢」）\n\n")
	b.WriteString("**不算**结束的情形：\n")
	b.WriteString("- 只是短促回应（「嗯」「哈哈」「好的」）——话题还在继续\n")
	b.WriteString("- 还在追问同一件事的后续\n")
	b.WriteString("- 单纯的情绪表达（「好累啊」），除非它明显是在收尾\n\n")
	b.WriteString("**拿不准就判 false**：多切一刀会把一段完整的对话劈成两半，\n")
	b.WriteString("少切一刀只是让这一段长一点——后者代价小得多。\n\n")
	b.WriteString("只输出一个 json 对象，不要解释、不要包在代码块里：\n")
	b.WriteString(`{"topicEnded": true 或 false, "reason": "一句话依据"}`)

	return "你是对话分析器，只输出 json。", b.String() + "\n\n对话：\n" + renderConversation(recent)
}

// renderConversation 把消息渲染成"用户：… / 你：…"的文本。
func renderConversation(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		who := "用户"
		if m.Role == llm.RoleAssistant {
			who = "你"
		}
		fmt.Fprintf(&b, "%s：%s\n", who, m.Content)
	}
	return b.String()
}

// ParseTopicVerdict 解析判断结果。
//
// 返回 error 表示这次输出不可用；调用方按"未结束"处理并记日志即可——
// 这是启发式判断，一次没判断出来不影响对话本身，而误判成"结束"的代价更大。
func ParseTopicVerdict(raw string) (TopicVerdict, error) {
	var v TopicVerdict
	if err := json.Unmarshal([]byte(llm.StripCodeFence(raw)), &v); err != nil {
		return TopicVerdict{}, fmt.Errorf("话题判断结果不是合法 json: %w", err)
	}
	return v, nil
}
