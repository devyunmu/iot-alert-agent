// Package service contains use cases shared by the web UI, JSON API and MCP
// tools: listing alerts with their device, acknowledging, and cached triage.
package service

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/llm"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

// View joins an alert with its device and (optionally) cached analysis.
type View struct {
	domain.Alert
	Device   domain.Device    `json:"device"`
	Analysis *domain.Analysis `json:"analysis,omitempty"`
}

// Service orchestrates the store and the analyzer.
type Service struct {
	store    *store.Store
	analyzer agent.Analyzer
	timeout  time.Duration
	// group collapses concurrent triage requests for the same alert into one
	// upstream call. Without it a cold cache plus a burst of requests produced
	// N identical /chat/completions calls and last-writer-wins cache entries.
	group singleflight.Group
}

// New builds a Service. timeout bounds a single upstream attempt; a flight gets
// timeout*llm.MaxAttempts so the client's retries never eat the first attempt's
// budget (previously both were the same value and a retry had no time left).
func New(st *store.Store, an agent.Analyzer, timeout time.Duration) *Service {
	return &Service{store: st, analyzer: an, timeout: timeout}
}

// flightBudget is the deadline for one shared upstream call, including retries.
func (s *Service) flightBudget() time.Duration {
	if s.timeout <= 0 {
		return 0
	}
	return s.timeout * time.Duration(llm.MaxAttempts)
}

// Analyzer exposes the configured analyzer (used for UI labels).
func (s *Service) Analyzer() agent.Analyzer { return s.analyzer }

// List returns alert views filtered by f, ordered by urgency.
func (s *Service) List(f store.Filter) []View {
	alerts := s.store.ListAlerts(f)
	out := make([]View, 0, len(alerts))
	for _, a := range alerts {
		d, err := s.store.GetDevice(a.DeviceID)
		if err != nil {
			// Referential integrity is validated at load time, so this only
			// happens if data is corrupted at runtime; keep listing rather than
			// failing the whole board.
			d = domain.Device{ID: a.DeviceID, Name: "未知设备"}
		}
		v := View{Alert: a, Device: d}
		if an, ok := s.store.GetAnalysis(a.ID); ok {
			copied := an
			v.Analysis = &copied
		}
		out = append(out, v)
	}
	return out
}

// Detail returns one alert view including any cached analysis.
func (s *Service) Detail(id string) (View, error) {
	a, err := s.store.GetAlert(id)
	if err != nil {
		return View{}, err
	}
	d, err := s.store.GetDevice(a.DeviceID)
	if err != nil {
		return View{}, err
	}
	v := View{Alert: a, Device: d}
	if an, ok := s.store.GetAnalysis(a.ID); ok {
		copied := an
		v.Analysis = &copied
	}
	return v, nil
}

// Ack acknowledges an alert on behalf of an operator.
func (s *Service) Ack(id, operator string) (domain.Alert, bool, error) {
	return s.store.AckAlert(id, operator)
}

// Close moves an acknowledged alert to closed and stamps ClosedAt, so every
// alert can reach the terminal state the board and timeline display.
func (s *Service) Close(id string) (domain.Alert, bool, error) {
	return s.store.CloseAlert(id)
}

// Analyze returns cached triage unless force is set. Only validated analyses are
// cached, so a bad model response never poisons subsequent requests.
//
// Concurrent requests share one in-flight call, keyed by alert id *and* the
// force flag: a forced refresh must not be silently collapsed into a flight
// that is serving cached data (and vice versa). Waiters wait on their own
// context, while the shared call is detached from the leader with
// context.WithoutCancel — one browser tab closing must not cancel an upstream
// call that other callers are still waiting for.
func (s *Service) Analyze(ctx context.Context, id string, force bool) (domain.Analysis, error) {
	if !force {
		if cached, ok := s.store.GetAnalysis(id); ok {
			return cached, nil
		}
	}
	key := id
	if force {
		key += "|force"
	}
	ch := s.group.DoChan(key, func() (any, error) {
		// A previous flight may have filled the cache between the check above
		// and Do; re-reading here avoids a duplicate call in that window.
		if !force {
			if cached, ok := s.store.GetAnalysis(id); ok {
				return cached, nil
			}
		}
		// Detach the shared call from any single caller's cancellation, then
		// bound it with the retry-aware budget.
		flightCtx := context.WithoutCancel(ctx)
		if budget := s.flightBudget(); budget > 0 {
			var cancel context.CancelFunc
			flightCtx, cancel = context.WithTimeout(flightCtx, budget)
			defer cancel()
		}
		return s.generate(flightCtx, id)
	})
	select {
	case <-ctx.Done():
		return domain.Analysis{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return domain.Analysis{}, res.Err
		}
		an, ok := res.Val.(domain.Analysis)
		if !ok {
			return domain.Analysis{}, fmt.Errorf("analyze %s: unexpected result type %T", id, res.Val)
		}
		return an, nil
	}
}

func (s *Service) generate(ctx context.Context, id string) (domain.Analysis, error) {
	a, err := s.store.GetAlert(id)
	if err != nil {
		return domain.Analysis{}, err
	}
	d, err := s.store.GetDevice(a.DeviceID)
	if err != nil {
		return domain.Analysis{}, fmt.Errorf("load device: %w", err)
	}
	an, err := s.analyzer.Analyze(ctx, a, d)
	if err != nil {
		return domain.Analysis{}, err
	}
	if err := s.store.SaveAnalysis(an); err != nil {
		return domain.Analysis{}, fmt.Errorf("cache analysis: %w", err)
	}
	return an, nil
}

// Stats proxies board counters.
func (s *Service) Stats() store.Stats { return s.store.Stats() }

// Counts returns the number of loaded alerts and devices.
func (s *Service) Counts() (alerts, devices int) { return s.store.Counts() }
