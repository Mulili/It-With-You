package tokenizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findModelDir 返回 tokenizer.json 所在目录，找不到返回 ""。
//
// 抽成函数是为了测试与基准共用；两者都只依赖"有没有模型文件"，
// 仓库里不该因为缺少 543MB 的模型而让全量测试挂掉。
func findModelDir() string {
	if d := os.Getenv("COMPANION_EMBED_MODEL_DIR"); d != "" {
		if _, err := os.Stat(filepath.Join(d, "tokenizer.json")); err == nil {
			return d
		}
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		p := filepath.Join(d, "It-With-You", "models", "bge-m3-onnx-int8")
		if _, err := os.Stat(filepath.Join(p, "tokenizer.json")); err == nil {
			return p
		}
	}
	return ""
}

// modelDir 同 findModelDir，但找不到就 Skip（测试不该因为缺模型而红）。
func modelDir(t *testing.T) string {
	t.Helper()
	d := findModelDir()
	if d == "" {
		t.Skip("找不到 tokenizer.json（设 COMPANION_EMBED_MODEL_DIR 或把模型放到默认位置），跳过对拍")
	}
	return d
}

type goldenCase struct {
	Text       string   `json:"text"`
	Normalized string   `json:"normalized"`
	IDs        []int32  `json:"ids"`
	Tokens     []string `json:"tokens"`
}

type goldenFile struct {
	Cases []goldenCase `json:"cases"`
}

// 这条测试是这个包存在的**唯一依据**：拿官方 HuggingFace `tokenizers` 产出的基准
// 逐 token 对拍。
//
// 为什么必须这么严：分词错了**不会报错**，只会让向量悄悄偏掉，而症状是"记忆偶尔召回
// 不到"——那种错从应用层根本看不出根因。所以这里要逐条逐 id 相等，不接受"大多数对"。
//
// 归一化与切分**分开报**：归一化错了会让后面全错，先把它摘出来，排查范围立刻小一半。
//
// # 基准（testdata/golden.json）是怎么来的
//
// 真值只有一处来源：**HuggingFace 官方的 `tokenizers`**（Python 包，与它家 Rust 实现
// 同一份代码）。用我们自己的 Go 实现去"重算基准"是自证循环，等于没有基准——所以这份
// 基准是用 Python 生成的，**并且已经提交进仓库**：
//
//	本项目**不需要 Python**：构建、运行、跑测试都只用 golden.json，不碰 Python。
//	只有"换了 tokenizer.json"时才需要重新生成一次，而那是一次性的开发动作。
//
// 刷新的办法（在仓库外随手跑，跑完把 golden.json 提交即可）：
//
//	pip install tokenizers
//
//	import json, os
//	from tokenizers import Tokenizer
//	MODEL = os.environ["COMPANION_EMBED_MODEL_DIR"]   # 含 tokenizer.json 的目录
//	tk = Tokenizer.from_file(os.path.join(MODEL, "tokenizer.json"))
//	p = "internal/llm/tokenizer/testdata/golden.json"
//	doc = json.load(open(p, encoding="utf-8"))
//	for c in doc["cases"]:
//	    e = tk.encode(c["text"])
//	    c["normalized"] = tk.normalizer.normalize_str(c["text"])
//	    c["ids"], c["tokens"] = e.ids, e.tokens
//	json.dump(doc, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
//
// 语料（每条用例的 `text`）本身就存在 golden.json 里，所以想加用例只要加一条
// 只带 `text` 的项、再跑上面那段；扩语料时优先收真实文本（项目文档、内置人格、
// 真实聊天），合成语料只是用来撞边界情况的。
//
// ⚠️ 换 tokenizer.json 时**基准和实现可能一起变**：那时候要对拍结果重新看，
// 不能假定现有实现还成立。
func TestAgainstOfficialTokenizer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatalf("读基准文件失败: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("解析基准文件失败: %v", err)
	}
	if len(g.Cases) == 0 {
		t.Fatal("基准文件里没有用例")
	}

	tk, err := New(filepath.Join(modelDir(t), "tokenizer.json"))
	if err != nil {
		t.Fatalf("建分词器失败: %v", err)
	}
	t.Logf("词表 %d 条，unigram 候选长度种类 %d 种", tk.VocabSize(), len(tk.unigram.pieceRuneLens))

	var normBad, idBad int
	// 详细条目只打前若干条：几百条语料全错时，刷屏反而看不出是哪一类错。
	const maxDetail = 8
	for _, c := range g.Cases {
		if got := tk.Normalize(c.Text); got != c.Normalized {
			normBad++
			if normBad <= maxDetail {
				t.Errorf("归一化不一致\n  文本 %q\n  基准 %q\n  实际 %q", c.Text, c.Normalized, got)
			}
			continue // 归一化都不对，比较 id 没有诊断价值
		}
		got := tk.Encode(c.Text)
		if !equalIDs(got, c.IDs) {
			idBad++
			if idBad <= maxDetail {
				t.Errorf("token 不一致\n  文本 %q（归一化后 %q）\n  基准 %v\n  实际 %v\n  基准 tokens %v",
					c.Text, c.Normalized, c.IDs, got, c.Tokens)
			}
		}
	}
	t.Logf("共 %d 条 / %d 个 token：归一化不一致 %d 条，token 不一致 %d 条",
		len(g.Cases), totalTokens(g.Cases), normBad, idBad)
	if normBad+idBad > 0 {
		t.Errorf("对拍未全过（详细只打了前 %d 条）", maxDetail)
	}
}

func totalTokens(cs []goldenCase) int {
	n := 0
	for _, c := range cs {
		n += len(c.IDs)
	}
	return n
}

func equalIDs(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 基准是为了回答一个具体问题：**自己写的分词器会不会拖慢对话**。
//
// 参照物不是"Rust 版多少微秒"，而是**同一条链路上后面的那一步**：
// int8 ONNX 在 CPU 上跑一次前向是毫秒级，分词只要比它小一个数量级就无关紧要。
func benchEncode(b *testing.B, text string) {
	b.Helper()
	d := findModelDir()
	if d == "" {
		b.Skip("找不到 tokenizer.json，跳过基准")
	}
	tk, err := New(filepath.Join(d, "tokenizer.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tk.Encode(text)
	}
}

// 一句短问句（9 个字）
func BenchmarkEncodeShort(b *testing.B) { benchEncode(b, "用户已经三天没来了") }

// 一条记忆/规则的真实长度（约 40 字）
func BenchmarkEncodeFact(b *testing.B) {
	benchEncode(b, "称呼：主人；她喜欢在晚上聊天，不喜欢被催；周三她通常会加班到很晚")
}

// 一段中型文本（约 170 字）
func BenchmarkEncodeMedium(b *testing.B) {
	benchEncode(b, strings.Repeat("今天天气不错，我们去散步吧。", 10))
}

// 片摘要量级（约 1700 字）
func BenchmarkEncodeLong(b *testing.B) {
	benchEncode(b, strings.Repeat("今天天气不错，我们去散步吧。", 100))
}
