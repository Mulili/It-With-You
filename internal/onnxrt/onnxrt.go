// Package onnxrt 收敛 ONNX Runtime 的**进程级**初始化。
//
// 为什么单独一个包：`InitializeEnvironment` 管的是全局环境，`SetSharedLibraryPath`
// 同样是全局的、且**只有第一次生效**。嵌入与 TTS 是两个功能、两处代码，却共用同一份运行时；
// 各写一份 `sync.Once` 迟早会出现"到底谁先初始化"的问题，而症状很难归因。
package onnxrt

import (
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

var (
	mu     sync.Mutex
	inited bool
	err    error
)

// Ensure 初始化 ONNX Runtime。进程内只真正执行一次，之后直接返回第一次的结果。
//
// runtimePath 是 onnxruntime.dll（Linux/macOS 是对应的 .so/.dylib）。
// ⚠️ 全局且只认第一个成功的路径：进程内无法更换，真要换只能重启。
//
// 同一进程里第一个调用者的路径生效，所以**几个功能应当拿到同一个 dll 路径**
// （都从 exe 同目录取），不要各自指定不同位置。
func Ensure(runtimePath string) error {
	mu.Lock()
	defer mu.Unlock()
	if inited {
		return err
	}
	if !ort.IsInitialized() {
		ort.SetSharedLibraryPath(runtimePath)
		err = ort.InitializeEnvironment()
	}
	inited = true
	return err
}

// Version 返回已加载的 onnxruntime 版本；未初始化时返回空串。
//
// binding 把 C API 头固定在某个版本上，**必须与 dll 对齐**；版本不匹配的症状是
// 加载期一句很难懂的报错，所以启动日志里把实际版本打出来。
func Version() string {
	if !ort.IsInitialized() {
		return ""
	}
	return ort.GetVersion()
}
