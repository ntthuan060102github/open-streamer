# Benchmark Kit

Tooling to measure Open-Streamer throughput, GPU/CPU saturation, and
failover behaviour on a single host.

## Layout

```text
bench/
├── README.md             ← this file
├── REPORT_TEMPLATE.md    ← copy per run, fill into results/<id>/report.md
├── scripts/              ← all executables
│   ├── prepare.sh        ← deps check, generate sample media, snapshot host
│   ├── create-streams.sh ← POST/DELETE N stream definitions via API
│   ├── source.sh         ← spawn N FFmpeg RTMP publishers
│   ├── sample.sh         ← write CSV of host/GPU/proc metrics
│   ├── run-bench.sh      ← run a single A/B/C phase
│   ├── run-failover.sh   ← Phase D — d1/d2/d3/d4 fully scripted
│   ├── summarize.sh      ← read results/<id>/* → emit summary.md
│   ├── run-all.sh        ← execute A+B+C+D plan, summarise + aggregate
│   ├── aggregate.sh      ← combine every summary.md into one master report.md
│   └── notify.sh         ← optional Telegram notifier (start / done / fail)
├── payloads/             ← stream JSON templates ({{CODE}} placeholder)
│   ├── passthrough.json
│   ├── abr3-legacy.json
│   └── abr3-multi.json
├── reports/              ← TRACKED — committable reports
│   └── <sweep>/
│       ├── report.md     ← master report (auto-generated)
│       ├── sysinfo.txt   ← copy of host snapshot
│       ├── run-all.log   ← copy of orchestrator log
│       └── runs/
│           └── <run-id>.md   ← copy of per-run summary
├── assets/               ← gitignored — generated test media
│   └── sample-1080p.ts
├── results/              ← gitignored — raw per-run artifacts
│   └── <run-id>/
│       ├── sample.csv
│       ├── runtime-{t0,warmup,mid,end}.json
│       ├── streams-{mid,end}.json
│       ├── metrics-end.txt
│       ├── config.json
│       ├── create.log
│       └── summary.md
└── .run/                 ← gitignored — live source.sh PIDs/logs
```

**The split:** `reports/` is shareable / committable; `results/` is the raw
working directory on the benchmarking host. After each sweep, `aggregate.sh`
copies the master report + per-run summaries + sysinfo + sweep log from
`results/` into `reports/<sweep>/` so a `git add bench/reports/<sweep>` is
the only commit needed.

## First-time setup

```bash
bench/scripts/prepare.sh all
# → check tools, generate assets/sample-1080p.ts, snapshot results/sysinfo-*.txt
```

Required: `ffmpeg`, `ffprobe`, `curl`, standard coreutils.
Recommended: `nvidia-smi`, `pidstat` (sysstat), `ifstat`, `iostat`, `dmidecode`, `jq`.

## Fully-automated sweep (one command, ~90 min)

```bash
bench/scripts/run-all.sh                # SWEEP = <tag>-<date>
bench/scripts/run-all.sh baseline       # SWEEP = <tag>-<date>-baseline
SWEEP=manual-name bench/scripts/run-all.sh   # full override
```

The sweep name is auto-composed:

| Part | Source | Example |
| --- | --- | --- |
| tag | `git describe --tags --exact-match HEAD` → `git describe --tags` → `dev` | `v0.0.31`, `v0.0.31-3-g869cb6c`, `dev` |
| date | `date +%Y-%m-%d` | `2026-04-27` |
| note | first positional arg, or `$NOTE` (skipped if empty) | `baseline`, `stress` |

Examples of fully-resolved names:
`v0.0.31-2026-04-27`, `v0.0.31-2026-04-27-baseline`, `v0.0.31-3-g869cb6c-2026-04-27-fix-X`,
`dev-2026-04-27-baseline`.

What it does:

1. Toggles `multi_output` config per phase (false for B, true for C).
2. For each run in the plan: `run-bench.sh` → `summarize.sh` → cooldown.
3. After every run: `aggregate.sh` produces `results/<sweep>/report.md`.
4. Logs progress to `results/<sweep>/run-all.log`.

Default plan (14 runs): A1/A2/A3, B1/B2/B3/B4, C2/C3/C4, D1/D2/D3/D4.

Knobs:

| Var | Default | Notes |
| --- | --- | --- |
| `SWEEP` | `<date>-<gpu>-<note>` | full override; bypasses auto-compose |
| `NOTE` | first positional arg, else git short SHA | the `<note>` part of auto-compose |
| `COOLDOWN` | `30` | seconds between A/B/C runs |
| `PLAN` | (all 10 in A/B/C) | space-separated subset, e.g. `PLAN="B3 C3" run-all.sh` |
| `SKIP_FAILOVER` | `0` | set to `1` to skip Phase D |
| `TELEGRAM_BOT_TOKEN` | _(unset)_ | enable Telegram start/done/fail notifications |
| `TELEGRAM_CHAT_ID` | _(unset)_ | destination chat (required together with the token) |
| `TELEGRAM_THREAD_ID` | _(unset)_ | optional forum-topic id for supergroups |

### Telegram notifications

When both `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` are set, `run-all.sh`
sends a Telegram message:

