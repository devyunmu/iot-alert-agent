package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/llm"
)

func sampleAlert() (domain.Alert, domain.Device) {
	val, th := 9.6, 8.0
	a := domain.Alert{
		ID: "ALT-0001", DeviceID: "DEV-1001", Type: domain.TypeSensorAnomaly,
		Title: "1号冷库回风温度持续偏高", Severity: domain.SeverityCritical,
		Status: domain.StatusOpen, Message: "回风温度 9.6°C，高于上限 8.0°C。",
		Metric: "temperature_c", Value: &val, Threshold: &th, Unit: "°C",
		Occurrences: 3, CreatedAt: time.Date(2026, 11, 17, 6, 12, 0, 0, time.UTC),
	}
	d := domain.Device{ID: "DEV-1001", Name: "冷库温湿度传感器-01", Model: "TH-200", Site: "园区A / 1号冷库", Firmware: "2.3.1", Status: "online"}
	return a, d
}

func TestParseOutputValid(t *testing.T) {
	raw := `{"summary":"冷库温度 9.6°C 超过上限","severity":"critical","severity_reason":"冷链温控失效风险","likely_causes":["制冷机组故障","库门未关"],"actions":["人工测温","检查制冷机组"],"confidence":0.82}`
	an, err := ParseOutput(raw)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if an.Severity != domain.SeverityCritical || an.Summary == "" || len(an.Actions) != 2 || an.Confidence != 0.82 {
		t.Fatalf("unexpected analysis: %+v", an)
	}
}

func TestParseOutputToleratesFraming(t *testing.T) {
	body := `{"summary":"s","severity":"warning","actions":["a"],"confidence":0.5}`
	for name, raw := range map[string]string{
		"fence":     "```json\n" + body + "\n```",
		"prefix":    "这是分析结果：\n" + body + "\n请查收。",
		"plain":     body,
		"uppercase": `{"summary":"s","severity":"WARNING","actions":["a"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			an, err := ParseOutput(raw)
			if err != nil {
				t.Fatalf("ParseOutput(%q): %v", raw, err)
			}
			if an.Severity != domain.SeverityWarning && name != "uppercase" {
				t.Errorf("severity = %s", an.Severity)
			}
		})
	}
}

func TestParseOutputRejectsInvalid(t *testing.T) {
	tests := map[string]string{
		"not json":        "sorry, I cannot help",
		"missing summary": `{"severity":"critical","actions":["a"]}`,
		"bad severity":    `{"summary":"s","severity":"urgent","actions":["a"]}`,
		"no actions":      `{"summary":"s","severity":"critical","actions":[]}`,
		"blank actions":   `{"summary":"s","severity":"critical","actions":["  ","- "]}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOutput(raw); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseOutputNormalises(t *testing.T) {
	raw := `{"summary":"  s  ","severity":"info","likely_causes":["c1","c1","","c2","c3","c4"],"actions":["- a1","a1","a2","a3","a4","a5"],"confidence":3}`
	an, err := ParseOutput(raw)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if an.Summary != "s" {
		t.Errorf("summary = %q", an.Summary)
	}
	if len(an.LikelyCauses) != 3 {
		t.Errorf("causes = %d, want 3", len(an.LikelyCauses))
	}
	if len(an.Actions) != 4 {
		t.Errorf("actions = %d, want 4", len(an.Actions))
	}
	if an.Actions[0] != "a1" {
		t.Errorf("action trimming failed: %q", an.Actions[0])
	}
	if an.Confidence != 1 {
		t.Errorf("confidence = %v, want clamped 1", an.Confidence)
	}

	an, err = ParseOutput(`{"summary":"s","severity":"info","actions":["a"]}`)
	if err != nil {
		t.Fatalf("ParseOutput without confidence: %v", err)
	}
	if an.Confidence != 0.6 {
		t.Errorf("default confidence = %v, want 0.6", an.Confidence)
	}
}

func TestBuildPromptIncludesContext(t *testing.T) {
	a, d := sampleAlert()
	system, user := BuildPrompt(a, d)
	if !strings.Contains(system, "只输出一个 JSON") {
		t.Error("system prompt missing JSON contract")
	}
	for _, want := range []string{"ALT-0001", "DEV-1001", "TH-200", "9.6", "8", "冷库"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q", want)
		}
	}
}

func TestMockIsDeterministicAndSpecific(t *testing.T) {
	a, d := sampleAlert()
	m := NewMock()
	if !m.Mock() || m.Describe() != "mock://rules-v1" {
		t.Fatalf("unexpected mock metadata: %s", m.Describe())
	}
	first, err := m.Analyze(context.Background(), a, d)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	second, _ := m.Analyze(context.Background(), a, d)
	if first.Summary != second.Summary || first.Severity != second.Severity {
		t.Error("mock output is not deterministic")
	}
	if first.Severity != a.Severity {
		t.Errorf("mock severity = %s, want baseline %s", first.Severity, a.Severity)
	}
	joined := strings.Join(append(first.LikelyCauses, first.Actions...), " ")
	for _, want := range []string{"制冷机组", "库门", "人工测温"} {
		if !strings.Contains(joined, want) {
			t.Errorf("temperature runbook missing %q", want)
		}
	}

	smoke := a
	smoke.Metric = "smoke_obscuration"
	out, err := m.Analyze(context.Background(), smoke, d)
	if err != nil {
		t.Fatalf("smoke Analyze: %v", err)
	}
	joined = strings.Join(append(out.LikelyCauses, out.Actions...), " ")
	for _, want := range []string{"消防", "疏散", "误报"} {
		if !strings.Contains(joined, want) {
			t.Errorf("smoke runbook missing %q", want)
		}
	}
}

func TestLLMAnalyzerSuccess(t *testing.T) {
	content, _ := json.Marshal(map[string]any{
		"summary":         "冷库温度超限",
		"severity":        "critical",
		"severity_reason": "冷链风险",
		"likely_causes":   []string{"制冷机组故障"},
		"actions":         []string{"人工测温", "检查机组"},
		"confidence":      0.9,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": string(content)}}},
		})
	}))
	defer srv.Close()

	a, d := sampleAlert()
	an := NewLLM(llm.New(srv.URL, "sk-x", "test-model", 2*time.Second, 500, 0.2))
	out, err := an.Analyze(context.Background(), a, d)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if out.AlertID != a.ID || out.Model != "test-model" || out.Mock || out.Severity != domain.SeverityCritical {
		t.Fatalf("unexpected output: %+v", out)
	}
	if out.GeneratedAt.IsZero() {
		t.Error("GeneratedAt not set")
	}
}

func TestLLMAnalyzerErrors(t *testing.T) {
	a, d := sampleAlert()
	t.Run("upstream failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		an := NewLLM(llm.New(srv.URL, "sk-x", "m", time.Second, 100, 0))
		if _, err := an.Analyze(context.Background(), a, d); err == nil {
			t.Fatal("expected upstream error")
		}
	})
	t.Run("invalid model output", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": "not json"}}},
			})
		}))
		defer srv.Close()
		an := NewLLM(llm.New(srv.URL, "sk-x", "m", time.Second, 100, 0))
		_, err := an.Analyze(context.Background(), a, d)
		if err == nil || !strings.Contains(err.Error(), "parse model output") {
			t.Fatalf("err = %v", err)
		}
	})
}
