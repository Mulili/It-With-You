package store

import (
	"testing"

	"agent-for-you-love/internal/memory"
	"agent-for-you-love/internal/pkg/timeutil"
)

// 同相似度时，新往事必须排在旧往事前面。
func TestSearchChunksPrefersRecentOnTie(t *testing.T) {
	s := NewMemoryStore()
	now := timeutil.NowMillis()
	const day = int64(24 * 60 * 60 * 1000)
	const persona = "p1"
	vec := []float32{1, 0, 0}

	old := memory.ChunkIndex{
		ChunkID: "old", SessionID: "s1", PersonaID: persona,
		Summary: "很久以前的事", CreatedAt: now - 30*day,
	}
	fresh := memory.ChunkIndex{
		ChunkID: "fresh", SessionID: "s2", PersonaID: persona,
		Summary: "前天的事", CreatedAt: now - 1*day,
	}
	for _, c := range []memory.ChunkIndex{old, fresh} {
		if err := s.IndexChunk(c, vec); err != nil {
			t.Fatalf("写入片索引失败：%v", err)
		}
	}

	hits, err := s.SearchChunks(persona, vec, 2)
	if err != nil {
		t.Fatalf("检索失败：%v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("期望 2 条候选，实际 %d", len(hits))
	}
	if hits[0].Chunk.ChunkID != "fresh" {
		t.Errorf("新往事应排第一，实际 %q（score %.3f）", hits[0].Chunk.ChunkID, hits[0].Score)
	}
	// 原始相似度不能被时间加成改写：两条仍是 1.0
	for _, h := range hits {
		if h.Score < 0.999 {
			t.Errorf("%s 的 Score 被改写了：%.3f", h.Chunk.ChunkID, h.Score)
		}
	}
}
