package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 托管搜索服务进程的几个数。
const (
	// probeTimeout 是一次"你还在吗"探测的上限。**要短**：它跑在搜索路径上，
	// 只是用来决定"要不要拉起进程"，不该让用户等。
	probeTimeout = 2 * time.Second
	// readyTimeout 是"拉起之后等它就绪"的上限。OpenSERP 先起一个 Fiber 服务、
	// 搜索时才拉 Chrome，实测 1~2 秒就绪；给 10 秒是留给机器慢、杀软扫描的情况。
	readyTimeout = 10 * time.Second
	// readyPollInterval 是等就绪时的轮询间隔。
	readyPollInterval = 300 * time.Millisecond
)

// State 是搜索服务此刻的状况（**给界面看**，不探测，用的是最后一次操作的结论）。
type State struct {
	// Running 为 true 表示服务可达——**我们自己起的、用户自己起的都算**
	Running bool `json:"running"`
	// Managed 为 true 表示这个进程是**应用拉起的**（所以退出时会由我们关掉）。
	// 用户手动起的那个我们不碰：那是他的进程。
	Managed bool `json:"managed"`
	// ExePath 是实际用的程序路径（空 = 没找到）
	ExePath string `json:"exePath"`
	// Message 是一句给人看的说明，直接显示在设置页那一行上
	Message string `json:"message"`
}

// Service 托管"本机搜索服务"这个**外部程序**的生死：它现在在不在、
// 要不要由我们拉起来、退出时要不要关掉。
//
// 为什么由应用托管：搜索是加分项，但**服务没起来时用户看到的只有"她老说查不到"**——
// 这个现象既不指向原因、也没法自己恢复（应用开着的时候没人会去手动起一个服务）。
// 把进程交给应用管，才有唯一一处能说清"它到底在不在"。
//
// ⚠️ 只托管**本机**地址：用户把地址配成别人的服务时，我们既起不了它、
// 也说不清它为什么不在，硬去起一个本地程序只会让现象更难解释。
type Service struct {
	// ExePath 返回可执行文件路径（空串 = 没找到）。
	ExePath func() string
	// BaseURL 返回搜索服务地址。
	BaseURL func() string

	// ensureMu 串行化"探活 + 拉起"：两个搜索同时发现服务不在，也只会拉起一个进程。
	ensureMu sync.Mutex
	mu       sync.Mutex
	// cmd 非 nil = 这个进程是**我们拉起的**（Stop 只关它）
	cmd   *exec.Cmd
	state State

	client *http.Client
}

// NewService 建一个托管器。exePath / baseURL 都用函数：设置改了就立刻生效。
func NewService(exePath, baseURL func() string) *Service {
	return &Service{ExePath: exePath, BaseURL: baseURL, client: &http.Client{}}
}

// Ensure 尽力让搜索服务可用；返回的错误是**给人看的**（调用方会显示它，而不是静默地搜不到）。
//
// 四步，顺序不能换：
//  1. 地址不是本机 → 不托管（用户配了远程服务，我们无从起、也无权起）
//  2. 已经可达 → 什么都不做（**可能是用户自己起的**，那种我们退出时不能关）
//  3. 找到程序 → 拉起 + 等它就绪
//  4. 没找到 → 明确报错
func (s *Service) Ensure(ctx context.Context) error {
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()

	base, err := localBaseURL(s.BaseURL())
	if err != nil {
		s.setState(State{ExePath: s.ExePath(), Message: err.Error()})
		return err
	}
	if s.probe(ctx, base) {
		managed := s.managed()
		msg := "运行中（你自己起的，应用不会关它）"
		if managed {
			msg = "运行中（由应用启动）"
		}
		s.setState(State{Running: true, Managed: managed, ExePath: s.ExePath(), Message: msg})
		return nil
	}

	exe := strings.TrimSpace(s.ExePath())
	if exe == "" {
		err := errors.New("没找到搜索服务程序（openserp），联网搜索用不了")
		s.setState(State{Message: err.Error()})
		return err
	}
	if err := s.spawn(exe, base); err != nil {
		s.setState(State{ExePath: exe, Message: err.Error()})
		return err
	}
	if err := s.waitReady(ctx, base); err != nil {
		s.setState(State{ExePath: exe, Message: err.Error()})
		return err
	}
	s.setState(State{Running: true, Managed: true, ExePath: exe, Message: "运行中（由应用启动）"})
	return nil
}

