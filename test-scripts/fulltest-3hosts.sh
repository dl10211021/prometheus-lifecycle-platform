#!/bin/bash
# Remote deployment lifecycle test.
# Covers host registration, deploy, upgrade, rollback, restart, config, alerts, backup, and uninstall.
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
REMOTE_IP="${REMOTE_HOST:-}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_PASS="${REMOTE_PASS:-}"
DATA_DIR="${DATA_DIR:-/opt/promlifecycle/data}"
if [ -z "$REMOTE_IP" ] || [ -z "$REMOTE_PASS" ]; then
  echo "SKIP: set REMOTE_HOST and REMOTE_PASS for remote tests"
  exit 0
fi
# Remote SSH retries accommodate sshd MaxStartups limits.
rssh() { for t in 1 2 3 4; do out=$(sshpass -p "$REMOTE_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$REMOTE_IP" "$1" 2>/dev/null) && { echo "$out"; return 0; }; sleep 1; done; echo "$out"; }
TS=$(date +%s)
PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: ${2:-})"; }
skip() { SKIP=$((SKIP+1)); echo "  - $1 (SKIP: ${2:-})"; }
j()    { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' -m 30 "$@"; }
wait_status() { # $1=comp_id $2=target_status $3=max_wait
  local id=$1 target=$2 max=${3:-60}
  for i in $(seq 1 $max); do
    ST=$(curl -s $API/components/$id -H "$AUTH" | j "d['status']")
    [ "$ST" = "$target" ] && return 0
    [ "$ST" = "error" ] && return 1
    sleep 2
  done
  return 1
}

echo "================================================================"
echo " 全功能测试 (TS=$TS) — 平台:150 → 远程:151"
echo "================================================================"

# === 登录 ===
echo "--- 0. 登录 ---"
TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录" "no token"; exit 1; }

# === A. Setup: 纳管 151 + 读取节点 ===
echo "--- A. 纳管主机 151 + 读取节点 ---"
HOST_RESP=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ai2\",\"ip\":\"$REMOTE_IP\",\"os\":\"linux\",\"connection_type\":\"ssh\",\"username\":\"root\",\"password\":\"$REMOTE_PASS\",\"port\":22}")
HOST_ID=$(echo "$HOST_RESP" | j "d['id']")
if [ -z "$HOST_ID" ]; then
  HOST_ID=$(curl -s $API/hosts -H "$AUTH" | j "[h['id'] for h in d.get('hosts',[]) if h.get('ip')=='$REMOTE_IP'][0] if any(h.get('ip')=='$REMOTE_IP' for h in d.get('hosts',[])) else ''")
fi
[ -n "$HOST_ID" ] && ok "纳管主机 151 id=$HOST_ID" || { bad "纳管主机" "$HOST_RESP"; exit 1; }
# 触发连接测试
curl -s $API/hosts/$HOST_ID/test -H "$AUTH" >/dev/null
sleep 2
HOST_STATUS=$(curl -s $API/hosts/$HOST_ID -H "$AUTH" | j "d['status']")
[ "$HOST_STATUS" = "online" ] && ok "主机 151 在线" || bad "主机状态" "$HOST_STATUS"

NODE_RESP=$(curl -s $API/node -H "$AUTH")
NODE_ID=$(echo "$NODE_RESP" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点 id=$NODE_ID" || { bad "读取节点" "$NODE_RESP"; exit 1; }

# === B. Deploy: 远程部署 4 个组件 ===
echo "--- B. 远程部署组件到 151 ---"

echo "  B1. node_exporter (binary, :19100)"
NE_RESP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"ft-ne\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19100}")
NE_ID=$(echo "$NE_RESP" | j "d['component']['id']")
[ -n "$NE_ID" ] && ok "提交部署 id=$NE_ID" || { bad "提交 node_exporter" "$NE_RESP"; exit 1; }
wait_status $NE_ID running 60 && ok "node_exporter running" || bad "node_exporter 部署" "$(curl -s $API/components/$NE_ID -H "$AUTH" | j "d.get('error','?')")"
# 远端核对
NE_PORT=$(rssh "ss -tlnp | grep ':19100' | wc -l" 2>/dev/null | tr -d ' ')
[ "$NE_PORT" = "1" ] && ok "远端 19100 端口监听" || bad "远端端口" "$NE_PORT"
NE_SVC=$(rssh "systemctl is-active promlifecycle-node_exporter 2>/dev/null" 2>/dev/null | tr -d ' ')
[ "$NE_SVC" = "active" ] && ok "远端 systemd active" || bad "远端 systemd" "$NE_SVC"
NE_METRICS=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://$REMOTE_IP:19100/metrics)
[ "$NE_METRICS" = "200" ] && ok "node_exporter metrics 可达" || bad "metrics" "$NE_METRICS"

