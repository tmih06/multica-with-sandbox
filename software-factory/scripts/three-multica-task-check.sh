#!/bin/sh
set -eu

api=${MULTICA_API_URL:-http://127.0.0.1:8080}
token=${MULTICA_TOKEN:?Set MULTICA_TOKEN}
workspace=${MULTICA_WORKSPACE_ID:?Set MULTICA_WORKSPACE_ID}
agent_ids=${MULTICA_AGENT_IDS:?Set MULTICA_AGENT_IDS to three comma-separated agent IDs}

old_ifs=$IFS
IFS=,
# Intentionally split the comma-separated agent list after changing IFS.
# shellcheck disable=SC2086
set -- $agent_ids
IFS=$old_ifs
[ "$#" -eq 3 ] || { echo "MULTICA_AGENT_IDS must contain exactly three IDs" >&2; exit 2; }

ids_file=${TMPDIR:-/tmp}/multica-task-check.$$
trap 'rm -f "$ids_file"' EXIT
: >"$ids_file"

for i in 1 2 3; do
  response=$(curl -fsS -X POST "$api/api/issues?workspace_id=$workspace" \
    -H "Authorization: Bearer $token" -H "X-Workspace-ID: $workspace" \
    -H 'content-type: application/json' \
    -d "{\"title\":\"Sandbox behavior check $i\",\"description\":\"Prepare the isolated workspace and report completion.\",\"status\":\"todo\",\"assignee_type\":\"agent\",\"assignee_id\":\"$1\"}")
  printf '%s\n' "$response" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' >>"$ids_file"
  shift
 done

for _ in $(seq 1 120); do
  pending=0
  while IFS= read -r issue_id; do
    runs=$(curl -fsS "$api/api/issues/$issue_id/task-runs" \
      -H "Authorization: Bearer $token" -H "X-Workspace-ID: $workspace")
    status=$(printf '%s' "$runs" | python3 -c 'import json,sys; x=json.load(sys.stdin); print(x[-1].get("status", "missing") if x else "missing")')
    case "$status" in
      completed) ;;
      failed|cancelled)
        echo "issue $issue_id reached $status" >&2
        exit 1
        ;;
      *) pending=$((pending + 1)) ;;
    esac
  done <"$ids_file"
  if [ "$pending" -eq 0 ]; then
    echo "Multica three-task behavior check passed"
    exit 0
  fi
  sleep 2
done

echo "Multica three-task behavior check timed out" >&2
exit 1
