package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func main() {
	workers := envOrInt("WORKERS", 4)
	queueSize := envOrInt("QUEUE_SIZE", 100)
	jobDuration := time.Duration(envOrInt("JOB_DURATION_MS", 2000)) * time.Millisecond
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8000"
	}

	svc := NewService(workers, queueSize, jobDuration)
	h := NewHandler(svc)

	server := &http.Server{
		Addr:              addr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("server listening addr=%s", server.Addr)
		serverErr <- server.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case s := <-sig:
		log.Printf("signal received signal=%s", s)
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := svc.Shutdown(shutdownCtx); err != nil {
		log.Printf("service shutdown error: %v", err)
	}
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown error: %v", err)
	}
}