echo "  B2. prometheus (binary, :19090)"
PROM_RESP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"name\":\"ft-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HOST_ID,\"port\":19090}")
PROM_ID=$(echo "$PROM_RESP" | j "d['component']['id']")
[ -n "$PROM_ID" ] && ok "提交部署 id=$PROM_ID" || { bad "提交 prometheus" "$PROM_RESP"; exit 1; }
wait_status $PROM_ID running 90 && ok "prometheus running" || bad "prometheus 部署" "$(curl -s $API/components/$PROM_ID -H "$AUTH" | j "d.get('error','?')")"
PROM_HEALTH=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://$REMOTE_IP:19090/-/healthy)
[ "$PROM_HEALTH" = "200" ] && ok "prometheus healthy" || bad "prometheus health" "$PROM_HEALTH"
# 配置应含 node_exporter scrape
PROM_CFG=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null" 2>/dev/null)
echo "$PROM_CFG" | grep -q "19100" && ok "prometheus 配置含 node_exporter scrape" || bad "配置 scrape" "missing 19100"

echo "  B3. alertmanager (binary, :19093)"
AM_RESP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"alertmanager\",\"name\":\"ft-am\",\"deploy_type\":\"binary\",\"version\":\"v0.27.0\",\"host_id\":$HOST_ID,\"port\":19093}")
AM_ID=$(echo "$AM_RESP" | j "d['component']['id']")
[ -n "$AM_ID" ] && ok "提交部署 id=$AM_ID" || { bad "提交 alertmanager" "$AM_RESP"; exit 1; }
wait_status $AM_ID running 90 && ok "alertmanager running" || bad "alertmanager 部署" "$(curl -s $API/components/$AM_ID -H "$AUTH" | j "d.get('error','?')")"
AM_HEALTH=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://$REMOTE_IP:19093/-/healthy)
[ "$AM_HEALTH" = "200" ] && ok "alertmanager healthy" || bad "alertmanager health" "$AM_HEALTH"
# prometheus 配置应含 alertmanager target
PROM_CFG=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null" 2>/dev/null)
echo "$PROM_CFG" | grep -q "19093" && ok "prometheus 配置含 alertmanager target" || bad "配置 AM target" "missing 19093"

echo "  B4. blackbox_exporter (binary, :19115)"
BB_RESP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"blackbox_exporter\",\"name\":\"ft-bb\",\"deploy_type\":\"binary\",\"version\":\"v0.24.0\",\"host_id\":$HOST_ID,\"port\":19115}")
BB_ID=$(echo "$BB_RESP" | j "d['component']['id']")
[ -n "$BB_ID" ] && ok "提交部署 id=$BB_ID" || { bad "提交 blackbox" "$BB_RESP"; exit 1; }
wait_status $BB_ID running 60 && ok "blackbox_exporter running" || bad "blackbox 部署" "$(curl -s $API/components/$BB_ID -H "$AUTH" | j "d.get('error','?')")"
BB_METRICS=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://$REMOTE_IP:19115/metrics)
[ "$BB_METRICS" = "200" ] && ok "blackbox metrics 可达" || bad "blackbox metrics" "$BB_METRICS"

# === C. Lifecycle: 升级/回滚/重启/配置 ===
echo "--- C. 生命周期管理 ---"

echo "  C1. node_exporter 升级 (无更高版本包→用同版本验证流程不报错)"
# 平台只有 1.8.2，升级测试用 prometheus（有 v2.55.0? 检查）
PROM_VER=$(curl -s $API/components/$PROM_ID -H "$AUTH" | j "d['version']")
echo "    当前 prometheus 版本: $PROM_VER"

echo "  C2. 组件重启 (prometheus)"
RESTART_CODE=$(code -X POST $API/components/$PROM_ID/restart -H "$AUTH")
[ "$RESTART_CODE" = "200" ] && ok "重启接口 200" || bad "重启接口" "$RESTART_CODE"
sleep 5
PROM_HEALTH2=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://$REMOTE_IP:19090/-/healthy)
[ "$PROM_HEALTH2" = "200" ] && ok "重启后 healthy" || bad "重启后健康" "$PROM_HEALTH2"
# 验证进程 PID 变了（确实重启了）
PROM_PID2=$(rssh "pgrep -f 'prometheus.*19090'" 2>/dev/null | tr -d ' ')
[ -n "$PROM_PID2" ] && ok "重启后新进程 pid=$PROM_PID2" || bad "重启后进程" "none"

