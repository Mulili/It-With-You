package llm

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 测试用的资源查找 ----------
//
// ⚠️ 这一段是**测试脚手架**：只在跑测试时找文件，找不到就 Skip。
// App 侧的正式解析（设置项 → exe 同目录 → 用户缓存目录 → 缺了就拦截）在接线那一步做，
// 不放在这里——测试不该替应用决定"模型该放哪"。

const (
	envONNXRuntimePath = "COMPANION_ONNXRUNTIME_DLL"
	envEmbedModelDir   = "COMPANION_EMBED_MODEL_DIR"
)

func testModelDir() string {
	if d := os.Getenv(envEmbedModelDir); d != "" {
		return d
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "It-With-You", "models", "bge-m3-onnx-int8")
	}
	return ""
}

// testEmbedConfig 组装测试用的配置；缺任何一件就 Skip。
//
// 两件资源都需要显式给（模型目录 + dll 路径）：
// 模型是 543MB 的外部文件、dll 跟 binding 版本绑死，都不该让仓库替用户假定位置。
func testEmbedConfig(t *testing.T) ONNXEmbedConfig {
	t.Helper()
	dll := os.Getenv(envONNXRuntimePath)
	dir := testModelDir()
	if dll == "" || dir == "" {
		t.Skipf("跳过：需要 %s（onnxruntime 动态库路径）与 %s（含 tokenizer.json 的模型目录）",
			envONNXRuntimePath, envEmbedModelDir)
	}
	cfg := ONNXEmbedConfig{
		RuntimePath:   dll,
		ModelPath:     filepath.Join(dir, "model_quantized.onnx"),
		TokenizerPath: filepath.Join(dir, "tokenizer.json"),
	}
	for _, p := range []string{cfg.RuntimePath, cfg.ModelPath, cfg.TokenizerPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("跳过：找不到 %s", p)
		}
	}
	return cfg
}

// ---------- 基准 ----------

type embedGoldenCase struct {
	Text   string    `json:"text"`
	Tokens int       `json:"tokens"`
	Vector []float32 `json:"vector"`
}

type embedGoldenFile struct {
	Dim                  int               `json:"dim"`
	ReferenceONNXRuntime string            `json:"reference_onnxruntime"`
	Cases                []embedGoldenCase `json:"cases"`
}

func loadEmbedGolden(t *testing.T) embedGoldenFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "embed_golden.json"))
	if err != nil {
		t.Fatalf("读向量基准失败: %v", err)
	}
	var g embedGoldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("解析向量基准失败: %v", err)
	}
	if len(g.Cases) == 0 {
		t.Fatal("向量基准里没有用例")
	}
	return g
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---------- 用例 ----------

// 三个路径都空、或文件不存在时必须**报错**（不是 panic、更不是勉强跑起来）。
//
// 这条守的是用户明确提过的那条线：不允许出现"聊完了没记忆"。
// 嵌入不可用的唯一正确处理是**在启动时拦住并说清缺什么**，而拦住的前提是这里先报错。
func TestONNXEmbedderRejectsMissingFiles(t *testing.T) {
	if _, err := NewONNXEmbedder(ONNXEmbedConfig{}); err == nil {
		t.Fatal("三个路径都是空，居然建成功了")
	}
	_, err := NewONNXEmbedder(ONNXEmbedConfig{
		RuntimePath: "nope.dll", ModelPath: "nope.onnx", TokenizerPath: "nope.json",
	})
	if err == nil {
		t.Fatal("文件都不存在，居然建成功了")
	}
	t.Logf("按预期报错: %v", err)
}

