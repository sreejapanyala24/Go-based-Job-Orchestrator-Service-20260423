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

func main() {
	port := getEnv("PORT", "8000")
	workerCount := getEnvInt("WORKERS", 4)

	svc := NewService(workerCount)
	svc.Start()

	handler := NewHandler(svc)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler.routes(),
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("server started on %s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutdown initiated")

	// 1. stop HTTP intake first
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelHTTP()
	_ = server.Shutdown(httpCtx)

	// 2. then shutdown service
	svcCtx, cancelSvc := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSvc()
	svc.Shutdown(svcCtx)

	log.Println("shutdown complete")
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}
