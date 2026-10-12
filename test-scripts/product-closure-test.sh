#!/usr/bin/env bash
set -u

BASE="${BASE:-http://127.0.0.1:18080}"
API="${API:-${BASE%/}/api/v1}"
DATA_DIR="${DATA_DIR:-}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-${PROMLIFECYCLE_ADMIN_PASSWORD:-}}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-${PROMLIFECYCLE_ADMIN_PASSWORD_FILE:-}}"
PASS=0; FAIL=0
AI_PID=""
ADOPT_PID=""
CREATED_USERS=""
CREATED_SCHEDULES=""

cleanup() {
  [ -z "$AI_PID" ] || kill "$AI_PID" >/dev/null 2>&1 || true
  [ -z "$AI_PID" ] || wait "$AI_PID" >/dev/null 2>&1 || true
  [ -z "$ADOPT_PID" ] || kill "$ADOPT_PID" >/dev/null 2>&1 || true
  [ -z "$ADOPT_PID" ] || wait "$ADOPT_PID" >/dev/null 2>&1 || true
  if [ -n "${AUTH:-}" ]; then
    for id in $CREATED_SCHEDULES; do curl -s -X DELETE "$API/backups/scheduled/$id" -H "$AUTH" >/dev/null; done
    [ -z "${COMP_ID:-}" ] || curl -s -X DELETE "$API/components/$COMP_ID" -H "$AUTH" -H 'Content-Type: application/json' -d '{"mode":"keep_service","reason":"closure cleanup"}' >/dev/null
    for id in $CREATED_USERS; do curl -s -X DELETE "$API/users/$id" -H "$AUTH" >/dev/null; done
  fi
}
trap cleanup EXIT INT TERM

ok() { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: ${2:-})"; }
require() { if [ "$2" != "$3" ]; then bad "$1" "$3, expected $2"; exit 1; fi; }
require_id() { [[ "$2" =~ ^[1-9][0-9]*$ ]] || { bad "$1 prerequisite returned no usable ID"; exit 1; }; }
expect() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "$3, expected $2"; fi; }
call() {
  local method="$1" path="$2" auth="${3:-}" body="${4:-}" raw
  local headers=()
  if [ -n "${PREFLIGHT_TOKEN:-}" ] && [ "$path" = "/components" ]; then
    headers=(-H "X-PromLifecycle-Preflight-Token: $PREFLIGHT_TOKEN")
  fi
  if [ -n "$body" ]; then
    raw=$(curl --max-time 30 -s "${headers[@]}" -w $'\n%{http_code}' -X "$method" "$API$path" -H "$auth" -H 'Content-Type: application/json' -d "$body")
  elif [ -n "$auth" ]; then
    raw=$(curl --max-time 30 -s "${headers[@]}" -w $'\n%{http_code}' -X "$method" "$API$path" -H "$auth")
  else
    raw=$(curl --max-time 30 -s "${headers[@]}" -w $'\n%{http_code}' -X "$method" "$API$path")
  fi
  CODE=${raw##*$'\n'}
  BODY=${raw%$'\n'*}
}
j() { BODY="$BODY" python3 -c "import json,os;d=json.loads(os.environ['BODY']);print($1)" 2>/dev/null; }

if [ -n "$ADMIN_PASSWORD_FILE" ] && [ -r "$ADMIN_PASSWORD_FILE" ]; then
  ADMIN_PASSWORD=$(tr -d '\r\n' < "$ADMIN_PASSWORD_FILE")
fi
if [ -z "$ADMIN_PASSWORD" ] || [ -z "$DATA_DIR" ]; then
  echo "SKIP: product closure needs ADMIN_PASSWORD and DATA_DIR"
  exit 0
fi

LOGIN_PAYLOAD=$(ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["ADMIN_PASSWORD"]}))')
call POST /auth/login '' "$LOGIN_PAYLOAD"
expect "system administrator login" 200 "$CODE"
require "administrator login prerequisite" 200 "$CODE"
TOKEN=$(j "d.get('token','')")
AUTH="Authorization: Bearer $TOKEN"
NODE_ID=$(curl -s "$API/node" -H "$AUTH" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
TS=$(date +%s)

printf '%s\n' '=== Product closure: roles and local inventory ==='
make_user() {
  local role="$1"
  local name="closure-${role}-${TS}"
  local password="closure-${TS}-${role}-Aa9"
  call POST /users "$AUTH" "{\"username\":\"$name\",\"password\":\"$password\",\"role\":\"$role\"}"
  expect "create $role" 201 "$CODE"
  local id token
  id=$(j "d.get('id','')")
  require_id "$role user" "$id"
  CREATED_USERS="$CREATED_USERS $id"
  call POST /auth/login '' "{\"username\":\"$name\",\"password\":\"$password\"}"
  require "$role login prerequisite" 200 "$CODE"
  token=$(j "d.get('token','')")
  [ -n "$token" ] || { bad "$role login missing token"; exit 1; }
  case "$role" in
    viewer) VIEW_AUTH="Authorization: Bearer $token" ;;
    operator) OP_AUTH="Authorization: Bearer $token" ;;
    change_admin) CHANGE_AUTH="Authorization: Bearer $token" ;;
    auditor) AUDIT_AUTH="Authorization: Bearer $token" ;;
  esac
}
make_user viewer
make_user operator
make_user change_admin
make_user auditor
call POST /onboarding/acceptance "$OP_AUTH" '{}'
expect "operator receives onboarding acceptance evidence" 422 "$CODE"
acceptance_checks=$(j "len(d.get('checks',[]))")
[ "$acceptance_checks" -gt 0 ] && ok "onboarding acceptance evidence" || bad "onboarding acceptance evidence" "$acceptance_checks"

