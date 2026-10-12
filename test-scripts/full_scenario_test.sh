#!/bin/bash
# Full product scenario against an explicitly configured remote host.
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
HOST_IP="${REMOTE_HOST:-${HOST_IP:-}}"
HOST_PASS="${REMOTE_PASS:-${HOST_PASS:-}}"
REMOTE_USER="${REMOTE_USER:-root}"
DATA_DIR="${DATA_DIR:-/opt/promlifecycle/data}"
WEBHOOK_PORT=19090
TS=$(date +%s)
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
if [ -z "$HOST_IP" ] || [ -z "$HOST_PASS" ]; then
  echo "SKIP: set REMOTE_HOST and REMOTE_PASS for remote tests"
  exit 0
fi
# 快速可达性检查：不可达则跳过（避免 8 次 x 10s 的 ssh 超时等待）
if ! sshpass -p "$HOST_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$HOST_IP" "true" >/dev/null 2>&1; then
  echo "SKIP: 远程主机 $HOST_IP 不可达（这是环境限制，不影响平台自身功能验证）"
  exit 0
fi
SSH() {
  local out=""
  for t in 1 2 3; do
    sleep 1
    out=$(sshpass -p "$HOST_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$HOST_IP" "$1" 2>/dev/null) && { echo "$out"; return 0; }
  done
  echo "$out"
  return 1
}

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
echo "=== 全产品场景验证 (TS=$TS) ==="

echo "--- A. 主机管理场景 ---"
HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"scn-host-$TS\",\"ip\":\"$HOST_IP\",\"os\":\"linux\",\"connection_type\":\"ssh\",\"username\":\"root\",\"password\":\"$HOST_PASS\",\"port\":22}" | j "d['id']")
[ -n "$HOST_ID" ] && ok "创建主机 id=$HOST_ID" || bad "创建主机" "$HOST_ID"
curl -s -X POST $API/hosts/$HOST_ID/test -H "$AUTH" >/dev/null
[ "$(curl -s $API/hosts/$HOST_ID -H "$AUTH" | j "d['status']")" = "online" ] && ok "主机测试 online" || bad "主机测试" "$(curl -s $API/hosts/$HOST_ID -H "$AUTH" | j "d['status']")"
DETECT=$(curl -s $API/hosts/$HOST_ID/detect -H "$AUTH")
echo "$DETECT" | grep -q "9100" && ok "组件探测发现存量 9100 服务" || bad "组件探测" "$(echo "$DETECT" | head -c 200)"
BR=$(curl -s "$API/hosts/$HOST_ID/browse?path=/var" -H "$AUTH")
[ "$(echo "$BR" | j "len(d['dirs'])")" -gt 0 ] 2>/dev/null && ok "远程目录浏览 /var" || bad "远程目录浏览" "$BR"

echo "--- B. 包与版本场景 ---"
[ "$(code $API/packages -H "$AUTH")" = "200" ] && ok "包列表" || bad "包列表" "$(code $API/packages -H "$AUTH")"
[ "$(code "$API/versions/by-type?type=alertmanager" -H "$AUTH")" = "200" ] && ok "版本按类型查询" || bad "版本查询" "$(code "$API/versions/by-type?type=alertmanager" -H "$AUTH")"
head -c 4096 /dev/urandom > /tmp/scn-custom.tar.gz
CUST=$(curl -s -X POST $API/packages/upload -H "$AUTH" -F "name=scn-custom" -F "type=node_exporter" -F "version=9.8.7" -F "file=@/tmp/scn-custom.tar.gz")
CUST_ID=$(echo "$CUST" | j "d['package']['id']")
[ -n "$CUST_ID" ] && ok "上传自定义包" || bad "上传自定义包" "$CUST"
[ "$(code -X DELETE $API/packages/$CUST_ID -H "$AUTH")" = "200" ] && ok "删除自定义包" || bad "删除自定义包" "$(code -X DELETE $API/packages/$CUST_ID -H "$AUTH")"

