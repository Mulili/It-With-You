// Package emotion 是阶段4.6 的情绪判定与状态推进。
//
// 分工（见 情感.md）：
//   - **判官（LLM）** 只负责"此刻是哪一种情绪"，读判定区；
//   - **状态机** 负责涨落、切换、冷却、并存；
//   - **注入区** 负责"这种情绪下她怎么想、怎么说"。
//
// 本文件只做第一件事的一半：把 情感.md 的判定区解析成结构化规则。
// 不解析注入区——那是给人看、给模型读的散文，不需要变成字段。
package emotion

import (
	"fmt"
	"regexp"
	"strings"
)

// Rule 是情感.md 里一条情绪的判定区。
//
// 字段名与标签一一对应；没写的可选栏就是空字符串。
type Rule struct {
	Key      string // 【编号】"0" / "1" / "2b" ...
	Name     string // 【名称】
	Object   string // 【对象】
	Speed    string // 【速度】
	Family   string // 【族】
	Trigger  string // 【触发】
	Exclude  string // 【排除】
	Boundary string // 【分界】
	Limit    string // 【限制】
	Slide    string // 【滑动】
	Cooldown string // 【冷却】
	NoJudge  string // 【不入判定】
}

// Rubric 是整份判定区。
type Rubric struct {
	Rules []Rule
	byKey map[string]Rule
}

// Lookup 按编号取一条规则。
func (r Rubric) Lookup(key string) (Rule, bool) {
	rule, ok := r.byKey[key]
	return rule, ok
}

// PerTurn 返回参与"每轮判定"的规则。
//
// 排除两类：
//   - 【不入判定】非空（孤独由状态触发，不由文本判）；
//   - 【速度】里写了"慢"（爱/依恋是慢变量，不参与每轮竞争）。
//
// 这个过滤必须和判官提示词里的说明一致，否则会出现"规则里没有 11、
// 但提示词又让它输出 11"这种自相矛盾。
func (r Rubric) PerTurn() []Rule {
	out := make([]Rule, 0, len(r.Rules))
	for _, rule := range r.Rules {
		if rule.NoJudge != "" || strings.Contains(rule.Speed, "慢") {
			continue
		}
		out = append(out, rule)
	}
	return out
}

var (
	// 只认 `## 6. 惊讶` / `## 2b. 共情（…）` 这种编号标题。
	// 非编号标题（`## 难过` 之类）一律忽略。
	sectionRe = regexp.MustCompile(`^##\s+([0-9]+[a-z]?)\.\s+(.+?)\s*$`)
	tagRe     = regexp.MustCompile(`^【([^】]+)】(.*)$`)
)

// requiredTags 是判定区必须写全的标签。
// 少一个就报错，而不是静默跳过——静默跳过的结果是判官少看到一条规则，
// 表现只是"她最近有点平"，这种问题极难查。
var requiredTags = []string{"编号", "名称", "对象", "速度", "族", "触发", "排除"}