// 逐条与**官方 tokenizers + onnxruntime** 产出的基准向量对拍。
//
// 逐条（而不是整批）比对是有意的：这个模型的 int8 动态量化让**激活的量化区间取决于
// 批内组成**，所以同一句话单独嵌与放在批量里嵌，结果会有约 1e-2 量级的逐元素差
// （见 TestONNXEmbedderBatchMatchesSingles）。逐条比才能把"管线的错"与"量化的正常抖动"分开。
//
// ⚠️ 容差 0.97 是**按实测定的**，不是拍的：基准用的是 onnxruntime 1.22.1
// （Python 那个包自带），而我们的 binding 把 C API 固定在 1.29.0、必须配 1.29.0 的 dll，
// 两者的 int8 算子实现不同，实测余弦落在 **0.9877 ~ 0.9933**。
// 这个量级与当初"ONNX int8 vs Ollama Q8_0"测出来的 0.98~0.99 完全一致，
// 说明它来自**两种 int8 实现**，而不是我们的管线。
//
// 这个阈值够不够严？管线错的量级完全不同：当初分词少一个 token 时余弦直接掉到 **0.86**。
// 0.97 与 0.86 之间有很大间隔，所以它抓得住"管线接错"这一类。
func TestONNXEmbedderMatchesReference(t *testing.T) {
	cfg := testEmbedConfig(t)
	e, err := NewONNXEmbedder(cfg)
	if err != nil {
		t.Fatalf("建嵌入器失败: %v", err)
	}
	defer e.Close()

	g := loadEmbedGolden(t)
	if e.Dim() != g.Dim {
		t.Fatalf("维度不一致：实现 %d，基准 %d", e.Dim(), g.Dim)
	}

	ctx := context.Background()
	minCos := 1.0
	for _, c := range g.Cases {
		got, err := e.Embed(ctx, []string{c.Text})
		if err != nil {
			t.Fatalf("嵌入 %q 失败: %v", c.Text, err)
		}
		if len(got) != 1 || len(got[0]) != g.Dim {
			t.Fatalf("嵌入 %q 返回 %d 条 %d 维，期望 1 条 %d 维", c.Text, len(got), len(got[0]), g.Dim)
		}
		cos := cosine(got[0], c.Vector)
		if cos < minCos {
			minCos = cos
		}
		if cos < 0.97 {
			t.Errorf("与基准向量偏差过大：cos=%.6f（文本 %q）", cos, c.Text)
		}
	}
	t.Logf("逐条对拍 %d 条通过，最低余弦 %.8f（基准 onnxruntime %s，本机 %s）",
		len(g.Cases), minCos, g.ReferenceONNXRuntime, ONNXRuntimeVersion())
}

// 整批嵌一次，逐条与"单独嵌"比较。
//
// ⚠️ 容差 0.97 是按**实测**定的，而且这里必须把一个被证伪的假设写清楚：
//
// 我原本以为偏差来自"int8 动态量化的尺度按整个张量统计，所以短句被长文拖着 padding 会失真"。
// 实测否掉了它——8 条一批（短句 pad 到 1800）时最低余弦 0.9786，
// 按长度分组后（短句只 pad 到 11）最低余弦 0.9792，**几乎没变**。
//
// 真正的原因是**这个 int8 模型对输入形状敏感**：形状不同会走不同的 GEMM kernel，
// 累加顺序不同，24 层攒出约 2% 的余弦差。这是模型+runtime 的性质，代码这一层改不了；
// 真要消除只能"一条一次 Run"，代价是批量场景慢一个数量级（不划算，见 Embed 的注释）。
//
// 所以这条用例守的不是"数值一致"，而是**接线正确**：
// 补位长度算错、mask 置错、行序错位都会让批量结果与单条结果差得远得多。
// 更尖的那两条在别处：TestONNXEmbedderRowsDoNotBleed（逐位相同）与
// TestONNXEmbedderDeterministic（两次调用逐位相同）。
func TestONNXEmbedderBatchMatchesSingles(t *testing.T) {
	cfg := testEmbedConfig(t)
	e, err := NewONNXEmbedder(cfg)
	if err != nil {
		t.Fatalf("建嵌入器失败: %v", err)
	}
	defer e.Close()

	g := loadEmbedGolden(t)
	texts := make([]string, len(g.Cases))
	for i, c := range g.Cases {
		texts[i] = c.Text
	}

	ctx := context.Background()
	batch, err := e.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("批量嵌入失败: %v", err)
	}
	if len(batch) != len(texts) {
		t.Fatalf("批量返回 %d 条，请求 %d 条", len(batch), len(texts))
	}

	minCos := 1.0
	for i, text := range texts {
		single, err := e.Embed(ctx, []string{text})
		if err != nil {
			t.Fatalf("单条嵌入 %q 失败: %v", text, err)
		}
		c := cosine(batch[i], single[0])
		if c < minCos {
			minCos = c
		}
		if c < 0.97 {
			t.Errorf("批量与单条差太多：cos=%.6f（第 %d 条 %q）——优先怀疑 padding / mask / 行序",
				c, i, text)
		}
	}
	t.Logf("批量 vs 单条 %d 条通过，最低余弦 %.6f（实测下界约 0.979，来自 int8 的形状敏感性）",
		len(texts), minCos)
}

