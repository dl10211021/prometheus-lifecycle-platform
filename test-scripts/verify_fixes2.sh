#!/bin/bash
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
TS=$(date +%s)

# Cleanup: this suite's host/node/component must not leak into later
# suites (residue components block host deletion in e2e_full).
cleanup() {
  [ -n "${COMP_ID:-}" ] && governed_action "$API/components/$COMP_ID/uninstall" '{"mode":"keep_service","reason":"test cleanup"}' "$AUTH" >/dev/null 2>&1
  [ "$(code $API/node -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code $API/node -H "$AUTH")"
  [ -n "${HOST_ID:-}" ] && curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" >/dev/null 2>&1
}
trap cleanup EXIT

HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"fix-local-$TS\",\"ip\":\"127.0.0.50\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"fix-node-$TS\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19100}" | j "d['component']['id']")
[ -n "$COMP_ID" ] && ok "创建组件 id=$COMP_ID" || bad "创建组件" "$COMP_ID"
FIX_DIR="$DATA_DIR/fix-$TS"
CFG_FILE="$FIX_DIR/node.yml"
mkdir -p "$FIX_DIR/data"
printf 'k: baseline\n' > "$CFG_FILE"
python3 - "$DATA_DIR/promlifecycle.db" "$COMP_ID" "$CFG_FILE" "$FIX_DIR/data" <<'PY'
import sqlite3,sys
with sqlite3.connect(sys.argv[1]) as db:
    db.execute("UPDATE components SET deploy_type='binary',status='stopped',config_path=?,data_path=? WHERE id=?", (sys.argv[3],sys.argv[4],int(sys.argv[2])))
PY

echo "=== 1. 路径穿越校验 ==="
R2=$(curl -s -o /dev/null -w '%{http_code}' -X POST $API/components/$COMP_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"../../etc/cron","content":"global:\n  x: 1","content_type":"yaml"}')
[ "$R2" = "400" ] && ok "配置名 ../ 被拒绝 ($R2)" || bad "配置名 ../ 被拒绝" "$R2"
R3=$(curl -s -o /dev/null -w '%{http_code}' -X PUT $API/gitops/config -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$R3" = "404" ] && ok "非生命周期 GitOps 路由已移除 ($R3)" || bad "GitOps 路由残留" "$R3"

echo "=== 2. 配置回滚落盘 ==="
governed_action "$API/components/$COMP_ID/configs" '{"name":"node.yml","content":"k: one","content_type":"yaml","reason":"verify config v1"}' "$AUTH" >/dev/null
V1=$(curl -s $API/components/$COMP_ID/configs -H "$AUTH" | j "max([x['version'] for x in d.get('configs',[])],default=0)")
governed_action "$API/components/$COMP_ID/configs" '{"name":"node.yml","content":"k: two","content_type":"yaml","reason":"verify config v2"}' "$AUTH" >/dev/null
V2=$(curl -s $API/components/$COMP_ID/configs -H "$AUTH" | j "max([x['version'] for x in d.get('configs',[])],default=0)")
[ "$V1" -gt 0 ] && [ "$V2" -gt "$V1" ] && ok "创建 config v1=$V1 v2=$V2" || bad "创建 config" "v1=$V1 v2=$V2"
RB_CODE=$(governed_code "$API/configs/$COMP_ID/rollback" "{\"config_name\":\"node.yml\",\"version\":$V1,\"reason\":\"verify rollback\"}" "$AUTH")
[ "$RB_CODE" = "200" ] && ok "rollback 治理计划执行" || bad "rollback 接口" "$RB_CODE"
ACTIVE_V=$(curl -s $API/components/$COMP_ID/configs -H "$AUTH" | j "[x for x in d['configs'] if x['is_active']][0]['version']")
ACTIVE_CONTENT=$(curl -s "$API/components/$COMP_ID/configs?raw=1" -H "$AUTH" | j "[x for x in d['configs'] if x['is_active']][0]['content']")
[ "$ACTIVE_V" -gt "$V2" ] && [ "$ACTIVE_CONTENT" = "k: one" ] && ok "回滚生成新活动版本 v$ACTIVE_V（内容来自 v$V1）" || bad "活动版本" "v=$ACTIVE_V content=$ACTIVE_CONTENT"
[ -f "$CFG_FILE" ] && grep -q "k: one" "$CFG_FILE" && ok "磁盘文件内容为 v1 ($CFG_FILE)" || bad "磁盘文件内容为 v1" "$CFG_FILE $(cat "$CFG_FILE" 2>/dev/null)"
LOGGED=$(curl -s "$API/operations/logs?limit=20" -H "$AUTH" | j "any(x['action']=='rollback_config' for x in d['logs'])")
[ "$LOGGED" = "True" ] && ok "审计日志记录 rollback_config" || bad "审计日志记录 rollback_config" "$LOGGED"

echo
echo "===== Result: $PASS passed, $FAIL failed ====="
