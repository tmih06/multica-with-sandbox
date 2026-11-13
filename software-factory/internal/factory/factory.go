package factory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is the externally visible lifecycle state of a ticket.
type Status string

const (
	StatusQueued   Status = "queued"
	StatusRunning  Status = "running"
	StatusFinished Status = "finished"
	StatusFailed   Status = "failed"
)

// ExecutionPolicy records the worker limits and agent choices used by a run.
type ExecutionPolicy struct {
	Model          string `json:"model,omitempty"`
	Thinking       string `json:"thinking,omitempty"`
	MaxTurns       int    `json:"max_turns,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	CPURequest     string `json:"cpu_request,omitempty"`
	MemoryRequest  string `json:"memory_request,omitempty"`
	NetworkProfile string `json:"network_profile,omitempty"`
}

func defaultExecutionPolicy() ExecutionPolicy {
	return ExecutionPolicy{MaxTurns: 100, TimeoutSeconds: 30 * 60, CPURequest: "500m", MemoryRequest: "512Mi", NetworkProfile: "repository-and-broker"}
}

func effectivePolicy(input TicketInput) ExecutionPolicy {
	policy := input.Policy
	if policy.Model == "" {
		policy.Model = input.Model
	}
	defaults := defaultExecutionPolicy()
	if policy.MaxTurns <= 0 {
		policy.MaxTurns = defaults.MaxTurns
	}
	if policy.TimeoutSeconds <= 0 {
		policy.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if policy.CPURequest == "" {
		policy.CPURequest = defaults.CPURequest
	}
	if policy.MemoryRequest == "" {
		policy.MemoryRequest = defaults.MemoryRequest
	}
	if policy.NetworkProfile == "" {
		policy.NetworkProfile = defaults.NetworkProfile
	}
	return policy
}

// Ticket is the persisted API record for one factory run.
type Ticket struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Prompt     string          `json:"prompt"`
	Repository string          `json:"repository,omitempty"`
	Model      string          `json:"model,omitempty"`
	Status     Status          `json:"status"`
	Sandbox    string          `json:"sandbox,omitempty"`
	Answer     string          `json:"answer,omitempty"`
	Logs       string          `json:"logs,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  time.Time       `json:"started_at,omitempty"`
	FinishedAt time.Time       `json:"finished_at,omitempty"`
	Policy     ExecutionPolicy `json:"policy"`
	// AuthToken is an ephemeral task credential supplied only to a running
	// Multica bridge task. It is intentionally excluded from JSON and SQLite.
	AuthToken string `json:"-"`
}

// TicketInput is accepted by POST /tickets.
type TicketInput struct {
	Title      string          `json:"title"`
	Prompt     string          `json:"prompt"`
	Repository string          `json:"repository,omitempty"`
	Model      string          `json:"model,omitempty"`
	Policy     ExecutionPolicy `json:"policy,omitempty"`
}

// RunResult contains the final answer and the worker's captured output.
type RunResult struct {
	Answer string
	Logs   string
}

// Runner executes one ticket in an isolated workspace.
type Runner interface {
	Run(context.Context, Ticket) (RunResult, error)
}

// RecoveryRunner can reattach to a persisted run after the factory restarts.
type RecoveryRunner interface {
	Recover(context.Context, Ticket) (RunResult, error)
}

type memoryStore struct {
	mu       sync.RWMutex
	tickets  map[string]Ticket
	events   map[string][]Event
	seq      uint64
	eventSeq int64
}

func newStore() *memoryStore {
	return &memoryStore{tickets: make(map[string]Ticket), events: make(map[string][]Event)}
}

func newTicket(input TicketInput, id string) Ticket {
	policy := effectivePolicy(input)
	return Ticket{ID: id, Title: input.Title, Prompt: input.Prompt, Repository: input.Repository, Model: policy.Model, Policy: policy, Status: StatusQueued, CreatedAt: time.Now().UTC()}
}

