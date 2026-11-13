package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MulticaTaskBridge registers the factory as one Multica runtime and translates
// claimed tasks into isolated Sandbox executions. Multica remains authoritative
// for task state; the bridge only owns the worker-side lease and callbacks.
// RuntimeSlot is one independently claimable Multica daemon identity. Multica
// intentionally deduplicates claims per agent, so one runtime/agent slot is
// required for each concurrent task target.
type RuntimeSlot struct {
	DaemonID  string
	RuntimeID string
}

type MulticaTaskBridge struct {
	Client            *MulticaClient
	WorkspaceID       string
	DaemonID          string
	Provider          string
	Repository        string
	AgentName         string
	AgentModel        string
	AgentThinking     string
	AgentInstructions string
	PollInterval      time.Duration
	Capacity          int
	Runner            Runner
	Slots             []RuntimeSlot
	RuntimeID         string // legacy single-slot test seam
	RuntimeMu         sync.RWMutex
	RunningMu         sync.Mutex
	Running           map[string]context.CancelFunc
}

// NewMulticaTaskBridge constructs a bridge. A missing Multica URL or token is
// treated by the caller as disabled configuration, not as an anonymous mode.
func NewMulticaTaskBridge(client *MulticaClient, workspaceID, daemonID, provider, repository string, pollInterval time.Duration, capacity int, runner Runner) *MulticaTaskBridge {
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	if capacity < 1 {
		capacity = 1
	}
	client.WorkspaceID = workspaceID
	return &MulticaTaskBridge{
		Client: client, WorkspaceID: workspaceID, DaemonID: daemonID,
		Provider: provider, Repository: repository, PollInterval: pollInterval,
		AgentName: "Sandbox Builder", AgentModel: "deploy/deepseek-ai/deepseek-v4.1-flash-1017",
		AgentThinking: "medium", AgentInstructions: "Work on the assigned issue. Make the requested changes, run relevant tests, and report the result.",
		Capacity: capacity, Runner: runner, Running: make(map[string]context.CancelFunc),
	}
}

