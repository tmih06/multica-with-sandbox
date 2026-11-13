package factory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type steeringRunner struct {
	control chan string
}

func (r *steeringRunner) Run(ctx context.Context, _ Ticket) (RunResult, error) {
	select {
	case <-r.control:
		return RunResult{Answer: "steered"}, nil
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	}
}

func (r *steeringRunner) SendControl(_ context.Context, _ Ticket, content string) error {
	select {
	case r.control <- content:
	default:
	}
	return nil
}

func TestMulticaBridgeForwardsNewIssueCommentToRunningWorker(t *testing.T) {
	var commentPolls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/daemon/tasks/task-1/start":
			w.WriteHeader(http.StatusOK)
		case "/api/issues/issue-1":
			_ = json.NewEncoder(w).Encode(map[string]string{"title": "Issue", "description": "Do it"})
		case "/api/issues/issue-1/comments":
			if commentPolls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode([]map[string]string{})
			} else {
				_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "comment-1", "content": "Also fix the failing test", "author_type": "member"}})
			}
		case "/api/daemon/tasks/task-1/progress", "/api/daemon/tasks/task-1/messages", "/api/daemon/tasks/task-1/usage", "/api/daemon/tasks/task-1/complete":
			w.WriteHeader(http.StatusOK)
		case "/api/daemon/tasks/task-1/status":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "running"})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()

	runner := &steeringRunner{control: make(chan string, 1)}
	bridge := NewMulticaTaskBridge(NewMulticaClient(server.URL, "pat"), "workspace", "factory", "omp", "", time.Second, 1, runner)
	task := MulticaTask{ID: "task-1", RuntimeID: "runtime-1", IssueID: "issue-1", IssueTitle: "Issue"}
	task.Agent.Model = "provider/model"
	done := make(chan struct{})
	go func() {
		bridge.execute(context.Background(), task)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("steered task did not finish")
	}
	if commentPolls.Load() < 2 {
		t.Fatalf("comment polls = %d, want initial and live poll", commentPolls.Load())
	}
}
