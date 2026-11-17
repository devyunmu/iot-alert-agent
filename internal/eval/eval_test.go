package eval

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/devyunmu/iot-alert-agent/internal/domain"
)

func TestScore(t *testing.T) {
	sc := Scoring{KeypointWeight: 0.8, SeverityWeight: 0.2, PassThreshold: 0.75}
	hits := []Hit{{Hit: true}, {Hit: true}, {Hit: true}, {Hit: false}}
	rate, sev, score := Score(hits, domain.SeverityCritical, domain.SeverityCritical, sc)
	if rate != 0.75 || !sev || score != 0.8 {
		t.Fatalf("rate=%v sev=%v score=%v, want 0.75/true/0.8", rate, sev, score)
	}

	_, sev, score = Score(hits, domain.SeverityWarning, domain.SeverityCritical, sc)
	if sev || math.Abs(score-0.6) > 1e-9 {
		t.Fatalf("severity mismatch: sev=%v score=%v, want false/0.6", sev, score)
	}

	rate, _, score = Score(nil, domain.SeverityInfo, domain.SeverityInfo, sc)
	if rate != 1 || score != 1 {
		t.Fatalf("empty keypoints: rate=%v score=%v", rate, score)
	}
}

// TestScoreUsesConfiguredWeights is the regression for a hard-coded 0.8/0.2:
// the same hits must produce a different score when the evalset says so.
func TestScoreUsesConfiguredWeights(t *testing.T) {
	hits := []Hit{{Hit: true}, {Hit: false}} // 50% keypoints, severity wrong
	custom := Scoring{KeypointWeight: 0.6, SeverityWeight: 0.4, PassThreshold: 0.5}
	rate, sev, score := Score(hits, domain.SeverityWarning, domain.SeverityCritical, custom)
	if rate != 0.5 || sev {
		t.Fatalf("rate=%v sev=%v", rate, sev)
	}
	if math.Abs(score-0.3) > 1e-9 {
		t.Fatalf("score = %v, want 0.6*0.5 = 0.3", score)
	}
}

func TestMatchesIgnoresWhitespaceAndCase(t *testing.T) {
	text := strings.ToLower("当前电流 38.6 A，额定 25A；ERR_SIGNATURE 校验失败")
	cases := map[string]bool{
		"38.6":          true,
		"38.6a":         true,
		"38.6 A":        true,
		"25a":           true,
		"err_signature": true,
		"36.8":          false,
		"":              false,
	}
	for pattern, want := range cases {
		if got := matches(text, pattern); got != want {
			t.Errorf("matches(%q) = %v, want %v", pattern, got, want)
		}
	}
}

