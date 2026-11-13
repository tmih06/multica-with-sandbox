package factory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReportWorkerEventForwardsLiveUsage(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/tasks/task-1/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bridge := NewMulticaTaskBridge(NewMulticaClient(server.URL, "pat"), "workspace", "factory", "omp", "", 0, 1, nil)
	bridge.reportWorkerEvent(context.Background(), "task-1", WorkerEvent{
		Type:  "usage",
		Usage: &UsageReport{Provider: "openai", Model: "gpt-test", InputTokens: 10, OutputTokens: 5, CostUSD: 0.25},
	})
	bridge.reportWorkerEvent(context.Background(), "task-1", WorkerEvent{
		Type:  "usage",
		Usage: &UsageReport{Provider: "openai", Model: "gpt-test", InputTokens: 4, OutputTokens: 2, CostUSD: 0.1},
	})
	rows, ok := body["usage"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("body = %+v", body)
	}
	row := rows[0].(map[string]any)
	if body["snapshot"] != true || row["input_tokens"] != float64(14) || row["output_tokens"] != float64(7) || row["cost_usd_ticks"] != float64(3500000000) {
		t.Fatalf("usage row = %+v", row)
	}
}
