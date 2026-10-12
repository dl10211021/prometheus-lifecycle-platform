#!/bin/bash
# Full CRUD test against an explicitly configured SSH host.
set -u
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
HOST_IP="${REMOTE_HOST:-${HOST_IP:-}}"
HOST_PASS="${REMOTE_PASS:-${HOST_PASS:-}}"
REMOTE_USER="${REMOTE_USER:-root}"
DATA_DIR="${DATA_DIR:-/opt/promlifecycle/data}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-/opt/promlifecycle/admin_initial_password}"
ADMIN_PASSWORD="${PROMLIFECYCLE_ADMIN_PASSWORD:-${ADMIN_PASSWORD:-}}"
if [ -z "$ADMIN_PASSWORD" ] && [ -r "$ADMIN_PASSWORD_FILE" ]; then
  ADMIN_PASSWORD="$(cat "$ADMIN_PASSWORD_FILE")"
fi
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
TS=$(date +%s)
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
if [ -z "$HOST_IP" ] || [ -z "$HOST_PASS" ]; then
  echo "SKIP: set REMOTE_HOST and REMOTE_PASS for remote tests"
  exit 0
fi
if [ -z "$ADMIN_PASSWORD" ]; then
  echo "SKIP: set ADMIN_PASSWORD or a readable ADMIN_PASSWORD_FILE"
  exit 0
fi
# 快速可达性检查：不可达则跳过（避免 8 次 x 10s 的 ssh 超时等待）
if ! SSHPASS="$HOST_PASS" sshpass -e ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$HOST_IP" "true" >/dev/null 2>&1; then
  echo "SKIP: 远程主机 $HOST_IP 不可达（这是环境限制，不影响平台自身功能验证）"
  exit 0
fi
SSH() {
  local out rc=1 delay
  for delay in 0 2 4 8 12 20 30 45; do
    [ "$delay" -eq 0 ] || sleep "$delay"
    out=$(SSHPASS="$HOST_PASS" timeout 25 sshpass -e ssh -o StrictHostKeyChecking=no -o ConnectTimeout=8 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$HOST_IP" "$1" 2>/dev/null)
    rc=$?
    [ $rc -eq 0 ] && break
  done
  printf '%s' "$out"
}

LOGIN_PAYLOAD=$(ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["ADMIN_PASSWORD"]}))')
TOKEN=$(curl -s -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "$LOGIN_PAYLOAD" | j "d['token']")
[ -n "$TOKEN" ] || { echo "ERROR: platform administrator authentication failed" >&2; exit 1; }
AUTH="Authorization: Bearer $TOKEN"
echo "=== 远程主机完整生命周期测试 (TS=$TS) ==="

echo "--- 1. Host CRUD ---"
HOST_PAYLOAD=$(TEST_HOST_NAME="remote-$TS" REMOTE_HOST="$HOST_IP" REMOTE_USER="$REMOTE_USER" REMOTE_PASS="$HOST_PASS" python3 "$(dirname "$0")/remote-host-payload.py")
HOST_ID=$(curl -s -X POST "$API/hosts" -H "$AUTH" -H 'Content-Type: application/json' -d "$HOST_PAYLOAD" | j "d['id']")
[[ "$HOST_ID" =~ ^[1-9][0-9]*$ ]] || { echo "ERROR: remote host registration returned no usable ID" >&2; exit 1; }
ok "创建远程主机 id=$HOST_ID"
curl -s -X POST $API/hosts/$HOST_ID/test -H "$AUTH" >/dev/null
ST=$(curl -s $API/hosts/$HOST_ID -H "$AUTH" | j "d['status']")
[ "$ST" = "online" ] && ok "远程主机测试 online" || bad "远程主机测试" "$ST"
curl -s -X PUT $API/hosts/$HOST_ID -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"remote-renamed-$TS\"}" >/dev/null
[ "$(curl -s $API/hosts/$HOST_ID -H "$AUTH" | j "d['name']")" = "remote-renamed-$TS" ] && ok "更新主机" || bad "更新主机" "$(curl -s $API/hosts/$HOST_ID -H "$AUTH" | j "d['name']")"
[ "$(code $API/hosts -H "$AUTH")" = "200" ] && ok "主机列表" || bad "主机列表" "$(code $API/hosts -H "$AUTH")"
BR=$(curl -s "$API/hosts/$HOST_ID/browse?path=/tmp" -H "$AUTH")
[ "$(echo "$BR" | j "len(d['dirs'])")" -gt 0 ] 2>/dev/null && ok "远程目录浏览 (/tmp)" || bad "远程目录浏览" "$BR"

