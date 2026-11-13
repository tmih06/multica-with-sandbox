package factory

import (
	"strings"
	"testing"
)

func TestMulticaTaskTicketUsesBridgeModelWhenClaimOmitsOne(t *testing.T) {
	task := MulticaTask{ID: "task-1"}
	ticket := task.ticket("")
	if ticket.Model != "" {
		t.Fatalf("claim ticket unexpectedly chose model %q", ticket.Model)
	}
	bridge := MulticaTaskBridge{AgentModel: "provider/default"}
	if bridge.AgentModel == "" {
		t.Fatal("bridge default model is empty")
	}
}

func TestMulticaTaskTicketCarriesRepositoryRevisionAndNonIssueContext(t *testing.T) {
	task := MulticaTask{ID: "task-1", IssueTitle: "", WorkspaceContext: "Use the repository conventions.", AutopilotTitle: "Nightly audit", AutopilotDescription: "Check for regressions.", QuickCreatePrompt: "Inspect the current implementation."}
	task.Agent.Instructions = "Report blockers honestly."
	task.Repos = []struct {
		URL string `json:"url"`
		Ref string `json:"ref,omitempty"`
	}{{URL: "https://example.test/repo", Ref: "release"}}
	ticket := task.ticket("")
	if ticket.Repository != "https://example.test/repo" || ticket.Revision != "release" {
		t.Fatalf("repository context = %+v", ticket)
	}
	for _, want := range []string{"Inspect the current implementation.", "Autopilot: Nightly audit", "Use the repository conventions.", "Report blockers honestly."} {
		if !strings.Contains(ticket.Prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, ticket.Prompt)
		}
	}
}
