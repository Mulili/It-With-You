package tts

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Symbols 是 GPT-SoVITS v2 的音素表（symbols_v2.json，符号 → id）。
//
// 这张表**必须用上游那份**，不能用通用拼音方案：模型认的是它训练时的符号集，
// 换一套就成了"每个字都念错"（方案 §6 第 3 条）。
type Symbols struct {
	ids map[string]int64
}

// LoadSymbols 读入音素表。
func LoadSymbols(path string) (*Symbols, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取音素表失败：%w", err)
	}
	var m map[string]int64
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%w", path, err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%s 是空的", path)
	}
	return &Symbols{ids: m}, nil
}

// IDs 把音素字符串序列变成 id 序列。
//
// 缺任何一个符号都**明确报错**：静默跳过会让"少了一个字"听起来只是口齿不清，
// 从音质上根本定位不到是这里丢了。
func (s *Symbols) IDs(phones []string) ([]int64, error) {
	out := make([]int64, 0, len(phones))
	var missing []string
	for _, p := range phones {
		id, ok := s.ids[p]
		if !ok {
			missing = append(missing, p)
			continue
		}
		out = append(out, id)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("音素表里没有这些符号：%s", strings.Join(missing, "、"))
	}
	return out, nil
}

// Len 返回符号数（启动日志用）。
func (s *Symbols) Len() int { return len(s.ids) }

// Filter 丢掉音素表里没有的符号，返回保留下来的序列与被丢掉的符号。
//
// 为什么是"丢"而不是"报错"：标点与少数韵母（如轻声 e5）未必都在表里，
// 而整句因为一个符号失败，用户看到的就是"点了没反应"。丢掉的表现只是那一个字听不清。
func (s *Symbols) Filter(phones []string) (kept, dropped []string) {
	kept = make([]string, 0, len(phones))
	for _, p := range phones {
		if _, ok := s.ids[p]; ok {
			kept = append(kept, p)
			continue
		}
		dropped = append(dropped, p)
	}
	return kept, dropped
}