// Run registers the bridge, polls Multica for work, and shuts down its runtime
// registration when the parent context ends. At most the runner's configured
// worker capacity is exercised because each poll claims only available slots.
func (b *MulticaTaskBridge) Run(ctx context.Context) error {
	defer b.deregister(context.Background())
	ticker := time.NewTicker(b.PollInterval)
	defer ticker.Stop()
	for {
		if err := b.ensureRegistered(ctx); err == nil {
			if err := b.claim(ctx); err != nil && ctx.Err() == nil {
				// A transient server or Kubernetes outage must not terminate the
				// bridge. The next poll retries registration or claim.
				b.clearRuntime()
			}
		} else if ctx.Err() == nil {
			log.Printf("multica registration failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (b *MulticaTaskBridge) clearRuntime() {
	b.RuntimeMu.Lock()
	b.RuntimeID = ""
	b.Slots = nil
	b.RuntimeMu.Unlock()
}

func (b *MulticaTaskBridge) ensureRegistered(ctx context.Context) error {
	b.RuntimeMu.RLock()
	registered := len(b.Slots) > 0 || b.RuntimeID != ""
	b.RuntimeMu.RUnlock()
	if registered {
		return nil
	}
	return b.register(ctx)
}

func (b *MulticaTaskBridge) register(ctx context.Context) error {
	slots := make([]RuntimeSlot, 0, b.Capacity)
	for index := 0; index < b.Capacity; index++ {
		daemonID := b.DaemonID
		if index > 0 {
			daemonID = fmt.Sprintf("%s-slot-%d", b.DaemonID, index+1)
		}
		var response struct {
			Runtimes []struct {
				ID string `json:"id"`
			} `json:"runtimes"`
		}
		if err := b.Client.Post(ctx, "/api/daemon/register", map[string]any{
			"workspace_id": b.WorkspaceID, "daemon_id": daemonID,
			"device_name": "Software Factory Sandbox", "cli_version": "software-factory",
			"runtimes": []map[string]string{{"name": "Kubernetes Sandboxes", "type": b.Provider, "runtime_mode": "local", "version": "software-factory", "status": "online"}},
		}, &response); err != nil {
			return err
		}
		if len(response.Runtimes) == 0 || response.Runtimes[0].ID == "" {
			return fmt.Errorf("multica runtime registration returned no runtime for %s", daemonID)
		}
		slot := RuntimeSlot{DaemonID: daemonID, RuntimeID: response.Runtimes[0].ID}
		slots = append(slots, slot)
		if err := b.ensureAgent(ctx, slot.RuntimeID, index+1); err != nil {
			return err
		}
	}
	b.RuntimeMu.Lock()
	b.Slots = slots
	b.RuntimeID = slots[0].RuntimeID
	b.RuntimeMu.Unlock()
	return nil
}

func (b *MulticaTaskBridge) ensureAgent(ctx context.Context, runtimeID string, slot int) error {
	var agents []struct {
		RuntimeID string `json:"runtime_id"`
	}
	if err := b.Client.Get(ctx, "/api/agents", &agents); err != nil {
		return fmt.Errorf("list Multica agents: %w", err)
	}
	for _, agent := range agents {
		if agent.RuntimeID == runtimeID {
			return nil
		}
	}
	name := b.AgentName
	if slot > 1 {
		name = fmt.Sprintf("%s %d", name, slot)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := b.Client.Post(ctx, "/api/agents", map[string]any{
		"name": name, "description": "Runs in an isolated Kubernetes Sandbox.",
		"instructions": b.AgentInstructions, "runtime_id": runtimeID,
		"visibility": "workspace", "model": b.AgentModel,
		"thinking_level": b.AgentThinking,
	}, &created); err != nil {
		return fmt.Errorf("create Multica agent for runtime %s: %w", runtimeID, err)
	}
	return nil
}

func (b *MulticaTaskBridge) deregister(ctx context.Context) {
	b.RuntimeMu.RLock()
	slots := append([]RuntimeSlot(nil), b.Slots...)
	if len(slots) == 0 && b.RuntimeID != "" {
		slots = []RuntimeSlot{{DaemonID: b.DaemonID, RuntimeID: b.RuntimeID}}
	}
	b.RuntimeMu.RUnlock()
	for _, slot := range slots {
		_ = b.Client.Post(ctx, "/api/daemon/deregister", map[string]any{"runtime_ids": []string{slot.RuntimeID}}, nil)
	}
}

func (b *MulticaTaskBridge) claim(ctx context.Context) error {
	b.RuntimeMu.RLock()
	slots := append([]RuntimeSlot(nil), b.Slots...)
	if len(slots) == 0 && b.RuntimeID != "" {
		slots = []RuntimeSlot{{DaemonID: b.DaemonID, RuntimeID: b.RuntimeID}}
	}
	b.RuntimeMu.RUnlock()
	if len(slots) == 0 {
		return b.register(ctx)
	}
	b.RunningMu.Lock()
	available := b.Capacity - len(b.Running)
	b.RunningMu.Unlock()
	if available <= 0 {
		return nil
	}
	for _, slot := range slots {
		if err := b.Client.Post(ctx, "/api/daemon/heartbeat", map[string]any{"runtime_id": slot.RuntimeID}, nil); err != nil {
			return err
		}
		if available <= 0 {
			break
		}
		var response struct {
			Tasks []MulticaTask `json:"tasks"`
		}
		if err := b.Client.Post(ctx, "/api/daemon/tasks/claim", map[string]any{
			"daemon_id": slot.DaemonID, "runtime_ids": []string{slot.RuntimeID}, "max_tasks": 1,
		}, &response); err != nil {
			log.Printf("multica claim failed for %s: %v", slot.DaemonID, err)
			return err
		}
		log.Printf("multica claim for %s returned %d task(s)", slot.DaemonID, len(response.Tasks))
		for _, task := range response.Tasks {
			b.RunningMu.Lock()
			if len(b.Running) >= b.Capacity {
				b.RunningMu.Unlock()
				break
			}
			b.Running[task.ID] = nil
			b.RunningMu.Unlock()
			go b.execute(ctx, task)
			available--
		}
	}
	return nil
}

func (b *MulticaTaskBridge) execute(parent context.Context, task MulticaTask) {
	ctx, cancel := context.WithCancel(parent)
	b.RunningMu.Lock()
	b.Running[task.ID] = cancel
	b.RunningMu.Unlock()
	defer func() {
		cancel()
		b.RunningMu.Lock()
		delete(b.Running, task.ID)
		b.RunningMu.Unlock()
	}()

	startBody := map[string]any{"runtime_id": task.RuntimeID}
	if task.DispatchedAt != "" {
		startBody["dispatched_at"] = task.DispatchedAt
	}
	if err := b.Client.Post(ctx, "/api/daemon/tasks/"+task.ID+"/start", startBody, nil); err != nil {
		log.Printf("multica task %s start failed (token=%t): %v", task.ID, task.AuthToken != "", err)
		return
	}

	ticket := task.ticket(b.Repository)
	issue := struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}{}
	if task.IssueID != "" {
		if err := b.Client.Get(ctx, "/api/issues/"+task.IssueID, &issue); err == nil {
			ticket.Prompt = strings.TrimSpace(fmt.Sprintf("Work on Multica issue %s: %s\n\n%s\n\n%s", task.IssueIdentifier, issue.Title, issue.Description, task.Agent.Instructions))
		} else {
			log.Printf("multica task %s issue fetch failed: %v", task.ID, err)
		}
	}
	resultCh := make(chan struct {
		result RunResult
		err    error
	}, 1)
	go func() {
		result, err := b.Runner.Run(ctx, ticket)
		resultCh <- struct {
			result RunResult
			err    error
		}{result: result, err: err}
	}()
	statusTicker := time.NewTicker(2 * time.Second)
	defer statusTicker.Stop()
	var result RunResult
	var runErr error
	for {
		select {
		case outcome := <-resultCh:
			result, runErr = outcome.result, outcome.err
			goto executionFinished
		case <-statusTicker.C:
			var status struct {
				Status string `json:"status"`
			}
			if err := b.Client.Get(ctx, "/api/daemon/tasks/"+task.ID+"/status", &status); err == nil && status.Status == "cancelled" {
				cancel()
			}
		case <-ctx.Done():
			goto executionFinished
		}
	}
executionFinished:
	if ctx.Err() != nil {
		log.Printf("multica task %s cancelled", task.ID)
		_ = b.Client.Post(context.Background(), "/api/daemon/tasks/"+task.ID+"/cancel-ack", map[string]any{"process_cleanup_confirmed": true}, nil)
		return
	}
	if runErr != nil {
		log.Printf("multica task %s sandbox failed: %v", task.ID, runErr)
		_ = b.Client.Post(context.Background(), "/api/daemon/tasks/"+task.ID+"/fail", map[string]any{
			"error": runErr.Error(), "failure_reason": "sandbox_execution_failed",
		}, nil)
		return
	}
	log.Printf("multica task %s sandbox completed", task.ID)
	_ = b.Client.Post(context.Background(), "/api/daemon/tasks/"+task.ID+"/complete", map[string]any{
		"output": result.Answer, "work_dir": "/workspace/repo",
	}, nil)
}

// MulticaTask is the stable subset of a daemon claim needed to launch a Sandbox.
type MulticaTask struct {
	ID              string `json:"id"`
	RuntimeID       string `json:"runtime_id"`
	IssueID         string `json:"issue_id"`
	IssueTitle      string `json:"issue_title"`
	IssueIdentifier string `json:"issue_identifier"`
	Status          string `json:"status"`
	DispatchedAt    string `json:"dispatched_at"`
	AuthToken       string `json:"auth_token"`
	Agent           struct {
		Instructions  string `json:"instructions"`
		Model         string `json:"model"`
		ThinkingLevel string `json:"thinking_level"`
	} `json:"agent"`
	Repos []struct {
		URL string `json:"url"`
	} `json:"repos"`
}

func (t MulticaTask) ticket(defaultRepository string) Ticket {
	repository := defaultRepository
	if len(t.Repos) > 0 && t.Repos[0].URL != "" {
		repository = t.Repos[0].URL
	}
	prompt := fmt.Sprintf("Work on Multica issue %s: %s\n\nIssue ID: %s\n\n%s", t.IssueIdentifier, t.IssueTitle, t.IssueID, t.Agent.Instructions)
	return Ticket{
		ID: t.ID, Title: t.IssueTitle, Prompt: strings.TrimSpace(prompt), AuthToken: t.AuthToken,
		Repository: repository, Model: t.Agent.Model,
		Policy: ExecutionPolicy{Model: t.Agent.Model, Thinking: t.Agent.ThinkingLevel, MaxTurns: 100, TimeoutSeconds: 30 * 60, CPURequest: "500m", MemoryRequest: "512Mi", NetworkProfile: "repository-and-broker"},
		Status: StatusRunning,
	}
}

// MulticaClient performs the small authenticated JSON surface used by the
// bridge. The configured PAT is used for runtime registration, claims, issue
// context, and lifecycle callbacks; an optional task token is only forwarded
// into the Sandbox environment for agent-side Multica integrations.
type MulticaClient struct {
	BaseURL     string
	Token       string
	WorkspaceID string
	HTTP        *http.Client
}

// NewMulticaClient constructs an authenticated client for a Multica server.
func NewMulticaClient(baseURL, token string) *MulticaClient {
	return &MulticaClient{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Post sends a PAT-authenticated JSON request and decodes a successful response.
func (c *MulticaClient) Post(ctx context.Context, path string, body any, out any) error {
	return c.post(ctx, path, c.Token, body, out)
}

// PostWithToken sends a task-token-authenticated JSON request.
func (c *MulticaClient) PostWithToken(ctx context.Context, path, token string, body any, out any) error {
	return c.post(ctx, path, token, body, out)
}

// Get reads a PAT-authenticated JSON resource.
func (c *MulticaClient) Get(ctx context.Context, path string, out any) error {
	return c.get(ctx, path, c.Token, out)
}

// GetWithToken reads a task-scoped JSON resource.
func (c *MulticaClient) GetWithToken(ctx context.Context, path, token string, out any) error {
	return c.get(ctx, path, token, out)
}

func (c *MulticaClient) get(ctx context.Context, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if c.WorkspaceID != "" {
		req.Header.Set("X-Workspace-ID", c.WorkspaceID)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("multica %s: status %s", path, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(out)
}

func (c *MulticaClient) post(ctx context.Context, path, token string, body any, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if c.WorkspaceID != "" {
		req.Header.Set("X-Workspace-ID", c.WorkspaceID)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("multica %s: status %s: %s", path, response.Status, strings.TrimSpace(string(body)))
	}
	if out == nil || response.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}
