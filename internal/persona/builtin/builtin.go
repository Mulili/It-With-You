package builtin

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"agent-for-you-love/internal/persona"
)

// Entry 是一个内置人格及其规则。
// 内置人格不落库（随 exe 分发、只读），所以规则跟着主体一起返回。
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

		// ID 由文件名推得，保证跨重启稳定：app_settings 里存的就是它，
		// 若用随机 uuid，每次启动都会认为"当前人格不存在了"。
		id := "builtin:" + strings.TrimSuffix(path.Base(name), ".json")

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
