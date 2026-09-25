// Package store 是 memory 的两种实现：内存（开发调试用）与 PostgreSQL（真源）。
package store

import (
	"math"
	"sort"
	"sync"

	"agent-for-you-love/internal/memory"

	"github.com/google/uuid"
)

// MemoryStore 把记忆放在进程内存里。
//
// 与 history 的内存实现同一姿态：只服务开发调试与快速验 UI，不追求查询能力。
type MemoryStore struct {
	mu sync.RWMutex
	// entries 把向量和记忆捆在一起存。分成两个平行切片的话，增删时任何一处漏改
	// 都会让它们错位——而错位后算出来的是"别人的相似度"，且完全不报错。
	entries []entry
	// chunkIndex 按 chunk_id 存片索引。用 map 而不是切片：PG 那边靠主键保证
	// "一片只有一条"，这边得自己找个等价物，而 map 的键天然就是它。
	chunkIndex map[string]indexEntry
}

type entry struct {
	m   memory.Memory
	vec []float32
}

type indexEntry struct {
	ci  memory.ChunkIndex
	vec []float32
}

// NewMemoryStore 建一个空的内存记忆存储。
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Save 实现 memory.Store。
func (s *MemoryStore) Save(m memory.Memory, vec []float32, threshold float64) (memory.SaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := memory.NowMillis()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.Importance == 0 {
		m.Importance = memory.DefaultImportance
	}

	// 去重：在"该人格可见的范围"里找最相似的一条。
	// Kind 必须相同——"喜欢猫"和"养过猫"相似度很高，但它们是两件事。
	best, bestSim := -1, -1.0
	for i := range s.entries {
		e := s.entries[i]
		if !visibleTo(e.m, m.PersonaID) || e.m.Kind != m.Kind {
			continue
		}
		if sim := cosine(vec, e.vec); sim > bestSim {
			best, bestSim = i, sim
		}
	}

	if best >= 0 && bestSim >= threshold {
		keep := s.entries[best].m
		keep.Content = m.Content
		keep.Evidence = m.Evidence
		// 重要性取大：这次说得更重，说明它比之前看更重要
		if m.Importance > keep.Importance {
			keep.Importance = m.Importance
		}
		// 只有新一轮带来了回访时间才覆盖（旧的还没问过就保留）
		if m.FollowUpAt != 0 {
			keep.FollowUpAt = m.FollowUpAt
		}
		keep.UpdatedAt = now
		s.entries[best] = entry{m: keep, vec: vec}
		return memory.SaveResult{ID: keep.ID, Updated: true}, nil
	}

	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	s.entries = append(s.entries, entry{m: m, vec: vec})
	return memory.SaveResult{ID: m.ID}, nil
}

