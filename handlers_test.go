package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandlersBasicHTTP(t *testing.T) {
	svc := NewService(1, 10, 20*time.Millisecond)
	defer svc.Shutdown(context.Background())
	h := NewHandler(svc).Routes()
	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"type":"backup"}`))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", res.Code, res.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"type":""}`))
	res = httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/jobs/missing", nil)
	res = httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", res.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	res = httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}
}

func TestReadyAfterShutdown(t *testing.T) {
	svc := NewService(1, 10, 20*time.Millisecond)
	h := NewHandler(svc).Routes()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/ready", nil)
	res = httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.Code)
	}
}
