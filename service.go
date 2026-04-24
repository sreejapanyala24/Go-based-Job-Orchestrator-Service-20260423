package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

var (
	ErrNotFound       = errors.New("job not found")
	ErrInvalidJobType = errors.New("job type is required")
	ErrShuttingDown   = errors.New("service is shutting down")
	ErrInvalidState   = errors.New("invalid job state transition")
	ErrQueueFull      = errors.New("job queue is full")
)

type Job struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Service struct {
	mu       sync.RWMutex
	jobs     map[string]Job
	cancel   map[string]context.CancelFunc
	queue    chan string
	ready    bool
	ctx      context.Context
	stop     context.CancelFunc
	workers  sync.WaitGroup
	sequence uint64

	workerCount int
	jobDuration time.Duration
}

func NewService(workerCount int, queueSize int, jobDuration time.Duration) *Service {
	if workerCount <= 0 {
		workerCount = 1
	}
	if queueSize <= 0 {
		queueSize = 100
	}
	if jobDuration <= 0 {
		jobDuration = 200 * time.Millisecond
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		jobs:        make(map[string]Job),
		cancel:      make(map[string]context.CancelFunc),
		queue:       make(chan string, queueSize),
		ready:       true,
		ctx:         ctx,
		stop:        cancel,
		workerCount: workerCount,
		jobDuration: jobDuration,
	}

	for i := 0; i < workerCount; i++ {
		s.workers.Add(1)
		go s.worker(i + 1)
	}

	return s
}

func (s *Service) SubmitJob(jobType string) (Job, error) {
	if jobType == "" {
		return Job{}, ErrInvalidJobType
	}

	now := time.Now().UTC()
	id := s.nextID()
	job := Job{ID: id, Type: jobType, Status: StatusPending, CreatedAt: now, UpdatedAt: now}

	s.mu.Lock()
	if !s.ready {
		s.mu.Unlock()
		return Job{}, ErrShuttingDown
	}
	s.jobs[id] = job
	s.mu.Unlock()

	select {
	case s.queue <- id:
		log.Printf("job created id=%s type=%s", id, jobType)
		return job, nil
	default:
		s.mu.Lock()
		delete(s.jobs, id)
		s.mu.Unlock()
		return Job{}, ErrQueueFull
	}
}

func (s *Service) GetJob(id string) (Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return job, nil
}

func (s *Service) ListJobs() []Job {
	s.mu.RLock()
	jobs := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.mu.RUnlock()

	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	return jobs
}

func (s *Service) CancelJob(id string) (Job, error) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return Job{}, ErrNotFound
	}
	if isTerminal(job.Status) {
		s.mu.Unlock()
		return Job{}, ErrInvalidState
	}
	job.Status = StatusCancelled
	job.UpdatedAt = time.Now().UTC()
	s.jobs[id] = job
	cancel := s.cancel[id]
	delete(s.cancel, id)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	log.Printf("job cancelled id=%s", id)
	return job, nil
}

func (s *Service) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.ready {
		s.mu.Unlock()
		return nil // Already shutting down
	}
	s.ready = false
	s.mu.Unlock()
	log.Printf("shutdown started")

	// Signal all workers to stop
	s.stop()

	// Wait for all workers to finish
	done := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Printf("shutdown completed")
		return nil
	case <-ctx.Done():
		log.Printf("shutdown timeout")
		return ctx.Err()
	}
}

func (s *Service) worker(workerNumber int) {
	defer s.workers.Done()
	workerID := fmt.Sprintf("worker-%d", workerNumber)
	for {
		select {
		case <-s.ctx.Done():
			return
		case jobID, ok := <-s.queue:
			if !ok {
				// Queue closed
				return
			}
			jobCtx, ready := s.markRunning(jobID)
			if !ready {
				// Job was cancelled, already processed, or service shutting down
				// Don't process this job
				continue
			}
			log.Printf("job started id=%s worker=%s", jobID, workerID)
			err := s.execute(jobCtx)
			if err != nil {
				s.markFailed(jobID, err)
				continue
			}
			s.markCompleted(jobID)
		}
	}
}

func (s *Service) execute(ctx context.Context) error {
	stages := 5
	stageDuration := s.jobDuration / time.Duration(stages)
	if stageDuration <= 0 {
		stageDuration = time.Millisecond
	}
	for i := 0; i < stages; i++ {
		timer := time.NewTimer(stageDuration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-time.After(stageDuration):
		}
	}
	return nil
}

func (s *Service) markRunning(id string) (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok || job.Status != StatusPending {
		// Job was already processed or cancelled
		return nil, false
	}
	job.Status = StatusRunning
	job.UpdatedAt = time.Now().UTC()
	// FIX: Use s.ctx as parent so shutdown signal propagates
	jobCtx, cancel := context.WithCancel(s.ctx)
	s.cancel[id] = cancel
	s.jobs[id] = job
	return jobCtx, true
}

func (s *Service) markCompleted(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok || job.Status != StatusRunning {
		return false
	}
	job.Status = StatusCompleted
	job.UpdatedAt = time.Now().UTC()
	s.jobs[id] = job
	delete(s.cancel, id)
	log.Printf("job completed id=%s", id)
	return true
}

func (s *Service) markFailed(id string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok || job.Status != StatusRunning {
		return false
	}
	job.Status = StatusFailed
	job.UpdatedAt = time.Now().UTC()
	s.jobs[id] = job
	delete(s.cancel, id)
	log.Printf("job failed id=%s error=%v", id, err)
	return true
}

func (s *Service) nextID() string {
	n := atomic.AddUint64(&s.sequence, 1)
	return fmt.Sprintf("job-%d", n)
}

func isTerminal(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}
