#!/bin/bash
# Alert pipeline E2E: rule -> Prometheus fires -> platform records firing -> target gone -> resolved (no flapping)
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

sleep 65  # wait out login rate-limit window from adversarial suite
TS=$(date +%s)
echo "=== Alert pipeline test (TS=$TS) ==="
TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录" "rate limited?"; exit 1; }
require_packages prometheus:v2.53.0

HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"alert-host-$TS\",\"ip\":\"127.0.0.62\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"name\":\"alert-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HOST_ID,\"port\":9091}" | j "d['component']['id']")
[ -n "$COMP_ID" ] && ok "部署 Prometheus id=$COMP_ID" || bad "部署 Prometheus" "$COMP_ID"
STATUS="pending"
for i in $(seq 1 40); do STATUS=$(curl -s $API/components/$COMP_ID -H "$AUTH" | j "d['status']"); [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ] && break; sleep 3; done
[ "$STATUS" = "running" ] && ok "Prometheus running" || bad "Prometheus running" "$STATUS"

JOB="down_job_$TS"
curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"down-target\",\"job_name\":\"$JOB\",\"targets\":\"[\\\"1.2.3.4:9100\\\"]\",\"scrape_interval\":\"15s\"}" >/dev/null
RULE_ID=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"AlertDown-$TS\",\"expression\":\"up{job=\\\"$JOB\\\"} == 0\",\"severity\":\"critical\",\"duration\":\"10s\"}" | j "d['id']")
ok "创建 down target + 告警规则 (rule=$RULE_ID)"

# Wait for rule to fire (Prometheus evaluation ~15s + scheduler poll 30s) up to 3 min
FIRED=""
for i in $(seq 1 18); do
  sleep 10
  curl -s "$API/alerts/history?limit=50" -H "$AUTH" > /tmp/alert-hist.json
  FIRED=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/alert-hist.json'))
    hits = [x for x in d.get('alerts', []) if 'AlertDown-'+ts in x.get('alert_name','')]
    print(len(hits))
except Exception:
    print(0)
PY
)
  [ "${FIRED:-0}" -gt 0 ] && break
done
FIRING_N=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/alert-hist.json'))
    print(len([x for x in d.get('alerts', []) if 'AlertDown-'+ts in x.get('alert_name','') and x.get('status')=='firing']))
except Exception:
    print(0)
PY
)
[ "${FIRING_N:-0}" -gt 0 ] && ok "告警进入 firing 状态 (count=$FIRING_N)" || bad "告警 firing" "hist=$(cat /tmp/alert-hist.json | head -c 300)"

# Verify it stays firing across two scheduler cycles (no 30s flapping)
STAY=0
for i in 1 2 3; do
  sleep 32
  curl -s "$API/alerts/history?limit=50" -H "$AUTH" > /tmp/alert-hist.json
  N2=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/alert-hist.json'))
    print(len([x for x in d.get('alerts', []) if 'AlertDown-'+ts in x.get('alert_name','') and x.get('status')=='firing']))
except Exception:
    print(0)
PY
)
  [ "${N2:-0}" -gt 0 ] && STAY=$((STAY+1))
done
[ "$STAY" = "3" ] && ok "连续 3 个采集周期保持 firing（无抖动）" || bad "firing 保持" "stay=$STAY"

# Remove the down target -> alert should resolve once, then stay resolved
curl -s $API/scrape-configs -H "$AUTH" > /tmp/sc.json
SCID=$(python3 - <<'PY'
import json
try:
    d = json.load(open('/tmp/sc.json'))
    items = d.get('configs', d if isinstance(d, list) else [])
    for x in items:
        if x.get('name') == 'down-target':
            print(x['id']); break
except Exception:
    pass
PY
)
[ -n "$SCID" ] && curl -s -X DELETE $API/scrape-configs/$SCID -H "$AUTH" >/dev/null
# Queue withdrawal of the alert rule too, avoiding the ~5 min up-series staleness.
# Acceptance is not withdrawal; the polling below must observe resolved history.
curl -s -X DELETE $API/alerts/rules/$RULE_ID -H "$AUTH" >/dev/null
RESOLVED=0
for i in $(seq 1 12); do
  sleep 10
  curl -s "$API/alerts/history?limit=50" -H "$AUTH" > /tmp/alert-hist.json
  RESOLVED=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/alert-hist.json'))
    print(len([x for x in d.get('alerts', []) if 'AlertDown-'+ts in x.get('alert_name','') and x.get('status')=='resolved' and x.get('ended_at')]))
except Exception:
    print(0)
PY
)
  [ "${RESOLVED:-0}" -gt 0 ] 2>/dev/null && break
done
[ "${RESOLVED:-0}" -gt 0 ] 2>/dev/null && ok "目标移除后告警 resolved (count=$RESOLVED)" || bad "告警 resolved" "$RESOLVED"
sleep 35
curl -s "$API/alerts/history?limit=50" -H "$AUTH" > /tmp/alert-hist.json
RESOLVED2=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/alert-hist.json'))
    print(len([x for x in d.get('alerts', []) if 'AlertDown-'+ts in x.get('alert_name','') and x.get('status')=='resolved']))
except Exception:
    print(0)
PY
)
FIRING2=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/alert-hist.json'))
    print(len([x for x in d.get('alerts', []) if 'AlertDown-'+ts in x.get('alert_name','') and x.get('status')=='firing']))
except Exception:
    print(0)
PY
)
[ "${FIRING2:-0}" = "0" ] && [ "${RESOLVED2:-0}" -gt 0 ] && ok "再等一个周期仍保持 resolved（无重新触发）" || bad "resolved 稳定" "firing=$FIRING2 resolved=$RESOLVED2"

governed_action "$API/components/$COMP_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" >/dev/null
echo ""
echo "Alert pipeline: $PASS passed, $FAIL failed"
echo ""
[ $FAIL -eq 0 ]
