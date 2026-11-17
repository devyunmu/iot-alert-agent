package store

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/devyunmu/iot-alert-agent/data"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	alerts, err := data.FS.ReadFile("alerts.json")
	if err != nil {
		t.Fatalf("read alerts: %v", err)
	}
	devices, err := data.FS.ReadFile("devices.json")
	if err != nil {
		t.Fatalf("read devices: %v", err)
	}
	s, err := New(alerts, devices)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestDatasetIntegrity(t *testing.T) {
	s := newTestStore(t)
	alerts, devices := s.Counts()
	if alerts != 30 {
		t.Errorf("alerts = %d, want 30", alerts)
	}
	if devices != 12 {
		t.Errorf("devices = %d, want 12", devices)
	}

	seenTypes := map[domain.AlertType]bool{}
	for _, a := range s.ListAlerts(Filter{}) {
		seenTypes[a.Type] = true
		if _, err := s.GetDevice(a.DeviceID); err != nil {
			t.Errorf("alert %s references missing device: %v", a.ID, err)
		}
		if a.Status == domain.StatusAcked && a.AckedBy == "" {
			t.Errorf("alert %s is acked without operator", a.ID)
		}
	}
	for _, typ := range []domain.AlertType{
		domain.TypeOffline, domain.TypeLowBattery, domain.TypeSensorAnomaly,
		domain.TypeOTAFailure, domain.TypeCommFlap,
	} {
		if !seenTypes[typ] {
			t.Errorf("dataset does not cover alert type %q", typ)
		}
	}
}

