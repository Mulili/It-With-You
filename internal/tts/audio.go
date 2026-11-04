package tts

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// DecodeWAV 读 PCM 16-bit 的 WAV，混成单声道 float32（-1..1），并返回采样率。
//
// 只支持 PCM16 单/双声道。参考音频是我们自己准备的素材，不做通用解码器——
// 播放与重采样用什么库方案 §8 还没定，这里先不引入依赖。
func DecodeWAV(path string) (samples []float32, rate int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("%s 不是 WAV（缺 RIFF/WAVE 头）", path)
	}

	channels, bits := 0, 0
	var data []byte
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := b[off+8:]
		if size > len(body) {
			size = len(body)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, fmt.Errorf("%s 的 fmt 块过短", path)
			}
			if tag := binary.LittleEndian.Uint16(body[0:2]); tag != 1 {
				return nil, 0, fmt.Errorf("%s 不是 PCM（格式码 %d）", path, tag)
			}
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			rate = int(binary.LittleEndian.Uint32(body[4:8]))
			bits = int(binary.LittleEndian.Uint16(body[14:16]))
		case "data":
			data = body[:size]
		}
		off += 8 + size
		if size%2 == 1 {
			off++ // 块长度为奇数时有一字节填充
		}
	}
	if channels <= 0 || rate <= 0 {
		return nil, 0, fmt.Errorf("%s 缺少 fmt 信息", path)
	}
	if bits != 16 {
		return nil, 0, fmt.Errorf("%s 是 %d bit，只支持 16 bit", path, bits)
	}
	frames := len(data) / (2 * channels)
	if frames == 0 {
		return nil, 0, fmt.Errorf("%s 里没有采样数据", path)
	}

	out := make([]float32, frames)
	for i := 0; i < frames; i++ {
		var sum float32
		for c := 0; c < channels; c++ {
			v := int16(binary.LittleEndian.Uint16(data[(i*channels+c)*2:]))
			sum += float32(v) / 32768
		}
		out[i] = sum / float32(channels)
	}
	return out, rate, nil
}

// Resample 用「窗函数 sinc」做重采样（Blackman 窗 + 低通）。
//
// 为什么不用线性插值：降采样时它没有任何抗混叠，32k→16k 会把 8k 以上的能量折回
// 可听频段，喂给 hubert 的特征与 GENIE（soxr）差得很远——症状是音色不像、齿音发毛。
//
// 参数取的是常见折中：半宽 16 个输入样本（降采样时按 1/ratio 放大以保持过渡带），
// Blackman 窗压旁瓣（−58dB），最后按权重和归一化，边缘不会被衰减。
// 复杂度 O(输出样本 × 抽头)，参考音频 4 秒量级在几十毫秒内跑完。
func Resample(in []float32, from, to int) ([]float32, error) {
	if from <= 0 || to <= 0 {
		return nil, fmt.Errorf("采样率非法：%d → %d", from, to)
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("没有采样数据")
	}
	if from == to {
		return append([]float32(nil), in...), nil
	}

	ratio := float64(to) / float64(from)
	step := float64(from) / float64(to) // 每个输出样本在输入轴上跨多远

	// 归一化截止频率（cycles/input-sample）：降采样时必须压到新 Nyquist 以下。
	cutoff := 0.5 * math.Min(1, ratio)
	halfWidth := 16.0
	if ratio < 1 {
		halfWidth = 16.0 / ratio
	}

	n := int(math.Round(float64(len(in)) * ratio))
	if n <= 0 {
		return nil, fmt.Errorf("重采样后长度为 0")
	}
	out := make([]float32, n)

	for i := 0; i < n; i++ {
		center := float64(i) * step
		lo := int(math.Floor(center - halfWidth))
		hi := int(math.Ceil(center + halfWidth))

		var acc, wsum float64
		for j := lo; j <= hi; j++ {
			t := float64(j) - center
			if math.Abs(t) >= halfWidth {
				continue
			}
			// 理想低通；t=0 时 sinc 的极限是 1
			x := 2 * cutoff * t
			var sinc float64
			if x == 0 {
				sinc = 1
			} else {
				sinc = math.Sin(math.Pi*x) / (math.Pi * x)
			}
			// 居中 Blackman：w(±H)=0，正好与截断点对上
			w := 0.42 + 0.5*math.Cos(math.Pi*t/halfWidth) + 0.08*math.Cos(2*math.Pi*t/halfWidth)

			jj := j
			if jj < 0 {
				jj = 0
			} else if jj >= len(in) {
				jj = len(in) - 1
			}
			h := 2 * cutoff * sinc * w
			acc += float64(in[jj]) * h
			wsum += h
		}
		if wsum != 0 {
			out[i] = float32(acc / wsum)
		}
	}
	return out, nil
}

// WriteWAV 把 float32 单声道写成 PCM16 WAV（一次性验证用，不进运行时）。
func WriteWAV(path string, samples []float32, rate int) error {
	buf := make([]byte, 44+len(samples)*2)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+len(samples)*2))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 1)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(len(samples)*2))
	for i, s := range samples {
		v := s * 32767
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(int16(v)))
	}
	return os.WriteFile(path, buf, 0o600)
}
