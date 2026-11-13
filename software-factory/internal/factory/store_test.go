package factory

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStorePersistsTicketAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.db")
	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.Create(TicketInput{Title: "Persist me", Prompt: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ticket.ID, func(ticket *Ticket) {
		ticket.Status = StatusRunning
		ticket.StartedAt = time.Now().UTC()
	}); err != nil {
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
	got, ok, err := store.Get(ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Status != StatusRunning || got.Title != "Persist me" {
		t.Fatalf("ticket = %#v, found = %v", got, ok)
	}
}

func TestSQLiteStorePersistsPendingLifecycleCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.db")
	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"error": "timeout", "failure_reason": "timeout"}
	if err := store.SavePendingCallback("task-1", "/api/daemon/tasks/task-1/fail", body); err != nil {
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
	pending, err := store.PendingCallbacks()
	if err != nil || len(pending) != 1 || pending[0].TaskID != "task-1" || pending[0].Path == "" {
		t.Fatalf("pending = %#v, err = %v", pending, err)
	}
	if err := store.DeletePendingCallback("task-1"); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingCallbacks()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after delete = %#v, err = %v", pending, err)
	}
}

func TestSQLiteStoreListsActiveTicketsForRecovery(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queued, err := store.Create(TicketInput{Title: "queued", Prompt: "run"})
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.Create(TicketInput{Title: "running", Prompt: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(running.ID, func(ticket *Ticket) { ticket.Status = StatusRunning }); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(queued.ID, func(ticket *Ticket) { ticket.Status = StatusFinished }); err != nil {
		t.Fatal(err)
	}

	active, err := store.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != running.ID {
		t.Fatalf("active tickets = %#v", active)
	}
}