// datasetClock returns the scenario "now" declared by the fixtures. Every
// timeline field must be relative to it; the prompt uses the same instant.
func datasetClock(t *testing.T) time.Time {
	t.Helper()
	raw, err := data.FS.ReadFile("alerts.json")
	if err != nil {
		t.Fatalf("read alerts: %v", err)
	}
	var head struct {
		GeneratedAt time.Time `json:"generated_at"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatalf("decode generated_at: %v", err)
	}
	return head.GeneratedAt
}

// TestDatasetTimelineConsistency is the regression for S4: every timestamp in the
// fixtures must sit on one consistent timeline (no ack/heartbeat after "now",
// no closed status without closed_at, no duration that contradicts the fields).
func TestDatasetTimelineConsistency(t *testing.T) {
	s := newTestStore(t)
	now := datasetClock(t)
	if now.IsZero() {
		t.Fatal("dataset has no generated_at")
	}

	shortForm := regexp.MustCompile(`(?:离线|中断)\s*(\d+)\s*分钟`)
	longForm := regexp.MustCompile(`(?:离线|中断)\s*(\d+)\s*小时\s*(\d+)\s*分钟`)

	for _, a := range s.ListAlerts(Filter{}) {
		if a.LastSeen.Before(a.FirstSeen) {
			t.Errorf("%s: last_seen before first_seen", a.ID)
		}
		if a.CreatedAt.After(now) || a.FirstSeen.After(now) || a.LastSeen.After(now) {
			t.Errorf("%s: timestamp after dataset now %s", a.ID, now.Format(time.RFC3339))
		}
		if a.AckedAt != nil {
			if a.AckedAt.Before(a.CreatedAt) || a.AckedAt.After(now) {
				t.Errorf("%s: acked_at outside [created_at, now]", a.ID)
			}
		}
		if a.ClosedAt != nil {
			if a.ClosedAt.After(now) {
				t.Errorf("%s: closed_at after now", a.ID)
			}
			if a.AckedAt != nil && a.ClosedAt.Before(*a.AckedAt) {
				t.Errorf("%s: closed_at before acked_at", a.ID)
			}
		}
		if a.Status == domain.StatusAcked && a.AckedAt == nil {
			t.Errorf("%s: acked status without acked_at", a.ID)
		}
		if a.Status == domain.StatusClosed {
			if a.ClosedAt == nil {
				t.Errorf("%s: closed status without closed_at", a.ID)
			}
			if a.AckedAt == nil {
				t.Errorf("%s: closed status without acked_at", a.ID)
			}
		}
		if a.Status != domain.StatusClosed && a.ClosedAt != nil {
			t.Errorf("%s: closed_at set while status is %s", a.ID, a.Status)
		}

		if a.Type != domain.TypeOffline {
			continue
		}
		// Offline alerts follow one convention: first_seen is the last heartbeat,
		// last_seen is the evaluation instant, and the device agrees with both.
		if !a.LastSeen.Equal(now) {
			t.Errorf("%s: offline alert last_seen %s != now %s", a.ID, a.LastSeen.Format(time.RFC3339), now.Format(time.RFC3339))
		}
		d, err := s.GetDevice(a.DeviceID)
		if err != nil {
			t.Errorf("%s: device: %v", a.ID, err)
			continue
		}
		if !d.LastSeen.Equal(a.FirstSeen) {
			t.Errorf("%s: device last_seen %s != alert first_seen %s", a.ID, d.LastSeen.Format(time.RFC3339), a.FirstSeen.Format(time.RFC3339))
		}
		if d.Status != "offline" {
			t.Errorf("%s: offline alert on device with status %q", a.ID, d.Status)
		}

		var stated time.Duration
		if m := longForm.FindStringSubmatch(a.Message); m != nil {
			h, _ := strconv.Atoi(m[1])
			min, _ := strconv.Atoi(m[2])
			stated = time.Duration(h)*time.Hour + time.Duration(min)*time.Minute
		} else if m := shortForm.FindStringSubmatch(a.Message); m != nil {
			min, _ := strconv.Atoi(m[1])
			stated = time.Duration(min) * time.Minute
		} else {
			t.Errorf("%s: offline message has no parsable duration: %q", a.ID, a.Message)
			continue
		}
		if got := now.Sub(a.FirstSeen); got != stated {
			t.Errorf("%s: message says %s offline, fields say %s", a.ID, stated, got)
		}
	}
}

func TestCloseLifecycle(t *testing.T) {
	s := newTestStore(t)

	// open → closed must be rejected: acknowledgement carries the audit trail.
	if _, _, err := s.CloseAlert("ALT-0001"); !errors.Is(err, ErrNotAcked) {
		t.Fatalf("close open alert err = %v, want ErrNotAcked", err)
	}
	if got, err := s.GetAlert("ALT-0001"); err != nil || got.Status != domain.StatusOpen || got.ClosedAt != nil {
		t.Fatalf("rejected close mutated alert: %+v err=%v", got, err)
	}

	// acked → closed stamps ClosedAt and is idempotent.
	if _, _, err := s.AckAlert("ALT-0001", "alice"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	got, changed, err := s.CloseAlert("ALT-0001")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !changed || got.Status != domain.StatusClosed || got.ClosedAt == nil {
		t.Fatalf("unexpected close result: %+v changed=%v", got, changed)
	}
	if got.ClosedAt.Before(*got.AckedAt) {
		t.Errorf("closed_at %s before acked_at %s", got.ClosedAt, got.AckedAt)
	}
	again, changed, err := s.CloseAlert("ALT-0001")
	if err != nil {
		t.Fatalf("second close: %v", err)
	}
	if changed || again.ClosedAt == nil || !again.ClosedAt.Equal(*got.ClosedAt) {
		t.Fatalf("close should be idempotent: %+v changed=%v", again, changed)
	}

	// A pre-closed alert stays a no-op; a missing one is not found.
	if a, changed, err := s.CloseAlert("ALT-0015"); err != nil || changed || a.Status != domain.StatusClosed {
		t.Fatalf("pre-closed alert: %+v changed=%v err=%v", a, changed, err)
	}
	if _, _, err := s.CloseAlert("ALT-9999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing alert close err = %v, want ErrNotFound", err)
	}
}

func TestListOrderAndFilters(t *testing.T) {
	s := newTestStore(t)

	all := s.ListAlerts(Filter{})
	if len(all) != 30 {
		t.Fatalf("list all = %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if prev.Severity.Rank() < cur.Severity.Rank() {
			t.Fatalf("severity order broken at %d: %s before %s", i, prev.Severity, cur.Severity)
		}
		if prev.Severity.Rank() == cur.Severity.Rank() && cur.CreatedAt.After(prev.CreatedAt) {
			t.Fatalf("recency order broken at %d", i)
		}
	}

	crit := s.ListAlerts(Filter{Severity: domain.SeverityCritical})
	if len(crit) == 0 {
		t.Fatal("expected critical alerts")
	}
	for _, a := range crit {
		if a.Severity != domain.SeverityCritical {
			t.Errorf("severity filter leaked %s", a.Severity)
		}
	}

	open := s.ListAlerts(Filter{Status: domain.StatusOpen})
	for _, a := range open {
		if a.Status != domain.StatusOpen {
			t.Errorf("status filter leaked %s", a.Status)
		}
	}

	offline := s.ListAlerts(Filter{Type: domain.TypeOffline})
	if len(offline) != 7 {
		t.Errorf("offline count = %d, want 7", len(offline))
	}

	dev := s.ListAlerts(Filter{DeviceID: "DEV-1004"})
	if len(dev) != 3 {
		t.Errorf("DEV-1004 alerts = %d, want 3", len(dev))
	}

	limited := s.ListAlerts(Filter{Limit: 5})
	if len(limited) != 5 {
		t.Errorf("limit = %d, want 5", len(limited))
	}

	search := s.ListAlerts(Filter{Query: "烟感"})
	if len(search) == 0 {
		t.Error("expected search hit for 烟感")
	}
	if got := s.ListAlerts(Filter{Query: "不存在的设备"}); len(got) != 0 {
		t.Errorf("unexpected results: %d", len(got))
	}
}

func TestAckLifecycle(t *testing.T) {
	s := newTestStore(t)

	got, changed, err := s.AckAlert("ALT-0001", "alice")
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if !changed || got.Status != domain.StatusAcked || got.AckedBy != "alice" || got.AckedAt == nil {
		t.Fatalf("unexpected ack result: %+v changed=%v", got, changed)
	}

	again, changed, err := s.AckAlert("ALT-0001", "bob")
	if err != nil {
		t.Fatalf("second ack: %v", err)
	}
	if changed || again.AckedBy != "alice" {
		t.Fatalf("ack should be idempotent, got %+v changed=%v", again, changed)
	}

	if _, _, err := s.AckAlert("ALT-0015", "alice"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed alert ack err = %v, want ErrClosed", err)
	}
	if _, _, err := s.AckAlert("ALT-9999", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing alert ack err = %v, want ErrNotFound", err)
	}

	// Empty operator must not produce an empty audit field.
	got, _, err = s.AckAlert("ALT-0003", "")
	if err != nil {
		t.Fatalf("empty operator ack: %v", err)
	}
	if got.AckedBy != "unknown" {
		t.Errorf("operator = %q, want unknown", got.AckedBy)
	}
}

func TestAnalysisCache(t *testing.T) {
	s := newTestStore(t)
	an := domain.Analysis{AlertID: "ALT-0003", Summary: "test", Severity: domain.SeverityWarning, Actions: []string{"check"}}
	if err := s.SaveAnalysis(an); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok := s.GetAnalysis("ALT-0003")
	if !ok || got.Summary != "test" {
		t.Fatalf("cache miss: %+v ok=%v", got, ok)
	}
	if err := s.SaveAnalysis(domain.Analysis{AlertID: "ALT-9999"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("save for unknown alert = %v", err)
	}
	if err := s.SaveAnalysis(domain.Analysis{}); err == nil {
		t.Fatal("expected error for empty alert_id")
	}
}

func TestStats(t *testing.T) {
	s := newTestStore(t)
	st := s.Stats()
	if st.Total != 30 || st.Open+st.Acked+st.Closed != 30 {
		t.Fatalf("inconsistent stats: %+v", st)
	}
	if st.ByType[string(domain.TypeOffline)] != 7 {
		t.Errorf("offline stat = %d", st.ByType[string(domain.TypeOffline)])
	}
	if st.Critical == 0 {
		t.Error("expected critical count")
	}
}

func TestNewRejectsInvalidData(t *testing.T) {
	validDevices := `{"devices":[{"id":"DEV-1","name":"d"}]}`
	tests := []struct {
		name    string
		alerts  string
		devices string
	}{
		{"malformed alerts", `{`, validDevices},
		{"malformed devices", `{"alerts":[]}`, `{`},
		{"unknown type", `{"alerts":[{"id":"A1","device_id":"DEV-1","type":"exploded","severity":"info","status":"open"}]}`, validDevices},
		{"unknown severity", `{"alerts":[{"id":"A1","device_id":"DEV-1","type":"offline","severity":"spicy","status":"open"}]}`, validDevices},
		{"unknown status", `{"alerts":[{"id":"A1","device_id":"DEV-1","type":"offline","severity":"info","status":"openish"}]}`, validDevices},
		{"missing device", `{"alerts":[{"id":"A1","device_id":"DEV-404","type":"offline","severity":"info","status":"open"}]}`, validDevices},
		{"duplicate alert", `{"alerts":[` +
			`{"id":"A1","device_id":"DEV-1","type":"offline","severity":"info","status":"open"},` +
			`{"id":"A1","device_id":"DEV-1","type":"offline","severity":"info","status":"open"}]}`, validDevices},
		{"empty alert id", `{"alerts":[{"id":"","device_id":"DEV-1","type":"offline","severity":"info","status":"open"}]}`, validDevices},
		{"duplicate device", `{"alerts":[]}`, `{"devices":[{"id":"DEV-1"},{"id":"DEV-1"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New([]byte(tc.alerts), []byte(tc.devices)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
