package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/liufengxi/dbguard/internal/agentgateway"
)

func main() {
	logger := log.New(os.Stdout, "[agent-gateway] ", log.LstdFlags|log.Lmicroseconds|log.LUTC)
	cfg, err := agentgateway.ConfigFromEnvironment()
	if err != nil {
		logger.Fatalf("configuration error: %v", err)
	}
	gateway, err := agentgateway.New(cfg, logger)
	if err != nil {
		logger.Fatalf("initialize gateway: %v", err)
	}
	defer gateway.Close()

	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           gateway.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      cfg.UpstreamTimeout + 15*time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          logger,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		logger.Printf("version=%s listen=%s upstream=%s", agentgateway.Version, cfg.ListenAddress, cfg.UpstreamURL.Redacted())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("server stopped unexpectedly: %v", err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("graceful shutdown failed: %v", err)
	}
}