- **🚀 start** — when the sweep begins (with run count + hostname)
- **✅ per run PASS** — after each A/B/C/D run, with the metrics row
- **⚠️ per run FAIL** — same, when the auto-suggested verdict is FAIL
- **❓ per run UNKNOWN** — when summary is missing or verdict can't be parsed
- **✅ done** — when every run passed (with duration + report path)
- **⚠️ done with fails** — when one or more runs were marked FAIL (lists the run ids)
- **❌ aborted** — on fatal error (api unreachable, missing sample, etc.)

If either env var is missing, notifications are silently skipped — the
benchmark itself never fails because notifications can't be sent.

Set them once in `~/.profile` or pass per-invocation:

```bash
TELEGRAM_BOT_TOKEN=123:abc TELEGRAM_CHAT_ID=-100123 \
  bench/scripts/run-all.sh baseline
```

Output:

```text
reports/<sweep>/                   ← TRACKED, ready to commit
├── report.md                      ← master report (<TODO> for human review)
├── sysinfo.txt
├── run-all.log
└── runs/
    ├── A1.md, A2.md, A3.md
    ├── B1.md, B2.md, B3.md, B4.md
    ├── C2.md, C3.md, C4.md
    └── D1.md, D2.md, D3.md, D4.md

results/<run-id>/, results/<sweep>/   ← gitignored, host-local only
```

After the sweep finishes:

```bash
git add bench/reports/<sweep>/
git commit -m "bench: <sweep> results"
```

The master report has every metric table auto-filled. You only fill the
`<TODO>` sections: highlights, recommendations, sign-off.

## Run one phase (the easy way)

```bash
# Phase A2 — 10 passthrough streams, 60s warmup + 5min sample
bench/scripts/run-bench.sh A2 10 passthrough

# Phase B3 — 4 ABR streams, legacy mode
bench/scripts/run-bench.sh B3 4 abr3-legacy

# Phase C3 — same load, multi-output mode (toggle global config first!)
curl -X POST http://127.0.0.1:8080/config \
  -H 'Content-Type: application/json' \
  -d '{"transcoder":{"multi_output":true}}'
bench/scripts/run-bench.sh C3 4 abr3-multi
```

`run-bench.sh` automatically tears down streams and source publishers on exit
(including Ctrl+C).

## Run one phase (manual, for D1–D4 failover scenarios)

```bash
bench/scripts/create-streams.sh 1 abr3-legacy
bench/scripts/source.sh 1
sleep 60                                              # warm-up
bench/scripts/sample.sh 300 bench/results/D2/sample.csv &

# now do whatever the case calls for, e.g. kill ffmpeg child
pkill -9 -f 'ffmpeg.*bench1'

wait
bench/scripts/source.sh stop
bench/scripts/create-streams.sh delete 1
```

## Phase plan

| Phase | What | Script invocation |
| --- | --- | --- |
| A | Passthrough load test | `run-bench.sh A<n> <N> passthrough` |
| B | ABR transcoding (legacy mode) | `run-bench.sh B<n> <N> abr3-legacy` |
| C | ABR transcoding (multi-output) | toggle config + `run-bench.sh C<n> <N> abr3-multi` |
| D | Failover / hot-reload | `run-failover.sh {d1\|d2\|d3\|d4\|all}` |
| E | DVR I/O (set `dvr` in payload first) | manual + `iostat -x 2` log |

See [REPORT_TEMPLATE.md](REPORT_TEMPLATE.md) for the full plan, stop conditions,
and metrics to record.

## Environment overrides

All scripts accept these env vars:

| Var | Default | Notes |
| --- | --- | --- |
| `API` | `http://127.0.0.1:8080` | open-streamer REST base (no `/api/v1` prefix — endpoints sit at root) |
| `PREFIX` | `bench` | stream code prefix; final code = `<PREFIX><i>` |
| `SERVER` | `rtmp://127.0.0.1:1935/live` | source.sh push target |
| `INPUT` | `assets/sample-1080p.ts` | source.sh input file |
| `NIC` | auto-detect via `ip route` | sample.sh interface for tx/rx |
| `METRICS` | `http://127.0.0.1:8080/metrics` | sample.sh Prometheus URL |
| `INTERVAL` | `2` | sample.sh sample interval (seconds) |

## Reading sample.csv

Columns: `ts, cpu_pct, rss_mb, procs, gpu_pct, enc_pct, dec_pct, vram_mb, net_rx_mbps, net_tx_mbps, restarts_total`.

Quick averages auto-printed at end of each `sample.sh` run.

For deeper analysis:

```bash
# percentiles for one column
awk -F, 'NR>1 {print $5}' results/B3/sample.csv | sort -n \
  | awk '{a[NR]=$1} END {print "p50="a[int(NR*0.5)]" p95="a[int(NR*0.95)]" p99="a[int(NR*0.99)]}'

# plot in gnuplot
gnuplot -e "set datafile separator ','; \
  plot 'results/B3/sample.csv' using 1:5 with lines title 'gpu%', \
       '' using 1:6 with lines title 'enc%', \
       '' using 1:7 with lines title 'dec%'; pause -1"
```
