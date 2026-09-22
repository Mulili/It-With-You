package main

import (
	"agent-for-you-love/internal/config"
	"agent-for-you-love/internal/db"
	historystore "agent-for-you-love/internal/history/store"
	"agent-for-you-love/internal/llm"
	memorystore "agent-for-you-love/internal/memory/store"
	"agent-for-you-love/internal/persona/builtin"
	"agent-for-you-love/internal/persona/store"
	"context"
	"embed"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

// trayIcon 复用 Wails 生成的应用图标作为托盘图标。
// 注意：systray 在 Windows 上要求 .ico，给 png 会加载失败。
//
//go:embed build/windows/icon.ico
var trayIcon []byte

func main() {
	// .env 只在这里加载一次，之后各包只管读环境变量（理由见 internal/config 的包注释）
	if path := config.LoadDotEnv(); path != "" {
		log.Printf("[config] 已加载 %s", path)
	}

	llmCfg := llm.ConfigFromEnv()
	if llmCfg.APIKey == "" {
		// 只是提示，不 Fatal：桌宠启动失败又看不到任何窗口，是最难排查的一类故障。
		log.Println("[llm] 未检测到 COMPANION_LLM_API_KEY，发消息时才会报错")
	}

	// 数据库：建池 + 建表 + 校验版本。连不上不阻止启动——人格退回内存、记忆整体停用。
	pool := openDB()

	// 人格存储：给了连接池就用 PG，否则退回内存实现（内置人格照常可用）
	personaStore := store.OpenStore(pool, loadBuiltinPersonas())

	// 历史存储：与人格共用同一个连接池（同一个库，三块数据都在上面）
	historyStore := historystore.Open(pool)

	// 记忆存储：同样共用这个连接池。注意"存储可用"与"嵌入可用"是两件事——
	// 后者不可用时，上层根本不会走到这里（记忆功能整体停用）
	memoryStore := memorystore.Open(pool)

	// 嵌入服务（阶段4 的记忆检索用）。与对话分开配，因为它们通常是两个服务。
	// 探测失败返回 nil：记忆功能整体停用，对话照常，且启动不被打断。
	embedder := probeEmbedder()

	app := NewApp(llm.NewOpenAIProvider(llmCfg), personaStore, historyStore, memoryStore, embedder)

	err := wails.Run(&options.App{
		Title:         "常驻助手",
		Width:         340,
		Height:        460,
		Frameless:     true, // 无边框：去掉标题栏，桌宠才能"浮"在桌面上
		AlwaysOnTop:   true, // 置顶：不被其他窗口遮住
		DisableResize: true,
		// Wails 在 Windows 上会丢掉这个颜色的 alpha：内部只用 RGB 建一把实心画刷，通过
		// GCLP_HBRBACKGROUND 刷成窗口类的背景（wails 内部 win32.SetBackgroundColour 的
		// 函数签名里就没有 alpha 参数）。也就是说填什么色，窗口就是什么底色，A=0 并不透明。
		//
		// 窗口透明不靠这里，靠下面 Windows 段的 WindowIsTranslucent：开了之后窗口没有
		// 重定向表面，这把刷子画出来的东西不再进入 DWM 合成，自然透不出来。
		// 若将来真机上仍看到实色底，先排查这里（试试把 BackgroundColour 设为 nil 隔离变量），
		// 而不是回到下面这些已证实无效的做法。
		//
		// 以下做法已验证无效或有害，不要再试：
		//   - 给窗口加 WS_EX_LAYERED：WebView2 走 DirectComposition，用 SetWindowLong
		//     改扩展样式会切换窗口合成模式，把它的视觉层踢掉，窗口整个不渲染。
		//   - DwmExtendFrameIntoClientArea 全玻璃（MARGINS 四个方向都设 -1）：无效。
		//   - 清窗口类背景画刷（GCLP_HBRBACKGROUND 设 0）：会造成"假透明"——
		//     客户区从此不再重绘，只是残留屏幕上的旧像素，看着像透明，实际是脏画面。
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 0},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			// 让 WebView2 的默认背景透明：网页自己画的透明区域不会被涂白。
			// 这一项只解决"网页层"，窗口那层底要靠下面的 WindowIsTranslucent，两层配套才有效果。
			WebviewIsTransparent: true,
			// 窗口级透明：Wails 在建窗时给窗口样式加上 WS_EX_NOREDIRECTIONBITMAP，窗口不再有
			// 重定向表面，WebView2 的视觉层直接交给 DWM 合成，网页透明的地方才能真正透出桌面。
			//
			// 这个样式必须在 CreateWindow 的那一刻就带上，事后用 SetWindowLong 补会切换窗口
			// 合成模式、把 WebView2 的视觉层踢掉（窗口整个不渲染）——所以不要自己手动加。
			// 同理，上面 BackgroundColour 画的那把实心刷子此时也不再进入合成，不会露出来。
			//
			// 透出来的是什么由 BackdropType 决定（本项默认 Auto，先不锁死，等真机看过再定）：
			// Win11 22621+ 走 DWM backdrop，可用 windows.None 直接看见桌面、Mica/Acrylic 做磨砂；
			// 更低的 Windows 版本 Wails 会回落成 ACCENT_ENABLE_BLURBEHIND，只能得到模糊透视。
			WindowIsTranslucent: true,
			// 关掉 Win11 无边框窗口的圆角与投影
			DisableFramelessWindowDecorations: true,
			// 阶段1 先不做点击穿透，按文档"关键决策"：先可交互，后续再加穿透开关。
			// 注意透明 ≠ 穿透：透明像素仍然属于窗口，一样会挡住桌面点击。
		},
	})

	if err != nil {
		log.Fatalf("应用启动失败: %v", err)
	}
}