// ParseRubric 解析 情感.md，抽出所有 `### 判定` 块。
//
// 语法：
//   - `## <编号>. <名称>` 开始一条情绪；
//   - `### 判定` 进入判定区，`### 注入` / `---` / 下一个 `##` 结束；
//   - 判定区内每行 `【标签】内容`；
//   - 不以 `【` 开头的非空行视为上一栏的续行（长句换行时用）。
func ParseRubric(md string) (Rubric, error) {
	var (
		rules   []Rule
		cur     *Rule
		inJudge bool
		lastTag string
	)

	flush := func() error {
		if cur == nil {
			return nil
		}
		if err := validate(*cur); err != nil {
			return err
		}
		rules = append(rules, *cur)
		cur = nil
		return nil
	}

	for i, ln := range strings.Split(md, "\n") {
		lineNo := i + 1
		s := strings.TrimSpace(ln)

		if m := sectionRe.FindStringSubmatch(s); m != nil {
			// 上一条如果开了头却没有判定区，直接报错——静默跳过会让判官少一条规则。
			if cur != nil {
				if !inJudge {
					return Rubric{}, fmt.Errorf("L%d: %s %s 缺少 `### 判定`", lineNo, cur.Key, cur.Name)
				}
				if err := flush(); err != nil {
					return Rubric{}, err
				}
			}
			cur = &Rule{Key: m[1], Name: m[2]}
			inJudge, lastTag = false, ""
			continue
		}
		// 其他标题（# 关系基线 / # 附录 / 主动发言里的 ## 难过）一律结束当前块
		if strings.HasPrefix(s, "# ") || strings.HasPrefix(s, "## ") {
			if cur != nil {
				if !inJudge {
					return Rubric{}, fmt.Errorf("L%d: %s %s 缺少 `### 判定`", lineNo, cur.Key, cur.Name)
				}
				if err := flush(); err != nil {
					return Rubric{}, err
				}
			}
			inJudge, lastTag = false, ""
			continue
		}
		if s == "### 判定" {
			if cur == nil {
				return Rubric{}, fmt.Errorf("L%d: `### 判定` 出现在任何情绪标题之前", lineNo)
			}
			inJudge, lastTag = true, ""
			continue
		}
		if strings.HasPrefix(s, "### ") || s == "---" {
			// 只有判定区里的 `###` / `---` 才算收尾；
			// 前面的 `### 注入` 不能 flush，否则会拿一个只有标题的空规则去校验。
			if inJudge {
				if err := flush(); err != nil {
					return Rubric{}, err
				}
				inJudge = false
			}
			lastTag = ""
			continue
		}
		if !inJudge || cur == nil || s == "" {
			continue
		}

		if m := tagRe.FindStringSubmatch(s); m != nil {
			tag, val := m[1], strings.TrimSpace(m[2])
			if !setTag(cur, tag, val) {
				return Rubric{}, fmt.Errorf("L%d: 未知的判定标签【%s】", lineNo, tag)
			}
			lastTag = tag
			continue
		}
		// 续行
		if lastTag != "" {
			appendTag(cur, lastTag, s)
		}
	}

	if err := flush(); err != nil {
		return Rubric{}, err
	}
	if len(rules) == 0 {
		return Rubric{}, fmt.Errorf("没有解析到任何 `### 判定` 块")
	}

	byKey := make(map[string]Rule, len(rules))
	for _, rule := range rules {
		if _, dup := byKey[rule.Key]; dup {
			return Rubric{}, fmt.Errorf("编号 %s 出现了两次", rule.Key)
		}
		byKey[rule.Key] = rule
	}
	return Rubric{Rules: rules, byKey: byKey}, nil
}

func setTag(rule *Rule, tag, val string) bool {
	switch tag {
	case "编号":
		// 标题里已经有；以标签为准，便于单独维护
		if val != "" {
			rule.Key = val
		}
	case "名称":
		if val != "" {
			rule.Name = val
		}
	case "对象":
		rule.Object = val
	case "速度":
		rule.Speed = val
	case "族":
		rule.Family = val
	case "触发":
		rule.Trigger = val
	case "排除":
		rule.Exclude = val
	case "分界":
		rule.Boundary = val
	case "限制":
		rule.Limit = val
	case "滑动":
		rule.Slide = val
	case "冷却":
		rule.Cooldown = val
	case "不入判定":
		rule.NoJudge = val
	default:
		return false
	}
	return true
}

func appendTag(rule *Rule, tag, line string) {
	switch tag {
	case "对象":
		rule.Object = joinLine(rule.Object, line)
	case "速度":
		rule.Speed = joinLine(rule.Speed, line)
	case "族":
		rule.Family = joinLine(rule.Family, line)
	case "触发":
		rule.Trigger = joinLine(rule.Trigger, line)
	case "排除":
		rule.Exclude = joinLine(rule.Exclude, line)
	case "分界":
		rule.Boundary = joinLine(rule.Boundary, line)
	case "限制":
		rule.Limit = joinLine(rule.Limit, line)
	case "滑动":
		rule.Slide = joinLine(rule.Slide, line)
	case "冷却":
		rule.Cooldown = joinLine(rule.Cooldown, line)
	case "不入判定":
		rule.NoJudge = joinLine(rule.NoJudge, line)
	}
}

func joinLine(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

func validate(rule Rule) error {
	for _, tag := range requiredTags {
		var val string
		switch tag {
		case "编号":
			val = rule.Key
		case "名称":
			val = rule.Name
		case "对象":
			val = rule.Object
		case "速度":
			val = rule.Speed
		case "族":
			val = rule.Family
		case "触发":
			val = rule.Trigger
		case "排除":
			val = rule.Exclude
		}
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("%s %s 缺少【%s】", rule.Key, rule.Name, tag)
		}
	}
	return nil
}
