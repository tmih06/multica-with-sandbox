# Kubernetes Agent Sandbox research

Research date: 2026-11-13

Primary repository: <https://github.com/kubernetes-sigs/agent-sandbox>

## Executive summary

Agent Sandbox is a Kubernetes **sandbox orchestrator**, not a container-isolation runtime. Its core `Sandbox` CRD manages one stateful, singleton workload and its backing Pod. It delegates low-level isolation to a Kubernetes `RuntimeClass`, such as gVisor or Kata Containers.

For this software factory, it is a good lifecycle layer around one AI coding run:

```text
FactoryRun
  -> SandboxClaim or Sandbox
      -> Pod + optional headless Service + PVCs
          -> RuntimeClass isolation
              -> ompig-slim durable run/resume
```

The factory must remain responsible for ticket state, Git/PR effects, acceptance policy, credentials, artifacts, distributed retries, and final cleanup.

## Repository structure and responsibilities

The upstream repository is a Kubernetes controller project with these important areas:

- `api/v1beta1`: core `Sandbox` API types.
- `controllers`: core Sandbox reconciler.
- `extensions/api/v1beta1`: extension CRDs such as `SandboxTemplate`, `SandboxClaim`, and `SandboxWarmPool`.
- `extensions/controllers`: extension reconcilers.
- `k8s`: generated/installable Kubernetes manifests.
- `helm`: Helm installation.
- `examples`: usage examples.
- `sandbox-router`: optional HTTP routing component.
- `clients/go` and `clients/python`: SDKs.

