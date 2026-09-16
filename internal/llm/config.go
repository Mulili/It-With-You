package llm

import (
	"bufio"
	"os"
	"strings"
)

const (
	envBaseUrl = "COMPANION_LLM_BASE_URL"
	envAPIKey  = "COMPANION_LLM_API_KEY"
	envModel   = "COMPANION_LLM_MODEL"

	// dotEnvPath 是开发期用的本地环境变量文件，相对当前工作目录。
	// 注意 wails dev 的工作目录是项目根，双击 exe 时是 build\bin，
	// 所以它只是开发期的便利：打包分发后靠系统环境变量，不要指望这个文件。
	dotEnvPath = ".env"
)

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

func ConfigFromEnv() Config {
	// 先让 .env 填进进程环境，再统一从这里取，取值的来源只有一处
	loadDotEnv(dotEnvPath)

	return Config{
		BaseURL: envOr(envBaseUrl, "https://api.deepseek.com"),
		APIKey:  os.Getenv(envAPIKey),
		Model:   envOr(envModel, "deepseek-chat"),
	}
}

// loadDotEnv 从 .env 读取键值对，只补齐当前进程尚未设置的变量。
//
// 刻意不覆盖已有的环境变量：系统级或 CI 里配置的优先级应该高于本地开发文件，
// 否则会出现"我在系统里改了值，跑起来却还是文件里的"这种很难查的问题。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // 没有 .env 是正常情况，直接用系统环境变量
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
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