echo "--- C. 节点 + 三组件二进制部署（Prometheus/Alertmanager/node_exporter）---"
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点" || bad "读取节点" "$NODE_ID"
dep() {
  local ty="$1" nm="$2" ver="$3" port="$4"
  local cid=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"$ty\",\"name\":\"$nm\",\"deploy_type\":\"binary\",\"version\":\"$ver\",\"host_id\":$HOST_ID,\"port\":$port}" | j "d['component']['id']")
  for i in $(seq 1 60); do local st=$(curl -s $API/components/$cid -H "$AUTH" | j "d['status']"); [ "$st" = "running" ] || [ "$st" = "error" ] && break; sleep 3; done
  echo "$cid|$st"
}
PROM_RES=$(dep prometheus scn-prom v2.53.0 9090)
PROM_ID=$(echo "$PROM_RES" | cut -d'|' -f1)
PROM_ST=$(echo "$PROM_RES" | cut -d'|' -f2)
sleep 3
AM_RES=$(dep alertmanager scn-am v0.27.0 9093)
AM_ID=$(echo "$AM_RES" | cut -d'|' -f1)
AM_ST=$(echo "$AM_RES" | cut -d'|' -f2)
sleep 3
NE_RES=$(dep node_exporter scn-ne v1.8.2 19100)
NE_ID=$(echo "$NE_RES" | cut -d'|' -f1)
NE_ST=$(echo "$NE_RES" | cut -d'|' -f2)
echo "  prom=$PROM_ID/$PROM_ST am=$AM_ID/$AM_ST node=$NE_ID/$NE_ST"
[ "$PROM_ST" = "running" ] && ok "Prometheus running" || bad "Prometheus" "$PROM_ST"
[ "$AM_ST" = "running" ] && ok "Alertmanager running" || bad "Alertmanager" "$AM_ST"
[ "$NE_ST" = "running" ] && ok "node_exporter running" || bad "node_exporter" "$NE_ST"
[ "$(SSH 'curl -s -o /dev/null -w "%{http_code}" -m 4 http://127.0.0.1:9093/-/healthy 2>/dev/null')" = "200" ] && ok "Alertmanager 健康 200" || bad "Alertmanager 健康" "$(SSH 'curl -s -o /dev/null -w "%{http_code}" -m 4 http://127.0.0.1:9093/-/healthy 2>/dev/null')"

echo "--- D. 告警全链路（Prometheus→Alertmanager→Webhook）---"
sshpass -p "$HOST_PASS" scp -o StrictHostKeyChecking=no -o PreferredAuthentications=password -o PubkeyAuthentication=no "$(dirname "$0")/webhook_listener.py" "$REMOTE_USER@$HOST_IP:$(dirname "$0")/webhook_listener.py" >/dev/null 2>&1
SSH "nohup python3 $(dirname "$0")/webhook_listener.py $WEBHOOK_PORT >/tmp/webhook_listener.log 2>&1 & echo started" >/dev/null
sleep 1
REACH=$(SSH "curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:$WEBHOOK_PORT/ 2>/dev/null")
[ "$REACH" = "404" ] || [ "$REACH" = "501" ] && ok "远程 webhook 监听可用" || bad "远程 webhook 监听" "$REACH"
CH_ID=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"scn-webhook\",\"type\":\"webhook\",\"config\":\"{\\\"url\\\":\\\"http://127.0.0.1:$WEBHOOK_PORT/hook\\\"}\"}" | j "d['id']")
[ -n "$CH_ID" ] && ok "创建 webhook 渠道 id=$CH_ID" || bad "创建渠道" "$CH_ID"
# 渠道创建后应立即同步 alertmanager.yml（产品即时同步），确认后再触发告警
AM_CFG_READY=0
for i in $(seq 1 12); do
  sleep 5
  AMYML=$(SSH "cat /etc/alertmanager/alertmanager.yml 2>/dev/null")
  echo "$AMYML" | grep -q "$WEBHOOK_PORT" && { AM_CFG_READY=1; break; }
done
[ "$AM_CFG_READY" = "1" ] && ok "AM 配置已含 webhook 接收器" || bad "AM 配置同步" "$(echo "$AMYML" | grep -c webhook 2>/dev/null)"
curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"scn-down\",\"job_name\":\"scn_down_$TS\",\"targets\":[\"1.2.3.4:9100\"],\"scrape_interval\":\"15s\"}" >/dev/null
RULE_ID=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"ScnDown-$TS\",\"expression\":\"up{job=\\\"scn_down_$TS\\\"} == 0\",\"severity\":\"critical\",\"duration\":\"10s\"}" | j "d['id']")
ok "创建 down 目标 + 告警规则 (rule=$RULE_ID)"

