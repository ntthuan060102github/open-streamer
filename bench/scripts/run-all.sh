#!/usr/bin/env bash
# Full benchmark sweep — runs Phase A/B/C end-to-end, summarises each run,
# and aggregates everything into one master report.
#
# Phase D (failover) and Phase E (DVR) require scenario-specific manipulation
# and are intentionally skipped here — run them manually after this completes.
#
# Usage:
#   bench/scripts/run-all.sh                # SWEEP = <tag>-<date>
#   bench/scripts/run-all.sh baseline       # SWEEP = <tag>-<date>-baseline
#   NOTE=stress bench/scripts/run-all.sh    # same as positional arg
#   SWEEP=manual-name bench/scripts/run-all.sh   # full override
#
# Auto-composed name examples:
#   v0.0.31-2026-04-27                  (HEAD exactly on tag v0.0.31)
#   v0.0.31-2026-04-27-baseline         (note = "baseline")
#   v0.0.31-3-g869cb6c-2026-04-27       (3 commits past v0.0.31)
#   dev-2026-04-27-baseline             (no tag in repo)
#
# Resume / partial:
#   PLAN="A2 B3 C3" bench/scripts/run-all.sh
#
set -euo pipefail

BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
SCRIPTS=$BENCH_ROOT/scripts

detect_tag() {
  local repo=$BENCH_ROOT/..
  local tag
  tag=$(cd "$repo" && git describe --tags --exact-match HEAD 2>/dev/null) && { echo "$tag"; return; }
  tag=$(cd "$repo" && git describe --tags 2>/dev/null) && { echo "$tag"; return; }
  echo "dev"
}

# Compose SWEEP if user did not pin it explicitly
if [[ -z "${SWEEP:-}" ]]; then
  TAG=$(detect_tag)
  DATE=$(date +%Y-%m-%d)
  NOTE=${NOTE:-${1:-}}
  if [[ -n "$NOTE" ]]; then
    SWEEP="$TAG-$DATE-$NOTE"
  else
    SWEEP="$TAG-$DATE"
  fi
fi