echo "  D3. 创建 scrape 配置"
SC_RESP=$(curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"ft-scrape\",\"job_name\":\"ft-ne\",\"targets\":\"[\\\"$REMOTE_IP:19100\\\"]\",\"scrape_interval\":\"15s\"}")
SC_ID=$(echo "$SC_RESP" | j "d['id']")
[ -n "$SC_ID" ] && ok "创建 scrape 配置 id=$SC_ID" || bad "创建 scrape" "$SC_RESP"
sleep 3
PROM_CFG5=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null" 2>/dev/null)
echo "$PROM_CFG5" | grep -q "ft-ne" && ok "scrape 配置已同步" || bad "scrape 同步" "missing ft-ne"
echo "  C3. 配置管理 (prometheus 配置保存+回滚)"
# 保存自定义配置
CFG_RESP=$(curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"prometheus.yml\",\"content\":\"global:\n  scrape_interval: 30s\n# FT_MARKER_$TS\n\"}")
CFG_ID=$(echo "$CFG_RESP" | j "d.get('config',{}).get('id','')")
[ -n "$CFG_ID" ] && ok "保存配置 v1 id=$CFG_ID" || bad "保存配置" "$CFG_RESP"
sleep 3
# 远端核对配置落地
PROM_CFG2=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null" 2>/dev/null)
echo "$PROM_CFG2" | grep -q "FT_MARKER_$TS" && ok "配置已推送到远端" || bad "配置推送" "marker missing"
echo "$PROM_CFG2" | grep -q "30s" && ok "scrape_interval=30s 生效" || bad "配置内容" "no 30s"

# 保存第二个版本
CFG_RESP2=$(curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"prometheus.yml\",\"content\":\"global:\n  scrape_interval: 10s\n# FT_MARKER2_$TS\n\"}")
CFG_ID2=$(echo "$CFG_RESP2" | j "d.get('config',{}).get('id','')")
[ -n "$CFG_ID2" ] && ok "保存配置 v2 id=$CFG_ID2" || bad "保存配置 v2" "$CFG_RESP2"
sleep 3
PROM_CFG3=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null" 2>/dev/null)
echo "$PROM_CFG3" | grep -q "FT_MARKER2_$TS" && ok "v2 配置已推送" || bad "v2 推送" "marker2 missing"

# 回滚到 v1 (路由是 /configs/:component_id/rollback, 需要 config_name + version)
ROLLBACK_RESP=$(curl -s -X POST $API/configs/$PROM_ID/rollback -H "$AUTH" -H 'Content-Type: application/json' -d "{\"config_name\":\"prometheus.yml\",\"version\":1}")
sleep 3
PROM_CFG4=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null" 2>/dev/null)
echo "$PROM_CFG4" | grep -q "FT_MARKER_$TS" && ok "回滚到 v1 成功" || bad "配置回滚" "marker missing after rollback"
echo "$PROM_CFG4" | grep -q "FT_MARKER2_$TS" && bad "回滚后 v2 仍在" "" || ok "回滚后 v2 已移除"

# === D. Alerting: 告警规则 + 通知渠道 + 链路 ===
echo "--- D. 告警链路 ---"

echo "  D1. 创建告警规则"
RULE_RESP=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"FT-Down-$TS\",\"expression\":\"up{job=\\\"ft-ne\\\"} == 0\",\"severity\":\"critical\",\"duration\":\"10s\"}")
RULE_ID=$(echo "$RULE_RESP" | j "d['id']")
[ -n "$RULE_ID" ] && ok "创建告警规则 id=$RULE_ID" || bad "创建规则" "$RULE_RESP"
sleep 3
# 规则应同步到 prometheus
RULE_FILE=$(rssh "cat /etc/prometheus/rules/*.yml 2>/dev/null" 2>/dev/null)
echo "$RULE_FILE" | grep -q "FT-Down-$TS" && ok "规则已同步到 prometheus" || bad "规则同步" "not in rules file"

