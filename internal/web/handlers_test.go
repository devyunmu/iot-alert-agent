package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devyunmu/iot-alert-agent/data"
	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/service"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

func newTestRouter(t *testing.T) (*gin.Engine, *store.Store) {
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
	r, err := NewRouter(Deps{
		Service:  service.New(st, an, 5*time.Second),
		Analyzer: an,
		Mock:     true,
	})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	return r, st
}

func perform(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestIndexPage(t *testing.T) {
	r, _ := newTestRouter(t)
	w := perform(r, http.MethodGet, "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"告警处置台", "ALT-0001", "冷库温湿度传感器-01", "MOCK", "list_alerts"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing security headers")
	}
}

func TestIndexFilters(t *testing.T) {
	r, _ := newTestRouter(t)
	w := perform(r, http.MethodGet, "/?type=offline&severity=critical&status=open", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "库区网关-01 离线") {
		t.Error("expected filtered critical offline alert")
	}
	if strings.Contains(body, "1号冷库回风温度持续偏高") {
		t.Error("sensor anomaly leaked into offline filter")
	}
}

func TestDetailAndAnalyzeFlow(t *testing.T) {
	r, _ := newTestRouter(t)

	w := perform(r, http.MethodGet, "/alerts/ALT-0001", "")
	if w.Code != http.StatusOK {
		t.Fatalf("detail status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"1号冷库回风温度持续偏高", "生成 AI 处置建议", "TH-200", "9.6°C"} {
		if !strings.Contains(body, want) {
			// "制冷机组" only appears after analysis; checked below.
			if want == "制冷机组" {
				continue
			}
			t.Errorf("detail missing %q", want)
		}
	}

	w = perform(r, http.MethodPost, "/alerts/ALT-0001/analyze", "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("analyze status = %d, want 303 (%s)", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/alerts/ALT-0001") {
		t.Errorf("redirect = %q", loc)
	}

	w = perform(r, http.MethodGet, "/alerts/ALT-0001", "")
	body = w.Body.String()
	for _, want := range []string{"AI 处置建议", "规则回退", "可能原因", "建议处置步骤", "制冷机组", "人工测温"} {
		if !strings.Contains(body, want) {
			t.Errorf("analyzed detail missing %q", want)
		}
	}
}

func TestAckFlow(t *testing.T) {
	r, st := newTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/alerts/ALT-0001/ack", strings.NewReader("operator=ops-user"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("ack status = %d", w.Code)
	}
	a, err := st.GetAlert("ALT-0001")
	if err != nil {
		t.Fatalf("get alert: %v", err)
	}
	if a.Status != domain.StatusAcked || a.AckedBy != "ops-user" {
		t.Fatalf("ack not persisted: %+v", a)
	}

	// Closed alerts must return a conflict page, not silently succeed.
	w = perform(r, http.MethodPost, "/alerts/ALT-0015/ack", "")
	if w.Code != http.StatusConflict {
		t.Errorf("closed ack status = %d, want 409", w.Code)
	}
}

