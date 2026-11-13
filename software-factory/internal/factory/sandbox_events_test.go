package factory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSandboxRunnerSendsControlJSONLToWorker(t *testing.T) {
	dir := t.TempDir()
	kubectl := filepath.Join(dir, "kubectl")
	capture := filepath.Join(dir, "control.jsonl")
	script := "#!/bin/sh\ncat > " + capture + "\n"
	if err := os.WriteFile(kubectl, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runner := NewSandboxRunner(kubectl, "factory-test", "image", time.Second)
	if err := runner.SendControl(context.Background(), Ticket{ID: "task-1"}, "continue with the failing test"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\"content\":\"continue with the failing test\"}\n" {
		t.Fatalf("control payload = %q", data)
	}
}

func TestSandboxRunnerPollsWorkerEventsWhilePodRuns(t *testing.T) {
	dir := t.TempDir()
	kubectl := filepath.Join(dir, "kubectl")
	script := `#!/bin/sh
case "$1" in
  exec) printf '%s\n' '{"type":"tool_call","call_id":"call-1","tool":"bash"}' ;;
  get) if [ "$2" = pod ]; then printf '%s\n' '{"status":{"phase":"Succeeded"}}'; else printf '%s\n' '{}'; fi ;;
  *) printf '%s\n' '{}' ;;
esac
`
	if err := os.WriteFile(kubectl, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	runner := NewSandboxRunner(kubectl, "factory-test", "image", time.Second)
	runner.Poll = time.Millisecond
	var events []WorkerEvent
	phase, err := runner.waitForPodEvents(context.Background(), "sandbox-1", func(event WorkerEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if phase != "Succeeded" || len(events) != 1 || events[0].CallID != "call-1" {
		t.Fatalf("phase=%s events=%+v", phase, events)
	}
}
