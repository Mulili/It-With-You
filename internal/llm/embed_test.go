package llm

import (
	"context"
	"math"
	"testing"
	"time"

	"agent-for-you-love/internal/config"
)

// 嵌入的集成测试：需要一个跑着的 OpenAI 兼容嵌入服务（默认本机 Ollama）。
// 与 openai_test.go 同一套约定——向上找 .env、探测不通就跳过、-count=1 才有意义：
//
//	go test ./internal/llm -run TestEmbed -v -count=1
//
// 探测不通选择 Skip 而不是 Fail：本机没装 Ollama 的人跑全量测试不该被卡住，
// 而"嵌入不可用"本身就是一个允许的运行状态（记忆功能停用，对话照常）。

func newTestEmbedder(t *testing.T) (*OpenAIEmbedder, context.Context) {
	t.Helper()
	config.LoadDotEnvUpward()
	cfg := EmbedConfigFromEnv()
	e := NewOpenAIEmbedder(cfg)

	// 给 60s：首次调用要把模型加载进内存，可能比后续请求慢很多
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	if err := e.Verify(ctx); err != nil {
		t.Skipf("嵌入服务不可用（%s @ %s），跳过集成测试: %v", cfg.Model, cfg.BaseURL, err)
	}
	return e, ctx
}

// cos 是两个向量的余弦相似度，只用于测试断言。
// 不处理零向量——被测数据出现零向量本身就是失败，另有断言专门拦它。
func cos(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// 维度要对，而且不能是全零。
// 全零向量能通过维度检查，但在余弦距离下毫无意义（分母为零）。
func TestEmbedDimAndNonZero(t *testing.T) {
	e, ctx := newTestEmbedder(t)

	vecs, err := e.Embed(ctx, []string{"hello"})
	if err != nil {
		t.Fatalf("取向量失败: %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("请求 1 条应当返回 1 条，实际 %d 条", len(vecs))
	}
	if got := len(vecs[0]); got != e.Dim() {
		t.Fatalf("维度应当是 %d，实际 %d", e.Dim(), got)
	}

	var absSum float64
	for _, v := range vecs[0] {
		absSum += math.Abs(float64(v))
	}
	if absSum == 0 {
		t.Fatal("返回了全零向量")
	}
	t.Logf("维度 %d，分量绝对值之和 %.4f", len(vecs[0]), absSum)
}

// 批量返回的顺序必须与入参一一对应。
//
// 这条要单独测：顺序错位是**静默错误**——存进去的向量指向另一句话，
// 检索时只表现为"偶尔搜出些不相关的东西"，从结果上根本看不出是顺序问题。
func TestEmbedBatchOrder(t *testing.T) {
	e, ctx := newTestEmbedder(t)

	const first = "I am very tired today"
	const second = "How much is a kilogram of apples"

	batch, err := e.Embed(ctx, []string{first, second})
	if err != nil {
		t.Fatalf("批量取向量失败: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("请求 2 条应当返回 2 条，实际 %d 条", len(batch))
	}

	// 逐条单独取一次做对照：同一段文本的向量应当基本一致
	onlyFirst, err := e.Embed(ctx, []string{first})
	if err != nil {
		t.Fatalf("单条取向量失败: %v", err)
	}
	if s := cos(batch[0], onlyFirst[0]); s < 0.999 {
		t.Errorf("批量第 0 条与单独请求的结果不一致（相似度 %.4f），批量可能错位", s)
	}
	// 两句无关文本的向量不该雷同，否则说明批量返回的是同一份结果
	if s := cos(batch[0], batch[1]); s > 0.9 {
		t.Errorf("两句无关文本的相似度高达 %.4f，批量可能返回了同一份结果", s)
	}
}

// 语义要真的成立，否则可能是"接了个只会返回固定值的假服务"。
//
// 断言用**相对关系**而不是绝对阈值：不同模型、不同量化程度的绝对相似度差异不小
// （本机 bge-m3 量化版实测 0.88），但"相似对明显高于无关对"是稳定的。
func TestEmbedSemanticSeparation(t *testing.T) {
	e, ctx := newTestEmbedder(t)

	vecs, err := e.Embed(ctx, []string{
		"今天好累啊",   // 0
		"我今天特别疲惫", // 1：与 0 同义
		"苹果多少钱一斤", // 2：与 0 无关
	})
	if err != nil {
		t.Fatalf("取向量失败: %v", err)
	}

	similar := cos(vecs[0], vecs[1])
	unrelated := cos(vecs[0], vecs[2])
	t.Logf("相似对 %.4f，无关对 %.4f", similar, unrelated)

	if similar <= unrelated+0.2 {
		t.Errorf("语义区分度不成立：相似对 %.4f 应当比无关对 %.4f 高出至少 0.2", similar, unrelated)
	}
}
