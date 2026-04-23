package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

var (
	ErrJobNotFound     = errors.New("job not found")
	ErrJobAlreadyDone  = errors.New("job already in terminal state")
	ErrServiceShutdown = errors.New("service is shutting down")
)

type Job struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type JobService struct {
	mu       sync.RWMutex
	jobs     map[string]*Job
	cancels  map[string]context.CancelFunc
	queue    chan *Job
	wg       sync.WaitGroup
	shutdown chan struct{}
	once     sync.Once
	logger   *slog.Logger
	workerN  int
}

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

func (s *JobService) Submit(jobType string) (*Job, error) {
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

	snapshot := *job

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	s.logger.Info("job submitted", "id", job.ID, "type", job.Type)

	select {
	case s.queue <- job:
	case <-s.shutdown:
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

	return &snapshot, nil
}

func (s *JobService) GetJob(id string) (Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	return *j, nil
}

func (s *JobService) ListJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	slices.SortFunc(out, func(a, b Job) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return out
}

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
		return nil
	}

	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}

	s.updateStatus(j, StatusCancelled)
	s.logger.Info("job cancelled", "id", id)
	return nil
}

func (s *JobService) Ready() bool {
	select {
	case <-s.shutdown:
		return false
	default:
		return true
	}
}

func (s *JobService) Stop() {
	s.once.Do(func() {
		s.logger.Info("service shutting down")
		close(s.shutdown)
		close(s.queue)
		s.wg.Wait()
		s.logger.Info("service shutdown complete")
	})
}

func (s *JobService) worker(id int) {
	defer s.wg.Done()

	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("worker recovered from panic",
				"worker_id", id, "panic", r)
		}
	}()

	s.logger.Info("worker started", "worker_id", id)

	for job := range s.queue {
		ctx, cancel := context.WithCancel(context.Background())

		s.mu.Lock()
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

		cancel()
	}

	s.logger.Info("worker stopped", "worker_id", id)
}

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

func (s *JobService) updateStatus(j *Job, status string) {
	j.Status = status
	j.UpdatedAt = time.Now().UTC()
}
