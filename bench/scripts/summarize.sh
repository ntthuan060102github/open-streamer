#!/usr/bin/env bash
# Read all artifacts in bench/results/<run-id>/ and produce summary.md with
# every mechanically-derivable field already filled in.
#
# What it computes from sample.csv:
#   - median / p95 / max for cpu, rss, gpu, enc, dec, vram, net_tx
#   - delta of restarts_total over the run window
#   - count of ffmpeg processes (from procs column)
#
# What it pulls from JSON snapshots:
#   - stream count, status of each stream (runtime-end.json)
#   - active input switches per stream (runtime-end.json)
#   - ladder summary from create.log
#
# What it leaves as <TODO>:
#   - Verdict (PASS / FAIL — needs human review of context)
#   - Notes / observations
#
# Usage:
#   bench/scripts/summarize.sh <run-id>
#   bench/scripts/summarize.sh B3
#
set -euo pipefail

[[ $# -ne 1 ]] && { echo "Usage: $0 <run-id>"; exit 1; }

RUN=$1
BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
DIR=$BENCH_ROOT/results/$RUN

[[ ! -d "$DIR" ]] && { echo "[summarize] no results dir: $DIR"; exit 1; }
CSV=$DIR/sample.csv
[[ ! -f "$CSV" ]] && { echo "[summarize] missing sample.csv in $DIR"; exit 1; }

OUT=$DIR/summary.md
echo "[summarize] writing $OUT"

# ---------- helpers ----------
stat_col() {
  # $1 = csv path, $2 = column index (1-based)
  awk -F, -v c="$2" 'NR>1 && $c!="" {print $c}' "$1" | sort -n | awk '
    { a[NR]=$1+0; sum+=$1 }
    END {
      if (NR==0) { print "0 0 0 0"; exit }
      med = a[int((NR+1)/2)]
      p95 = a[int(NR*0.95)]
      max = a[NR]
      avg = sum/NR
      printf "%.1f %.1f %.1f %.1f", avg, med, p95, max
    }'
}

restart_delta() {
  awk -F, 'NR==2 {first=$11} {last=$11} END {printf "%d", (last+0)-(first+0)}' "$CSV"
}

procs_max() {
  awk -F, 'NR>1 && $4>m {m=$4} END {printf "%d", m+0}' "$CSV"
}

dur_sec() {
  awk -F, 'NR==2 {first=$1} {last=$1} END {printf "%d", (last+0)-(first+0)}' "$CSV"
}

samples() {
  awk -F, 'NR>1 {n++} END {printf "%d", n+0}' "$CSV"
}

git_sha() {
  (cd "$BENCH_ROOT/.." && git rev-parse --short HEAD 2>/dev/null) || echo "unknown"
}

# ---------- gather ----------
read CPU_AVG CPU_MED CPU_P95 CPU_MAX <<<"$(stat_col "$CSV" 2)"
read RSS_AVG RSS_MED RSS_P95 RSS_MAX <<<"$(stat_col "$CSV" 3)"
read GPU_AVG GPU_MED GPU_P95 GPU_MAX <<<"$(stat_col "$CSV" 5)"
read ENC_AVG ENC_MED ENC_P95 ENC_MAX <<<"$(stat_col "$CSV" 6)"
read DEC_AVG DEC_MED DEC_P95 DEC_MAX <<<"$(stat_col "$CSV" 7)"
read VRAM_AVG VRAM_MED VRAM_P95 VRAM_MAX <<<"$(stat_col "$CSV" 8)"
read RX_AVG RX_MED RX_P95 RX_MAX <<<"$(stat_col "$CSV" 9)"
read TX_AVG TX_MED TX_P95 TX_MAX <<<"$(stat_col "$CSV" 10)"

DUR=$(dur_sec)
SAMPLES=$(samples)
PROCS_MAX=$(procs_max)
RESTART_DELTA=$(restart_delta)
SHA=$(git_sha)
NOW=$(date -Iseconds 2>/dev/null || date)

# Phase from run-id prefix (A/B/C/D/E)
PHASE=$(echo "$RUN" | sed -E 's/^([A-Z]).*/\1/')

# Stream count + statuses from runtime-end.json (a snapshot of GET /streams).
# Response shape: {"data": [{"code":"...", "runtime":{"status":"...","switches":[...]}}, ...]}.
# Prefer jq; fall back to tolerant grep when jq is missing.
STREAM_LIST=""
DEGRADED_COUNT=0
SWITCHES_TOTAL=0
if [[ -f "$DIR/runtime-end.json" ]]; then
  if command -v jq >/dev/null; then
    STREAM_LIST=$(jq -r '(.data // [])[] | .code' "$DIR/runtime-end.json" 2>/dev/null | sort -u | tr '\n' ' ' || true)
    DEGRADED_COUNT=$(jq -r '[(.data // [])[] | select(.runtime.status == "degraded")] | length' "$DIR/runtime-end.json" 2>/dev/null || echo 0)
    SWITCHES_TOTAL=$(jq -r '[(.data // [])[] | (.runtime.switches // []) | length] | add // 0' "$DIR/runtime-end.json" 2>/dev/null || echo 0)
  else
    STREAM_LIST=$( { grep -oE '"code"[[:space:]]*:[[:space:]]*"[^"]+"' "$DIR/runtime-end.json" 2>/dev/null \
                      | sed 's/.*"\([^"]*\)"$/\1/' | sort -u | tr '\n' ' '; } || true)
    DEGRADED_COUNT=$(grep -cE '"status"[[:space:]]*:[[:space:]]*"degraded"' "$DIR/runtime-end.json" 2>/dev/null || echo 0)
    SWITCHES_TOTAL=$(grep -cE '"switches"[[:space:]]*:[[:space:]]*\[' "$DIR/runtime-end.json" 2>/dev/null || echo 0)
  fi
fi
STREAM_COUNT=$(echo "$STREAM_LIST" | wc -w | tr -d ' ')

# Pull profile name from create.log
PROFILE=$( { grep -oE 'payloads/[a-z0-9-]+\.json' "$DIR/create.log" 2>/dev/null \
              | head -1 | sed -E 's/.*\/([a-z0-9-]+)\.json/\1/'; } || true)
[[ -z "$PROFILE" ]] && PROFILE="<unknown>"

# Map profile name to a human-readable ladder string for the master report row.
case "$PROFILE" in
  passthrough)         LADDER="copy" ;;
  abr3-legacy)         LADDER="1080p+720p+480p (legacy)" ;;
  abr3-multi)          LADDER="1080p+720p+480p (multi)" ;;
  abr3-multi-hlsdash)  LADDER="1080p+720p+480p (multi, HLS+DASH)" ;;
  abr3-x264)           LADDER="1080p+720p+480p (libx264)" ;;
  abr2-*)              LADDER="720p+480p" ;;
  *)                   LADDER="<unknown>" ;;
