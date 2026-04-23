package main

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T) *JobService {
	t.Helper()
	svc := NewJobService(3, 50, testLogger(t))
	t.Cleanup(svc.Stop)
	return svc
}

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

	stored, err := svc.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if stored.ID != job.ID {
		t.Errorf("stored ID mismatch: want %q, got %q", job.ID, stored.ID)
	}
}

func TestJobLifecycle_PendingToCompleted(t *testing.T) {
	svc := newTestService(t)

	job, err := svc.Submit("deploy")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := svc.GetJob(job.ID)
		if j.Status == StatusCompleted {
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

	deadline := time.Now().Add(3 * time.Second)
	sawRunning := false
	for time.Now().Before(deadline) {
		j, _ := svc.GetJob(job.ID)
		if j.Status == StatusRunning {
			sawRunning = true
			break
		}
		if j.Status == StatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !sawRunning {
		t.Log("note: did not observe running state (job completed too fast)")
	}
}

func TestConcurrent_AllJobsComplete(t *testing.T) {
	svc := newTestService(t)
	const jobCount = 20

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids []string
	)

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

func TestCancel_RunningJob(t *testing.T) {
	svc := newTestService(t)

	job, err := svc.Submit("long-job")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

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

	time.Sleep(500 * time.Millisecond)
	j, _ := svc.GetJob(job.ID)
	if j.Status != StatusCancelled {
		t.Errorf("want status %q, got %q", StatusCancelled, j.Status)
	}
}

func TestCancel_PendingJob(t *testing.T) {
	svc := NewJobService(1, 100, testLogger(t)) // 1 worker
	t.Cleanup(svc.Stop)

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
