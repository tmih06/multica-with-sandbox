package factory

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTicketStoresEffectiveExecutionPolicy(t *testing.T) {
	policy := ExecutionPolicy{
		Model:          "deploy/model",
		Thinking:       "high",
		MaxTurns:       12,
		TimeoutSeconds: 90,
		CPURequest:     "750m",
		MemoryRequest:  "1Gi",
		NetworkProfile: "repository-and-broker",
	}
	path := filepath.Join(t.TempDir(), "factory.db")
	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ticket, err := store.Create(TicketInput{Title: "Policy", Prompt: "run", Model: "deploy/model", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Policy != policy || ticket.Model != policy.Model {
		t.Fatalf("created ticket policy = %#v, model = %q", ticket.Policy, ticket.Model)
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
	if !ok || got.Policy != policy {
		t.Fatalf("persisted policy = %#v, found = %v", got.Policy, ok)
	}
}

func TestSandboxTimeoutUsesTicketPolicy(t *testing.T) {
	runner := NewSandboxRunner("kubectl", "factory-test", "image", 30*time.Minute)
	ticket := Ticket{Policy: ExecutionPolicy{TimeoutSeconds: 7}}
	if got := runner.timeoutFor(ticket); got != 7*time.Second {
		t.Fatalf("timeout = %s, want 7s", got)
	}
}

func TestSandboxManifestSpreadsConcurrentRunsAcrossWorkers(t *testing.T) {
	runner := NewSandboxRunner("kubectl", "factory-test", "image", time.Minute)
	manifest := runner.manifest("ticket-1", Ticket{ID: "ticket-1", Policy: defaultExecutionPolicy()})
	spec := manifest["spec"].(map[string]any)
	podTemplate := spec["podTemplate"].(map[string]any)
	podSpec := podTemplate["spec"].(map[string]any)
	constraints := podSpec["topologySpreadConstraints"].([]any)
	if len(constraints) != 1 {
		t.Fatalf("topology spread constraints = %#v, want one hostname constraint", constraints)
	}
	constraint := constraints[0].(map[string]any)
	if constraint["topologyKey"] != "kubernetes.io/hostname" {
		t.Fatalf("topology key = %v, want kubernetes.io/hostname", constraint["topologyKey"])
	}
	if constraint["whenUnsatisfiable"] != "DoNotSchedule" {
		t.Fatalf("when unsatisfiable = %v, want DoNotSchedule", constraint["whenUnsatisfiable"])
	}
}

func TestSandboxManifestCarriesRepositoryRevision(t *testing.T) {
	runner := NewSandboxRunner("kubectl", "factory-test", "image", time.Minute)
	manifest := runner.manifest("ticket-1", Ticket{ID: "ticket-1", Repository: "https://example.test/repo", Revision: "main", Policy: defaultExecutionPolicy()})
	env := manifest["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)
	found := false
	for _, raw := range env {
		item := raw.(map[string]any)
		if item["name"] == "FACTORY_REPOSITORY_REF" && item["value"] == "main" {
			found = true
		}
	}
	if !found {
		t.Fatal("repository revision was not added to Sandbox environment")
	}
}

func TestSandboxScriptSupportsOldAndEventCapableWorkers(t *testing.T) {
	for _, fragment := range []string{
		"durable run --help",
		"--events-file",
		"--control-file",
		"durable resume",
		"/workspace/state/job.json",
	} {
		if !strings.Contains(agentScript, fragment) {
			t.Fatalf("agent script missing compatibility fragment %q", fragment)
		}
	}
}
