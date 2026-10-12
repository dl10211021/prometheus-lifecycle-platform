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
require_packages prometheus:v2.53.0
TS=$(date +%s)

echo "=== 1. 准备（主机/节点）==="
HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e-local-$TS\",\"ip\":\"127.0.0.65\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | json "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | json "d['id']")
ok "主机 id=$HOST_ID / 节点 id=$NODE_ID"

echo "=== 2. 二进制部署 Prometheus（systemd）==="
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"type\":\"prometheus\",\"name\":\"bin-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HOST_ID,\"port\":19161}" | json "d['component']['id']")
STATUS="pending"
for i in $(seq 1 60); do
  STATUS=$(curl -s $API/components/$COMP_ID -H "$AUTH" | json "d['status']")
  [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ] && break
  sleep 3
done
[ "$STATUS" = "running" ] && ok "组件状态 running（等待 $((i*3))s）" || { bad "组件状态 running" "$STATUS"; curl -s $API/components/$COMP_ID -H "$AUTH" | python3 -m json.tool | head -15; }

ACTIVE=$(systemctl is-active promlifecycle-prometheus 2>&1)
[ "$ACTIVE" = "active" ] && ok "systemd 单元 active" || bad "systemd 单元" "$ACTIVE"
HEALTH=$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:19161/-/healthy)
[ "$HEALTH" = "200" ] && ok "Prometheus /-/healthy 200" || bad "Prometheus 健康" "$HEALTH"

echo "=== 3. 告警规则 → /etc/prometheus/rules + Prometheus 生效 ==="
RULE_ID=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"cluster_id\":$NODE_ID,\"name\":\"PromDown-$TS\",\"expression\":\"up == 0\",\"severity\":\"warning\",\"duration\":\"1m\"}" | json "d['id']")
sleep 3
RULE_FILE=/etc/prometheus/rules/rules.yml
[ -f "$RULE_FILE" ] && grep -q "PromDown-$TS" "$RULE_FILE" && ok "rules.yml 已写入并包含规则" || bad "rules.yml 包含规则" "$(cat $RULE_FILE 2>/dev/null | head -5)"
API_RULES=$(curl -s -m 5 "http://127.0.0.1:19161/api/v1/rules" 2>/dev/null | grep -c "PromDown-$TS")
[ "$API_RULES" -gt 0 ] && ok "Prometheus /api/v1/rules 已加载告警规则" || bad "Prometheus 加载规则" "$API_RULES"

echo "=== 4. scrape 配置 → prometheus.yml + Prometheus 生效 ==="
SC_ID=$(curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"cluster_id\":$NODE_ID,\"name\":\"bin-job-$TS\",\"job_name\":\"bin_job_$TS\",\"targets\":\"[\\\"1.2.3.4:9100\\\"]\"}" | json "d['id']")
sleep 4
CFG_SYNCED=$(grep -c "bin_job_$TS" /etc/prometheus/prometheus.yml 2>/dev/null)
[ "$CFG_SYNCED" -gt 0 ] && ok "prometheus.yml 已包含新 job" || bad "prometheus.yml 包含新 job" "$CFG_SYNCED"
for i in $(seq 1 10); do
  TARGETS=$(curl -s -m 5 "http://127.0.0.1:19161/api/v1/targets" 2>/dev/null | grep -c "bin_job_$TS")
  [ "$TARGETS" -gt 0 ] && break
  sleep 2
done
[ "$TARGETS" -gt 0 ] && ok "Prometheus targets 已加载新 job" || bad "Prometheus targets 加载" "$TARGETS"

echo "=== 5. 删除组件并清理 ==="
RC=$(governed_code "$API/components/$COMP_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH")
[ "$RC" = "200" ] && ok "删除组件 200" || bad "删除组件" "$RC"
sleep 2
[ "$(systemctl is-active promlifecycle-prometheus 2>&1 || true)" != "active" ] && ok "prometheus 服务已停止" || bad "prometheus 服务已停止" "$(systemctl is-active promlifecycle-prometheus 2>&1)"
[ "$(ss -tln 2>/dev/null | grep -c ':19161 ')" = "0" ] && ok "19161 已释放" || bad "19161 已释放" "$(ss -tln | grep 19161)"
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" > /dev/null
ok "节点/主机已清理"

echo ""
echo "============================================"
echo "Prometheus Binary Test: $PASS passed, $FAIL failed"
echo "============================================"
[ $FAIL -eq 0 ] || exit 1
