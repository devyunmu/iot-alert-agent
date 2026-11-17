// Command server runs the web console and JSON API.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/devyunmu/iot-alert-agent/internal/app"
	"github.com/devyunmu/iot-alert-agent/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	a, err := app.New()
	if err != nil {
		log.Fatalf("[server] init failed: %v", err)
	}
	a.LogStartup()

	router, err := web.NewRouter(web.Deps{
		Service:  a.Service,
		Analyzer: a.Analyzer,
		Mock:     a.Config.Mock,
	})
	if err != nil {
		log.Fatalf("[server] build router: %v", err)
	}

	srv := &http.Server{
		Addr:              a.Config.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      240 * time.Second, // triage calls can take over a minute
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", a.Config.Addr)
	if err != nil {
		log.Fatalf("[server] listen on %s: %v", a.Config.Addr, err)
	}
	go func() {
		log.Printf("[server] listening on http://%s", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[server] serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[server] shutdown: %v", err)
	}
	log.Println("[server] stopped")
}