echo "--- 2. 单节点 API ---"
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点 id=$NODE_ID" || bad "读取节点" "$NODE_ID"
curl -s -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"remote lifecycle node"}' >/dev/null
[ "$(curl -s $API/node -H "$AUTH" | j "d['description']")" = "remote lifecycle node" ] && ok "更新节点" || bad "更新节点" "$(curl -s $API/node -H "$AUTH" | j "d['description']")"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "节点详情" || bad "节点详情" "$(code $API/node -H "$AUTH")"
echo "--- 2a. observe 接管与无损解除 ---"
FIXTURE_LOCAL=$(mktemp -d)
FIXTURE_REMOTE="/tmp/promlifecycle-observe-$TS"
bash "$(dirname "$0")/build-node-fixture.sh" "$FIXTURE_LOCAL/node_exporter" || exit 1
SSH "mkdir -p '$FIXTURE_REMOTE'" >/dev/null
SSHPASS="$HOST_PASS" sshpass -e scp -o StrictHostKeyChecking=yes "$FIXTURE_LOCAL/node_exporter" "$REMOTE_USER@$HOST_IP:$FIXTURE_REMOTE/node_exporter" || exit 1
rm -rf "$FIXTURE_LOCAL"
OBS_PID=$(SSH "nohup '$FIXTURE_REMOTE/node_exporter' --web.listen-address=127.0.0.1:19105 >/tmp/observe-$TS.log 2>&1 & echo \$!")
sleep 2
OBS=$(curl -s -X POST "$API/hosts/$HOST_ID/adopt" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"port\":19105,\"name\":\"observed-$TS\",\"ownership\":\"observe\"}")
OBS_ID=$(echo "$OBS" | j "d.get('component',{}).get('id','')")
[ -n "$OBS_ID" ] && ok "observe 接管远程进程" || bad "observe 接管" "$OBS"
[ "$(code -X POST "$API/components/$OBS_ID/upgrade" -H "$AUTH" -H 'Content-Type: application/json' -d '{"new_version":"v1.8.2","reason":"must remain read-only"}')" = "403" ] && ok "observe 禁止变更" || bad "observe 变更隔离" "$(code -X POST "$API/components/$OBS_ID/upgrade" -H "$AUTH" -H 'Content-Type: application/json' -d '{"new_version":"v1.8.2","reason":"must remain read-only"}')"
[ "$(code -X DELETE "$API/components/$OBS_ID/adoption" -H "$AUTH")" = "200" ] && ok "无损解除 observe 纳管" || bad "解除 observe 纳管" "$(code -X DELETE "$API/components/$OBS_ID/adoption" -H "$AUTH")"
[ "$(SSH "kill -0 $OBS_PID 2>/dev/null; echo \$?")" = "0" ] && ok "解除纳管不停止远程进程" || bad "解除纳管保留进程" "$OBS_PID"
SSH "kill $OBS_PID >/dev/null 2>&1 || true" >/dev/null