func TestLoadSetValidation(t *testing.T) {
	valid := `{"version":1,"cases":[{"id":"E1","alert_id":"A1","expected_severity":"critical","keypoints":[{"id":"k1","desc":"d","any":["x"]}]}]}`
	set, err := LoadSet(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if set.Scoring.PassThreshold != 0.75 || set.Scoring.KeypointWeight != 0.8 || set.Scoring.SeverityWeight != 0.2 {
		t.Fatalf("defaults not applied: %+v", set.Scoring)
	}
	if len(set.Cases) != 1 {
		t.Fatalf("cases = %d", len(set.Cases))
	}

	// Explicit weights must survive loading untouched.
	custom, err := LoadSet(strings.NewReader(`{"scoring":{"keypoint_weight":0.6,"severity_weight":0.4,"pass_threshold":0.5},"cases":[{"id":"E1","alert_id":"A1","expected_severity":"critical","keypoints":[{"id":"k1","any":["x"]}]}]}`))
	if err != nil {
		t.Fatalf("LoadSet(custom): %v", err)
	}
	if custom.Scoring.KeypointWeight != 0.6 || custom.Scoring.SeverityWeight != 0.4 {
		t.Fatalf("configured weights were not read: %+v", custom.Scoring)
	}

	bad := map[string]string{
		"empty":            `{"cases":[]}`,
		"negative weight":  `{"scoring":{"keypoint_weight":-0.2},"cases":[{"id":"E1","alert_id":"A1","expected_severity":"info","keypoints":[{"id":"k","any":["x"]}]}]}`,
		"weight above one": `{"scoring":{"severity_weight":1.5},"cases":[{"id":"E1","alert_id":"A1","expected_severity":"info","keypoints":[{"id":"k","any":["x"]}]}]}`,
		"bad severity":     `{"cases":[{"id":"E1","alert_id":"A1","expected_severity":"nope","keypoints":[{"any":["x"]}]}]}`,
		"no keypoints":     `{"cases":[{"id":"E1","alert_id":"A1","expected_severity":"info","keypoints":[]}]}`,
		"no patterns":      `{"cases":[{"id":"E1","alert_id":"A1","expected_severity":"info","keypoints":[{"id":"k","any":[]}]}]}`,
		"missing alert id": `{"cases":[{"id":"E1","expected_severity":"info","keypoints":[{"any":["x"]}]}]}`,
		"duplicate id": `{"cases":[` +
			`{"id":"E1","alert_id":"A1","expected_severity":"info","keypoints":[{"any":["x"]}]},` +
			`{"id":"E1","alert_id":"A2","expected_severity":"info","keypoints":[{"any":["x"]}]}]}`,
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSet(strings.NewReader(raw)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSummarizeAndReport(t *testing.T) {
	results := []Result{
		{CaseID: "E1", AlertID: "A1", Expected: domain.SeverityCritical, Actual: domain.SeverityCritical,
			Hits: []Hit{{Hit: true}, {Hit: true}}, KeypointRate: 1, SeverityOK: true, Score: 1},
		{CaseID: "E2", AlertID: "A2", Expected: domain.SeverityWarning, Actual: domain.SeverityCritical,
			Hits: []Hit{{Hit: false}, {Hit: false}}, KeypointRate: 0, SeverityOK: false, Score: 0, Err: "timeout"},
	}
	s := Summarize(results)
	if s.Cases != 2 || s.Errors != 1 || s.Hits != 2 || s.Keypoints != 2 {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if s.SeverityAccuracy != 1 {
		t.Errorf("severity accuracy over valid cases = %v, want 1", s.SeverityAccuracy)
	}

	var buf bytes.Buffer
	s = RenderReport(&buf, "mock", "mock://rules-v1", results, Scoring{KeypointWeight: 0.8, SeverityWeight: 0.2, PassThreshold: 0.75})
	out := buf.String()
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "错误 1") {
		t.Errorf("report missing failure verdict:\n%s", out)
	}
	if !strings.Contains(out, "E2") || !strings.Contains(out, "timeout") {
		t.Errorf("report missing failed case detail:\n%s", out)
	}
	if !strings.Contains(out, "weight 0.80") || !strings.Contains(out, "weight 0.20") {
		t.Errorf("report does not print the scoring weights:\n%s", out)
	}
}

// TestReportPrintsConfiguredWeights covers S3 end to end at the reporting layer:
// tuning evalset weights must change both the score and the printed header.
func TestReportPrintsConfiguredWeights(t *testing.T) {
	results := []Result{{
		CaseID: "E1", AlertID: "A1", Expected: domain.SeverityInfo, Actual: domain.SeverityInfo,
		Hits: []Hit{{Hit: true}, {Hit: false}}, KeypointRate: 0.5, SeverityOK: true,
		Score: 0.6*0.5 + 0.4*1,
	}}
	var buf bytes.Buffer
	RenderReport(&buf, "mock", "m", results, Scoring{KeypointWeight: 0.6, SeverityWeight: 0.4, PassThreshold: 0.5})
	out := buf.String()
	t.Logf("report with 0.6/0.4 weights:\n%s", out)
	for _, want := range []string{"weight 0.60", "weight 0.40", "阈值 0.50", "PASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}
