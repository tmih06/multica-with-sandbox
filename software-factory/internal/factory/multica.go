package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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

type pendingLifecycle struct {
	Path string
	Body any
}

type pendingCallbackStore interface {
	SavePendingCallback(string, string, any) error
	DeletePendingCallback(string) error
	PendingCallbacks() ([]pendingCallback, error)
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
	MessageSeq        map[string]int
	UsageMu           sync.Mutex
	Usage             map[string]map[string]UsageReport
	PendingMu         sync.Mutex
	Pending           map[string]pendingLifecycle
	PendingStore      pendingCallbackStore
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
		Capacity: capacity, Runner: runner, Running: make(map[string]context.CancelFunc), MessageSeq: make(map[string]int), Usage: make(map[string]map[string]UsageReport), Pending: make(map[string]pendingLifecycle),
	}
}

// Run registers the bridge, polls Multica for work, and shuts down its runtime
// registration when the parent context ends. At most the runner's configured
// worker capacity is exercised because each poll claims only available slots.
func (b *MulticaTaskBridge) Run(ctx context.Context) error {
	defer func() {
		b.flushPending(context.Background())
		b.deregister(context.Background())
	}()
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
	b.flushPending(ctx)
	// Heartbeat every slot before admission accounting. A full bridge still
	// owns active runtimes and must not let them expire while their Sandboxes run.
	for _, slot := range slots {
		if err := b.Client.Post(ctx, "/api/daemon/heartbeat", map[string]any{"runtime_id": slot.RuntimeID}, nil); err != nil {
			return err
		}
	}
	b.RunningMu.Lock()
	available := b.Capacity - len(b.Running)
	b.RunningMu.Unlock()
	if available <= 0 {
		return nil
	}
	for _, slot := range slots {
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
		delete(b.MessageSeq, task.ID)
		b.RunningMu.Unlock()
		b.UsageMu.Lock()
		delete(b.Usage, task.ID)
		b.UsageMu.Unlock()
	}()

	startBody := map[string]any{
		"runtime_id":   task.RuntimeID,
		"capabilities": []string{"task-supplement-v1", "pi-process-cleanup-v1", "checkout-keeps-work-v1"},
	}
	if task.DispatchedAt != "" {
		startBody["dispatched_at"] = task.DispatchedAt
	}
	if err := b.postLifecycle(ctx, "/api/daemon/tasks/"+task.ID+"/start", startBody); err != nil {
		log.Printf("multica task %s start failed (token=%t): %v", task.ID, task.AuthToken != "", err)
		return
	}
	b.reportProgress(ctx, task.ID, "Sandbox started", 1, 3)

	ticket := task.ticket(b.Repository)
	if task.PriorSessionID != "" || task.PriorWorkDir != "" {
		ticket.Prompt += fmt.Sprintf("\n\nPrior execution context: session=%s work_dir=%s. Inspect the existing workspace before assuming prior work is present.", task.PriorSessionID, task.PriorWorkDir)
	}
	if ticket.Model == "" {
		ticket.Model = b.AgentModel
		ticket.Policy.Model = b.AgentModel
	}
	if ticket.Policy.Thinking == "" {
		ticket.Policy.Thinking = b.AgentThinking
	}
	if task.IssueID == "" {
		sections := []string{}
		if strings.TrimSpace(task.QuickCreatePrompt) != "" {
			sections = append(sections, task.QuickCreatePrompt)
		}
		if strings.TrimSpace(task.AutopilotTitle) != "" || strings.TrimSpace(task.AutopilotDescription) != "" {
			sections = append(sections, fmt.Sprintf("Autopilot: %s\n%s", task.AutopilotTitle, task.AutopilotDescription))
		}
		if strings.TrimSpace(task.WorkspaceContext) != "" {
			sections = append(sections, "Workspace context:\n"+task.WorkspaceContext)
		}
		if len(task.ProjectResources) > 0 {
			if resources, err := json.Marshal(task.ProjectResources); err == nil {
				sections = append(sections, "Project resources:\n"+string(resources))
			}
		}
		if len(task.AutopilotTriggerPayload) > 0 {
			sections = append(sections, "Autopilot trigger payload:\n"+string(task.AutopilotTriggerPayload))
		}
		for _, comment := range task.CoalescedComments {
			sections = append(sections, "Additional task comment:\n"+comment.Content)
		}
		if len(sections) > 0 {
			sections = append(sections, task.Agent.Instructions)
			ticket.Prompt = strings.TrimSpace(strings.Join(sections, "\n\n"))
		}
	}
	issue := struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}{}
	if task.IssueID != "" {
		if err := b.Client.Get(ctx, "/api/issues/"+task.IssueID, &issue); err == nil {
			sections := []string{fmt.Sprintf("Work on Multica issue %s: %s", task.IssueIdentifier, issue.Title), issue.Description}
			if strings.TrimSpace(task.WorkspaceContext) != "" {
				sections = append(sections, "Workspace context:\n"+task.WorkspaceContext)
			}
			if strings.TrimSpace(task.ProjectTitle) != "" || strings.TrimSpace(task.ProjectDescription) != "" {
				sections = append(sections, fmt.Sprintf("Project: %s\n%s", task.ProjectTitle, task.ProjectDescription))
			}
			for _, comment := range task.CoalescedComments {
				sections = append(sections, "Additional task comment:\n"+comment.Content)
			}
			if len(task.ProjectResources) > 0 {
				if resources, err := json.Marshal(task.ProjectResources); err == nil {
					sections = append(sections, "Project resources:\n"+string(resources))
				}
			}
			sections = append(sections, task.Agent.Instructions)
			ticket.Prompt = strings.TrimSpace(strings.Join(sections, "\n\n"))
		} else {
			log.Printf("multica task %s issue fetch failed: %v", task.ID, err)
		}
	}
	resultCh := make(chan struct {
		result RunResult
		err    error
	}, 1)
	go func() {
		var result RunResult
		var err error
		if runner, ok := b.Runner.(EventRunner); ok {
			result, err = runner.RunWithEvents(ctx, ticket, func(event WorkerEvent) {
				b.reportWorkerEvent(ctx, task.ID, event)
			})
		} else {
			result, err = b.Runner.Run(ctx, ticket)
		}
		resultCh <- struct {
			result RunResult
			err    error
		}{result: result, err: err}
	}()
	executionStartedAt := time.Now().UTC()
	statusTicker := time.NewTicker(2 * time.Second)
	defer statusTicker.Stop()
	seenComments := make(map[string]struct{}, len(task.CoalescedComments))
	for _, comment := range task.CoalescedComments {
		seenComments[comment.ID] = struct{}{}
	}
	if task.IssueID != "" {
		var existing []struct {
			ID string `json:"id"`
		}
		if err := b.Client.Get(ctx, "/api/issues/"+task.IssueID+"/comments?summary=true", &existing); err == nil {
			for _, comment := range existing {
				seenComments[comment.ID] = struct{}{}
			}
		}
	}
	if task.ChatSessionID != "" {
		var existing []struct {
			ID string `json:"id"`
		}
		if err := b.Client.Get(ctx, "/api/chat/sessions/"+task.ChatSessionID+"/messages", &existing); err == nil {
			for _, message := range existing {
				seenComments[message.ID] = struct{}{}
			}
		}
	}
	var result RunResult
	var runErr error
	remoteCancelled := false
	remoteTerminal := false
	executionReturned := false
	for {
		select {
		case outcome := <-resultCh:
			result, runErr = outcome.result, outcome.err
			executionReturned = true
			goto executionFinished
		case <-statusTicker.C:
			b.reportProgress(ctx, task.ID, "Sandbox running", 2, 3)
			if controller, ok := b.Runner.(ControlRunner); ok && (task.IssueID != "" || task.ChatSessionID != "") {
				var comments []struct {
					ID         string `json:"id"`
					Content    string `json:"content"`
					AuthorType string `json:"author_type"`
					Role       string `json:"role"`
				}
				path := "/api/issues/" + task.IssueID + "/comments?since=" + url.QueryEscape(executionStartedAt.Format(time.RFC3339Nano))
				if task.ChatSessionID != "" {
					path = "/api/chat/sessions/" + task.ChatSessionID + "/messages"
				}
				if err := b.Client.Get(ctx, path, &comments); err == nil {
					for _, comment := range comments {
						if comment.ID == "" || comment.Content == "" || comment.AuthorType == "agent" || comment.AuthorType == "system" || (comment.Role != "" && comment.Role != "user") {
							continue
						}
						if _, seen := seenComments[comment.ID]; seen {
							continue
						}
						if err := controller.SendControl(ctx, ticket, comment.Content); err != nil {
							log.Printf("multica task %s supplemental comment delivery failed: %v", task.ID, err)
							continue
						}
						seenComments[comment.ID] = struct{}{}
					}
				}
			}
			var status struct {
				Status string `json:"status"`
			}
			if err := b.Client.Get(ctx, "/api/daemon/tasks/"+task.ID+"/status", &status); err == nil {
				switch status.Status {
				case "running":
					// Expected state while the Sandbox owns execution.
				case "cancelled":
					remoteCancelled = true
					cancel()
				default:
					// queued/dispatched/waiting or any terminal state means this
					// worker no longer owns execution and must stop.
					remoteTerminal = true
					cancel()
				}
			} else {
				var httpErr *MulticaHTTPError
				if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
					// A deleted task has no lifecycle callback left to acknowledge.
					remoteTerminal = true
					cancel()
				}
			}
		case <-ctx.Done():
			goto executionFinished
		}
	}