func (s *memoryStore) Create(input TicketInput) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ticket := newTicket(input, fmt.Sprintf("ticket-%d", s.seq))
	s.tickets[ticket.ID] = ticket
	return ticket, nil
}

func (s *memoryStore) Get(id string) (Ticket, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ticket, ok := s.tickets[id]
	return ticket, ok, nil
}

func (s *memoryStore) Update(id string, fn func(*Ticket)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ticket, ok := s.tickets[id]
	if !ok {
		return fmt.Errorf("ticket %s not found", id)
	}
	fn(&ticket)
	s.tickets[id] = ticket
	return nil
}

func (s *memoryStore) Active() ([]Ticket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var active []Ticket
	for _, ticket := range s.tickets {
		if ticket.Status == StatusQueued || ticket.Status == StatusRunning {
			active = append(active, ticket)
		}
	}
	return active, nil
}

func (s *memoryStore) AddEvent(ticketID, eventType, data string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventSeq++
	event := Event{ID: s.eventSeq, TicketID: ticketID, Type: eventType, Data: data, CreatedAt: time.Now().UTC()}
	s.events[ticketID] = append(s.events[ticketID], event)
	return event, nil
}

func (s *memoryStore) EventsAfter(ticketID string, after int64) ([]Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Event
	for _, event := range s.events[ticketID] {
		if event.ID > after {
			result = append(result, event)
		}
	}
	return result, nil
}

func (s *memoryStore) Close() error { return nil }

// Server exposes the ticket API and runs one ticket at a time. Serial execution
// prevents a single factory process from overcommitting CPU and memory.
type runRequest struct {
	ctx context.Context
	id  string
	run func(context.Context, Ticket) (RunResult, error)
}

type Server struct {
	store  ticketStore
	runner Runner
	queue  chan runRequest
	subMu  sync.Mutex
	subs   map[string]map[chan Event]struct{}
}

func NewServer(runner Runner) *Server {
	return NewServerWithCapacity(runner, 1)
}

// NewServerWithCapacity creates an in-memory server with the requested worker capacity.
func NewServerWithCapacity(runner Runner, capacity int) *Server {
	return NewServerWithStoreAndCapacity(runner, newStore(), capacity)
}

// NewServerWithStore creates a server backed by the supplied persistent store.
func NewServerWithStore(runner Runner, store ticketStore) *Server {
	return NewServerWithStoreAndCapacity(runner, store, 1)
}

// NewServerWithStoreAndCapacity creates a persistent server with bounded worker concurrency.
func NewServerWithStoreAndCapacity(runner Runner, store ticketStore, capacity int) *Server {
	if capacity < 1 {
		capacity = 1
	}
	server := &Server{store: store, runner: runner, queue: make(chan runRequest, capacity*4), subs: make(map[string]map[chan Event]struct{})}
	for range capacity {
		go server.worker()
	}
	return server
}

// Handler returns the HTTP interface for ticket submission and inspection.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /tickets", s.createTicket)
	mux.HandleFunc("GET /tickets/{id}", s.getTicket)
	mux.HandleFunc("GET /tickets/{id}/logs", s.getLogs)
	mux.HandleFunc("GET /tickets/{id}/events", s.events)
	return mux
}

func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var input TicketInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "title and prompt are required")
		return
	}
	if input.Model != "" && !strings.Contains(input.Model, "/") {
		writeError(w, http.StatusBadRequest, "model must be provider/model")
		return
	}
	ticket, err := s.store.Create(input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create ticket: "+err.Error())
		return
	}
	s.publish(ticket.ID, "ticket.queued", "ticket accepted")
	go s.process(ticket.ID)
	writeJSON(w, http.StatusAccepted, ticket)
}

