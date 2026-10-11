#!/usr/bin/env bash
set -euo pipefail
cd /app

HOST="${HOST:-${TURNSTILE_HOST:-127.0.0.1}}"
PORT="${TURNSTILE_PORT:-5072}"
THREAD="${TURNSTILE_THREAD:-2}"
BROWSER_TYPE="${TURNSTILE_BROWSER_TYPE:-camoufox}"
DEBUG_FLAG=()
if [[ "${GROK2API_SOLVER_DEBUG:-${TURNSTILE_DEBUG:-0}}" == "1" || "${GROK2API_SOLVER_DEBUG:-${TURNSTILE_DEBUG:-false}}" == "true" ]]; then
  DEBUG_FLAG=(--debug)
fi

mkdir -p /app/logs /app/keys

echo "[turnstile-solver] browser=${BROWSER_TYPE} thread=${THREAD} ${HOST}:${PORT}"
exec python api_solver.py \
  --browser_type "${BROWSER_TYPE}" \
  --thread "${THREAD}" \
  --host "${HOST}" \
  --port "${PORT}" \
  "${DEBUG_FLAG[@]}"