executionFinished:
	if remoteCancelled {
		log.Printf("multica task %s cancelled remotely", task.ID)
		b.postTerminal(task.ID, "/api/daemon/tasks/"+task.ID+"/cancel-ack", map[string]any{"process_cleanup_confirmed": true})
		return
	}
	if remoteTerminal && !remoteCancelled {
		log.Printf("multica task %s was already terminal remotely", task.ID)
		return
	}
	if ctx.Err() != nil && !executionReturned {
		if parent.Err() != nil {
			// Factory shutdown is not a task cancellation. Leave the task for
			// Multica's stale-runtime recovery rather than mutating its state.
			log.Printf("multica task %s interrupted by factory shutdown", task.ID)
			return
		}
		log.Printf("multica task %s exceeded its execution deadline", task.ID)
		b.postTerminal(task.ID, "/api/daemon/tasks/"+task.ID+"/fail", map[string]any{
			"error": "sandbox execution deadline exceeded", "failure_reason": "timeout",
		})
		return
	}
	if runErr != nil {
		log.Printf("multica task %s sandbox failed: %v", task.ID, runErr)
		b.reportProgress(context.Background(), task.ID, "Sandbox failed", 3, 3)
		b.postTerminal(task.ID, "/api/daemon/tasks/"+task.ID+"/fail", map[string]any{
			"error": runErr.Error(), "failure_reason": "sandbox_execution_failed",
		})
		return
	}
	// The runner currently returns the durable assistant answer as one value.
	// Persist it as a normal task message before the terminal callback so the
	// issue/chat transcript and live clients see the same answer as the daemon
	// backend, rather than only receiving it through the completion payload.
	events := result.Events
	if len(events) == 0 && strings.TrimSpace(result.Answer) != "" {
		events = []WorkerEvent{{Type: "text", Content: result.Answer}}
	}
	// Re-send the committed final event set after execution. Stable event IDs
	// make this a loss-recovery pass for live callbacks, not a duplicate transcript.
	b.reportWorkerEvents(context.Background(), task.ID, events)
	b.reportUsage(context.Background(), task.ID, result.Usage, false)
	b.reportProgress(context.Background(), task.ID, "Sandbox completed", 3, 3)
	log.Printf("multica task %s sandbox completed", task.ID)
	b.postTerminal(task.ID, "/api/daemon/tasks/"+task.ID+"/complete", map[string]any{
		"output": result.Answer, "work_dir": "/workspace/repo",
	})
}

