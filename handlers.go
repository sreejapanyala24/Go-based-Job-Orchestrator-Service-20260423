package main

import (
	"encoding/json"
	"net/http"
)

type Handler struct {
	svc *Service
}

func NewHandler(s *Service) *Handler {
	return &Handler{svc: s}
}

func (h *Handler) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", h.health)
	mux.HandleFunc("/ready", h.ready)

	mux.HandleFunc("/jobs", h.jobs)
	mux.HandleFunc("/jobs/", h.jobByID)

	return mux
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) jobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.submit(w, r)
	case http.MethodGet:
		h.list(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) jobByID(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/jobs/"):]
	switch r.Method {
	case http.MethodGet:
		h.get(w, r, id)
	case http.MethodDelete:
		h.cancel(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Ready() {
		http.Error(w, "service shutting down", http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req struct {
		Type string `json:"type"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Type == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	job, err := h.svc.Submit(req.Type)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	writeJSON(w, http.StatusCreated, job)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.List())
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, id string) {
	job, ok := h.svc.Get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request, id string) {
	err := h.svc.Cancel(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
