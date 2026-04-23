package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func setupHandlerTest(t *testing.T) (*Handlers, *JobService) {
	t.Helper()
	svc := newTestService(t) // reuse helper from service_test.go
	h := NewHandlers(svc, testLogger(t))
	return h, svc
}

func TestHandlerSubmit_Valid(t *testing.T) {
	h, _ := setupHandlerTest(t)

	body := `{"type":"build"}`
	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.SubmitJob(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("want 201, got %d", rec.Code)
	}
	var job Job
	json.NewDecoder(rec.Body).Decode(&job)
	if job.ID == "" {
		t.Error("expected job ID in response")
	}
	if job.Status != StatusPending {
		t.Errorf("want status %q, got %q", StatusPending, job.Status)
	}
}

func TestHandlerSubmit_MissingType(t *testing.T) {
	h, _ := setupHandlerTest(t)

	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	h.SubmitJob(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestHandlerGetJob_NotFound(t *testing.T) {
	h, _ := setupHandlerTest(t)

	req := httptest.NewRequest(http.MethodGet, "/jobs/does-not-exist", nil)
	// Simulate Go 1.22 path value for unit tests.
	req.SetPathValue("id", "does-not-exist")
	rec := httptest.NewRecorder()
	h.GetJob(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestHandlerHealth(t *testing.T) {
	h, _ := setupHandlerTest(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.Health(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestHandlerCancelJob_Conflict(t *testing.T) {
	h, svc := setupHandlerTest(t)

	// Submit and wait for completion.
	job, _ := svc.Submit("quick")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := svc.GetJob(job.ID)
		if j.Status == StatusCompleted {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	req := httptest.NewRequest(http.MethodDelete, "/jobs/"+job.ID, nil)
	req.SetPathValue("id", job.ID)
	rec := httptest.NewRecorder()
	h.CancelJob(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", rec.Code)
	}
}