func (s *Server) getTicket(w http.ResponseWriter, r *http.Request) {
	ticket, ok, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get ticket: "+err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "ticket not found")
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	ticket, ok, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get ticket: "+err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "ticket not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(ticket.Logs))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok, err := s.store.Get(id); err != nil {
		writeError(w, http.StatusInternalServerError, "get ticket: "+err.Error())
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "ticket not found")
		return
	}
	after, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if err != nil {
		after = 0
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	channel := s.subscribe(id)
	defer s.unsubscribe(id, channel)
	if events, err := s.store.EventsAfter(id, after); err != nil {
		writeError(w, http.StatusInternalServerError, "get events: "+err.Error())
		return
	} else {
		for _, event := range events {
			writeEvent(w, event)
			flusher.Flush()
			if isTerminalEvent(event.Type) {
				return
			}
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-channel:
			if !open {
				return
			}
			writeEvent(w, event)
			flusher.Flush()
			if isTerminalEvent(event.Type) {
				return
			}
		}
	}
}

func (s *Server) process(id string) {
	s.enqueue(runRequest{ctx: context.Background(), id: id, run: s.runner.Run})
}

func (s *Server) enqueue(request runRequest) {
	s.queue <- request
}

func (s *Server) worker() {
	for request := range s.queue {
		s.processWithContext(request.ctx, request.id, request.run)
	}
}

// Reconcile resumes queued tickets and delegates running tickets to a recovery
// adapter. A running ticket without that adapter is failed explicitly rather
// than being retried blindly, because its previous worker may have caused an
// external side effect before the factory process stopped.
func (s *Server) Reconcile(ctx context.Context) error {
	active, err := s.store.Active()
	if err != nil {
		return err
	}
	for _, ticket := range active {
		switch ticket.Status {
		case StatusQueued:
			go s.enqueue(runRequest{ctx: ctx, id: ticket.ID, run: s.runner.Run})
		case StatusRunning:
			recoverer, ok := s.runner.(RecoveryRunner)
			if !ok {
				reason := "factory restarted while ticket was running; no recovery adapter is available"
				if err := s.store.Update(ticket.ID, func(t *Ticket) { t.Status = StatusFailed; t.Error = reason; t.FinishedAt = time.Now().UTC() }); err != nil {
					return err
				}
				s.publish(ticket.ID, "ticket.failed", reason)
				continue
			}
			s.publish(ticket.ID, "ticket.recovering", "reattaching worker")
			go s.enqueue(runRequest{ctx: ctx, id: ticket.ID, run: recoverer.Recover})
		}
	}
	return nil
}

func (s *Server) processWithContext(ctx context.Context, id string, run func(context.Context, Ticket) (RunResult, error)) {
	ticket, ok, err := s.store.Get(id)
	if err != nil || !ok {
		return
	}
	startedAt := time.Now().UTC()
	if err := s.store.Update(id, func(t *Ticket) { t.Status = StatusRunning; t.StartedAt = startedAt }); err != nil {
		return
	}
	s.publish(id, "ticket.running", "worker started")
	ticket.Status = StatusRunning
	ticket.StartedAt = startedAt
	result, err := run(ctx, ticket)
	_ = s.store.Update(id, func(t *Ticket) {
		t.FinishedAt = time.Now().UTC()
		t.Answer = result.Answer
		t.Logs = result.Logs
		if err != nil {
			t.Status = StatusFailed
			t.Error = err.Error()
			return
		}
		t.Status = StatusFinished
	})
	if err != nil {
		s.publish(id, "ticket.failed", err.Error())
	} else {
		s.publish(id, "ticket.finished", result.Answer)
	}
}

func (s *Server) publish(ticketID, eventType, data string) {
	event, err := s.store.AddEvent(ticketID, eventType, data)
	if err != nil {
		return
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for channel := range s.subs[ticketID] {
		select {
		case channel <- event:
		default:
		}
	}
}

func (s *Server) subscribe(ticketID string) chan Event {
	channel := make(chan Event, 16)
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.subs[ticketID] == nil {
		s.subs[ticketID] = make(map[chan Event]struct{})
	}
	s.subs[ticketID][channel] = struct{}{}
	return channel
}

func (s *Server) unsubscribe(ticketID string, channel chan Event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	delete(s.subs[ticketID], channel)
	close(channel)
}

func writeEvent(w http.ResponseWriter, event Event) {
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, strings.ReplaceAll(event.Data, "\n", "\ndata: "))
}

