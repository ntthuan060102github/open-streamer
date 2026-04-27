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

COOLDOWN=${COOLDOWN:-10}
WARMUP=${WARMUP:-30}
SAMPLE_SEC=${SAMPLE_SEC:-120}
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

# Human-readable description for each run id, surfaced in Telegram messages
# so the reader doesn't have to keep the full plan in their head.
run_description() {
  local id=$1 n=${2:-} profile=${3:-}
  case "$id" in
    D1) echo "2 inputs, kill primary publisher → measure switch latency"; return ;;
    D2) echo "1 ABR stream, kill -9 transcoder ffmpeg → measure restart"; return ;;
    D3) echo "1 stream, hot-add push destination → verify HLS continuity"; return ;;
    D4) echo "push to dead sink → observe Reconnecting state machine"; return ;;
  esac
  case "$profile" in
    passthrough)         echo "$n stream(s), passthrough (no transcode)" ;;
    abr2-legacy)         echo "$n stream(s), NVENC ABR 1080p+720p, legacy mode (1 ffmpeg per rendition)" ;;
    abr2-multi)          echo "$n stream(s), NVENC ABR 1080p+720p, multi-output (1 ffmpeg/stream)" ;;
    abr2-multi-hlsdash)  echo "$n stream(s), NVENC multi-output + HLS+DASH outputs" ;;
    abr2-x264)           echo "$n stream(s), libx264 CPU encoder ABR 1080p+720p" ;;
    *)                   echo "$n stream(s), $profile" ;;
  esac
}

