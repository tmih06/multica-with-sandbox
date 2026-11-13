#!/bin/sh
set -eu

factory_url=${FACTORY_URL:-http://127.0.0.1:8081}
count=${TASK_COUNT:-3}
ids_file=${TMPDIR:-/tmp}/software-factory-ticket-ids.$$
trap 'rm -f "$ids_file"' EXIT

: >"$ids_file"
for i in $(seq 1 "$count"); do
  response=$(curl -fsS -X POST "$factory_url/tickets" \
    -H 'content-type: application/json' \
    -d "{\"title\":\"Compose concurrency check $i\",\"prompt\":\"Run the configured worker task for check $i\",\"model\":\"${FACTORY_MODEL:-}\",\"repository\":\"${FACTORY_REPOSITORY:-}\"}")
  id=$(printf '%s' "$response" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  test -n "$id"
  printf '%s\n' "$id" >>"$ids_file"
done

for _ in $(seq 1 180); do
  pending=0
  while IFS= read -r id; do
    status=$(curl -fsS "$factory_url/tickets/$id" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')
    case "$status" in
      finished) ;;
      failed)
        echo "ticket $id failed" >&2
        exit 1
        ;;
      *) pending=$((pending + 1)) ;;
    esac
  done <"$ids_file"
  if [ "$pending" -eq 0 ]; then
    echo "three-ticket check passed: $count isolated runs finished"
    exit 0
  fi
  sleep 2
done

echo "three-ticket check timed out" >&2
exit 1