call GET /hosts "$AUTH"
require "local inventory prerequisite" 200 "$CODE"
HOST_ID=$(j "next((h['id'] for h in d.get('hosts',[]) if h.get('connection_type')=='local'), '')")
require_id "auto-registered local host" "$HOST_ID"
ok "reuse auto-registered local host"
FIXTURE_DIR="$DATA_DIR/closure-$TS/fixture"
bash "$(dirname "$0")/build-node-fixture.sh" "$FIXTURE_DIR/node_exporter" || exit 1
"$FIXTURE_DIR/node_exporter" --web.listen-address=127.0.0.1:19198 >"$DATA_DIR/adoption-$TS.log" 2>&1 &
ADOPT_PID=$!
for _ in $(seq 1 20); do
  curl -fsS "http://127.0.0.1:19198/metrics" >/dev/null 2>&1 && break
  sleep 0.1
done
call POST "/hosts/$HOST_ID/adopt" "$AUTH" "{\"type\":\"node_exporter\",\"port\":19198,\"name\":\"closure-managed-$TS\",\"ownership\":\"managed\"}"
expect "managed takeover blocked by failing preflight" 422 "$CODE"
call POST "/hosts/$HOST_ID/adopt" "$AUTH" "{\"type\":\"node_exporter\",\"port\":19198,\"name\":\"closure-observe-$TS\",\"ownership\":\"observe\"}"
expect "observe local takeover" 201 "$CODE"
require "observe adoption prerequisite" 201 "$CODE"
ADOPTED_ID=$(j "d.get('component',{}).get('id','')")
require_id "adopted component" "$ADOPTED_ID"
ADOPTION_MODE=$(j "d.get('adoption',{}).get('ownership','')")
expect "observe takeover ownership" observe "$ADOPTION_MODE"
call DELETE "/components/$ADOPTED_ID/adoption" "$AUTH"
expect "non-destructive observe release" 200 "$CODE"
kill -0 "$ADOPT_PID" 2>/dev/null && ok "release leaves process running" || bad "release leaves process running" "stopped"
kill "$ADOPT_PID" >/dev/null 2>&1 || true
wait "$ADOPT_PID" >/dev/null 2>&1 || true
ADOPT_PID=""
COMPONENT_PAYLOAD="{\"type\":\"node_exporter\",\"name\":\"closure-target-$TS\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19199}"
call POST /components/preflight "$AUTH" "$COMPONENT_PAYLOAD"
require "target preflight prerequisite" 200 "$CODE"
PREFLIGHT_TOKEN=$(j "d.get('preflight_token','')")
[ -n "$PREFLIGHT_TOKEN" ] || { bad "missing preflight credential"; exit 1; }
call POST /components "$AUTH" "$COMPONENT_PAYLOAD"
PREFLIGHT_TOKEN=""
expect "register local component" 202 "$CODE"
COMP_ID=$(j "d.get('component',{}).get('id','')")
require_id "target component" "$COMP_ID"
for _ in $(seq 1 20); do
  call GET "/components/$COMP_ID" "$AUTH"
  [ "$(j "d.get('status','')")" = "running" ] && break
  sleep 1