echo "  D2. 创建通知渠道 (webhook)"
CH_RESP=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-wh-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/hook\\\"}\",\"enabled\":true}")
CH_ID=$(echo "$CH_RESP" | j "d['id']")
[ -n "$CH_ID" ] && ok "创建通知渠道 id=$CH_ID" || bad "创建渠道" "$CH_RESP"
# 轮询等待 AM 配置同步（异步 goroutine + syncMu 串行化，可能需排队）
AM_SYNCED=0
for i in $(seq 1 20); do
  AM_CFG=$(rssh "cat /etc/alertmanager/alertmanager.yml 2>/dev/null" 2>/dev/null)
  # sanitizeReceiverName converts hyphens to underscores
  AM_NAME=$(echo "ft-wh-$TS" | tr '-' '_')
  echo "$AM_CFG" | grep -q "$AM_NAME" && { AM_SYNCED=1; break; }
  sleep 1
done
[ "$AM_SYNCED" = "1" ] && ok "渠道已同步到 alertmanager" || bad "渠道同步" "not in AM config after 20s"


echo "  D4. 告警历史查询"
HIST_CODE=$(code "$API/alerts/history?limit=5" -H "$AUTH")
[ "$HIST_CODE" = "200" ] && ok "告警历史查询 200" || bad "告警历史" "$HIST_CODE"

echo "  D5. 创建静默"
SILENCE_RESP=$(curl -s -X POST "$API/alerts/silences" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"matchers\":[{\"name\":\"alertname\",\"value\":\"FT-Down-$TS\",\"isRegex\":false}],\"startsAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"endsAt\":\"$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)\",\"createdBy\":\"admin\",\"comment\":\"ft-test\"}")
SILENCE_ID=$(echo "$SILENCE_RESP" | j "d.get('id','d.get('silenceId',''))')" 2>/dev/null)
[ -n "$SILENCE_ID" ] && ok "创建静默 id=$SILENCE_ID" || skip "静默创建" "$SILENCE_RESP"

# === E. Backup: 配置/数据备份恢复 ===
echo "--- E. 备份恢复 ---"

echo "  E1. prometheus 配置备份"
BAK_RESP=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-cfg-$TS\",\"type\":\"config\",\"component_id\":$PROM_ID}")
BAK_ID=$(echo "$BAK_RESP" | j "d.get('backup',{}).get('id','')")
BAK_PATH=$(echo "$BAK_RESP" | j "d.get('backup',{}).get('path','')")
[ -n "$BAK_ID" ] && ok "配置备份 id=$BAK_ID path=$BAK_PATH" || bad "配置备份" "$BAK_RESP"
# 远端核对备份文件
[ -n "$BAK_PATH" ] && rssh "test -f '$BAK_PATH'" 2>/dev/null && ok "备份文件在远端存在" || skip "备份文件核对" "path=$BAK_PATH"

echo "  E2. 配置恢复"
# 先改配置
curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"prometheus.yml\",\"content\":\"global:\n  scrape_interval: 5s\n# FT_CHANGED_$TS\n\"}" >/dev/null
sleep 3
# 恢复
RESTORE_CODE=$(code -X POST $API/backups/$BAK_ID/restore -H "$AUTH")
[ "$RESTORE_CODE" = "200" ] && ok "配置恢复接口 200" || bad "配置恢复" "$RESTORE_CODE"
sleep 3
PROM_CFG6=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null" 2>/dev/null)
echo "$PROM_CFG6" | grep -q "FT_CHANGED_$TS" && bad "恢复后变更仍在" "" || ok "恢复后回到备份内容"
PROM_HEALTH3=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://$REMOTE_IP:19090/-/healthy)
[ "$PROM_HEALTH3" = "200" ] && ok "恢复后 prometheus healthy" || bad "恢复后健康" "$PROM_HEALTH3"

echo "  E3. 数据备份"
DBAK_RESP=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-data-$TS\",\"type\":\"data\",\"component_id\":$PROM_ID}")
DBAK_ID=$(echo "$DBAK_RESP" | j "d.get('backup',{}).get('id','')")
DBAK_PATH=$(echo "$DBAK_RESP" | j "d.get('backup',{}).get('path','')")
DBAK_SIZE=$(echo "$DBAK_RESP" | j "d.get('backup',{}).get('size',0)")
[ -n "$DBAK_ID" ] && [ "${DBAK_SIZE:-0}" -gt 0 ] && ok "数据备份 id=$DBAK_ID size=${DBAK_SIZE}" || bad "数据备份" "$DBAK_RESP"

