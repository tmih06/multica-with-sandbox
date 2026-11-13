package factory

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type recoveringRunner struct {
	runs atomic.Int32
}

func (r *recoveringRunner) Run(context.Context, Ticket) (RunResult, error) {
	r.runs.Add(1)
	return RunResult{Answer: "queued complete"}, nil
}

func (r *recoveringRunner) Recover(context.Context, Ticket) (RunResult, error) {
	r.runs.Add(1)
	return RunResult{Answer: "recovered complete"}, nil
}

func TestReconcileResumesQueuedAndRunningTickets(t *testing.T) {
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
	if err := store.Update(running.ID, func(ticket *Ticket) {
		ticket.Status = StatusRunning
		ticket.Sandbox = "factory-running"
	}); err != nil {
		t.Fatal(err)
	}
	runner := &recoveringRunner{}
	server := NewServerWithStore(runner, store)
	if err := server.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		queuedTicket, _, _ := store.Get(queued.ID)
		runningTicket, _, _ := store.Get(running.ID)
		if queuedTicket.Status == StatusFinished && runningTicket.Status == StatusFinished {
			if queuedTicket.Answer != "queued complete" || runningTicket.Answer != "recovered complete" {
				t.Fatalf("reconciled answers = %q, %q", queuedTicket.Answer, runningTicket.Answer)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("runs did not reconcile: queued=%#v running=%#v", mustGet(store, queued.ID), mustGet(store, running.ID))
}

func TestReconcileFailsRunningTicketWithoutRecoveryAdapter(t *testing.T) {
	store := newStore()
	ticket, err := store.Create(TicketInput{Title: "running", Prompt: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ticket.ID, func(ticket *Ticket) { ticket.Status = StatusRunning }); err != nil {
		t.Fatal(err)
	}
	server := NewServerWithStore(fakeRunner{}, store)
	if err := server.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Get(ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.Error == "" {
		t.Fatalf("ticket = %#v, want explicit recovery failure", got)
	}
}

func mustGet(store ticketStore, id string) Ticket {
	ticket, _, _ := store.Get(id)
	return ticket
}