// List 实现 memory.Store。
func (s *MemoryStore) List(personaID string, limit int) ([]memory.Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]memory.Memory, 0, len(s.entries))
	for _, e := range s.entries {
		if visibleTo(e.m, personaID) {
			out = append(out, e.m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// IndexChunk 实现 memory.Store。
func (s *MemoryStore) IndexChunk(ci memory.ChunkIndex, vec []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.chunkIndex == nil {
		s.chunkIndex = make(map[string]indexEntry)
	}
	// 覆盖时保留原有的 created_at，与 PG 的 ON CONFLICT DO UPDATE 保持一致
	// （两个实现在"重放会不会刷新索引时间"上分叉，会导致契约测试看到不同结果）
	if old, ok := s.chunkIndex[ci.ChunkID]; ok && ci.CreatedAt == 0 {
		ci.CreatedAt = old.ci.CreatedAt
	}
	if ci.CreatedAt == 0 {
		ci.CreatedAt = memory.NowMillis()
	}
	s.chunkIndex[ci.ChunkID] = indexEntry{ci: ci, vec: vec}
	return nil
}

// Search 实现 memory.Store。
func (s *MemoryStore) Search(personaID string, vec []float32, limit int) ([]memory.MemoryHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hits := make([]memory.MemoryHit, 0, len(s.entries))
	for _, e := range s.entries {
		if !visibleTo(e.m, personaID) {
			continue
		}
		// 相似度用与 PG 的 <=> 同一个口径（余弦），否则同一条断言在两种实现下会得出不同结论
		hits = append(hits, memory.MemoryHit{Memory: e.m, Score: cosine(vec, e.vec)})
	}
	sortHits(hits, func(h memory.MemoryHit) float64 { return h.Score })
	return trimHits(hits, limit), nil
}

// SearchChunks 实现 memory.Store。
func (s *MemoryStore) SearchChunks(personaID string, vec []float32, limit int) ([]memory.ChunkHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hits := make([]memory.ChunkHit, 0, len(s.chunkIndex))
	for _, e := range s.chunkIndex {
		// 片索引的 persona_id 不可空（片必然属于某个人格），所以这里是严格相等，
		// 与"记忆可公共"的语义不同
		if e.ci.PersonaID != personaID {
			continue
		}
		hits = append(hits, memory.ChunkHit{Chunk: e.ci, Score: cosine(vec, e.vec)})
	}
	sortHits(hits, func(h memory.ChunkHit) float64 { return h.Score })
	return trimHits(hits, limit), nil
}

// MarkRecalled 实现 memory.Store。
func (s *MemoryStore) MarkRecalled(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := memory.NowMillis()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for i := range s.entries {
		if want[s.entries[i].m.ID] {
			s.entries[i].m.LastRecalledAt = now
		}
	}
	return nil
}

// sortHits 按分数倒序排。
//
// 用 SliceStable 而不是 Slice：分数相同时至少保持"插入顺序"这一确定的次序。
// 注意这只让**内存实现自己**可复现，PG 侧同分行的顺序仍不保证——
// 所以两种实现之间不该依赖同分时的顺序（见 memory.Store 的接口注释）。
func sortHits[T any](hits []T, score func(T) float64) {
	sort.SliceStable(hits, func(i, j int) bool { return score(hits[i]) > score(hits[j]) })
}

// trimHits 截到 limit 条（limit <= 0 视为不限）。
func trimHits[T any](hits []T, limit int) []T {
	if limit > 0 && len(hits) > limit {
		return hits[:limit]
	}
	return hits
}

// Delete 实现 memory.Store。
func (s *MemoryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.entries {
		if s.entries[i].m.ID != id {
			continue
		}
		// 就地删掉、保持切片紧凑：entries 的顺序本来就没有语义
		// （检索按相似度排、List 按时间排，都是取出后再排），留空洞只会让每处遍历都要跳过 nil
		s.entries = append(s.entries[:i], s.entries[i+1:]...)
		return nil
	}
	return nil // 找不到就是已经删掉了，不是错误（见接口注释）
}

// DeletePersona 实现 memory.Store。
func (s *MemoryStore) DeletePersona(personaID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]entry, 0, len(s.entries))
	for _, e := range s.entries {
		// 公共记忆不删：人格没了，"用户养了猫"这类事实仍然成立
		if e.m.PersonaID != personaID {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return nil
}

// Close 实现 io.Closer（空操作，与另外两个内存实现保持一致）。
func (s *MemoryStore) Close() error { return nil }

// visibleTo 判断一条记忆对某个身份是否可见：公共的（无归属）加上它自己的。
func visibleTo(m memory.Memory, personaID string) bool {
	return m.PersonaID == "" || m.PersonaID == personaID
}

// cosine 算两个向量的余弦相似度。
//
// 内存实现自己算，PG 实现用 pgvector 的 <=> 算子——两边**口径必须一致**，
// 否则同一条断言在两种实现下会得出不同结论（契约测试就是为了钉住这一点）。
//
// 维度不一致或零向量时返回 -1（永远跨不过阈值）：这属于调用方传错了数据，
// 让它"匹配不上"比让它"匹配上一条不相干的"安全。
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
