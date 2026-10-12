#!/bin/bash
# AlertHistory labels-dedup verification:
# one alertname firing on TWO instances must create TWO independent history rows,
# and both must resolve independently when the targets disappear.
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

TS=$(date +%s)
echo "=== AlertHistory dedup test (TS=$TS) ==="
TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录" "rate limited?"; exit 1; }
require_packages prometheus:v2.53.0

HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"dedup-host-$TS\",\"ip\":\"127.0.0.61\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"name\":\"dedup-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HOST_ID,\"port\":9092}" | j "d['component']['id']")
[ -n "$COMP_ID" ] && ok "部署 Prometheus id=$COMP_ID" || { bad "部署 Prometheus" "$COMP_ID"; exit 1; }
STATUS="pending"
for i in $(seq 1 40); do STATUS=$(curl -s $API/components/$COMP_ID -H "$AUTH" | j "d['status']"); [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ] && break; sleep 3; done
[ "$STATUS" = "running" ] && ok "Prometheus running" || { bad "Prometheus running" "$STATUS"; exit 1; }

JOB="dedup_job_$TS"
curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"dedup-targets\",\"job_name\":\"$JOB\",\"targets\":\"[\\\"1.2.3.4:9100\\\",\\\"1.2.3.5:9100\\\"]\",\"scrape_interval\":\"10s\"}" >/dev/null
RULE_ID=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"DedupAlert-$TS\",\"expression\":\"up{job=\\\"$JOB\\\"} == 0\",\"severity\":\"critical\",\"duration\":\"5s\"}" | j "d['id']")
ok "创建双 target + 告警规则 (rule=$RULE_ID)"

# Wait for the alert to fire on both instances (evaluation ~10s + poll 30s)
N=0
for i in $(seq 1 18); do
  sleep 10
  curl -s "$API/alerts/history?limit=100" -H "$AUTH" > /tmp/dedup-hist.json
  N=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/dedup-hist.json'))
    hits = [x for x in d.get('alerts', []) if 'DedupAlert-'+ts == x.get('alert_name','') and x.get('status') == 'firing']
    print(len(hits))
except Exception:
    print(0)
PY
)
  [ "${N:-0}" -ge 2 ] && break
done
if [ "${N:-0}" -ge 2 ]; then
  ok "同名告警双实例各建一条历史 (firing=$N)"
  LABELS_N=$(python3 - <<'PY'
import json
d = json.load(open('/tmp/dedup-hist.json'))
hits = [x for x in d.get('alerts', []) if 'DedupAlert-' in x.get('alert_name','') and x.get('status') == 'firing']
print(len({x.get('labels') for x in hits}))
PY
)
  [ "$LABELS_N" = "2" ] && ok "两条历史 labels 不同 (distinct=$LABELS_N)" || bad "labels 区分" "distinct=$LABELS_N"
else
  bad "同名双实例各建一条历史" "firing=$N hist=$(head -c 400 /tmp/dedup-hist.json)"
fi

# Remove targets -> both must resolve independently
SCID=$(python3 - <<'PY'
import json
d = json.load(open('/tmp/dedup-hist.json'))
try:
    r = __import__('urllib.request', fromlist=['urlopen']).urlopen
except Exception:
    pass
PY
)
curl -s $API/scrape-configs -H "$AUTH" > /tmp/dedup-sc.json
SCID=$(python3 - <<'PY'
import json
try:
    d = json.load(open('/tmp/dedup-sc.json'))
    items = d.get('configs', d if isinstance(d, list) else [])
    for x in items:
        if x.get('name') == 'dedup-targets':
            print(x['id']); break
except Exception:
    pass
PY
)
[ -n "$SCID" ] && curl -s -X DELETE $API/scrape-configs/$SCID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/alerts/rules/$RULE_ID -H "$AUTH" >/dev/null
RES=0
for i in $(seq 1 12); do
  sleep 10
  curl -s "$API/alerts/history?limit=100" -H "$AUTH" > /tmp/dedup-hist.json
  RES=$(python3 - "$TS" <<'PY'
import sys, json
ts = sys.argv[1]
try:
    d = json.load(open('/tmp/dedup-hist.json'))
    print(len([x for x in d.get('alerts', []) if 'DedupAlert-'+ts == x.get('alert_name','') and x.get('status') == 'resolved' and x.get('ended_at')]))
except Exception:
    print(0)
PY
)
  [ "${RES:-0}" -ge 2 ] && break
done
[ "${RES:-0}" -ge 2 ] && ok "目标移除后两条历史各自 resolved (n=$RES)" || bad "双实例 resolved" "resolved=$RES hist=$(head -c 400 /tmp/dedup-hist.json)"

governed_action "$API/components/$COMP_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" >/dev/null
echo ""
echo "Dedup test: $PASS passed, $FAIL failed"
echo ""
[ $FAIL -eq 0 ]