// Stop 关掉**应用拉起的**那个进程。别人起的（用户手动起的、开机自启的）一律不动。
func (s *Service) Stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	if cmd != nil {
		// 状态要和事实对上：下次打开设置页看到的不该还是"运行中"
		s.state = State{Message: "已关闭"}
	}
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil {
		log.Printf("[web] 关闭搜索服务失败: %v", err)
		return
	}
	log.Printf("[web] 已关闭搜索服务（PID %d）", cmd.Process.Pid)
}

// Status 返回当前状况。**不探测**：这一个是给设置页每次打开时顺手拿的，
// 不该为了显示一行状态去做一次可能 2 秒的往返（结论由 Ensure 更新）。
func (s *Service) Status() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// managed 报告"我们是不是正握着一个自己拉起的进程"。
func (s *Service) managed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}

func (s *Service) setState(st State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// probe 问一次"你在吗"（打 /health）。任何失败都算"不在"，不区分原因——
// 这里唯一要回答的就是"要不要拉起进程"。
func (s *Service) probe(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// 读空一小段再关：不读会让连接没法复用（每轮一次探测，复用是有意义的）
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode == http.StatusOK
}

// spawn 起一个 openserp 服务进程。
func (s *Service) spawn(exe, base string) error {
	port := portOf(base)
	cmd := exec.Command(exe, "serve", "-p", port)
	// 输出**一律丢弃**：它的 JSON 日志会淹掉我们的控制台。要排查就手动起一次
	// （命令见 operation.md）。nil 在 os/exec 里就是"接到空设备"，不是继承我们的。
	//
	// 但它还会在**工作目录**里额外写一个 logs.txt（实测）。不给它一个专门的地方，
	// 它就会丢进应用的工作目录——开发时那正好是仓库根目录，会把一个受版本控制的
	// 文件改脏（本仓库根目录那个 logs.txt 就是这么来的）。给它一个缓存目录，眼不见为净。
	if dir, err := os.UserCacheDir(); err == nil {
		dir = filepath.Join(dir, "It-With-You")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			cmd.Dir = dir
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动搜索服务失败: %w", err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()

	// 必须有人 Wait，否则它退出后会留一个僵尸进程；顺带在它退出时清掉句柄，
	// 好让下一次 Ensure 重新拉起——**服务崩了就该自愈**，而不是从此一直"查不到"。
	go func() {
		_ = cmd.Wait()
		s.mu.Lock()
		if s.cmd == cmd {
			s.cmd = nil
		}
		s.mu.Unlock()
		log.Printf("[web] 搜索服务进程已退出（PID %d）", cmd.Process.Pid)
	}()

	log.Printf("[web] 已拉起搜索服务：%s serve -p %s（PID %d）", exe, port, cmd.Process.Pid)
	return nil
}

// waitReady 轮询 /health 直到就绪、超时、被取消、或进程自己退了。
func (s *Service) waitReady(ctx context.Context, base string) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		if s.probe(ctx, base) {
			return nil
		}
		// 进程已经没了（端口被占用、程序起不来）：不必再等满超时
		if !s.managed() {
			return errors.New("搜索服务刚启动就退出了（端口被占用，或者程序跑不起来）")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("搜索服务启动超时（%.0f 秒内没就绪）", readyTimeout.Seconds())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

// localBaseURL 判断地址是不是**本机**服务，并返回规整后的基址（去掉末尾斜杠）。
//
// 为什么不是本机就不托管：地址指向别人的机器时，我们既起不了它、也说不清它为什么不在。
// 另外**必须写明端口**——不然无从知道该把服务起在哪个端口。
func localBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("没有配置搜索服务地址")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("搜索服务地址看不懂：%s", raw)
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return "", fmt.Errorf("搜索服务地址不是本机（%s），不托管它的进程", u.Hostname())
	}
	if u.Port() == "" {
		return "", errors.New("搜索服务地址要写明端口（形如 http://127.0.0.1:7000）")
	}
	return strings.TrimRight(raw, "/"), nil
}

// portOf 取地址里的端口（localBaseURL 已保证它不为空）。
func portOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Port()
}
