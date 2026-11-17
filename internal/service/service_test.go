package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/devyunmu/iot-alert-agent/data"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

type fakeAnalyzer struct {
	mu    sync.Mutex
	calls int
	err   error
	block bool
}

func (f *fakeAnalyzer) Analyze(ctx context.Context, a domain.Alert, _ domain.Device) (domain.Analysis, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	err := f.err
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return domain.Analysis{}, ctx.Err()
	}
	if err != nil {
		return domain.Analysis{}, err
	}
	return domain.Analysis{
		AlertID: a.ID, Summary: "fake summary", Severity: a.Severity,
		Actions: []string{"do something"}, Model: "fake", GeneratedAt: time.Now().UTC(),
	}, nil
}

func (f *fakeAnalyzer) Describe() string { return "fake" }
func (f *fakeAnalyzer) Mock() bool       { return true }
func (f *fakeAnalyzer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newService(t *testing.T) (*Service, *store.Store, *fakeAnalyzer) {
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
	an := &fakeAnalyzer{}
	return New(st, an, 2*time.Second), st, an
}

func TestAnalyzeCachesUntilForced(t *testing.T) {
	svc, st, an := newService(t)
	ctx := context.Background()

	first, err := svc.Analyze(ctx, "ALT-0001", false)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if an.callCount() != 1 {
		t.Fatalf("calls = %d", an.callCount())
	}
	if cached, ok := st.GetAnalysis("ALT-0001"); !ok || cached.Summary != first.Summary {
		t.Fatal("analysis was not cached")
	}

	second, err := svc.Analyze(ctx, "ALT-0001", false)
	if err != nil {
		t.Fatalf("cached analyze: %v", err)
	}
	if an.callCount() != 1 {
		t.Errorf("cache miss caused extra LLM call: %d", an.callCount())
	}
	if second.Summary != first.Summary {
		t.Error("cached result differs")
	}

	if _, err := svc.Analyze(ctx, "ALT-0001", true); err != nil {
		t.Fatalf("force analyze: %v", err)
	}
	if an.callCount() != 2 {
		t.Errorf("force did not regenerate: %d calls", an.callCount())
	}
}

func TestAnalyzeValidationAndTimeout(t *testing.T) {
	svc, _, an := newService(t)
	ctx := context.Background()

	if _, err := svc.Analyze(ctx, "ALT-9999", false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing alert err = %v", err)
	}

	an.err = errors.New("upstream down")
	if _, err := svc.Analyze(ctx, "ALT-0002", false); err == nil {
		t.Fatal("expected analyzer error")
	}
	// Failures must not be cached: a later attempt must reach the analyzer again.
	an.err = nil
	if _, err := svc.Analyze(ctx, "ALT-0002", false); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if an.callCount() != 2 {
		t.Errorf("failed call was cached, calls = %d", an.callCount())
	}
	an.block = true
	svc.timeout = 20 * time.Millisecond
	if _, err := svc.Analyze(ctx, "ALT-0003", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout err = %v", err)
	}
}

func TestDetailAndAck(t *testing.T) {
	svc, _, _ := newService(t)

	v, err := svc.Detail("ALT-0001")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if v.Device.Name == "" || v.Analysis != nil {
		t.Errorf("unexpected detail: %+v", v)
	}

	a, changed, err := svc.Ack("ALT-0001", "tester")
	if err != nil || !changed || a.AckedBy != "tester" {
		t.Fatalf("ack: %+v changed=%v err=%v", a, changed, err)
	}
	if _, _, err := svc.Ack("ALT-0015", "tester"); !errors.Is(err, store.ErrClosed) {
		t.Errorf("closed ack err = %v", err)
	}

	list := svc.List(store.Filter{Type: domain.TypeOffline, Limit: 3})
	if len(list) != 3 || list[0].Device.ID == "" {
		t.Errorf("list = %d", len(list))
	}
	if alerts, devices := svc.Counts(); alerts != 30 || devices != 12 {
		t.Errorf("counts = %d/%d", alerts, devices)
	}
}