// openDB 连库并保证表结构就位；失败只记日志并返回 nil。
//
// 返回 nil 表示"没有可用的数据库"，由上层各自决定怎么降级：人格退回内存实现照常可用，
// 记忆功能整体停用——两种姿态不同，所以这里不替它们做决定。
func openDB() *pgxpool.Pool {
	dsn := db.DSNFromEnv()
	if dsn == "" {
		log.Printf("[db] 未配置 COMPANION_PG_DSN，数据只存在内存里（进程重启即丢）")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), db.ConnectTimeout)
	defer cancel()

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		log.Printf("[db] 数据库不可用，数据只存在内存里：%v", err)
		return nil
	}
	log.Printf("[db] 数据库就绪（表结构 v%d）", db.SchemaVersion)
	return pool
}

// probeEmbedder 在启动时确认嵌入服务可用，并返回可用的嵌入器（不可用则返回 nil）。
//
// 这里**不返回错误、也不阻止启动**：嵌入只是记忆功能的前提，对话并不依赖它。
// 探测的意义是把"服务没起""维度不匹配"这类问题在启动日志里说清楚，而不是留到
// 用户第一次写记忆时，由 pgvector 报一个与嵌入看不出关系的错（expected 1024 dimensions）。
//
// 返回 nil 而不是返回"一个不可用的嵌入器"，是为了让上层只需要判空：
// 否则每个调用点都得自己记得"先探测再用"。
//
// 同步探测是刻意的：本机 Ollama 没启动时，连接被拒是瞬时的，不会拖慢启动；
// 只有把 base_url 指到不可达的远程地址，才会真的等满这 5 秒。
func probeEmbedder() llm.Embedder {
	cfg := llm.EmbedConfigFromEnv()
	e := llm.NewOpenAIEmbedder(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := e.Verify(ctx); err != nil {
		log.Printf("[embed] 嵌入服务不可用（%s @ %s）: %v", cfg.Model, cfg.BaseURL, err)
		log.Printf("[embed] 记忆功能将停用（不抽取、不写索引），对话与人格不受影响；" +
			"本机装 Ollama 后 `ollama pull bge-m3` 可恢复")
		return nil
	}
	log.Printf("[embed] 嵌入服务就绪：%s @ %s（%d 维）", cfg.Model, cfg.BaseURL, cfg.Dim)
	return e
}

// loadBuiltinPersonas 读取内置人格；失败只记日志，不让应用起不来。
//
// 内置人格坏掉本该在开发期被 TestBuiltinPersonasValid 拦住（它是随 exe 分发的数据，
// 编译时就已经确定），真走到这里说明打包异常；此时让人格列表为空、应用照常启动，
// 比直接崩掉更好排查。
func loadBuiltinPersonas() []builtin.Entry {
	builtins, err := builtin.Load()
	if err != nil {
		log.Printf("[persona] 加载内置人格失败: %v", err)
	}
	return builtins
}