# Format a markdown table row as vertical key:value lines.
# $1 = row text starting with "|", $2.. = column labels in order.
# A row like "| A | 2 | foo |" splits into "", " A ", " 2 ", " foo ", "" so the
# first value sits at index 1. We pair each key with parts[i+1].
format_kv() {
  local row=$1; shift
  local -a keys=("$@")
  local IFS='|'
  local -a parts
  read -ra parts <<<"$row"
  local i out=""
  for ((i=0; i<${#keys[@]}; i++)); do
    local v="${parts[$((i+1))]:-}"
    v="${v#"${v%%[![:space:]]*}"}"
    v="${v%"${v##*[![:space:]]}"}"
    out+=$(printf '%-12s %s' "${keys[$i]}:" "$v")
    out+=$'\n'
  done
  printf '%s' "$out"
}

# Pull the verdict reason from a summary.md (works for SATURATED + FAIL).
# A/B/C/F/H summarize.sh format: line `**<VERDICT>** — <reason>`
# Phase D run-failover.sh format: table `| Result | **<result>** |`
extract_verdict_reason() {
  local sum=$1 id=$2
  if [[ "$id" =~ ^D ]]; then
    grep -E '\| Result \| \*\*' "$sum" 2>/dev/null | head -1 \
      | sed -E 's/.*\| \*\*([^*]+)\*\* \|.*/\1/' || true
  else
    grep -E '^\*\*(SATURATED|FAIL|PASS)\*\* —' "$sum" 2>/dev/null | head -1 \
      | sed -E 's/^\*\*[A-Z]+\*\* — //; s/;$//' || true
  fi
}

# Send a per-step notification by scanning the run's summary.md for verdict.
notify_run() {
  local id=$1 n=${2:-} profile=${3:-}
  local sum="$BENCH_ROOT/results/$id/summary.md"
  local emoji verdict row desc reason=""

  desc=$(run_description "$id" "$n" "$profile")

  if [[ ! -f "$sum" ]]; then
    notify "❓ \`$id\` finished — no summary.md
${desc}
Reason: case did not produce a summary (likely create_stream / push setup error — check run-all.log)"
    return
  fi

  if grep -qE '\| \*\*FAIL\*\* \||^\*\*FAIL\*\*' "$sum"; then
    verdict="FAIL"; emoji="❌"
    reason=$(extract_verdict_reason "$sum" "$id")
  elif grep -qE '\| \*\*SATURATED\*\* \||^\*\*SATURATED\*\*' "$sum"; then
    verdict="SATURATED"; emoji="📈"
    reason=$(extract_verdict_reason "$sum" "$id")
  elif grep -qE '\| \*\*PASS\*\* \||^\*\*PASS\*\*' "$sum"; then
    verdict="PASS"; emoji="✅"
  else
    verdict="UNKNOWN"; emoji="❓"
    reason="verdict could not be parsed from summary"
  fi

  row=$(extract_summary_row "$sum")
  local reason_line=""
  [[ -n "$reason" ]] && reason_line=$'\n'"Reason: ${reason}"

  if [[ -z "$row" ]]; then
    notify "$emoji \`$id\` $verdict — ${desc}${reason_line}"
    return
  fi

  local body
  if [[ "$id" =~ ^D ]]; then
    body=$(format_kv "$row" "Case" "Description" "Measure" "Result" "Verdict")
  else
    body=$(format_kv "$row" \
      "Run" "N" "Ladder" "CPU%" "RAM(MB)" "RAM%" "GPU%" "Enc%" "Dec%" "VRAM(MB)" "Restart" "Verdict")
  fi

  notify "$emoji \`$id\` $verdict — ${desc}${reason_line}
\`\`\`
${body}\`\`\`"
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
# Pre-hook flips global config: legacy → multi_output=false, multi → true.
#
# Phases A, B, C, F, H are "load-ceiling" phases — execution auto-stops on
# the first SATURATED (resource ceiling found) or FAIL. Phase D is behavior
# testing — every case runs unless a FAIL aborts the whole sweep.
#
# Steps were widened (B/C/F/H step 4 instead of 2) because the previous CPU
# accounting bug under-reported capacity; with the fix in sample.sh, the
# real ceiling is much higher than the original 8-stream cap suggested.
declare -a PLAN_ALL=(
  # Phase A — passthrough, runs until first ceiling (max 6)
  "A1 1    passthrough           noop"
  "A2 25   passthrough           noop"
  "A3 50   passthrough           noop"
  "A4 100  passthrough           noop"
  "A5 150  passthrough           noop"
  "A6 200  passthrough           noop"

  # Phase B — Legacy ABR NVENC, step 4 (max 7)
  "B1 2    abr2-legacy           legacy"
  "B2 6    abr2-legacy           legacy"
  "B3 10   abr2-legacy           legacy"
  "B4 14   abr2-legacy           legacy"
  "B5 18   abr2-legacy           legacy"
  "B6 22   abr2-legacy           legacy"
  "B7 28   abr2-legacy           legacy"

  # Phase C — Multi-output NVENC, step 4 (max 7)
  "C1 2    abr2-multi            multi"
  "C2 6    abr2-multi            multi"
  "C3 10   abr2-multi            multi"
  "C4 14   abr2-multi            multi"
  "C5 18   abr2-multi            multi"
  "C6 22   abr2-multi            multi"
  "C7 28   abr2-multi            multi"

  # Phase F — CPU encoder fallback (libx264), step 2 (heavy, finer) (max 5)
  "F1 1    abr2-x264             legacy"
  "F2 2    abr2-x264             legacy"
  "F3 4    abr2-x264             legacy"
  "F4 6    abr2-x264             legacy"
  "F5 8    abr2-x264             legacy"

  # Phase H — Multi-protocol HLS+DASH overhead, step 4 (max 5)
  "H1 1    abr2-multi-hlsdash    multi"
  "H2 4    abr2-multi-hlsdash    multi"
  "H3 8    abr2-multi-hlsdash    multi"
  "H4 12   abr2-multi-hlsdash    multi"
  "H5 16   abr2-multi-hlsdash    multi"
)

# Phases that auto-stop on the first SATURATED/FAIL within them.
# Phase D is behavior tests — every case is independent and always runs.
auto_stop_phase() {
  case "$1" in
    A|B|C|F|H) return 0 ;;
    *)         return 1 ;;
  esac
}

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

  if "$SCRIPTS"/run-bench.sh "$id" "$n" "$profile" "$SAMPLE_SEC" "$WARMUP" >>"$LOG" 2>&1; then
    log "  run completed"
  else
    log "  RUN FAILED — see $LOG (continuing sweep)"
  fi

  if "$SCRIPTS"/summarize.sh "$id" >>"$LOG" 2>&1; then
    log "  summary: results/$id/summary.md"
  else
    log "  WARN: summarize failed for $id"
  fi

  notify_run "$id" "$n" "$profile"

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
Plan: ${#PLAN[@]} runs${PHASE_D_NOTE}
B/C/F/H auto-stop on first FAIL per phase.
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
# FAILED_PHASES is a string of phase letters that already hit FAIL — used to
# skip subsequent runs of the same auto-stop phase (B/C/F/H) once the load
# ceiling has been found.
START=$(date +%s)
FAILED_PHASES=""
for line in "${PLAN[@]}"; do
  read -r id n profile hook <<<"$line"
  phase=${id:0:1}

  if auto_stop_phase "$phase" && [[ "$FAILED_PHASES" == *"$phase"* ]]; then
    log "=== $id skipped — phase $phase already hit ceiling ==="
    notify "⏭️ \`$id\` skipped — phase $phase ceiling already found"
    continue
  fi

  run_one "$id" "$n" "$profile" "$hook"

  sum="$BENCH_ROOT/results/$id/summary.md"

  # Hard stop: any FAIL aborts the entire sweep — FAIL means the bench
  # itself broke (script error, behavior didn't work), not a resource
  # ceiling, so continuing wastes time and pollutes later results.
  if [[ -f "$sum" ]] && \
     grep -qE '\| \*\*FAIL\*\* \||^\*\*FAIL\*\*' "$sum" 2>/dev/null; then
    log "  ✗ $id FAILED — aborting sweep (no point continuing past a real failure)"
    notify "🛑 Sweep *${SWEEP}* aborted after \`$id\` FAIL
Subsequent runs cancelled. Fix the failure then rerun."
    SWEEP_ABORTED=1
    break
  fi

  # Soft stop per phase: SATURATED means resource ceiling — skip remaining
  # entries of the SAME phase only.
  if [[ -f "$sum" ]] && \
     grep -qE '\| \*\*SATURATED\*\* \||^\*\*SATURATED\*\*' "$sum" 2>/dev/null && \
     auto_stop_phase "$phase"; then
    FAILED_PHASES="$FAILED_PHASES$phase"
    log "  📈 phase $phase ceiling at N=$n; later $phase runs will be skipped"
  fi
done

SWEEP_ABORTED=${SWEEP_ABORTED:-0}

# Reset config to baseline
set_multi_output false

# ===== Phase D — failover =====
if [[ "$SWEEP_ABORTED" == "1" ]]; then
  log "=== Phase D skipped — sweep was aborted ==="
elif [[ "$SKIP_FAILOVER" != "1" ]]; then
  log "=== Phase D — failover scenarios ==="
  for d_case in d1 d2 d3 d4; do
    log "  → $d_case"
    if "$SCRIPTS"/run-failover.sh "$d_case" >>"$LOG" 2>&1; then
      log "    $d_case completed"
    else
      log "    $d_case FAILED — see $LOG"
    fi
    d_id=$(echo "$d_case" | tr a-z A-Z)
    notify_run "$d_id"
    # Phase D failures abort too — same logic as A/B/C/F/H
    sum="$BENCH_ROOT/results/$d_id/summary.md"
    if [[ -f "$sum" ]] && \
       grep -qE '\| \*\*FAIL\*\* \||^\*\*FAIL\*\*' "$sum" 2>/dev/null; then
      log "  ✗ $d_id FAILED — aborting Phase D"
      notify "🛑 Phase D aborted after \`$d_id\` FAIL"
      SWEEP_ABORTED=1
      break
    fi
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

# Tally per-run verdicts. PASS + SATURATED are healthy (capacity test working);
# FAIL means the bench infrastructure broke somewhere.
PASS_COUNT=0; SAT_COUNT=0; FAIL_COUNT=0
SAT_RUNS=""; FAILED_RUNS=""
for sum in "$BENCH_ROOT"/results/*/summary.md; do
  [[ -f "$sum" ]] || continue
  rid=$(basename "$(dirname "$sum")")
  if grep -qE '\| \*\*FAIL\*\* \||^\*\*FAIL\*\*' "$sum" 2>/dev/null; then
    FAIL_COUNT=$((FAIL_COUNT + 1))
    FAILED_RUNS="$FAILED_RUNS $rid"
  elif grep -qE '\| \*\*SATURATED\*\* \||^\*\*SATURATED\*\*' "$sum" 2>/dev/null; then
    SAT_COUNT=$((SAT_COUNT + 1))
    SAT_RUNS="$SAT_RUNS $rid"
  else
    PASS_COUNT=$((PASS_COUNT + 1))
  fi
done

log "=== sweep complete in $((DUR / 60))m $((DUR % 60))s ==="
log "  PASS: $PASS_COUNT   SATURATED: $SAT_COUNT${SAT_RUNS:+ ($SAT_RUNS )}   FAIL: $FAIL_COUNT${FAILED_RUNS:+ ($FAILED_RUNS )}"
log
log "Committable report:  $BENCH_ROOT/reports/$SWEEP/report.md"
log "Local raw artifacts: $LOGDIR/  (gitignored)"

if [[ "$FAIL_COUNT" -eq 0 ]]; then
  notify "✅ Bench *${SWEEP}* done in $((DUR/60))m $((DUR%60))s
PASS: ${PASS_COUNT}  📈 SATURATED: ${SAT_COUNT}  ❌ FAIL: 0
${SAT_RUNS:+Ceilings hit:${SAT_RUNS}
}Report: \`bench/reports/${SWEEP}/report.md\`"
else
  notify "❌ Bench *${SWEEP}* done in $((DUR/60))m $((DUR%60))s
PASS: ${PASS_COUNT}  📈 SATURATED: ${SAT_COUNT}  ❌ FAIL: ${FAIL_COUNT}
Failed runs:${FAILED_RUNS}
Report: \`bench/reports/${SWEEP}/report.md\`"
fi
