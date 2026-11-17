// Command mcp runs the Model Context Protocol server over stdio. stdout is
// reserved for the protocol; all logs go to stderr.
package main

import (
	"log"
	"os"

	"github.com/devyunmu/iot-alert-agent/internal/app"
	"github.com/devyunmu/iot-alert-agent/internal/mcpserver"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	a, err := app.New()
	if err != nil {
		log.Fatalf("[mcp] init failed: %v", err)
	}
	a.LogStartup()

	s := mcpserver.New(mcpserver.Deps{
		Store:    a.Store,
		Service:  a.Service,
		Analyzer: a.Analyzer,
	})
	log.Printf("[mcp] serving %s over stdio", mcpserver.ServerName())
	if err := mcpserver.Serve(s); err != nil {
		log.Fatalf("[mcp] serve: %v", err)
	}
}
