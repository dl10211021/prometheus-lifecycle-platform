#!/usr/bin/env bash
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:18080}"
API="${API:-${BASE%/}/api/v1}"
DATA_DIR="${DATA_DIR:-}"
BASE_PATH="${BASE_PATH:-}"
SERVER_BIN="${SERVER_BIN:-./promlifecycle-server}"
START_SERVER="${START_SERVER:-1}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-${PROMLIFECYCLE_ADMIN_PASSWORD:-}}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-${PROMLIFECYCLE_ADMIN_PASSWORD_FILE:-}}"
RUN_PRODUCT_CLOSURE="${RUN_PRODUCT_CLOSURE:-0}"

OWN_DATA=0
if [ -z "$DATA_DIR" ]; then
  DATA_DIR="$(mktemp -d)"
  OWN_DATA=1
fi
BASE_PATH="${BASE_PATH:-$DATA_DIR/base}"
mkdir -p "$DATA_DIR" "$BASE_PATH"
SERVER_PID=""
SERVER_LOG="$DATA_DIR/server.log"

cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [ "$OWN_DATA" -eq 1 ]; then
    rm -rf "$DATA_DIR"
  fi
}
trap cleanup EXIT INT TERM

if [ "$START_SERVER" = "1" ]; then
  if [ ! -x "$SERVER_BIN" ]; then
    echo "Smoke server binary is not executable: $SERVER_BIN" >&2
    exit 1
  fi
  endpoint="${BASE#http://}"
  if [ "$endpoint" = "$BASE" ] || [ "${endpoint#*/}" != "$endpoint" ]; then
    echo "Local smoke BASE must be an http://host:port URL without a path" >&2
    exit 1
  fi
  hostport="$endpoint"
  host="${hostport%:*}"
  port="${hostport##*:}"
  if [ "$host" = "$hostport" ] || [ -z "$host" ] || [ -z "$port" ]; then
    echo "Local smoke BASE must include an explicit port" >&2
    exit 1
  fi
  JWT_SECRET_FILE="$BASE_PATH/jwt_secret"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n' > "$JWT_SECRET_FILE"
  printf '\n' >> "$JWT_SECRET_FILE"
  chmod 0600 "$JWT_SECRET_FILE"
  ADMIN_PASSWORD_FILE="$BASE_PATH/admin_initial_password"
  dd if=/dev/urandom bs=16 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n' > "$ADMIN_PASSWORD_FILE"
  printf '\n' >> "$ADMIN_PASSWORD_FILE"
  chmod 0600 "$ADMIN_PASSWORD_FILE"
  MOCK_BIN="$BASE_PATH/test-bin"
  mkdir -p "$MOCK_BIN" "$BASE_PATH/backups"
  cat > "$MOCK_BIN/systemctl" <<'EOF'
#!/usr/bin/env sh
verb="$1"; shift
unit=""
quiet=0
for arg in "$@"; do
  case "$arg" in --quiet) quiet=1 ;; -*) ;; *) unit="$arg" ;; esac
done
state="$(dirname "$0")/fixture-${unit}.stopped"
case "$verb" in
  show) printf 'loaded\n' ;;
  stop) : > "$state" ;;
  start|restart) rm -f "$state" ;;
  is-active)
    [ ! -f "$state" ] || exit 3
    [ "$quiet" -eq 1 ] || printf 'active\n'
    ;;
esac
exit 0
EOF
  chmod 0755 "$MOCK_BIN/systemctl"
  PATH="$MOCK_BIN:$PATH" \
  BACKUP_ROOT="$BASE_PATH/backups" \
  PROMLIFECYCLE_OFFLINE=1 \
  PROMLIFECYCLE_ALLOW_PRIVATE_FETCH=1 \
  PROMLIFECYCLE_ALLOW_INSECURE_HTTP=true \
  PROMLIFECYCLE_JWT_SECRET_FILE="$JWT_SECRET_FILE" \
  PROMLIFECYCLE_ADMIN_PASSWORD_FILE="$ADMIN_PASSWORD_FILE" \
    "$SERVER_BIN" --listen-address "$host" --port "$port" \
      --base-path "$BASE_PATH" --data-dir "$DATA_DIR" >"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
fi

ready=0
for _ in $(seq 1 30); do
  if curl --silent --show-error --fail "${BASE%/}/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if [ -n "$SERVER_PID" ] && ! kill -0 "$SERVER_PID" 2>/dev/null; then
    break
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "Local smoke server did not become healthy at $BASE" >&2
  [ ! -f "$SERVER_LOG" ] || cat "$SERVER_LOG" >&2
  exit 1
