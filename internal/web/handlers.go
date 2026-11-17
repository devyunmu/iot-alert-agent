package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/service"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

type handlers struct {
	deps Deps
}

type option struct {
	Value string
	Label string
}

type event struct {
	At     time.Time
	Label  string
	Detail string
}

type pageData struct {
	Title       string
	Mock        bool
	Model       string
	Stats       store.Stats
	Filter      store.Filter
	Alerts      []service.View
	Alert       service.View
	Events      []event
	Error       string
	Notice      string
	TypeOptions []option
	SevOptions  []option
	StatusOpts  []option
	Now         time.Time
}

var (
	allTypes = []option{
		{string(domain.TypeOffline), domain.TypeOffline.Label()},
		{string(domain.TypeLowBattery), domain.TypeLowBattery.Label()},
		{string(domain.TypeSensorAnomaly), domain.TypeSensorAnomaly.Label()},
		{string(domain.TypeOTAFailure), domain.TypeOTAFailure.Label()},
		{string(domain.TypeCommFlap), domain.TypeCommFlap.Label()},
	}
	allSeverities = []option{
		{string(domain.SeverityCritical), domain.SeverityCritical.Label()},
		{string(domain.SeverityWarning), domain.SeverityWarning.Label()},
		{string(domain.SeverityInfo), domain.SeverityInfo.Label()},
	}
	allStatuses = []option{
		{string(domain.StatusOpen), domain.StatusOpen.Label()},
		{string(domain.StatusAcked), domain.StatusAcked.Label()},
		{string(domain.StatusClosed), domain.StatusClosed.Label()},
	}
)

func (h *handlers) baseData(title string) pageData {
	return pageData{
		Title:       title,
		Mock:        h.deps.Mock,
		Model:       h.deps.Analyzer.Describe(),
		Stats:       h.deps.Service.Stats(),
		TypeOptions: allTypes,
		SevOptions:  allSeverities,
		StatusOpts:  allStatuses,
		Now:         time.Now(),
	}
}

func (h *handlers) index(c *gin.Context) {
	d := h.baseData("告警处置台")
	d.Filter = storeFilter(c)
	d.Alerts = h.deps.Service.List(d.Filter)
	switch c.Query("ok") {
	case "acked":
		d.Notice = "告警已确认"
	case "analyzed":
		d.Notice = "AI 处置建议已生成"
	}
	c.HTML(http.StatusOK, "index.html", d)
}

func (h *handlers) detail(c *gin.Context) {
	d := h.baseData("告警详情")
	v, err := h.deps.Service.Detail(c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.HTML(http.StatusNotFound, "error.html", d)
			return
		}
		d.Error = "读取告警失败"
		c.HTML(http.StatusInternalServerError, "error.html", d)
		return
	}
	d.Alert = v
	d.Events = buildTimeline(v)
	switch c.Query("ok") {
	case "acked":
		d.Notice = "告警已确认"
	case "analyzed":
		d.Notice = "AI 处置建议已生成"
	}
	c.HTML(http.StatusOK, "detail.html", d)
}

func (h *handlers) ack(c *gin.Context) {
	operator := strings.TrimSpace(c.PostForm("operator"))
	if operator == "" {
		operator = "web-console"
	}
	if len([]rune(operator)) > 40 {
		operator = string([]rune(operator)[:40])
	}
	_, _, err := h.deps.Service.Ack(c.Param("id"), operator)
	if err != nil {
		h.renderDetailError(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/alerts/"+c.Param("id")+"?ok=acked")
}

func (h *handlers) analyzePage(c *gin.Context) {
	force := c.Query("force") == "1"
	if _, err := h.deps.Service.Analyze(c.Request.Context(), c.Param("id"), force); err != nil {
		h.renderDetailError(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/alerts/"+c.Param("id")+"?ok=analyzed")
}

func (h *handlers) renderDetailError(c *gin.Context, err error) {
	d := h.baseData("告警详情")
	v, getErr := h.deps.Service.Detail(c.Param("id"))
	if getErr != nil {
		d.Error = "读取告警失败"
		c.HTML(http.StatusNotFound, "error.html", d)
		return
	}
	d.Alert = v
	d.Events = buildTimeline(v)
	switch {
	case errors.Is(err, store.ErrClosed):
		d.Error = "该告警已关闭，无法执行此操作"
		c.HTML(http.StatusConflict, "detail.html", d)
	case errors.Is(err, store.ErrNotFound):
		d.Error = "告警不存在"
		c.HTML(http.StatusNotFound, "detail.html", d)
	default:
		d.Error = "AI 分析失败：" + err.Error()
		c.HTML(http.StatusBadGateway, "detail.html", d)
	}
}

func buildTimeline(v service.View) []event {
	out := []event{
		{At: v.CreatedAt, Label: "告警触发", Detail: "规则 " + v.RuleID + " 命中"},
	}
	if v.AckedAt != nil {
		out = append(out, event{At: *v.AckedAt, Label: "人工确认", Detail: "操作人 " + v.AckedBy})
	}
	if v.ClosedAt != nil {
		out = append(out, event{At: *v.ClosedAt, Label: "告警关闭", Detail: "现场已处置"})
	}
	if v.Analysis != nil {
		out = append(out, event{
			At:     v.Analysis.GeneratedAt,
			Label:  "AI 分诊",
			Detail: "模型 " + v.Analysis.Model,
		})
	}
	return out
}

func (h *handlers) health(c *gin.Context) {
	alerts, devices := h.deps.Service.Counts()
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"alerts":  alerts,
		"devices": devices,
		"mock":    h.deps.Mock,
		"model":   h.deps.Analyzer.Describe(),
	})
}

func (h *handlers) apiList(c *gin.Context) {
	f := storeFilter(c)
	f.Limit = queryInt(c, "limit", 0, 200)
	views := h.deps.Service.List(f)
	c.JSON(http.StatusOK, gin.H{"count": len(views), "alerts": views})
}

func (h *handlers) apiDetail(c *gin.Context) {
	v, err := h.deps.Service.Detail(c.Param("id"))
	if err != nil {
		apiError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

type ackBody struct {
	Operator string `json:"operator"`
}

func (h *handlers) apiAck(c *gin.Context) {
	var body ackBody
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
			return
		}
	}
	op := strings.TrimSpace(body.Operator)
	if op == "" {
		op = "api-client"
	}
	if len([]rune(op)) > 40 {
		op = string([]rune(op)[:40])
	}
	a, changed, err := h.deps.Service.Ack(c.Param("id"), op)
	if err != nil {
		apiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"alert": a, "changed": changed})
}

type analyzeBody struct {
	Force bool `json:"force"`
}

func (h *handlers) apiAnalyze(c *gin.Context) {
	var body analyzeBody
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
			return
		}
	}
	an, err := h.deps.Service.Analyze(c.Request.Context(), c.Param("id"), body.Force)
	if err != nil {
		apiError(c, err)
		return
	}
	c.JSON(http.StatusOK, an)
}

func (h *handlers) apiStats(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"stats": h.deps.Service.Stats(),
		"model": h.deps.Analyzer.Describe(),
		"mock":  h.deps.Mock,
	})
}

func apiError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, store.ErrClosed):
		c.JSON(http.StatusConflict, gin.H{"error": "alert is closed"})
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "AI analysis timed out"})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	}
}

func queryInt(c *gin.Context, key string, def, max int) int {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}
