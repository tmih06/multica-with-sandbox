package factory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type concurrentRunner struct {
	mu      sync.Mutex
	active  int
	peak    int
	started chan struct{}
	release chan struct{}
}

func (r *concurrentRunner) Run(context.Context, Ticket) (RunResult, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.peak {
		r.peak = r.active
	}
	r.mu.Unlock()
	r.started <- struct{}{}
	<-r.release
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return RunResult{Answer: "done"}, nil
}

func TestServerRunsThreeTicketsConcurrentlyAtCapacityThree(t *testing.T) {
	runner := &concurrentRunner{started: make(chan struct{}, 3), release: make(chan struct{}, 3)}
	server := httptest.NewServer(NewServerWithCapacity(runner, 3).Handler())
	defer server.Close()
	for range 3 {
		response, err := http.Post(server.URL+"/tickets", "application/json", strings.NewReader(`{"title":"parallel","prompt":"run"}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	for range 3 {
		select {
		case <-runner.started:
		case <-time.After(time.Second):
			t.Fatal("ticket did not start concurrently")
		}
	}
	for range 3 {
		runner.release <- struct{}{}
	}
	if runner.peak != 3 {
		t.Fatalf("peak active runners = %d, want 3", runner.peak)
	}
}

func TestServerRunsTwoTicketsConcurrentlyAtCapacityTwo(t *testing.T) {
	runner := &concurrentRunner{started: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	server := httptest.NewServer(NewServerWithCapacity(runner, 2).Handler())
	defer server.Close()
	for range 2 {
		response, err := http.Post(server.URL+"/tickets", "application/json", strings.NewReader(`{"title":"parallel","prompt":"run"}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first ticket did not start")
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("second ticket did not start concurrently")
	}
	runner.release <- struct{}{}
	runner.release <- struct{}{}
	if runner.peak != 2 {
		t.Fatalf("peak active runners = %d, want 2", runner.peak)
	}
}