fi

ROOT_HTML="$(curl --silent --show-error --fail "${BASE%/}/")"
grep -q 'Prometheus 生命周期管理' <<<"$ROOT_HTML" || {
  echo "Product brand is missing from the management HTML" >&2
  exit 1
}
curl --silent --show-error --fail "${BASE%/}/metrics" >/dev/null
curl --silent --show-error --fail "$API/auth/login" -X POST \
  -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1 && {
    echo "Unauthenticated empty login unexpectedly succeeded" >&2
    exit 1
  }
if [ -n "$ADMIN_PASSWORD_FILE" ]; then
  if [ ! -r "$ADMIN_PASSWORD_FILE" ]; then
    echo "Admin password file is not readable: $ADMIN_PASSWORD_FILE" >&2
    exit 1
  fi
  ADMIN_PASSWORD="$(tr -d '\r\n' < "$ADMIN_PASSWORD_FILE")"
fi
if [ -z "$ADMIN_PASSWORD" ]; then
  echo "Set ADMIN_PASSWORD or ADMIN_PASSWORD_FILE for authenticated single-node smoke checks" >&2
  exit 1
fi

LOGIN_PAYLOAD=$(ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["ADMIN_PASSWORD"]}))')
LOGIN_JSON="$(curl --silent --show-error --fail "$API/auth/login" -X POST \
  -H 'Content-Type: application/json' \
  -d "$LOGIN_PAYLOAD")"
TOKEN="$(printf '%s' "$LOGIN_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token",""))')"
if [ -z "$TOKEN" ]; then
  echo "Authenticated login did not return a token" >&2
  exit 1
fi
NODE_JSON="$(curl --silent --show-error --fail "$API/node" -H "Authorization: Bearer $TOKEN")"
printf '%s' "$NODE_JSON" | python3 -c 'import json,sys; node=json.load(sys.stdin); assert node.get("deployment_type") == "single-node" and node.get("id"), node'
ME_JSON="$(curl --silent --show-error --fail "$API/auth/me" -H "Authorization: Bearer $TOKEN")"
printf '%s' "$ME_JSON" | python3 -c 'import json,sys; me=json.load(sys.stdin); assert me.get("role") == "system_admin", me'
LEGACY_COLLECTION="/clu""sters"
LEGACY_CODE="$(curl --silent --output /dev/null --write-out '%{http_code}' "$API$LEGACY_COLLECTION" -H "Authorization: Bearer $TOKEN")"
if [ "$LEGACY_CODE" != "404" ]; then
  echo "Legacy collection endpoint must be absent, got HTTP $LEGACY_CODE" >&2
  exit 1
fi
ONBOARDING_STATUS="$(curl --silent --show-error --fail "$API/onboarding/status" -H "Authorization: Bearer $TOKEN")"
printf '%s' "$ONBOARDING_STATUS" | python3 -c 'import json,sys; status=json.load(sys.stdin); assert status, status'
PREFLIGHT="$(curl --silent --show-error --fail "$API/onboarding/preflight" -H "Authorization: Bearer $TOKEN")"
printf '%s' "$PREFLIGHT" | python3 -c 'import json,sys; report=json.load(sys.stdin); assert isinstance(report,dict), report'
DISCOVERY="$(curl --silent --show-error --fail "$API/onboarding/discovery" -H "Authorization: Bearer $TOKEN")"
printf '%s' "$DISCOVERY" | python3 -c 'import json,sys; report=json.load(sys.stdin); assert report.get("host") == "127.0.0.1" and isinstance(report.get("components"),list), report'
TEMPLATES="$(curl --silent --show-error --fail "$API/stack-templates" -H "Authorization: Bearer $TOKEN")"
printf '%s' "$TEMPLATES" | python3 -c 'import json,sys; templates=json.load(sys.stdin); assert templates, templates'
if [ "$RUN_PRODUCT_CLOSURE" = "1" ]; then
  BASE="$BASE" API="$API" DATA_DIR="$DATA_DIR" BASE_PATH="$BASE_PATH" \
    ADMIN_PASSWORD="$ADMIN_PASSWORD" ADMIN_PASSWORD_FILE="$ADMIN_PASSWORD_FILE" \
    bash "$(dirname "$0")/product-closure-test.sh"
fi
printf 'localhost smoke passed: %s\n' "$BASE"
