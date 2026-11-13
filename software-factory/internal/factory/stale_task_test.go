package factory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type blockingRunner struct {
	started chan struct{}
}

func (r *blockingRunner) Run(ctx context.Context, _ Ticket) (RunResult, error) {
	close(r.started)
	<-ctx.Done()
	return RunResult{}, ctx.Err()
}

func TestMulticaBridgeStopsWhenTaskIsTerminalRemotely(t *testing.T) {
	var mu sync.Mutex
	var terminalCallbacks []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path != "/api/daemon/tasks/task-1/start" && r.URL.Path != "/api/daemon/tasks/task-1/status" {
			terminalCallbacks = append(terminalCallbacks, r.URL.Path)
		}
		mu.Unlock()
		switch r.URL.Path {
		case "/api/daemon/tasks/task-1/start":
			w.WriteHeader(http.StatusOK)
		case "/api/daemon/tasks/task-1/status":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "completed"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	runner := &blockingRunner{started: make(chan struct{})}
	bridge := NewMulticaTaskBridge(NewMulticaClient(server.URL, "pat"), "workspace", "factory", "omp", "", 2*time.Millisecond, 1, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	task := MulticaTask{ID: "task-1", RuntimeID: "runtime-1", IssueTitle: "test"}
	task.Agent.Model = "provider/model"
	go func() {
		bridge.execute(ctx, task)
		close(done)
	}()
	<-runner.started
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("stale task did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range terminalCallbacks {
		if path == "/api/daemon/tasks/task-1/complete" || path == "/api/daemon/tasks/task-1/fail" || path == "/api/daemon/tasks/task-1/cancel-ack" {
			t.Fatalf("stale task sent terminal callback %s", path)
		}
	}
}
