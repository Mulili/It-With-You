// Package config 负责读取本地配置：.env 文件与进程环境变量。
//
// 为什么单独成包：加载 .env 是"进程级一次性"的动作，而读它的地方不止一处
// （LLM 与数据库）。放在任一业务包里都会让另一个包反向依赖它，也会出现
// "两个包各自解析 .env、行为不一致"的隐患。
//
// 约定：由 main 在启动时调用一次 LoadDotEnv，之后各包只管读环境变量。
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DotEnvPath 是开发期用的本地环境变量文件名，相对当前工作目录。
//
// 注意 wails dev 的工作目录是项目根，双击 exe 时是 build\bin——
// 所以它只是开发期的便利：打包分发后靠系统环境变量，不要指望这个文件。
const DotEnvPath = ".env"

// MaxSearchUpLevels 是向上查找 .env 的最大层数（包目录里跑测试时要用）。
const MaxSearchUpLevels = 5

// LoadDotEnv 把当前工作目录下的 .env 读进进程环境，返回实际加载的路径；没找到返回空串。
func LoadDotEnv() string {
	return loadDotEnvAt(DotEnvPath)
}

// LoadDotEnvUpward 从当前工作目录向上逐层查找 .env 并加载，返回路径；没找到返回空串。
//
// 用途：`go test ./internal/xxx` 的工作目录是**包目录**，而 .env 在项目根，
// 直接 LoadDotEnv 是找不到的。这样测试与"从子目录启动"都不必在 shell 里手写解析命令
// （那类命令的引号很容易被终端粘贴弄坏）。
func LoadDotEnvUpward() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < MaxSearchUpLevels; i++ {
		candidate := filepath.Join(dir, DotEnvPath)
		if _, err := os.Stat(candidate); err == nil {
			return loadDotEnvAt(candidate)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// Get 读环境变量，为空（或只有空白）时返回默认值。
func Get(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// GetInt 读环境变量并解析成整数；缺失**或解析失败**时返回默认值。
//
// 解析失败也回落默认值（而不是报错）：一个格式写错的配置项不该让应用起不来。
// 代价是"写错了却看不出来"，所以需要严格校验的调用方要自己再验一次——
// 例如嵌入维度，会在启动时与服务端实际返回的维度比对（见 llm.Embedder 的 Verify）。
func GetInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func loadDotEnvAt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "" // 没有 .env 是正常情况，直接用系统环境变量
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		// 允许 KEY=value / KEY="value" / KEY='value' 三种写法
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		// 刻意不覆盖已有环境变量：系统级或 CI 里配置的值优先级应该高于本地开发文件，
		// 否则会出现"我在系统里改了值，跑起来却还是文件里的"这种很难查的问题。
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}
	return path
}
