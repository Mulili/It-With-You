package store

import (
	"context"
	"fmt"

	"agent-for-you-love/internal/memory"
)

// 重建索引（换嵌入模型之后把三张表的向量重算一遍）。
//
// 为什么它值得在存储层做成一个方法、而不是让上层自己遍历：**源文本只有这里知道**。
// 三张表各自存着自己被嵌入的那份文本（content / summary / content），
// 而上层拿到的是结构体——让上层拼"该嵌哪句话"就等于把那三个约定复制一份到 app 层，
// 将来改了一处（比如摘要的渲染方式）就会两边不一致，而症状是"命中分数整体偏低"，
// 谁也看不出是这里的问题。

// reembedBatchSize 是一次交给嵌入函数的条数。
//
// 32 是折中：够大到摊平调用开销，又小到"失败时已经改了一半"最多只影响 32 行
// （重嵌是幂等的，再跑一次就补上了）。
const reembedBatchSize = 32

// reembedOne 是两种实现共用的批处理骨架：分批 → 嵌 → 逐条写回。
//
// 为什么抽出来：批大小、"条数必须一一对应"、"失败即停"这三条是**业务规则**，
// 各写一遍迟早会漂移——而漂移的症状是"某一侧偶尔少嵌几条"，那种错谁也看不出来。
func reembedOne(texts []string,
	apply func(i int, vec []float32) error,
	embed func([]string) ([][]float32, error),
) error {
	for start := 0; start < len(texts); start += reembedBatchSize {
		end := min(start+reembedBatchSize, len(texts))
		batch := texts[start:end]
		vecs, err := embed(batch)
		if err != nil {
			return fmt.Errorf("重嵌第 %d~%d 条失败: %w", start, end-1, err)
		}
		// 条数对不上就是静默丢文本（与 llm.validateVectors 同一条理由）：
		// 照收会让"第 i 条"和"第 i+1 条"共用一个向量，而检索只会表现为"偶尔串味"。
		if len(vecs) != len(batch) {
			return fmt.Errorf("嵌入返回 %d 条向量，请求了 %d 条（必须一一对应）", len(vecs), len(batch))
		}
		for i := range vecs {
			if err := apply(start+i, vecs[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReembedAll 实现 memory.Store。
func (s *MemoryStore) ReembedAll(embed func([]string) ([][]float32, error)) (memory.ReembedResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res memory.ReembedResult

	// 记忆
	memTexts := make([]string, len(s.entries))
	for i := range s.entries {
		memTexts[i] = s.entries[i].m.Content
	}
	if err := reembedOne(memTexts, func(i int, vec []float32) error {
		s.entries[i].vec = vec
		return nil
	}, embed); err != nil {
		return res, err
	}
	res.Memories = len(memTexts)

	// 片索引：map 的遍历顺序不确定，但每一行只写回它自己的键，所以不影响正确性
	chunkKeys := make([]string, 0, len(s.chunkIndex))
	chunkTexts := make([]string, 0, len(s.chunkIndex))
	for k, e := range s.chunkIndex {
		chunkKeys = append(chunkKeys, k)
		chunkTexts = append(chunkTexts, e.ci.Summary)
	}
	if err := reembedOne(chunkTexts, func(i int, vec []float32) error {
		e := s.chunkIndex[chunkKeys[i]]
		e.vec = vec
		s.chunkIndex[chunkKeys[i]] = e
		return nil
	}, embed); err != nil {
		return res, err
	}
	res.Chunks = len(chunkTexts)

	// 归档规则索引
	ruleKeys := make([]string, 0, len(s.ruleIndex))
	ruleTexts := make([]string, 0, len(s.ruleIndex))
	for k, e := range s.ruleIndex {
		ruleKeys = append(ruleKeys, k)
		ruleTexts = append(ruleTexts, e.ri.Content)
	}
	if err := reembedOne(ruleTexts, func(i int, vec []float32) error {
		e := s.ruleIndex[ruleKeys[i]]
		e.vec = vec
		s.ruleIndex[ruleKeys[i]] = e
		return nil
	}, embed); err != nil {
		return res, err
	}
	res.Rules = len(ruleTexts)

	return res, nil
}

// reembedTables 列出三张表的取数与更新语句。
//
// 用一张表驱动，而不是三段几乎一样的代码：三张表只有"主键列名 / 文本列名"不同，
// 而这三处正是最容易抄错的地方（抄错了会更新到不存在的行，**不报错**，只是没改到）。
var reembedTables = []struct {
	name  string
	fetch string
	upd   string
}{
	{"memories", `SELECT id, content FROM memories`, `UPDATE memories SET embedding = $2::vector WHERE id = $1`},
	{"chunk_index", `SELECT chunk_id, summary FROM chunk_index`, `UPDATE chunk_index SET embedding = $2::vector WHERE chunk_id = $1`},
	{"rule_index", `SELECT rule_id, content FROM rule_index`, `UPDATE rule_index SET embedding = $2::vector WHERE rule_id = $1`},
}

// ReembedAll 实现 memory.Store。
func (s *PgStore) ReembedAll(embed func([]string) ([][]float32, error)) (memory.ReembedResult, error) {
	ctx, cancel := s.ctx()
	defer cancel()

	var res memory.ReembedResult
	for i, tb := range reembedTables {
		n, err := s.reembedTable(ctx, tb.fetch, tb.upd, embed)
		if err != nil {
			return res, fmt.Errorf("重建 %s 失败: %w", tb.name, err)
		}
		// 顺序必须与 reembedTables 一致：这里是"第几张表 → 结果里哪个计数"
		switch i {
		case 0:
			res.Memories = n
		case 1:
			res.Chunks = n
		case 2:
			res.Rules = n
		}
	}
	return res, nil
}

// reembedTable 重算一张表里所有行的向量，返回改了多少行。
func (s *PgStore) reembedTable(ctx context.Context, fetch, upd string,
	embed func([]string) ([][]float32, error),
) (int, error) {
	rows, err := s.pool.Query(ctx, fetch)
	if err != nil {
		return 0, fmt.Errorf("读取待重嵌的行失败: %w", err)
	}
	var keys, texts []string
	for rows.Next() {
		var k, t string
		if err := rows.Scan(&k, &t); err != nil {
			rows.Close()
			return 0, fmt.Errorf("扫描待重嵌的行失败: %w", err)
		}
		keys = append(keys, k)
		texts = append(texts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("遍历待重嵌的行失败: %w", err)
	}
	if len(keys) == 0 {
		return 0, nil
	}

	// 逐行 UPDATE，不攒一条大 SQL：不同行要写**不同的向量**，攒批就得自己拼参数下标，
	// 而那种 SQL 一旦拼错，症状是"某些行没更新到"——静默，且极难定位。
	err = reembedOne(texts, func(i int, vec []float32) error {
		if _, uerr := s.pool.Exec(ctx, upd, keys[i], vectorLiteral(vec)); uerr != nil {
			return fmt.Errorf("更新第 %d 行（%s）失败: %w", i, keys[i], uerr)
		}
		return nil
	}, embed)
	if err != nil {
		return 0, err
	}
	return len(keys), nil
}
