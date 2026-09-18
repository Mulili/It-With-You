package llm

import (
	"os"

	"agent-for-you-love/internal/config"
)

const (
	envBaseUrl = "COMPANION_LLM_BASE_URL"
	envAPIKey  = "COMPANION_LLM_API_KEY"
	envModel   = "COMPANION_LLM_MODEL"
)

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

// ConfigFromEnv 从环境变量读 LLM 配置。
//
// .env 的加载由 main 在启动时统一做一次（见 internal/config.LoadDotEnv）——
// 各个包不再自己解析 .env，否则会出现"谁先读谁生效""有的包读到了、有的没读到"这类时序问题。
func ConfigFromEnv() Config {
	return Config{
		BaseURL: config.Get(envBaseUrl, "https://api.deepseek.com"),
		// API Key 没有默认值：为空就是没配，由调用方决定怎么提示
		APIKey: os.Getenv(envAPIKey),
		Model:  config.Get(envModel, "deepseek-chat"),
	}
}
