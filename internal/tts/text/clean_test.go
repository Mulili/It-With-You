package text

import (
	"strings"
	"testing"
)

// 朗读前清洗的核心要求只有两条：读出来的不是排版符号，且**边界不能丢**。
// 边界丢了就等于把段落、列表连成一整句——那正是"整段连读"的来源。
func TestCleanForSpeech(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // 必须依次出现
		drop []string // 必须不出现
	}{
		{
			name: "粗体与行内代码",
			in:   "这是**很重要**的一句话，代码是 `npm run dev`。",
			want: []string{"这是很重要的一句话", "npm run dev"},
			drop: []string{"**", "`"},
		},
		{
			name: "标题与列表",
			in:   "## 标题\n- 第一项\n- 第二项\n",
			want: []string{"标题", "第一项", "第二项"},
			drop: []string{"##", "- "},
		},
		{
			name: "有序列表与任务列表",
			in:   "1. 先起床\n2. 再刷牙\n- [x] 已经做完",
			want: []string{"先起床", "再刷牙", "已经做完"},
			drop: []string{"1. ", "[x]"},
		},
		{
			name: "链接留文字去地址",
			in:   "可以看[官方文档](https://example.com/a/b)里的说明。",
			want: []string{"官方文档", "里的说明"},
			drop: []string{"http", "example.com"},
		},
		{
			name: "图片整条丢掉",
			in:   "看一下这个！![示意图](https://example.com/x.png)好看吗？",
			want: []string{"看一下这个", "好看吗"},
			drop: []string{"示意图", "png"},
		},
		{
			name: "代码块整段丢掉",
			in:   "大概是这样的：\n```go\nfunc main() {}\n```\n然后就好了。",
			want: []string{"大概是这样的", "然后就好了"},
			drop: []string{"func main", "```"},
		},
		{
			name: "引用与水平线",
			in:   "> 她轻轻地说\n---\n好。",
			want: []string{"她轻轻地说", "好"},
			drop: []string{">", "---"},
		},
		{
			name: "斜体与删除线",
			in:   "这是*轻轻*说的，~~别当真~~。",
			want: []string{"这是轻轻说的", "别当真"},
			drop: []string{"*", "~~"},
		},
	}
	for _, c := range cases {
		got := CleanForSpeech(c.in)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s：清洗后应当保留 %q，实际 %q", c.name, w, got)
			}
		}
		for _, d := range c.drop {
			if strings.Contains(got, d) {
				t.Errorf("%s：清洗后不该出现 %q，实际 %q", c.name, d, got)
			}
		}
	}
}

// 换行必须变成停顿：单换行 → 逗号，空行 → 句号；且不能叠出连续的句末标点。
func TestCleanForSpeechNewlines(t *testing.T) {
	got := CleanForSpeech("第一句\n第二句\n\n第三段")
	if got != "第一句，第二句。第三段" {
		t.Errorf("换行改写不对：%q", got)
	}

	// 前面已经有标点时不再补，避免 "你好。。" 这种（它随后会被压掉，等于边界白做）
	if got := CleanForSpeech("你好。\n再见。"); got != "你好。再见。" {
		t.Errorf("已有标点后不该再补：%q", got)
	}
}

// 分句器要把破折号当成**必定断开**：它不是逗号（可以继续攒），也不是句末（后面还有话）。
func TestSplitterBreaksAtDash(t *testing.T) {
	parts := (&Splitter{MaxLen: 160, MinLen: 1}).Split("他看了很久——然后什么也没说。")
	if len(parts) != 2 {
		t.Fatalf("破折号应当切成两段，实际 %d 段：%q", len(parts), parts)
	}
	if !strings.HasSuffix(parts[0], "——") {
		t.Errorf("切点应当落在破折号之后，实际 %q", parts[0])
	}
	if PauseAfterMs(parts[0]) != PauseDash {
		t.Errorf("破折号那段的停顿应当是 %dms，实际 %d", PauseDash, PauseAfterMs(parts[0]))
	}
}
