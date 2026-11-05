package tts

import (
	"math"
	"testing"
)

func sine(freq float64, rate, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / float64(rate)))
	}
	return out
}

func rms(s []float32) float64 {
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func TestResampleLengthAndGain(t *testing.T) {
	const from, to = 48000, 16000
	in := sine(440, from, from) // 1 秒 440Hz
	out, err := Resample(in, from, to)
	if err != nil {
		t.Fatalf("重采样失败：%v", err)
	}
	if want := from / 3; len(out) != want {
		t.Errorf("长度 = %d，期望 %d", len(out), want)
	}
	// 1kHz 以下应当原样通过：正弦的 RMS 是 0.707
	if got := rms(out[1000 : len(out)-1000]); math.Abs(got-0.7071) > 0.02 {
		t.Errorf("通带 RMS = %.4f，期望 ≈0.7071", got)
	}
}

func TestResampleAntiAliases(t *testing.T) {
	// 48k 下的 20kHz，重采样到 16k 后必须被滤掉（否则会折回 4kHz 的哨音）
	out, err := Resample(sine(20000, 48000, 48000), 48000, 16000)
	if err != nil {
		t.Fatalf("重采样失败：%v", err)
	}
	if got := rms(out[2000 : len(out)-2000]); got > 0.05 {
		t.Errorf("混叠残留 RMS = %.4f，应当被压到 0.05 以下", got)
	}
}

func TestTrimSilence(t *testing.T) {
	const rate = 32000
	// 0.3s 静音 + 0.2s 正弦 + 0.5s 静音
	in := make([]float32, 0, rate)
	in = append(in, make([]float32, rate*3/10)...)
	in = append(in, sine(440, rate, rate/5)...)
	in = append(in, make([]float32, rate*5/10)...)

	out := TrimSilence(in, rate)
	got := float64(len(out)) / rate
	// 期望 ≈ 0.2s 主体 + 40ms 头 + 80ms 尾 = 0.32s，允许 ±0.1s
	if got < 0.22 || got > 0.42 {
		t.Errorf("裁剪后 %.3fs，期望 ≈0.32s", got)
	}
	if len(out) > len(in) {
		t.Error("裁剪不应变长")
	}
}

func TestResampleSameRateCopies(t *testing.T) {
	in := []float32{0.1, -0.2, 0.3}
	out, err := Resample(in, 16000, 16000)
	if err != nil {
		t.Fatalf("重采样失败：%v", err)
	}
	if len(out) != len(in) || out[1] != in[1] {
		t.Errorf("同采样率应当原样返回：%v", out)
	}
}
