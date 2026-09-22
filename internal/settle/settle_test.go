package settle

import (
	"errors"
	"strings"
	"testing"

	"agent-for-you-love/internal/llm"
	"agent-for-you-love/internal/memory"
)

// source 是校验原话用的原文：三条消息，两句话可引用。
func source() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: "我上周末买了个 HHKB，静电容的"},
		{Role: llm.RoleAssistant, Content: "静电容的手感很特别吧？"},
		{Role: llm.RoleUser, Content: "敲起来那种顿挫感我挺喜欢的"},
	}
}

// 提示词里必须出现 "json" 字样：OpenAI / DeepSeek 的 json_object 模式是硬性要求，
// 不满足时服务端直接报错——而报错发生在运行时、还看不出跟提示词有关。
func TestPromptRequiresJSONWordAndRendersRoles(t *testing.T) {
	system, user := Prompt(source())

	if !strings.Contains(strings.ToLower(system+user), "json") {
		t.Error("提示词里必须出现 json 字样，否则 json_object 模式会直接报错")
	}
	for _, want := range []string{"用户：", "你：", "我上周末买了个 HHKB，静电容的"} {
		if !strings.Contains(user, want) {
			t.Errorf("提示词里应当包含 %q", want)
		}
	}
	// 转述过的内容不该出现在提示词里：模型只能从原文里挑，给它看的是原文
	if strings.Contains(user, "用户在聊键盘") {
		t.Error("提示词里出现了转述内容，它应当只有原文")
	}
}

const goodJSON = `{
  "title": "聊新买的键盘",
  "topic": "这段主要在聊新买的 HHKB 键盘",
  "key_points": [
    {"point": "用户买了 HHKB", "quote": "我上周末买了个 HHKB，静电容的"},
    {"point": "在意手感", "quote": "敲起来那种顿挫感我挺喜欢的"}
  ],
  "facts": [
    {"content": "用户有一把 HHKB 键盘", "about": "user", "kind": "fact", "importance": 3,
     "quote": "我上周末买了个 HHKB，静电容的"},
    {"content": "用户说过喜欢这种手感", "about": "persona", "kind": "preference", "importance": 4,
     "quote": "敲起来那种顿挫感我挺喜欢的"}
  ],
  "unresolved": ["空格键有点晃，还没决定是否换货"]
}`

func TestParseAcceptsGroundedOutput(t *testing.T) {
	res, err := Parse(goodJSON, source())
	if err != nil {
		t.Fatalf("正常输出不该报错: %v", err)
	}
	if res.Title != "聊新买的键盘" {
		t.Errorf("标题解析错了: %q", res.Title)
	}
	if len(res.KeyPoints) != 2 || len(res.Facts) != 2 || len(res.Unresolved) != 1 {
		t.Fatalf("条目数不对: 要点 %d / 事实 %d / 未了 %d",
			len(res.KeyPoints), len(res.Facts), len(res.Unresolved))
	}
	if res.Dropped != 0 {
		t.Errorf("没有该丢的条目，实际丢了 %d 条", res.Dropped)
	}

	// about 决定这条事实是公共还是私有——写错会让"另一个她知道不该知道的事"
	if res.Facts[0].Private {
		t.Error("about=user 应当是公共事实")
	}
	if !res.Facts[1].Private {
		t.Error("about=persona 应当是本人格私有经历")
	}

	sum := res.Summary()
	for _, want := range []string{"主题：", "用户买了 HHKB", "「我上周末买了个 HHKB，静电容的」", "还没说完"} {
		if !strings.Contains(sum, want) {
			t.Errorf("摘要里应当包含 %q，实际：\n%s", want, sum)
		}
	}
	// 事实不进摘要：它们会单独进 memories（可去重、可逐条删），
	// 同时写两处会让同一件事在注入时出现两遍
	if strings.Contains(sum, "用户有一把 HHKB 键盘") {
		t.Errorf("事实不该出现在摘要里，实际：\n%s", sum)
	}
}

// 抽不出来就丢掉：这是"抽取式"区别于"生成式"的唯一闸门。
func TestParseDropsEntriesWithoutGrounding(t *testing.T) {
	raw := `{
	  "topic": "聊键盘",
	  "key_points": [
	    {"point": "编的", "quote": "我昨天买了个 HHKB"},
	    {"point": "真的", "quote": "敲起来那种顿挫感我挺喜欢的"}
	  ],
	  "facts": [
	    {"content": "用户养了猫", "about": "user", "kind": "fact", "importance": 3,
	     "quote": "我家的猫叫团子"}
	  ]
	}`

	res, err := Parse(raw, source())
	if err != nil {
		t.Fatalf("还有可用条目，不该整体作废: %v", err)
	}
	if len(res.KeyPoints) != 1 {
		t.Errorf("原话对不上的要点应当被丢掉，实际留下 %d 条", len(res.KeyPoints))
	}
	if len(res.Facts) != 0 {
		t.Errorf("没有原文依据的事实应当一条都不留，实际 %d 条", len(res.Facts))
	}
	if res.Dropped != 2 {
		t.Errorf("丢弃数应当是 2（一条要点 + 一条事实），实际 %d", res.Dropped)
	}
}

