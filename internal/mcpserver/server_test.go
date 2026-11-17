package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/mcptest"

	"github.com/devyunmu/iot-alert-agent/data"
	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/service"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

func newDeps(t *testing.T) Deps {
	t.Helper()
	alerts, err := data.FS.ReadFile("alerts.json")
	if err != nil {
		t.Fatalf("read alerts: %v", err)
	}
	devices, err := data.FS.ReadFile("devices.json")
	if err != nil {
		t.Fatalf("read devices: %v", err)
	}
	st, err := store.New(alerts, devices)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	an := agent.NewMock()
	return Deps{Store: st, Service: service.New(st, an, 5*time.Second), Analyzer: an}
}

func decode[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var out T
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode structured content: %v (%s)", err, string(b))
	}
	return out
}

func call(t *testing.T, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func TestMCPToolSurface(t *testing.T) {
	deps := newDeps(t)
	srv, err := mcptest.NewServer(t, Tools(deps)...)
	if err != nil {
		t.Fatalf("start MCP server: %v", err)
	}
	defer srv.Close()
	c := srv.Client()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 4 {
		t.Fatalf("tool count = %d, want 4", len(tools.Tools))
	}
	want := map[string]bool{"list_alerts": false, "get_device": false, "ack_alert": false, "summarize_alert": false}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing tool %q", name)
		}
	}
}

type alertListOut struct {
	Count  int `json:"count"`
	Alerts []struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		DeviceID string `json:"device_id"`
		Severity string `json:"severity"`
	} `json:"alerts"`
}

func TestMCPListAlerts(t *testing.T) {
	deps := newDeps(t)
	srv, err := mcptest.NewServer(t, Tools(deps)...)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	c := srv.Client()

	res := call(t, c, "list_alerts", map[string]any{"status": "open", "limit": 3})
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	out := decode[alertListOut](t, res)
	if out.Count != 3 || len(out.Alerts) != 3 {
		t.Fatalf("count = %d len = %d", out.Count, len(out.Alerts))
	}
	for _, a := range out.Alerts {
		if a.Status != "open" || a.DeviceID == "" || a.ID == "" {
			t.Errorf("unexpected alert %+v", a)
		}
	}

	res = call(t, c, "list_alerts", map[string]any{"severity": "urgent"})
	if !res.IsError {
		t.Fatal("expected tool error for invalid severity")
	}
	if txt := res.Content[0].(mcp.TextContent).Text; !strings.Contains(txt, "invalid severity") {
		t.Errorf("error text = %q", txt)
	}
}

func TestMCPGetDevice(t *testing.T) {
	deps := newDeps(t)
	srv, err := mcptest.NewServer(t, Tools(deps)...)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	c := srv.Client()

	res := call(t, c, "get_device", map[string]any{"device_id": "DEV-1004"})
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	var out struct {
		Device struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Site string `json:"site"`
		} `json:"device"`
		RecentAlerts []struct {
			ID string `json:"id"`
		} `json:"recent_alerts"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Device.ID != "DEV-1004" || out.Device.Name != "库区网关-01" {
		t.Errorf("device = %+v", out.Device)
	}
	if len(out.RecentAlerts) != 3 {
		t.Errorf("recent alerts = %d, want 3", len(out.RecentAlerts))
	}

	res = call(t, c, "get_device", map[string]any{"device_id": "DEV-404"})
	if !res.IsError {
		t.Error("expected error for unknown device")
	}
}

func TestMCPSummarizeAlert(t *testing.T) {
	deps := newDeps(t)
	srv, err := mcptest.NewServer(t, Tools(deps)...)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	c := srv.Client()

	res := call(t, c, "summarize_alert", map[string]any{"alert_id": "ALT-0012"})
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	var out struct {
		Analysis struct {
			AlertID  string   `json:"alert_id"`
			Severity string   `json:"severity"`
			Mock     bool     `json:"mock"`
			Actions  []string `json:"actions"`
			Summary  string   `json:"summary"`
		} `json:"analysis"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Analysis.AlertID != "ALT-0012" || !out.Analysis.Mock || out.Analysis.Severity != "critical" {
		t.Fatalf("analysis = %+v", out.Analysis)
	}
	if len(out.Analysis.Actions) == 0 || !strings.Contains(out.Analysis.Summary, "烟雾") {
		t.Errorf("analysis content = %+v", out.Analysis)
	}
}

func TestMCPAckAlert(t *testing.T) {
	deps := newDeps(t)
	srv, err := mcptest.NewServer(t, Tools(deps)...)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	c := srv.Client()

	res := call(t, c, "ack_alert", map[string]any{"alert_id": "ALT-0009", "operator": "mcp-tester"})
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	var out struct {
		Alert struct {
			Status  string `json:"status"`
			AckedBy string `json:"acked_by"`
		} `json:"alert"`
		Changed bool `json:"changed"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Changed || out.Alert.Status != "acked" || out.Alert.AckedBy != "mcp-tester" {
		t.Fatalf("ack result = %+v", out)
	}

	res = call(t, c, "ack_alert", map[string]any{"alert_id": "ALT-0009"})
	b, _ = json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	if out.Changed {
		t.Error("second ack should be idempotent")
	}

	res = call(t, c, "ack_alert", map[string]any{"alert_id": "ALT-0020"})
	if !res.IsError {
		t.Error("expected error when acknowledging a closed alert")
	}
}
