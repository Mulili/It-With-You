package emotion

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeEmbedder 用关键词造一维正交向量：够验通路（谁跟谁最近），不验语义质量。
type fakeEmbedder struct {
	calls atomic.Int32
}

func (f *fakeEmbedder) Dim() int { return 3 }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 3)
		if strings.Contains(t, "嫌弃") || strings.Contains(t, "嘴硬") {
			v[0] = 1
		}
		if strings.Contains(t, "生气") || strings.Contains(t, "烦") {
			v[1] = 1
		}
		if strings.Contains(t, "开心") || strings.Contains(t, "高兴") {
			v[2] = 1
		}
		out[i] = v
	}
	return out, nil
}

func newTestLibrary(t *testing.T) (*Library, *fakeEmbedder) {
	t.Helper()
	f := &fakeEmbedder{}
	return NewLibrary(f, []Seed{
		{Text: "嘴硬怎么了，我这不是在跟你斗嘴玩吗", Label: "调侃"},
		{Text: "嘴硬归嘴硬，其实心里受用", Label: "调侃"},
		{Text: "我现在真的很生气，别再说了", Label: "生气"},
		{Text: "听到这个我挺高兴的", Label: "开心"},
	}), f
}

func TestClassifyByNearestSeeds(t *testing.T) {
	lib, f := newTestLibrary(t)
	m, err := lib.Classify(context.Background(), "表面嫌弃，实际心里挺受用")
	if err != nil {
		t.Fatalf("归类失败：%v", err)
	}
	if m.Label != "调侃" {
		t.Fatalf("应当归到调侃，实际 %+v", m)
	}
	if m.Score < MinScore {
		t.Fatalf("命中分数过低：%.2f", m.Score)
	}
	// 种子嵌入一次 + 查询一次，共两次批量调用
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("嵌入调用次数应当是 2（种子 + 查询），实际 %d", got)
	}
	// 第二次归类不该再嵌入种子
	if _, err := lib.Classify(context.Background(), "生气"); err != nil {
		t.Fatalf("第二次归类失败：%v", err)
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("种子向量应当缓存，实际调用 %d 次", got)
	}
}

func TestClassifyKeepsWinnerAndRunnerUp(t *testing.T) {
	lib, _ := newTestLibrary(t)
	m, err := lib.Classify(context.Background(), "语气里有点嫌弃")
	if err != nil {
		t.Fatalf("归类失败：%v", err)
	}
	// 赢家要给出次高，日志才能看出"有多犹豫"
	if m.Label != "调侃" || m.RunnerUp == "" || m.RunnerUp == m.Label {
		t.Fatalf("应当给出赢家与次高：%+v", m)
	}
}

// 两类都像时不硬判：差值不够就给空标签（用默认语气）。
func TestClassifyRejectsNearTie(t *testing.T) {
	lib, _ := newTestLibrary(t)
	m, err := lib.Classify(context.Background(), "有点生气，但更多是嫌弃")
	if err != nil {
		t.Fatalf("归类失败：%v", err)
	}
	if m.Label != "" {
		t.Fatalf("打平时不该给标签，实际 %+v", m)
	}
}

func TestClassifyUnrelatedGivesNoLabel(t *testing.T) {
	lib, _ := newTestLibrary(t)
	m, err := lib.Classify(context.Background(), "这是一段完全无关的陈述")
	if err != nil {
		t.Fatalf("归类失败：%v", err)
	}
	if m.Label != "" {
		t.Fatalf("低于阈值时不该给标签，实际 %+v", m)
	}
}

func TestClassifyWithoutSeeds(t *testing.T) {
	lib := NewLibrary(&fakeEmbedder{}, nil)
	if _, err := lib.Classify(context.Background(), "随便"); err != ErrNoSeeds {
		t.Fatalf("没有种子时应当返回 ErrNoSeeds，实际 %v", err)
	}
	var nilLib *Library
	if _, err := nilLib.Classify(context.Background(), "随便"); err != ErrNoSeeds {
		t.Fatalf("nil 库应当返回 ErrNoSeeds，实际 %v", err)
	}
	if nilLib.Len() != 0 {
		t.Fatalf("nil 库长度应当为 0")
	}
}
