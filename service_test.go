package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestService(workers int, duration time.Duration) *Service {
	return NewService(workers, 1000, duration)
}

func waitForStatus(t *testing.T, svc *Service, id string, status string, timeout time.Duration) Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := svc.GetJob(id)
		if err != nil {
			t.Fatalf("GetJob failed: %v", err)
		}
		if job.Status == status {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := svc.GetJob(id)
	t.Fatalf("timed out waiting for status %q, got %q", status, job.Status)
	return Job{}
}

func TestJobCreation(t *testing.T) {
	svc := newTestService(1, 100*time.Millisecond)
	defer svc.Shutdown(context.Background())
	job, err := svc.SubmitJob("backup")
	if err != nil {
		t.Fatalf("SubmitJob failed: %v", err)
	}
	if job.ID == "" || job.Type != "backup" || job.Status != StatusPending {
		t.Fatalf("bad job: %+v", job)
	}
	if job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() {
		t.Fatal("expected timestamps")
	}
	stored, err := svc.GetJob(job.ID)
	if err != nil || stored.ID != job.ID {
		t.Fatalf("expected stored job, got %+v err=%v", stored, err)
	}
}

func TestJobLifecycle(t *testing.T) {
	svc := newTestService(1, 80*time.Millisecond)
	defer svc.Shutdown(context.Background())
	job, err := svc.SubmitJob("deploy")
	if err != nil {
		t.Fatalf("SubmitJob failed: %v", err)
	}
	created := job.UpdatedAt
	running := waitForStatus(t, svc, job.ID, StatusRunning, time.Second)
	if !running.UpdatedAt.After(created) {
		t.Fatal("UpdatedAt did not advance on running")
	}
	completed := waitForStatus(t, svc, job.ID, StatusCompleted, time.Second)
	if !completed.UpdatedAt.After(running.UpdatedAt) {
		t.Fatal("UpdatedAt did not advance on completed")
	}
}

func TestConcurrentSubmissionsCompleteWithoutRace(t *testing.T) {
	svc := newTestService(8, 20*time.Millisecond)
	defer svc.Shutdown(context.Background())
	const count = 100
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := svc.SubmitJob("task")
			if err != nil {
				t.Errorf("SubmitJob failed: %v", err)
				return
			}
			ids <- job.ID
		}()
	}
	wg.Wait()
	close(ids)
	for id := range ids {
		waitForStatus(t, svc, id, StatusCompleted, 3*time.Second)
	}
	if got := len(svc.ListJobs()); got != count {
		t.Fatalf("expected %d jobs, got %d", count, got)
	}
}

func TestCancelRunningJobDoesNotComplete(t *testing.T) {
	svc := newTestService(1, 300*time.Millisecond)
	defer svc.Shutdown(context.Background())
	job, err := svc.SubmitJob("long-running")
	if err != nil {
		t.Fatalf("SubmitJob failed: %v", err)
	}
	waitForStatus(t, svc, job.ID, StatusRunning, time.Second)
	cancelled, err := svc.CancelJob(job.ID)
	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("cancel failed: %+v err=%v", cancelled, err)
	}
	time.Sleep(400 * time.Millisecond)
	final, _ := svc.GetJob(job.ID)
	if final.Status != StatusCancelled {
		t.Fatalf("cancelled job completed: %q", final.Status)
	}
}

func TestNonExistentJobLookup(t *testing.T) {
	svc := newTestService(1, 50*time.Millisecond)
	defer svc.Shutdown(context.Background())
	_, err := svc.GetJob("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCancelCompletedJob(t *testing.T) {
	svc := newTestService(1, 20*time.Millisecond)
	defer svc.Shutdown(context.Background())
	job, _ := svc.SubmitJob("quick")
	waitForStatus(t, svc, job.ID, StatusCompleted, time.Second)
	_, err := svc.CancelJob(job.ID)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
}

func TestShutdownRejectsNewJobsAndLetsRunningFinish(t *testing.T) {
	svc := newTestService(1, 120*time.Millisecond)
	job, err := svc.SubmitJob("drain")
	if err != nil {
		t.Fatalf("SubmitJob failed: %v", err)
	}
	waitForStatus(t, svc, job.ID, StatusRunning, time.Second)
	done := make(chan error, 1)
	go func() { done <- svc.Shutdown(context.Background()) }()
	for i := 0; i < 50 && svc.Ready(); i++ {
		time.Sleep(2 * time.Millisecond)
	}
	if svc.Ready() {
		t.Fatal("expected not ready")
	}
	_, err = svc.SubmitJob("late")
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("expected ErrShuttingDown, got %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	final, _ := svc.GetJob(job.ID)
	if final.Status != StatusCompleted {
		t.Fatalf("running job should finish, got %q", final.Status)
	}
}