func isTerminalEvent(eventType string) bool {
	return eventType == "ticket.finished" || eventType == "ticket.failed"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// SandboxRunner uses kubectl as the MVP adapter instead of importing the full
// Kubernetes client. The adapter creates one Sandbox, waits for its Pod to
// finish, collects logs, and always attempts deletion.
type SandboxRunner struct {
	Kubectl           string
	Namespace         string
	Image             string
	Timeout           time.Duration
	Poll              time.Duration
	BrokerURL         string
	BrokerTokenSecret string
	BrokerTokenKey    string
	ProfileDir        string
	MulticaURL        string
	MulticaWorkspace  string
	limiter           chan struct{}
}

func NewSandboxRunner(kubectl, namespace, image string, timeout time.Duration) *SandboxRunner {
	return &SandboxRunner{
		Kubectl: kubectl, Namespace: namespace, Image: image,
		Timeout: timeout, Poll: 2 * time.Second,
		BrokerTokenKey: "token",
		ProfileDir:     "/workspace/profile",
	}
}

// SetCapacity shares a process-wide Sandbox admission limit between local API
// tickets and the Multica bridge. The limit is acquired before Kubernetes
// creation, so the factory cannot accidentally exceed the configured worker
// budget through two independent submission paths.
func (r *SandboxRunner) SetCapacity(capacity int) {
	if capacity < 1 {
		capacity = 1
	}
	r.limiter = make(chan struct{}, capacity)
}

// Recover reuses the deterministic Sandbox name so a restarted factory can
// reattach to an existing worker or recreate its missing Kubernetes object.
func (r *SandboxRunner) Recover(parent context.Context, ticket Ticket) (RunResult, error) {
	return r.Run(parent, ticket)
}

func (r *SandboxRunner) timeoutFor(ticket Ticket) time.Duration {
	if ticket.Policy.TimeoutSeconds > 0 {
		return time.Duration(ticket.Policy.TimeoutSeconds) * time.Second
	}
	return r.Timeout
}

func (r *SandboxRunner) Run(parent context.Context, ticket Ticket) (result RunResult, runErr error) {
	if r.limiter != nil {
		select {
		case r.limiter <- struct{}{}:
			defer func() { <-r.limiter }()
		case <-parent.Done():
			return result, parent.Err()
		}
	}
	ctx, cancel := context.WithTimeout(parent, r.timeoutFor(ticket))
	defer cancel()
	name := sandboxName(ticket.ID)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := r.delete(cleanupCtx, name); err != nil && runErr == nil {
			result.Logs += "phase=cleanup-failed\n" + err.Error() + "\n"
			runErr = fmt.Errorf("cleanup sandbox %s: %w", name, err)
		} else {
			result.Logs += "phase=cleanup-requested\n"
		}
	}()

	result.Logs = fmt.Sprintf("ticket=%s\nphase=creating sandbox=%s\n", ticket.ID, name)
	if err := r.apply(ctx, name, ticket); err != nil {
		result.Logs += "phase=create-failed\n" + err.Error() + "\n"
		return result, err
	}
	result.Logs += "phase=sandbox-created\n"
	if err := r.waitForReady(ctx, name); err != nil {
		result.Logs += "phase=readiness-failed\n" + err.Error() + "\n"
		return result, err
	}
	result.Logs += "phase=sandbox-ready\n"
	phase, err := r.waitForPod(ctx, name)
	if err != nil {
		result.Logs += "phase=pod-wait-failed\n" + err.Error() + "\n"
		return result, err
	}
	result.Logs += "phase=pod-" + strings.ToLower(phase) + "\n"
	podLogs, err := r.logs(ctx, name)
	result.Logs += "--- pod output ---\n" + podLogs
	if err != nil {
		return result, err
	}
	result.Answer = podLogs
	if phase != "Succeeded" {
		return result, fmt.Errorf("sandbox pod ended in phase %s", phase)
	}
	return result, nil
}