esac

# Suggest verdict — use awk for portable float comparison (no bc dependency)
gt() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a+0 > b+0)}'; }

VERDICT="PASS"
VERDICT_REASON=""
if gt "$CPU_P95" 90; then
  VERDICT="FAIL"; VERDICT_REASON="$VERDICT_REASON CPU p95=${CPU_P95}% > 90%;"
fi
if gt "$RESTART_DELTA" 0; then
  VERDICT="FAIL"; VERDICT_REASON="$VERDICT_REASON transcoder restarts=${RESTART_DELTA};"
fi
if gt "$DEGRADED_COUNT" 0; then
  VERDICT="FAIL"; VERDICT_REASON="$VERDICT_REASON ${DEGRADED_COUNT} streams ended Degraded;"
fi
[[ -z "$VERDICT_REASON" ]] && VERDICT_REASON="all stop conditions clear"

# ---------- write ----------
cat >"$OUT" <<EOF
# Run Summary — $RUN

Auto-generated by \`bench/scripts/summarize.sh\` on $NOW from artifacts in \`bench/results/$RUN/\`.

## Overview

| Field | Value |
|---|---|
| Run ID | \`$RUN\` |
| Phase | $PHASE |
| Profile | \`$PROFILE\` |
| Streams created | $STREAM_COUNT |
| Stream codes | \`$(echo "$STREAM_LIST" | xargs)\` |
| Sample window | ${DUR}s ($SAMPLES samples @ 2s) |
| Max ffmpeg children | $PROCS_MAX |
| Open-Streamer commit | \`$SHA\` |

## Metrics (steady window)

| Metric | avg | median | p95 | max |
|---|---|---|---|---|
| CPU sum (%) | $CPU_AVG | $CPU_MED | $CPU_P95 | $CPU_MAX |
| RSS sum (MB) | $RSS_AVG | $RSS_MED | $RSS_P95 | $RSS_MAX |
| GPU util (%) | $GPU_AVG | $GPU_MED | $GPU_P95 | $GPU_MAX |
| NVENC util (%) | $ENC_AVG | $ENC_MED | $ENC_P95 | $ENC_MAX |
| NVDEC util (%) | $DEC_AVG | $DEC_MED | $DEC_P95 | $DEC_MAX |
| VRAM (MB) | $VRAM_AVG | $VRAM_MED | $VRAM_P95 | $VRAM_MAX |
| Net Rx (Mbps) | $RX_AVG | $RX_MED | $RX_P95 | $RX_MAX |
| Net Tx (Mbps) | $TX_AVG | $TX_MED | $TX_P95 | $TX_MAX |

## Health signals

| Signal | Value |
|---|---|
| Transcoder restarts during run | **$RESTART_DELTA** |
| Streams ending in \`degraded\` | **$DEGRADED_COUNT** |
| Total switch-history entries | $SWITCHES_TOTAL |

## Verdict (auto-suggested)

**$VERDICT** — $VERDICT_REASON

> Stop conditions checked: CPU p95 > 90%, transcoder restart delta > 0, any stream ended Degraded.
> Other stop conditions (packet drop in streamer.log, HLS jitter > 15%) require manual inspection — see below.

## Manual review checklist

- [ ] \`grep -i 'drop\\|buffer hub' streamer.log\` — packet drop?
- [ ] \`ffprobe -i <hls_url>/index.m3u8\` — segment duration jitter < 15% of target?
- [ ] Visual sanity: \`runtime-end.json\` per-stream \`switches\` array — any unexpected entry?
- [ ] Verdict above matches reality?

## Row to paste into the master report

Use this row in the matching Phase $PHASE table of the master \`report.md\`:

\`\`\`markdown
| $RUN | $STREAM_COUNT | $LADDER | $CPU_MED | $RSS_MED | $GPU_MED | $ENC_MED | $DEC_MED | $VRAM_MED | $RESTART_DELTA | $VERDICT |
\`\`\`

## Files in this run

\`\`\`
$(ls -la "$DIR" 2>/dev/null | tail -n +2)
\`\`\`

## TODO (human review)

- Set final verdict + add reasoning beyond auto-suggested check
- Add observations / surprises in the master report § 5
- File issues if FAIL — link from master report § 6
EOF

echo "[summarize] done."
echo
echo "  $OUT"
echo
echo "Quick view:"
echo "  cat $OUT"
echo "  open $OUT      # macOS"
