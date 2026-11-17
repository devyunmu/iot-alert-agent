// Package agent turns a raw alert into a structured triage result. It owns the
// prompt contract and the strict parsing/validation of model output, so the HTTP
// server, MCP server and evaluation runner all share identical behaviour.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/llm"
)

// Analyzer produces a triage analysis for one alert.
type Analyzer interface {
	Analyze(ctx context.Context, alert domain.Alert, device domain.Device) (domain.Analysis, error)
	// Describe identifies the backing model, e.g. "glm-5.3-flash" or "mock://rules-v1".
	Describe() string
	// Mock reports whether results are deterministic/rule-based.
	Mock() bool
}

// LLMAnalyzer asks an OpenAI-compatible endpoint for the triage JSON.
type LLMAnalyzer struct {
	client *llm.Client
}

// NewLLM wires an Analyzer to an LLM client.
func NewLLM(client *llm.Client) *LLMAnalyzer { return &LLMAnalyzer{client: client} }

// Describe returns the model name.
func (a *LLMAnalyzer) Describe() string { return a.client.Model() }

// Mock always reports false for the LLM analyzer.
func (a *LLMAnalyzer) Mock() bool { return false }

// Analyze calls the model and validates its JSON output.
func (a *LLMAnalyzer) Analyze(ctx context.Context, alert domain.Alert, device domain.Device) (domain.Analysis, error) {
	system, user := BuildPrompt(alert, device)
	start := time.Now()
	raw, err := a.client.Chat(ctx, system, user)
	if err != nil {
		return domain.Analysis{}, err
	}
	out, err := ParseOutput(raw)
	if err != nil {
		return domain.Analysis{}, fmt.Errorf("parse model output: %w", err)
	}
	out.AlertID = alert.ID
	out.Model = a.client.Model()
	out.Mock = false
	out.LatencyMS = time.Since(start).Milliseconds()
	out.GeneratedAt = time.Now().UTC()
	return out, nil
}

const systemPrompt = `你是工业物联网平台的告警处置工程师。根据设备档案和告警事件，输出一份可以直接交给值班人员执行的中文处置分析。

只输出一个 JSON 对象，不要输出 Markdown 代码块、前后缀说明或多余文本。字段定义：
{
  "summary": "不超过 80 字，说明发生了什么、涉及哪台设备/哪个站点、影响范围",
  "severity": "critical | warning | info",
  "severity_reason": "不超过 60 字，说明定级依据",
  "likely_causes": ["最多 3 条，按可能性从高到低"],
  "actions": ["2 到 4 条，按执行顺序，包含验证步骤和止损动作"],
  "confidence": 0.0
}

定级规则：
- critical：影响人身/消防安全、冷链温控、核心业务连续性或数据完整性，需要立即处置。
- warning：需要人工跟进，但暂时不影响核心业务。
- info：仅需知悉的提示。

约束：
1. 只能使用输入中出现的事实，不得编造设备、数值、时间或客户信息。
2. 处置建议要结合设备类型与告警类型：离线先查供电与链路；低电量先核对耗电曲线再安排更换；数值异常先做冗余比对排除传感器故障，再检查现场工况；OTA 失败先校验包完整性与升级前置条件；通信抖动先看信号质量与上联端口。
3. 不要承诺"已修复"，只给出待执行的步骤。
4. confidence 表示你对本次分析整体的把握，范围 0 到 1。`

// BuildPrompt renders the system and user messages for one alert.
func BuildPrompt(alert domain.Alert, device domain.Device) (string, string) {
	payload := map[string]any{
		"now": "2026-11-17T08:00:00Z",
		"device": map[string]any{
			"id": device.ID, "name": device.Name, "model": device.Model,
			"site": device.Site, "firmware": device.Firmware, "status": device.Status,
			"battery_pct": device.Battery, "last_seen": device.LastSeen, "tags": device.Tags,
		},
		"alert": map[string]any{
			"id": alert.ID, "type": alert.Type, "rule_id": alert.RuleID,
			"title": alert.Title, "message": alert.Message,
			"baseline_severity": alert.Severity, "status": alert.Status,
			"metric": alert.Metric, "value": alert.Value, "unit": alert.Unit,
			"threshold": alert.Threshold, "occurrences": alert.Occurrences,
			"first_seen": alert.FirstSeen, "last_seen": alert.LastSeen,
			"ota_target": alert.OTATarget,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// The payload is composed of plain structs; this cannot realistically fail.
		return systemPrompt, fmt.Sprintf(`{"alert_id":%q}`, alert.ID)
	}
	return systemPrompt, "告警上下文（JSON）：\n" + string(b)
}

type outputDTO struct {
	Summary        string   `json:"summary"`
	Severity       string   `json:"severity"`
	SeverityReason string   `json:"severity_reason"`
	LikelyCauses   []string `json:"likely_causes"`
	Actions        []string `json:"actions"`
	Confidence     float64  `json:"confidence"`
}

var fenceRE = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

// ParseOutput extracts and validates the triage JSON from a model response.
// Models occasionally wrap JSON in fences or add a sentence around it, so the
// parser is tolerant about framing but strict about required fields.
func ParseOutput(raw string) (domain.Analysis, error) {
	text := strings.TrimSpace(raw)
	if m := fenceRE.FindStringSubmatch(text); m != nil {
		text = strings.TrimSpace(m[1])
	}
	if !strings.HasPrefix(text, "{") {
		start := strings.Index(text, "{")
		end := strings.LastIndex(text, "}")
		if start >= 0 && end > start {
			text = text[start : end+1]
		}
	}

	var dto outputDTO
	dec := json.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&dto); err != nil {
		return domain.Analysis{}, fmt.Errorf("invalid JSON: %w", err)
	}

	summary := strings.TrimSpace(dto.Summary)
	if summary == "" {
		return domain.Analysis{}, fmt.Errorf("summary is required")
	}
	sev := domain.Severity(strings.ToLower(strings.TrimSpace(dto.Severity)))
	if !sev.Valid() {
		return domain.Analysis{}, fmt.Errorf("invalid severity %q", dto.Severity)
	}
	actions := cleanList(dto.Actions, 4)
	if len(actions) == 0 {
		return domain.Analysis{}, fmt.Errorf("at least one action is required")
	}

	conf := dto.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	if conf == 0 {
		conf = 0.6 // absent confidence is common; do not treat it as zero certainty
	}

	return domain.Analysis{
		Summary:        clampRunes(summary, 220),
		Severity:       sev,
		SeverityReason: clampRunes(strings.TrimSpace(dto.SeverityReason), 160),
		LikelyCauses:   cleanList(dto.LikelyCauses, 3),
		Actions:        actions,
		Confidence:     conf,
	}, nil
}

func cleanList(in []string, max int) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(strings.TrimLeft(s, "-•* "))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, clampRunes(s, 200))
		if len(out) == max {
			break
		}
	}
	return out
}

func clampRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
