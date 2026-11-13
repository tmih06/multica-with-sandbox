# Multica execution-backend inventory

This is the initial inventory for making `software-factory` a swappable execution
core behind Multica. The intended product boundary is:

- **Multica owns the interface and product state:** workspaces, users, members,
  permissions, issues, comments, statuses, agents, skills, chat, notifications,
  and the user-visible task history.
- **Software Factory owns execution:** admission into an isolated workspace,
  Kubernetes Sandbox lifecycle, agent process execution, logs, heartbeats,
  cancellation, recovery, artifacts, and cleanup.

The factory should not become a second ticket tracker. Multica should remain the
source of truth for a task and its issue timeline.

## Current software-factory boundary

### Existing responsibilities

`cmd/factory/main.go` starts one HTTP service and wires:

- a SQLite store (`factory.db` by default),
- a bounded in-process worker queue,
- a `kubectl`-based Agent Sandbox runner,
- startup reconciliation, and
- configurable image, namespace, timeout, capacity, and broker settings.

`internal/factory/factory.go` currently provides:

- `POST /tickets` to create a local factory ticket,
- `GET /tickets/{id}` for a snapshot,
- `GET /tickets/{id}/logs` for captured output,
- `GET /tickets/{id}/events` for persisted/replayed SSE events,
- queued/running/finished/failed status,
- execution policy defaults,
- bounded worker concurrency, and
- a `Runner` interface with optional `RecoveryRunner` support.

`internal/factory/store.go` persists the local ticket, policy, timestamps,
answer, logs, error, and lifecycle events in SQLite.

### Current Sandbox execution

`SandboxRunner`:

1. derives a deterministic Sandbox name from the local ticket ID,
2. applies an `agents.x-k8s.io/v1beta1` Sandbox manifest,
3. starts the `ompig-slim-workspace` image,
4. optionally mounts a broker token Secret,
5. waits for Sandbox readiness,
6. waits for the Pod to reach `Succeeded` or `Failed`,
7. collects Pod logs, and
8. requests Sandbox deletion in a deferred cleanup path.

The container script clones `FACTORY_REPOSITORY` into `/workspace/repo` and
runs `ompig-slim durable run` with the model, request ID, prompt, and turn
budget. The current runner does not yet expose a structured stream of tool
calls, assistant messages, usage, or files.

### Important current limitations

- Factory tickets are not Multica issue/task IDs.
- SQLite is local to one factory process; there is no shared Multica identity,
  workspace scope, or authorization model.
- The API has no authentication or authorization.
- There is no idempotency key or external task identity in the public API.
- There is no explicit cancellation endpoint.
- Logs are captured after completion, not streamed from the running worker.
- Recovery currently reuses the Sandbox name but does not yet establish a
  durable, provider-neutral execution protocol.
- The runner does not own Git branches, commits, PRs, or validation stages.
- The factory's `finished` state means worker completion, not acceptance.

## Current Multica execution boundary

Multica has a substantially larger product and execution protocol.

### Product/source-of-truth layer

The Go server owns PostgreSQL-backed entities including:

- workspaces and membership,
- issues, comments, statuses, labels, projects, and timelines,
- agents and skills,
- agent runtimes and runtime profiles,
- `agent_task_queue` task records,
- task messages/transcripts,
- task usage and receipts,
- cancellation/retry/recovery state, and
- realtime broadcasts.

The issue API is centered under `/api/issues`. Task execution records are under
`/api/tasks` and daemon-specific task endpoints.

### Existing runtime protocol

The current execution model assumes a connected daemon:

1. Multica creates an `agent_task_queue` row when an issue, comment, chat turn,
   autopilot, or other trigger starts work.
2. A daemon registers one or more `agent_runtime` records and maintains liveness
   over the daemon WebSocket.
3. The daemon claims work through the daemon claim endpoint/RPC.
4. The daemon starts the task and receives task-scoped context and credentials.
5. During execution it reports messages, tool events, usage, progress, and
   heartbeats.
6. It completes, fails, or acknowledges cancellation through Multica handlers.
7. Multica updates the task, issue timeline, comments, unread state, and
   realtime subscribers.

The key server-side operations are implemented in `internal/service/task.go`
(`Enqueue*`, `ClaimTasksForRuntimes`, `StartTask`, `CompleteTask`, `FailTask`,
and cancellation methods) and `internal/handler/daemon.go`.

