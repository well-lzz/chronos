// Package config loads runtime settings from environment variables with sane
// defaults. No framework — plain os.Getenv, mirroring the Python Settings.
package config

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
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
	TSecBenchHosted      bool
	PlatformInsecureTLS  bool
	MaxConcurrent        int
	KillchainTimeLimitS  int
	TotalBudgetSeconds   int
	MinRemainingToSpawnS int
	MaxChallengeAttempts int
	DeferRetryCooldownS  int
	AutoResumeIdleS      int

	// LLM providers (OpenAI-compatible). Base URLs have defaults; keys from env.
	DeepSeekKey         string
	DeepSeekKeys        []string
	DeepSeekBaseURL     string
	QwenKey             string
	QwenBaseURL         string
	GLMKey              string
	GLMBaseURL          string
	GLMModelID          string
	ContextWindowTokens int
	MaxDelegateDepth    int
	MaxDelegateChildren int
	MaxDelegateSteps    int
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

func uniqueNonEmpty(values ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		for _, item := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
			item = strings.TrimSpace(item)
			if item != "" && !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	return out
}

// Load reads settings from the environment.
func Load() Settings {
	hosted := getenvBool("TSECBENCH_HOSTED", false)
	deepSeekKeys := uniqueNonEmpty(os.Getenv("DEEPSEEK_API_KEY"), os.Getenv("DEEPSEEK_API_KEY_2"), os.Getenv("DEEPSEEK_API_KEYS"))
	deepSeekKey := ""
	if len(deepSeekKeys) > 0 {
		deepSeekKey = deepSeekKeys[0]
	}
	settings := Settings{
		StateDir:            getenv("CHRONOS_STATE_DIR", getenv("STATE_DIR", ".chronos-state")),
		ChallengeTimeLimitS: getenvInt("CHALLENGE_TIME_LIMIT_SECONDS", 1800),
		MaxBumps:            getenvInt("MAX_BUMPS", 20),
		SolverRetryDelayS:   getenvFloat("SOLVER_RETRY_DELAY_SECONDS", 2.0),
		AllowLocalExec:      getenvBool("ALLOW_LOCAL_EXEC", false),
		TargetInsecureTLS:   getenvBool("TARGET_INSECURE_TLS", false),

		DeepSeekKey:         deepSeekKey,
		DeepSeekKeys:        deepSeekKeys,
		DeepSeekBaseURL:     getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1"),
		QwenKey:             os.Getenv("QWEN_API_KEY"),
		QwenBaseURL:         getenv("QWEN_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1"),
		GLMKey:              os.Getenv("GLM_API_KEY"),
		GLMBaseURL:          getenv("GLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
		GLMModelID:          getenv("GLM_MODEL_ID", "glm-4-plus"),
		ContextWindowTokens: getenvInt("MODEL_CONTEXT_WINDOW_TOKENS", 0),
		MaxDelegateDepth:    getenvInt("MAX_DELEGATE_DEPTH", 1),
		MaxDelegateChildren: getenvInt("MAX_DELEGATE_CHILDREN", 3),
		MaxDelegateSteps:    getenvInt("MAX_DELEGATE_STEPS", 20),

		PlatformMode:         getenv("PLATFORM_MODE", "tsec"),
		PlatformURL:          getenv("PLATFORM_URL", ""),
		BenchmarkURL:         getenv("BENCHMARK_BASE_URL", "https://tsecbench.zc.tencent.com"),
		BenchmarkToken:       os.Getenv("BENCHMARK_TOKEN"),
		TSecBenchHosted:      hosted,
		PlatformInsecureTLS:  getenvBool("PLATFORM_INSECURE_TLS", false),
		MaxConcurrent:        getenvInt("MAX_CONCURRENT_CHALLENGES", 3),
		KillchainTimeLimitS:  getenvInt("KILLCHAIN_TIME_LIMIT_SECONDS", 2700),
		TotalBudgetSeconds:   getenvInt("TOTAL_BENCHMARK_BUDGET_SECONDS", 0),
		MinRemainingToSpawnS: getenvInt("MIN_REMAINING_SECONDS_TO_SPAWN", 300),
		MaxChallengeAttempts: getenvInt("MAX_CHALLENGE_ATTEMPTS", 3),
		DeferRetryCooldownS:  getenvInt("DEFER_RETRY_COOLDOWN_SECONDS", 120),
		AutoResumeIdleS:      getenvInt("AUTO_RESUME_IDLE_SECONDS", 240),
	}
	if hosted {
		settings.DeepSeekBaseURL = TSecBenchGatewayURL(settings.DeepSeekBaseURL)
		settings.QwenBaseURL = TSecBenchGatewayURL(settings.QwenBaseURL)
		settings.GLMBaseURL = TSecBenchGatewayURL(settings.GLMBaseURL)
	}
	return settings
}

// PlatformBase returns the effective platform base URL (PLATFORM_URL overrides).
func (s Settings) PlatformBase() string {
	if s.PlatformURL != "" {
		return s.PlatformURL
	}
	return s.BenchmarkURL
}

// TSecBenchGatewayURL converts a public OpenAI-compatible endpoint to the
// allowlisted gateway form used inside TSecBench hosted containers. Invalid or
// non-HTTP URLs are returned unchanged so validation can report them clearly.
func TSecBenchGatewayURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return raw
	}
	if !strings.HasSuffix(strings.ToLower(u.Hostname()), ".tsecbench.gw") {
		host := u.Hostname() + ".tsecbench.gw"
		if port := u.Port(); port != "" {
			host = net.JoinHostPort(host, port)
		}
		u.Host = host
	}
	u.Scheme = "http"
	return u.String()
}

// IsTSecBenchGatewayURL reports whether raw is ready for the hosted network.
func IsTSecBenchGatewayURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "http" && strings.HasSuffix(strings.ToLower(u.Hostname()), ".tsecbench.gw")
}
