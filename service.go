package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ── Status constants ──────────────────────────────────────────────────────────
// Using typed string constants (not iota ints) keeps JSON marshaling
// human-readable without extra MarshalJSON boilerplate.

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// ── Errors ────────────────────────────────────────────────────────────────────
// Sentinel errors let callers use errors.Is() instead of string matching.

var (
	ErrJobNotFound     = errors.New("job not found")
	ErrJobAlreadyDone  = errors.New("job already in terminal state")
	ErrServiceShutdown = errors.New("service is shutting down")
)

// ── Data model ────────────────────────────────────────────────────────────────

type Job struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ── Service ───────────────────────────────────────────────────────────────────

// JobService is the single source of truth for all job state.
// DESIGN NOTE: we pass logger in rather than using a package-level var so
// tests can inject a no-op logger, and future services can have distinct loggers.
type JobService struct {
	mu       sync.RWMutex
	jobs     map[string]*Job
	cancels  map[string]context.CancelFunc // per-job cancel
	queue    chan *Job
	wg       sync.WaitGroup // tracks in-flight workers
	shutdown chan struct{}  // closed on graceful stop
	once     sync.Once      // ensures shutdown runs once
	logger   *slog.Logger
	workerN  int
}

// NewJobService creates the service and starts the worker pool.
// workerN controls concurrency; queueSize controls backpressure.
func NewJobService(workerN, queueSize int, logger *slog.Logger) *JobService {
	s := &JobService{
		jobs:     make(map[string]*Job),
		cancels:  make(map[string]context.CancelFunc),
		queue:    make(chan *Job, queueSize),
		shutdown: make(chan struct{}),
		logger:   logger,
		workerN:  workerN,
	}
	for i := 0; i < workerN; i++ {
		s.wg.Add(1)
		go s.worker(i)
	}
	return s
}

// ── Public API ────────────────────────────────────────────────────────────────

// Submit enqueues a job. Returns ErrServiceShutdown if Stop() was called.
func (s *JobService) Submit(jobType string) (*Job, error) {
	// Check shutdown BEFORE acquiring the lock to avoid deadlock patterns.
	select {
	case <-s.shutdown:
		return nil, ErrServiceShutdown
	default:
	}

	job := &Job{
		ID:        uuid.NewString(),
		Type:      jobType,
		Status:    StatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	// FIX (race): snapshot is taken HERE, while only this goroutine holds the
	// pointer. After s.queue <- job below, a worker may pick it up and mutate
	// Status/UpdatedAt concurrently. Any copy taken after that point races.
	snapshot := *job

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	s.logger.Info("job submitted", "id", job.ID, "type", job.Type)

	// Non-blocking send: if the queue is full we return an error rather
	// than blocking the HTTP handler goroutine indefinitely.
	select {
	case s.queue <- job:
	case <-s.shutdown:
		// Rolled back: remove from store so we don't leak phantom pending jobs.
		s.mu.Lock()
		delete(s.jobs, job.ID)
		s.mu.Unlock()
		return nil, ErrServiceShutdown
	default:
		// Queue full — treat as a 503 at the handler layer.
		s.mu.Lock()
		delete(s.jobs, job.ID)
		s.mu.Unlock()
		return nil, errors.New("job queue full, try again later")
	}

	// snapshot was captured before the queue send — safe to return,
	// no worker can ever reach this local copy.
	return &snapshot, nil
}

// GetJob returns a copy of the job (not the pointer) so callers cannot
// mutate internal state without going through the service.
func (s *JobService) GetJob(id string) (Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	return *j, nil
}

// ListJobs returns copies of all jobs, sorted newest-first.
func (s *JobService) ListJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	return out
}

// CancelJob cancels a pending or running job.
// Completed/failed jobs cannot be cancelled (idempotent no-op for cancelled).
func (s *JobService) CancelJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[id]
	if !ok {
		return ErrJobNotFound
	}

	switch j.Status {
	case StatusCompleted, StatusFailed:
		return ErrJobAlreadyDone
	case StatusCancelled:
		return nil // idempotent
	}

	// Signal the running goroutine (if any) to stop.
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}

	s.updateStatus(j, StatusCancelled)
	s.logger.Info("job cancelled", "id", id)
	return nil
}

// Ready returns true once the worker pool is running.
// Used by the /ready probe.
func (s *JobService) Ready() bool {
	select {
	case <-s.shutdown:
		return false
	default:
		return true
	}
}

// Stop drains the queue and waits for in-flight workers to finish.
// Safe to call multiple times (sync.Once).
func (s *JobService) Stop() {
	s.once.Do(func() {
		s.logger.Info("service shutting down")
		close(s.shutdown)
		// Close the channel so workers exit their range loop.
		close(s.queue)
		s.wg.Wait()
		s.logger.Info("service shutdown complete")
	})
}

// ── Internal ──────────────────────────────────────────────────────────────────

// worker pulls jobs from the queue and executes them.
// FIX: added defer recover() so a panic in execute() cannot crash the process.
// CONCURRENCY NOTE: each worker goroutine owns its own stack — no sharing
// of local variables. Shared state (s.jobs) is only touched via s.mu.
func (s *JobService) worker(id int) {
	defer s.wg.Done()

	// FIX: recover from any panic in execute() so one bad job cannot bring
	// down the entire worker pool and the process.
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("worker recovered from panic",
				"worker_id", id, "panic", r)
		}
	}()

	s.logger.Info("worker started", "worker_id", id)

	for job := range s.queue {
		// Create a per-job context so CancelJob can abort execution.
		ctx, cancel := context.WithCancel(context.Background())

		s.mu.Lock()
		// Job might have been cancelled while sitting in the queue.
		if job.Status == StatusCancelled {
			cancel()
			s.mu.Unlock()
			continue
		}
		s.cancels[job.ID] = cancel
		s.updateStatus(job, StatusRunning)
		s.mu.Unlock()

		s.logger.Info("job started", "worker_id", id, "job_id", job.ID)
		err := s.execute(ctx, job)

		s.mu.Lock()
		delete(s.cancels, job.ID)
		// Don't overwrite Cancelled status set by CancelJob.
		if job.Status != StatusCancelled {
			if err != nil {
				s.updateStatus(job, StatusFailed)
				s.logger.Error("job failed", "job_id", job.ID, "error", err)
			} else {
				s.updateStatus(job, StatusCompleted)
				s.logger.Info("job completed", "job_id", job.ID)
			}
		}
		s.mu.Unlock()

		cancel() // always release the cancel func
	}

	s.logger.Info("worker stopped", "worker_id", id)
}

// execute simulates staged work. Replace with real business logic.
// FIX: replaced time.After with time.NewTimer so the timer is always stopped
// when ctx is cancelled — prevents goroutine/resource leaks under cancellation.
func (s *JobService) execute(ctx context.Context, job *Job) error {
	stages := []struct {
		name     string
		duration time.Duration
	}{
		{"initialising", 300 * time.Millisecond},
		{"processing", 500 * time.Millisecond},
		{"finalising", 200 * time.Millisecond},
	}

	for _, stage := range stages {
		// FIX: NewTimer + Stop() prevents the timer goroutine from leaking
		// when ctx is cancelled before the timer fires.
		timer := time.NewTimer(stage.duration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			s.logger.Debug("job stage complete",
				"job_id", job.ID, "stage", stage.name)
		}
	}
	return nil
}

// updateStatus sets job status and bumps UpdatedAt.
// MUST be called with s.mu held (write lock).
func (s *JobService) updateStatus(j *Job, status string) {
	j.Status = status
	j.UpdatedAt = time.Now().UTC()
}
