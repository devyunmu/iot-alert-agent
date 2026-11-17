// Package evalset runs the labelled evaluation against the configured analyzer.
//
//	go test ./evalset -v                 # deterministic offline run (default)
//	EVAL_MODE=live go test ./evalset -v  # require a real endpoint
//	EVAL_MODE=auto go test ./evalset -v  # live when a key is configured
package evalset

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/devyunmu/iot-alert-agent/data"
	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/config"
	"github.com/devyunmu/iot-alert-agent/internal/eval"
	"github.com/devyunmu/iot-alert-agent/internal/llm"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

//go:embed evals.json
var evalsJSON []byte

func TestEvalSet(t *testing.T) {
	set, err := eval.LoadSet(bytes.NewReader(evalsJSON))
	if err != nil {
		t.Fatalf("load evalset: %v", err)
	}

	st, err := loadStore()
	if err != nil {
		t.Fatalf("load dataset: %v", err)
	}
	for _, c := range set.Cases {
		if _, err := st.GetAlert(c.AlertID); err != nil {
			t.Fatalf("case %s references unknown alert: %v", c.ID, err)
		}
	}

	mode := os.Getenv("EVAL_MODE")
	if mode == "" {
		// Default to offline so `go test ./...` is fast, free and stable.
		// Live model evaluation must be requested explicitly.
		mode = "mock"
	}
	if v := os.Getenv("EVAL_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("invalid EVAL_LIMIT %q", v)
		}
		if n < len(set.Cases) {
			set.Cases = set.Cases[:n]
		}
	}
	an, modeLabel, err := selectAnalyzer(mode)
	if err != nil {
		t.Fatalf("analyzer: %v", err)
	}

	workers := envInt("EVAL_WORKERS", 3)
	threshold := set.Scoring.PassThreshold
	if v := os.Getenv("EVAL_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Fatalf("invalid EVAL_THRESHOLD %q: %v", v, err)
		}
		threshold = f
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	start := time.Now()
	results := eval.Run(ctx, an, st, set, workers)
	summary := eval.RenderReport(os.Stdout, modeLabel, an.Describe(), results, threshold)
	fmt.Printf("总耗时 %.1fs（并发 %d）\n\n", time.Since(start).Seconds(), workers)

	if summary.Errors > 0 {
		t.Errorf("%d/%d cases failed to produce a valid analysis", summary.Errors, summary.Cases)
	}
	if summary.AvgScore < threshold {
		t.Errorf("average score %.3f below threshold %.2f", summary.AvgScore, threshold)
	}
	if !summary.Passed {
		t.Errorf("evaluation did not pass (errors=%d, avg=%.3f)", summary.Errors, summary.AvgScore)
	}
}

func selectAnalyzer(mode string) (agent.Analyzer, string, error) {
	cfg := config.Load()
	switch mode {
	case "mock":
		if cfg.APIKey != "" {
			return agent.NewMock(), "mock（默认离线）", nil
		}
		return agent.NewMock(), "mock（未配置 key）", nil
	case "live":
		if cfg.APIKey == "" {
			return nil, "", fmt.Errorf("EVAL_MODE=live requires LLM_API_KEY or OPENAI_API_KEY")
		}
		return agent.NewLLM(llm.New(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Timeout, cfg.MaxTokens, cfg.Temperature)), "live", nil
	case "auto":
		if cfg.Mock {
			return agent.NewMock(), "mock（auto：未配置 key）", nil
		}
		return agent.NewLLM(llm.New(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Timeout, cfg.MaxTokens, cfg.Temperature)), "live", nil
	default:
		return nil, "", fmt.Errorf("unknown EVAL_MODE %q (use auto, mock or live)", mode)
	}
}

func loadStore() (*store.Store, error) {
	alerts, err := data.FS.ReadFile("alerts.json")
	if err != nil {
		return nil, err
	}
	devices, err := data.FS.ReadFile("devices.json")
	if err != nil {
		return nil, err
	}
	return store.New(alerts, devices)
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}
