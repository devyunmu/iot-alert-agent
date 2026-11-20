// Package eval loads the human-labelled evaluation set and scores analyzer
// output by keypoint recall plus severity agreement. It is intentionally free of
// network code so the same logic scores live LLM output and the offline mock.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

// Keypoint is one expected fact or recommendation. Any matching pattern counts
// as a hit, which keeps scoring tolerant of phrasing while still requiring
// specific values and domain actions.
type Keypoint struct {
	ID   string   `json:"id"`
	Desc string   `json:"desc"`
	Any  []string `json:"any"`
	// RequiresInference marks a point that cannot be produced by copying the
	// alert text (e.g. "escalate because cold-chain monitoring is about to go
	// blind"). Echoing title/message earns no credit for such a point, which
	// keeps the score from rewarding restatement.
	RequiresInference bool `json:"requires_inference"`
}

// Case is one labelled alert.
type Case struct {
	ID               string          `json:"id"`
	AlertID          string          `json:"alert_id"`
	Focus            string          `json:"focus"`
	ExpectedSeverity domain.Severity `json:"expected_severity"`
	Keypoints        []Keypoint      `json:"keypoints"`
}

// Scoring is the weight/threshold block from the evalset. It is read from the
// config file and used verbatim by Score and RenderReport so the report always
// shows the weights that actually produced the numbers.
type Scoring struct {
	KeypointWeight float64 `json:"keypoint_weight"`
	SeverityWeight float64 `json:"severity_weight"`
	PassThreshold  float64 `json:"pass_threshold"`
}

// Set is the full evaluation file.
type Set struct {
	Version     int     `json:"version"`
	Description string  `json:"description"`
	Scoring     Scoring `json:"scoring"`
	Cases       []Case  `json:"cases"`
}

// LoadSet decodes and validates an evaluation set.
func LoadSet(r io.Reader) (*Set, error) {
	var s Set
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode evalset: %w", err)
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("evalset has no cases")
	}
	if s.Scoring.KeypointWeight == 0 {
		s.Scoring.KeypointWeight = 0.8
	}
	if s.Scoring.SeverityWeight == 0 {
		s.Scoring.SeverityWeight = 0.2
	}
	if s.Scoring.PassThreshold == 0 {
		s.Scoring.PassThreshold = 0.75
	}
	if err := s.Scoring.Validate(); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(s.Cases))
	for i := range s.Cases {
		c := &s.Cases[i]
		if c.ID == "" || c.AlertID == "" {
			return nil, fmt.Errorf("case %d: id and alert_id are required", i)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		if !c.ExpectedSeverity.Valid() {
			return nil, fmt.Errorf("case %s: invalid expected_severity %q", c.ID, c.ExpectedSeverity)
		}
		if len(c.Keypoints) == 0 {
			return nil, fmt.Errorf("case %s: no keypoints", c.ID)
		}
		for _, kp := range c.Keypoints {
			if len(kp.Any) == 0 {
				return nil, fmt.Errorf("case %s keypoint %s: no patterns", c.ID, kp.ID)
			}
		}
	}
	return &s, nil
}

// Validate rejects nonsensical weights instead of silently producing a score
// that cannot reach the threshold.
func (sc Scoring) Validate() error {
	if sc.KeypointWeight < 0 || sc.KeypointWeight > 1 {
		return fmt.Errorf("scoring.keypoint_weight %.2f outside [0,1]", sc.KeypointWeight)
	}
	if sc.SeverityWeight < 0 || sc.SeverityWeight > 1 {
		return fmt.Errorf("scoring.severity_weight %.2f outside [0,1]", sc.SeverityWeight)
	}
	if sc.KeypointWeight+sc.SeverityWeight == 0 {
		return fmt.Errorf("scoring weights must not both be zero")
	}
	if sc.PassThreshold < 0 || sc.PassThreshold > 1 {
		return fmt.Errorf("scoring.pass_threshold %.2f outside [0,1]", sc.PassThreshold)
	}
	return nil
}

// Hit records whether one keypoint was found and which pattern matched.
type Hit struct {
	ID        string `json:"id"`
	Desc      string `json:"desc"`
	Hit       bool   `json:"hit"`
	Matched   string `json:"matched,omitempty"`
	Inference bool   `json:"inference"`
	// Echoable means the alert's own title/message already satisfy this
	// keypoint: any hit on it may be restatement rather than triage.
	Echoable bool `json:"echoable"`
}

