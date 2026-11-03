// Package voice 预留：语音对话里"能听"的那条链（阶段5b）。
//
// 目标数据流：
//
//	麦克风 → VAD → ASR → LLM → TTS → 扬声器
//	                ↑
//	            用户说话时打断
//
// 本包只负责**输入侧**：采集、VAD、ASR、语音打断（需要 AEC 参考回流，
// 否则她会听见自己的声音当作用户）。
//
// 输出侧（TTS）已经独立在 internal/tts：**纯 Go 内嵌 ONNX**，不依赖云 API，
// 也不起本地常驻服务（取舍见 TTS原生go方案.md §0）。
//
// 计划中的形态（5b 实现时再落成真实代码）：
//
//	type Recognizer interface {
//		// Start 开始采集并返回识别出的文本流。
//		Start(ctx context.Context) (<-chan string, error)
//		Stop() error
//	}
package voice
