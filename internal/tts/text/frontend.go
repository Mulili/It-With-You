// Package text 是中文文本前端的第一步：归一化 → 拼音（含多音字）→ opencpop 音素。
//
// 它**只做中文**。英/日/韩与 BERT 侧都还没进来（方案 §6：英日韩直接喂零、中文零 BERT 可跑）。
// 已知差距写在 doc 末尾，验收时要对着它一项项销。
package text

import (
	"fmt"
	"os"
	"strings"

	"github.com/mozillazg/go-pinyin"
)

// Options 是前端需要的两份数据。
type Options struct {
	// OpencpopPath 是 opencpop-strict.txt（拼音 → 声母 + 韵母）
	OpencpopPath string
	// PolyphonePath 是多音字表（词 → 逐字拼音），可为空
	PolyphonePath string
	// MaxWordLen 是多音字词表的最大词长；<= 0 时取 4
	MaxWordLen int
}

// Frontend 把一句中文变成 GPT-SoVITS 的音素序列。
type Frontend struct {
	opencpop map[string]string   // "ni" -> "n i"
	poly     map[string][]string // "湖泊" -> ["hu2","po1"]
	maxWord  int
	pyArgs   pinyin.Args
}

// New 载入数据。
func New(opts Options) (*Frontend, error) {
	f := &Frontend{
		opencpop: make(map[string]string, 512),
		poly:     make(map[string][]string, 45000),
		maxWord:  opts.MaxWordLen,
	}
	if f.maxWord <= 0 {
		f.maxWord = 4
	}
	f.pyArgs = pinyin.NewArgs()
	f.pyArgs.Style = pinyin.Tone3
	f.pyArgs.Heteronym = false

	lines, err := readLines(opts.OpencpopPath)
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		f.opencpop[parts[0]] = parts[1]
	}
	if len(f.opencpop) == 0 {
		return nil, fmt.Errorf("%s 里没有映射（格式应为「拼音<TAB>声母 韵母」）", opts.OpencpopPath)
	}

	if opts.PolyphonePath != "" {
		if err := f.loadPolyphones(opts.PolyphonePath); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.Split(text, "\n"), nil
}
