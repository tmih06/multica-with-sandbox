# Software Factory Roadmap

This roadmap tracks the software factory from ticket intake to a finished,
validated change. Phases 1 through 3 are complete. Phase 4 remains deferred while
execution policy and multi-worker scheduling are implemented.

## Phase 1: Make runs durable ✅ Complete

Replace the in-memory ticket map with a persistent `FactoryRun` store, initially
SQLite and later PostgreSQL if multiple factory instances are required.

Persist at least:

- Ticket ID and source provider
- Immutable run ID and idempotency key
- Repository, revision, branch, prompt, model, and effort policy
- Factory phase and terminal outcome
- Sandbox name, namespace, node, and resource profile
- Attempt count, timeout, and cancellation state
- Created, started, updated, and finished timestamps
- Final answer, error, and artifact references
- Cleanup state and last reconciliation error

The state machine should be explicit:

```text
queued → provisioning → running → validating → finished
                         │             │
                         ├─────────────┴→ failed
                         └────────────────→ cancelled

finished/failed/cancelled → cleaning → cleaned
```

Requirements:

- Ticket submission is idempotent for the same external ticket/run key.
- A factory restart does not create a duplicate Sandbox.
- State transitions are validated and timestamped.
- A run cannot be marked finished before worker output is committed.
- Cleanup is retryable and has its own terminal status.

## Phase 2: Add live observability ✅ Complete

Expose a live event stream in addition to the current ticket snapshot and final
logs:

```text
GET /tickets/{id}/events
```

Use Server-Sent Events for the first implementation. Events should include:

- Ticket accepted
- Sandbox requested/created/ready
- Repository checkout started/completed
- Agent started
- Pod log chunks
- Durable worker state changes
- Validation started/completed
- Cleanup requested/completed
- Final success/failure

Keep a bounded persisted event log so clients that reconnect can resume from an
event ID. Pod logs should be retained outside the Sandbox before cleanup.

The MVP currently captures factory markers and Pod output after completion. The
next implementation should stream those logs while the Pod is running. Exact
internal Durable tool-event records are a separate integration and should not be
reconstructed by scraping terminal output.

## Phase 3: Recover interrupted runs ✅ Complete

On factory startup, reconcile all non-terminal runs:

1. Load persisted runs in active phases.
2. Inspect the referenced Sandbox and Pod.
3. Reattach log collection where the worker is still running.
4. Resume `ompig-slim durable` when the workspace and state are recoverable.
5. Mark infrastructure failure when the Sandbox is gone and recovery is not safe.
6. Reconcile external effects before retrying.
7. Continue cleanup until every owned resource is gone.

Recovery must distinguish:

- Factory process restart
- Agent process failure
- Pod failure
- Worker-node loss
- Kubernetes control-plane outage
- Model/provider outage
- Cancellation
- Timeout

Do not claim exactly-once behavior for Git, ticket APIs, model billing, or other
external effects. Use idempotency keys and reconciliation instead.

## Phase 4: Connect a real ticket provider ⏸ Deferred

Implement GitHub Issues first.

Required behavior:

- Webhook intake, with polling as a fallback
- Signature verification
- Ticket deduplication
- Distributed pickup lock
- Source ticket status updates
- Cancellation detection
- Ticket comments containing progress and final results
- Link from the source ticket to the FactoryRun

The provider adapter should be behind a small interface so additional systems can
be added without changing the run state machine.

## Phase 5: Define agent execution policy 🚧 In progress

Make execution policy part of the ticket/run request rather than hidden in the
worker image.

Policy fields should cover:

- Model provider and model ID
- Thinking/reasoning effort
- Maximum Durable turns
- Wall-clock timeout
- CPU, memory, and ephemeral-storage limits
- Allowed tools
- Network access profile
- Repository write policy
- Build image/toolchain
- Artifact retention

The factory must record the effective policy used by each run. A run should not
silently inherit a changing global default.

