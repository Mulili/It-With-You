package tool

import (
	"context"
	"fmt"
	"time"

	"agent-for-you-love/internal/llm"
)

// Clock 报当前时间。它是本项目接的**第一个工具**。
//
// 选它先上的三个理由（都不是"因为它简单"）：
//  1. **她确实缺这个能力**：现在 system 里根本没有当前时间——你问她「今天周几」，
//     她只能猜（本项目所有时间都是相对值，如回忆时的「3 天前」，那是我们替她算好的）；
//  2. **零依赖、零成本、结果确定**：正好用来把整条 tool 链路（声明 → 调用 → 结果回灌 → 收尾）
//     先跑通，并写出**确定性测试**。在它上面接搜索（网络、超时、抓正文、防注入）之前，
//     链路本身不该带着「网络」这个变量一起测——否则失败了分不清是哪一环；
//  3. 它是后面所有工具（联网搜索、抓网页）的模板：声明怎么写、参数怎么校验、
//     结果那句话怎么组织才让模型用得对。
//
// 为什么不干脆把时间写进 system：那确实省一次往返，但它是**每轮都带**的固定开销，
// 而时间只有偶尔被问到。工具是「按需取一次」，也顺手覆盖了「她想说精确到分的时间」这种情形。
// （两者并不冲突：真嫌慢可以 system 里只放日期、工具补精确时刻，那是以后的事。）
type Clock struct{}

// Spec 实现 Tool。
//
// 描述里那两句「什么时候用 / 什么时候别用」是有意写的：function calling 里模型靠描述
// 决定调不调，只写「查询当前时间」它会在每次闲聊开场都调一下。
//
// 参数显式写成空对象（而不是省略）：「没有参数」与「参数忘了写」在请求 JSON 里长得一样，
// 但写清楚能让模型不去编参数，也让下一个工具照着改时一眼看出 schema 放在哪。
func (Clock) Spec() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name: "get_current_time",
			Description: "查询当前的日期、星期与精确时间。" +
				"当用户问到今天是几号、星期几、现在几点，或者你需要知道当前时间才能把话说准" +
				"（例如他说了「今天」「现在」「明天」这类相对说法）时使用。不需要知道时间时不要调用。",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
	}
}

// Run 实现 Tool。
//
// args 现在恒为空（这个工具没有参数），所以不做解析；保留形参是为了与接口一致，
// 也为了将来加「时区」参数时不必改签名。
func (Clock) Run(_ context.Context, _ string) (string, error) {
	now := time.Now()
	// 只到分：她要回答「现在几点」时秒没有意义，反而容易被念出来
	return fmt.Sprintf("现在是 %s（%s）", now.Format("2006-01-02 15:04"), weekdayCN(now)), nil
}

// weekdayCN 返回中文星期。Go 的 Weekday 是英文枚举，直接印出来会变成 "Thursday"。
func weekdayCN(t time.Time) string {
	return "星期" + [...]string{"日", "一", "二", "三", "四", "五", "六"}[int(t.Weekday())]
}