func TestCloseFlow(t *testing.T) {
	r, st := newTestRouter(t)

	// The close endpoint only accepts acknowledged alerts.
	w := perform(r, http.MethodPost, "/alerts/ALT-0001/close", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("close open alert status = %d, want 409", w.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/alerts/ALT-0001/ack", strings.NewReader("operator=ops-user"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("ack status = %d", w.Code)
	}

	// Detail page must now offer the close button.
	if w := perform(r, http.MethodGet, "/alerts/ALT-0001", ""); !strings.Contains(w.Body.String(), "关闭告警") {
		t.Error("acked detail page has no close action")
	}

	w = perform(r, http.MethodPost, "/alerts/ALT-0001/close", "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("close status = %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "ok=closed") {
		t.Errorf("redirect = %q", loc)
	}
	a, err := st.GetAlert("ALT-0001")
	if err != nil {
		t.Fatalf("get alert: %v", err)
	}
	if a.Status != domain.StatusClosed || a.ClosedAt == nil {
		t.Fatalf("close not persisted: %+v", a)
	}

	w = perform(r, http.MethodGet, "/alerts/ALT-0001", "")
	body := w.Body.String()
	for _, want := range []string{"已关闭", "告警关闭"} {
		if !strings.Contains(body, want) {
			t.Errorf("closed detail missing %q", want)
		}
	}
}

func TestAPIClose(t *testing.T) {
	r, _ := newTestRouter(t)

	if w := perform(r, http.MethodPost, "/api/alerts/ALT-0003/close", "{}"); w.Code != http.StatusConflict {
		t.Errorf("API close open alert status = %d, want 409", w.Code)
	}

	if w := perform(r, http.MethodPost, "/api/alerts/ALT-0003/ack", `{"operator":"api-ops"}`); w.Code != http.StatusOK {
		t.Fatalf("ack status = %d: %s", w.Code, w.Body.String())
	}
	w := perform(r, http.MethodPost, "/api/alerts/ALT-0003/close", "")
	if w.Code != http.StatusOK {
		t.Fatalf("close status = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Alert   domain.Alert `json:"alert"`
		Changed bool         `json:"changed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if !resp.Changed || resp.Alert.Status != domain.StatusClosed || resp.Alert.ClosedAt == nil {
		t.Fatalf("unexpected close response: %+v", resp)
	}
}

func TestNotFound(t *testing.T) {
	r, _ := newTestRouter(t)
	if w := perform(r, http.MethodGet, "/alerts/ALT-9999", ""); w.Code != http.StatusNotFound {
		t.Errorf("page status = %d", w.Code)
	}
	if w := perform(r, http.MethodGet, "/api/alerts/ALT-9999", ""); w.Code != http.StatusNotFound {
		t.Errorf("api status = %d", w.Code)
	}
	// Regression: integer-valued metrics must not lose trailing zeros.
	w := perform(r, http.MethodGet, "/alerts/ALT-0028", "")
	if !strings.Contains(w.Body.String(), "26分钟 / 阈值 10分钟") {
		t.Errorf("metric formatting lost digits: %s", w.Body.String())
	}
}

func TestAPIAlertsAndStats(t *testing.T) {
	r, _ := newTestRouter(t)

	w := perform(r, http.MethodGet, "/api/alerts?type=offline&limit=2", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var list struct {
		Count  int            `json:"count"`
		Alerts []service.View `json:"alerts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if list.Count != 2 || len(list.Alerts) != 2 {
		t.Fatalf("count = %d len = %d", list.Count, len(list.Alerts))
	}
	for _, a := range list.Alerts {
		if a.Type != domain.TypeOffline || a.Device.Name == "" {
			t.Errorf("unexpected alert: %+v", a)
		}
	}

	w = perform(r, http.MethodGet, "/api/stats", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":30`) {
		t.Errorf("stats response: %s", w.Body.String())
	}

	w = perform(r, http.MethodGet, "/healthz", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mock":true`) {
		t.Errorf("healthz: %s", w.Body.String())
	}
}

func TestAPIAnalyzeAndAck(t *testing.T) {
	r, _ := newTestRouter(t)

	w := perform(r, http.MethodPost, "/api/alerts/ALT-0002/analyze", "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("analyze status = %d: %s", w.Code, w.Body.String())
	}
	var an domain.Analysis
	if err := json.Unmarshal(w.Body.Bytes(), &an); err != nil {
		t.Fatalf("decode analysis: %v", err)
	}
	if an.AlertID != "ALT-0002" || !an.Mock || an.Summary == "" || len(an.Actions) == 0 {
		t.Fatalf("unexpected analysis: %+v", an)
	}

	w = perform(r, http.MethodPost, "/api/alerts/ALT-0006/ack", `{"operator":"api-ops"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ack status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"changed":true`) || !strings.Contains(w.Body.String(), `"api-ops"`) {
		t.Errorf("ack response: %s", w.Body.String())
	}

	w = perform(r, http.MethodPost, "/api/alerts/ALT-0002/analyze", `{"force":"yes"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", w.Code)
	}
}
