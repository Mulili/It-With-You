package emotion

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"

	"agent-for-you-love/internal/llm"
)

// 第二环：把 <tone> 描述文本变成「情绪 key」。
//
// 这里刻意**不用分类器**：之前实测 8 类对话情绪模型会把 42.5% 的调侃判成生气，
// 而且换一个人格就得重训。改成「人工标注的种子句 + 向量检索」：
// 种子是 情感参考集.md 里她自己的真实发言，换人格就换一份文件，代码不动。
//
// 本环节只负责"是哪一种"，不负责"该用哪条参考音频"——参考音频按 (音色, 情绪) 组织，
// 素材还没准备，音频那一侧先欠着。

// 命中阈值：低于它就不给标签（用默认语气），宁可不换语气，也不要换错。
//
// 实测真实语气描述的余弦几乎都落在 0.55~0.75（bge-m3 的中文相似度本来就偏高），
// 所以平时起作用的是下面的 MinMargin，这里只负责挡掉"完全不相关"的查询。
const MinScore = 0.45

// MinMargin 是"赢家必须领先第二名多少"。
//
// 实测（19 条人工标注的语气描述）：各标签余弦挤在 0.6~0.7，赢家与次高常常只差 0.01，
// 那种"赢"其实是掷硬币。差值不够就当作没判出来，用默认语气——
// 语音上"语气没换"比"换错语气"伤害小得多。
const MinMargin = 0.02

// Match 是一次检索的结果。
type Match struct {
	// Label 为空表示没命中（最高分低于 MinScore）。
	Label string
	// Score 是命中标签下最接近的那条种子的余弦（0~1），用来判断"有多像"。
	Score float64
	// RunnerUp / RunnerUpScore 是第二名的标签与分数，差值反映"有多犹豫"。
	RunnerUp      string
	RunnerUpScore float64
	// Seed 是最接近的那条种子原文（排查用：能一眼看出为什么归到这一类）。
	Seed string
}

// Library 是种子向量库：第一次用时把种子句批量嵌入，之后只嵌入查询。
type Library struct {
	embed llm.Embedder
	seeds []Seed

	once sync.Once
	vecs [][]float32
	err  error
}

// NewLibrary 只做组装；向量在首次 Classify 时才建（不能挡在启动路径上）。
func NewLibrary(embed llm.Embedder, seeds []Seed) *Library {
	return &Library{embed: embed, seeds: seeds}
}

// Len 返回种子条数。
func (l *Library) Len() int {
	if l == nil {
		return 0
	}
	return len(l.seeds)
}

// ErrNoSeeds 表示没有可用的种子（文件缺失或解析为空）。
var ErrNoSeeds = errors.New("情绪参考集为空")

// Classify 把一段语气描述归到一个标签。
func (l *Library) Classify(ctx context.Context, text string) (Match, error) {
	if l == nil || l.embed == nil || len(l.seeds) == 0 {
		return Match{}, ErrNoSeeds
	}
	if err := l.prepare(ctx); err != nil {
		return Match{}, err
	}
	vs, err := l.embed.Embed(ctx, []string{text})
	if err != nil {
		return Match{}, err
	}
	if len(vs) != 1 {
		return Match{}, errors.New("嵌入返回条数与输入不一致")
	}
	return l.match(vs[0]), nil
}

func (l *Library) prepare(ctx context.Context) error {
	l.once.Do(func() {
		texts := make([]string, len(l.seeds))
		for i, s := range l.seeds {
			texts[i] = s.Text
		}
		l.vecs, l.err = l.embed.Embed(ctx, texts)
		if l.err == nil && len(l.vecs) != len(l.seeds) {
			l.vecs, l.err = nil, errors.New("种子嵌入条数与种子数不一致")
		}
	})
	return l.err
}

// match 取每个标签里最接近的一条种子，再比这些"最佳代表"。
//
// 不用"每标签求质心"：标签分布极不均衡（调侃 42 条、难过 2 条），质心会被大标签
// 拖向它的平均语义，小众标签几乎永远赢不了。
// 也不用 top-K 求和：实测各标签的余弦挤在 0.6 上下，求和等于按"谁的邻居多"投票，
// 赢家与次高经常只差 0.00x——而报出来的分数却是另一条种子的，日志会自相矛盾。
func (l *Library) match(q []float32) Match {
	best := map[string]float64{} // 标签 → 该标签下最高余弦
	seed := map[string]string{}  // 标签 → 那条种子原文
	for i, v := range l.vecs {
		label := l.seeds[i].Label
		c := cosine(q, v)
		// 必须先判断"这个标签还没出现过"：零分标签不能因为 0 > 0 为假而被漏掉，
		// 否则次高永远是空的（真机上余弦不会是 0，但测试里会，而且漏掉次高会误导日志）。
		if cur, ok := best[label]; !ok || c > cur {
			best[label] = c
			seed[label] = l.seeds[i].Text
		}
	}

	type cand struct {
		label string
		cos   float64
	}
	cands := make([]cand, 0, len(best))
	for label, c := range best {
		cands = append(cands, cand{label, c})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].cos > cands[j].cos })
	if len(cands) == 0 {
		return Match{}
	}

	m := Match{Label: cands[0].label, Score: cands[0].cos, Seed: seed[cands[0].label]}
	if len(cands) > 1 {
		m.RunnerUp, m.RunnerUpScore = cands[1].label, cands[1].cos
	}
	if m.Score < MinScore || m.Score-m.RunnerUpScore < MinMargin {
		// 不给标签：调用方按默认语气处理。保留分数供日志排查。
		m.Label = ""
	}
	return m
}

// cosine 与 memory 里的口径一致（未归一化的原始向量也能比）。
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
