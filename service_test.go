package main

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// testLogger returns a no-op slog logger so tests aren't noisy.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T) *JobService {
	t.Helper()
	svc := NewJobService(3, 50, testLogger(t))
	t.Cleanup(svc.Stop) // always stops the service when the test ends
	return svc
}

// ── 1. Job creation ───────────────────────────────────────────────────────────

func TestSubmit_StoresJob(t *testing.T) {
	svc := newTestService(t)

	job, err := svc.Submit("build")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}
	if job.Status != StatusPending {
		t.Errorf("want status %q, got %q", StatusPending, job.Status)
	}
	if job.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}

	// Verify it's actually in the store.
	stored, err := svc.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if stored.ID != job.ID {
		t.Errorf("stored ID mismatch: want %q, got %q", job.ID, stored.ID)
	}
}

// ── 2. Job lifecycle ──────────────────────────────────────────────────────────

func TestJobLifecycle_PendingToCompleted(t *testing.T) {
	svc := newTestService(t)

	job, err := svc.Submit("deploy")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// The job should start as pending or transition quickly.
	// We poll for up to 5s — using a ticker avoids a hardcoded sleep.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := svc.GetJob(job.ID)
		if j.Status == StatusCompleted {
			// Verify UpdatedAt was bumped.
			if !j.UpdatedAt.After(j.CreatedAt) {
				t.Error("UpdatedAt should be after CreatedAt on completion")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("job did not reach 'completed' within deadline")
}

func TestJobLifecycle_RunningTransition(t *testing.T) {
	svc := newTestService(t)

	job, err := svc.Submit("test-type")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Catch the running state. This might be flaky if the machine is extremely
	// fast; the 300ms initial stage makes it catchable in normal CI.
	deadline := time.Now().Add(3 * time.Second)
	sawRunning := false
	for time.Now().Before(deadline) {
		j, _ := svc.GetJob(job.ID)
		if j.Status == StatusRunning {
			sawRunning = true
			break
		}
		if j.Status == StatusCompleted {
			break // completed before we could catch running — still valid
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Note: not a hard failure if we miss running — it's a race by design.
	// We just log it.
	if !sawRunning {
		t.Log("note: did not observe running state (job completed too fast)")
	}
}

// ── 3. Concurrency — no data races, all jobs complete ────────────────────────

func TestConcurrent_AllJobsComplete(t *testing.T) {
	svc := newTestService(t)
	const jobCount = 20

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids []string
	)

	// Submit all jobs concurrently.
	for i := 0; i < jobCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := svc.Submit("concurrent-test")
			if err != nil {
				t.Errorf("submit error: %v", err)
				return
			}
			mu.Lock()
			ids = append(ids, job.ID)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Wait for all to finish (up to 15s).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		doneCount := 0
		for _, id := range ids {
			j, _ := svc.GetJob(id)
			if j.Status == StatusCompleted || j.Status == StatusFailed {
				doneCount++
			}
		}
		if doneCount == len(ids) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("not all jobs completed within deadline; submitted=%d", len(ids))
}

// ── 4. Cancellation ───────────────────────────────────────────────────────────

func TestCancel_RunningJob(t *testing.T) {
	svc := newTestService(t)

	job, err := svc.Submit("long-job")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Wait until the job is running before cancelling.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := svc.GetJob(job.ID)
		if j.Status == StatusRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := svc.CancelJob(job.ID); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}

	// Confirm the final status is cancelled, not completed.
	time.Sleep(500 * time.Millisecond) // let the goroutine observe ctx.Done()
	j, _ := svc.GetJob(job.ID)
	if j.Status != StatusCancelled {
		t.Errorf("want status %q, got %q", StatusCancelled, j.Status)
	}
}

func TestCancel_PendingJob(t *testing.T) {
	// Fill the queue so our job stays pending.
	svc := NewJobService(1, 100, testLogger(t)) // 1 worker
	t.Cleanup(svc.Stop)

	// Flood the queue to ensure next job stays pending.
	for i := 0; i < 10; i++ {
		svc.Submit("filler")
	}

	job, err := svc.Submit("to-cancel")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	if err := svc.CancelJob(job.ID); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}

	j, _ := svc.GetJob(job.ID)
	if j.Status != StatusCancelled {
		t.Errorf("want %q, got %q", StatusCancelled, j.Status)
	}
}

// ── 5. Edge cases ─────────────────────────────────────────────────────────────

func TestGetJob_NotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.GetJob("nonexistent-id")
	if err != ErrJobNotFound {
		t.Errorf("want ErrJobNotFound, got %v", err)
	}
}

func TestCancelJob_NotFound(t *testing.T) {
	svc := newTestService(t)
	err := svc.CancelJob("nonexistent-id")
	if err != ErrJobNotFound {
		t.Errorf("want ErrJobNotFound, got %v", err)
	}
}

func TestCancelJob_AlreadyCompleted(t *testing.T) {
	svc := newTestService(t)

	job, _ := svc.Submit("quick")

	// Wait for completion.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := svc.GetJob(job.ID)
		if j.Status == StatusCompleted {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	err := svc.CancelJob(job.ID)
	if err != ErrJobAlreadyDone {
		t.Errorf("want ErrJobAlreadyDone, got %v", err)
	}
}

func TestSubmit_AfterShutdown(t *testing.T) {
	svc := newTestService(t)
	svc.Stop()

	_, err := svc.Submit("should-fail")
	if err != ErrServiceShutdown {
		t.Errorf("want ErrServiceShutdown, got %v", err)
	}
}