func (r *SandboxRunner) apply(ctx context.Context, name string, ticket Ticket) error {
	if err := r.validate(); err != nil {
		return err
	}
	manifest := r.manifest(name, ticket)
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	_, err = r.run(ctx, body, "apply", "-f", "-")
	return err
}

// manifest builds one isolated Sandbox workload. The hostname spread constraint
// asks Kubernetes to distribute simultaneous runs across distinct worker nodes.
// DoNotSchedule prevents three concurrent runs from collapsing onto one worker;
// a run stays queued until a suitable worker is available.
func (r *SandboxRunner) manifest(name string, ticket Ticket) map[string]any {
	env := []any{
		map[string]any{"name": "FACTORY_REPOSITORY", "value": ticket.Repository},
		map[string]any{"name": "FACTORY_MODEL", "value": ticket.Model},
		map[string]any{"name": "FACTORY_PROMPT", "value": ticket.Prompt},
		map[string]any{"name": "FACTORY_REQUEST_ID", "value": ticket.ID},
		map[string]any{"name": "FACTORY_MAX_TURNS", "value": fmt.Sprintf("%d", ticket.Policy.MaxTurns)},
	}
	if r.MulticaURL != "" {
		env = append(env, map[string]any{"name": "MULTICA_SERVER_URL", "value": r.MulticaURL})
	}
	if ticket.AuthToken != "" {
		env = append(env,
			map[string]any{"name": "MULTICA_TOKEN", "value": ticket.AuthToken},
			map[string]any{"name": "MULTICA_TASK_ID", "value": ticket.ID},
		)
	}
	if r.MulticaWorkspace != "" {
		env = append(env, map[string]any{"name": "MULTICA_WORKSPACE_ID", "value": r.MulticaWorkspace})
	}
	volumeMounts := []any{}
	volumes := []any{}
	if r.BrokerURL != "" {
		env = append(env,
			map[string]any{"name": "PIG_HOME", "value": r.ProfileDir},
			map[string]any{"name": "OMPIG_BROKER_URL", "value": r.BrokerURL},
			map[string]any{"name": "OMPIG_BROKER_TOKEN_FILE", "value": "/run/secrets/broker/token"},
		)
		volumeMounts = append(volumeMounts, map[string]any{"name": "broker-token", "mountPath": "/run/secrets/broker", "readOnly": true})
		volumes = append(volumes, map[string]any{"name": "broker-token", "secret": map[string]any{"secretName": r.BrokerTokenSecret, "items": []any{map[string]any{"key": r.BrokerTokenKey, "path": "token"}}}})
	}
	return map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1",
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name": name, "namespace": r.Namespace,
			"labels": map[string]string{"factory.example.com/ticket": ticket.ID},
		},
		"spec": map[string]any{
			"podTemplate": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"factory.example.com/managed": "sandbox"}},
				"spec": map[string]any{
					"restartPolicy": "Never",
					"volumes":       volumes,
					"topologySpreadConstraints": []any{map[string]any{
						"maxSkew":           1,
						"topologyKey":       "kubernetes.io/hostname",
						"whenUnsatisfiable": "DoNotSchedule",
						"labelSelector": map[string]any{
							"matchLabels": map[string]string{"factory.example.com/managed": "sandbox"},
						},
					}},
					"containers": []any{map[string]any{
						"name":         "agent",
						"image":        r.Image,
						"command":      []string{"/bin/sh", "-c"},
						"args":         []string{agentScript},
						"env":          env,
						"volumeMounts": volumeMounts,
						"resources": map[string]any{
							"requests": map[string]string{"cpu": ticket.Policy.CPURequest, "memory": ticket.Policy.MemoryRequest},
							"limits":   map[string]string{"cpu": ticket.Policy.CPURequest, "memory": ticket.Policy.MemoryRequest},
						},
					}},
				},
			},
		},
	}
}

