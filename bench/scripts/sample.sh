#!/usr/bin/env bash
# Sample host + open-streamer + GPU + network metrics every 2s into one CSV.
# Designed to run for a fixed duration during a benchmark run window.
#
# Usage:
#   bench/scripts/sample.sh 300 bench/results/B3/sample.csv
#
# Columns:
#   ts                unix seconds
#   cpu_pct           sum of CPU% across open-streamer + ffmpeg children (host-relative, 0..100)
#   rss_mb            sum of RSS across open-streamer + ffmpeg children
#   rss_pct           rss_mb expressed as % of host total RAM (0..100)
#   procs             number of running ffmpeg children
#   gpu_pct           SM utilization summed across open-streamer's PIDs (%)
#   enc_pct           NVENC utilization summed across open-streamer's PIDs (%)
#   dec_pct           NVDEC utilization summed across open-streamer's PIDs (%)
#   vram_mb           VRAM allocated by open-streamer's PIDs (MiB)
#   net_rx_mbps       Mbit/s received on $NIC
#   net_tx_mbps       Mbit/s transmitted on $NIC
#   restarts_total    counter from /metrics — TranscoderRestartsTotal (if present)
#
set -euo pipefail

BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
DUR=${1:-300}
OUT=${2:-$BENCH_ROOT/results/sample-$(date +%s).csv}
NIC=${NIC:-$(ip route get 1 2>/dev/null | awk '{print $5; exit}')}
METRICS=${METRICS:-http://127.0.0.1:8080/metrics}
INTERVAL=${INTERVAL:-2}
# cpu_pct is computed as the rate of CPU jiffies consumed by tracked PIDs
# during the sample interval, divided by the maximum jiffies the host could
# accumulate in that window (INTERVAL * USER_HZ * N_CORES). This matches the
# instantaneous-load semantics of `top` / Grafana and is host-relative
# (0..100% of total host CPU capacity).
#
# We do NOT use `ps -o pcpu` because it reports lifetime-cumulative CPU%,
# which is meaningless for a long-running process briefly stressed by bench.
N_CORES=$(nproc 2>/dev/null || echo 1)
USER_HZ=$(getconf CLK_TCK 2>/dev/null || echo 100)
# Total host RAM in MB. Used to express the per-process RSS sum as a
# host-relative percentage so it's comparable with cpu_pct.
TOTAL_RAM_MB=$(awk '/^MemTotal:/ {printf "%.0f", $2/1024; exit}' /proc/meminfo 2>/dev/null || echo 1024)

mkdir -p "$(dirname "$OUT")"

# Build the PID list we account for: the open-streamer process itself plus
# every direct child it spawned (these are the transcoder ffmpeg workers).
# This intentionally EXCLUDES bench-side ffmpeg publishers (parent = bash) and
# any other ffmpeg processes on the host that are not driven by open-streamer.
read_pids() {
  local osp
  osp=$(pgrep -x open-streamer 2>/dev/null | head -1)
  [[ -z "$osp" ]] && { echo ""; return; }
  local children
  children=$(pgrep -P "$osp" 2>/dev/null | tr '\n' ',' | sed 's/,$//')
  if [[ -n "$children" ]]; then
    echo "${osp},${children}"
  else
    echo "$osp"
  fi
}

# Fill an associative array (passed by name) with pid -> utime+stime jiffies
# for each tracked PID. The caller intersects this map with the prior tick's
# map so only PIDs present in BOTH samples contribute to the CPU delta. This
# avoids the spurious spike that would otherwise happen when a new ffmpeg
# child appears mid-bench (its full lifetime jiffies would land entirely in
# one interval) and the symmetric under-read when a child exits.
read_proc_jiffies() {
  local -n out=$1
  local pids=$2
  out=()
  [[ -z "$pids" ]] && return
  local jiff
  local IFS=,
  for pid in $pids; do
    [[ -r /proc/$pid/stat ]] || continue
    # /proc/<pid>/stat fields: utime=14, stime=15
    jiff=$(awk '{print $14+$15}' "/proc/$pid/stat" 2>/dev/null) || continue
    out[$pid]=$jiff
  done
}

# RSS + ffmpeg-child count from `ps`. RSS is instantaneous, no delta needed.
read_proc_mem() {
  local pids=$1
  [[ -z "$pids" ]] && { echo "0,0,0"; return; }
  ps -o rss=,comm= -p "$pids" 2>/dev/null \
    | awk -v totram="$TOTAL_RAM_MB" '
        {rss+=$1; if ($2 ~ /ffmpeg/) ffmpeg++}
        END {
          rss_mb = rss/1024
          rss_pct = (totram > 0) ? rss_mb*100/totram : 0
          printf "%.1f,%.1f,%d", rss_mb, rss_pct, ffmpeg+0
        }'
}

read_gpu() {
  local pids=$1
  command -v nvidia-smi >/dev/null || { echo "0,0,0,0"; return; }
  if [[ -z "$pids" ]]; then
    echo "0,0,0,0"; return
  fi

  # Build alternation regex: "1234,5678" → "1234|5678"
  local pid_re
  pid_re=$(echo "$pids" | tr ',' '|')

  # Per-PID utilization from `nvidia-smi pmon -s u`. Columns:
  #   gpu pid type sm% mem% enc% dec% command
  # We sum sm/enc/dec across our tracked PIDs (others on the host are ignored).
  local util
  util=$(nvidia-smi pmon -c 1 -s u 2>/dev/null \
    | awk -v re="^($pid_re)\$" '
        $2 ~ re && $4 != "-" {sm+=$4; enc+=$6; dec+=$7}
        END {printf "%d,%d,%d", sm+0, enc+0, dec+0}') || util="0,0,0"

  # Per-PID VRAM via compute-apps query.
  local vram
  vram=$(nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader,nounits 2>/dev/null \
    | awk -F, -v re="^[[:space:]]*($pid_re)[[:space:]]*\$" '
        $1 ~ re {sum += $2+0}
        END {printf "%d", sum+0}') || vram=0

  echo "${util:-0,0,0},${vram:-0}"
}

# /proc/net/dev counters → Mbit/s using bench delta
read_net_bytes() {
  awk -v iface="$NIC" '$1 ~ "^"iface":" {gsub(":", " "); print $2, $10}' /proc/net/dev
}

restart_counter() {
  curl -fs --max-time 1 "$METRICS" 2>/dev/null \
    | awk '/^transcoder_restarts_total / {print $2; exit} /^TranscoderRestartsTotal / {print $2; exit}' \
    || echo 0
}

echo "[sample] writing $OUT (duration ${DUR}s, interval ${INTERVAL}s, nic ${NIC:-none}, cores ${N_CORES}, USER_HZ ${USER_HZ}, totRAM ${TOTAL_RAM_MB}MB)"
echo "ts,cpu_pct,rss_mb,rss_pct,procs,gpu_pct,enc_pct,dec_pct,vram_mb,net_rx_mbps,net_tx_mbps,restarts_total" >"$OUT"

prev_rx=0; prev_tx=0
if [[ -n "${NIC:-}" ]]; then
  read prev_rx prev_tx < <(read_net_bytes)
fi
declare -A prev_jiff_map=()
read_proc_jiffies prev_jiff_map "$(read_pids)"

end=$(( $(date +%s) + DUR ))
while [[ $(date +%s) -lt $end ]]; do
  ts=$(date +%s)
  pids=$(read_pids)

  # CPU% = delta jiffies (over the PID intersection of prev and curr ticks)
  # divided by the max possible jiffies in this interval. New PIDs contribute
  # 0 this tick — their first valid delta is reported on the next sample;
  # dead PIDs are dropped naturally because we iterate curr_jiff_map's keys.
  declare -A curr_jiff_map=()
  read_proc_jiffies curr_jiff_map "$pids"
  total_delta=0
  for pid in "${!curr_jiff_map[@]}"; do
    if [[ -n "${prev_jiff_map[$pid]:-}" ]]; then
      d=$(( curr_jiff_map[$pid] - prev_jiff_map[$pid] ))
      (( d > 0 )) && total_delta=$(( total_delta + d ))
    fi
  done
  cpu_pct=$(awk -v d="$total_delta" -v hz="$USER_HZ" \
                -v cores="$N_CORES" -v iv="$INTERVAL" '
    BEGIN {
      max = iv * hz * cores
      if (max > 0) printf "%.1f", d * 100 / max
      else         print "0.0"
    }')
  # Rotate: prev = curr.
  prev_jiff_map=()
  for pid in "${!curr_jiff_map[@]}"; do
    prev_jiff_map[$pid]=${curr_jiff_map[$pid]}
  done

  mem_line=$(read_proc_mem "$pids")
  gpu_line=$(read_gpu "$pids")

  if [[ -n "${NIC:-}" ]]; then
    read cur_rx cur_tx < <(read_net_bytes)
    rx_mbps=$(awk -v a="$cur_rx" -v b="$prev_rx" -v i="$INTERVAL" 'BEGIN{printf "%.2f",(a-b)*8/1e6/i}')
    tx_mbps=$(awk -v a="$cur_tx" -v b="$prev_tx" -v i="$INTERVAL" 'BEGIN{printf "%.2f",(a-b)*8/1e6/i}')
    prev_rx=$cur_rx; prev_tx=$cur_tx
  else
    rx_mbps=0; tx_mbps=0
  fi

  restart=$(restart_counter)

  echo "$ts,$cpu_pct,$mem_line,$gpu_line,$rx_mbps,$tx_mbps,$restart" >>"$OUT"
  sleep "$INTERVAL"
done

echo "[sample] done. Quick summary:"
awk -F, 'NR>1 {n++; cpu+=$2; rss_mb+=$3; rss_pct+=$4; gpu+=$6; enc+=$7; dec+=$8; vram+=$9; tx+=$11}
  END {if (n>0) printf "  avg cpu=%.1f%%  rss=%.0fMB (%.1f%%)  gpu=%.0f%%  enc=%.0f%%  dec=%.0f%%  vram=%.0fMB  tx=%.1fMbps\n",
       cpu/n, rss_mb/n, rss_pct/n, gpu/n, enc/n, dec/n, vram/n, tx/n}' "$OUT"
