// Package store provides a small in-memory alert store backed by the embedded
// synthetic dataset. It is deliberately dependency-free: a demo should start in
// one command, and the concurrency-safe accessors mirror a real repository layer.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devyunmu/iot-alert-agent/internal/domain"
)

var (
	// ErrNotFound is returned when an alert or device does not exist.
	ErrNotFound = errors.New("not found")
	// ErrClosed is returned when an alert is already closed and cannot be acked.
	ErrClosed = errors.New("alert is closed")
)

// Filter narrows an alert listing. Zero values mean "no constraint".
type Filter struct {
	Query    string
	Type     domain.AlertType
	Severity domain.Severity
	Status   domain.AlertStatus
	DeviceID string
	Limit    int
}

// Store is an in-memory, goroutine-safe repository for alerts and devices.
type Store struct {
	mu       sync.RWMutex
	alerts   map[string]domain.Alert
	devices  map[string]domain.Device
	order    []string
	analyses map[string]domain.Analysis
	loadedAt time.Time
}

type alertsFile struct {
	Alerts []domain.Alert `json:"alerts"`
}

type devicesFile struct {
	Devices []domain.Device `json:"devices"`
}

// New parses the embedded JSON payloads. It fails loudly on malformed data so a
// broken build is caught by tests rather than at demo time.
func New(alertsJSON, devicesJSON []byte) (*Store, error) {
	var af alertsFile
	if err := json.Unmarshal(alertsJSON, &af); err != nil {
		return nil, fmt.Errorf("parse alerts: %w", err)
	}
	var df devicesFile
	if err := json.Unmarshal(devicesJSON, &df); err != nil {
		return nil, fmt.Errorf("parse devices: %w", err)
	}

	s := &Store{
		alerts:   make(map[string]domain.Alert, len(af.Alerts)),
		devices:  make(map[string]domain.Device, len(df.Devices)),
		order:    make([]string, 0, len(af.Alerts)),
		analyses: make(map[string]domain.Analysis),
		loadedAt: time.Now(),
	}
	for _, d := range df.Devices {
		if d.ID == "" {
			return nil, errors.New("device with empty id")
		}
		if _, dup := s.devices[d.ID]; dup {
			return nil, fmt.Errorf("duplicate device id %q", d.ID)
		}
		s.devices[d.ID] = d
	}
	for _, a := range af.Alerts {
		if a.ID == "" {
			return nil, errors.New("alert with empty id")
		}
		if !a.Type.Valid() {
			return nil, fmt.Errorf("alert %s: unknown type %q", a.ID, a.Type)
		}
		if !a.Severity.Valid() {
			return nil, fmt.Errorf("alert %s: unknown severity %q", a.ID, a.Severity)
		}
		if !a.Status.Valid() {
			return nil, fmt.Errorf("alert %s: unknown status %q", a.ID, a.Status)
		}
		if _, ok := s.devices[a.DeviceID]; !ok {
			return nil, fmt.Errorf("alert %s references unknown device %q", a.ID, a.DeviceID)
		}
		if _, dup := s.alerts[a.ID]; dup {
			return nil, fmt.Errorf("duplicate alert id %q", a.ID)
		}
		s.alerts[a.ID] = a
		s.order = append(s.order, a.ID)
	}
	return s, nil
}

// ListAlerts returns matching alerts ordered by severity, then recency.
func (s *Store) ListAlerts(f Filter) []domain.Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.Alert, 0, len(s.order))
	q := strings.ToLower(strings.TrimSpace(f.Query))
	for _, id := range s.order {
		a := s.alerts[id]
		if f.Type != "" && a.Type != f.Type {
			continue
		}
		if f.Severity != "" && a.Severity != f.Severity {
			continue
		}
		if f.Status != "" && a.Status != f.Status {
			continue
		}
		if f.DeviceID != "" && a.DeviceID != f.DeviceID {
			continue
		}
		if q != "" && !matchesQuery(a, s.devices[a.DeviceID], q) {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity.Rank() != out[j].Severity.Rank() {
			return out[i].Severity.Rank() > out[j].Severity.Rank()
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

func matchesQuery(a domain.Alert, d domain.Device, q string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		a.ID, a.Title, a.Message, a.DeviceID, d.Name, d.Site, string(a.Type),
	}, " "))
	return strings.Contains(haystack, q)
}

// GetAlert returns one alert by id.
func (s *Store) GetAlert(id string) (domain.Alert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.alerts[id]
	if !ok {
		return domain.Alert{}, fmt.Errorf("alert %q: %w", id, ErrNotFound)
	}
	return a, nil
}

// GetDevice returns one device by id.
func (s *Store) GetDevice(id string) (domain.Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[id]
	if !ok {
		return domain.Device{}, fmt.Errorf("device %q: %w", id, ErrNotFound)
	}
	return d, nil
}

// AckAlert marks an alert as acknowledged. It is idempotent: a second ack
// returns the existing record with changed=false. Closed alerts cannot be acked.
func (s *Store) AckAlert(id, operator string) (domain.Alert, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.alerts[id]
	if !ok {
		return domain.Alert{}, false, fmt.Errorf("alert %q: %w", id, ErrNotFound)
	}
	if a.Status == domain.StatusClosed {
		return domain.Alert{}, false, fmt.Errorf("alert %q: %w", id, ErrClosed)
	}
	if a.Status == domain.StatusAcked {
		return a, false, nil
	}
	if operator == "" {
		operator = "unknown"
	}
	now := time.Now().UTC()
	a.Status = domain.StatusAcked
	a.AckedBy = operator
	a.AckedAt = &now
	s.alerts[id] = a
	return a, true, nil
}

// SaveAnalysis caches an LLM triage result for an alert.
func (s *Store) SaveAnalysis(a domain.Analysis) error {
	if a.AlertID == "" {
		return errors.New("analysis has empty alert_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.alerts[a.AlertID]; !ok {
		return fmt.Errorf("alert %q: %w", a.AlertID, ErrNotFound)
	}
	s.analyses[a.AlertID] = a
	return nil
}

// GetAnalysis returns a cached analysis, if any.
func (s *Store) GetAnalysis(alertID string) (domain.Analysis, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.analyses[alertID]
	return a, ok
}

// Stats summarizes the current alert board.
type Stats struct {
	Total    int            `json:"total"`
	Open     int            `json:"open"`
	Acked    int            `json:"acked"`
	Closed   int            `json:"closed"`
	Critical int            `json:"critical"`
	ByType   map[string]int `json:"by_type"`
}

// Stats computes counters for the dashboard header.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{Total: len(s.order), ByType: make(map[string]int)}
	for _, id := range s.order {
		a := s.alerts[id]
		st.ByType[string(a.Type)]++
		switch a.Status {
		case domain.StatusOpen:
			st.Open++
		case domain.StatusAcked:
			st.Acked++
		case domain.StatusClosed:
			st.Closed++
		}
		if a.Severity == domain.SeverityCritical && a.Status != domain.StatusClosed {
			st.Critical++
		}
	}
	return st
}

// Counts returns alert and device counts, useful for startup logs.
func (s *Store) Counts() (alerts, devices int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order), len(s.devices)
}
