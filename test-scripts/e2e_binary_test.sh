#!/bin/bash
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
json() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | json "d['token']")
AUTH="Authorization: Bearer $TOKEN"
require_packages blackbox_exporter:v0.24.0 blackbox_exporter:v0.26.0
TS=$(date +%s)

echo "=== 1. 创建本地主机 + 节点 ==="
HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e-local-$TS\",\"ip\":\"127.0.0.64\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | json "d['id']")
[ -n "$HOST_ID" ] && ok "创建主机 id=$HOST_ID" || bad "创建主机" "$HOST_ID"
NODE_ID=$(curl -s $API/node -H "$AUTH" | json "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点 id=$NODE_ID" || bad "读取节点" "$NODE_ID"

echo "=== 2. 二进制部署 blackbox_exporter（systemd，无 Docker）==="
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"type\":\"blackbox_exporter\",\"name\":\"bin-blackbox-exporter\",\"deploy_type\":\"binary\",\"version\":\"v0.24.0\",\"host_id\":$HOST_ID,\"port\":19160}" | json "d['component']['id']")
[ -n "$COMP_ID" ] && ok "创建组件 id=$COMP_ID (deploy_type=binary)" || bad "创建组件" "$COMP_ID"

STATUS="pending"
for i in $(seq 1 60); do
  STATUS=$(curl -s $API/components/$COMP_ID -H "$AUTH" | json "d['status']")
  [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ] && break
  sleep 3
done
echo "  → 最终状态: $STATUS（等待 $((i*3))s）"
[ "$STATUS" = "running" ] && ok "组件状态 running" || { bad "组件状态 running" "$STATUS"; curl -s $API/components/$COMP_ID -H "$AUTH" | python3 -m json.tool 2>/dev/null | head -20; }

echo "=== 3. 本机 systemd 单元与端口 ==="
ACTIVE=$(systemctl is-active promlifecycle-blackbox_exporter 2>&1)
[ "$ACTIVE" = "active" ] && ok "systemd 单元 active ($ACTIVE)" || bad "systemd 单元 active" "$ACTIVE"
METRICS=$(curl -s -m 5 http://127.0.0.1:19160/metrics 2>/dev/null | grep -c '^blackbox_' )
[ "$METRICS" -gt 0 ] && ok "19160 指标可达 ($METRICS 行 blackbox_*)" || bad "19160 指标" "$METRICS"
systemctl cat promlifecycle-blackbox_exporter 2>/dev/null | grep -E '^(ExecStart|Restart=)' | head -3

echo "=== 4. API 健康检查 ==="
HEALTH=$(curl -s $API/components/$COMP_ID/health -H "$AUTH" | json "d['status']")
[ "$HEALTH" = "healthy" ] && ok "API 健康检查 healthy" || bad "API 健康检查" "$HEALTH"

echo "=== 5. 重启 ==="
RC=$(curl -s -o /dev/null -w '%{http_code}' -X POST $API/components/$COMP_ID/restart -H "$AUTH")
[ "$RC" = "200" ] && ok "restart 接口 200" || bad "restart 接口" "$RC"
sleep 2
ACTIVE2=$(systemctl is-active promlifecycle-blackbox_exporter 2>&1)
[ "$ACTIVE2" = "active" ] && ok "重启后仍 active" || bad "重启后仍 active" "$ACTIVE2"

echo "=== 6. 升级到 v0.26.0 ==="
UP=$(governed_action "$API/components/$COMP_ID/upgrade" '{"new_version":"v0.26.0","reason":"test lifecycle"}' "$AUTH")
UP_STATUS=$(echo "$UP" | json "d.get('plan',{}).get('status','')")
[ "$UP_STATUS" = "verified" ] && ok "upgrade 计划执行并验证" || bad "upgrade 计划" "$UP"
sleep 8
ACTIVE3=$(systemctl is-active promlifecycle-blackbox_exporter 2>&1)
VERS=$(curl -s -m 5 http://127.0.0.1:19160/metrics 2>/dev/null | grep '^blackbox_exporter_build_info' | head -1)
[ "$ACTIVE3" = "active" ] && ok "升级后服务 active" || bad "升级后服务 active" "$ACTIVE3"
echo "  → $VERS"

echo "=== 7. 删除组件（systemd 单元应被移除）==="
RC=$(governed_code "$API/components/$COMP_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH")
[ "$RC" = "200" ] && ok "删除组件 200" || bad "删除组件" "$RC"
sleep 2
UNIT_STATE=$(systemctl is-active promlifecycle-blackbox_exporter 2>&1 || true)
UNIT_FILE=$(ls /etc/systemd/system/promlifecycle-blackbox_exporter.service 2>/dev/null || echo gone)
[ "$UNIT_FILE" = "gone" ] && ok "unit 文件已删除" || bad "unit 文件已删除" "$UNIT_FILE"
[ "$UNIT_STATE" != "active" ] && ok "服务已停止 ($UNIT_STATE)" || bad "服务已停止" "$UNIT_STATE"
PORT_STATE=$(ss -tln 2>/dev/null | grep -c ':19160 ')
[ "$PORT_STATE" = "0" ] && ok "19160 已释放" || bad "19160 已释放" "$PORT_STATE"

echo "=== 8. 清理 ==="
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" > /dev/null
ok "节点/主机已清理"

echo ""
echo "============================================"
echo "Binary Deploy Test: $PASS passed, $FAIL failed"
echo "============================================"
[ $FAIL -eq 0 ] || exit 1