echo "--- 2b. 无 systemd 的远程进程拒绝托管 ---"
MANAGED_PID=$(SSH "nohup '$FIXTURE_REMOTE/node_exporter' --web.listen-address=127.0.0.1:19106 >/tmp/managed-$TS.log 2>&1 & echo \$!")
sleep 2
[ "$(code -X POST "$API/hosts/$HOST_ID/adopt" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"port\":19106,\"name\":\"managed-$TS\",\"ownership\":\"managed\"}")" = "422" ] && ok "无 systemd 的远程进程拒绝托管并提示 observe" || bad "远程非 systemd 进程边界" "$(code -X POST "$API/hosts/$HOST_ID/adopt" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"port\":19106,\"name\":\"managed-$TS\",\"ownership\":\"managed\"}")"
[ "$(SSH "kill -0 $MANAGED_PID 2>/dev/null; echo \$?")" = "0" ] && ok "拒绝 managed 不影响进程" || bad "managed 拒绝保留进程" "$MANAGED_PID"
SSH "kill $MANAGED_PID >/dev/null 2>&1 || true" >/dev/null
SSH "rm -rf -- '$FIXTURE_REMOTE'" >/dev/null
# The preceding host/adoption checks intentionally create several short SSH
# sessions. Let hosts with a strict sshd MaxStartups/rate limit drain before
# the first package upload.
sleep 20

echo "--- 3. 远程二进制部署 node_exporter (端口 19100) ---"
N1=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"rm-node\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19100}" | j "d['component']['id']")
for i in $(seq 1 60); do S=$(curl -s $API/components/$N1 -H "$AUTH" | j "d['status']"); [ "$S" = "running" ] || [ "$S" = "error" ] && break; sleep 3; done
[ "$S" = "running" ] && ok "远程部署 node_exporter running" || bad "远程部署 node_exporter" "$S"
REMOTE_ACTIVE=$(SSH "systemctl is-active promlifecycle-node_exporter 2>&1")
[ "$REMOTE_ACTIVE" = "active" ] && ok "远程 systemd 单元 active" || bad "远程 systemd 单元" "$REMOTE_ACTIVE"
REMOTE_ENABLED=$(SSH "systemctl is-enabled promlifecycle-node_exporter 2>&1")
[ "$REMOTE_ENABLED" = "enabled" ] && ok "远程 systemd 单元 enabled 自启动" || bad "远程 systemd 自启动" "$REMOTE_ENABLED"
STOP_CODE=$(code -X POST $API/components/$N1/stop -H "$AUTH")
[ "$STOP_CODE" = "200" ] && [ "$(SSH 'systemctl is-active promlifecycle-node_exporter 2>&1')" != "active" ] && [ "$(SSH 'systemctl is-enabled promlifecycle-node_exporter 2>&1')" = "enabled" ] && ok "远程 stop 保留自启动" || bad "远程 stop" "$STOP_CODE"
START_CODE=$(code -X POST $API/components/$N1/start -H "$AUTH")
[ "$START_CODE" = "200" ] && [ "$(SSH 'systemctl is-active promlifecycle-node_exporter 2>&1')" = "active" ] && [ "$(SSH 'systemctl is-enabled promlifecycle-node_exporter 2>&1')" = "enabled" ] && ok "远程 start 恢复运行" || bad "远程 start" "$START_CODE"
MET=$(SSH "curl -s -m 4 http://127.0.0.1:19100/metrics 2>/dev/null | grep -c '^node_'")
[ "${MET:-0}" -gt 0 ] 2>/dev/null && ok "远程 19100 指标可达 ($MET 行)" || bad "远程指标" "$MET"
HLT=$(curl -s $API/components/$N1/health -H "$AUTH" | j "d['status']")
[ "$HLT" = "healthy" ] && ok "远程健康检查 healthy" || bad "远程健康检查" "$HLT"
[ "$(code -X POST $API/components/$N1/restart -H "$AUTH")" = "200" ] && ok "远程 restart" || bad "远程 restart" "$(code -X POST $API/components/$N1/restart -H "$AUTH")"
sleep 3
[ "$(SSH 'systemctl is-active promlifecycle-node_exporter 2>&1')" = "active" ] && ok "重启后远程 active" || bad "重启后远程" "$(SSH 'systemctl is-active promlifecycle-node_exporter 2>&1')"
UP_CODE=$(governed_code "$API/components/$N1/upgrade" '{"new_version":"v1.7.0","reason":"remote lifecycle verification"}' "$AUTH")
[ "$UP_CODE" = "200" ] && ok "远程 upgrade v1.7.0" || bad "远程 upgrade" "$UP_CODE"
sleep 8
V=$(SSH "curl -s -m 4 http://127.0.0.1:19100/metrics 2>/dev/null | grep '^node_exporter_build_info' | grep -o 'version=\"1\.7\.0\"'")
[ -n "$V" ] && ok "远程升级后版本 1.7.0" || bad "远程升级版本" "$V"
ROLLBACK_CODE=$(governed_code "$API/components/$N1/rollback" '{"target_version":"v1.8.2","reason":"remote rollback verification"}' "$AUTH")
[ "$ROLLBACK_CODE" = "200" ] && ok "远程 rollback v1.8.2" || bad "远程 rollback" "$ROLLBACK_CODE"
sleep 8
V2=$(SSH "curl -s -m 4 http://127.0.0.1:19100/metrics 2>/dev/null | grep '^node_exporter_build_info' | grep -o 'version=\"1\.8\.2\"'")
[ -n "$V2" ] && ok "远程回滚后版本 1.8.2" || bad "远程回滚版本" "$V2"
LOGS=$(curl -s $API/components/$N1/logs -H "$AUTH" | j "len(d['logs'])")
[ "${LOGS:-0}" -gt 0 ] 2>/dev/null && ok "远程日志 ($LOGS 行)" || bad "远程日志" "$LOGS"
DET=$(curl -s $API/components/$N1 -H "$AUTH")
[ "$(echo "$DET" | j "d['install_path']")" = "/usr/local/promlifecycle" ] && ok "远程 install_path 持久化" || bad "远程 install_path" "$(echo "$DET" | j "d['install_path']")"
UNINSTALL_NODE=$(governed_code "$API/components/$N1/uninstall" '{"mode":"keep_data","reason":"remote cleanup"}' "$AUTH")
[ "$UNINSTALL_NODE" = "200" ] && ok "远程卸载 node_exporter" || bad "远程卸载" "$UNINSTALL_NODE"
sleep 3
[ "$(SSH 'systemctl is-active promlifecycle-node_exporter 2>&1')" != "active" ] && ok "远程单元已移除" || bad "远程单元移除" "$(SSH 'systemctl is-active promlifecycle-node_exporter 2>&1')"
[ "$(SSH 'systemctl is-enabled promlifecycle-node_exporter 2>&1')" != "enabled" ] && ok "远程 node_exporter 自启动已禁用" || bad "远程 node_exporter 自启动残留" "$(SSH 'systemctl is-enabled promlifecycle-node_exporter 2>&1')"