done

require "target running prerequisite" running "$(j "d.get('status','')")"

CONFIG_PATH="$DATA_DIR/closure-$TS/config.yml"
DATA_PATH="$DATA_DIR/closure-$TS/data"
mkdir -p "$(dirname "$CONFIG_PATH")" "$DATA_PATH"
printf 'global:\n  scrape_interval: 15s\n' > "$CONFIG_PATH"
printf 'closure-data-%s\n' "$TS" > "$DATA_PATH/marker"
DB_PATH="$DATA_DIR/promlifecycle.db"
python3 - "$DB_PATH" "$COMP_ID" "$CONFIG_PATH" "$DATA_PATH" <<'PY'
import sqlite3,sys
path, component_id, config_path, data_path = sys.argv[1:]
with sqlite3.connect(path) as db:
    db.execute("UPDATE components SET config_path=?, data_path=?, deploy_type='binary', service_name='closure-mock', internal_url='', port=0, status='running' WHERE id=?", (config_path, data_path, int(component_id)))
PY

printf '%s\n' '=== Product closure: scheduled backups, manifests and recovery ==='
for scope in config data full; do
  call POST /backups/scheduled "$CHANGE_AUTH" "{\"name\":\"closure-$scope-$TS\",\"cron\":\"0 3 * * *\",\"type\":\"$scope\",\"keep_count\":2,\"enabled\":true,\"component_id\":$COMP_ID}"
  expect "schedule $scope backup" 201 "$CODE"
  schedule_id=$(j "d.get('id','')")
  require_id "$scope backup schedule" "$schedule_id"
  CREATED_SCHEDULES="$CREATED_SCHEDULES $schedule_id"

  call POST /backups "$CHANGE_AUTH" "{\"name\":\"closure-$scope-$TS\",\"type\":\"$scope\",\"component_id\":$COMP_ID}"
  expect "create $scope backup" 201 "$CODE"
  backup_id=$(j "d.get('backup',{}).get('id','')")
  require_id "$scope backup" "$backup_id"
  checksum=$(j "d.get('manifest',{}).get('sha256','')")
  [ ${#checksum} -eq 64 ] && ok "$scope manifest SHA-256" || bad "$scope manifest SHA-256" "$checksum"
  case "$scope" in config) CONFIG_BACKUP=$backup_id;; data) DATA_BACKUP=$backup_id;; full) FULL_BACKUP=$backup_id; FULL_PATH=$(j "d.get('backup',{}).get('path','')");; esac
  call POST "/backups/$backup_id/verify" "$CHANGE_AUTH"
  expect "verify $scope backup" 200 "$CODE"
done
call GET /backups/scheduled/statuses "$AUTH"
expect "scheduled backup status endpoint" 200 "$CODE"

cp "$FULL_PATH" "$FULL_PATH.clean"
printf 'corrupt' >> "$FULL_PATH"
call POST "/backups/$FULL_BACKUP/verify" "$CHANGE_AUTH"
expect "reject corrupted backup" 422 "$CODE"
mv "$FULL_PATH.clean" "$FULL_PATH"
call POST "/backups/$FULL_BACKUP/verify" "$CHANGE_AUTH"
expect "accept restored artifact" 200 "$CODE"

printf 'global:\n  scrape_interval: 30s\n' > "$CONFIG_PATH"
call POST "/backups/$CONFIG_BACKUP/restore" "$CHANGE_AUTH"
expect "restore with pre_restore checkpoint" 200 "$CODE"
grep -q '15s' "$CONFIG_PATH" && ok "restore replaced live configuration" || bad "restore replaced live configuration" "$(cat "$CONFIG_PATH")"
# Restore checkpoints are durable recovery records, separate from manual backups.
if python3 - "$DB_PATH" "$COMP_ID" <<'PY'
import json, pathlib, sqlite3, sys
with sqlite3.connect(sys.argv[1]) as db:
    row = db.execute("SELECT path, paths, status FROM component_restore_checkpoints WHERE component_id=? ORDER BY id DESC LIMIT 1", (int(sys.argv[2]),)).fetchone()