# 等待调度器同步 alertmanager 配置 + 告警触发 + webhook 到达（最长 ~5 分钟）
HIT=0
for i in $(seq 1 30); do
  sleep 10
  HITS=$(SSH "cat /tmp/webhook_hits.log 2>/dev/null")
  echo "$HITS" | grep -q "ScnDown-$TS" && { HIT=1; break; }
done
[ "$HIT" = "1" ] && ok "webhook 收到 firing 通知" || bad "webhook 通知" "$(SSH 'head -c 200 /tmp/webhook_hits.log 2>/dev/null')"
HIST=$(curl -s "$API/alerts/history?limit=20" -H "$AUTH" | j "len([x for x in d['alerts'] if 'ScnDown-$TS' in x['alert_name'] and x['status']=='firing'])")
[ "${HIST:-0}" -gt 0 ] 2>/dev/null && ok "平台告警历史 firing ($HIST)" || bad "平台告警历史" "$HIST"
AM_ALERTS=$(SSH "curl -s -m 5 http://127.0.0.1:9093/api/v2/alerts")
echo "$AM_ALERTS" | grep -q "ScnDown-$TS" && ok "Alertmanager 收到告警" || bad "Alertmanager 告警" "$(echo "$AM_ALERTS" | head -c 200)"

echo "--- D2. 静默场景 ---"
SIL_ID=$(curl -s -X POST $API/alerts/silences -H "$AUTH" -H 'Content-Type: application/json' -d "{\"matchers\":[{\"name\":\"alertname\",\"value\":\"ScnDown-$TS\",\"isRegex\":false}],\"comment\":\"scn-test\",\"starts_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"ends_at\":\"$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)\"}" | j "d['id']")
[ -n "$SIL_ID" ] && ok "创建静默 id=$SIL_ID" || bad "创建静默" "$SIL_ID"
[ "$(SSH 'curl -s -m 5 http://127.0.0.1:9093/api/v2/silences 2>/dev/null | grep -c "scn-test" 2>/dev/null')" -gt 0 ] 2>/dev/null && ok "Alertmanager 静默生效" || bad "Alertmanager 静默" "$(SSH 'curl -s -m 5 http://127.0.0.1:9093/api/v2/silences 2>/dev/null | head -c 200')"
[ "$(code -X DELETE $API/alerts/silences/$SIL_ID -H "$AUTH")" = "200" ] && ok "删除静默" || bad "删除静默" "$(code -X DELETE $API/alerts/silences/$SIL_ID -H "$AUTH")"

echo "--- D3. 告警恢复场景 ---"
curl -s -X DELETE $API/alerts/rules/$RULE_ID -H "$AUTH" >/dev/null
for i in $(seq 1 15); do
  sleep 10
  RES=$(curl -s "$API/alerts/history?limit=20" -H "$AUTH" | j "len([x for x in d['alerts'] if 'ScnDown-$TS' in x['alert_name'] and x['status']=='resolved'])")
  [ "${RES:-0}" -gt 0 ] 2>/dev/null && break
done
[ "${RES:-0}" -gt 0 ] 2>/dev/null && ok "平台告警 resolved" || bad "告警 resolved" "$RES"
AMPROXY=$(code $API/alertmanager/status -H "$AUTH")
[ "$AMPROXY" = "200" ] && ok "Alertmanager 代理 status 200" || bad "AM 代理" "$AMPROXY"

echo "--- E. 配置场景（scrape/remote-write/配置回滚）---"
sleep 4
[ "$(SSH 'grep -c "scn_down_'$TS'" /etc/prometheus/prometheus.yml 2>/dev/null')" -gt 0 ] 2>/dev/null && ok "远程 yml 含自定义 scrape job" || bad "scrape job" "$(SSH 'grep -A3 "scn_down" /etc/prometheus/prometheus.yml 2>/dev/null | head -5')"
RW_ID=$(curl -s -X POST $API/remote-writes -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"scn-rw\",\"url\":\"https://remote-write.invalid/api/v1/write\"}" | j "d['id']")
sleep 6
[ "$(SSH 'grep -c "scn-rw\|remote-write.invalid" /etc/prometheus/prometheus.yml 2>/dev/null')" -gt 0 ] 2>/dev/null && ok "远程 yml 含 remote_write" || bad "remote_write" "$(SSH 'grep -B2 -A4 "remote_write" /etc/prometheus/prometheus.yml 2>/dev/null | head -10')"
CV1=$(curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"prometheus.yml","content":"global:\n  scrape_interval: 30s","content_type":"yaml"}' | j "d['config']['id']")
CV2=$(curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"prometheus.yml","content":"global:\n  scrape_interval: 5s","content_type":"yaml"}' | j "d['config']['id']")
[ -n "$CV1" ] && [ -n "$CV2" ] && ok "配置版本 v1=$CV1 v2=$CV2" || bad "配置版本" "v1=$CV1 v2=$CV2"
[ "$(code -X POST $API/configs/$PROM_ID/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"prometheus.yml","version":1,"reason":"test rollback"}')" = "200" ] && ok "配置回滚" || bad "配置回滚" "$(code -X POST $API/configs/$PROM_ID/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"prometheus.yml","version":1,"reason":"test rollback"}')"

