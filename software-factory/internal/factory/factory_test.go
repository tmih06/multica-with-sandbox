package factory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	answer string
	err    error
}

func (f fakeRunner) Run(context.Context, Ticket) (RunResult, error) {
	return RunResult{Answer: f.answer, Logs: "worker log"}, f.err
}

func TestTicketRunsToFinished(t *testing.T) {
	server := httptest.NewServer(NewServer(fakeRunner{answer: "done"}).Handler())
	defer server.Close()

	response, err := http.Post(server.URL+"/tickets", "application/json", strings.NewReader(`{"title":"Fix test","prompt":"run tests"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want %d", response.StatusCode, http.StatusAccepted)
	}
	var submitted Ticket
	if err := json.NewDecoder(response.Body).Decode(&submitted); err != nil {
		t.Fatal(err)
	}

	var got Ticket
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		result, err := http.Get(server.URL + "/tickets/" + submitted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(result.Body).Decode(&got); err != nil {
			result.Body.Close()
			t.Fatal(err)
		}
		result.Body.Close()
		if got.Status == StatusFinished {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got.Status != StatusFinished || got.Answer != "done" || got.Logs != "worker log" {
		t.Fatalf("ticket = %#v, want finished ticket with answer and logs", got)
	}
	logs, err := http.Get(server.URL + "/tickets/" + submitted.ID + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Body.Close()
	body, err := io.ReadAll(logs.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "worker log" {
		t.Fatalf("logs = %q, want worker log", body)
	}
}

func TestTicketInputValidation(t *testing.T) {
	handler := NewServer(fakeRunner{}).Handler()
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "missing title", body: `{"prompt":"work"}`},
		{name: "missing prompt", body: `{"title":"ticket"}`},
		{name: "wrong model", body: `{"title":"ticket","prompt":"work","model":"not-a-model"}`},
		{name: "unknown field", body: `{"title":"ticket","prompt":"work","extra":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(test.body))
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestSandboxNameIsDNSLabelSafeAndBounded(t *testing.T) {
	name := sandboxName("Ticket_123/with a very long name that should be bounded")
	if len(name) > 63 {
		t.Fatalf("name length = %d, want <= 63: %q", len(name), name)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "_") || strings.Contains(name, "/") {
		t.Fatalf("name is not DNS-label safe: %q", name)
	}
}
