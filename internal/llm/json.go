package llm

import "strings"

// StripCodeFence 去掉模型偶尔加上的 ```json ... ``` 包裹。
//
// 各处的提示词都会要求"只输出 json、不要包在代码块里"，但实际仍会偶发——
// 不做这层容错就会白丢一次调用，而用户看到的现象是"我说了它却没记住"。
//
// 放在 llm 包而不是各调用点各写一份：凡是解析模型 JSON 输出的地方都需要它
// （人格指令抽取、话题边界判断、将来的记忆抽取），而它只依赖字符串，与业务无关。
func StripCodeFence(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	s = strings.TrimPrefix(s, "JSON")
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
