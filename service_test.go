package main

import (
	"sync"
	"testing"
	"time"
)

func TestLifecycle(t *testing.T) {
	s := NewService(2)
	s.Start()

	job, _ := s.Submit("test")

	time.Sleep(1 * time.Second)

	j, _ := s.Get(job.ID)

	if j.Status != StatusCompleted {
		t.Fatalf("expected completed got %v", j.Status)
	}
}

func TestConcurrency(t *testing.T) {
	s := NewService(4)
	s.Start()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Submit("test")
		}()
	}
	wg.Wait()

	time.Sleep(2 * time.Second)

	for _, j := range s.List() {
		if j.Status == StatusPending {
			t.Fatal("pending job")
		}
	}
}

func TestCancel(t *testing.T) {
	s := NewService(1)
	s.Start()

	job, _ := s.Submit("test")

	time.Sleep(100 * time.Millisecond)
	s.Cancel(job.ID)

	time.Sleep(1 * time.Second)

	j, _ := s.Get(job.ID)
	if j.Status == StatusCompleted {
		t.Fatal("cancelled job completed")
	}
}
