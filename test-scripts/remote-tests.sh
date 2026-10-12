#!/usr/bin/env bash
set -euo pipefail

BASE="${BASE:-}"
API="${API:-${BASE%/}/api/v1}"
REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_PASS="${REMOTE_PASS:-}"
DATA_DIR="${DATA_DIR:-/opt/promlifecycle/data}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-/opt/promlifecycle/admin_initial_password}"
ADMIN_PASSWORD="${PROMLIFECYCLE_ADMIN_PASSWORD:-${ADMIN_PASSWORD:-}}"
if [ -z "$ADMIN_PASSWORD" ] && [ -r "$ADMIN_PASSWORD_FILE" ]; then
  ADMIN_PASSWORD="$(cat "$ADMIN_PASSWORD_FILE")"
fi
REMOTE_SCRIPTS="${REMOTE_SCRIPTS:-remote_crud_test.sh remote_deploy_e2e_test.sh}"
export BASE API REMOTE_HOST REMOTE_USER REMOTE_PASS DATA_DIR ADMIN_PASSWORD ADMIN_PASSWORD_FILE PROMLIFECYCLE_ADMIN_PASSWORD

if [ -z "$REMOTE_HOST" ] || [ -z "$REMOTE_PASS" ]; then
  echo "SKIP: set REMOTE_HOST and REMOTE_PASS to run remote tests"
  exit 0
fi
if [ -z "$BASE" ]; then
  echo "ERROR: set BASE to the dedicated test platform URL; localhost smoke has already stopped" >&2
  exit 2
fi
case "$BASE" in
  http://*|https://*) ;;
  *) echo "ERROR: BASE must be an explicit http(s) test platform URL" >&2; exit 2 ;;
esac
if [ -z "$ADMIN_PASSWORD" ]; then
  echo "ERROR: set ADMIN_PASSWORD or a readable ADMIN_PASSWORD_FILE to authenticate the test platform" >&2
  exit 2
fi
for tool in curl python3 ssh sshpass timeout; do
  command -v "$tool" >/dev/null || { echo "ERROR: remote tests require $tool" >&2; exit 2; }
done
curl --fail --silent --show-error --connect-timeout 5 --max-time 15 "${BASE%/}/health" >/dev/null || {
  echo "ERROR: dedicated test platform is unavailable at BASE" >&2
  exit 1
}
LOGIN_PAYLOAD=$(ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["ADMIN_PASSWORD"]}))')
LOGIN_RESPONSE=$(curl --fail --silent --show-error --connect-timeout 5 --max-time 15 "$API/auth/login" \
  -H 'Content-Type: application/json' -d "$LOGIN_PAYLOAD") || {
  echo "ERROR: test platform administrator authentication failed; remote tests were not dispatched" >&2
  exit 1
}
if ! printf '%s' "$LOGIN_RESPONSE" | python3 -c 'import json,sys; response=json.load(sys.stdin); token=response.get("token"); sys.exit(0 if isinstance(token,str) and token.strip() else 1)' 2>/dev/null; then
  echo "ERROR: test platform administrator authentication returned no token; remote tests were not dispatched" >&2
  exit 1
fi
unset LOGIN_PAYLOAD LOGIN_RESPONSE
SSHPASS="$REMOTE_PASS" timeout 15 sshpass -e ssh -o StrictHostKeyChecking=yes -o BatchMode=no -o ConnectTimeout=5 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$REMOTE_HOST" true >/dev/null || {
  echo "ERROR: remote SSH prerequisite failed; verify connectivity, credentials and known_hosts" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
for script_name in $REMOTE_SCRIPTS; do
  case "$script_name" in
    */*|*..*) echo "Invalid REMOTE_SCRIPTS entry: $script_name" >&2; exit 2 ;;
  esac
  if [ ! -f "$SCRIPT_DIR/$script_name" ]; then
    echo "Remote test script not found: $script_name" >&2
    exit 1
  fi
  bash "$SCRIPT_DIR/$script_name"
done
