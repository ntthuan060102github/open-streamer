#!/usr/bin/env bash
# Create N stream definitions in open-streamer using one of the bench payloads.
# The payload is templated with {{CODE}} so each stream gets a unique code.
#
# Usage:
#   bench/scripts/create-streams.sh 10 passthrough
#   bench/scripts/create-streams.sh 4  abr3-legacy
#   bench/scripts/create-streams.sh 4  abr3-multi
#
# Cleanup:
#   bench/scripts/create-streams.sh delete 10
#   bench/scripts/create-streams.sh delete-all
#
set -euo pipefail

BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
API=${API:-http://127.0.0.1:8080}
PREFIX=${PREFIX:-bench}

usage() {
  echo "Usage: $0 <N> <profile>           # profile: passthrough | abr3-legacy | abr3-multi"
  echo "       $0 delete <N>"
  echo "       $0 delete-all              # removes every stream whose code starts with $PREFIX"
  exit 1
}

[[ $# -lt 1 ]] && usage

case $1 in
  delete-all)
    echo "[create-streams] deleting every stream with prefix '$PREFIX'..."
    codes=$(curl -s "$API/streams" | grep -oE '"code"[[:space:]]*:[[:space:]]*"'"$PREFIX"'[^"]*"' | sed -E 's/.*"([^"]+)"$/\1/')
    for c in $codes; do
      curl -s -XDELETE "$API/streams/$c" >/dev/null && echo "  deleted $c"
    done
    exit 0
    ;;
  delete)
    [[ $# -lt 2 ]] && usage
    N=$2
    for i in $(seq 1 "$N"); do
      curl -s -XDELETE "$API/streams/${PREFIX}${i}" >/dev/null && echo "  deleted ${PREFIX}${i}"
    done
    exit 0
    ;;
esac

[[ $# -lt 2 ]] && usage
N=$1
PROFILE=$2
PAYLOAD="$BENCH_ROOT/payloads/${PROFILE}.json"

[[ ! -f "$PAYLOAD" ]] && { echo "[create-streams] missing payload: $PAYLOAD"; exit 1; }

# wait_registered polls /streams/<code> until the pipeline reports a non-stopped
# runtime status — that means the push slot is registered and a publisher can
# now connect without being rejected with "no stream registered for key".
wait_registered() {
  local code=$1 deadline=$(( $(date +%s) + 15 ))
  while [[ $(date +%s) -lt $deadline ]]; do
    local status
    status=$(curl -fs "$API/streams/$code" 2>/dev/null \
      | grep -oE '"status"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 \
      | sed 's/.*"\([^"]*\)"$/\1/' 2>/dev/null || true)
    [[ -n "$status" && "$status" != "stopped" ]] && return 0
    sleep 0.2
  done
  return 1
}

echo "[create-streams] creating $N streams from $PAYLOAD..."
fail=0
for i in $(seq 1 "$N"); do
  code="${PREFIX}${i}"
  body=$(sed -e "s/{{CODE}}/$code/g" -e "s/{{RTMP_PORT}}/$RTMP_PORT/g" "$PAYLOAD")
  http=$(curl -s -o /tmp/cs.out -w "%{http_code}" \
    -XPOST "$API/streams/$code" \
    -H 'Content-Type: application/json' \
    -d "$body")
  if [[ "$http" != "200" && "$http" != "201" ]]; then
    echo "  $code → HTTP $http"
    cat /tmp/cs.out
    fail=$((fail + 1))
    continue
  fi
  if ! wait_registered "$code"; then
    echo "  $code → registered timeout (still 'stopped' after 15s)"
    fail=$((fail + 1))
  fi
done

ok=$((N - fail))
echo "[create-streams] done: $ok ok, $fail failed (after registration check)"
[[ $fail -eq 0 ]]
