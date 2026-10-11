#!/usr/bin/env bash
# Upgrade helper: apply PostgreSQL schema migrations via bin/grok2api-migrate
# and guide importing legacy data/*.json via the Admin UI.
set -euo pipefail
cd "$(dirname "$0")/.."

DATA_DIR="${DATA_DIR:-./data}"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-./migrations}"
DATABASE_URL="${DATABASE_URL:-${GROK2API_DATABASE_URL:-}}"
COMPOSE="${COMPOSE:-1}"
ACTION="up"

load_dotenv() {
  local env_file="${1:-.env}"
  [[ -f "${env_file}" ]] || return 0
  local line key val
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line%$'\r'}"
    line="${line#"${line%%[![:space:]]*}"}"
    [[ -z "${line}" || "${line}" == \#* ]] && continue
    if [[ "${line}" == export[[:space:]]* ]]; then
      line="${line#export}"
      line="${line#"${line%%[![:space:]]*}"}"
    fi
    [[ "${line}" == *=* ]] || continue
    key="${line%%=*}"
    val="${line#*=}"
    key="${key#"${key%%[![:space:]]*}"}"
    key="${key%"${key##*[![:space:]]}"}"
    [[ "${key}" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || continue
    val="${val#"${val%%[![:space:]]*}"}"
    if [[ "${val}" =~ ^\"(([^\"\\]|\\.)*)\"[[:space:]]*(#.*)?$ ]]; then
      val="${BASH_REMATCH[1]}"
      val="${val//\\\"/\"}"
      val="${val//\\\\/\\}"
    elif [[ "${val}" =~ ^\'([^\']*)\'[[:space:]]*(#.*)?$ ]]; then
      val="${BASH_REMATCH[1]}"
    else
      if [[ "${val}" == \#* ]]; then
        val=""
      else
        val="${val%%[[:space:]]#*}"
        val="${val%"${val##*[![:space:]]}"}"
      fi
    fi
    export "${key}=${val}"
  done < "${env_file}"
}

mask_url() {
  local url="${1:-}"
  echo "${url}" | sed -E 's|^([a-zA-Z0-9+.-]+://[^/:@]*):(.+)@([^@]+)$|\1:***@\3|'
}

usage() {
  cat <<'EOF'
Usage: scripts/upgrade_from_file_backend.sh [options]

Options:
  --data-dir PATH       Path to legacy data/ directory (default: ./data)
  --migrations-dir PATH Path to SQL migrations directory (default: ./migrations)
  --database-url URL    PostgreSQL URL (or set DATABASE_URL)
  --no-compose          Do not run docker compose up for redis/postgres
  --dry-run, --status   Run `grok2api-migrate status` without applying changes
  --verify              Run `grok2api-migrate verify`
  -h, --help            Show help

Environment:
  DATABASE_URL / GROK2API_DATABASE_URL
  DATA_DIR
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --migrations-dir) MIGRATIONS_DIR="$2"; shift 2 ;;
    --database-url) DATABASE_URL="$2"; shift 2 ;;
    --no-compose) COMPOSE=0; shift ;;
    --dry-run|--status) ACTION="status"; shift ;;
    --verify) ACTION="verify"; shift ;;
    --no-merge-pool|--merge-pool) shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ -f .env ]]; then
  CLI_DATABASE_URL="${DATABASE_URL}"
  load_dotenv .env
  if [[ -n "${CLI_DATABASE_URL}" ]]; then
    DATABASE_URL="${CLI_DATABASE_URL}"
  else
    DATABASE_URL="${GROK2API_DATABASE_URL:-${DATABASE_URL:-}}"
  fi
fi
if [[ -z "$DATABASE_URL" ]]; then
  DATABASE_URL="postgresql://${POSTGRES_USER:-grok2api}:${POSTGRES_PASSWORD:-grok2api}@127.0.0.1:5432/${POSTGRES_DB:-grok2api}"
  echo "WARN: DATABASE_URL unset; using default: $(mask_url "$DATABASE_URL")"
fi

echo "== upgrade to PostgreSQL hybrid backend =="
echo "data_dir=$DATA_DIR"
echo "migrations_dir=$MIGRATIONS_DIR"
echo "database=$(mask_url "$DATABASE_URL")"
echo "action=$ACTION"

if [[ "$COMPOSE" == "1" ]] && command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
  echo "== ensure redis + postgres =="
  docker compose up -d redis postgres
  echo "waiting for postgres..."
  for i in $(seq 1 40); do
    if docker compose exec -T postgres pg_isready -U "${POSTGRES_USER:-grok2api}" -d "${POSTGRES_DB:-grok2api}" >/dev/null 2>&1; then
      echo "postgres ready"
      break
    fi
    sleep 1
  done
fi

export DATABASE_URL
export GROK2API_DATABASE_URL="${GROK2API_DATABASE_URL:-$DATABASE_URL}"

MIGRATE_BIN=""
if [[ -x ./bin/grok2api-migrate ]]; then
  MIGRATE_BIN="./bin/grok2api-migrate"
elif [[ -x /app/bin/grok2api-migrate ]]; then
  MIGRATE_BIN="/app/bin/grok2api-migrate"
elif command -v go >/dev/null 2>&1; then
  echo "== building bin/grok2api-migrate =="
  mkdir -p bin
  go build -o ./bin/grok2api-migrate ./cmd/grok2api-migrate
  MIGRATE_BIN="./bin/grok2api-migrate"
fi

echo "== run schema migrator (${ACTION}) =="
if [[ -n "$MIGRATE_BIN" ]]; then
  "$MIGRATE_BIN" -dir "$MIGRATIONS_DIR" "$ACTION"
elif [[ "$COMPOSE" == "1" ]] && command -v docker >/dev/null 2>&1; then
  COMPOSE_DB_URL="${GROK2API_COMPOSE_DATABASE_URL:-${GROK2API_DATABASE_URL:-${DATABASE_URL:-postgresql://${POSTGRES_USER:-grok2api}:${POSTGRES_PASSWORD:-grok2api}@postgres:5432/${POSTGRES_DB:-grok2api}}}}"
  docker compose run --rm --no-deps \
    -e DATABASE_URL="${COMPOSE_DB_URL}" \
    -e GROK2API_DATABASE_URL="${COMPOSE_DB_URL}" \
    grokcli-2api /app/bin/grok2api-migrate -dir /app/migrations "$ACTION"
else
  echo "ERROR: bin/grok2api-migrate not found and neither Go toolchain nor Docker Compose is available." >&2
  exit 1
fi

echo
echo "Next steps:"
echo "  1) Ensure .env has GROK2API_STORE_BACKEND=hybrid and REDIS_URL / DATABASE_URL"
echo "  2) Start the stack: docker compose up -d (entrypoint also auto-runs grok2api-migrate up)"
echo "  3) Check health: curl -fsS http://127.0.0.1:\${GROK2API_PORT:-3000}/health"
echo "  4) Import legacy JSON accounts (e.g. ${DATA_DIR}/auth.json) via Admin UI (/admin → 账号管理 → 导入)"
echo "See docs/UPGRADE.md for details."
