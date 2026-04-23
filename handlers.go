package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ── Handler dependencies ──────────────────────────────────────────────────────
// Using an interface makes handlers testable without a real JobService.

type jobServicer interface {
	Submit(jobType string) (*Job, error)
	GetJob(id string) (Job, error)
	ListJobs() []Job
	CancelJob(id string) error
	Ready() bool
}

type Handlers struct {
	svc    jobServicer
	logger *slog.Logger
}

func NewHandlers(svc jobServicer, logger *slog.Logger) *Handlers {
	return &Handlers{svc: svc, logger: logger}
}

// ── Request / response types ──────────────────────────────────────────────────

type submitRequest struct {
	Type string `json:"type"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// ── Middleware ────────────────────────────────────────────────────────────────

// LoggingMiddleware logs method, path, status, and latency for every request.
func (h *Handlers) LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		h.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// SubmitJob handles POST /jobs
// FIX: body is capped at 1 MB via MaxBytesReader to prevent memory exhaustion
// from large payloads (DoS protection).
func (h *Handlers) SubmitJob(w http.ResponseWriter, r *http.Request) {
	// FIX: limit request body to 1 MB; anything larger is a 400.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.writeError(w, "request body too large (max 1 MB)", http.StatusRequestEntityTooLarge)
			return
		}
		h.writeError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Type = strings.TrimSpace(req.Type)
	if req.Type == "" {
		h.writeError(w, "field 'type' is required", http.StatusBadRequest)
		return
	}

	job, err := h.svc.Submit(req.Type)
	if err != nil {
		switch {
		case errors.Is(err, ErrServiceShutdown):
			h.writeError(w, "service is shutting down", http.StatusServiceUnavailable)
		default:
			h.writeError(w, err.Error(), http.StatusServiceUnavailable)
		}
		return
	}
	// job is a copy returned by Submit — safe to encode concurrently.
	h.writeJSON(w, job, http.StatusCreated)
}

// ListJobs handles GET /jobs
func (h *Handlers) ListJobs(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, h.svc.ListJobs(), http.StatusOK)
}

// GetJob handles GET /jobs/{id}
func (h *Handlers) GetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id") // requires Go 1.22+
	job, err := h.svc.GetJob(id)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			h.writeError(w, "job not found", http.StatusNotFound)
			return
		}
		h.writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.writeJSON(w, job, http.StatusOK)
}

// CancelJob handles DELETE /jobs/{id}
func (h *Handlers) CancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.svc.CancelJob(id)
	if err != nil {
		switch {
		case errors.Is(err, ErrJobNotFound):
			h.writeError(w, "job not found", http.StatusNotFound)
		case errors.Is(err, ErrJobAlreadyDone):
			h.writeError(w, "job already in terminal state", http.StatusConflict)
		default:
			h.writeError(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Health handles GET /health — always 200 if the process is alive
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
}

// Ready handles GET /ready — 200 if workers are running, 503 if shutting down
func (h *Handlers) Ready(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Ready() {
		h.writeError(w, "service not ready", http.StatusServiceUnavailable)
		return
	}
	h.writeJSON(w, map[string]string{"status": "ready"}, http.StatusOK)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *Handlers) writeJSON(w http.ResponseWriter, v any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("failed to encode response", "error", err)
	}
}

func (h *Handlers) writeError(w http.ResponseWriter, msg string, status int) {
	h.writeJSON(w, errorResponse{Error: msg}, status)
}
