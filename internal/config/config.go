// Package config loads runtime settings from environment variables with sane
// defaults. No framework — plain os.Getenv, mirroring the Python Settings.
package config

import (
	"os"
	"strconv"
)

type Settings struct {
	StateDir            string
	ChallengeTimeLimitS int
	MaxBumps            int
	SolverRetryDelayS   float64
	AllowLocalExec      bool
	TargetInsecureTLS   bool

	// Platform + scheduler (mechanical shell). The platform is a pluggable work
	// source; TSecBench is one adapter (PlatformMode="tsec"), not a hard binding.
	PlatformMode         string
	PlatformURL          string
	BenchmarkURL         string
	BenchmarkToken       string
	PlatformInsecureTLS  bool
	MaxConcurrent        int
	KillchainTimeLimitS  int
	TotalBudgetSeconds   int
	MinRemainingToSpawnS int
	MaxChallengeAttempts int
	DeferRetryCooldownS  int
	AutoResumeIdleS      int

	// LLM providers (OpenAI-compatible). Base URLs have defaults; keys from env.
	DeepSeekKey     string
	DeepSeekBaseURL string
	QwenKey         string
	QwenBaseURL     string
	GLMKey          string
	GLMBaseURL      string
	GLMModelID      string
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	switch v {
	case "1", "true", "TRUE", "yes", "on":
		return true
	case "0", "false", "FALSE", "no", "off":
		return false
	default:
		return def
	}
}

// Load reads settings from the environment.
func Load() Settings {
	return Settings{
		StateDir:            getenv("CHRONOS_STATE_DIR", getenv("STATE_DIR", ".chronos-state")),
		ChallengeTimeLimitS: getenvInt("CHALLENGE_TIME_LIMIT_SECONDS", 1800),
		MaxBumps:            getenvInt("MAX_BUMPS", 20),
		SolverRetryDelayS:   getenvFloat("SOLVER_RETRY_DELAY_SECONDS", 2.0),
		AllowLocalExec:      getenvBool("ALLOW_LOCAL_EXEC", false),
		TargetInsecureTLS:   getenvBool("TARGET_INSECURE_TLS", false),

		DeepSeekKey:     os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekBaseURL: getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1"),
		QwenKey:         os.Getenv("QWEN_API_KEY"),
		QwenBaseURL:     getenv("QWEN_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1"),
		GLMKey:          os.Getenv("GLM_API_KEY"),
		GLMBaseURL:      getenv("GLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
		GLMModelID:      getenv("GLM_MODEL_ID", "glm-4-plus"),

		PlatformMode:         getenv("PLATFORM_MODE", "tsec"),
		PlatformURL:          getenv("PLATFORM_URL", ""),
		BenchmarkURL:         getenv("BENCHMARK_BASE_URL", "https://tsecbench.zc.tencent.com"),
		BenchmarkToken:       os.Getenv("BENCHMARK_TOKEN"),
		PlatformInsecureTLS:  getenvBool("PLATFORM_INSECURE_TLS", false),
		MaxConcurrent:        getenvInt("MAX_CONCURRENT_CHALLENGES", 3),
		KillchainTimeLimitS:  getenvInt("KILLCHAIN_TIME_LIMIT_SECONDS", 2700),
		TotalBudgetSeconds:   getenvInt("TOTAL_BENCHMARK_BUDGET_SECONDS", 0),
		MinRemainingToSpawnS: getenvInt("MIN_REMAINING_SECONDS_TO_SPAWN", 300),
		MaxChallengeAttempts: getenvInt("MAX_CHALLENGE_ATTEMPTS", 3),
		DeferRetryCooldownS:  getenvInt("DEFER_RETRY_COOLDOWN_SECONDS", 120),
		AutoResumeIdleS:      getenvInt("AUTO_RESUME_IDLE_SECONDS", 240),
	}
}

// PlatformBase returns the effective platform base URL (PLATFORM_URL overrides).
func (s Settings) PlatformBase() string {
	if s.PlatformURL != "" {
		return s.PlatformURL
	}
	return s.BenchmarkURL
}
