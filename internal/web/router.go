package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/domain"
	"github.com/devyunmu/iot-alert-agent/internal/service"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Deps are the collaborators required by the HTTP layer.
type Deps struct {
	Service  *service.Service
	Analyzer agent.Analyzer
	Mock     bool
}

// NewRouter builds the Gin engine with templates and routes registered.
func NewRouter(d Deps) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.Use(securityHeaders)

	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	r.StaticFS("/static", http.FS(staticFS))

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"ts":          timestamp,
		"sevLabel":    func(s domain.Severity) string { return s.Label() },
		"typeLabel":   func(t domain.AlertType) string { return t.Label() },
		"statusLabel": func(s domain.AlertStatus) string { return s.Label() },
		"valueText":   valueText,
		"pct":         func(f float64) int { return int(f*100 + 0.5) },
		"battery": func(b *int) string {
			if b == nil {
				return "市电"
			}
			return itoa(*b) + "%"
		},
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	r.SetHTMLTemplate(tmpl)

	h := &handlers{deps: d}
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.HTML(http.StatusNotFound, "error.html", h.baseData("页面不存在"))
	})
	r.GET("/", h.index)
	r.GET("/alerts/:id", h.detail)
	r.POST("/alerts/:id/ack", h.ack)
	r.POST("/alerts/:id/analyze", h.analyzePage)

	r.GET("/healthz", h.health)
	api := r.Group("/api")
	{
		api.GET("/alerts", h.apiList)
		api.GET("/alerts/:id", h.apiDetail)
		api.POST("/alerts/:id/ack", h.apiAck)
		api.POST("/alerts/:id/analyze", h.apiAnalyze)
		api.GET("/stats", h.apiStats)
	}
	return r, nil
}

func securityHeaders(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	c.Header("Referrer-Policy", "no-referrer")
	c.Next()
}

func timestamp(t time.Time) string {
	return t.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
}

func valueText(v service.View) string {
	a := v.Alert
	if a.Value == nil {
		return "—"
	}
	s := itoaFloat(*a.Value)
	if a.Threshold != nil {
		return s + a.Unit + " / 阈值 " + itoaFloat(*a.Threshold) + a.Unit
	}
	return s + a.Unit
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func itoaFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// storeFilter builds a validated filter from query parameters.
func storeFilter(c *gin.Context) store.Filter {
	f := store.Filter{
		Query: strings.TrimSpace(c.Query("q")),
	}
	if t := domain.AlertType(c.Query("type")); t.Valid() {
		f.Type = t
	}
	if s := domain.Severity(c.Query("severity")); s.Valid() {
		f.Severity = s
	}
	if s := domain.AlertStatus(c.Query("status")); s.Valid() {
		f.Status = s
	}
	if len(f.Query) > 80 {
		f.Query = f.Query[:80]
	}
	return f
}
