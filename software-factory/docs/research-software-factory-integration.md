# ompig-slim as a software-factory worker

Research date: 2026-02-14

## Executive conclusion

`ompig-slim` is already a credible **single-ticket execution worker**, not a factory orchestrator. Its strongest fit is as the process inside one Agent Sandbox: the factory owns ticket state, scheduling, credentials, Git/PR effects, acceptance policy, and cleanup; `ompig-slim durable` owns resumable model/tool execution inside the workspace.

The existing durable mode should be the integration seam. Do not drive the interactive TUI, scrape terminal output, or build a second execution journal around it.

## What the repository provides

### Durable headless execution

`ompig-slim durable run` accepts a state directory, repository directory, exact `provider/model`, stable `request-id`, prompt, and turn budget. `resume` and `status` operate on the same state directory. See [`README.md`](../README.md#durable-headless-work) and [`internal/workdesk/workdesk.go`](../internal/workdesk/workdesk.go).

Important properties:

- The state directory represents one immutable assignment.
- `job.json` is written atomically and fsynced before task admission.
- `session.sqlite` plus its WAL is the canonical Durable journal.
- An OS advisory lock prevents two active writers for one workdesk.
- Repeating the same request is deduplicated; changing the assignment is refused.
- `status` does not start a scheduler or resolve model credentials.
- SIGINT/SIGTERM leaves pending work recoverable.
- Tool/model interruption can repeat model work; unsafe tool calls are not automatically replayed.

These are useful primitives for crash recovery, but they do not provide distributed fencing or exactly-once external effects. See [`docs/durable.md`](durable.md#recovery-contract).

### Workspace image

`Dockerfile.workspace` builds a static, unprivileged Alpine image containing `ompig-slim`, Git, SSH, Python, `rg`, `fd`, `jq`, curl, patch/archive tools, and shell utilities. It intentionally excludes language toolchains. See [`Dockerfile.workspace`](../Dockerfile.workspace) and [`CONTEXT.md`](../CONTEXT.md#the-workspace-image).

That makes it a good base image for a Sandbox, but repositories needing Go, Node, Rust, Java, system packages, or Docker image builds need a project-specific image or an approved build service.

### Agent state

The regular runtime publishes an advisory per-process JSON status document under `<PIG_HOME|~/.pi>/state/agent/<pid>/status.json`. It contains state, PID, session, mode, cwd, detail, and heartbeat timestamp. The file is atomic and stale-aware, but it is not a durable workflow record. See [`internal/agentstate/agentstate.go`](../internal/agentstate/agentstate.go).

For factory orchestration, prefer durable receipts and Kubernetes status as authoritative. Agent-state files are useful for live UI/diagnostics only, and durable mode currently does not make them the factory protocol.

### Remote broker

The broker centralizes OAuth refresh and profile/preferences, while clients still call model providers directly. It is not an inference gateway, task queue, workspace service, or multi-tenant authorization layer. Clients receive access credentials; the broker retains refresh capability. See [`docs/remote-broker.md`](remote-broker.md#api-and-security-boundary).

This can reduce repeated provider login setup across workers, but it is a sensitive shared credential authority. The bearer has broad account mutation authority, and the broker/database contain plaintext credential material at rest. It should live outside agent Sandboxes, behind a private network or HTTPS, with a separate factory-controlled credential path where possible.

## Recommended factory boundary

```text
Factory controller / durable database
    ├── ticket lock and lifecycle
    ├── worker allocation
    ├── Git credentials and PR credentials
    ├── acceptance/review policy
    ├── artifacts and logs
    └── cleanup/reconciliation
             │
             ▼
      Agent Sandbox / SandboxClaim
             │
             ├── repo checkout and working tree
             ├── ompig-slim durable run/resume
             ├── durable state directory
             └── project-specific toolchain/build environment
```

The factory should invoke the worker roughly as follows:

```sh
ompig-slim durable run \
  --state /factory/state \
  --cwd /workspace/repo \
  --model provider/model \
  --request-id factory-run-<immutable-id> \
  --max-turns 100 \
  'Implement ticket ...; run the required checks; inspect the final diff; report blockers honestly.'
```

After a Pod/process restart, the Sandbox command should become `durable resume --state /factory/state`. The state and repository must be mounted at the same absolute paths. Persist the complete state directory, including SQLite WAL files, and the checkout including `.git`, untracked files, and edits. The storage class must support SQLite locking/WAL and OS advisory locks; this needs deployment testing, not assumption.

## What the factory must own

`ompig-slim` explicitly does not claim to be a ticket queue, GitHub workflow controller, approval service, or self-restarting daemon. The factory must own:

1. Ticket acquisition and distributed locking.
2. Repository checkout, branch naming, and Git identity.
3. Short-lived Git/provider credentials.
4. Test/build/review stages and their acceptance rules.
5. Draft PR creation and idempotent PR updates.
6. Worker timeout, cancellation, retry, and node-loss handling.
7. Durable artifact/log storage.
8. Sandbox/PVC cleanup and reconciliation.
9. External-effect idempotency keys.

The agent prompt should state that ticket orchestration, push, and PR policy belong to the caller. That boundary is already encoded in `internal/workdesk/workdesk.go`.

## Security findings for the planned cluster

- `ompig-slim` has no OS sandbox. The repository's own security policy says containment is the operator's responsibility; Agent Sandbox/gVisor/Kata must provide the containment boundary.
- Codemode and shell tools run with the container user's permissions. Never mount the host Docker socket into the agent.
- The workspace image runs as the unprivileged `agent` user, which is a useful default, but it is not a security boundary against a compromised kernel/runtime.
- Avoid privileged Docker-in-Docker for untrusted tickets. Prefer rootless BuildKit, daemonless builders, or a separate build service. If DinD is unavoidable, isolate it in a distinct runtime profile and treat it as higher risk.
- Do not give the agent long-lived broad GitHub credentials. Let a factory-side service perform push/PR operations, or inject narrowly scoped short-lived credentials.
- A killed host can leave shell descendants alive, and SIGKILL does not roll back file edits, Git actions, API calls, or external jobs. The Sandbox must contain the full process tree and the factory must reconcile external effects before retrying.
- The broker is not appropriate as the sole security boundary for untrusted agents. Its clients can access API keys/access tokens, and its bearer grants broad account mutation authority.

## Suggested first integration milestone

1. Run one k3s worker or one local Kubernetes node.
2. Create one `Sandbox` per factory run using the workspace image plus a project-specific toolchain layer.
3. Mount two durable volumes: `/factory/state` and `/workspace/repo`.
4. Have the factory create the assignment and launch `durable run`.
5. On restart, use `durable resume` and inspect `durable status` without starting the model.
6. Keep Git push and PR creation outside the agent initially.
7. Store the final answer, logs, diff, tests, and PR metadata in the factory database/object storage.
8. Delete the Sandbox and volumes in every terminal path; reconcile deletion asynchronously.
9. Add warm pools only after cold-start cleanup and credential isolation are proven.

## Open questions before implementation

- Which ticket and Git provider(s) are required first?
- Does the factory need the agent to push branches, or can a privileged sidecar/service do that?
- Which repositories/toolchains must be supported, and can images be prebuilt?
- Are the five machines trusted enough for a shared Kubernetes control plane, and do they support gVisor/Kata prerequisites?
- What artifact/object storage is available outside the workers?
- What is the retry policy after a node loss when the previous agent may have already pushed or opened a PR?
- Is a shared broker acceptable, or should each run receive a factory-issued model credential?

## Primary sources

- Agent Sandbox README: <https://github.com/kubernetes-sigs/agent-sandbox/blob/main/README.md>
- Agent Sandbox API/controller: <https://github.com/kubernetes-sigs/agent-sandbox/tree/main/api> and <https://github.com/kubernetes-sigs/agent-sandbox/tree/main/controllers>
- Local durable command: [`README.md`](../README.md#durable-headless-work)
- Durable guarantees and Kubernetes deployment notes: [`docs/durable.md`](durable.md)
- Durable implementation: [`internal/workdesk/workdesk.go`](../internal/workdesk/workdesk.go)
- Workspace image: [`Dockerfile.workspace`](../Dockerfile.workspace)
- Security boundary: [`SECURITY.md`](../SECURITY.md)
- Broker contract: [`docs/remote-broker.md`](remote-broker.md)
