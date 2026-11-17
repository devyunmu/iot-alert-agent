package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ADDR", "")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("LLM_MODEL", "")
	t.Setenv("LLM_TIMEOUT", "")
	t.Setenv("LLM_MAX_TOKENS", "")
	t.Setenv("LLM_TEMPERATURE", "")
	t.Setenv("LLM_MOCK", "")

	c := Load()
	if c.Addr != ":8080" {
		t.Errorf("addr = %q", c.Addr)
	}
	if c.BaseURL != "http://127.0.0.1:3000/v1" {
		t.Errorf("base url = %q", c.BaseURL)
	}
	if !c.Mock {
		t.Error("missing key should enable mock mode")
	}
	if c.Timeout != 180*time.Second || c.MaxTokens != 4000 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("ADDR", "127.0.0.1:9090")
	t.Setenv("LLM_BASE_URL", "http://llm.local/v1/")
	t.Setenv("LLM_API_KEY", "sk-test")
	t.Setenv("LLM_MODEL", "my-model")
	t.Setenv("LLM_TIMEOUT", "5s")
	t.Setenv("LLM_MAX_TOKENS", "1234")
	t.Setenv("LLM_TEMPERATURE", "0.7")
	t.Setenv("LLM_MOCK", "false")

	c := Load()
	if c.Addr != "127.0.0.1:9090" || c.BaseURL != "http://llm.local/v1" {
		t.Errorf("addr/base = %q/%q", c.Addr, c.BaseURL)
	}
	if c.APIKey != "sk-test" || c.Model != "my-model" || c.Mock {
		t.Errorf("unexpected: %+v", c)
	}
	if c.Timeout != 5*time.Second || c.MaxTokens != 1234 || c.Temperature != 0.7 {
		t.Errorf("numeric overrides: %+v", c)
	}
}

func TestLoadInvalidValuesFallBack(t *testing.T) {
	t.Setenv("LLM_API_KEY", "sk-test")
	t.Setenv("LLM_TIMEOUT", "not-a-duration")
	t.Setenv("LLM_MAX_TOKENS", "-3")
	t.Setenv("LLM_TEMPERATURE", "hot")
	t.Setenv("LLM_MOCK", "maybe")

	c := Load()
	if c.Timeout != 180*time.Second || c.MaxTokens != 4000 || c.Temperature != 0.2 || c.Mock {
		t.Errorf("invalid values not handled: %+v", c)
	}
}

func TestOpenAIKeyFallback(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	c := Load()
	if c.APIKey != "sk-openai" || c.Mock {
		t.Errorf("OPENAI_API_KEY fallback failed: %+v", c)
	}
}
