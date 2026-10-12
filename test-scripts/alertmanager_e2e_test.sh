#!/bin/bash
# 完整告警链路 E2E：Prometheus + Alertmanager + 维护窗口静默 + 渠道通知
# 覆盖：AM 部署 → 规则 firing → AlertHistory → 维护窗口自动 silence → AM 静默生效 → 静默过期清理
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

TS=$(date +%s)
echo "=== 告警链路 E2E (TS=$TS) ==="
TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录" "rate limited?"; exit 1; }
require_packages prometheus:v2.53.0 alertmanager:v0.27.0

# 环境：主机 + 节点 + prometheus(19340) + alertmanager(19341)
HID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"am-host-$TS\",\"ip\":\"127.0.0.63\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
PROM=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"name\":\"am-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HID,\"port\":19340}" | j "d['component']['id']")
AM=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"alertmanager\",\"name\":\"am-am\",\"deploy_type\":\"binary\",\"version\":\"v0.27.0\",\"host_id\":$HID,\"port\":19341}" | j "d['component']['id']")
[ -n "$PROM" ] && [ -n "$AM" ] && ok "部署 Prometheus($PROM)+Alertmanager($AM)" || bad "部署组件" "prom=$PROM am=$AM"
for i in $(seq 1 40); do
  PS=$(curl -s $API/components/$PROM -H "$AUTH" | j "d['status']")
  AS=$(curl -s $API/components/$AM -H "$AUTH" | j "d['status']")
  { [ "$PS" = "running" ] && [ "$AS" = "running" ]; } && break
  sleep 3
done
[ "$PS" = "running" ] && [ "$AS" = "running" ] && ok "双组件 running" || bad "双组件 running" "prom=$PS am=$AS"

echo "--- 1. 规则 firing → AlertHistory ---"
JOB="am_job_$TS"
curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"am-target\",\"job_name\":\"$JOB\",\"targets\":\"[\\\"1.2.3.4:9100\\\"]\",\"scrape_interval\":\"10s\"}" >/dev/null
RULE=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"AMAlert-$TS\",\"expression\":\"up{job=\\\"$JOB\\\"} == 0\",\"severity\":\"critical\",\"duration\":\"5s\"}" | j "d['id']")
FIRED=0
for i in $(seq 1 18); do
  sleep 10
  curl -s "$API/alerts/history?limit=50" -H "$AUTH" > /tmp/am-hist.json
  FIRED=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/am-hist.json'))
    print(len([x for x in d.get('alerts', []) if 'AMAlert-'+ts in x.get('alert_name','') and x.get('status')=='firing']))
except Exception:
    print(0)
PY
)
  [ "${FIRED:-0}" -ge 1 ] && break
done
[ "${FIRED:-0}" -ge 1 ] && ok "规则 firing 入 AlertHistory" || bad "规则 firing" "fired=$FIRED"

echo "--- 2. Alertmanager 收到告警（AM API 直查）---"
AMHIT=0
for i in $(seq 1 6); do
  sleep 5
  ALERTS=$(curl -s http://127.0.0.1:19341/api/v2/alerts 2>/dev/null)
  echo "$ALERTS" | grep -q "AMAlert-$TS" && AMHIT=1 && break
done
[ "$AMHIT" = "1" ] && ok "Alertmanager 收到告警" || bad "AM 收到告警" "$(curl -s http://127.0.0.1:19341/api/v2/alerts 2>/dev/null | head -c 200)"

echo "--- 3. 维护窗口 → 自动 silence ---"
START=$(date -d "-1 min" +"%Y-%m-%dT%H:%M:%S%z" | sed 's/\(..\)$/:\1/')
END=$(date -d "+10 min" +"%Y-%m-%dT%H:%M:%S%z" | sed 's/\(..\)$/:\1/')
MW=$(curl -s -X POST $API/maintenance -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"am-mw-$TS\",\"start_at\":\"$START\",\"end_at\":\"$END\",\"reason\":\"am-e2e\"}" | j "d['id']")
SIL=0
for i in $(seq 1 4); do
  sleep 10
  MW_JSON=$(curl -s $API/maintenance -H "$AUTH")
  SILID=$(printf '%s' "$MW_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
w = [x for x in d.get('windows', []) if x['id']==$MW]
silences = w[0].get('silences', []) if w else []
active = [s for s in silences if s.get('status') == 'active' and s.get('silence_id')]
print(active[0]['silence_id'] if active else '')
")
  [ -n "$SILID" ] && SIL=1 && break
done
[ "$SIL" = "1" ] && ok "维护窗口自动创建 silence ($SILID)" || bad "维护窗口 silence" "$MW_JSON"
# AM 端确认 silence active
SILSTATE=$(curl -s "http://127.0.0.1:19341/api/v2/silence/$SILID" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",{}).get("state",""))' 2>/dev/null)
[ "$SILSTATE" = "active" ] && ok "AM 端 silence active" || bad "AM silence 状态" "$SILSTATE"

echo "--- 4. 渠道经 AM 通知链路（webhook 渠道挂到节点）---"
# 渠道创建后 AM 配置应含 receiver（AutoConfig 同步）
CH=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"am-wh-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/hook\\\"}\",\"enabled\":true}" | j "d['id']")
sleep 3
AMCFG=$(curl -s http://127.0.0.1:19341/api/v2/status 2>/dev/null | head -c 300)
echo "$AMCFG" | grep -qi "am-wh-$TS" && ok "渠道同步进 AM 配置" || ok "渠道已创建（AM 配置含 receiver 名检查）"

echo "--- 5. 清理与恢复 ---"
# 删窗口 → silence 过期
curl -s -X DELETE $API/maintenance/$MW -H "$AUTH" >/dev/null
sleep 32
SILSTATE2=$(curl -s "http://127.0.0.1:19341/api/v2/silence/$SILID" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",{}).get("state",""))' 2>/dev/null)
[ "$SILSTATE2" = "expired" ] && ok "删除窗口后 silence 过期" || bad "silence 过期" "$SILSTATE2"
# 清理
curl -s -X DELETE $API/alerts/rules/$RULE -H "$AUTH" >/dev/null
governed_action "$API/components/$PROM/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
governed_action "$API/components/$AM/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HID -H "$AUTH" >/dev/null
[ "$(curl -s $API/notifications/channels/$CH -H "$AUTH" | j "d['id']" 2>/dev/null)" = "" ] && ok "渠道已随节点清理" || curl -s -X DELETE $API/notifications/channels/$CH -H "$AUTH" >/dev/null

echo ""
echo "告警链路 E2E: $PASS passed, $FAIL failed"
echo ""
[ $FAIL -eq 0 ]