// 同一批里放多条**完全相同**的文本，它们的向量必须逐位相同。
//
// 这条是"行与行之间不能串味"的直接证据：张量里每一行都是独立的（长度相同、mask 相同、
// 内容相同），所以量化尺度也一样，结果**必须**一模一样。padding 算错、mask 串行、
// 行序错位都会破坏它，而这类错在真实数据上只会表现为"有些记忆召回不准"。
func TestONNXEmbedderRowsDoNotBleed(t *testing.T) {
	cfg := testEmbedConfig(t)
	e, err := NewONNXEmbedder(cfg)
	if err != nil {
		t.Fatalf("建嵌入器失败: %v", err)
	}
	defer e.Close()

	// 短句 ×6 + 一条长文：分组后短句会在一组，长文在另一组
	const same = "她喜欢在晚上聊天"
	texts := []string{same, same, same, same, same, same, strings.Repeat("今天天气不错，我们去散步吧。", 40)}

	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("批量嵌入失败: %v", err)
	}
	for i := 1; i < 6; i++ {
		for j := range got[0] {
			if got[i][j] != got[0][j] {
				t.Fatalf("第 %d 条与第 0 条本应逐位相同，却在第 %d 维不同：%v vs %v",
					i, j, got[i][j], got[0][j])
			}
		}
	}
	// 行序也要对：第 6 条与前面几条内容不同，不该撞成一样
	if cosine(got[0], got[6]) > 0.99 {
		t.Errorf("长文那条与短句的向量几乎相同（cos=%.4f），行序可能错位了", cosine(got[0], got[6]))
	}
}

// 同样的输入跑两次必须逐位相同。
//
// 这条守的是"实现里别引入不确定性"：复用临时缓冲、忘了清零、把上一次的 mask 带过来，
// 都会让同一个字符串在两次调用里给出不同向量——而那会让"存进去的"和"查出来的"对不上。
func TestONNXEmbedderDeterministic(t *testing.T) {
	cfg := testEmbedConfig(t)
	e, err := NewONNXEmbedder(cfg)
	if err != nil {
		t.Fatalf("建嵌入器失败: %v", err)
	}
	defer e.Close()

	texts := []string{"用户已经三天没来了", "称呼：主人", "今天天气不错，我们去散步吧。"}
	ctx := context.Background()
	a, err := e.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("第一次嵌入失败: %v", err)
	}
	b, err := e.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("第二次嵌入失败: %v", err)
	}
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("第 %d 条两次结果不同（第 %d 维）：%v vs %v", i, j, a[i][j], b[i][j])
			}
		}
	}
}

// 超过 token 上限时要**截断而不是报错**，并且仍然出正确维度。
//
// 把上限压到 16 来触发，避免造一条上万 token 的文本（那会把测试跑成秒级）。
func TestONNXEmbedderTruncatesLongText(t *testing.T) {
	cfg := testEmbedConfig(t)
	cfg.MaxTokens = 16
	e, err := NewONNXEmbedder(cfg)
	if err != nil {
		t.Fatalf("建嵌入器失败: %v", err)
	}
	defer e.Close()

	long := ""
	for i := 0; i < 200; i++ {
		long += "今天天气不错，我们去散步吧。"
	}
	got, err := e.Embed(context.Background(), []string{long})
	if err != nil {
		t.Fatalf("长文本嵌入失败: %v", err)
	}
	if len(got) != 1 || len(got[0]) != e.Dim() {
		t.Fatalf("长文本返回 %d 条 %d 维，期望 1 条 %d 维", len(got), len(got[0]), e.Dim())
	}
}

// 空输入返回空，不发推理（调用方在"这一轮没有待存记忆"时会传空切片）。
func TestONNXEmbedderEmptyInput(t *testing.T) {
	cfg := testEmbedConfig(t)
	e, err := NewONNXEmbedder(cfg)
	if err != nil {
		t.Fatalf("建嵌入器失败: %v", err)
	}
	defer e.Close()

	got, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("空输入报错了: %v", err)
	}
	if got != nil {
		t.Fatalf("空输入应返回 nil，得到 %v", got)
	}
}

// 已取消的 ctx 必须在入口就被拦住 —— 推理本身打断不了（CPU 算子不像 HTTP 能取消），
// 所以这是唯一能省下这次计算的机会。
func TestONNXEmbedderHonoursCanceledContext(t *testing.T) {
	cfg := testEmbedConfig(t)
	e, err := NewONNXEmbedder(cfg)
	if err != nil {
		t.Fatalf("建嵌入器失败: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Embed(ctx, []string{"你好"}); err == nil {
		t.Fatal("ctx 已取消，居然还跑了推理")
	}
}
