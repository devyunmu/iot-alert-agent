// Package domain contains the core IoT alerting types shared by the HTTP
// server, the MCP server and the evaluation runner.
package domain

import "time"

// AlertType is the telemetry rule family that produced an alert.
type AlertType string

const (
	TypeOffline       AlertType = "offline"
	TypeLowBattery    AlertType = "low_battery"
	TypeSensorAnomaly AlertType = "sensor_anomaly"
	TypeOTAFailure    AlertType = "ota_failure"
	TypeCommFlap      AlertType = "comm_flap"
)

// Valid reports whether t is one of the supported alert types.
func (t AlertType) Valid() bool {
	switch t {
	case TypeOffline, TypeLowBattery, TypeSensorAnomaly, TypeOTAFailure, TypeCommFlap:
		return true
	}
	return false
}

// Label returns the human readable Chinese label used by the UI.
func (t AlertType) Label() string {
	switch t {
	case TypeOffline:
		return "设备离线"
	case TypeLowBattery:
		return "低电量"
	case TypeSensorAnomaly:
		return "数值异常"
	case TypeOTAFailure:
		return "OTA 失败"
	case TypeCommFlap:
		return "通信抖动"
	}
	return string(t)
}

// Severity is the operational urgency of an alert.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Valid reports whether s is a known severity.
func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// Rank orders severities for sorting; higher is more urgent.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// Label returns the Chinese label used by the UI.
func (s Severity) Label() string {
	switch s {
	case SeverityCritical:
		return "严重"
	case SeverityWarning:
		return "警告"
	case SeverityInfo:
		return "提示"
	}
	return string(s)
}

// AlertStatus tracks the human handling state of an alert.
type AlertStatus string

const (
	StatusOpen   AlertStatus = "open"
	StatusAcked  AlertStatus = "acked"
	StatusClosed AlertStatus = "closed"
)

// Valid reports whether s is a known status.
func (s AlertStatus) Valid() bool {
	switch s {
	case StatusOpen, StatusAcked, StatusClosed:
		return true
	}
	return false
}

// Label returns the Chinese label used by the UI.
func (s AlertStatus) Label() string {
	switch s {
	case StatusOpen:
		return "待处理"
	case StatusAcked:
		return "已确认"
	case StatusClosed:
		return "已关闭"
	}
	return string(s)
}

// Alert is one device alarm event. All records in this project are synthetic.
type Alert struct {
	ID          string      `json:"id"`
	DeviceID    string      `json:"device_id"`
	Type        AlertType   `json:"type"`
	Title       string      `json:"title"`
	Severity    Severity    `json:"severity"`
	Status      AlertStatus `json:"status"`
	Message     string      `json:"message"`
	Metric      string      `json:"metric,omitempty"`
	Value       *float64    `json:"value,omitempty"`
	Unit        string      `json:"unit,omitempty"`
	Threshold   *float64    `json:"threshold,omitempty"`
	Occurrences int         `json:"occurrences"`
	FirstSeen   time.Time   `json:"first_seen"`
	LastSeen    time.Time   `json:"last_seen"`
	CreatedAt   time.Time   `json:"created_at"`
	AckedBy     string      `json:"acked_by,omitempty"`
	AckedAt     *time.Time  `json:"acked_at,omitempty"`
	ClosedAt    *time.Time  `json:"closed_at,omitempty"`
	OTATarget   string      `json:"ota_target,omitempty"`
	RuleID      string      `json:"rule_id,omitempty"`
}

// Device is the asset that emitted an alert.
type Device struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Model    string    `json:"model"`
	Site     string    `json:"site"`
	Firmware string    `json:"firmware"`
	Status   string    `json:"status"`
	Battery  *int      `json:"battery,omitempty"`
	LastSeen time.Time `json:"last_seen"`
	Tags     []string  `json:"tags"`
}

// Analysis is the structured LLM triage result for one alert.
type Analysis struct {
	AlertID        string    `json:"alert_id"`
	Summary        string    `json:"summary"`
	Severity       Severity  `json:"severity"`
	SeverityReason string    `json:"severity_reason"`
	LikelyCauses   []string  `json:"likely_causes"`
	Actions        []string  `json:"actions"`
	Confidence     float64   `json:"confidence"`
	Model          string    `json:"model"`
	Mock           bool      `json:"mock"`
	LatencyMS      int64     `json:"latency_ms"`
	GeneratedAt    time.Time `json:"generated_at"`
}
