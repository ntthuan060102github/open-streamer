#!/usr/bin/env bash
# Send a Telegram notification.
#
# Required env (skip silently if either is missing — benchmarks must not fail
# just because notifications aren't configured):
#   TELEGRAM_BOT_TOKEN   bot token from @BotFather
#   TELEGRAM_CHAT_ID     destination chat id (user, group, or channel)
#
# Optional env:
#   TELEGRAM_THREAD_ID   forum-topic thread id (supergroups with topics)
#
# Usage:
#   bench/scripts/notify.sh "message text with *Markdown*"
#
# Exit code is always 0 — caller never has to handle notification failures.
set -euo pipefail

MSG=${1:-}
[[ -z "$MSG" ]] && exit 0
[[ -z "${TELEGRAM_BOT_TOKEN:-}" || -z "${TELEGRAM_CHAT_ID:-}" ]] && exit 0

# Telegram caps a single message at 4096 chars
if [[ ${#MSG} -gt 4000 ]]; then
  MSG="${MSG:0:3900}
…(truncated)"
fi

args=(--data-urlencode "chat_id=${TELEGRAM_CHAT_ID}"
      --data-urlencode "text=${MSG}"
      --data-urlencode "parse_mode=Markdown"
      --data-urlencode "disable_web_page_preview=true")
[[ -n "${TELEGRAM_THREAD_ID:-}" ]] && \
  args+=(--data-urlencode "message_thread_id=${TELEGRAM_THREAD_ID}")

curl -s --max-time 10 -X POST \
  "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
  "${args[@]}" >/dev/null 2>&1 || true

exit 0