COOLDOWN=${COOLDOWN:-30}
API=${API:-http://127.0.0.1:8080}
SKIP_FAILOVER=${SKIP_FAILOVER:-0}
LOGDIR=$BENCH_ROOT/results/$SWEEP
LOG=$LOGDIR/run-all.log

mkdir -p "$LOGDIR"

log()  { echo "[$(date +%H:%M:%S)] $*" | tee -a "$LOG"; }
notify() { "$SCRIPTS"/notify.sh "$1" 2>/dev/null || true; }
fail() {
  log "FATAL: $*"
  notify "❌ Bench *${SWEEP}* aborted
$*

Log: \`$LOG\`"
  exit 1
}

# Extract the markdown row a per-run summary.md emits for aggregate.sh.
extract_summary_row() {
  awk '
    /^## Row to paste into the master report/ {found=1; next}
    found && /^```markdown/ {inblk=1; next}
    inblk && /^```/ {exit}
    inblk {print; exit}
  ' "$1" 2>/dev/null
}

# Send a per-step notification by scanning the run's summary.md for verdict.
notify_run() {
  local id=$1
  local sum="$BENCH_ROOT/results/$id/summary.md"
  local emoji verdict row

  if [[ ! -f "$sum" ]]; then
    notify "❓ \`$id\` finished — no summary.md"
    return
  fi

  if grep -qE '\| \*\*FAIL\*\* \||^\*\*FAIL\*\*' "$sum"; then
    verdict="FAIL"; emoji="⚠️"
  elif grep -qE '\| \*\*PASS\*\* \||^\*\*PASS\*\*' "$sum"; then
    verdict="PASS"; emoji="✅"
  else
    verdict="UNKNOWN"; emoji="❓"
  fi

  row=$(extract_summary_row "$sum")
  if [[ -n "$row" ]]; then
    notify "$emoji \`$id\` $verdict
\`\`\`
$row
\`\`\`"
  else
    notify "$emoji \`$id\` $verdict"
  fi
}

api_check() {
  curl -fs --max-time 5 "$API/streams" >/dev/null \
    || fail "open-streamer not responding at $API"
}

set_multi_output() {
  local val=$1
  log "config → multi_output=$val"
  curl -fs -X POST "$API/config" -H 'Content-Type: application/json' \
    -d "{\"transcoder\":{\"multi_output\":$val}}" >/dev/null \
    || log "WARN: failed to toggle multi_output (continuing)"
}

# Full plan — (id N profile [pre-hook])
declare -a PLAN_ALL=(
  # Phase A — passthrough
  "A1 1  passthrough  noop"
  "A2 10 passthrough  noop"
  "A3 25 passthrough  noop"
  # Phase B — legacy ABR
  "B1 1  abr3-legacy  legacy"
  "B2 1  abr3-legacy  legacy"
  "B3 4  abr3-legacy  legacy"
  "B4 8  abr3-legacy  legacy"
  # Phase C — multi-output
  "C2 1  abr3-multi   multi"
  "C3 4  abr3-multi   multi"
  "C4 8  abr3-multi   multi"
)

# Filter by env PLAN if provided (space-separated run IDs)
declare -a PLAN
if [[ -n "${PLAN:-}" ]]; then
  for line in "${PLAN_ALL[@]}"; do
    id=${line%% *}
    [[ " $PLAN " == *" $id "* ]] && PLAN+=("$line")
  done
else
  PLAN=("${PLAN_ALL[@]}")
fi

run_one() {
  local id=$1 n=$2 profile=$3 hook=$4
  log "=== $id  N=$n  profile=$profile  ==="
  api_check

  case "$hook" in
    legacy) set_multi_output false ;;
    multi)  set_multi_output true ;;
    *) ;;
  esac

  if "$SCRIPTS"/run-bench.sh "$id" "$n" "$profile" >>"$LOG" 2>&1; then
    log "  run completed"
  else
    log "  RUN FAILED — see $LOG (continuing sweep)"
  fi

  if "$SCRIPTS"/summarize.sh "$id" >>"$LOG" 2>&1; then
    log "  summary: results/$id/summary.md"
  else
    log "  WARN: summarize failed for $id"
  fi

  notify_run "$id"

  log "  cooldown ${COOLDOWN}s..."
  sleep "$COOLDOWN"
}

# ===== preflight =====
PHASE_D_NOTE=""
[[ "$SKIP_FAILOVER" != "1" ]] && PHASE_D_NOTE=" + Phase D"
log "=== sweep '$SWEEP' starting (${#PLAN[@]} A/B/C runs$PHASE_D_NOTE) ==="
log "  log:    $LOG"
log "  api:    $API"
log "  report: $BENCH_ROOT/reports/$SWEEP/report.md (generated at end)"
notify "🚀 Bench *${SWEEP}* started
${#PLAN[@]} A/B/C runs${PHASE_D_NOTE}
Host: \`$(hostname)\`"
api_check

if [[ ! -f "$BENCH_ROOT/assets/sample-1080p.ts" ]]; then
  log "missing assets/sample-1080p.ts → running prepare.sh sample"
  "$SCRIPTS"/prepare.sh sample >>"$LOG" 2>&1 || fail "prepare.sh sample failed"
fi

# Capture sysinfo once for the whole sweep
"$SCRIPTS"/prepare.sh sysinfo >>"$LOG" 2>&1
mv "$BENCH_ROOT"/results/sysinfo-*.txt "$LOGDIR/sysinfo.txt" 2>/dev/null || true

# ===== execute plan =====
START=$(date +%s)
for line in "${PLAN[@]}"; do
  read -r id n profile hook <<<"$line"
  run_one "$id" "$n" "$profile" "$hook"
done

# Reset config to baseline
set_multi_output false

# ===== Phase D — failover =====
if [[ "$SKIP_FAILOVER" != "1" ]]; then
  log "=== Phase D — failover scenarios ==="
  for d_case in d1 d2 d3 d4; do
    log "  → $d_case"
    if "$SCRIPTS"/run-failover.sh "$d_case" >>"$LOG" 2>&1; then
      log "    $d_case completed"
    else
      log "    $d_case FAILED — see $LOG"
    fi
    notify_run "$(echo "$d_case" | tr a-z A-Z)"
  done
fi

# ===== aggregate =====
log "=== aggregating master report ==="
if "$SCRIPTS"/aggregate.sh "$SWEEP" >>"$LOG" 2>&1; then
  log "report: bench/reports/$SWEEP/report.md (committable)"
else
  log "WARN: aggregate.sh failed — per-run summaries still available"
fi

DUR=$(( $(date +%s) - START ))

# Tally per-run verdicts from each results/<id>/summary.md
PASS_COUNT=0; FAIL_COUNT=0; FAILED_RUNS=""
for sum in "$BENCH_ROOT"/results/*/summary.md; do
  [[ -f "$sum" ]] || continue
  rid=$(basename "$(dirname "$sum")")
  if grep -qE '^\| Verdict \| \*\*FAIL\*\*|^\*\*FAIL\*\*' "$sum" 2>/dev/null; then
    FAIL_COUNT=$((FAIL_COUNT + 1))
    FAILED_RUNS="$FAILED_RUNS $rid"
  else
    PASS_COUNT=$((PASS_COUNT + 1))
  fi
done

log "=== sweep complete in $((DUR / 60))m $((DUR % 60))s ==="
log "  PASS: $PASS_COUNT   FAIL: $FAIL_COUNT${FAILED_RUNS:+ ($FAILED_RUNS )}"
log
log "Committable report:  $BENCH_ROOT/reports/$SWEEP/report.md"
log "Local raw artifacts: $LOGDIR/  (gitignored)"

if [[ "$FAIL_COUNT" -eq 0 ]]; then
  notify "✅ Bench *${SWEEP}* done in $((DUR/60))m $((DUR%60))s
PASS: ${PASS_COUNT}  FAIL: 0
Report: \`bench/reports/${SWEEP}/report.md\`"
else
  notify "⚠️ Bench *${SWEEP}* done in $((DUR/60))m $((DUR%60))s
PASS: ${PASS_COUNT}  FAIL: ${FAIL_COUNT}
Failed runs:${FAILED_RUNS}
Report: \`bench/reports/${SWEEP}/report.md\`"
fi
