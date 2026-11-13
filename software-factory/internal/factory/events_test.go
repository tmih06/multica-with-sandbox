package factory

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteStorePersistsEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.db")
	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.Create(TicketInput{Title: "Events", Prompt: "run"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AddEvent(ticket.ID, "ticket.queued", "accepted")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.EventsAfter(ticket.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != first.ID || events[0].Data != "accepted" {
		t.Fatalf("events = %#v", events)
	}
}

func TestTicketEventsEndpointStreamsTerminalEvent(t *testing.T) {
	server := httptest.NewServer(NewServer(fakeRunner{answer: "done"}).Handler())
	defer server.Close()
	response, err := http.Post(server.URL+"/tickets", "application/json", strings.NewReader(`{"title":"Events","prompt":"run"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	var ticket Ticket
	if err := waitForTicket(server.URL, &ticket); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/tickets/"+ticket.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Body.Close()
	if events.StatusCode != http.StatusOK || events.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q", events.StatusCode, events.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(events.Body)
	var body strings.Builder
	for scanner.Scan() {
		body.WriteString(scanner.Text())
		body.WriteByte('\n')
		if strings.Contains(body.String(), "event: ticket.finished") {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	stream := body.String()
	if !strings.Contains(stream, "id: 1\nevent: ticket.queued\ndata: ticket accepted\n\n") {
		t.Fatalf("SSE queued event is not framed correctly: %q", stream)
	}
	if !strings.Contains(stream, "event: ticket.finished\n") {
		t.Fatalf("SSE body = %q", stream)
	}
}

func waitForTicket(baseURL string, ticket *Ticket) error {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		result, err := http.Get(baseURL + "/tickets/ticket-1")
		if err != nil {
			return err
		}
		if err := json.NewDecoder(result.Body).Decode(ticket); err != nil {
			result.Body.Close()
			return err
		}
		result.Body.Close()
		if ticket.Status == StatusFinished || ticket.Status == StatusFailed {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return context.DeadlineExceeded
}
