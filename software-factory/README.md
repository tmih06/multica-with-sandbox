# Software Factory MVP

This MVP accepts a ticket over HTTP, runs one ticket at a time in a Kubernetes Agent Sandbox using the `ompig-slim` workspace image, waits for the agent Pod to finish, returns its logs as the answer, and deletes the Sandbox.

It intentionally stops before Git push or PR creation.

## Run against the local simulation

The current local cluster uses the `kind-factory` kubeconfig context:

```sh
KUBECONFIG="$HOME/.kube/config" \
KUBECTL=/tmp/factory-tools/kubectl \
go run ./cmd/factory \
  -namespace factory-test \
  -addr :8080 \
  -broker-url http://factory-broker.factory-system.svc.cluster.local:8765 \
  -broker-token-secret factory-broker-client
```

The factory stores tickets in SQLite. By default it permits two concurrent
Sandbox runs, matching the two-worker simulation. Tickets are dispatched through
a bounded queue and one worker loop is created per capacity slot. Override this
with `-capacity` or `FACTORY_CAPACITY`. Override the default `factory.db` path with
`-db` or `FACTORY_DB`; keep this file on durable storage in a cluster.

The namespace must exist first:

```sh
/tmp/factory-tools/kubectl --context kind-factory create namespace factory-test
```

Run the process with the same kubeconfig context that owns the cluster:

```sh
kubectl config use-context kind-factory
KUBECTL=/tmp/factory-tools/kubectl go run ./cmd/factory -namespace factory-test
```

The default image is `ghcr.io/tmih06/ompig-slim-workspace:v0.1.5`. Override it with `-image` or `FACTORY_IMAGE`.

To run the factory as a Sandbox-backed Multica runtime, provide a Multica PAT
with access to the target workspace. The factory registers one runtime, claims
up to three tasks at a time, and reports terminal results through Multica's
existing daemon task protocol:

```sh
MULTICA_SERVER_URL=http://multica-api:8080 \
MULTICA_TOKEN=pat_... \
MULTICA_WORKSPACE_ID=<workspace-uuid> \
MULTICA_PROVIDER=omp \
FACTORY_CAPACITY=3 \
go run ./cmd/factory -namespace factory-test
```

The bridge keeps Multica as the source of truth for issues and task state. Each
claimed task gets its own Agent Sandbox and `/workspace/repo`; Kubernetes
spreads concurrent Sandboxes across worker hostnames when capacity exists.
`MULTICA_REPOSITORY` is only a fallback for tasks whose claim has no repository.

To build and run the local Multica web/API/factory integration stack:

```sh
export KUBECONFIG_HOST_PATH="$HOME/.kube/config"
export MULTICA_TOKEN=pat_...
export MULTICA_WORKSPACE_ID=<workspace-uuid>
docker compose -f docker-compose.multica.yml up --build
```

This starts PostgreSQL, the Multica backend, the Multica frontend, and the
factory. The factory container uses the mounted kubeconfig to create Sandboxes
in the external cluster. The cluster must already have the Agent Sandbox CRD,
controller, and at least three eligible worker nodes. Open the frontend at
`http://localhost:3000`; the factory health endpoint is `http://localhost:8081/healthz`.

When broker flags are set, each Sandbox receives the private broker origin and a
mounted Kubernetes Secret at `/run/secrets/broker/token`. `ompig-slim` starts with
that broker configuration and synchronizes the broker-owned `models.json`. The
broker Secret must exist in the factory namespace; its value is never placed in a
ticket or committed file. The broker owns the provider API key; the worker only
selects the model name.

## Submit and inspect a ticket

A public repository can be used for the first end-to-end test:

```sh
curl -X POST http://127.0.0.1:8080/tickets \
  -H 'content-type: application/json' \
  -d '{
    "title": "Inspect the repository",
    "repository": "https://github.com/tmih06/ompig-slim.git",
    "model": "deploy/deepseek-ai/deepseek-v4.1-flash-1017",
    "prompt": "Inspect the repository and report its top-level architecture. Do not modify files."
  }'
```

The response includes an ID such as `ticket-1`. Poll it:

```sh
curl http://127.0.0.1:8080/tickets/ticket-1
```

A ticket ends as `finished` or `failed`. `finished` means the worker Pod completed successfully; the factory does not claim that a ticket is correct or approved.

## MVP interface

- `GET /healthz`
- `POST /tickets`
- `GET /tickets/{id}`
- `GET /tickets/{id}/logs`
- `GET /tickets/{id}/events` (Server-Sent Events)

The events endpoint replays persisted lifecycle events and then streams new events. Send `Last-Event-ID` to resume after a disconnect.

The logs endpoint returns the factory lifecycle markers and captured Pod stdout/stderr. It is the audit trail for the MVP; the exact internal Durable tool-event stream is not exposed yet. Ticket state is persisted in SQLite and active runs are reconciled on startup.

`POST /tickets` requires `title` and `prompt`. Optional fields are `repository`, `model`, and `policy`. Policy supports `thinking`, `max_turns`, `timeout_seconds`, `cpu_request`, `memory_request`, and `network_profile`. The effective policy is persisted with the ticket. `model` must use `provider/model` format when supplied.

## What is deliberately missing

- Authentication and authorization
- Ticket-provider webhooks or polling
- Private repository credentials
- Git branch/commit/push
- PR creation or approval
- Durable PVC-backed recovery after Pod loss
- Build isolation such as rootless BuildKit
- Admission policy and tenant isolation

## Questions this MVP forces us to answer

1. Which ticket API is authoritative, and how do we lock a ticket against another worker?
2. Which model credential may a Sandbox use, and how is it scoped and revoked?
3. Should Git push happen inside the Sandbox or through a factory-side Git adapter?
4. What evidence makes a run acceptable: tests, review, diff size, or human approval?
5. What should happen when the Pod dies after an external side effect but before the factory receives the result?
6. Which repositories need language-specific images or a separate build service?
7. How long should logs and workspaces be retained after a finished run?