func (b *MulticaTaskBridge) postTerminal(taskID, path string, body any) {
	if err := b.postLifecycle(context.Background(), path, body); err == nil {
		if b.PendingStore != nil {
			_ = b.PendingStore.DeletePendingCallback(taskID)
		}
		return
	}
	b.PendingMu.Lock()
	b.Pending[taskID] = pendingLifecycle{Path: path, Body: body}
	b.PendingMu.Unlock()
	if b.PendingStore != nil {
		if err := b.PendingStore.SavePendingCallback(taskID, path, body); err != nil {
			log.Printf("multica task %s pending callback persistence failed: %v", taskID, err)
		}
	}
	log.Printf("multica task %s terminal callback queued for retry: %s", taskID, path)
}

func (b *MulticaTaskBridge) flushPending(ctx context.Context) {
	if b.PendingStore != nil {
		if stored, err := b.PendingStore.PendingCallbacks(); err == nil {
			b.PendingMu.Lock()
			for _, item := range stored {
				b.Pending[item.TaskID] = pendingLifecycle{Path: item.Path, Body: item.Body}
			}
			b.PendingMu.Unlock()
		} else {
			log.Printf("pending callback load failed: %v", err)
		}
	}
	b.PendingMu.Lock()
	pending := make(map[string]pendingLifecycle, len(b.Pending))
	for id, callback := range b.Pending {
		pending[id] = callback
	}
	b.PendingMu.Unlock()
	for taskID, callback := range pending {
		if err := b.postLifecycle(ctx, callback.Path, callback.Body); err != nil {
			log.Printf("multica task %s pending terminal callback failed: %v", taskID, err)
			continue
		}
		b.PendingMu.Lock()
		delete(b.Pending, taskID)
		b.PendingMu.Unlock()
		if b.PendingStore != nil {
			_ = b.PendingStore.DeletePendingCallback(taskID)
		}
	}
}

