package emotion

import (
	"os"
	"path/filepath"
	"strings"
)

// Seed 是一条人工标注过的语气样本（来自 情感参考集.md 的「参考集」表）。
//
// 它是**向量库的种子**，不进代码、不进提示词：文件是本地的、含私人对话，
// 已 gitignore。换一个人格就换一份种子，判定跟着一起换，代码不用改。
type Seed struct {
	Text  string
	Label string
}

// 参考集文件名（默认放在仓库根或可执行文件旁边）。
const SeedFileName = "情感参考集.md"

// SeedsFromFile 读取并解析参考集文件。
func SeedsFromFile(path string) ([]Seed, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSeeds(string(data))
}

// DefinitionsFromFile 读取参考集里「标签定义」那张表。
func DefinitionsFromFile(path string) ([]Seed, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseDefinitions(string(data))
}

// ParseSeeds 从「参考集」表格里取「当前句」与「人工标签」两列。
func ParseSeeds(md string) ([]Seed, error) {
	return parseRows(md, "当前句", "人工标签")
}

// ParseDefinitions 从「标签定义」表格里取「判定标准」与「标签」两列。
//
// 语气描述是**元描述**（"表面嫌弃、实际受用"），拿她的一句原话去比是跨层的；
// 而标签定义本身就是元描述，和查询同层。两条都留着，用哪条由调用方决定。
func ParseDefinitions(md string) ([]Seed, error) {
	return parseRows(md, "判定标准", "标签")
}

// parseRows 按表头定位列，取到表格结束为止。
//
// 按表头定位而不是写死序号：参考集是人手维护的，插一列「说话人」不该让解析错位
// （错位的后果非常隐蔽——每条句子的标签都串到别人身上，判定照样能跑，只是全错）。
// 列数一变就收尾：文件里有好几张表，不拦住会把下一张表的行当成这一张读进来。
func parseRows(md, textHeader, labelHeader string) ([]Seed, error) {
	textCol, labelCol, ncols := -1, -1, 0
	var out []Seed

	for _, ln := range strings.Split(md, "\n") {
		s := strings.TrimSpace(ln)
		if !strings.HasPrefix(s, "|") {
			continue
		}
		cells := splitRow(s)

		if textCol < 0 {
			for i, c := range cells {
				switch c {
				case textHeader:
					textCol = i
				case labelHeader:
					labelCol = i
				}
			}
			if textCol >= 0 && labelCol >= 0 {
				ncols = len(cells)
			}
			continue
		}
		if len(cells) != ncols {
			// 表格结束（下一张表的列数不同）：已经收过数据就别再往下捡。
			if len(out) > 0 {
				break
			}
			continue
		}
		if textCol >= len(cells) || labelCol >= len(cells) {
			continue
		}
		text, label := strings.TrimSpace(cells[textCol]), strings.TrimSpace(cells[labelCol])
		if text == "" || label == "" || isSeparator(text) || isSeparator(label) {
			continue
		}
		out = append(out, Seed{Text: text, Label: label})
	}
	return out, nil
}

// splitRow 去掉首尾的竖线后按列切分。
func splitRow(s string) []string {
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// isSeparator 判断是不是 markdown 表格的分隔单元格（--- / :---:）。
func isSeparator(s string) bool {
	s = strings.Trim(s, ":- ")
	return s == ""
}

// LocateSeeds 按优先级找参考集文件：环境变量 → 可执行文件目录 → 工作目录（并向上几级）。
//
// 向上找是因为开发期的可执行文件在 build/bin（或临时目录），而参考集在仓库根。
// 找不到返回空串——调用方据此**静默停用**情绪归类，对话与语音照常。
func LocateSeeds() string {
	if p := strings.TrimSpace(os.Getenv("COMPANION_EMOTION_SEEDS")); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	dirs := make([]string, 0, 8)
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		for i := 0; i < 4 && d != ""; i++ {
			p := filepath.Join(d, SeedFileName)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return ""
}