echo "--- 4. 远程二进制部署 Prometheus (9090) + 配置/规则同步 ---"
P1=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"name\":\"rm-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HOST_ID,\"port\":9090}" | j "d['component']['id']")
for i in $(seq 1 60); do S=$(curl -s $API/components/$P1 -H "$AUTH" | j "d['status']"); [ "$S" = "running" ] || [ "$S" = "error" ] && break; sleep 3; done
[ "$S" = "running" ] && ok "远程部署 Prometheus running" || bad "远程部署 Prometheus" "$S"
sleep 6
[ "$(SSH 'curl -s -o /dev/null -w "%{http_code}" -m 4 http://127.0.0.1:9090/-/healthy 2>/dev/null')" = "200" ] && ok "远程 Prometheus 健康 200" || bad "远程 Prometheus 健康" "$(SSH 'curl -s -o /dev/null -w "%{http_code}" -m 4 http://127.0.0.1:9090/-/healthy 2>/dev/null')"
RULE_ID=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"RemoteDown-$TS\",\"expression\":\"up == 0\",\"severity\":\"warning\",\"duration\":\"1m\"}" | j "d['id']")
sleep 6
[ "$(SSH 'grep -c "RemoteDown-'$TS'" /etc/prometheus/rules/rules.yml 2>/dev/null')" -gt 0 ] 2>/dev/null && ok "远程 rules.yml 含规则" || bad "远程规则文件" "$(SSH 'cat /etc/prometheus/rules/rules.yml 2>/dev/null | head -3')"
[ "$(SSH 'grep -c "prometheus_self" /etc/prometheus/prometheus.yml 2>/dev/null')" -gt 0 ] 2>/dev/null && ok "远程 prometheus.yml 为平台生成配置" || bad "远程 yml" "$(SSH 'head -15 /etc/prometheus/prometheus.yml 2>/dev/null')"
# This host rate-limits repeated password authentications for roughly one
# minute. Drain that window before the governed config sequence so the test
# measures config behavior rather than sshd throttling.
sleep 60
V1=$(curl -s -X POST $API/components/$P1/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"prometheus.yml","content":"global:\n  scrape_interval: 30s","content_type":"yaml","reason":"remote config baseline"}' | j "d['config']['version']")
V2=$(curl -s -X POST $API/components/$P1/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"prometheus.yml","content":"global:\n  scrape_interval: 5s","content_type":"yaml","reason":"remote config change"}' | j "d['config']['version']")
[ -n "$V1" ] && [ -n "$V2" ] && ok "远程配置 v1=$V1 v2=$V2" || bad "远程配置创建" "v1=$V1 v2=$V2"
ROLLBACK_CONFIG_CODE=$(code -X POST $API/configs/$P1/rollback -H "$AUTH" -H 'Content-Type: application/json' -d "{\"config_name\":\"prometheus.yml\",\"version\":$V1,\"reason\":\"test rollback\"}")
[ "$ROLLBACK_CONFIG_CODE" = "200" ] && ok "远程配置回滚 200" || bad "远程配置回滚" "$ROLLBACK_CONFIG_CODE"
sleep 5
[ "$(SSH 'grep -c "scrape_interval: 30s" /etc/prometheus/prometheus.yml 2>/dev/null')" -gt 0 ] 2>/dev/null && ok "远程回滚后 yml 为 v1 内容" || bad "远程回滚内容" "$(SSH 'grep "scrape_interval" /etc/prometheus/prometheus.yml 2>/dev/null')"
PLOG=0
for t in 1 2 3 4; do
  PLOG=$(curl -s $API/components/$P1/logs -H "$AUTH" | j "len(d['logs'])")
  [ "${PLOG:-0}" -gt 0 ] 2>/dev/null && break
  sleep 3