assert row and row[2] == "applied", row
assert "30s" in (pathlib.Path(row[0]) / "0").read_text(), "checkpoint did not preserve pre-restore configuration"
PY
then ok "pre_restore checkpoint preserves prior configuration"
else bad "pre_restore checkpoint evidence"; fi

call POST /recovery-exercises "$CHANGE_AUTH" "{\"name\":\"closure-rehearsal-$TS\",\"backup_id\":$FULL_BACKUP}"
expect "create recovery exercise" 201 "$CODE"
EXERCISE_ID=$(j "d.get('recovery_exercise',{}).get('id','')")
require_id "recovery exercise" "$EXERCISE_ID"
call POST "/recovery-exercises/$EXERCISE_ID/run" "$CHANGE_AUTH"
expect "rehearse full backup" 200 "$CODE"
checks=$(j "len(d.get('result',{}).get('checks',[]))")
[ "$checks" -ge 6 ] && ok "rehearsal evidence" || bad "rehearsal evidence" "$checks"

printf '%s\n' '=== Product closure: direct lifecycle change ==='
call POST /change-plans "$CHANGE_AUTH" "{\"kind\":\"remove\",\"resource_type\":\"component\",\"resource_id\":$COMP_ID,\"reason\":\"closure direct removal\",\"risk\":\"high\",\"after\":{\"mode\":\"keep_service\"}}"
expect "create lifecycle change record" 201 "$CODE"
PLAN_ID=$(j "d.get('plan',{}).get('id','')")
require_id "change plan" "$PLAN_ID"
DIFF=$(j "d.get('plan',{}).get('diff','')")
[ -n "$DIFF" ] && ok "change record includes diff" || bad "change record includes diff" "empty"
call POST "/change-plans/$PLAN_ID/approve" "$AUTH"
expect "approval route removed" 404 "$CODE"
call POST "/change-plans/$PLAN_ID/execute" "$CHANGE_AUTH"
expect "execute change directly" 200 "$CODE"
call POST "/change-plans/$PLAN_ID/verify" "$CHANGE_AUTH"
expect "verify executed change" 200 "$CODE"
call POST "/change-plans/$PLAN_ID/rollback" "$CHANGE_AUTH"
expect "rollback with recovery point" 200 "$CODE"
call GET "/components/$COMP_ID" "$AUTH"
expect "rollback restored component" 200 "$CODE"

printf '%s\n' '=== Product closure: configuration drift ==='
python3 - "$DB_PATH" "$COMP_ID" "$CONFIG_PATH" <<'PY'
import sqlite3,sys
with sqlite3.connect(sys.argv[1]) as db:
    component_id = int(sys.argv[2])
    config_path = sys.argv[3]
    db.execute("UPDATE components SET deploy_type='binary', config_path=? WHERE id=?", (config_path, component_id))
    db.execute("DELETE FROM component_configs WHERE component_id=?", (component_id,))
    db.execute(
        "INSERT INTO component_configs(component_id,name,content_type,content,version,is_active,file_path,description,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)",
        (component_id, "config.yml", "yaml", "global:\\n  scrape_interval: 99s\\n", 1, 1, config_path, "closure drift baseline"),
    )
PY
call GET /config/drift "$AUTH"
expect "configuration drift check" 200 "$CODE"

printf '%s\n' '=== Product closure: audit ==='
call GET /operations/logs/export "$VIEW_AUTH"
expect "viewer audit CSV denied" 403 "$CODE"
call GET /operations/logs/export "$AUDIT_AUTH"
expect "auditor audit CSV allowed" 200 "$CODE"
printf '%s' "$BODY" | grep -q 'correlation_id' && ok "audit CSV header" || bad "audit CSV header" "missing correlation_id"

printf '\nProduct closure: %s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
