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
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	sc := set.Scoring
	if v := os.Getenv("EVAL_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Fatalf("invalid EVAL_THRESHOLD %q: %v", v, err)
		}
		sc.PassThreshold = f
	}

	// Generous ceiling: a case may legitimately spend per-attempt timeout ×
	// retry attempts, so this bounds a hung run rather than a slow one.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	start := time.Now()
	results := eval.Run(ctx, an, st, set, workers)
	summary := eval.RenderReport(os.Stdout, modeLabel, an.Describe(), results, sc)
	fmt.Printf("总耗时 %.1fs（并发 %d）\n\n", time.Since(start).Seconds(), workers)

	// EVAL_DUMP=<path> persists one JSON line per case (full structured output
	// included) so reports are auditable without re-running the model.
	if dump := os.Getenv("EVAL_DUMP"); dump != "" {
		f, err := os.Create(dump)
		if err != nil {
			t.Fatalf("open dump %s: %v", dump, err)
		}
		enc := json.NewEncoder(f)
		for _, r := range results {
			if err := enc.Encode(r); err != nil {
				f.Close()
				t.Fatalf("encode dump: %v", err)
			}
		}
		f.Close()
		fmt.Printf("评测产物已写入 %s（%d 条）\n", dump, len(results))
	}

	if summary.Errors > 0 {
		t.Errorf("%d/%d cases failed to produce a valid analysis", summary.Errors, summary.Cases)
	}
	if summary.AvgScore < sc.PassThreshold {
		t.Errorf("average score %.3f below threshold %.2f", summary.AvgScore, sc.PassThreshold)
	}
	if !summary.Passed {
		t.Errorf("evaluation did not pass (errors=%d, avg=%.3f)", summary.Errors, summary.AvgScore)
	}
}

// TestEvalSetHonesty guards the anti-leakage design: some expected severities
// must deliberately differ from the rule baseline, and a keypoint marked
// requires_inference must not be satisfiable by copying the alert text.
func TestEvalSetHonesty(t *testing.T) {
	set, err := eval.LoadSet(bytes.NewReader(evalsJSON))
	if err != nil {
		t.Fatalf("load evalset: %v", err)
	}
	st, err := loadStore()
	if err != nil {
		t.Fatalf("load dataset: %v", err)
	}

	var counter, keypoints, inference, echoable int
	var baseKPs, baseEchoable int
	seenAlerts := map[string]string{}
	for _, c := range set.Cases {
		if prev, ok := seenAlerts[c.AlertID]; ok {
			t.Errorf("cases %s and %s score the same alert %s", prev, c.ID, c.AlertID)
		}
		seenAlerts[c.AlertID] = c.ID

		alert, err := st.GetAlert(c.AlertID)
		if err != nil {
			t.Fatalf("case %s: %v", c.ID, err)
		}
		baselineCase := c.ExpectedSeverity == alert.Severity
		if !baselineCase {
			counter++
			infer := 0
			for _, kp := range c.Keypoints {
				if kp.RequiresInference {
					infer++
				}
			}
			if infer < 2 {
				t.Errorf("case %s disagrees with baseline %s but has %d inference keypoints; the disagreement needs a written reasoning basis",
					c.ID, alert.Severity, infer)
			}
		}
		source := strings.ToLower(alert.Title + "\n" + alert.Message)
		if baselineCase {
			baseKPs += len(c.Keypoints)
		}
		for _, kp := range c.Keypoints {
			keypoints++
			echo := false
			for _, p := range kp.Any {
				if eval.Matches(source, p) {
					echo = true
					if kp.RequiresInference {
						t.Errorf("case %s keypoint %s is requires_inference but pattern %q is copyable from the alert text",
							c.ID, kp.ID, p)
					}
				}
			}
			if echo {
				echoable++
				if baselineCase {
					baseEchoable++
				}
			}
			if kp.RequiresInference {
				inference++
			}
		}
	}
	if counter < 3 {
		t.Errorf("only %d cases disagree with the rule baseline; need at least 3", counter)
	}
	t.Logf("cases=%d keypoints=%d inference=%d counter-baseline=%d echoable=%d (%.0f%%)",
		len(set.Cases), keypoints, inference, counter, echoable, 100*float64(echoable)/float64(keypoints))
	t.Logf("baseline-aligned subset: keypoints=%d echoable=%d (%.0f%%)",
		baseKPs, baseEchoable, 100*float64(baseEchoable)/float64(baseKPs))
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