`internal/daemonws/` provides connection-scoped runtime liveness and wakeup
hints. The WebSocket is an optimization and notification channel; task claim
correctness remains server/database based.

### Compatibility obligations

A Sandbox backend must preserve the semantics Multica clients already depend on,
not merely return a final string:

- stable task identity and idempotent terminal writes,
- workspace and agent authorization,
- queued, dispatched, running, completed, failed, and cancelled states,
- cancellation behavior and acknowledgement,
- ordered task messages/transcript records,
- progress and tool-event visibility,
- usage/model attribution,
- retry and stale-worker recovery,
- issue/comment delivery and timeline broadcasts, and
- reconnect-safe behavior after factory or Sandbox failure.

## Proposed replacement seam

The smallest safe seam is to replace the **runtime execution plane**, not the
Multica product server:

```text
Multica UI/API/PostgreSQL
        │
        │ issue/task lifecycle and execution request
        ▼
Execution backend adapter
        │
        ├── local daemon backend (existing)
        └── Sandbox factory backend (new)
                │
                └── Kubernetes Sandbox + isolated repo + ompig-slim durable
```

Multica should create and own the canonical task row. The adapter should submit
an execution request containing at least:

- Multica task ID and issue ID,
- workspace and agent identity,
- repository/revision/branch policy,
- prompt plus relevant issue/comment context,
- provider/model and execution policy,
- cancellation and deadline information, and
- an idempotency/request key.

The factory should return or stream:

- an execution/run ID,
- lifecycle transitions,
- ordered assistant/tool/progress messages,
- usage and model metadata,
- durable logs and artifacts,
- cancellation acknowledgement, and
- terminal success/failure details.

The adapter then translates those events into Multica's existing task service
and realtime behavior. The factory's local SQLite ticket should be removed or
reduced to an execution/run record keyed by the Multica task ID.

## Implemented bridge slice

The factory now includes an optional `MulticaTaskBridge` in
`internal/factory/multica.go`. When `MULTICA_SERVER_URL` (or `MULTICA_URL`), `MULTICA_TOKEN`, and
`MULTICA_WORKSPACE_ID` are set, it:

- registers one `omp`-compatible runtime slot per configured capacity,
- keeps each slot alive with heartbeats,
- claims up to the configured factory capacity, defaulting to three tasks,
- starts and completes tasks through Multica's PAT-authenticated daemon API,
- forwards an optional task token into the Sandbox for agent-side integrations,
- launches each task through the Sandbox runner,
- polls for Multica cancellation while the Sandbox runs,
- reports completion, failure, or cancellation acknowledgement, and
- deregisters the runtime during shutdown.

The Sandbox Pod template adds a hostname topology-spread constraint with
`DoNotSchedule`, so three simultaneous runs require three distinct eligible
worker hostnames instead of silently sharing one worker. Each run retains a
separate `/workspace/repo` inside its own Sandbox.

This is intentionally an adapter over Multica's existing daemon protocol. It
keeps the existing Multica frontend and task service as the source of truth,
without introducing a second issue/task database in the factory.

## Verified local integration run

The integration stack was run against a real local three-node kind cluster named
`factory`, not a fabricated kubeconfig or fake Kubernetes implementation. The
cluster had the Agent Sandbox CRD/controller installed and three eligible nodes.
The Compose stack started PostgreSQL, the Multica backend, the frontend, and the
factory; the factory mounted the host kubeconfig and used host networking to
reach the kind API and backend.

A three-issue behavior run was completed through the Multica API. The bridge
claimed all three issues concurrently, created three isolated Sandboxes, and
Kubernetes scheduled the corresponding Pods on three distinct hostnames. All
three task records reached `completed`, and the bridge deleted the Sandboxes
after collecting their results. The repeatable harness is
`scripts/three-multica-task-check.sh`; provide `MULTICA_TOKEN`,
`MULTICA_WORKSPACE_ID`, and three comma-separated `MULTICA_AGENT_IDS` values.

Verification completed in `software-factory`:

- `go test ./...`
- `go test -race ./internal/factory`
- `go vet ./...`
- `git diff --check`
- Shellcheck for both behavior scripts
- targeted Multica daemon handler tests
- Docker Compose build and health checks

The full Multica suite was not declared green: the full server run exceeded the
available execution timeout, and the known frontend failures were
`packages/views/modals/create-issue.test.tsx` and
`apps/web/app/text-contrast.test.ts`. Those failures are in
`/home/ubuntu/multica-with-sandbox`; that checkout was not modified by this
integration work.