// 模型爱给原话再包一层引号、或者把换行改动一下：
// 这类"形式上的不逐字"不该被判成编造——那会让摘要薄得没法用。
func TestParseToleratesWrappersAndWhitespace(t *testing.T) {
	raw := `{
	  "topic": "x",
	  "key_points": [{"point": "p", "quote": "「 我上周末买了个\nHHKB，静电容的 」"}]
	}`

	res, err := Parse(raw, source())
	if err != nil {
		t.Fatalf("剥掉外层引号后应当能匹配上: %v", err)
	}
	if len(res.KeyPoints) != 1 {
		t.Fatalf("应当留下 1 条，实际 %d 条（丢弃 %d）", len(res.KeyPoints), res.Dropped)
	}
	// 存下来的是**剥干净**的版本：引号是渲染摘要时自己加的，
	// 原话里再带一层就会在注入时看着很脏
	if got := res.KeyPoints[0].Quote; got != "我上周末买了个 HHKB，静电容的" {
		t.Errorf("存下来的原话应当是剥掉引号与空白的版本，实际 %q", got)
	}
}

// 一个字的"嗯"算不上依据。这一条与"对不上原文"是两条不同的规则，
// 分开测才能保证任一条没被写坏。
func TestParseDropsTooShortQuotes(t *testing.T) {
	// 故意不带 topic：唯一那条要点被刷掉之后，这次输出就什么也没剩下了
	raw := `{"key_points":[{"point":"原话太短","quote":"我"}]}`

	res, err := Parse(raw, source())
	if err == nil {
		t.Fatalf("超短原话应当被判为没有依据，实际留下了 %d 条", len(res.KeyPoints))
	}
	if !errors.Is(err, ErrUnusable) {
		t.Fatalf("应当返回 ErrUnusable，实际 %v", err)
	}
}

// 输出不可用的三种典型：不是 json、什么都没抽到、全被校验刷掉。
// 调用方靠 ErrUnusable 决定"本进程内不再重试"，所以它必须稳定地被返回。
func TestParseRejectsUnusableOutput(t *testing.T) {
	cases := map[string]string{
		"不是 json":   "我觉得这段聊得挺好的",
		"什么都没抽到":    `{"title":"只有标题"}`,
		"原话全对不上":    `{"key_points":[{"point":"p","quote":"一句原文里没有的话"}]}`,
		"json 是空对象": `{}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(raw, source()); !errors.Is(err, ErrUnusable) {
				t.Fatalf("应当返回 ErrUnusable，实际 %v", err)
			}
		})
	}
}

// about 缺失或写错时必须**按私有处理**：公共记忆对所有人生效，
// 猜错的代价是"另一个她知道了不该知道的事"，而这是不可逆的。
func TestParseDefaultsAboutToPrivate(t *testing.T) {
	raw := `{
	  "topic": "x",
	  "facts": [
	    {"content": "甲", "kind": "fact", "quote": "我上周末买了个 HHKB，静电容的"},
	    {"content": "乙", "about": "USER", "kind": "fact", "quote": "我上周末买了个 HHKB，静电容的"},
	    {"content": "丙", "about": "我们之间", "kind": "fact", "quote": "敲起来那种顿挫感我挺喜欢的"}
	  ]
	}`

	res, err := Parse(raw, source())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(res.Facts) != 3 {
		t.Fatalf("应当留下 3 条，实际 %d 条", len(res.Facts))
	}
	if !res.Facts[0].Private {
		t.Error("缺少 about 时应当按私有处理")
	}
	// 大小写不该改变含义：模型写 "User" 与写 "user" 是同一个意思
	if res.Facts[1].Private {
		t.Error("about 的大小写不该影响判断，USER 应当被认成公共")
	}
	if !res.Facts[2].Private {
		t.Error("about 值不认识时应当按私有处理")
	}
}

// 类型不认识就丢掉（它会参与"同类型才合并"的去重，猜错不如不存）；
// 重要性则是夹到区间里（它只是个排序权重，为它丢掉一条事实不值得）。
func TestParseDropsUnknownKindAndClampsImportance(t *testing.T) {
	raw := `{
	  "topic": "x",
	  "facts": [
	    {"content": "类型不认识", "kind": "vibe", "quote": "我上周末买了个 HHKB，静电容的"},
	    {"content": "重要性越界", "kind": "fact", "importance": 9, "quote": "我上周末买了个 HHKB，静电容的"},
	    {"content": "没给重要性", "kind": "fact", "quote": "敲起来那种顿挫感我挺喜欢的"}
	  ]
	}`

	res, err := Parse(raw, source())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(res.Facts) != 2 {
		t.Fatalf("类型不认识的应当被丢掉，实际留下 %d 条", len(res.Facts))
	}
	if res.Facts[0].Importance != memory.MaxImportance {
		t.Errorf("越界的重要性应当夹到 %d，实际 %d", memory.MaxImportance, res.Facts[0].Importance)
	}
	if res.Facts[1].Importance != memory.DefaultImportance {
		t.Errorf("缺省的重要性应当是 %d，实际 %d", memory.DefaultImportance, res.Facts[1].Importance)
	}
	if res.Dropped != 1 {
		t.Errorf("丢弃数应当是 1，实际 %d", res.Dropped)
	}
}

// 条数上限：模型偶尔会一次给几十条，那会让摘要失去"浓缩"的意义、也吃光注入预算。
func TestParseCapsItemCounts(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"topic":"x","key_points":[`)
	for i := 0; i < MaxKeyPoints+3; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"point":"p","quote":"敲起来那种顿挫感我挺喜欢的"}`)
	}
	b.WriteString(`]}`)

	res, err := Parse(b.String(), source())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(res.KeyPoints) != MaxKeyPoints {
		t.Errorf("应当截到 %d 条，实际 %d 条", MaxKeyPoints, len(res.KeyPoints))
	}
	if res.Dropped != 3 {
		t.Errorf("超出的 3 条应当计入丢弃，实际 %d", res.Dropped)
	}
}
