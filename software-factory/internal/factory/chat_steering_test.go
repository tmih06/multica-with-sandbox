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

func TestMulticaBridgeForwardsNewChatMessageToRunningWorker(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/daemon/tasks/task-1/start":
			w.WriteHeader(http.StatusOK)
		case "/api/chat/sessions/chat-1/messages":
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode([]map[string]string{})
			} else {
				_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "message-1", "role": "user", "content": "please inspect the failing test"}})
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
	task := MulticaTask{ID: "task-1", RuntimeID: "runtime-1", ChatSessionID: "chat-1"}
	task.Agent.Model = "provider/model"
	done := make(chan struct{})
	go func() {
		bridge.execute(context.Background(), task)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("chat-steered task did not finish")
	}
	if polls.Load() < 2 {
		t.Fatalf("chat polls = %d, want initial and live poll", polls.Load())
	}
}
