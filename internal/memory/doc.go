// Package memory 预留：长期记忆 / RAG（阶段4 落地）。
//
// 技术选型已定：PostgreSQL + pgvector，memories 表存 content + embedding，
// HNSW 索引 + 余弦距离做检索，对话结束后由 LLM 抽取「值得记住的事实」。
//
// ⚠️ 动手前必须先锁定嵌入模型，因为向量维度会写进表结构：
//   - OpenAI text-embedding-3-small / ada-002 → 1536 维
//   - BGE-M3                                  → 1024 维
//
// 建表时不要把维度写死，用配置项拼接（详见 README 阶段4「关键决策」）。
//
// 计划中的形态（阶段4 实现时再落成真实代码）：
//
//	type Memory struct {
//		ID        string
//		UserID    string
//		Content   string
//		Embedding []float32
//		Category  string // fact / preference / event
//		CreatedAt time.Time
//	}
//
//	type Store interface {
//		// Save 写入一条记忆（内部负责生成 embedding）。
//		Save(ctx context.Context, m Memory) error
//		// Search 按语义检索 topK 条相关记忆。
//		Search(ctx context.Context, query string, topK int) ([]Memory, error)
//	}
package memory