// postLifecycle retries callbacks whose response may be lost after the server
// commits the transition. Multica terminal callbacks are idempotent, so retrying
// is safer than leaving a task permanently running on a transient network error.
func (b *MulticaTaskBridge) postLifecycle(ctx context.Context, path string, body any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := b.Client.Post(ctx, path, body, nil); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
			}
		}
	}
	return lastErr
}

func (b *MulticaTaskBridge) reportWorkerEvent(ctx context.Context, taskID string, event WorkerEvent) {
	if event.Usage != nil {
		b.reportUsage(ctx, taskID, []UsageReport{*event.Usage}, true)
		return
	}
	b.reportEvents(ctx, taskID, []WorkerEvent{event})
}

func (b *MulticaTaskBridge) reportWorkerEvents(ctx context.Context, taskID string, events []WorkerEvent) {
	transcript := make([]WorkerEvent, 0, len(events))
	for _, event := range events {
		if event.Usage != nil {
			b.reportUsage(ctx, taskID, []UsageReport{*event.Usage}, true)
			continue
		}
		transcript = append(transcript, event)
	}
	b.reportEvents(ctx, taskID, transcript)
}

// reportEvents forwards the structured worker transcript using Multica's
// daemon message vocabulary. Each event has a stable idempotency key so a
// repeated terminal delivery cannot duplicate the transcript.
func (b *MulticaTaskBridge) reportEvents(ctx context.Context, taskID string, events []WorkerEvent) {
	messages := make([]map[string]any, 0, len(events))
	b.RunningMu.Lock()
	startSeq := b.MessageSeq[taskID]
	b.MessageSeq[taskID] = startSeq + len(events)
	b.RunningMu.Unlock()
	for index, event := range events {
		seq := startSeq + index + 1
		messageType := event.Type
		if messageType == "tool_call" {
			messageType = "tool_use"
		}
		key := fmt.Sprintf("software-factory:%s:event:%d", taskID, seq)
		if event.ID != "" {
			key = "software-factory:" + taskID + ":event:" + event.ID
		}
		message := map[string]any{
			"seq": seq, "type": messageType,
			"call_id": event.CallID, "tool": event.Tool,
			"content": event.Content, "input": event.Input, "output": event.Output,
			"idempotency_key": key,
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return
	}
	if err := b.postLifecycle(ctx, "/api/daemon/tasks/"+taskID+"/messages", map[string]any{"messages": messages}); err != nil {
		log.Printf("multica task %s event report failed: %v", taskID, err)
	}
}

// reportUsage forwards the worker's authoritative cumulative usage receipt.
func (b *MulticaTaskBridge) reportUsage(ctx context.Context, taskID string, usage []UsageReport, snapshot bool) {
	if len(usage) == 0 {
		return
	}
	if snapshot {
		b.UsageMu.Lock()
		byModel := b.Usage[taskID]
		if byModel == nil {
			byModel = make(map[string]UsageReport)
			b.Usage[taskID] = byModel
		}
		for _, item := range usage {
			key := item.Provider + "\x00" + item.Model
			total := byModel[key]
			total.Provider, total.Model = item.Provider, item.Model
			total.InputTokens += item.InputTokens
			total.OutputTokens += item.OutputTokens
			total.CacheReadTokens += item.CacheReadTokens
			total.CacheWriteTokens += item.CacheWriteTokens
			total.CostUSD += item.CostUSD
			byModel[key] = total
		}
		usage = make([]UsageReport, 0, len(byModel))
		for _, item := range byModel {
			usage = append(usage, item)
		}
		b.UsageMu.Unlock()
	}
	rows := make([]map[string]any, 0, len(usage))
	for _, item := range usage {
		row := map[string]any{
			"provider": item.Provider, "model": item.Model,
			"input_tokens": item.InputTokens, "output_tokens": item.OutputTokens,
			"cache_read_tokens": item.CacheReadTokens, "cache_write_tokens": item.CacheWriteTokens,
		}
		if item.CostUSD > 0 {
			// Multica stores provider-reported cost in 1e-10 USD ticks.
			row["cost_usd_ticks"] = int64(item.CostUSD * 1e10)
		}
		rows = append(rows, row)
	}
	if err := b.postLifecycle(ctx, "/api/daemon/tasks/"+taskID+"/usage", map[string]any{"usage": rows, "snapshot": snapshot}); err != nil {
		log.Printf("multica task %s usage report failed: %v", taskID, err)
	}
}

// reportProgress mirrors the daemon's live progress contract. Progress is
// deliberately best effort: a transient observability failure must not turn a
// successful Sandbox run into a failed task.
func (b *MulticaTaskBridge) reportProgress(ctx context.Context, taskID, summary string, step, total int) {
	if err := b.Client.Post(ctx, "/api/daemon/tasks/"+taskID+"/progress", map[string]any{
		"summary": summary, "step": step, "total": total,
	}, nil); err != nil {
		log.Printf("multica task %s progress report failed: %v", taskID, err)
	}
}

// MulticaTask is the stable subset of a daemon claim needed to launch a Sandbox.
type MulticaTask struct {
	ID                      string           `json:"id"`
	RuntimeID               string           `json:"runtime_id"`
	IssueID                 string           `json:"issue_id"`
	ChatSessionID           string           `json:"chat_session_id,omitempty"`
	IssueTitle              string           `json:"issue_title"`
	IssueIdentifier         string           `json:"issue_identifier"`
	WorkspaceContext        string           `json:"workspace_context,omitempty"`
	ProjectTitle            string           `json:"project_title,omitempty"`
	ProjectDescription      string           `json:"project_description,omitempty"`
	AutopilotTitle          string           `json:"autopilot_title,omitempty"`
	AutopilotDescription    string           `json:"autopilot_description,omitempty"`
	QuickCreatePrompt       string           `json:"quick_create_prompt,omitempty"`
	AutopilotTriggerPayload json.RawMessage  `json:"autopilot_trigger_payload,omitempty"`
	PriorSessionID          string           `json:"prior_session_id,omitempty"`
	PriorWorkDir            string           `json:"prior_work_dir,omitempty"`
	ProjectResources        []map[string]any `json:"project_resources,omitempty"`
	CoalescedComments       []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	} `json:"coalesced_comments,omitempty"`
	Status       string `json:"status"`
	DispatchedAt string `json:"dispatched_at"`
	AuthToken    string `json:"auth_token"`
	Agent        struct {
		Instructions  string `json:"instructions"`
		Model         string `json:"model"`
		ThinkingLevel string `json:"thinking_level"`
	} `json:"agent"`
	Repos []struct {
		URL string `json:"url"`
		Ref string `json:"ref,omitempty"`
	} `json:"repos"`
}