const agentScript = `set -eu
mkdir -p /workspace/state
if [ -n "$FACTORY_REPOSITORY" ]; then
  git clone --depth=1 "$FACTORY_REPOSITORY" /workspace/repo
else
  mkdir -p /workspace/repo
fi
if [ -z "$FACTORY_MODEL" ]; then
  printf '%s\n' '{"status":"done","answer":"workspace prepared; no model requested"}'
  exit 0
fi
exec ompig-slim durable run --state /workspace/state --cwd /workspace/repo --model "$FACTORY_MODEL" --request-id "$FACTORY_REQUEST_ID" --max-turns "$FACTORY_MAX_TURNS" "$FACTORY_PROMPT"`

func (r *SandboxRunner) waitForReady(ctx context.Context, name string) error {
	for {
		data, err := r.output(ctx, "get", "sandbox", name, "-n", r.Namespace, "-o", "json")
		if err == nil {
			var object struct {
				Status struct {
					Conditions []struct{ Type, Status, Reason string } `json:"conditions"`
				} `json:"status"`
			}
			if json.Unmarshal(data, &object) == nil {
				readyReason := ""
				for _, condition := range object.Status.Conditions {
					switch {
					case condition.Type == "Ready" && condition.Status == "True":
						return nil
					case condition.Type == "Ready":
						readyReason = condition.Reason
					case condition.Type == "Finished" && condition.Status == "True":
						if condition.Reason == "PodSucceeded" {
							return nil
						}
						return fmt.Errorf("Sandbox finished before becoming ready: %s", condition.Reason)
					}
				}
				if readyReason == "PodFailed" || readyReason == "ReconcilerError" || readyReason == "InvalidConfiguration" {
					return fmt.Errorf("Sandbox cannot become ready: %s", readyReason)
				}
			}
		}
		if err := waitTick(ctx, r.Poll); err != nil {
			return fmt.Errorf("waiting for Sandbox readiness: %w", err)
		}
	}
}

func (r *SandboxRunner) waitForPod(ctx context.Context, name string) (string, error) {
	for {
		data, err := r.output(ctx, "get", "pod", name, "-n", r.Namespace, "-o", "json")
		if err == nil {
			var object struct {
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			}
			if json.Unmarshal(data, &object) == nil {
				switch object.Status.Phase {
				case "Succeeded", "Failed":
					return object.Status.Phase, nil
				}
			}
		}
		if err := waitTick(ctx, r.Poll); err != nil {
			return "", fmt.Errorf("waiting for Sandbox Pod: %w", err)
		}
	}
}

func (r *SandboxRunner) logs(ctx context.Context, name string) (string, error) {
	data, err := r.output(ctx, "logs", "pod/"+name, "-n", r.Namespace)
	return string(data), err
}

func (r *SandboxRunner) delete(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "delete", "sandbox", name, "-n", r.Namespace, "--ignore-not-found=true", "--wait=false")
	return err
}

func (r *SandboxRunner) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Kubectl, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (r *SandboxRunner) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Kubectl, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

func waitTick(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var namePattern = regexp.MustCompile(`[^a-z0-9-]+`)

func sandboxName(id string) string {
	clean := strings.ToLower(namePattern.ReplaceAllString(id, "-"))
	clean = strings.Trim(clean, "-")
	if len(clean) > 45 {
		clean = clean[:45]
	}
	hash := sha256.Sum256([]byte(id))
	return "factory-" + clean + "-" + hex.EncodeToString(hash[:])[:8]
}

// Ensure the production adapter fails clearly when kubectl is missing.
func (r *SandboxRunner) validate() error {
	if _, err := exec.LookPath(r.Kubectl); err != nil {
		return errors.New("kubectl is required: " + err.Error())
	}
	if r.Namespace == "" || r.Image == "" {
		return errors.New("factory namespace and image are required")
	}
	if r.BrokerURL != "" && r.BrokerTokenSecret == "" {
		return errors.New("broker token Secret is required when broker URL is configured")
	}
	return nil
}
