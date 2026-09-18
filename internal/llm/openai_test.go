package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
)

// 这两个测试会**真的调用一次模型**，属于集成测试：没配 API Key 时自动跳过。
//
// 为什么不做成纯单元测试：本步要验证的是"服务端确实接受 response_format: json_object
// 并返回可解析的 JSON"，这件事只有打一次真请求才能确认，mock 掉就没有意义了。
//
// 运行方式（在项目根目录，一条命令即可）：
//
//	go test ./internal/llm -run TestChat -v -count=1
//
// Key 从哪来：`go test` 的工作目录是**包目录**，而 .env 在项目根，所以要向上找一次
// （config.LoadDotEnvUpward）。已经设好的环境变量仍然优先，因为它不覆盖已有值。
//
// -count=1 是为了绕开 go test 的结果缓存，否则第二次跑会直接复用上次结论、根本没发请求。

// newTestProvider 返回一个可用的 provider；没有 Key 时跳过测试。
func newTestProvider(t *testing.T) (*OpenAIProvider, context.Context) {
	t.Helper()
	// 顺序不能反：先补 .env，再读配置
	config.LoadDotEnvUpward()
	cfg := ConfigFromEnv()
	if cfg.APIKey == "" {
		t.Skip("未配置 COMPANION_LLM_API_KEY（.env 或环境变量），跳过集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return NewOpenAIProvider(cfg), ctx
}

// 验证 JSON 模式：这是阶段3 抽取链路的地基，必须确认服务端真的返回可解析的 JSON。
func TestChatJSONMode(t *testing.T) {
	p, ctx := newTestProvider(t)

	// 提示词里必须出现 "json" 字样，否则服务端会拒绝 response_format（OpenAI 与 DeepSeek 都如此）
	out, err := p.Chat(ctx, []Message{
		{Role: RoleSystem, Content: "你是信息抽取器，只输出 json，不要任何解释文字。"},
		{Role: RoleUser, Content: `从这句话里抽取「称呼」相关信息，以 json 输出，字段为 slot 与 value。句子：以后呢喊你Lapwing怎么样`},
	}, ChatOptions{JSON: true})
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}

	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("返回内容不是合法 JSON: %v\n原始内容: %s", err, out)
	}
	if len(v) == 0 {
		t.Fatalf("返回的是空 JSON 对象: %s", out)
	}
	t.Logf("JSON 模式正常，模型输出: %s", out)
}

// 验证非 JSON 模式：确认抽出来的公用请求构造没有把普通问答带坏
// （例如误加了 response_format，或 Accept 头改错了）。
func TestChatPlainText(t *testing.T) {
	p, ctx := newTestProvider(t)

	out, err := p.Chat(ctx, []Message{
		{Role: RoleUser, Content: "只回复两个字：你好"},
	}, ChatOptions{})
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("返回内容为空")
	}
	// 普通模式不该被强制成 JSON：这里只要求它不是纯 JSON 对象即可
	var v map[string]any
	if json.Unmarshal([]byte(out), &v) == nil && len(v) > 0 && !strings.Contains(out, "你好") {
		t.Fatalf("普通模式疑似被强加了 JSON 输出: %s", out)
	}
	t.Logf("非流式普通回复正常，模型输出: %s", out)
}
