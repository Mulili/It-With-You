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
}

type entry struct {
	m   memory.Memory
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