## Distributed worker topology

The target deployment is one control plane with five remote worker systems joining
the Kubernetes cluster. Each worker contributes bounded compute capacity; the
factory core remains the scheduler and execution authority.

```text
Multica UI/API
      │
      ▼
Software Factory core / control-plane service
      │  durable queue, placement, reconciliation
      ▼
Kubernetes control plane
      │
      ├── worker-1 ── Sandbox ── isolated checkout + agent
      ├── worker-2 ── Sandbox ── isolated checkout + agent
      ├── worker-3 ── Sandbox ── isolated checkout + agent
      ├── worker-4 ── Sandbox ── isolated checkout + agent
      └── worker-5 ── Sandbox ── isolated checkout + agent
```

A worker should not be another ticket API or an independently scheduled daemon.
Kubernetes should place each Sandbox, while the factory controls admission,
capacity, policy, and reconciliation. Each worker needs:

- a stable node identity and readiness signal,
- labels for architecture, runtime class, toolchain, and trust profile,
- allocatable CPU, memory, ephemeral-storage, and Sandbox capacity,
- a reserved system/control-plane budget,
- network access only to the required source/model/artifact services, and
- enough isolation that one agent cannot inspect another worker's host or
  working directory.

The factory scheduler must account for more than a global concurrency number.
Placement should consider node availability, current reservations, resource
requests, repository/toolchain requirements, trust/network profile, and retry
history. A run must be assigned a deterministic placement constraint before
Sandbox creation, or remain queued when no worker satisfies its policy.

The first capacity model should be conservative: one meaningful coding Sandbox
per 2-vCPU worker, with explicit requests and limits. Kubernetes scheduling is
the final placement authority, but the factory must persist the selected node
or placement decision and reconcile it after restarts. A successful Pod
schedule is not proof that the agent is healthy; Sandbox readiness, worker
heartbeats, task heartbeats, and wall-clock deadlines remain separate signals.

If the five systems are actually separate Kubernetes clusters rather than five
nodes in one cluster, this design changes: each system needs a worker agent and
an authenticated worker protocol, and the factory must schedule across cluster
registrations instead of relying on Kubernetes node scheduling. This inventory
currently assumes one Kubernetes cluster with five worker nodes.

## Migration risks to resolve first

1. **Identity:** define the immutable mapping between Multica task IDs and
   factory execution IDs. Retries must not create ambiguous duplicate work.
2. **Context:** decide whether Multica sends a complete prompt snapshot or the
   factory fetches context. Sending a signed, immutable snapshot is safer for
   recovery and decoupling.
3. **Event contract:** map `ompig-slim` output to structured Multica task
   messages without scraping terminal text.
4. **Cancellation:** define the factory operation that stops the agent process,
   deletes or preserves the Sandbox, and reports whether stopping was confirmed.
5. **Recovery:** reconcile Multica task state, factory state, Sandbox state, and
   durable worker state independently after any component restart.
6. **Credentials:** keep Multica authentication and provider/Git credentials
   outside the agent workspace where possible; broker access must be scoped to a
   run rather than treated as tenant isolation.
7. **Repository isolation:** every execution gets its own checkout and Sandbox;
   no shared host working directory or concurrent agent process may touch the
   same checkout.
8. **Acceptance:** distinguish agent completion from tests, review, diff policy,
   commit, push, and PR acceptance.

## Recommended implementation order

1. Freeze a provider-neutral execution contract and state/event vocabulary.
2. Define the five-node worker contract: node labels, capacity reservations,
   readiness/heartbeat signals, trust profiles, and placement constraints.
3. Add a factory-facing Multica adapter with a fake backend and contract tests.
4. Add factory submission/status/event/cancel endpoints keyed by Multica task ID.
5. Add durable worker-aware scheduling and persist the placement decision for
   every run.
6. Stream structured worker events and usage from the Sandbox.
7. Implement idempotent recovery and cancellation before enabling production
   routing.
8. Route one narrow Multica task type through Sandboxes while local daemons stay
   available as a fallback.
9. Add per-run repository checkout, artifact retention, validation, and cleanup.
10. Only then consider making Sandbox execution the default backend.

This inventory deliberately does not propose deleting Multica's existing daemon
protocol yet. It identifies the execution-provider seam so both backends can be
verified against the same task semantics before the local-machine path is retired.
