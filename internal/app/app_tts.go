package app

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-for-you-love/internal/tts"
	"agent-for-you-love/internal/tts/text"
	"agent-for-you-love/internal/ui"
)

// speech 把流式回复变成一句句语音：攒到够长 → 分句 → 串行合成 → 推给前端播放。
//
// 三个约束决定了它的形状：
//   - 合成很慢（一句 2~3 秒），**不能阻塞流式输出**，所以是独立 worker + 队列；
//   - ONNX session 不能并发，所以**单 worker 串行**（Engine 自己也有一把锁）；
//   - 用户发了新问题 / 点了停止，旧语音必须立刻闭嘴，所以每次换轮次就换一个 generation，
//     队列里过期的任务直接丢掉。
type speech struct {
	app *App

	mu     sync.Mutex
	eng    *tts.Engine
	engErr error
	tried  bool

	splitter *text.Splitter
	buf      strings.Builder
	curID    string
	gen      int
	seq      int

	start       time.Time
	firstLogged bool

	jobs chan speechJob
}

type speechJob struct {
	id   string
	text string
	gen  int
	seq  int
}

func newSpeech(app *App) *speech {
	s := &speech{
		app: app,
		// 分句规则按用户的约定定死：**每个标点就是一次推理任务**，
		// 单次任务最多 80 个汉字（effectiveLen 里 CJK 记 2，所以是 160）。
		// 任务越短，模型越不容易"说到一半就结束"，也越早能出声。
		splitter: &text.Splitter{MaxLen: 160, MinLen: 1},
		jobs:     make(chan speechJob, 32),
	}
	go s.run()
	return s
}

// Feed 接收一段流式正文。
func (s *speech) Feed(id, delta string) {
	if s == nil || delta == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if id != s.curID {
		s.curID = id
		s.buf.Reset()
		s.gen++
		s.seq = 0
		s.start = time.Now()
		s.firstLogged = false
	}
	s.buf.WriteString(delta)
	s.flushLocked(false)
}

// Done 表示本轮正文结束：把最后那句没等到标点的尾巴也发出去。
func (s *speech) Done(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.curID {
		return
	}
	s.flushLocked(true)
}

// Stop 立刻打断：清空待合成队列，并让前端停下正在播的。
func (s *speech) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.gen++
	s.buf.Reset()
	s.curID = ""
	s.mu.Unlock()

	if s.app != nil {
		s.app.emit(ui.EventTTSStop, nil)
	}
}

func (s *speech) flushLocked(final bool) {
	raw := s.buf.String()
	if strings.TrimSpace(raw) == "" {
		s.buf.Reset()
		return
	}
	parts := s.splitter.Split(raw)
	if len(parts) == 0 {
		s.buf.Reset()
		return
	}
	if final {
		for _, p := range parts {
			s.enqueueLocked(p)
		}
		s.buf.Reset()
		return
	}
	for _, p := range parts[:len(parts)-1] {
		s.enqueueLocked(p)
	}
	s.buf.Reset()
	s.buf.WriteString(parts[len(parts)-1])
}

func (s *speech) enqueueLocked(p string) {
	if strings.TrimSpace(p) == "" {
		return
	}
	s.seq++
	job := speechJob{id: s.curID, text: p, gen: s.gen, seq: s.seq}
	select {
	case s.jobs <- job:
	default:
		// 队列满说明合成已经跟不上生成：丢掉这一句，别把内存堆起来。
		// 用户听到的是"少了一句"，比整轮卡死强。
		log.Printf("tts: 队列已满，丢弃一句：%q", p)
	}
}

func (s *speech) run() {
	for job := range s.jobs {
		s.mu.Lock()
		stale := job.gen != s.gen
		s.mu.Unlock()
		if stale {
			continue
		}

		eng, err := s.engine()
		if err != nil {
			continue
		}

		t := time.Now()
		audio, err := eng.Speak(context.Background(), job.text, "")
		if err != nil {
			log.Printf("tts: 合成失败（%q）：%v", job.text, err)
			continue
		}
		// 偶发的"生成到一半就 EOS"会让这一句只有几十毫秒。用字数估个下限
		// （实测约 0.19s/字），明显偏短就重试一次并把证据打出来。
		want := float64(utf8.RuneCountInString(job.text)) * 0.08
		if d := float64(len(audio)) / float64(eng.Voice().SampleRate); len(audio) > 0 && d < want {
			log.Printf("tts: 该句偏短（%.2fs，%d 字）：%q，重试一次", d, utf8.RuneCountInString(job.text), job.text)
			if again, err2 := eng.Speak(context.Background(), job.text, ""); err2 == nil && len(again) > len(audio) {
				audio = again
			}
		}
		if len(audio) == 0 {
			continue
		}

		s.mu.Lock()
		stale = job.gen != s.gen
		first := !s.firstLogged
		if first {
			s.firstLogged = true
		}
		start := s.start
		s.mu.Unlock()
		if stale {
			continue
		}

		s.app.emit(ui.EventTTSAudio, ui.TTSAudioPayload{
			ID:   job.id,
			Seq:  job.seq,
			Rate: eng.Voice().SampleRate,
			PCM:  base64.StdEncoding.EncodeToString(pcm16(audio)),
			Text: job.text,
		})

		if first {
			log.Printf("tts: 首字延迟 %dms（首句合成 %dms，%q）",
				time.Since(start).Milliseconds(), time.Since(t).Milliseconds(), job.text)
		}
	}
}

// engine 懒加载语音引擎：加载要十几秒，不能放在启动路径上。
// 失败只记一次日志——语音停用、对话照常，与 probeEmbedder 同一种姿态。
func (s *speech) engine() (*tts.Engine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tried {
		return s.eng, s.engErr
	}
	s.tried = true

	opts := tts.Options{
		VoiceDir:    strings.TrimSpace(os.Getenv("COMPANION_TTS_VOICE_DIR")),
		BackboneDir: strings.TrimSpace(os.Getenv("COMPANION_TTS_BACKBONE_DIR")),
		RuntimePath: strings.TrimSpace(os.Getenv("COMPANION_TTS_RUNTIME")),
		// 4 线程：合成速度（RTF）直接决定句间会不会空等——2 线程时实测 RTF≈1.0，
		// 播放 2 秒就要等 2 秒合成，听感就是"问号后面停了两秒"。
		Threads: 4,
	}
	eng, err := tts.NewEngine(opts)
	if err != nil {
		s.engErr = err
		log.Printf("tts: 语音不可用（对话不受影响）：%v", err)
		return nil, err
	}
	s.eng = eng
	log.Printf("tts: 已加载音色 %s（%s）", eng.Voice().ID, eng.Voice().ModelVersion)

	// 参考侧预热放在后台：hubert + speaker + prompt_encoder 大约 1 秒，
	// 放在第一句话前面就是白等。
	go func() {
		t := time.Now()
		if err := eng.Prewarm(context.Background(), ""); err != nil {
			log.Printf("tts: 预热失败：%v", err)
			return
		}
		log.Printf("tts: 参考侧预热完成 %dms", time.Since(t).Milliseconds())
	}()
	return eng, nil
}

func pcm16(samples []float32) []byte {
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		f := v * 32767
		if f > 32767 {
			f = 32767
		} else if f < -32768 {
			f = -32768
		}
		u := uint16(int16(f))
		out[i*2] = byte(u)
		out[i*2+1] = byte(u >> 8)
	}
	return out
}
