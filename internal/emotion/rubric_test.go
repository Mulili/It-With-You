package emotion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 真实文件的路径：测试的 cwd 是包目录。
const realRubricPath = "../../情感.md"

func loadRealRubric(t *testing.T) Rubric {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "情感.md"))
	if err != nil {
		t.Fatalf("读不到 %s: %v", realRubricPath, err)
	}
	r, err := ParseRubric(string(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return r
}

func TestParseRealRubric(t *testing.T) {
	r := loadRealRubric(t)

	wantKeys := []string{"0", "1", "2", "2b", "3", "4", "4b", "5", "6", "7", "8", "9", "10", "11", "12"}
	if len(r.Rules) != len(wantKeys) {
		t.Fatalf("解析到 %d 条，期望 %d 条", len(r.Rules), len(wantKeys))
	}
	for i, key := range wantKeys {
		if r.Rules[i].Key != key {
			t.Errorf("第 %d 条编号是 %q，期望 %q", i+1, r.Rules[i].Key, key)
		}
	}

	// 必填栏在 ParseRubric 里已经校验过；这里只查可选栏有没有被正确吃到。
	if got := r.Rules[13].NoJudge; got == "" {
		t.Error("11 孤独应当有【不入判定】")
	}
	if got := r.Rules[14].Speed; !strings.Contains(got, "慢") {
		t.Errorf("12 爱/依恋的速度应当含“慢”，实际 %q", got)
	}
	if got := r.Rules[3].Boundary; got == "" {
		t.Error("2b 共情应当有【分界】")
	}

	// 每轮判定 = 15 - 孤独 - 爱/依恋 = 13
	if got := len(r.PerTurn()); got != 13 {
		t.Errorf("PerTurn 返回 %d 条，期望 13 条", got)
	}
	for _, rule := range r.PerTurn() {
		if rule.Key == "11" || rule.Key == "12" {
			t.Errorf("%s 不该参与每轮判定", rule.Key)
		}
	}
}

func TestParseRubricRejectsMissingField(t *testing.T) {
	md := `## 1. 喜悦

### 判定

【编号】1
【名称】喜悦
【对象】情境
【速度】快
【族】正向
【触发】好消息
`
	_, err := ParseRubric(md)
	if err == nil {
		t.Fatal("缺少【排除】应当报错")
	}
	if !strings.Contains(err.Error(), "排除") {
		t.Errorf("报错里应当指出缺哪一栏，实际: %v", err)
	}
}

func TestParseRubricRejectsDuplicateKey(t *testing.T) {
	body := `【对象】自己
【速度】快
【族】正向
【触发】某事
【排除】某事
`
	md := "## 1. 甲\n\n### 判定\n\n【编号】1\n【名称】甲\n" + body +
		"\n---\n\n## 1. 乙\n\n### 判定\n\n【编号】1\n【名称】乙\n" + body
	_, err := ParseRubric(md)
	if err == nil || !strings.Contains(err.Error(), "两次") {
		t.Fatalf("重复编号应当报错，实际: %v", err)
	}
}

func TestParseRubricRejectsUnknownTag(t *testing.T) {
	md := `## 1. 喜悦

### 判定

【编号】1
【名称】喜悦
【对象】情境
【速度】快
【族】正向
【触发】好消息
【排除】坏消息
【心情】不该有这个标签
`
	_, err := ParseRubric(md)
	if err == nil || !strings.Contains(err.Error(), "未知") {
		t.Fatalf("未知标签应当报错，实际: %v", err)
	}
}

func TestParseRubricSupportsContinuationLines(t *testing.T) {
	md := `## 2. 难过

### 判定

【编号】2
【名称】难过
【对象】自己
【速度】快
【族】低落
【触发】失去
【排除】话题本身沉重
  而她并不低落
`
	r, err := ParseRubric(md)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := r.Rules[0].Exclude; !strings.Contains(got, "而她并不低落") {
		t.Errorf("续行没被拼进上一栏，实际 %q", got)
	}
}

func TestJudgeSystemPrompt(t *testing.T) {
	r := loadRealRubric(t)
	p := r.JudgeSystemPrompt()

	for _, want := range []string{"1 喜悦", "2b 共情", "4b 担心他", "10 嫉妒"} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词里缺少 %q", want)
		}
	}
	// 不参与每轮判定的两条不该出现在**规则表**里（说明文字里提到它们是对的）。
	for _, banned := range []string{"\n11 孤独\n", "\n12 爱 / 依恋\n"} {
		if strings.Contains(p, banned) {
			t.Errorf("规则表里不该有 %q", banned)
		}
	}
	if !strings.Contains(p, "不要输出 11 和 12") {
		t.Error("提示词应当明确禁止输出 11/12")
	}
	if !strings.Contains(p, `{"label":"2b"`) {
		t.Error("提示词应当给出输出字段示例")
	}
}

func TestJudgeUserPrompt(t *testing.T) {
	prev := &Snapshot{Label: "2", Name: "难过", Intensity: 3, Object: "自己"}
	history := []Turn{
		{Role: "用户", Content: "今天好累"},
		{Role: "她", Content: "那就到我这来"},
	}
	got := JudgeUserPrompt(prev, history, "我没事，你别管我")

	// 顺序：上一轮 → 历史 → 最新
	idxPrev := strings.Index(got, "2 难过")
	idxHist := strings.Index(got, "今天好累")
	idxLatest := strings.Index(got, "我没事，你别管我")
	if idxPrev < 0 || idxHist < 0 || idxLatest < 0 {
		t.Fatalf("提示词缺内容:\n%s", got)
	}
	if !(idxPrev < idxHist && idxHist < idxLatest) {
		t.Errorf("顺序不对: prev=%d hist=%d latest=%d", idxPrev, idxHist, idxLatest)
	}

	if got := JudgeUserPrompt(nil, nil, "在吗"); !strings.Contains(got, "（还没有）") {
		t.Errorf("没有上一轮时应当写明，实际:\n%s", got)
	}
}
