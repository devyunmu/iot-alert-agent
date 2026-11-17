// Package mcpserver exposes the alert platform through the Model Context
// Protocol. Agents get the same read/ack/analyse capabilities as the web UI,
// which is the core narrative of this project: IoT platform APIs become tools an
// agent can call.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpsdk "github.com/mark3labs/mcp-go/server"

	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/service"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

const (
	serverName    = "iot-alert-agent"
	serverVersion = "0.1.0"
)

// Deps are the collaborators used by the MCP tools.
type Deps struct {
	Store    *store.Store
	Service  *service.Service
	Analyzer agent.Analyzer
}

// New builds the MCP server with all four tools registered.
func New(d Deps) *mcpsdk.MCPServer {
	s := mcpsdk.NewMCPServer(serverName, serverVersion,
		mcpsdk.WithToolCapabilities(false),
		mcpsdk.WithInstructions("IoT alert triage tools. Typical flow: list_alerts to find open alerts, get_device for asset context, summarize_alert to produce a triage note, then ack_alert after a human confirms. All data is synthetic."),
	)
	for _, t := range Tools(d) {
		s.AddTool(t.Tool, t.Handler)
	}
	return s
}

// ServerName returns the advertised MCP implementation name.
func ServerName() string { return serverName }

// Serve runs the server over stdio until the client disconnects.
func Serve(s *mcpsdk.MCPServer) error { return mcpsdk.ServeStdio(s) }

// Tools returns the tool definitions so tests can mount them on an in-process server.
func Tools(d Deps) []mcpsdk.ServerTool {
	return []mcpsdk.ServerTool{
		listAlertsTool(d),
		getDeviceTool(d),
		summarizeAlertTool(d),
		ackAlertTool(d),
	}
}

// ---- list_alerts ----

type listAlertsArgs struct {
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Type     string `json:"type"`
	Limit    int    `json:"limit"`
}

type alertSummary struct {
	ID          string             `json:"id"`
	Title       string             `json:"title"`
	Type        domain.AlertType   `json:"type"`
	Severity    domain.Severity    `json:"severity"`
	Status      domain.AlertStatus `json:"status"`
	DeviceID    string             `json:"device_id"`
	DeviceName  string             `json:"device_name"`
	Site        string             `json:"site"`
	Message     string             `json:"message"`
	Metric      string             `json:"metric,omitempty"`
	Value       *float64           `json:"value,omitempty"`
	Unit        string             `json:"unit,omitempty"`
	Threshold   *float64           `json:"threshold,omitempty"`
	Occurrences int                `json:"occurrences"`
	CreatedAt   string             `json:"created_at"`
}

func listAlertsTool(d Deps) mcpsdk.ServerTool {
	tool := mcp.NewTool("list_alerts",
		mcp.WithDescription("List device alerts, ordered by severity then recency. Use the filters to find the queue an operator should handle next."),
		mcp.WithString("status", mcp.Description("Filter by handling status"), mcp.Enum("open", "acked", "closed")),
		mcp.WithString("severity", mcp.Description("Filter by severity"), mcp.Enum("critical", "warning", "info")),
		mcp.WithString("type", mcp.Description("Filter by alert type"), mcp.Enum("offline", "low_battery", "sensor_anomaly", "ota_failure", "comm_flap")),
		mcp.WithNumber("limit", mcp.Description("Maximum alerts to return (1-50, default 10)"), mcp.DefaultNumber(10), mcp.Min(1), mcp.Max(50)),
	)
	handler := func(ctx context.Context, _ mcp.CallToolRequest, args listAlertsArgs) (*mcp.CallToolResult, error) {
		f := store.Filter{
			Query:    "",
			Status:   domain.AlertStatus(strings.TrimSpace(args.Status)),
			Severity: domain.Severity(strings.TrimSpace(args.Severity)),
			Type:     domain.AlertType(strings.TrimSpace(args.Type)),
		}
		if f.Status != "" && !f.Status.Valid() {
			return mcp.NewToolResultError(fmt.Sprintf("invalid status %q: use open, acked or closed", args.Status)), nil
		}
		if f.Severity != "" && !f.Severity.Valid() {
			return mcp.NewToolResultError(fmt.Sprintf("invalid severity %q: use critical, warning or info", args.Severity)), nil
		}
		if f.Type != "" && !f.Type.Valid() {
			return mcp.NewToolResultError(fmt.Sprintf("invalid type %q: use offline, low_battery, sensor_anomaly, ota_failure or comm_flap", args.Type)), nil
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}
		f.Limit = limit

		views := d.Service.List(f)
		out := make([]alertSummary, 0, len(views))
		for _, v := range views {
			out = append(out, summarize(v.Alert, v.Device))
		}
		return mcp.NewToolResultJSON(map[string]any{
			"count":  len(out),
			"alerts": out,
		})
	}
	return mcpsdk.ServerTool{Tool: tool, Handler: mcp.NewTypedToolHandler(handler)}
}

