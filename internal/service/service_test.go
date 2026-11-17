package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devyunmu/iot-alert-agent/data"
	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/llm"
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

// blockingAnalyzer lets a test hold a flight open and decide when it finishes.
type blockingAnalyzer struct {
	calls   int32
	entered chan struct{}
	release chan struct{}
}

func newBlockingAnalyzer() *blockingAnalyzer {
	return &blockingAnalyzer{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (b *blockingAnalyzer) Analyze(ctx context.Context, a domain.Alert, _ domain.Device) (domain.Analysis, error) {
	atomic.AddInt32(&b.calls, 1)
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return domain.Analysis{
			AlertID: a.ID, Summary: "blocked result", Severity: a.Severity,
			Actions: []string{"a"}, Model: "blocking", GeneratedAt: time.Now().UTC(),
		}, nil
	case <-ctx.Done():
		return domain.Analysis{}, ctx.Err()
	}
}

func (b *blockingAnalyzer) Describe() string { return "blocking" }
func (b *blockingAnalyzer) Mock() bool       { return true }

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

// TestAnalyzeSingleflightCollapsesBurst is the S2 regression: six concurrent
// requests for the same cold alert must produce exactly one upstream call, and
// every caller must observe the same analysis.
func TestAnalyzeSingleflightCollapsesBurst(t *testing.T) {
	const reply = `{"summary":"冷库温度超限","severity":"warning","severity_reason":"r","actions":["a"],"confidence":0.7}`
	var upstreamCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&upstreamCalls, 1)
		time.Sleep(100 * time.Millisecond) // keep the flight open long enough to join
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": reply}}},
		})
	}))
	defer srv.Close()

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
	an := agent.NewLLM(llm.New(srv.URL, "sk-test", "test-model", 5*time.Second, 500, 0))
	svc := New(st, an, 5*time.Second)

	const callers = 6
	results := make([]domain.Analysis, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.Analyze(context.Background(), "ALT-0001", false)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&upstreamCalls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 for %d concurrent cold requests", got, callers)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].Summary != results[0].Summary || results[i].Model != "test-model" {
			t.Fatalf("caller %d got a different result: %+v", i, results[i])
		}
	}
	// The cache now serves further requests with no upstream traffic.
	if _, err := svc.Analyze(context.Background(), "ALT-0001", false); err != nil {
		t.Fatalf("warm analyze: %v", err)
	}
	if got := atomic.LoadInt32(&upstreamCalls); got != 1 {
		t.Errorf("warm cache caused another upstream call: %d", got)
	}
}

func newServiceWith(t *testing.T, an agent.Analyzer) *Service {
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
	return New(st, an, 2*time.Second)
}

// TestForceDoesNotJoinInFlightNonForcedCall is the S-1 regression: a forced
// refresh arriving while a normal request is in flight must reach the analyzer
// itself, not silently receive the in-flight (cache-serving) result.
func TestForceDoesNotJoinInFlightNonForcedCall(t *testing.T) {
	an := newBlockingAnalyzer()
	svc := newServiceWith(t, an)

	done := make(chan error, 2)
	go func() {
		_, err := svc.Analyze(context.Background(), "ALT-0001", false)
		done <- err
	}()
	<-an.entered // normal flight is now running

	go func() {
		_, err := svc.Analyze(context.Background(), "ALT-0001", true)
		done <- err
	}()
	select {
	case <-an.entered:
	case <-time.After(time.Second):
		t.Fatal("forced request joined the non-forced flight instead of calling upstream")
	}

	close(an.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&an.calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one normal + one forced)", got)
	}
}

// TestCancelledLeaderDoesNotCancelFollower is the S-2 regression: one caller
// disconnecting (browser tab closed, MCP timeout) must not fail everyone else
// waiting on the same shared call.
func TestCancelledLeaderDoesNotCancelFollower(t *testing.T) {
	an := newBlockingAnalyzer()
	svc := newServiceWith(t, an)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := svc.Analyze(leaderCtx, "ALT-0001", false)
		leaderErr <- err
	}()
	<-an.entered

	followerErr := make(chan error, 1)
	go func() {
		_, err := svc.Analyze(context.Background(), "ALT-0001", false)
		followerErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the follower join the flight
	cancelLeader()

	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want context.Canceled", err)
	}
	close(an.release)
	if err := <-followerErr; err != nil {
		t.Fatalf("follower inherited the leader's cancellation: %v", err)
	}
	if got := atomic.LoadInt32(&an.calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (follower must join, not re-call)", got)
	}
}

// TestWaiterHonoursOwnContext proves a waiter can give up on its own request
// while the shared flight keeps running for the others.
func TestWaiterHonoursOwnContext(t *testing.T) {
	an := newBlockingAnalyzer()
	svc := newServiceWith(t, an)

	leaderErr := make(chan error, 1)
	go func() {
		_, err := svc.Analyze(context.Background(), "ALT-0001", false)
		leaderErr <- err
	}()
	<-an.entered

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := svc.Analyze(waiterCtx, "ALT-0001", false)
		waiterErr <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancelWaiter()

	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored its own cancellation")
	}
	close(an.release)
	if err := <-leaderErr; err != nil {
		t.Fatalf("leader failed after waiter left: %v", err)
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
	if _, _, err := svc.Close("ALT-0001"); err != nil {
		t.Fatalf("close after ack: %v", err)
	}
	if v, err := svc.Detail("ALT-0001"); err != nil || v.Status != domain.StatusClosed || v.ClosedAt == nil {
		t.Fatalf("detail after close: %+v err=%v", v.Alert, err)
	}
	if _, _, err := svc.Ack("ALT-0015", "tester"); !errors.Is(err, store.ErrClosed) {
		t.Errorf("closed ack err = %v", err)
	}
	if _, _, err := svc.Close("ALT-0005"); !errors.Is(err, store.ErrNotAcked) {
		t.Errorf("close open alert err = %v", err)
	}

	list := svc.List(store.Filter{Type: domain.TypeOffline, Limit: 3})
	if len(list) != 3 || list[0].Device.ID == "" {
		t.Errorf("list = %d", len(list))
	}
	if alerts, devices := svc.Counts(); alerts != 30 || devices != 12 {
		t.Errorf("counts = %d/%d", alerts, devices)
	}
}
