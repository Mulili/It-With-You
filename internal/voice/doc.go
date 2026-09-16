// Package voice 预留：语音对话（阶段5 落地）。
//
// 目标数据流：
//
//	麦克风 → VAD → ASR → LLM → TTS → 扬声器
//	                ↑
//	            用户说话时打断
//
// 选型倾向（README 阶段5「关键决策」）：ASR/TTS 优先云 API 换取低延迟与低负载，
// 本地模型（Whisper.cpp / GPT-SoVITS）作为隐私选项后置。
//
// 计划中的形态（阶段5 实现时再落成真实代码）：
//
//	type Recognizer interface {
//		// Start 开始采集并返回识别出的文本流。
//		Start(ctx context.Context) (<-chan string, error)
//		Stop() error
//	}
//
//	type Synthesizer interface {
//		// Speak 合成并播放，ctx 取消即打断。
//		Speak(ctx context.Context, text string) error
//	}
package voice