## Phase 6: Schedule multiple workers 🚧 In progress

The local integration slice now registers one Multica runtime and agent slot per
configured factory capacity, claims one task per slot, and uses Kubernetes
hostname topology spread with `DoNotSchedule`. On the local three-node kind
cluster this has been exercised with three simultaneous Multica tasks, each
creating a separate Sandbox and Pod on a different node. The remaining
production scheduler work is to persist explicit worker assignments and expose
worker capacity/heartbeat metrics.

Move from one process-wide semaphore to a worker-aware queue:

- One active run per small worker by default
- Node labels for architecture, runtime, and capability
- Resource requests and limits on every Sandbox
- Queue fairness and backpressure
- Retry with exponential backoff
- Per-provider and per-repository concurrency limits
- Worker heartbeats and stale-worker detection
- Capacity metrics

Implemented in the current slice:

- Three independent Multica runtime/agent slots when capacity is three
- One task claim per slot to avoid Multica's per-agent deduplication
- Kubernetes hostname topology spread across eligible nodes
- Shared factory admission limit across local tickets and Multica tasks
- Local Compose stack using the host's kubeconfig and kind cluster

The five-node target topology should initially reserve one control-plane node and
use the remaining workers for Sandboxes. The first production profile should
still assume one meaningful coding run per 2-vCPU worker.

## Phase 7: Harden credentials and isolation

Before running untrusted repositories at scale:

- Rotate the API key that was used during the initial simulation.
- Keep the broker private beside the control plane.
- Stop sharing one broad broker bearer across all runs.
- Introduce run-scoped or worker-scoped credentials.
- Restrict Git credentials to the required repository and operation.
- Use short-lived model and registry credentials.
- Never mount the host Docker socket.
- Prefer rootless BuildKit or a daemonless builder over privileged DinD.
- Use gVisor or Kata where the nodes support the required runtime.
- Add network egress policy and resource/process limits.
- Add authentication and authorization to the factory API.
- Audit all credential and artifact access.

The broker is a credential and preference service, not an inference gateway or a
complete multi-tenant authorization layer. Treat its bearer as sensitive until a
run-scoped credential design exists.

## Phase 8: Add acceptance and validation

After the agent finishes, run factory-owned validation stages:

1. Formatting
2. Static analysis
3. Unit tests
4. Integration tests
5. Build/package validation
6. Secret scanning
7. Dependency/security scanning
8. Diff size and file policy checks
9. Final review-agent pass, if enabled

Each stage should produce a persisted result, exit status, duration, logs, and
artifact references. A worker's `done` answer is not proof that validation passed.

Failed validation should be resumable from the same run when safe, with an
attempt limit and an explicit reason for each retry.

## Phase 9: Git branch and PR workflow

Add Git operations only after durable runs, recovery, credentials, and validation
are reliable.

Recommended sequence:

1. Create a deterministic branch from the ticket and base revision.
2. Let the agent edit only the Sandbox checkout.
3. Run factory-owned validation.
4. Inspect the final diff.
5. Commit with a factory-controlled identity.
6. Push using a narrowly scoped credential.
7. Create or update a draft PR idempotently.
8. Attach test results and run metadata.
9. Reconcile push/PR state after interruptions.
10. Retain artifacts and clean the Sandbox.

The agent should not be the authority for PR creation, approval, or ticket
completion.

## Immediate next milestone ✅ Complete

Implemented one vertical slice:

```text
SQLite FactoryRun store
        ↓
SSE event stream with persisted event IDs
        ↓
Startup reconciliation for active Sandboxes
```

Acceptance criteria:

- Submit a ticket and restart the factory while it is running.
- Observe the same run after restart without duplicate Sandbox creation.
- Reconnect to the event stream and receive missed events.
- Capture Pod logs before Sandbox deletion.
- Recover a resumable worker or report a clear infrastructure failure.
- Preserve the final answer, logs, and cleanup outcome after process restart.
