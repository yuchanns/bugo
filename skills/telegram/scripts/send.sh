#!/usr/bin/env bash
set -euo pipefail

CHAT_ID=""
TEXT=""
TOKEN="${BUGO_TELEGRAM_TOKEN:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --chat-id)
      CHAT_ID="${2:-}"
      shift 2
      ;;
    --text)
      TEXT="${2:-}"
      shift 2
      ;;
    --token)
      TOKEN="${2:-}"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

if [[ -z "$TOKEN" ]]; then
  echo "missing telegram token: set BUGO_TELEGRAM_TOKEN" >&2
  exit 1
fi
if [[ -z "$CHAT_ID" ]]; then
  echo "missing --chat-id" >&2
  exit 1
fi
if [[ -z "$TEXT" ]]; then
  echo "missing --text" >&2
  exit 1
fi

curl -sS -X POST "https://api.telegram.org/bot${TOKEN}/sendMessage" \
  --data-urlencode "chat_id=${CHAT_ID}" \
  --data-urlencode "text=${TEXT}"
