package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// ── Logger ─────────────────────────────────────────────────────────────────
	// slog (stdlib since 1.21) — structured, levelled, zero dependencies.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Service ────────────────────────────────────────────────────────────────
	const (
		workerCount = 5
		queueSize   = 100
	)
	svc := NewJobService(workerCount, queueSize, logger)

	// ── Routes ─────────────────────────────────────────────────────────────────
	// Go 1.22 ServeMux supports method+path patterns natively.
	h := NewHandlers(svc, logger)
	mux := http.NewServeMux()

	mux.HandleFunc("POST /jobs", h.SubmitJob)
	mux.HandleFunc("GET /jobs", h.ListJobs)
	mux.HandleFunc("GET /jobs/{id}", h.GetJob)
	mux.HandleFunc("DELETE /jobs/{id}", h.CancelJob)
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /ready", h.Ready)

	addr := envOr("ADDR", ":8080")
	srv := &http.Server{
		Addr:         addr,
		Handler:      h.LoggingMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── Graceful shutdown ──────────────────────────────────────────────────────
	// Listen for SIGINT/SIGTERM in a dedicated goroutine.
	// We use a buffered channel (size 1) so the signal isn't dropped if we're
	// momentarily busy before calling signal.Notify.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("signal received, shutting down", "signal", sig)

		// 1. Stop accepting new jobs.
		svc.Stop()

		// 2. Stop the HTTP server with a deadline for in-flight requests.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("server shutdown error", "error", err)
		}
	}()

	logger.Info("server starting", "addr", addr, "workers", workerCount)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
