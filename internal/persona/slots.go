package persona

import "strings"

// SlotOther 是"没归并成功"时的兜底槽位。
//
// 宁可落到这里，也不要把一个含义不明的说法硬塞进某个具体槽位：
// 槽位是"覆盖"的单位，错并会直接覆盖掉本来正确的值。
const SlotOther = "other"

// SlotSpec 描述一个规范槽位。
//
// ⚠️ 这个结构体会**原样序列化给前端**（经 Meta.Slots 到 GetPersonaMeta），
// json tag 必须写全 —— 漏了 tag 会被序列化成 Go 字段名（Key / Label / Multi），
// 而前端读的是小写，于是 UI 上表现为"槽位下拉是空白的"，且不报任何错。
type SlotSpec struct {
	Key string `json:"key"` // 英文 key，进数据库与导入导出 JSON
	// Label 是中文标签，只在 UI 显示；它同时也是一种别名——
	// 模型或用户很可能直接用标签称呼这个槽位，所以会一起进匹配索引。
	Label string `json:"label"`
	// Desc 是这个槽位"装什么"的一句话说明：抽取提示词据此告诉模型该怎么选，
	// UI 也用它做输入提示。改这里会同时影响两处，所以措辞要站在"给模型看"的角度写。
	Desc string `json:"desc"`
	// Aliases 是该槽位的近义说法。
	//
	// 放在这里而不是单开一张大表：别名、标签、kind、是否多值都是"同一个槽位的属性"，
	// 拆成两份数据迟早会出现"加了槽位忘了补别名"。放在一起后，
	// ⑤ 的抽取 prompt 清单、UI 的槽位下拉、导入校验的可用值提示都能直接从 SlotSpecs() 生成。
	Aliases []string `json:"aliases"`
	Kind    string   `json:"kind"` // stable / volatile
	// Multi 为 true 表示多值（追加 + 同值去重），false 表示单值（写入即覆盖）
	Multi bool `json:"multi"`
}

// slotSpecs 是规范槽位清单。阶段3 初版，可增，但有两条约束：
//
//   - Key 一旦发布就不要改名（数据库与已导出的文件里都有它），要改就新增一个；
//   - 别名**宁可少写**。错并会覆盖掉正确的值，漏并只是多出一条 other 规则、后续人工整理即可，
//     所以像"风格"（可能是语气，也可能是性格）这种歧义说法故意不收录。
//
// 另外别名只需写"与 Key、Label 都不同的说法"——Key 与 Label 会自动参与匹配。
var slotSpecs = []SlotSpec{
	{
		Key: "address_user", Label: "称呼用户", Kind: KindVolatile,
		Desc:    "用户希望被怎么称呼（如：叫主人 / 直接叫名字）",
		Aliases: []string{"称呼", "对我的称呼", "如何称呼我", "称呼方式", "用户称呼", "address"},
	},
	{
		Key: "address_self", Label: "自称", Kind: KindStable,
		Desc:    "它该怎么称呼自己（如：自称小助手 / 用自己的名字）",
		Aliases: []string{"角色自称", "自我称呼"},
	},
	{
		Key: "tone", Label: "语气", Kind: KindVolatile,
		Desc:    "说话的语气与风格（如：温和简洁 / 毒舌幽默）",
		Aliases: []string{"说话语气", "口吻", "说话风格"},
	},
	{
		Key: "verbosity", Label: "回答长度", Kind: KindVolatile,
		Desc:    "回答的长短详略（如：三句以内 / 可以展开讲）",
		Aliases: []string{"详细程度", "啰嗦程度", "简洁程度", "长度"},
	},
	{
		Key: "personality", Label: "性格倾向", Kind: KindStable,
		Desc:    "稳定的性格特征（如：活泼好奇 / 沉稳克制）",
		Aliases: []string{"性格", "人设", "个性"},
	},
	{
		Key: "taboo", Label: "禁忌", Kind: KindStable, Multi: true,
		Desc:    "不许做或不许提的事（如：不要长篇大论 / 不要说自己是 AI）",
		Aliases: []string{"雷区", "不要做的事", "禁止事项", "避讳"},
	},
	{
		Key: "catchphrase", Label: "口头禅", Kind: KindVolatile, Multi: true,
		Desc:    "常挂在嘴边的话（如：好耶 / 交给我吧）",
		Aliases: []string{"常用语", "常说的话", "口癖"},
	},
	{
		Key: SlotOther, Label: "未归并", Kind: KindVolatile, Multi: true,
		Desc:    "确实是长期要求，但归不进以上任何一类时用这个",
		Aliases: []string{"其他", "其它", "unknown"},
	},
}

// slotIndex 是"归一化后的说法 → 规范 key"的索引，包初始化时由 slotSpecs 构建。
//
// Key、Label、Aliases 三者都进索引，于是 "Address_User"、"address user"、"addressuser"
// 这类大小写与分隔符差异都不需要单独写别名。
var slotIndex = buildSlotIndex()

func buildSlotIndex() map[string]string {
	idx := make(map[string]string)
	add := func(alias, key string) {
		if n := normalizeSlot(alias); n != "" {
			idx[n] = key
		}
	}
	for _, s := range slotSpecs {
		add(s.Key, s.Key)
		add(s.Label, s.Key)
		for _, a := range s.Aliases {
			add(a, s.Key)
		}
	}
	return idx
}

// SlotSpecs 返回槽位清单的副本，避免调用方改到内部表。
// 别名切片也要复制——只复制结构体会让内外共享同一个底层数组。
func SlotSpecs() []SlotSpec {
	out := make([]SlotSpec, len(slotSpecs))
	for i, s := range slotSpecs {
		s.Aliases = append([]string(nil), s.Aliases...)
		out[i] = s
	}
	return out
}

// LookupSlot 按规范 key 查槽位。
func LookupSlot(key string) (SlotSpec, bool) {
	for _, s := range slotSpecs {
		if s.Key == key {
			return s, true
		}
	}
	return SlotSpec{}, false
}

// slotKeyList 返回"key, key, ..."形式的清单，用于报错提示与 ⑤ 的抽取 prompt。
func slotKeyList() string {
	keys := make([]string, 0, len(slotSpecs))
	for _, s := range slotSpecs {
		keys = append(keys, s.Key)
	}
	return strings.Join(keys, ", ")
}

// SlotLabel 返回槽位的中文标签，用于回执与 UI 显示；未知槽位原样返回。
func SlotLabel(key string) string {
	if spec, ok := LookupSlot(key); ok {
		return spec.Label
	}
	return key
}

// CanonicalizeSlot 把模型输出的槽位说法收敛到规范 key；认不出来就落 SlotOther。
//
// 这是"写入前收敛"的第二道闸门：第一道是抽取 prompt 里直接给出规范清单要求模型选，
// 这里负责挡住模型不听话、吐了变体的情况。
func CanonicalizeSlot(raw string) string {
	trimmed := strings.TrimSpace(raw)
	// 先按原样匹配规范 key（含下划线形式）
	if _, ok := LookupSlot(trimmed); ok {
		return trimmed
	}
	if key, ok := slotIndex[normalizeSlot(trimmed)]; ok {
		return key
	}
	return SlotOther
}

// normalizeSlot 归一化说法：去首尾空白、转小写、去掉空格与下划线、连字符。
// 于是 "Address_User"、"address user"、"addressuser" 会落到同一个 key 上。
func normalizeSlot(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	return s
}