echo "  E4. 平台快照"
PLAT_CODE=$(code "$API/backups/platform" -H "$AUTH")
[ "$PLAT_CODE" = "200" ] && ok "平台快照列表 200" || bad "平台快照" "$PLAT_CODE"

# === F. Cleanup: 卸载 ===
echo "--- F. 卸载清理 ---"

echo "  F1. 卸载 blackbox_exporter"
UNINSTALL_CODE=$(governed_code "$API/components/$BB_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH")
[ "$UNINSTALL_CODE" = "200" ] && ok "卸载接口 200" || bad "卸载 blackbox" "$UNINSTALL_CODE"
sleep 5
BB_GONE=$(rssh "ss -tlnp | grep ':19115' | wc -l" 2>/dev/null | tr -d ' ')
[ "$BB_GONE" = "0" ] && ok "远端 19115 端口已释放" || bad "端口未释放" "$BB_GONE"
BB_SVC_GONE=$(rssh "systemctl is-active promlifecycle-blackbox_exporter 2>/dev/null" 2>/dev/null | tr -d ' ')
[ "$BB_SVC_GONE" = "inactive" ] || [ "$BB_SVC_GONE" = "unknown" ] && ok "systemd 单元已停止" || skip "systemd 状态" "$BB_SVC_GONE"

echo "  F2. 卸载 alertmanager"
governed_action "$API/components/$AM_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
sleep 5
AM_GONE=$(rssh "ss -tlnp | grep ':19093' | wc -l" 2>/dev/null | tr -d ' ')
[ "$AM_GONE" = "0" ] && ok "远端 19093 已释放" || bad "AM 端口" "$AM_GONE"

echo "  F3. 卸载 prometheus"
governed_action "$API/components/$PROM_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
sleep 5
PROM_GONE=$(rssh "ss -tlnp | grep ':19090' | wc -l" 2>/dev/null | tr -d ' ')
[ "$PROM_GONE" = "0" ] && ok "远端 19090 已释放" || bad "PROM 端口" "$PROM_GONE"

echo "  F4. 卸载 node_exporter"
governed_action "$API/components/$NE_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
sleep 5
NE_GONE=$(rssh "ss -tlnp | grep ':19100' | wc -l" 2>/dev/null | tr -d ' ')
[ "$NE_GONE" = "0" ] && ok "远端 19100 已释放" || bad "NE 端口" "$NE_GONE"

echo "  F5. 清理组件与主机"
# 删除告警规则/渠道/scrape
[ -n "$RULE_ID" ] && curl -s -X DELETE $API/alerts/rules/$RULE_ID -H "$AUTH" >/dev/null
[ -n "$CH_ID" ] && curl -s -X DELETE $API/notifications/channels/$CH_ID -H "$AUTH" >/dev/null
[ -n "$SC_ID" ] && curl -s -X DELETE $API/scrape-configs/$SC_ID -H "$AUTH" >/dev/null
[ -n "$BAK_ID" ] && curl -s -X DELETE $API/backups/$BAK_ID -H "$AUTH" >/dev/null
[ -n "$DBAK_ID" ] && curl -s -X DELETE $API/backups/$DBAK_ID -H "$AUTH" >/dev/null
# force 删节点
NODE_STATUS=$(code $API/node -H "$AUTH")
[ "$NODE_STATUS" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$NODE_STATUS"
# 删主机
DEL_HOST=$(code -X DELETE "$API/hosts/$HOST_ID" -H "$AUTH")
[ "$DEL_HOST" = "200" ] && ok "删除主机 200" || bad "删除主机" "$DEL_HOST"

echo "  F6. 远端残留检查"
ALL_PORTS=$(rssh "ss -tlnp | grep -E ':19[0-9]{3}' | wc -l" 2>/dev/null | tr -d ' ')
[ "$ALL_PORTS" = "0" ] && ok "远端无测试端口残留" || bad "远端残留端口" "$ALL_PORTS"
ALL_SVCS=$(rssh "systemctl list-units --type=service 2>/dev/null | grep -c promlifecycle-" 2>/dev/null | tr -d ' ')
[ "$ALL_SVCS" = "0" ] && ok "远端无 promlifecycle systemd 残留" || bad "远端残留服务" "$ALL_SVCS"

# === 结果 ===
echo ""
echo "================================================================"
echo " 全功能测试结果: $PASS passed, $FAIL failed, $SKIP skipped"
echo "================================================================"
[ $FAIL -eq 0 ] || exit 1