Source: [repository tree](https://github.com/kubernetes-sigs/agent-sandbox/tree/main).

## Core `Sandbox` API

A `Sandbox` represents one namespaced stateful workload. The controller creates and manages a backing Pod with a stable identity, rather than a replicated Deployment or numbered StatefulSet.

The `Sandbox` blueprint contains:

- A Pod template.
- Optional persistent-volume-claim templates.
- Optional automatic headless Service creation.
- A failed-Pod policy: retain or recreate.

Runtime lifecycle fields include:

- `operatingMode: Running|Suspended`.
- `shutdownTime`.
- `shutdownPolicy: Retain|Delete`.

Suspension removes the backing Pod while retaining the Sandbox and its volumes. Resuming creates a new Pod from the current template and reattaches retained volumes.

The volume-claim template list is immutable after creation. This matters for factory runs: a run should choose its durable state/repository storage layout at admission time rather than expecting to mutate PVC templates later.

Sources:

- [Sandbox API types](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/api/v1beta1/sandbox_types.go)
- [Agent Sandbox README, Core: Sandbox](https://github.com/kubernetes-sigs/agent-sandbox#core-sandbox)

## Readiness and status

The controller distinguishes desired operation from observed readiness:

- `operatingMode: Running` means the controller should maintain the backing Pod.
- `Ready=True` means the Pod is actually Running and Ready, has an IP, and has the required Service when applicable.
- `Suspended=True` means suspension has completed and the backing Pod is terminated.
- `Finished=True` records a terminal Pod phase when failed-Pod recreation is not being used.
- `PodScheduled` mirrors the backing Pod's scheduling condition.

A factory should wait for `Ready=True`, not merely for the Sandbox object or Pod object to exist. It should also distinguish a normal suspended/expired state from a provisioning or controller error.

Source: [Sandbox condition definitions](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/api/v1beta1/sandbox_types.go).

## Extensions and warm allocation

### `SandboxTemplate`

A reusable workload blueprint. It is the right place to define a standard agent image, security context, resource limits, volumes, and runtime class for a class of factory jobs.

### `SandboxWarmPool`

Maintains a desired number of pre-provisioned Sandboxes from a template. It exposes desired/actual/ready replica counts and can use:

- `Recreate`: immediately replace stale, unclaimed Sandboxes.
- `OnReplenish`: leave stale unclaimed Sandboxes until deletion/claim/replenishment.

Only unclaimed Sandboxes are managed or replaced by the pool. Once claimed, ownership transfers to the `SandboxClaim` and the pool does not replace it.

### `SandboxClaim`

Requests a Sandbox from a named warm pool. The claim can add Pod metadata and environment variables, and can request PVC templates. Environment injection or extra PVCs may force a cold start because an existing warm Pod does not match the requested shape.

For the factory, warm pools can reduce startup latency, but they introduce data-hygiene and credential risks. A warm Sandbox must never contain a previous run's repository, durable state, credentials, shell processes, or build cache that is visible across tenants. Start with cold Sandboxes and add warm pools only after reset and adoption tests exist.

Sources:

- [SandboxTemplate API](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/api/v1beta1/sandboxtemplate_types.go)
- [SandboxWarmPool API](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/api/v1beta1/sandboxwarmpool_types.go)
- [SandboxClaim API](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/api/v1beta1/sandboxclaim_types.go)

## Isolation model

Agent Sandbox does not make an ordinary Pod safe by itself. The README explicitly describes it as an orchestrator that configures Pods for an external secure runtime using `RuntimeClass`.

Recommended profiles for the factory:

- A default unprivileged profile for trusted repositories.
- gVisor for stronger syscall/kernel separation with broad compatibility.
- Kata Containers where VM-style isolation is required and the nodes support its prerequisites.

The runtime choice must be tested on the actual five amd64, 2-vCPU nodes. In particular, Kata may require virtualization support that is unavailable or restricted by the hosting provider.

Source: [Agent Sandbox scope note](https://github.com/kubernetes-sigs/agent-sandbox#scope).

## Docker and build isolation

Agent Sandbox does not solve Docker-in-Docker security. A factory Pod that mounts `/var/run/docker.sock` grants the agent effective control over the node's Docker daemon and defeats the intended boundary.

For untrusted AI-generated code:

1. Prefer rootless BuildKit or a daemonless image builder.
2. Avoid privileged containers.
3. Do not mount host sockets or host filesystems.
4. Use a dedicated runtime class for builds if needed.
5. Apply CPU, memory, ephemeral-storage, process, and network limits.
6. Use short-lived registry credentials and revoke them after the run.

If privileged DinD is unavoidable for compatibility, treat that run as a separate high-risk workload class rather than as equivalent to a normal Sandbox.

This conclusion follows from Agent Sandbox's stated scope: it manages Pods and delegates isolation; it does not claim to provide a universal container escape boundary.

## Recommended factory resource shape

The five remote machines are all amd64 with 2 vCPU and 12 GB RAM. Agent Sandbox does not pool memory or CPU beyond Kubernetes scheduling, so capacity is limited by each node's allocatable resources.

Start with:

- One active factory Sandbox per worker node.
- Explicit requests and limits on every container.
- A reserved amount for k3s/Kubernetes, networking, logging, and image pulls.
- A separate control-plane capacity budget.
- Ephemeral storage limits to prevent repository/build exhaustion.

A practical first profile is approximately 1 to 1.5 vCPU and 4 to 6 GB memory per active agent, adjusted after measuring real repositories. This leaves headroom for system processes and test/build bursts, but may still permit only one meaningful run per node.

The controller's standard readiness signal should be combined with factory-level heartbeats and a wall-clock timeout. A Ready Pod can still contain a hung agent or runaway child process.

## Factory integration design

Create the Sandbox from a factory-owned run identifier and label every resource:

```text
factory.example.com/run-id=<immutable-run-id>
factory.example.com/repository=<safe-hash>
factory.example.com/phase=agent
```

Use owner references where appropriate, but do not rely on garbage collection alone. The factory should reconcile cleanup after:

- Successful PR creation.
- Agent failure.
- Timeout.
- User cancellation.
- Node loss.
- Controller restart.

Persist two logical data sets:

1. `/factory/state`: `ompig-slim durable` assignment and SQLite/WAL.
2. `/workspace/repo`: Git checkout, `.git`, untracked files, and edits.

Use PVCs only when resumption across Pod replacement is required. Otherwise, reconstructing from Git and external artifacts reduces cleanup risk. The storage implementation must support SQLite WAL and advisory file locks.

A controller restart should inspect Sandbox status and durable status independently:

```text
Sandbox Ready=False + durable pending  -> infrastructure recovery
Sandbox Ready=True  + durable working  -> agent execution
Sandbox Ready=True  + durable done      -> factory acceptance
Sandbox deleted     + durable pending  -> recreate/resume according to policy
```

Do not interpret `durable done` as “tests passed” or “PR approved”; that is explicitly caller-owned policy in the worker contract.

## Network and routing

The optional Sandbox Router is useful when clients cannot directly reach Pod IPs, including some gVisor/Kata deployments. It should not be required for the first factory version if the factory controller can observe Kubernetes and collect logs through the API.

If agents need outbound Git/model/registry access, use egress policy and private connectivity. If external clients need to reach a running Sandbox, expose only the required port and identity-check the request at the factory/router layer.

Source: [Sandbox Router directory](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/sandbox-router).

## Suggested rollout

### Phase 1: one cold Sandbox

- Install core CRDs/controller only.
- Create one Sandbox manually.
- Run the `ompig-slim` workspace image as an unprivileged user.
- Verify Pod readiness, logs, timeout, deletion, and PVC cleanup.
- Use no warm pool and no privileged DinD.

### Phase 2: durable worker recovery

- Mount durable state and repository storage.
- Launch `durable run`.
- Restart the Pod and launch `durable resume`.
- Verify SQLite/WAL behavior on the selected storage class.
- Test node loss and orphaned child processes.

### Phase 3: factory lifecycle

- Add `FactoryRun` state and distributed ticket locks.
- Add idempotent branch/commit/PR operations outside the agent.
- Add acceptance stages, artifact retention, and asynchronous cleanup.
- Add node labels, quotas, and per-run limits.

### Phase 4: warm pools

- Add a `SandboxTemplate` and a small `SandboxWarmPool`.
- Prove that claims receive a pristine workspace.
- Prove credentials and process trees are absent before claim.
- Measure cold versus warm latency and memory overhead.
- Only then increase pool size.

## Bottom line

Agent Sandbox is a strong Kubernetes lifecycle primitive for the factory, especially because it models a stateful singleton workspace rather than a replicated service. It should sit below a durable factory controller and above the actual runtime/security profile.

The safe division of responsibility is:

```text
Factory: ticket → policy → allocation → Git/PR → acceptance → cleanup
Agent Sandbox: Pod/workspace lifecycle and status
RuntimeClass: kernel/container isolation
ompig-slim durable: resumable model/tool execution
Build system: controlled image/build execution
```

## Primary sources

- [Agent Sandbox repository](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox README](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/README.md)
- [Core Sandbox API](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/api/v1beta1/sandbox_types.go)
- [Core Sandbox controller](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/controllers/sandbox_controller.go)
- [SandboxClaim API](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/extensions/api/v1beta1/sandboxclaim_types.go)
- [SandboxWarmPool API](https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/main/extensions/api/v1beta1/sandboxwarmpool_types.go)
- [Kubernetes RuntimeClass documentation](https://kubernetes.io/docs/concepts/containers/runtime-class/)
