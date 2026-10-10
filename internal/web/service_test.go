package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// localBaseURL 决定"要不要托管进程"，所以边界必须严：
// 非本机不管、没写端口不管（根本无从知道该起在哪个端口）。
func TestLocalBaseURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"http://127.0.0.1:7000", "http://127.0.0.1:7000", false},
		{"http://127.0.0.1:7000/", "http://127.0.0.1:7000", false}, // 末尾斜杠要抹掉
		{"  http://localhost:7070  ", "http://localhost:7070", false},
		{"http://[::1]:7000", "http://[::1]:7000", false},
		{"https://search.example.com:443", "", true}, // 不是本机
		{"http://192.168.1.9:7000", "", true},        // 局域网里别人的机器，也不管
		{"http://127.0.0.1", "", true},               // 没写端口
		{"", "", true},
		{"不是地址", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := localBaseURL(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("该报错，实际返回 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got != c.want {
				t.Errorf("规整结果 %q，期望 %q", got, c.want)
			}
		})
	}
}

func TestPortOf(t *testing.T) {
	if got := portOf("http://127.0.0.1:7000"); got != "7000" {
		t.Errorf("取到端口 %q，期望 7000", got)
	}
}

// 服务**已经在跑**时：Ensure 什么都不做，而且状态里不该说"是我们起的"——
// 用户自己起的那个进程，应用退出时不能去关。
func TestEnsureReusesRunningService(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer srv.Close()

	svc := NewService(func() string { return "" }, func() string { return srv.URL })
	if err := svc.Ensure(context.Background()); err != nil {
		t.Fatalf("服务明明在跑，Ensure 不该失败: %v", err)
	}
	if path != "/health" {
		t.Errorf("探活该打 /health，实际打了 %q", path)
	}
	st := svc.Status()
	if !st.Running {
		t.Errorf("状态该是运行中：%+v", st)
	}
	if st.Managed {
		t.Errorf("不是我们拉起的，不该标成 Managed：%+v", st)
	}
	svc.Stop() // 没有要关的进程，应当安静地什么都不做（不能 panic）
}

// 地址不是本机时不托管：别人的服务我们既起不了、也说不清它为什么不在。
func TestEnsureSkipsRemoteAddress(t *testing.T) {
	svc := NewService(
		func() string { return `C:\nope\openserp.exe` },
		func() string { return "https://search.example.com:443" },
	)
	err := svc.Ensure(context.Background())
	if err == nil {
		t.Fatal("远程地址该被拒绝托管")
	}
	if !strings.Contains(err.Error(), "不是本机") {
		t.Errorf("错误该说清「为什么不管」，而不是笼统失败：%v", err)
	}
}

// 没找到程序时，那句话必须指出"是没找到程序"——用户看到的就是它，也是他下一步的依据。
func TestEnsureSaysWhenExeMissing(t *testing.T) {
	// 端口 1 上不会有人监听：探活会立刻失败，于是走到"找程序"那一步
	svc := NewService(func() string { return "" }, func() string { return "http://127.0.0.1:1" })
	err := svc.Ensure(context.Background())
	if err == nil {
		t.Fatal("没有程序、又没有服务在跑，该报错")
	}
	if !strings.Contains(err.Error(), "没找到搜索服务程序") {
		t.Errorf("错误该指出「没找到程序」：%v", err)
	}
}