func summarize(a domain.Alert, dev domain.Device) alertSummary {
	return alertSummary{
		ID: a.ID, Title: a.Title, Type: a.Type, Severity: a.Severity, Status: a.Status,
		DeviceID: a.DeviceID, DeviceName: dev.Name, Site: dev.Site, Message: a.Message,
		Metric: a.Metric, Value: a.Value, Unit: a.Unit, Threshold: a.Threshold,
		Occurrences: a.Occurrences, CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// ---- get_device ----

type getDeviceArgs struct {
	DeviceID string `json:"device_id"`
}

func getDeviceTool(d Deps) mcpsdk.ServerTool {
	tool := mcp.NewTool("get_device",
		mcp.WithDescription("Get a device profile (model, site, firmware, battery, last heartbeat) plus its most recent alerts. Call this before interpreting a single alert."),
		mcp.WithString("device_id", mcp.Required(), mcp.Description("Device ID exactly as shown in the alert, e.g. DEV-1001")),
	)
	handler := func(ctx context.Context, _ mcp.CallToolRequest, args getDeviceArgs) (*mcp.CallToolResult, error) {
		id := strings.TrimSpace(args.DeviceID)
		if id == "" {
			return mcp.NewToolResultError("device_id is required"), nil
		}
		dev, err := d.Store.GetDevice(id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return mcp.NewToolResultError(fmt.Sprintf("device %q not found", id)), nil
			}
			return mcp.NewToolResultError("internal error while loading device"), nil
		}
		views := d.Service.List(store.Filter{DeviceID: id})
		total := len(views)
		sort.SliceStable(views, func(i, j int) bool { return views[i].CreatedAt.After(views[j].CreatedAt) })
		if len(views) > 5 {
			views = views[:5]
		}
		recent := make([]alertSummary, 0, len(views))
		for _, v := range views {
			recent = append(recent, summarize(v.Alert, v.Device))
		}
		return mcp.NewToolResultJSON(map[string]any{
			"device":        dev,
			"recent_alerts": recent,
			"alert_count":   total,
		})
	}
	return mcpsdk.ServerTool{Tool: tool, Handler: mcp.NewTypedToolHandler(handler)}
}

// ---- summarize_alert ----

type summarizeArgs struct {
	AlertID string `json:"alert_id"`
	Force   bool   `json:"force"`
}

func summarizeAlertTool(d Deps) mcpsdk.ServerTool {
	tool := mcp.NewTool("summarize_alert",
		mcp.WithDescription("Generate an AI triage note for one alert: summary, severity with reasoning, likely causes and ordered remediation steps. Results are cached; set force=true to regenerate."),
		mcp.WithString("alert_id", mcp.Required(), mcp.Description("Alert ID, e.g. ALT-0001")),
		mcp.WithBoolean("force", mcp.Description("Regenerate even if a cached analysis exists"), mcp.DefaultBool(false)),
	)
	handler := func(ctx context.Context, _ mcp.CallToolRequest, args summarizeArgs) (*mcp.CallToolResult, error) {
		id := strings.TrimSpace(args.AlertID)
		if id == "" {
			return mcp.NewToolResultError("alert_id is required"), nil
		}
		an, err := d.Service.Analyze(ctx, id, args.Force)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return mcp.NewToolResultError(fmt.Sprintf("alert %q not found", id)), nil
			}
			return mcp.NewToolResultError("analysis failed: " + err.Error()), nil
		}
		v, err := d.Service.Detail(id)
		if err != nil {
			return mcp.NewToolResultError("analysis succeeded but alert lookup failed"), nil
		}
		return mcp.NewToolResultJSON(map[string]any{
			"analysis": an,
			"alert":    summarize(v.Alert, v.Device),
			"device":   v.Device,
		})
	}
	return mcpsdk.ServerTool{Tool: tool, Handler: mcp.NewTypedToolHandler(handler)}
}

// ---- ack_alert ----

type ackArgs struct {
	AlertID  string `json:"alert_id"`
	Operator string `json:"operator"`
}

func ackAlertTool(d Deps) mcpsdk.ServerTool {
	tool := mcp.NewTool("ack_alert",
		mcp.WithDescription("Acknowledge an alert after a human has seen it. Idempotent; closed alerts cannot be acknowledged."),
		mcp.WithString("alert_id", mcp.Required(), mcp.Description("Alert ID to acknowledge")),
		mcp.WithString("operator", mcp.Description("Human operator name recorded on the alert (default mcp-agent)")),
	)
	handler := func(ctx context.Context, _ mcp.CallToolRequest, args ackArgs) (*mcp.CallToolResult, error) {
		id := strings.TrimSpace(args.AlertID)
		if id == "" {
			return mcp.NewToolResultError("alert_id is required"), nil
		}
		op := strings.TrimSpace(args.Operator)
		if op == "" {
			op = "mcp-agent"
		}
		if len([]rune(op)) > 40 {
			op = string([]rune(op)[:40])
		}
		a, changed, err := d.Service.Ack(id, op)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				return mcp.NewToolResultError(fmt.Sprintf("alert %q not found", id)), nil
			case errors.Is(err, store.ErrClosed):
				return mcp.NewToolResultError(fmt.Sprintf("alert %q is closed and cannot be acknowledged", id)), nil
			default:
				return mcp.NewToolResultError("internal error while acknowledging alert"), nil
			}
		}
		msg := "alert acknowledged"
		if !changed {
			msg = "alert was already acknowledged"
		}
		return mcp.NewToolResultJSON(map[string]any{
			"alert":   a,
			"changed": changed,
			"message": msg,
		})
	}
	return mcpsdk.ServerTool{Tool: tool, Handler: mcp.NewTypedToolHandler(handler)}
}
