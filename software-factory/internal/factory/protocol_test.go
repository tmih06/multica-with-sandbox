package factory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestParseWorkerReceiptPreservesStructuredEventsAndUsage(t *testing.T) {
	receipt, ok := parseWorkerReceipt(`phase=pod-succeeded
{"status":"done","answer":"finished","events":[{"type":"tool_call","call_id":"call-1","tool":"bash","input":{"command":"go test ./..."}},{"type":"tool_result","call_id":"call-1","tool":"bash","output":"ok"}],"usage":[{"provider":"openai","model":"gpt-test","input_tokens":12,"output_tokens":7}]}
`)
	if !ok {
		t.Fatal("worker receipt was not parsed")
	}
	if receipt.Answer != "finished" || len(receipt.Events) != 2 || len(receipt.Usage) != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receipt.Events[0].Tool != "bash" || receipt.Usage[0].InputTokens != 12 {
		t.Fatalf("structured data was not preserved: %+v", receipt)
	}
}

func TestMulticaClientPreservesHTTPStatusForLifecycleDecisions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := NewMulticaClient(server.URL, "pat")
	var out map[string]any
	err := client.Get(context.Background(), "/api/daemon/tasks/task-1/status", &out)
	var httpErr *MulticaHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("error = %v, typed status = %+v", err, httpErr)
	}
}

func TestReportEventsUsesStableWorkerEventIDForReplay(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bridge := NewMulticaTaskBridge(NewMulticaClient(server.URL, "pat"), "workspace", "factory", "omp", "", 0, 1, nil)
	event := WorkerEvent{ID: "entry:1:0", Type: "text", Content: "hello"}
	bridge.reportEvents(context.Background(), "task-1", []WorkerEvent{event})
	bridge.reportEvents(context.Background(), "task-1", []WorkerEvent{event})
	if len(bodies) != 2 {
		t.Fatalf("request count = %d", len(bodies))
	}
	first := bodies[0]["messages"].([]any)[0].(map[string]any)
	second := bodies[1]["messages"].([]any)[0].(map[string]any)
	if first["idempotency_key"] != second["idempotency_key"] {
		t.Fatalf("replay keys differ: %v vs %v", first["idempotency_key"], second["idempotency_key"])
	}
}

func TestPendingTerminalCallbackRetriesOnNextCycle(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bridge := NewMulticaTaskBridge(NewMulticaClient(server.URL, "pat"), "workspace", "factory", "omp", "", 0, 1, nil)
	bridge.postTerminal("task-1", "/api/daemon/tasks/task-1/complete", map[string]any{"output": "done"})
	bridge.flushPending(context.Background())
	bridge.PendingMu.Lock()
	deferred := len(bridge.Pending)
	bridge.PendingMu.Unlock()
	if deferred != 0 || attempts.Load() < 2 {
		t.Fatalf("pending callbacks = %d, attempts = %d", deferred, attempts.Load())
	}
}

func TestReportEventsUsesMulticaMessageVocabularyAndStableKeys(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/tasks/task-1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bridge := NewMulticaTaskBridge(NewMulticaClient(server.URL, "pat"), "workspace", "factory", "omp", "", 0, 1, nil)
	bridge.reportEvents(context.Background(), "task-1", []WorkerEvent{{Type: "tool_call", CallID: "call-1", Tool: "bash"}})
	bridge.reportEvents(context.Background(), "task-1", []WorkerEvent{{Type: "tool_result", CallID: "call-1", Tool: "bash"}})
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("body = %+v", body)
	}
	message := messages[0].(map[string]any)
	if message["type"] != "tool_result" || message["seq"] != float64(2) || message["idempotency_key"] != "software-factory:task-1:event:2" {
		t.Fatalf("message = %+v", message)
	}
}
