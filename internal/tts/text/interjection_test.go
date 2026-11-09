package text

import (
	"strings"
	"testing"
)

// 语气词不能整字丢掉：go-pinyin 给的是 n2 / m2，opencpop 里没有这两个音节。
func TestInterjectionPhones(t *testing.T) {
	fe := newTestFrontend(t)

	cases := []struct {
		text string
		want []string
	}{
		{"嗯。", []string{"EE", "en5", "."}},
		{"嗯嗯。", []string{"EE", "en5", "EE", "en5", "."}},
		{"呣。", []string{"m", "u5", "."}},
	}
	for _, c := range cases {
		got := fe.Phones(c.text)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%q → %v，期望 %v", c.text, got, c.want)
		}
	}
}
