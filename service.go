package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

type Job struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Service struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	queue    chan string
	workers  int
	ready    bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	sequence uint64
}

func NewService(workers int) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		jobs:    make(map[string]*Job),
		queue:   make(chan string, 100),
		workers: workers,
		ready:   true,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (s *Service) Start() {
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
}

func (s *Service) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *Service) Submit(t string) (*Job, error) {
	s.mu.Lock()
	if !s.ready {
		s.mu.Unlock()
		return nil, errors.New("not accepting jobs")
	}

	id := s.nextID()
	job := &Job{
		ID:        id,
		Type:      t,
		Status:    StatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.jobs[id] = job
	s.mu.Unlock()

	select {
	case s.queue <- id:
	default:
		return nil, errors.New("queue full")
	}

	return copyJob(job), nil
}

func (s *Service) worker() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return
		case id := <-s.queue:
			s.process(id)
		}
	}
}

func (s *Service) process(id string) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok || job.Status != StatusPending || !s.ready {
		s.mu.Unlock()
		return
	}

	job.Status = StatusRunning
	job.UpdatedAt = time.Now()
	s.mu.Unlock()

	log.Printf("job %s running", id)

	select {
	case <-time.After(500 * time.Millisecond):
	case <-s.ctx.Done():
		s.failIfRunning(id)
		return
	}

	s.completeIfRunning(id)
}

func (s *Service) completeIfRunning(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok || job.Status != StatusRunning {
		return
	}
	job.Status = StatusCompleted
	job.UpdatedAt = time.Now()
}

func (s *Service) failIfRunning(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok || job.Status != StatusRunning {
		return
	}
	job.Status = StatusFailed
	job.UpdatedAt = time.Now()
}

func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return errors.New("not found")
	}

	if job.Status == StatusCompleted || job.Status == StatusFailed {
		return errors.New("cannot cancel")
	}

	job.Status = StatusCancelled
	job.UpdatedAt = time.Now()
	return nil
}

func (s *Service) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	return copyJob(job), true
}

func (s *Service) List() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, copyJob(j))
	}
	return out
}

func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()

	s.cancel()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Println("shutdown timeout")
	}
}

func (s *Service) nextID() string {
	n := atomic.AddUint64(&s.sequence, 1)
	return fmt.Sprintf("job-%d", n)
}

func copyJob(j *Job) *Job {
	c := *j
	return &c
}
