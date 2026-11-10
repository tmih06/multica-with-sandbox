# Release runbook

## Normal release

Release from a reviewed commit on `main` by creating and pushing a new semantic
version tag such as `v0.18.4`. The Release workflow intentionally has no manual
trigger: a tag push is the only event that can publish binaries, Homebrew
formulae, and container images.

The verification job runs the Go tests and `govulncheck` before any publishing
job starts. The vulnerability scan is fail-closed by default.

CI and release jobs require Go `~1.27.3`: at least 1.27.3, while accepting
newer 1.27 patches. This security floor is independent of the development
minimum in `server/go.mod`. A plain `1.27.x` can resolve to an older patch
while the `setup-go` version manifest catches up with an official Go release.
When raising the security floor, update all Go workflows and the pinned
Go builder image in `Dockerfile` together so shipped binaries also receive
the fixes.

## Emergency vulnerability-scan bypass

Use the bypass only when `govulncheck` itself or its live vulnerability database
is unavailable, or when maintainers have documented a confirmed false positive
that blocks an urgent release. Never use it to publish a release with an
unresolved reachable vulnerability.

1. Record the reason and maintainer approval in the release issue or pull
   request, and confirm no other release is in progress.
2. In **Settings → Secrets and variables → Actions → Variables**, set the
   repository variable `ALLOW_VULN_BYPASS_FOR_TAG` to the exact release tag,
   for example `v0.18.4`.
3. Re-run the failed Release workflow for that tag. A different tag, an empty
   value, or any typo keeps the scan enabled.
4. Confirm the verification log contains the explicit bypass warning and retain
   the workflow URL in the incident record.
5. Delete `ALLOW_VULN_BYPASS_FOR_TAG` immediately after the release run
   completes. The tag-scoped value prevents a concurrent release with another
   tag from inheriting the bypass.

Every Go binary retains its compiler version in the standard Go build metadata;
use `go version -m <binary>` when auditing a downloaded release artifact.

## Releasing from a fork

The workflow derives every publication target from `github.repository`, so a
tag pushed to a fork publishes to that fork and never to upstream. Nothing in
the repository needs editing to release from a fork.

| Artifact | On a fork |
|---|---|
| GitHub Release with CLI archives + `checksums.txt` | Published to the fork |
| Container images | `ghcr.io/<fork-owner>/multica-{backend,web}`, tagged `<tag>`, `sha-*`, and `latest` for stable tags |
| Helm chart | `oci://ghcr.io/<fork-owner>/charts/multica` |
| Desktop installers (Linux/Windows) | Uploaded to the fork's release |
| Homebrew formula | Built into `dist/` but **not** pushed: forks have no `HOMEBREW_TAP_GITHUB_TOKEN`, which sets `RELEASE_HOMEBREW_ENABLED=false` and makes `brews[].skip_upload` skip the tap |

A fork needs no extra secrets: the default `GITHUB_TOKEN` covers the release,
GHCR, and Desktop uploads. To publish a Homebrew formula too, add a
`HOMEBREW_TAP_GITHUB_TOKEN` secret and set the `BREW_TAP_OWNER`/`BREW_TAP_REPO`
variables to the tap repository.

Two consequences of releasing from a fork:

- **Binaries follow their own repository.** Release builds stamp
  `MULTICA_RELEASE_REPO` into the CLI, so `multica update` and the daemon's
  auto-updater poll the fork's releases. Without this, a fork binary at a
  version below upstream's latest would silently replace itself with the
  upstream release. The value can still be overridden at runtime, and a
  malformed slug falls back to the stamped repository.
- **Pick a version above upstream's, or suffix it.** `IsNewerVersion` compares
  only major.minor.patch and rejects any non-numeric tail, so a tag like
  `v0.8.3-sandbox.1` makes auto-update treat the build as current instead of
  newer or older. A plain `v0.1.0` on a fork is far below upstream's line and
  compares unfavourably for anything still pointed at upstream releases.

CI runs on both `main` and `master`, so a fork whose default branch is
`master` still gets branch CI. Release jobs are tag-triggered and have no
branch filter.
