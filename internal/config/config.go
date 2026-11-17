// Package config loads runtime settings from environment variables only.
// No credential is ever compiled into the binary or committed to the repo.
package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds process configuration.
type Config struct {
	Addr        string
	BaseURL     string
	APIKey      string
	Model       string
	Timeout     time.Duration
	MaxTokens   int
	Temperature float64
	Mock        bool
}

// Load reads configuration from the environment, applying demo-friendly
// defaults. The default endpoint points at a local OpenAI-compatible gateway;
// it is not a credential and can be overridden with LLM_BASE_URL.
//
// The HTTP listener defaults to loopback. The service has no authentication, so
// binding 0.0.0.0 would expose the API to the whole LAN; operators who really
// want that must opt in explicitly with ADDR.
func Load() Config {
	c := Config{
		Addr:        env("ADDR", "127.0.0.1:8080"),
		BaseURL:     strings.TrimRight(env("LLM_BASE_URL", "http://127.0.0.1:3000/v1"), "/"),
		APIKey:      firstNonEmpty(os.Getenv("LLM_API_KEY"), os.Getenv("OPENAI_API_KEY")),
		Model:       env("LLM_MODEL", "gpt-4o-mini"),
		Timeout:     envDuration("LLM_TIMEOUT", 180*time.Second),
		MaxTokens:   envInt("LLM_MAX_TOKENS", 4000),
		Temperature: envFloat("LLM_TEMPERATURE", 0.2),
		Mock:        envBool("LLM_MOCK", false),
	}
	if c.APIKey == "" {
		// Running without credentials is a supported demo mode, but it must be
		// obvious in the logs why output is rule-based instead of LLM-generated.
		c.Mock = true
		log.Printf("[config] LLM_API_KEY not set; using deterministic mock analyzer")
	}
	return c
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("[config] invalid %s=%q, using %d", key, v, def)
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("[config] invalid %s=%q, using %.2f", key, v, def)
		return def
	}
	return f
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("[config] invalid %s=%q, using %s", key, v, def)
		return def
	}
	return d
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	log.Printf("[config] invalid %s=%q, using %v", key, v, def)
	return def
}
