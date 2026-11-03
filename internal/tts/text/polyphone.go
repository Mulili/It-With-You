package text

import (
	"encoding/json"
	"fmt"
	"os"
)

func (f *Frontend) loadPolyphones(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取多音字表失败：%w", err)
	}
	if err := json.Unmarshal(b, &f.poly); err != nil {
		return fmt.Errorf("%s 解析失败：%w", path, err)
	}
	if len(f.poly) == 0 {
		return fmt.Errorf("%s 是空的", path)
	}
	return nil
}

// matchWord 在 rs[i:] 上做最长匹配，返回命中的词长与逐字拼音。
//
// 这里没有分词器：多音字表本身就是词表，最长匹配能覆盖大部分情况（湖泊、地壳、曝光…）。
// 剩下那些必须靠上下文才能定的，交给 go-pinyin 的默认读音——它不会崩，只是偶尔读错。
func (f *Frontend) matchWord(rs []rune, i int) (int, []string) {
	limit := len(rs) - i
	if limit > f.maxWord {
		limit = f.maxWord
	}
	for n := limit; n >= 2; n-- {
		key := string(rs[i : i+n])
		if py, ok := f.poly[key]; ok && len(py) == n {
			return n, py
		}
	}
	return 0, nil
}