func (t MulticaTask) ticket(defaultRepository string) Ticket {
	repository := defaultRepository
	revision := ""
	if len(t.Repos) > 0 && t.Repos[0].URL != "" {
		repository = t.Repos[0].URL
		revision = t.Repos[0].Ref
	}
	sections := []string{}
	if t.IssueID != "" || t.IssueTitle != "" {
		sections = append(sections, fmt.Sprintf("Work on Multica issue %s: %s\n\nIssue ID: %s", t.IssueIdentifier, t.IssueTitle, t.IssueID))
	}
	if strings.TrimSpace(t.QuickCreatePrompt) != "" {
		sections = append(sections, t.QuickCreatePrompt)
	}
	if strings.TrimSpace(t.AutopilotTitle) != "" || strings.TrimSpace(t.AutopilotDescription) != "" {
		sections = append(sections, fmt.Sprintf("Autopilot: %s\n%s", t.AutopilotTitle, t.AutopilotDescription))
	}
	if strings.TrimSpace(t.WorkspaceContext) != "" {
		sections = append(sections, "Workspace context:\n"+t.WorkspaceContext)
	}
	sections = append(sections, t.Agent.Instructions)
	prompt := strings.Join(sections, "\n\n")
	return Ticket{
		ID: t.ID, Title: t.IssueTitle, Prompt: strings.TrimSpace(prompt), AuthToken: t.AuthToken,
		Repository: repository, Revision: revision, Model: t.Agent.Model,
		Policy: ExecutionPolicy{Model: t.Agent.Model, Thinking: t.Agent.ThinkingLevel, MaxTurns: 100, TimeoutSeconds: 30 * 60, CPURequest: "500m", MemoryRequest: "512Mi", NetworkProfile: "repository-and-broker"},
		Status: StatusRunning,
	}
}

// MulticaClient performs the small authenticated JSON surface used by the
// bridge. The configured PAT is used for runtime registration, claims, issue
// context, and lifecycle callbacks; an optional task token is only forwarded
// into the Sandbox environment for agent-side Multica integrations.
type MulticaHTTPError struct {
	Path       string
	StatusCode int
	Status     string
	Body       string
}

func (e *MulticaHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("multica %s: status %s", e.Path, e.Status)
	}
	return fmt.Sprintf("multica %s: status %s: %s", e.Path, e.Status, e.Body)
}

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
		return &MulticaHTTPError{Path: path, StatusCode: response.StatusCode, Status: response.Status}
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
		return &MulticaHTTPError{Path: path, StatusCode: response.StatusCode, Status: response.Status, Body: strings.TrimSpace(string(body))}
	}
	if out == nil || response.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}
