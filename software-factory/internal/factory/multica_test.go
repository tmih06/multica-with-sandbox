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

type bridgeRunner struct {
	mu  sync.Mutex
	ids []string
}

func (r *bridgeRunner) Run(_ context.Context, ticket Ticket) (RunResult, error) {
	r.mu.Lock()
	r.ids = append(r.ids, ticket.ID)
	r.mu.Unlock()
	return RunResult{Answer: "sandbox complete"}, nil
}

func TestMulticaBridgeClaimsAndCompletesThreeSandboxTasks(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		calls[req.URL.Path]++
		mu.Unlock()
		switch req.URL.Path {
		case "/api/daemon/heartbeat":
			w.WriteHeader(http.StatusOK)
		case "/api/daemon/tasks/claim":
			_ = json.NewEncoder(w).Encode(map[string]any{"tasks": []map[string]any{
				{"id": "task-1", "runtime_id": "runtime-1", "auth_token": "token-1", "issue_title": "one", "agent": map[string]string{"model": "provider/model"}},
				{"id": "task-2", "runtime_id": "runtime-1", "auth_token": "token-2", "issue_title": "two", "agent": map[string]string{"model": "provider/model"}},
				{"id": "task-3", "runtime_id": "runtime-1", "auth_token": "token-3", "issue_title": "three", "agent": map[string]string{"model": "provider/model"}},
			}})
		case "/api/daemon/tasks/task-1/start", "/api/daemon/tasks/task-2/start", "/api/daemon/tasks/task-3/start",
			"/api/daemon/tasks/task-1/complete", "/api/daemon/tasks/task-2/complete", "/api/daemon/tasks/task-3/complete":
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s", req.URL.Path)
		}
	}))
	defer server.Close()

	runner := &bridgeRunner{}
	bridge := NewMulticaTaskBridge(NewMulticaClient(server.URL, "pat"), "workspace", "factory", "omp", "", time.Second, 3, runner)
	bridge.RuntimeID = "runtime-1"
	if err := bridge.claim(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		count := len(runner.ids)
		runner.mu.Unlock()
		if count == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	runner.mu.Lock()
	if len(runner.ids) != 3 {
		runner.mu.Unlock()
		t.Fatalf("runner received %d tasks, want 3", len(runner.ids))
	}
	runner.mu.Unlock()
	mu.Lock()
	heartbeats := calls["/api/daemon/heartbeat"]
	claims := calls["/api/daemon/tasks/claim"]
	mu.Unlock()
	if heartbeats != 1 || claims != 1 {
		t.Fatalf("heartbeat calls = %d, claim calls = %d", heartbeats, claims)
	}
}
