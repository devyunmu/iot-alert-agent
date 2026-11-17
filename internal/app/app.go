// Package app wires configuration, embedded data, analyzer and service together
// for both the HTTP server and the MCP server entrypoints.
package app

import (
	"fmt"
	"log"

	"github.com/devyunmu/iot-alert-agent/data"
	"github.com/devyunmu/iot-alert-agent/internal/agent"
	"github.com/devyunmu/iot-alert-agent/internal/config"
	"github.com/devyunmu/iot-alert-agent/internal/llm"
	"github.com/devyunmu/iot-alert-agent/internal/service"
	"github.com/devyunmu/iot-alert-agent/internal/store"
)

// App is the assembled dependency graph.
type App struct {
	Config   config.Config
	Store    *store.Store
	Analyzer agent.Analyzer
	Service  *service.Service
}

// New loads the embedded synthetic dataset and selects the analyzer based on
// environment configuration. Without an API key it falls back to the rule-based
// mock analyzer instead of failing to start.
func New() (*App, error) {
	cfg := config.Load()

	alertsJSON, err := data.FS.ReadFile("alerts.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded alerts: %w", err)
	}
	devicesJSON, err := data.FS.ReadFile("devices.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded devices: %w", err)
	}
	st, err := store.New(alertsJSON, devicesJSON)
	if err != nil {
		return nil, fmt.Errorf("load dataset: %w", err)
	}

	var analyzer agent.Analyzer
	if cfg.Mock {
		analyzer = agent.NewMock()
	} else {
		client := llm.New(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Timeout, cfg.MaxTokens, cfg.Temperature)
		analyzer = agent.NewLLM(client)
	}

	return &App{
		Config:   cfg,
		Store:    st,
		Analyzer: analyzer,
		Service:  service.New(st, analyzer, cfg.Timeout),
	}, nil
}

// LogStartup prints non-sensitive configuration for operators.
func (a *App) LogStartup() {
	alerts, devices := a.Store.Counts()
	log.Printf("[app] loaded %d alerts / %d devices (synthetic)", alerts, devices)
	if a.Analyzer.Mock() {
		log.Printf("[app] analyzer=mock source=rules (no LLM_API_KEY)")
		return
	}
	log.Printf("[app] analyzer=llm model=%s base_url=%s", a.Config.Model, a.Config.BaseURL)
}