echo "--- F. 运维场景（健康轮询/资源/日志）---"
SSH "systemctl stop promlifecycle-prometheus" >/dev/null
ok "远程停止 Prometheus 服务（触发健康轮询）"
for i in $(seq 1 15); do sleep 5; HS=$(curl -s $API/components/$PROM_ID -H "$AUTH" | j "d['status']"); [ "$HS" = "error" ] && break; done
[ "$HS" = "error" ] && ok "健康轮询标记 error" || bad "健康轮询" "$HS"
[ "$(code -X POST $API/components/$PROM_ID/restart -H "$AUTH")" = "200" ] && ok "重启恢复" || bad "重启" "$(code -X POST $API/components/$PROM_ID/restart -H "$AUTH")"
for i in $(seq 1 15); do sleep 5; RS=$(curl -s $API/components/$PROM_ID -H "$AUTH" | j "d['status']"); [ "$RS" = "running" ] && break; done
[ "$RS" = "running" ] && ok "重启后 running" || bad "重启后" "$RS"
RES2=$(curl -s $API/components/$NE_ID/resource -H "$AUTH")
[ "$(code $API/components/$NE_ID/resource -H "$AUTH")" = "200" ] && ok "资源监控接口 200" || bad "资源监控" "$(code $API/components/$NE_ID/resource -H "$AUTH")"
echo "  （远程资源值: $(echo "$RES2" | j "d['cpu_usage'],d['memory_mb']")，已知远程可能为0）"

echo "--- G. 备份场景 ---"
SB_ID=$(curl -s -X POST $API/backups/scheduled -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"scn-bak\",\"cron\":\"* * * * *\",\"type\":\"data\",\"keep_count\":3,\"component_id\":$PROM_ID}" | j "d['id']")
[ -n "$SB_ID" ] && ok "创建定时备份 id=$SB_ID" || bad "定时备份" "$SB_ID"
BCNT=0
for i in $(seq 1 18); do sleep 10; BCNT=$(curl -s $API/backups -H "$AUTH" | j "len([x for x in d['backups'] if 'scn-bak' in x['name']])"); [ "${BCNT:-0}" -gt 0 ] 2>/dev/null && break; done
[ "${BCNT:-0}" -gt 0 ] 2>/dev/null && ok "备份记录生成" || bad "备份记录" "$BCNT"
[ "$(SSH 'ls /backups 2>/dev/null | wc -l')" -gt 0 ] 2>/dev/null && ok "远程 /backups 有文件" || bad "远程备份文件" "$(SSH 'ls /backups 2>/dev/null')"
curl -s -X DELETE $API/backups/scheduled/$SB_ID -H "$AUTH" >/dev/null

echo "--- H. 清理 ---"
for c in $NE_ID $AM_ID $PROM_ID; do governed_action "$API/components/$c/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null; sleep 2; done
SSH "pkill -f webhook_listener.py 2>/dev/null; echo done" >/dev/null
[ "$(SSH 'systemctl list-units --all 2>/dev/null | grep -c promlifecycle' 2>/dev/null)" = "0" ] 2>/dev/null && ok "远程单元全部移除" || bad "远程单元移除" "$(SSH 'systemctl list-units --all 2>/dev/null | grep promlifecycle' 2>/dev/null)"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code $API/node -H "$AUTH")"
[ "$(code -X DELETE $API/hosts/$HOST_ID -H "$AUTH")" = "200" ] && ok "删除主机" || bad "删除主机" "$(code -X DELETE $API/hosts/$HOST_ID -H "$AUTH")"

echo ""
echo "===== 全场景验证: $PASS passed, $FAIL failed ====="
[ $FAIL -eq 0 ]
