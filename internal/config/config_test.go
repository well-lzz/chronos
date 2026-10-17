package config

import "testing"

func TestTSecBenchGatewayURL(t *testing.T) {
	tests := map[string]string{
		"https://api.deepseek.com/v1":                               "http://api.deepseek.com.tsecbench.gw/v1",
		"https://dashscope.aliyuncs.com/compatible-mode/v1":         "http://dashscope.aliyuncs.com.tsecbench.gw/compatible-mode/v1",
		"http://api.deepseek.com.tsecbench.gw/v1":                   "http://api.deepseek.com.tsecbench.gw/v1",
		"https://example.com:8443/openai/v1?api-version=2026-01-01": "http://example.com.tsecbench.gw:8443/openai/v1?api-version=2026-01-01",
	}
	for input, want := range tests {
		if got := TSecBenchGatewayURL(input); got != want {
			t.Errorf("TSecBenchGatewayURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadRewritesModelEndpointsOnlyInHostedMode(t *testing.T) {
	t.Setenv("TSECBENCH_HOSTED", "true")
	t.Setenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1")
	t.Setenv("QWEN_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1")
	t.Setenv("GLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4")

	cfg := Load()
	if !cfg.TSecBenchHosted {
		t.Fatal("hosted mode was not enabled")
	}
	for name, base := range map[string]string{
		"deepseek": cfg.DeepSeekBaseURL,
		"qwen":     cfg.QwenBaseURL,
		"glm":      cfg.GLMBaseURL,
	} {
		if !IsTSecBenchGatewayURL(base) {
			t.Errorf("%s base URL was not rewritten for TSecBench: %q", name, base)
		}
	}
}
