// Command ttsprobe 是 5a 的验证程序：文本 → 文本前端 → 合成 → wav，并打印每句耗时。
//
// 它不走 UI，用来把"首字延迟"和"能不能读对"分开量。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"agent-for-you-love/internal/onnxrt"
	"agent-for-you-love/internal/tts"
	"agent-for-you-love/internal/tts/text"
)

func main() {
	voiceDir := flag.String("voice", "models/voices/feibi", "音色目录")
	backboneDir := flag.String("backbone", "models/shared", "共享骨干目录")
	dll := flag.String("dll", "build/bin/onnxruntime.dll", "onnxruntime 动态库")
	out := flag.String("out", "out.wav", "输出 wav")
	txt := flag.String("text", "你好，我是菲比。今天天气不错，我们一起出去走走吧。", "要合成的文本")
	threads := flag.Int("threads", 2, "算子内线程数")
	flag.Parse()

	t0 := time.Now()
	e, err := tts.NewEngine(tts.Options{
		VoiceDir:    *voiceDir,
		BackboneDir: *backboneDir,
		RuntimePath: *dll,
		Threads:     *threads,
	})
	if err != nil {
		fail("加载失败", err)
	}
	defer e.Close()
	fmt.Printf("加载完成 %dms · ort %s · 音色 %s（%s）\n",
		time.Since(t0).Milliseconds(), onnxrt.Version(), e.Voice().ID, e.Voice().ModelVersion)

	ctx := context.Background()
	tPrewarm := time.Now()
	if err := e.Prewarm(ctx, ""); err != nil {
		fail("预热失败", err)
	}
	fmt.Printf("参考侧预热 %dms（之后每句只算文本侧）\n", time.Since(tPrewarm).Milliseconds())

	// 与 app 侧一致：第一句用更激进的阈值（首字优先），其余用默认阈值。
	firstParts := (&text.Splitter{MaxLen: 16, MinLen: 3}).Split(*txt)
	sents := firstParts
	if len(firstParts) > 1 {
		rest := text.NewSplitter().Split(strings.Join(firstParts[1:], ""))
		sents = append([]string{firstParts[0]}, rest...)
	}
	if len(sents) == 0 {
		fail("参数错误", fmt.Errorf("-text 里没有可读内容"))
	}

	var all []float32
	var firstMs int64
	for i, s := range sents {
		t := time.Now()
		audio, err := e.Speak(ctx, s, "")
		if err != nil {
			fail("合成失败", fmt.Errorf("第 %d 句 %q：%w", i+1, s, err))
		}
		ms := time.Since(t).Milliseconds()
		if i == 0 {
			firstMs = ms
		}
		all = append(all, audio...)
		fmt.Printf("  #%d %4dms · %5.2fs 音频 · %q\n",
			i+1, ms, float64(len(audio))/float64(e.Voice().SampleRate), s)
	}

	if err := tts.WriteWAV(*out, all, e.Voice().SampleRate); err != nil {
		fail("写 wav 失败", err)
	}
	fmt.Printf("首字（首句）延迟 %dms · 总音频 %.2fs → %s\n",
		firstMs, float64(len(all))/float64(e.Voice().SampleRate), *out)
}

func fail(what string, err error) {
	fmt.Fprintf(os.Stderr, "%s：%v\n", what, err)
	os.Exit(1)
}
