package builtin

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"agent-for-you-love/internal/persona"

	"github.com/google/uuid"
)

// Entry 是一个内置人格及其规则。
//
// 内置人格**只读**，内容（种子文本与规则）的权威来源始终是 exe 里的 json、不落库；
// 库里只留一行"锚点"给它占位（见 persona/store 的 ensureBuiltinRows）。
// 所以规则跟着主体一起返回，而不是从库里读。
type Entry struct {
	Persona persona.Persona
	Rules   []persona.PersonaRule
}

//go:embed *.json
var builtinFS embed.FS

// Load 读取并校验全部内置人格。
//
// 任何一个文件不合法都返回错误：内置人格随 exe 分发，坏数据必须在开发期被拦住，
// 而不是等用户点开设置页才发现"人格是空的"。TestBuiltinPersonasValid 常驻守着这条。
func Load() ([]Entry, error) {
	names, err := fs.Glob(builtinFS, "*.json")
	if err != nil {
		return nil, fmt.Errorf("枚举内置人格失败: %w", err)
	}
	sort.Strings(names)

	out := make([]Entry, 0, len(names))
	seen := make(map[string]string, len(names)) // 人格名 → 文件名
	for _, name := range names {
		data, err := builtinFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", name, err)
		}
		f, err := persona.ParsePersonaFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := f.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		// 内置文件里 origin 只能缺省或写 builtin；写成别的通常是从导出文件复制过来忘了改
		if f.Persona.Origin != "" && f.Persona.Origin != persona.OriginBuiltin {
			return nil, fmt.Errorf("%s: persona.origin = %q，内置人格只能是 %q 或省略", name, f.Persona.Origin, persona.OriginBuiltin)
		}
		if prev, dup := seen[f.Persona.Name]; dup {
			return nil, fmt.Errorf("%s: 人格名 %q 与 %s 重复，内置人格的名字必须唯一", name, f.Persona.Name, prev)
		}
		seen[f.Persona.Name] = name

		// id 必须由文件写死，理由见 validateBuiltinID
		if err := validateBuiltinID(f.Persona.ID, name); err != nil {
			return nil, err
		}
		id := f.Persona.ID

		out = append(out, Entry{
			Persona: persona.Persona{
				ID:         id,
				Name:       f.Persona.Name,
				SeedText:   f.Persona.SeedText,
				Origin:     persona.OriginBuiltin,
				IsBuiltin:  true,
				AvatarPath: f.Persona.AvatarPath,
			},
			Rules: f.RulesFor(id),
		})
	}
	return out, nil
}

// validateBuiltinID 校验内置人格的 id：非空且是合法 uuid。
//
// 为什么不能由文件名推出来（以前的做法是 "builtin:" + 文件名）：那个 id 现在要在
// personas 表里当外键锚点，所以必须是 uuid；更重要的是它必须**永久稳定**——
// 改动它会让已经存在的历史与记忆变成孤儿（指向一个不存在的人格），
// 而这件事从文件上看完全看不出来。所以把话说在这里，宁可启动就报错。
func validateBuiltinID(id, fileName string) error {
	if id == "" {
		return fmt.Errorf("%s: 缺少 persona.id。内置人格必须写一个固定 uuid —— "+
			"它在 personas 表里是历史与记忆的外键锚点，改动会让已有数据变成孤儿", fileName)
	}
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("%s: persona.id = %q 不是合法 uuid：%v", fileName, id, err)
	}
	return nil
}
