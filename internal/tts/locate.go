package tts

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 目录名与文件名。与嵌入那套（internal/llm/embed_onnx.go）保持同一套约定：
// 随包分发就放 exe 同目录的 models/ 下，用户自己的放用户目录。
const (
	modelsDirName = "models"
	sharedDirName = "shared"
	voicesDirName = "voices"
	appDirName    = "It-With-You"

	// maxUpLevels 是"从工作目录向上找"的最大层数。
	// 与 internal/config 的 MaxSearchUpLevels 取同一个值，理由也一样：
	// 开发期的 exe 不在项目根，而模型在项目根。
	maxUpLevels = 5
)

// upwardDirs 从工作目录起逐层向上给出候选目录（含工作目录本身）。
//
// 为什么需要它：`wails dev` 的 exe 在 build/bin，模型却在仓库根的 models/，
// 只按 exe 同目录找必然落空。这与 env.go 里 LoadDotEnvUpward 要解决的是同一类问题——
// 那里是 .env 在项目根、测试或在子目录启动时找不到。
//
// 打包之后工作目录通常不是项目根，这条候选不会命中，**不影响分发布局**：
// 发布仍然靠"exe 同目录的 models/"那一档。
func upwardDirs() []string {
	dir, err := os.Getwd()
	if err != nil {
		return nil
	}
	out := make([]string, 0, maxUpLevels)
	for i := 0; i < maxUpLevels; i++ {
		out = append(out, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

// LocateBackbone 找共享骨干目录（里面有 hubert、config.json）。
//
// 顺序：显式覆盖 → exe 同目录 models/shared → **工作目录向上逐层** models/shared →
// 用户缓存目录。与音色不同，骨干是**只读、随包**的东西，所以用户侧放在缓存目录。
func LocateBackbone(override string) (string, error) {
	var dirs []string
	if d := strings.TrimSpace(override); d != "" {
		dirs = append(dirs, d)
	}
	if exe := exeDirOrEmpty(); exe != "" {
		dirs = append(dirs, filepath.Join(exe, modelsDirName, sharedDirName))
	}
	for _, d := range upwardDirs() {
		dirs = append(dirs, filepath.Join(d, modelsDirName, sharedDirName))
	}
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(cache, appDirName, modelsDirName, sharedDirName))
	}

	tried := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !fileExists(filepath.Join(dir, "config.json")) {
			tried = append(tried, dir+"（没有 config.json）")
			continue
		}
		return dir, nil
	}
	return "", fmt.Errorf("找不到共享骨干。试过：%s", joinOrNone(tried))
}

// LocateVoice 找音色目录。
//
// 顺序：显式覆盖 → exe 同目录 models/voices/<id>（内置音色，只读）→
// **工作目录向上逐层** models/voices/<id>（开发期）→
// %APPDATA%/<应用名>/voices/<id>（用户音色，可写）。
// Program Files 下不可写，所以用户音色绝不能落在 exe 同目录。
func LocateVoice(id, override string) (string, error) {
	if d := strings.TrimSpace(override); d != "" {
		if !fileExists(filepath.Join(d, "config.json")) {
			return "", fmt.Errorf("指定的音色目录 %s 里没有 config.json", d)
		}
		return d, nil
	}
	if strings.TrimSpace(id) == "" {
		return locateOnlyVoice()
	}

	var dirs []string
	if exe := exeDirOrEmpty(); exe != "" {
		dirs = append(dirs, filepath.Join(exe, modelsDirName, voicesDirName, id))
	}
	for _, d := range upwardDirs() {
		dirs = append(dirs, filepath.Join(d, modelsDirName, voicesDirName, id))
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(cfg, appDirName, voicesDirName, id))
	}

	tried := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !fileExists(filepath.Join(dir, "config.json")) {
			tried = append(tried, dir+"（没有 config.json）")
			continue
		}
		return dir, nil
	}
	return "", fmt.Errorf("找不到音色 %q。试过：%s", id, joinOrNone(tried))
}

// locateOnlyVoice 在没指定 id 时使用：**前提是只有一个音色**。
//
// 为什么允许不指定：App 目前还没有"选音色"的设置项，而开发期就一个 feibi。
// 要求调用方必须先知道 id，等于把"目录里明明有一个"变成"启动不了"。
// 多个音色时**不猜**——报错把候选列出来，选哪个是用户的事（与嵌入那边 findModelFile 同一原则）。
func locateOnlyVoice() (string, error) {
	var roots []string
	if exe := exeDirOrEmpty(); exe != "" {
		roots = append(roots, filepath.Join(exe, modelsDirName, voicesDirName))
	}
	for _, d := range upwardDirs() {
		roots = append(roots, filepath.Join(d, modelsDirName, voicesDirName))
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		roots = append(roots, filepath.Join(cfg, appDirName, voicesDirName))
	}

	for _, root := range roots {
		matches, err := filepath.Glob(filepath.Join(root, "*", "config.json"))
		if err != nil || len(matches) == 0 {
			continue
		}
		if len(matches) > 1 {
			names := make([]string, 0, len(matches))
			for _, m := range matches {
				names = append(names, filepath.Base(filepath.Dir(m)))
			}
			return "", fmt.Errorf("%s 下有 %d 个音色（%s），无法确定用哪个：请在设置里指定",
				root, len(matches), strings.Join(names, "、"))
		}
		return filepath.Dir(matches[0]), nil
	}
	return "", fmt.Errorf("找不到任何音色（models/voices/<id>/config.json）。试过：%s", joinOrNone(roots))
}

// LocateRuntime 找 onnxruntime 动态库。
//
// 先看显式覆盖，再看 exe 同目录——dll 与 exe 放一起是分发布局，
// 而 dev 环境（exe 在临时目录里）应当显式给路径。
func LocateRuntime(override string) (string, error) {
	var cands []string
	if p := strings.TrimSpace(override); p != "" {
		cands = append(cands, p)
	}
	if exe := exeDirOrEmpty(); exe != "" {
		cands = append(cands, filepath.Join(exe, runtimeFileName()))
	}

	tried := make([]string, 0, len(cands))
	for _, p := range cands {
		if !fileExists(p) {
			tried = append(tried, p+"（不存在）")
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("找不到 %s。试过：%s。把它放到 exe 同目录，或显式指定路径",
		runtimeFileName(), joinOrNone(tried))
}

func runtimeFileName() string {
	switch runtime.GOOS {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.dylib"
	default:
		return "libonnxruntime.so"
	}
}

func exeDirOrEmpty() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return ""
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "没有可试的位置"
	}
	return strings.Join(items, "；")
}