// Result is the outcome for one case.
type Result struct {
	CaseID       string          `json:"case_id"`
	AlertID      string          `json:"alert_id"`
	Focus        string          `json:"focus"`
	Expected     domain.Severity `json:"expected_severity"`
	Actual       domain.Severity `json:"actual_severity,omitempty"`
	Hits         []Hit           `json:"hits"`
	KeypointRate float64         `json:"keypoint_rate"`
	SeverityOK   bool            `json:"severity_ok"`
	Score        float64         `json:"score"`
	LatencyMS    int64           `json:"latency_ms"`
	SummaryText  string          `json:"summary,omitempty"`
	// Analysis is the analyzer's full structured output, retained so eval
	// artifacts can be audited without re-running the model.
	Analysis *domain.Analysis `json:"analysis,omitempty"`
	Err      string           `json:"error,omitempty"`
}

// Run analyzes every case against the given alert store. workers bounds
// concurrency so a rate-limited endpoint is not hammered.
func Run(ctx context.Context, an agent.Analyzer, st *store.Store, set *Set, workers int) []Result {
	if workers < 1 {
		workers = 1
	}
	results := make([]Result, len(set.Cases))
	jobs := make(chan int)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for idx := range jobs {
			results[idx] = runOne(ctx, an, st, set.Cases[idx], set.Scoring)
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	for i := range set.Cases {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func runOne(ctx context.Context, an agent.Analyzer, st *store.Store, c Case, sc Scoring) Result {
	r := Result{CaseID: c.ID, AlertID: c.AlertID, Focus: c.Focus, Expected: c.ExpectedSeverity, Hits: make([]Hit, len(c.Keypoints))}
	start := time.Now()

	alert, err := st.GetAlert(c.AlertID)
	if err != nil {
		r.Err = "load alert: " + err.Error()
		return r
	}
	device, err := st.GetDevice(alert.DeviceID)
	if err != nil {
		r.Err = "load device: " + err.Error()
		return r
	}
	// A case budget covers three attempts of the documented 4-minute endpoint
	// timeout; retries must not inherit an exhausted first attempt.
	callCtx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	an1, err := an.Analyze(callCtx, alert, device)
	r.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.Actual = an1.Severity
	r.SummaryText = an1.Summary
	r.Analysis = &an1
	text := matchText(an1)
	source := sourceText(alert)
	for i, kp := range c.Keypoints {
		hit := Hit{ID: kp.ID, Desc: kp.Desc, Inference: kp.RequiresInference, Echoable: anyMatch(source, kp.Any)}
		for _, pattern := range kp.Any {
			if !matches(text, pattern) {
				continue
			}
			// An inference point matched by a phrase that is also present in
			// the alert text is restatement, not judgement: no credit.
			if kp.RequiresInference && matches(source, pattern) {
				continue
			}
			hit.Hit = true
			hit.Matched = pattern
			break
		}
		r.Hits[i] = hit
	}
	r.KeypointRate, r.SeverityOK, r.Score = Score(r.Hits, an1.Severity, c.ExpectedSeverity, sc)
	return r
}

func matchText(a domain.Analysis) string {
	parts := append([]string{a.Summary, a.SeverityReason}, a.LikelyCauses...)
	parts = append(parts, a.Actions...)
	return strings.ToLower(strings.Join(parts, "\n"))
}

// sourceText is what the model was given about the alert itself; anything
// matched here can be produced by copying, without triage reasoning.
func sourceText(a domain.Alert) string {
	return strings.ToLower(a.Title + "\n" + a.Message)
}

func anyMatch(text string, patterns []string) bool {
	for _, p := range patterns {
		if matches(text, p) {
			return true
		}
	}
	return false
}

// Matches is the exported form used by evalset guards that check a keypoint is
// not satisfiable by the raw alert text.
func Matches(text, pattern string) bool { return matches(text, pattern) }

// matches compares case-insensitively, ignoring whitespace so "25 A" and "25A"
// are equivalent. Numeric patterns are matched on a boundary: "8.5" must not
// hit "18.5", "87" must not hit "187", and "1.9.0" must not hit "11.9.0".
// Chinese text is unaffected by lowercasing.
func matches(text, pattern string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return false
	}
	t := strings.ToLower(text)
	if strings.ContainsAny(p, "0123456789") {
		if containsBounded(t, p) {
			return true
		}
		ts, ps := stripSpace(t), stripSpace(p)
		if ts == t && ps == p {
			return false
		}
		return containsBounded(ts, ps)
	}
	if strings.Contains(t, p) {
		return true
	}
	return strings.Contains(stripSpace(t), stripSpace(p))
}

// containsBounded finds p in text and requires that every numeric edge of the
// pattern is not glued to another digit or decimal point.
func containsBounded(text, p string) bool {
	for from := 0; from+len(p) <= len(text); {
		i := strings.Index(text[from:], p)
		if i < 0 {
			return false
		}
		i += from
		j := i + len(p)
		leftOK := !isDigit(p[0]) || i == 0 || !isNumericByte(text[i-1])
		rightOK := !isDigit(p[len(p)-1]) || j == len(text) || !isNumericByte(text[j])
		if leftOK && rightOK {
			return true
		}
		from = i + 1
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isNumericByte treats '.' as numeric so "8.5" also rejects "8.55".
func isNumericByte(b byte) bool { return isDigit(b) || b == '.' }

func stripSpace(s string) string {
	return strings.NewReplacer(" ", "", "\t", "", "\n", "", "\u3000", "").Replace(s)
}

// Score combines keypoint recall with severity agreement using the weights from
// the evalset (defaults: 0.8 and 0.2). Passing the weights in — instead of
// hard-coding them — keeps the score honest when someone tunes the file.
func Score(hits []Hit, actual, expected domain.Severity, sc Scoring) (keypointRate float64, severityOK bool, score float64) {
	if len(hits) == 0 {
		keypointRate = 1
	} else {
		n := 0
		for _, h := range hits {
			if h.Hit {
				n++
			}
		}
		keypointRate = float64(n) / float64(len(hits))
	}
	severityOK = actual == expected
	score = sc.KeypointWeight*keypointRate + sc.SeverityWeight*boolFloat(severityOK)
	return keypointRate, severityOK, score
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Summary aggregates a run.
type Summary struct {
	Cases            int     `json:"cases"`
	Errors           int     `json:"errors"`
	Keypoints        int     `json:"keypoints"`
	Hits             int     `json:"hits"`
	KeypointRate     float64 `json:"keypoint_rate"`
	SeverityAccuracy float64 `json:"severity_accuracy"`
	AvgScore         float64 `json:"avg_score"`
	// Inference vs replication split, so restatement cannot hide inside one
	// aggregate recall number.
	Inference       int `json:"inference_keypoints"`
	InferenceHits   int `json:"inference_hits"`
	Replication     int `json:"replication_keypoints"`
	ReplicationHits int `json:"replication_hits"`
	// Echoable counts keypoints whose patterns already appear in the alert
	// text: an upper bound on what copying title/message could score.
	Echoable int     `json:"echoable_keypoints"`
	EchoRate float64 `json:"echo_keypoint_rate"`
	Passed   bool    `json:"passed"`
}

// Summarize aggregates results; errored cases score zero but are excluded from
// severity accuracy, since there was no prediction to compare.
func Summarize(results []Result) Summary {
	s := Summary{Cases: len(results)}
	var sevN, sevOK int
	var sum float64
	for _, r := range results {
		if r.Err != "" {
			s.Errors++
			continue
		}
		sevN++
		if r.SeverityOK {
			sevOK++
		}
		for _, h := range r.Hits {
			s.Keypoints++
			if h.Hit {
				s.Hits++
			}
			if h.Inference {
				s.Inference++
				if h.Hit {
					s.InferenceHits++
				}
			} else {
				s.Replication++
				if h.Hit {
					s.ReplicationHits++
				}
			}
			if h.Echoable {
				s.Echoable++
			}
		}
		sum += r.Score
	}
	if s.Cases > 0 {
		s.AvgScore = sum / float64(s.Cases)
	}
	if s.Keypoints > 0 {
		s.KeypointRate = float64(s.Hits) / float64(s.Keypoints)
		s.EchoRate = float64(s.Echoable) / float64(s.Keypoints)
	}
	if sevN > 0 {
		s.SeverityAccuracy = float64(sevOK) / float64(sevN)
	}
	return s
}

// RenderReport writes a human-readable score report and the pass/fail verdict.
// The scoring line prints the weights actually used, so a tuned evalset cannot
// silently disagree with the report.
func RenderReport(w io.Writer, mode, analyzer string, results []Result, sc Scoring) Summary {
	s := Summarize(results)
	s.Passed = s.Errors == 0 && s.AvgScore >= sc.PassThreshold

	fmt.Fprintf(w, "\n=== IoT Alert Agent · 评测报告 ===\n")
	fmt.Fprintf(w, "模式: %-8s 分析器: %-28s 用例: %d\n", mode, analyzer, len(results))
	fmt.Fprintf(w, "评分: 要点命中 %.0f%% (weight %.2f) + 定级准确率（模型 vs 规则基线）%.0f%% (weight %.2f)，通过阈值 %.2f\n",
		sc.KeypointWeight*100, sc.KeypointWeight, sc.SeverityWeight*100, sc.SeverityWeight, sc.PassThreshold)
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 78))
	fmt.Fprintf(w, "%-7s %-10s %-6s %-4s %-7s %-6s %-8s\n", "CASE", "ALERT", "LEVEL", "SEV", "KEYPOINTS", "SCORE", "LATENCY")
	for _, r := range results {
		if r.Err != "" {
			fmt.Fprintf(w, "%-7s %-10s %-6s %-4s %-9s %-6s %-8s\n", r.CaseID, r.AlertID, short(r.Expected), "-", "ERROR", "0.00", fmt.Sprintf("%dms", r.LatencyMS))
			continue
		}
		sevMark := "✗"
		if r.SeverityOK {
			sevMark = "✓"
		}
		hitN := 0
		for _, h := range r.Hits {
			if h.Hit {
				hitN++
			}
		}
		fmt.Fprintf(w, "%-7s %-10s %-6s %-4s %-9s %-6.2f %-8s\n",
			r.CaseID, r.AlertID, short(r.Expected), sevMark,
			fmt.Sprintf("%d/%d", hitN, len(r.Hits)), r.Score, fmt.Sprintf("%dms", r.LatencyMS))
	}

	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 78))
	fmt.Fprintf(w, "要点命中率 %.3f (%d/%d) · 推理要点 %d/%d · 复读要点 %d/%d · 定级准确率 %.3f · 平均得分 %.3f · 错误 %d\n",
		s.KeypointRate, s.Hits, s.Keypoints,
		s.InferenceHits, s.Inference, s.ReplicationHits, s.Replication,
		s.SeverityAccuracy, s.AvgScore, s.Errors)
	fmt.Fprintf(w, "可复读要点 %d/%d (%.0f%%)：仅抄告警 title/message 即可命中，不反映分诊能力；推理要点按 requires_inference 单独计分\n",
		s.Echoable, s.Keypoints, s.EchoRate*100)

	var misses []string
	for _, r := range results {
		if r.Err != "" {
			misses = append(misses, fmt.Sprintf("  %s %s: 分析失败: %s", r.CaseID, r.AlertID, truncate(r.Err, 120)))
			continue
		}
		for _, h := range r.Hits {
			if !h.Hit {
				misses = append(misses, fmt.Sprintf("  %s %s: 漏检 %s (%s)", r.CaseID, r.AlertID, h.ID, h.Desc))
			}
		}
		if r.Actual != r.Expected {
			misses = append(misses, fmt.Sprintf("  %s %s: 定级 %s != 期望 %s", r.CaseID, r.AlertID, r.Actual, r.Expected))
		}
	}
	if len(misses) > 0 {
		sort.Strings(misses)
		fmt.Fprintf(w, "未命中明细:\n%s\n", strings.Join(misses, "\n"))
	}
	verdict := "PASS"
	if !s.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "结论: %s（阈值 %.2f，错误数 %d）\n\n", verdict, sc.PassThreshold, s.Errors)
	return s
}

func short(s domain.Severity) string {
	switch s {
	case domain.SeverityCritical:
		return "crit"
	case domain.SeverityWarning:
		return "warn"
	case domain.SeverityInfo:
		return "info"
	}
	return string(s)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
