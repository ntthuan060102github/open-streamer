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
#   gpu_pct           overall GPU util %
#   enc_pct           NVENC util %
#   dec_pct           NVDEC util %
#   vram_mb           VRAM used (MiB)
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
# `ps -o pcpu` reports per-process %CPU normalized to "100% per core". On an
# N-core host the sum across processes can reach N×100%. We divide by N so
# the cpu_pct column is host-relative (0..100), comparable to the threshold
# in summarize.sh and to what `top`/Grafana report.
N_CORES=$(nproc 2>/dev/null || echo 1)
# Total host RAM in MB. Used to express the per-process RSS sum as a
# host-relative percentage so it's comparable with cpu_pct.
TOTAL_RAM_MB=$(awk '/^MemTotal:/ {printf "%.0f", $2/1024; exit}' /proc/meminfo 2>/dev/null || echo 1024)

mkdir -p "$(dirname "$OUT")"

read_pids() {
  pgrep -d, -x "open-streamer|streamer|ffmpeg" 2>/dev/null || true
}

read_proc_cpu_mem() {
  local pids=$1
  [[ -z "$pids" ]] && { echo "0,0,0,0"; return; }
  ps -o pid=,pcpu=,rss=,comm= -p "$pids" 2>/dev/null \
    | awk -v cores="$N_CORES" -v totram="$TOTAL_RAM_MB" '
        {cpu+=$2; rss+=$3; if ($4 ~ /ffmpeg/) ffmpeg++}
        END {
          rss_mb = rss/1024
          rss_pct = (totram > 0) ? rss_mb*100/totram : 0
          printf "%.1f,%.1f,%.1f,%d", cpu/cores, rss_mb, rss_pct, ffmpeg+0
        }'
}

read_gpu() {
  if command -v nvidia-smi >/dev/null; then
    nvidia-smi --query-gpu=utilization.gpu,utilization.encoder,utilization.decoder,memory.used \
      --format=csv,noheader,nounits 2>/dev/null | head -1 | tr -d ' '
  else
    echo "0,0,0,0"
  fi
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

echo "[sample] writing $OUT (duration ${DUR}s, interval ${INTERVAL}s, nic ${NIC:-none}, cores ${N_CORES}, totRAM ${TOTAL_RAM_MB}MB)"
echo "ts,cpu_pct,rss_mb,rss_pct,procs,gpu_pct,enc_pct,dec_pct,vram_mb,net_rx_mbps,net_tx_mbps,restarts_total" >"$OUT"

prev_rx=0; prev_tx=0
if [[ -n "${NIC:-}" ]]; then
  read prev_rx prev_tx < <(read_net_bytes)
fi

end=$(( $(date +%s) + DUR ))
while [[ $(date +%s) -lt $end ]]; do
  ts=$(date +%s)
  pids=$(read_pids)
  proc_line=$(read_proc_cpu_mem "$pids")
  gpu_line=$(read_gpu)

  if [[ -n "${NIC:-}" ]]; then
    read cur_rx cur_tx < <(read_net_bytes)
    rx_mbps=$(awk -v a="$cur_rx" -v b="$prev_rx" -v i="$INTERVAL" 'BEGIN{printf "%.2f",(a-b)*8/1e6/i}')
    tx_mbps=$(awk -v a="$cur_tx" -v b="$prev_tx" -v i="$INTERVAL" 'BEGIN{printf "%.2f",(a-b)*8/1e6/i}')
    prev_rx=$cur_rx; prev_tx=$cur_tx
  else
    rx_mbps=0; tx_mbps=0
  fi

  restart=$(restart_counter)

  echo "$ts,$proc_line,$gpu_line,$rx_mbps,$tx_mbps,$restart" >>"$OUT"
  sleep "$INTERVAL"
done

echo "[sample] done. Quick summary:"
awk -F, 'NR>1 {n++; cpu+=$2; rss_mb+=$3; rss_pct+=$4; gpu+=$6; enc+=$7; dec+=$8; vram+=$9; tx+=$11}
  END {if (n>0) printf "  avg cpu=%.1f%%  rss=%.0fMB (%.1f%%)  gpu=%.0f%%  enc=%.0f%%  dec=%.0f%%  vram=%.0fMB  tx=%.1fMbps\n",
       cpu/n, rss_mb/n, rss_pct/n, gpu/n, enc/n, dec/n, vram/n, tx/n}' "$OUT"
