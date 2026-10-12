#!/bin/bash
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
TS=$(date +%s)
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
require_packages node_exporter:v1.8.2 node_exporter:v1.7.0
HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"rb-host-$TS\",\"ip\":\"127.0.0.66\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"rb-node\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19120}" | j "d['component']['id']")
ok "创建组件 id=$COMP_ID"
for i in $(seq 1 40); do S=$(curl -s $API/components/$COMP_ID -H "$AUTH" | j "d['status']"); [ "$S" = "running" ] || [ "$S" = "error" ] && break; sleep 3; done
[ "$S" = "running" ] && ok "部署 running" || bad "部署" "$S"
governed_action "$API/components/$COMP_ID/upgrade" '{"new_version":"v1.7.0","reason":"test lifecycle"}' "$AUTH" >/dev/null
sleep 10
V=$(curl -s -m 5 http://127.0.0.1:19120/metrics 2>/dev/null | grep '^node_exporter_build_info' | grep -o 'version="1\.7\.0"')
[ -n "$V" ] && ok "升级到 v1.7.0" || bad "升级 v1.7.0" "$V"
RB=$(governed_code "$API/components/$COMP_ID/rollback" '{"target_version":"v1.8.2","reason":"binary rollback verification"}' "$AUTH")
[ "$RB" = "200" ] && ok "rollback 计划执行并验证" || bad "rollback 计划" "$RB"
sleep 10
V2=$(curl -s -m 5 http://127.0.0.1:19120/metrics 2>/dev/null | grep '^node_exporter_build_info' | grep -o 'version="1\.8\.2"')
[ -n "$V2" ] && ok "回滚后运行 v1.8.2" || bad "回滚版本" "$V2"
ACTIVE=$(systemctl is-active promlifecycle-node_exporter 2>&1)
[ "$ACTIVE" = "active" ] && ok "回滚后 systemd active" || bad "systemd" "$ACTIVE"
ST=$(curl -s $API/components/$COMP_ID -H "$AUTH" | j "d['status']")
[ "$ST" = "running" ] && ok "回滚后状态 running" || bad "状态" "$ST"
governed_action "$API/components/$COMP_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" >/dev/null
echo ""
echo "Rollback Binary: $PASS passed, $FAIL failed"
[ $FAIL -eq 0 ]