done
[ "${PLOG:-0}" -gt 0 ] 2>/dev/null && ok "远程 Prometheus 日志 ($PLOG 行)" || bad "远程 Prometheus 日志" "$PLOG"
UNINSTALL_PROM=$(governed_code "$API/components/$P1/uninstall" '{"mode":"keep_data","reason":"remote cleanup"}' "$AUTH")
[ "$UNINSTALL_PROM" = "200" ] && ok "远程卸载 Prometheus" || bad "远程卸载 Prometheus" "$UNINSTALL_PROM"
sleep 3
[ "$(SSH 'systemctl is-active promlifecycle-prometheus 2>&1')" != "active" ] && ok "远程 Prometheus 单元已移除" || bad "远程 Prometheus 单元移除" "$(SSH 'systemctl is-active promlifecycle-prometheus 2>&1')"
[ "$(SSH 'systemctl is-enabled promlifecycle-prometheus 2>&1')" != "enabled" ] && ok "远程 Prometheus 自启动已禁用" || bad "远程 Prometheus 自启动残留" "$(SSH 'systemctl is-enabled promlifecycle-prometheus 2>&1')"

echo "--- 5. 清理组件记录与主机 ---"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code $API/node -H "$AUTH")"
[ "$(code -X DELETE $API/hosts/$HOST_ID -H "$AUTH")" = "200" ] && ok "删除主机" || bad "删除主机" "$(code -X DELETE $API/hosts/$HOST_ID -H "$AUTH")"

echo ""
echo "===== Remote CRUD Result: $PASS passed, $FAIL failed ====="
[ $FAIL -eq 0 ]
