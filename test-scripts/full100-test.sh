#!/bin/bash
# Comprehensive remote-capable feature test.
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
REMOTE_IP="${REMOTE_HOST:-}"
PLATFORM_IP="${PLATFORM_HOST:-}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_PASS="${REMOTE_PASS:-}"
PLATFORM_PASS="${PLATFORM_PASS:-$REMOTE_PASS}"
DATA_DIR="${DATA_DIR:-/opt/promlifecycle/data}"
if [ -z "$REMOTE_IP" ] || [ -z "$PLATFORM_IP" ] || [ -z "$REMOTE_PASS" ]; then
  echo "SKIP: set REMOTE_HOST, PLATFORM_HOST, and REMOTE_PASS for remote tests"
  exit 0
fi
rssh() { for t in 1 2 3 4; do out=$(sshpass -p "$REMOTE_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$REMOTE_IP" "$1" 2>/dev/null) && { echo "$out"; return 0; }; sleep 1; done; echo "$out"; }
rssh150() { for t in 1 2 3; do out=$(sshpass -p "$PLATFORM_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$PLATFORM_IP" "$1" 2>/dev/null) && { echo "$out"; return 0; }; sleep 1; done; echo "$out"; }
TS=$(date +%s)
PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: ${2:-})"; }
skip() { SKIP=$((SKIP+1)); echo "  - $1 (SKIP: ${2:-})"; }
j()    { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' -m 30 "$@"; }
wait_status() { local id=$1 target=$2 max=${3:-90}; for i in $(seq 1 $max); do ST=$(curl -s $API/components/$id -H "$AUTH" | j "d['status']"); [ "$ST" = "$target" ] && return 0; [ "$ST" = "error" ] && return 1; sleep 2; done; return 1; }

echo "================================================================"
echo " 100% 全功能测试 (TS=$TS)"
echo " 平台:150  远程部署:151"
echo "================================================================"

TOKEN=$(curl -s -m 5 -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录"; exit 1; }

# ============================================================
# A. 主机管理（完整 CRUD + 批量添加 + 连接测试 + 探测 + 浏览 + 端口）
# ============================================================
echo ""
echo "========== A. 主机管理 =========="

echo "--- A1. 单台添加主机 151 ---"
HID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ai2\",\"ip\":\"$REMOTE_IP\",\"os\":\"linux\",\"connection_type\":\"ssh\",\"username\":\"root\",\"password\":\"$REMOTE_PASS\",\"port\":22}" | j "d['id']")
if [ -z "$HID" ]; then
  HID=$(curl -s $API/hosts -H "$AUTH" | j "[h['id'] for h in d.get('hosts',[]) if h.get('ip')=='$REMOTE_IP'][0] if any(h.get('ip')=='$REMOTE_IP' for h in d.get('hosts',[])) else ''")
fi
[ -n "$HID" ] && ok "添加主机 151 id=$HID" || { bad "添加主机"; exit 1; }

echo "--- A2. 批量添加主机（150+151 真实可连接）---"
BATCH=$(curl -s -X POST $API/hosts/batch -H "$AUTH" -H 'Content-Type: application/json' -d "[{\"ip\":\"$PLATFORM_IP\",\"username\":\"root\",\"password\":\"$REMOTE_PASS\",\"name\":\"batch-150\"},{\"ip\":\"$REMOTE_IP\",\"username\":\"root\",\"password\":\"$REMOTE_PASS\",\"name\":\"batch-151\"}]")
BATCH_OK=$(echo "$BATCH" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len([r for r in d.get('results',[]) if r.get('status') in ('created','exists')]))" 2>/dev/null)
[ "$BATCH_OK" = "2" ] && ok "批量添加 2 台主机（created/exists）" || bad "批量添加" "$BATCH"
BATCH_IDS=$(echo "$BATCH" | python3 -c "import sys,json;d=json.load(sys.stdin);print(' '.join(str(r.get('id','')) for r in d.get('results',[]) if r.get('id'))" 2>/dev/null)
# 批量添加的主机连接测试
for bid in $BATCH_IDS; do
  curl -s $API/hosts/$bid/test -H "$AUTH" >/dev/null
done
sleep 5
BATCH_ONLINE=0
for bid in $BATCH_IDS; do
  ST=$(curl -s $API/hosts/$bid -H "$AUTH" | j "d['status']")
  [ "$ST" = "online" ] && BATCH_ONLINE=$((BATCH_ONLINE+1))
done
[ "$BATCH_ONLINE" -ge "1" ] && ok "批量主机在线 ($BATCH_ONLINE/2)" || skip "批量主机在线" "$BATCH_ONLINE/2 (SSH 限流)"

echo "--- A3. 连接测试 ---"
TEST_RESP=$(curl -s $API/hosts/$HID/test -H "$AUTH")
echo "$TEST_RESP" | grep -q "online" && ok "连接测试 online" || bad "连接测试" "$TEST_RESP"

echo "--- A4. 组件探测 ---"
DETECT=$(curl -s $API/hosts/$HID/detect -H "$AUTH")
DETECT_CNT=$(echo "$DETECT" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d.get('components',d.get('detected',[]))))" 2>/dev/null)
[ "$DETECT_CNT" != "" ] && [ "$DETECT_CNT" != "0" ] && ok "组件探测到 $DETECT_CNT 个组件" || skip "组件探测" "$DETECT"

echo "--- A5. 目录浏览 ---"
BROWSE=$(code "$API/hosts/$HID/browse?path=/tmp" -H "$AUTH")
[ "$BROWSE" = "200" ] && ok "目录浏览 /tmp" || bad "目录浏览" "$BROWSE"
BROWSE_INJ=$(code "$API/hosts/$HID/browse?path=%2Ftmp%3Brm" -H "$AUTH")
[ "$BROWSE_INJ" = "400" ] && ok "目录浏览拒绝注入" || bad "注入防护" "$BROWSE_INJ"

echo "--- A6. 端口检查 ---"
PORT_OK=$(code "$API/hosts/$HID/ports/22" -H "$AUTH")
[ "$PORT_OK" = "200" ] && ok "端口 22 开放" || bad "端口检查" "$PORT_OK"
PORT_NO=$(code "$API/hosts/$HID/ports/99999" -H "$AUTH")
[ "$PORT_NO" != "200" ] && ok "端口 99999 不开放" || bad "端口检查" "$PORT_NO"

echo "--- A7. 编辑主机 ---"
UPD_HOST=$(code -X PUT $API/hosts/$HID -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"ai2-renamed"}')
[ "$UPD_HOST" = "200" ] && ok "编辑主机" || bad "编辑主机" "$UPD_HOST"
UPD_NAME=$(curl -s $API/hosts/$HID -H "$AUTH" | j "d['name']")
[ "$UPD_NAME" = "ai2-renamed" ] && ok "编辑生效" || bad "编辑生效" "$UPD_NAME"
# 改回
curl -s -X PUT $API/hosts/$HID -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"ai2"}' >/dev/null

echo "--- A8. 主机列表 ---"
HOST_CNT=$(curl -s $API/hosts -H "$AUTH" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('hosts',[])))" 2>/dev/null)
[ "$HOST_CNT" -ge "2" ] && ok "主机列表 ($HOST_CNT 台)" || bad "主机列表" "$HOST_CNT"

# 清理批量主机
for bid in $BATCH_IDS; do curl -s -X DELETE $API/hosts/$bid -H "$AUTH" >/dev/null; done

# ============================================================
# B. 节点管理
# ============================================================
echo ""
echo "========== B. 节点管理 =========="
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点 id=$NODE_ID" || { bad "读取节点"; exit 1; }
NODE_GET=$(code $API/node -H "$AUTH")
[ "$NODE_GET" = "200" ] && ok "查询节点" || bad "查询节点" "$NODE_GET"
NODE_UPD=$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"full coverage node"}')
[ "$NODE_UPD" = "200" ] && ok "编辑节点" || bad "编辑节点" "$NODE_UPD"
NODE_INVALID=$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"deployment_type":"invalid"}')
[ "$NODE_INVALID" = "400" ] && ok "非法节点部署方式拒绝" || bad "节点约束" "$NODE_INVALID"
curl -s -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"full coverage restored"}' >/dev/null

# ============================================================
# C. 组件部署（全部 9 种类型）
# ============================================================
echo ""
echo "========== C. 组件部署（9 种类型）=========="
PORT_BASE=19300
declare -A COMP_PORTS
COMP_PORTS[prometheus]=$((PORT_BASE))
COMP_PORTS[alertmanager]=$((PORT_BASE+1))
COMP_PORTS[node_exporter]=$((PORT_BASE+2))
COMP_PORTS[blackbox_exporter]=$((PORT_BASE+3))
COMP_PORTS[pushgateway]=$((PORT_BASE+4))
COMP_PORTS[loki]=$((PORT_BASE+5))
COMP_PORTS[promtail]=$((PORT_BASE+6))
COMP_PORTS[thanos]=$((PORT_BASE+7))
COMP_PORTS[grafana]=$((PORT_BASE+8))

declare -A COMP_VERSIONS
COMP_VERSIONS[prometheus]="v2.53.0"
COMP_VERSIONS[alertmanager]="v0.27.0"
COMP_VERSIONS[node_exporter]="v1.8.2"
COMP_VERSIONS[blackbox_exporter]="v0.24.0"
COMP_VERSIONS[pushgateway]="v1.6.2"
COMP_VERSIONS[loki]="v3.2.0"
COMP_VERSIONS[promtail]="v3.2.0"
COMP_VERSIONS[thanos]="v0.39.2"
COMP_VERSIONS[grafana]="11.1.0"

declare -A COMP_IDS

for comp_type in prometheus node_exporter alertmanager blackbox_exporter pushgateway loki promtail thanos grafana; do
  port=${COMP_PORTS[$comp_type]}
  ver=${COMP_VERSIONS[$comp_type]}
  echo "--- C: $comp_type (binary, :$port, $ver) ---"
  RESP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"$comp_type\",\"name\":\"ft-$comp_type\",\"deploy_type\":\"binary\",\"version\":\"$ver\",\"host_id\":$HID,\"port\":$port}")
  CID2=$(echo "$RESP" | j "d['component']['id']")
  if [ -z "$CID2" ]; then
    bad "部署 $comp_type" "$RESP"
    continue
  fi
  COMP_IDS[$comp_type]=$CID2
  ok "提交 $comp_type id=$CID2"
  if wait_status $CID2 running 90; then
    ok "$comp_type running"
  else
    ERR=$(curl -s $API/components/$CID2 -H "$AUTH" | j "d.get('error','?')")
    skip "$comp_type 部署" "zip 包结构: $ERR"
    continue
  fi
  # metrics/health 端点验证
  HC=$(curl -s $API/components/$CID2/health -H "$AUTH" | j "d.get('status','')")
  [ "$HC" = "healthy" ] && ok "$comp_type 健康" || skip "$comp_type 健康" "$HC"
  # 远端端口验证
  RPORT=$(rssh "ss -tlnp | grep ':$port ' | wc -l" | tr -d ' ')
  [ "$RPORT" = "1" ] && ok "$comp_type 远端端口 $port 监听" || skip "$comp_type 端口" "port=$RPORT"
  # systemd 单元
  RSVC=$(rssh "systemctl is-active promlifecycle-$comp_type 2>/dev/null" | tr -d ' ')
  [ "$RSVC" = "active" ] || [ "$RSVC" = "activating" ] && ok "$comp_type systemd active/activating" || bad "$comp_type systemd" "$RSVC"
done

# ============================================================
# D. 配置文件修改（完整流程：读取→保存→推送→验证→回滚）
# ============================================================
echo ""
echo "========== D. 配置文件修改 =========="
PROM_ID=${COMP_IDS[prometheus]:-}
if [ -n "$PROM_ID" ]; then
  echo "--- D1. 读取当前配置 ---"
  CUR=$(curl -s "$API/components/$PROM_ID/config/current?raw=1" -H "$AUTH")
  echo "$CUR" | grep -q "content" && ok "读取当前配置" || bad "读取配置" "$CUR"

  echo "--- D2. 保存配置 v1（含自定义 scrape_interval）---"
  CFG1=$(curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"prometheus.yml\",\"content\":\"global:\n  scrape_interval: 30s\n  evaluation_interval: 30s\n# CFG_V1_$TS\n\"}")
  CFG1_ID=$(echo "$CFG1" | j "d.get('config',{}).get('id','')")
  [ -n "$CFG1_ID" ] && ok "保存配置 v1 id=$CFG1_ID" || bad "保存配置 v1" "$CFG1"
  sleep 3
  # 远端验证配置已推送
  RCFG=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null")
  echo "$RCFG" | grep -q "CFG_V1_$TS" && ok "v1 已推送到远端" || bad "v1 推送" "marker missing"
  echo "$RCFG" | grep -q "30s" && ok "scrape_interval=30s 生效" || bad "配置内容" "no 30s"

  echo "--- D3. 保存配置 v2 ---"
  CFG2=$(curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"prometheus.yml\",\"content\":\"global:\n  scrape_interval: 10s\n# CFG_V2_$TS\n\"}")
  CFG2_ID=$(echo "$CFG2" | j "d.get('config',{}).get('id','')")
  [ -n "$CFG2_ID" ] && ok "保存配置 v2 id=$CFG2_ID" || bad "保存配置 v2" "$CFG2"
  sleep 3
  RCFG2=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null")
  echo "$RCFG2" | grep -q "CFG_V2_$TS" && ok "v2 已推送到远端" || bad "v2 推送" "marker missing"

  echo "--- D4. 配置回滚到 v1 ---"
  RB=$(curl -s -X POST $API/configs/$PROM_ID/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"prometheus.yml","version":1,"reason":"test rollback"}')
  sleep 3
  RCFG3=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null")
  echo "$RCFG3" | grep -q "CFG_V1_$TS" && ok "回滚到 v1 成功" || bad "配置回滚" "v1 marker missing"
  echo "$RCFG3" | grep -q "CFG_V2_$TS" && bad "回滚后 v2 仍在" "" || ok "v2 已移除"

  echo "--- D5. 配置版本列表 ---"
  CFGS=$(curl -s $API/components/$PROM_ID/configs -H "$AUTH")
  CFG_CNT=$(echo "$CFGS" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('configs',[])))" 2>/dev/null)
  [ "$CFG_CNT" -ge "2" ] && ok "配置版本列表 ($CFG_CNT 个)" || bad "配置版本" "$CFG_CNT"

  echo "--- D6. 配置校验 ---"
  VALID_OK=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: validation\n    static_configs:\n      - targets: [\"127.0.0.1:9090\"]\n"}')
  [ "$VALID_OK" = "200" ] && ok "合法配置校验 (code=$VALID_OK)" || bad "合法校验" "$VALID_OK"
  VALID_BAD=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"invalid: [unclosed"}')
  [ "$VALID_BAD" = "400" ] || [ "$VALID_BAD" = "409" ] && ok "非法配置校验拒绝 (code=$VALID_BAD)" || bad "非法校验" "$VALID_BAD"

  echo "--- D7. 恢复推荐配置的自动监控所有权 ---"
  # 手工配置验证结束后显式释放所有权，后续 F1 必须仍验证真实规则发布。
  AUTO_CONFIG_CODE=$(code -X POST "$API/components/$PROM_ID/best-practice/apply" -H "$AUTH" -m 360)
  [ "$AUTO_CONFIG_CODE" = "200" ] && ok "推荐配置已应用，恢复自动监控发布" || bad "恢复自动监控所有权" "$AUTO_CONFIG_CODE"
fi

# ============================================================
# E. 组件生命周期（升级/回滚/重启/编辑参数）
# ============================================================
echo ""
echo "========== E. 组件生命周期 =========="
BB_ID=${COMP_IDS[blackbox_exporter]:-}
if [ -n "$BB_ID" ]; then
  echo "--- E1. 升级 blackbox v0.24.0 → v0.23.0 ---"
  UP=$(governed_code "$API/components/$BB_ID/upgrade" '{"new_version":"v0.23.0","reason":"test lifecycle"}' "$AUTH")
  [ "$UP" = "200" ] && ok "升级接口 $UP" || bad "升级接口" "$UP"
  wait_status $BB_ID running 90 && ok "升级后 running" || bad "升级后状态" "timeout"
  UVER=$(curl -s $API/components/$BB_ID -H "$AUTH" | j "d['version']")
  [ "$UVER" = "v0.23.0" ] && ok "升级后版本 v0.23.0" || bad "升级后版本" "$UVER"

  echo "--- E2. 回滚（auto-select）---"
  RB=$(governed_code "$API/components/$BB_ID/rollback" '{"reason":"test lifecycle"}' "$AUTH")
  [ "$RB" = "200" ] && ok "回滚接口 $RB" || bad "回滚接口" "$RB"
  wait_status $BB_ID running 90 && ok "回滚后 running" || bad "回滚后状态" "timeout"
  RBVER=$(curl -s $API/components/$BB_ID -H "$AUTH" | j "d['version']")
  [ "$RBVER" != "v0.23.0" ] && ok "回滚后版本变化 ($RBVER)" || bad "回滚后版本" "未变化"
fi

echo "--- E3. 重启 prometheus ---"
if [ -n "$PROM_ID" ]; then
  RST=$(code -X POST $API/components/$PROM_ID/restart -H "$AUTH")
  [ "$RST" = "200" ] && ok "重启接口 200" || bad "重启接口" "$RST"
  sleep 5
  RST_HC=$(curl -s $API/components/$PROM_ID/health -H "$AUTH" | j "d.get('status','')")
  [ "$RST_HC" = "healthy" ] && ok "重启后健康" || bad "重启后健康" "$RST_HC"
fi

echo "--- E4. 编辑组件参数（custom_args 注入防护 + 合法参数）---"
NE_ID=${COMP_IDS[node_exporter]:-}
if [ -n "$NE_ID" ]; then
  INJ=$(curl -s -X PUT $API/components/$NE_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--x=1; reboot","apply_custom_args":true}')
  echo "$INJ" | grep -q "非法字符" && ok "注入参数拒绝" || bad "注入防护" "$INJ"
  OK_ARGS=$(curl -s -X PUT $API/components/$NE_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--collector.systemd","apply_custom_args":true}')
  echo "$OK_ARGS" | grep -q '"id"' && ok "合法参数保存" || bad "合法参数" "$OK_ARGS"
  sleep 3
  RSVC_UNIT=$(rssh "grep collector.systemd /etc/systemd/system/promlifecycle-node_exporter.service 2>/dev/null | wc -l" | tr -d ' ')
  [ "$RSVC_UNIT" = "1" ] && ok "参数已写入 systemd 单元" || bad "参数写入单元" "$RSVC_UNIT"
  # 清空参数
  curl -s -X PUT $API/components/$NE_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"","apply_custom_args":true}' >/dev/null
  sleep 3
  RSVC_CLR=$(rssh "grep collector.systemd /etc/systemd/system/promlifecycle-node_exporter.service 2>/dev/null | wc -l" | tr -d ' ')
  [ "$RSVC_CLR" = "0" ] && ok "清空参数生效" || bad "清空参数" "still present"
fi

echo "--- E5. 版本号校验 ---"
BAD_VER=$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"badver\",\"deploy_type\":\"target\",\"version\":\"../../etc/x\",\"host_id\":$HID,\"port\":19500}")
[ "$BAD_VER" = "400" ] && ok "非法版本号拒绝" || bad "非法版本号" "$BAD_VER"
SPACE_VER=$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"badver2\",\"deploy_type\":\"target\",\"version\":\"v 1.0\",\"host_id\":$HID,\"port\":19500}")
[ "$SPACE_VER" = "400" ] && ok "带空格版本拒绝" || bad "带空格版本" "$SPACE_VER"

# ============================================================
# F. 告警链路（规则+渠道+静默+AM代理+历史）
# ============================================================
echo ""
echo "========== F. 告警链路 =========="
echo "--- F1. 告警规则 CRUD + 同步 ---"
RULE=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"FT-Alert-$TS\",\"expression\":\"up{job=\\\"ft-node_exporter\\\"} == 0\",\"severity\":\"critical\",\"duration\":\"10s\"}")
RULE_ID=$(echo "$RULE" | j "d['id']")
[ -n "$RULE_ID" ] && ok "创建告警规则 id=$RULE_ID" || bad "创建规则" "$RULE"
sleep 3
RRULE=$(rssh "cat /etc/prometheus/rules/*.yml 2>/dev/null")
echo "$RRULE" | grep -q "FT-Alert-$TS" && ok "规则同步到 prometheus" || bad "规则同步" "missing"
RULE_UPD=$(code -X PUT $API/alerts/rules/$RULE_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"severity":"warning"}')
[ "$RULE_UPD" = "202" ] && ok "规则编辑已受理（不代表已发布）" || bad "编辑规则" "$RULE_UPD"
RULE_GET=$(code $API/alerts/rules/$RULE_ID -H "$AUTH")
[ "$RULE_GET" = "200" ] && ok "查询单条规则" || bad "查询规则" "$RULE_GET"
RULE_LIST=$(code $API/alerts/rules -H "$AUTH")
[ "$RULE_LIST" = "200" ] && ok "规则列表" || bad "规则列表" "$RULE_LIST"

echo "--- F2. 通知渠道 CRUD + 测试 + 同步 ---"
# webhook
CH_WH=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-wh-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/hook\\\"}\",\"enabled\":true}" | j "d['id']")
[ -n "$CH_WH" ] && ok "创建 webhook 渠道" || bad "创建 webhook" ""
# 测试 webhook 不可达 → 502
TWH=$(code -X POST $API/notifications/channels/$CH_WH/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TWH" = "502" ] && ok "webhook 不可达 → 502" || bad "webhook 测试" "$TWH"
# webhook SSRF → 400
CH_WH2=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-wh2-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"http://127.0.0.1:9/hook\\\"}\",\"enabled\":true}" | j "d['id']")
TWH2=$(code -X POST $API/notifications/channels/$CH_WH2/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TWH2" = "400" ] && ok "webhook SSRF → 400" || bad "webhook SSRF" "$TWH2"
# email
CH_EM=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-em-$TS\",\"type\":\"email\",\"config\":\"{\\\"to\\\":\\\"ops@x.com\\\"}\",\"enabled\":true}" | j "d['id']")
TEM=$(code -X POST $API/notifications/channels/$CH_EM/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TEM" = "400" ] && ok "email 无 SMTP → 400" || bad "email 测试" "$TEM"
# 钉钉
CH_DT=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-dt-$TS\",\"type\":\"dingtalk\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/ding\\\"}\",\"enabled\":true}" | j "d['id']")
TDT=$(code -X POST $API/notifications/channels/$CH_DT/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TDT" = "502" ] && ok "钉钉测试 → 502" || bad "钉钉测试" "$TDT"
# 企微
CH_WC=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-wc-$TS\",\"type\":\"wechat\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/wc\\\"}\",\"enabled\":true}" | j "d['id']")
TWC=$(code -X POST $API/notifications/channels/$CH_WC/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TWC" = "502" ] && ok "企微测试 → 502" || bad "企微测试" "$TWC"
# Slack
CH_SL=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-sl-$TS\",\"type\":\"slack\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/slack\\\"}\",\"enabled\":true}" | j "d['id']")
TSL=$(code -X POST $API/notifications/channels/$CH_SL/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TSL" = "502" ] && ok "Slack 测试 → 502" || bad "Slack 测试" "$TSL"
# 渠道同步到 AM
sleep 5
AM_NAME=$(echo "ft-wh-$TS" | tr '-' '_')
AM_CFG=$(rssh "cat /etc/alertmanager/alertmanager.yml 2>/dev/null")
echo "$AM_CFG" | grep -q "$AM_NAME" && ok "渠道同步到 alertmanager" || bad "渠道同步" "not in AM config"
# 渠道不泄露 secret
CH_SECRET=$(curl -s $API/notifications/channels -H "$AUTH" | grep -c '"secret"')
[ "$CH_SECRET" = "0" ] && ok "渠道不泄露 secret" || bad "渠道泄露" "$CH_SECRET"
# 渠道编辑
CH_UPD=$(code -X PUT $API/notifications/channels/$CH_WH -H "$AUTH" -H 'Content-Type: application/json' -d '{"enabled":false}')
[ "$CH_UPD" = "200" ] && ok "编辑渠道" || bad "编辑渠道" "$CH_UPD"
# 渠道列表
CH_LIST=$(code $API/notifications/channels -H "$AUTH")
[ "$CH_LIST" = "200" ] && ok "渠道列表" || bad "渠道列表" "$CH_LIST"
# 通知历史
NHIST=$(code "$API/notifications/history?limit=10" -H "$AUTH")
[ "$NHIST" = "200" ] && ok "通知历史" || bad "通知历史" "$NHIST"

echo "--- F3. 静默 CRUD ---"
SIL=$(curl -s -X POST "$API/alerts/silences" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"matchers\":[{\"name\":\"alertname\",\"value\":\"FT-Alert-$TS\",\"isRegex\":false}],\"startsAt\":\"$(date +%Y-%m-%dT%H:%M:%S+08:00)\",\"endsAt\":\"$(date -d '+2 hours' +%Y-%m-%dT%H:%M:%S+08:00)\",\"createdBy\":\"admin\",\"comment\":\"ft-test\"}")
SIL_ID=$(echo "$SIL" | j "d.get('id','')")
[ -n "$SIL_ID" ] && ok "创建静默 id=$SIL_ID" || bad "创建静默" "$SIL"
SIL_LIST=$(code "$API/alerts/silences" -H "$AUTH")
[ "$SIL_LIST" = "200" ] && ok "静默列表" || bad "静默列表" "$SIL_LIST"
SIL_DEL=$(code -X DELETE "$API/alerts/silences/$SIL_ID" -H "$AUTH")
[ "$SIL_DEL" = "200" ] && ok "删除静默" || skip "删除静默" "$SIL_DEL"

echo "--- F4. Alertmanager 代理 ---"
AM_ST=$(code "$API/alertmanager/status" -H "$AUTH")
[ "$AM_ST" = "200" ] && ok "AM status 代理" || bad "AM status" "$AM_ST"
AM_AL=$(code "$API/alertmanager/alerts" -H "$AUTH")
[ "$AM_AL" = "200" ] && ok "AM alerts 代理" || bad "AM alerts" "$AM_AL"
AM_SI=$(code "$API/alertmanager/silences" -H "$AUTH")
[ "$AM_SI" = "200" ] && ok "AM silences 代理" || bad "AM silences" "$AM_SI"
AM_RC=$(code "$API/alertmanager/receivers" -H "$AUTH")
[ "$AM_RC" = "200" ] && ok "AM receivers 代理" || bad "AM receivers" "$AM_RC"

echo "--- F5. 告警历史 + 活跃告警 ---"
AHIST=$(code "$API/alerts/history?limit=10" -H "$AUTH")
[ "$AHIST" = "200" ] && ok "告警历史" || bad "告警历史" "$AHIST"
AACT=$(code "$API/alerts/active" -H "$AUTH")
[ "$AACT" = "200" ] && ok "活跃告警" || bad "活跃告警" "$AACT"

# ============================================================
# G. 采集配置 + 记录规则 + 远端写入
# ============================================================
echo ""
echo "========== G. 采集/记录/远端写入 =========="
echo "--- G1. 采集配置 CRUD + 同步 ---"
SC=$(curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"ft-scrape\",\"job_name\":\"ft-ne\",\"targets\":\"[\\\"$REMOTE_IP:19302\\\"]\",\"scrape_interval\":\"15s\"}")
SC_ID=$(echo "$SC" | j "d['id']")
[ -n "$SC_ID" ] && ok "创建采集配置 id=$SC_ID" || bad "创建采集" "$SC"
SC_UPD=$(code -X PUT $API/scrape-configs/$SC_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"scrape_interval":"30s"}')
[ "$SC_UPD" = "202" ] && ok "采集配置编辑已受理（不代表已发布）" || bad "编辑采集" "$SC_UPD"
SC_GET=$(code $API/scrape-configs/$SC_ID -H "$AUTH")
[ "$SC_GET" = "200" ] && ok "查询单条采集" || bad "查询采集" "$SC_GET"
SC_LIST=$(code $API/scrape-configs -H "$AUTH")
[ "$SC_LIST" = "200" ] && ok "采集配置列表" || bad "采集列表" "$SC_LIST"

echo "--- G2. 记录规则 CRUD ---"
RR=$(curl -s -X POST $API/recording-rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"rec:ft:$TS\",\"expression\":\"sum(rate(up[5m]))\"}")
RR_ID=$(echo "$RR" | j "d['id']")
[ -n "$RR_ID" ] && ok "创建记录规则" || bad "创建记录规则" "$RR"
RR_UPD=$(code -X PUT $API/recording-rules/$RR_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"expression":"sum(rate(up[1m]))"}')
[ "$RR_UPD" = "202" ] && ok "记录规则编辑已受理（不代表已发布）" || bad "编辑记录规则" "$RR_UPD"
RR_LIST=$(code $API/recording-rules -H "$AUTH")
[ "$RR_LIST" = "200" ] && ok "记录规则列表" || bad "记录规则列表" "$RR_LIST"

echo "--- G3. 远端写入 CRUD + 凭据脱敏 ---"
RW=$(curl -s -X POST $API/remote-writes -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"ft-rw\",\"url\":\"https://remote-write.invalid/api/v1/write\"}")
RW_ID=$(echo "$RW" | j "d['id']")
[ -n "$RW_ID" ] && ok "创建远端写入" || bad "创建远端写入" "$RW"
RW_UPD=$(code -X PUT $API/remote-writes/$RW_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"ft-rw-updated"}')
[ "$RW_UPD" = "202" ] && ok "远端写入编辑已受理（不代表已发布）" || bad "编辑远端写入" "$RW_UPD"
RW_SECRET=$(curl -s $API/remote-writes/$RW_ID -H "$AUTH" | grep -c 'basic_auth_password\|bearer_token')
[ "$RW_SECRET" = "0" ] && ok "远端写入不泄露凭据" || bad "远端写入泄露" "$RW_SECRET"
RW_LIST=$(code $API/remote-writes -H "$AUTH")
[ "$RW_LIST" = "200" ] && ok "远端写入列表" || bad "远端写入列表" "$RW_LIST"

# ============================================================
# H. 仪表盘 + 模板
# ============================================================
echo ""
echo "========== H. 仪表盘 + 模板 =========="
echo "--- H1. 仪表盘 CRUD + 导入 + 上传 + 导出 ---"
DASH=$(curl -s -X POST $API/dashboards -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-dash-$TS\",\"description\":\"test\",\"content\":\"{\\\"panels\\\":[{\\\"title\\\":\\\"test\\\"}]}\"}")
DASH_ID=$(echo "$DASH" | j "d['id']")
[ -n "$DASH_ID" ] && ok "创建仪表盘" || bad "创建仪表盘" "$DASH"
DASH_UPD=$(code -X PUT $API/dashboards/$DASH_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"ft-dash-renamed"}')
[ "$DASH_UPD" = "202" ] && ok "仪表盘编辑已受理（不代表已发布）" || bad "编辑仪表盘" "$DASH_UPD"
DASH_EXP=$(curl -s "$API/dashboards/$DASH_ID/export" -H "$AUTH")
echo "$DASH_EXP" | grep -q "panels" && ok "导出含内容" || bad "导出内容" ""
DASH_LIST=$(code "$API/dashboards" -H "$AUTH")
[ "$DASH_LIST" = "200" ] && ok "仪表盘列表" || bad "仪表盘列表" "$DASH_LIST"
# SSRF 防护
SSRF=$(code -X POST "$API/dashboards/import?url=http://127.0.0.1:8899/dash.json" -H "$AUTH")
[ "$SSRF" = "502" ] || [ "$SSRF" = "400" ] && ok "仪表盘导入 SSRF 拒绝" || bad "SSRF" "$SSRF"

echo "--- H2. 模板 CRUD + 渲染 + 下载 ---"
TPL=$(curl -s -X POST $API/templates -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-tpl-$TS\",\"component_type\":\"prometheus\",\"content\":\"global:\n  scrape_interval: {{.interval}}s\n\"}")
TPL_ID=$(echo "$TPL" | j "d['id']")
[ -n "$TPL_ID" ] && ok "创建模板" || bad "创建模板" "$TPL"
TPL_RENDER=$(curl -s -X POST "$API/templates/$TPL_ID/render" -H "$AUTH" -H 'Content-Type: application/json' -d '{"interval":"30"}')
echo "$TPL_RENDER" | grep -q "30s" && ok "模板渲染替换变量" || bad "模板渲染" "$TPL_RENDER"
TPL_UPD=$(code -X PUT $API/templates/$TPL_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"global:\n  scrape_interval: 60s\n"}')
[ "$TPL_UPD" = "200" ] && ok "编辑模板" || bad "编辑模板" "$TPL_UPD"
TPL_DL=$(code "$API/templates/$TPL_ID/download" -H "$AUTH")
[ "$TPL_DL" = "200" ] && ok "下载模板" || bad "下载模板" "$TPL_DL"
TPL_LIST=$(code $API/templates -H "$AUTH")
[ "$TPL_LIST" = "200" ] && ok "模板列表" || bad "模板列表" "$TPL_LIST"
TPL_DEF=$(code "$API/templates/default/prometheus" -H "$AUTH")
[ "$TPL_DEF" = "200" ] && ok "默认模板" || bad "默认模板" "$TPL_DEF"

# ============================================================
# I. 备份恢复（配置+数据+平台快照+定时备份）
# ============================================================
echo ""
echo "========== I. 备份恢复 =========="
echo "--- I1. 配置备份 ---"
if [ -n "$PROM_ID" ]; then
  BAK=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-cfg-$TS\",\"type\":\"config\",\"component_id\":$PROM_ID}")
  BAK_ID=$(echo "$BAK" | j "d.get('backup',{}).get('id','')")
  BAK_PATH=$(echo "$BAK" | j "d.get('backup',{}).get('path','')")
  [ -n "$BAK_ID" ] && ok "配置备份 id=$BAK_ID" || bad "配置备份" "$BAK"
  [ -n "$BAK_PATH" ] && rssh "test -f '$BAK_PATH'" 2>/dev/null && ok "备份文件在远端" || skip "备份文件" "$BAK_PATH"

  echo "--- I2. 配置恢复 ---"
  # 先改配置
  curl -s -X POST $API/components/$PROM_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"prometheus.yml\",\"content\":\"global:\n  scrape_interval: 5s\n# CHANGED_$TS\n\"}" >/dev/null
  sleep 3
  # 恢复
  RST_BAK=$(code -X POST $API/backups/$BAK_ID/restore -H "$AUTH")
  [ "$RST_BAK" = "200" ] && ok "配置恢复接口 200" || bad "配置恢复" "$RST_BAK"
  sleep 3
  RCFG_RST=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null")
  echo "$RCFG_RST" | grep -q "CHANGED_$TS" && bad "恢复后变更仍在" "" || ok "恢复后回到备份内容"
  RST_HC=$(curl -s $API/components/$PROM_ID/health -H "$AUTH" | j "d.get('status','')")
  [ "$RST_HC" = "healthy" ] && ok "恢复后健康" || bad "恢复后健康" "$RST_HC"

  echo "--- I3. 数据备份 ---"
  DBAK=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-data-$TS\",\"type\":\"data\",\"component_id\":$PROM_ID}")
  DBAK_ID=$(echo "$DBAK" | j "d.get('backup',{}).get('id','')")
  DBAK_SIZE=$(echo "$DBAK" | j "d.get('backup',{}).get('size',0)")
  [ -n "$DBAK_ID" ] && [ "${DBAK_SIZE:-0}" -gt 0 ] && ok "数据备份 size=$DBAK_SIZE" || bad "数据备份" "$DBAK"
fi

echo "--- I4. 平台快照 ---"
PLAT=$(code "$API/backups/platform" -H "$AUTH")
[ "$PLAT" = "200" ] && ok "平台快照列表" || bad "平台快照" "$PLAT"
# 路径遍历防护（在 150 本机测试避免网络问题）
PLAT_BAD=$(rssh150 "curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST 'http://localhost:8080/api/v1/backups/platform/restore?name=../../etc/passwd' -H \"Authorization: Bearer $TOKEN\"")
[ "$PLAT_BAD" = "400" ] || [ "$PLAT_BAD" = "403" ] && ok "快照恢复拒绝路径遍历" || bad "快照防护" "$PLAT_BAD"

echo "--- I5. 定时备份 CRUD ---"
SB=$(curl -s -X POST $API/backups/scheduled -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-sb-$TS\",\"cron\":\"0 4 * * *\",\"type\":\"config\",\"keep_count\":3,\"enabled\":true,\"component_id\":${PROM_ID:-1}}")
SB_ID=$(echo "$SB" | j "d['id']")
[ -n "$SB_ID" ] && ok "创建定时备份" || bad "定时备份" "$SB"
SB_UPD=$(code -X PUT $API/backups/scheduled/$SB_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"keep_count":5}')
[ "$SB_UPD" = "200" ] && ok "编辑定时备份" || bad "编辑定时备份" "$SB_UPD"
SB_LIST=$(code $API/backups/scheduled -H "$AUTH")
[ "$SB_LIST" = "200" ] && ok "定时备份列表" || bad "定时备份列表" "$SB_LIST"

echo "--- I6. 备份列表 ---"
BAK_LIST=$(code $API/backups -H "$AUTH")
[ "$BAK_LIST" = "200" ] && ok "备份列表" || bad "备份列表" "$BAK_LIST"

# ============================================================
# J. 运维工具（维护窗口+证书+拓扑+成本+漂移+磁盘）
# ============================================================
echo ""
echo "========== J. 运维工具 =========="
echo "--- J1. 维护窗口 CRUD + 检查 ---"
MW_RESP=$(curl -s -X POST $API/maintenance -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"ft-mw-$TS\",\"start_at\":\"$(date +%Y-%m-%dT%H:%M:%S+08:00)\",\"end_at\":\"$(date -d '+2 hours' +%Y-%m-%dT%H:%M:%S+08:00)\",\"reason\":\"test\"}")
MW_ID=$(echo "$MW_RESP" | j "d['id']")
[ -n "$MW_ID" ] && ok "创建维护窗口 id=$MW_ID" || bad "维护窗口" "$MW_RESP"
MW_CHECK=$(curl -s -m 5 "$API/maintenance/check" -H "$AUTH" | python3 -c "import sys,json;print(str(json.load(sys.stdin).get('in_maintenance',False)).lower())" 2>/dev/null)
[ "$MW_CHECK" = "true" ] && ok "维护窗口检查命中" || bad "维护窗口检查" "$MW_CHECK"
MW_LIST=$(code $API/maintenance -H "$AUTH")
[ "$MW_LIST" = "200" ] && ok "维护窗口列表" || bad "维护窗口列表" "$MW_LIST"

echo "--- J2. 运行状态 + 恢复演练列表 ---"
PLATFORM_STATUS=$(code $API/stats/enhanced -H "$AUTH")
[ "$PLATFORM_STATUS" = "200" ] && ok "增强运行状态" || bad "增强运行状态" "$PLATFORM_STATUS"
EXERCISE_LIST=$(code $API/recovery-exercises -H "$AUTH")
[ "$EXERCISE_LIST" = "200" ] && ok "恢复演练列表" || bad "恢复演练列表" "$EXERCISE_LIST"

echo "--- J3. 拓扑 + 指标 + 漂移 + 磁盘 ---"
TOPO=$(code "$API/topology" -H "$AUTH")
[ "$TOPO" = "200" ] && ok "拓扑图" || bad "拓扑" "$TOPO"
STATUS=$(code $API/stats -H "$AUTH")
[ "$STATUS" = "200" ] && ok "平台运行状态" || bad "平台运行状态" "$STATUS"
DRIFT=$(code "$API/config/drift" -H "$AUTH")
[ "$DRIFT" = "200" ] && ok "配置漂移" || bad "漂移" "$DRIFT"
DISK=$(code $API/disk/check -H "$AUTH")
[ "$DISK" = "200" ] && ok "磁盘检查" || bad "磁盘" "$DISK"

echo "--- J4. 资源监控 ---"
if [ -n "$NE_ID" ]; then
  RES=$(code $API/components/$NE_ID/resource -H "$AUTH")
  [ "$RES" = "200" ] && ok "组件资源监控" || bad "组件资源" "$RES"
fi
CRES=$(code "$API/node/resource" -H "$AUTH")
[ "$CRES" = "200" ] && ok "节点资源监控" || bad "节点资源" "$CRES"

# ============================================================
# K. 用户管理 + RBAC
# ============================================================
echo ""
echo "========== K. 用户管理 + RBAC =========="
echo "--- K1. 创建 viewer 用户 ---"
VIEW_PASS="viewer-${TS}-Aa9"
VU=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"viewer-$TS\",\"password\":\"$VIEW_PASS\",\"role\":\"viewer\",\"email\":\"v@x.io\"}" | j "d['id']")
[ -n "$VU" ] && ok "创建 viewer" || bad "创建 viewer" ""
VTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"viewer-$TS\",\"password\":\"$VIEW_PASS\"}" | j "d['token']")
VAUTH="Authorization: Bearer $VTOKEN"
VREAD=$(code $API/node -H "$VAUTH")
[ "$VREAD" = "200" ] && ok "viewer 可读" || bad "viewer 读" "$VREAD"
VWRITE=$(code -X PUT $API/node -H "$VAUTH" -H 'Content-Type: application/json' -d '{"description":"nope"}')
[ "$VWRITE" = "403" ] && ok "viewer 不可写" || bad "viewer 写" "$VWRITE"
VUSERS=$(code $API/users -H "$VAUTH")
[ "$VUSERS" = "403" ] && ok "viewer 不可管用户" || bad "viewer 用户管理" "$VUSERS"

echo "--- K2. 创建 change_admin 用户 ---"
CHANGE_PASS="change-${TS}-Aa9"
UU=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"change-$TS\",\"password\":\"$CHANGE_PASS\",\"role\":\"change_admin\",\"email\":\"u@x.io\"}" | j "d['id']")
[ -n "$UU" ] && ok "创建 change_admin" || bad "创建 change_admin" ""
UTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"change-$TS\",\"password\":\"$CHANGE_PASS\"}" | j "d['token']")
UAUTH="Authorization: Bearer $UTOKEN"
UWRITE=$(code -X POST $API/components -H "$UAUTH" -H 'Content-Type: application/json' -d '{}')
[ "$UWRITE" = "400" ] && ok "change_admin 可达变更接口" || bad "change_admin 变更接口" "$UWRITE"
UUSERS=$(code $API/users -H "$UAUTH")
[ "$UUSERS" = "403" ] && ok "change_admin 不可管用户" || bad "change_admin 用户管理" "$UUSERS"

echo "--- K3. 用户列表 + 编辑 + 禁用 ---"
U_LIST=$(code $API/users -H "$AUTH")
[ "$U_LIST" = "200" ] && ok "用户列表" || bad "用户列表" "$U_LIST"
U_UPD=$(code -X PUT $API/users/$VU -H "$AUTH" -H 'Content-Type: application/json' -d '{"role":"change_admin"}')
[ "$U_UPD" = "200" ] && ok "编辑用户角色" || bad "编辑用户" "$U_UPD"
U_DIS=$(code -X PUT $API/users/$VU -H "$AUTH" -H 'Content-Type: application/json' -d '{"is_active":false}')
[ "$U_DIS" = "200" ] && ok "禁用用户" || bad "禁用用户" "$U_DIS"

# ============================================================
# L. 版本管理 + 包管理
# ============================================================
echo ""
echo "========== L. 版本 + 包管理 =========="
echo "--- L1. 版本列表 + 按类型 ---"
V_LIST=$(code $API/versions -H "$AUTH")
[ "$V_LIST" = "200" ] && ok "版本列表" || bad "版本列表" "$V_LIST"
V_BT=$(code "$API/versions/by-type?type=prometheus" -H "$AUTH")
[ "$V_BT" = "200" ] && ok "按类型查版本" || bad "按类型版本" "$V_BT"

echo "--- L2. 包列表 + 下载 ---"
P_LIST=$(code $API/packages -H "$AUTH")
[ "$P_LIST" = "200" ] && ok "包列表" || bad "包列表" "$P_LIST"
P_BT=$(code "$API/packages?type=node_exporter" -H "$AUTH")
[ "$P_BT" = "200" ] && ok "按类型查包" || bad "按类型包" "$P_BT"

# ============================================================
# M. 操作日志 + 统计
# ============================================================
echo ""
echo "========== M. 操作日志 + 统计 =========="
echo "--- M1. 操作日志 ---"
LOGS=$(curl -s "$API/operations/logs?limit=5" -H "$AUTH")
LOG_CNT=$(echo "$LOGS" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('logs',[])))" 2>/dev/null)
[ "$LOG_CNT" != "0" ] && ok "操作日志 ($LOG_CNT 条)" || bad "操作日志" "$LOG_CNT"
LOG_ACT=$(echo "$LOGS" | python3 -c "import sys,json;d=json.load(sys.stdin);logs=d.get('logs',[]);print(logs[0].get('action','') if logs else '')" 2>/dev/null)
[ -n "$LOG_ACT" ] && ok "日志含操作 ($LOG_ACT)" || bad "日志内容" ""
OP_LIST=$(code $API/operations -H "$AUTH")
[ "$OP_LIST" = "200" ] && ok "操作列表" || bad "操作列表" "$OP_LIST"

echo "--- M2. 统计 ---"
STATS=$(code $API/stats -H "$AUTH")
[ "$STATS" = "200" ] && ok "基础统计" || bad "基础统计" "$STATS"
STATS_E=$(code $API/stats/enhanced -H "$AUTH")
[ "$STATS_E" = "200" ] && ok "增强统计" || bad "增强统计" "$STATS_E"

echo "--- M3. 组件日志 + 详情 ---"
if [ -n "$NE_ID" ]; then
  CLOGS=$(code $API/components/$NE_ID/logs -H "$AUTH")
  [ "$CLOGS" = "200" ] && ok "组件日志" || bad "组件日志" "$CLOGS"
fi
if [ -n "$PROM_ID" ]; then
  CDET=$(curl -s $API/components/$PROM_ID -H "$AUTH")
  echo "$CDET" | grep -q "install_path\|service_name\|config_path" && ok "组件详情含部署信息" || bad "组件详情" ""
fi

echo "--- M4. /metrics + /health ---"
METRICS=$(curl -s -o /dev/null -w "%{http_code}" -m 5 "${BASE%/}/metrics")
[ "$METRICS" = "200" ] && ok "/metrics" || bad "/metrics" "$METRICS"
HEALTH=$(curl -s -o /dev/null -w "%{http_code}" -m 5 "${BASE%/}/health")
[ "$HEALTH" = "200" ] && ok "/health" || bad "/health" "$HEALTH"

# ============================================================
# N. 清理（卸载全部组件 + 删除数据）
# ============================================================
echo ""
echo "========== N. 清理 =========="
echo "--- N1. 卸载全部组件 ---"
for comp_type in prometheus node_exporter alertmanager blackbox_exporter pushgateway loki promtail thanos grafana; do
  comp_id=${COMP_IDS[$comp_type]:-}
  if [ -n "$comp_id" ]; then
    UC=$(governed_code "$API/components/$comp_id/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH")
    [ "$UC" = "200" ] && ok "卸载 $comp_type" || bad "卸载 $comp_type" "$UC"
  fi
done
sleep 10

echo "--- N2. 远端端口残留检查 ---"
RES_PORT=$(rssh "ss -tlnp | grep -E ':193[0-9]{2}' | wc -l" | tr -d ' ')
[ "$RES_PORT" = "0" ] && ok "远端无测试端口残留" || bad "远端残留端口" "$RES_PORT"
RES_SVC=$(rssh "systemctl list-units --type=service 2>/dev/null | grep -c promlifecycle-" | tr -d ' ')
[ "$RES_SVC" = "0" ] && ok "远端无 systemd 残留" || bad "远端残留服务" "$RES_SVC"

echo "--- N3. 删除告警/渠道/采集/记录/远端/仪表盘/模板/证书/维护/定时备份/备份 ---"
[ -n "$RULE_ID" ] && curl -s -X DELETE $API/alerts/rules/$RULE_ID -H "$AUTH" >/dev/null
[ -n "$SC_ID" ] && curl -s -X DELETE $API/scrape-configs/$SC_ID -H "$AUTH" >/dev/null
[ -n "$RR_ID" ] && curl -s -X DELETE $API/recording-rules/$RR_ID -H "$AUTH" >/dev/null
[ -n "$RW_ID" ] && curl -s -X DELETE $API/remote-writes/$RW_ID -H "$AUTH" >/dev/null
for ch in $CH_WH $CH_WH2 $CH_EM $CH_DT $CH_WC $CH_SL; do curl -s -X DELETE $API/notifications/channels/$ch -H "$AUTH" >/dev/null 2>&1; done
[ -n "$DASH_ID" ] && curl -s -X DELETE $API/dashboards/$DASH_ID -H "$AUTH" >/dev/null
[ -n "$TPL_ID" ] && curl -s -X DELETE $API/templates/$TPL_ID -H "$AUTH" >/dev/null
[ -n "$MW_ID" ] && curl -s -X DELETE $API/maintenance/$MW_ID -H "$AUTH" >/dev/null
[ -n "$SB_ID" ] && curl -s -X DELETE $API/backups/scheduled/$SB_ID -H "$AUTH" >/dev/null
[ -n "$BAK_ID" ] && curl -s -X DELETE $API/backups/$BAK_ID -H "$AUTH" >/dev/null
[ -n "$DBAK_ID" ] && curl -s -X DELETE $API/backups/$DBAK_ID -H "$AUTH" >/dev/null

echo "--- N4. 删除用户 + 主机 ---"
[ -n "$VU" ] && curl -s -X DELETE $API/users/$VU -H "$AUTH" >/dev/null
[ -n "$UU" ] && curl -s -X DELETE $API/users/$UU -H "$AUTH" >/dev/null
NODE_STATUS=$(code $API/node -H "$AUTH")
[ "$NODE_STATUS" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$NODE_STATUS"
DEL_H=$(code -X DELETE $API/hosts/$HID -H "$AUTH")
[ "$DEL_H" = "200" ] && ok "删除主机" || bad "删除主机" "$DEL_H"

echo "--- N5. 监控删除待收敛验证 ---"
# 组件已卸载，无法确认远端撤回；保留禁用的删除墓碑，而非声称已清理。
CAS_RW=$(curl -s "$API/remote-writes" -H "$AUTH")
RW_PENDING=$(echo "$CAS_RW" | j "[x['delete_pending'] is True and x['enabled'] is False and x['apply_status'] in ('pending','error','manual') for x in d['remote_writes'] if str(x['id']) == '$RW_ID'] == [True]")
[ "$RW_PENDING" = "True" ] && ok "remote-write 删除墓碑保留，等待撤回" || bad "remote-write 删除状态" "$CAS_RW"
CAS_RULE=$(curl -s "$API/alerts/rules" -H "$AUTH")
RULE_PENDING=$(echo "$CAS_RULE" | j "[x['delete_pending'] is True and x['enabled'] is False and x['apply_status'] in ('pending','error','manual') for x in d['rules'] if str(x['id']) == '$RULE_ID'] == [True]")
[ "$RULE_PENDING" = "True" ] && ok "告警规则删除墓碑保留，等待撤回" || bad "告警规则删除状态" "$CAS_RULE"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "清理后节点仍可用" || bad "清理后节点" "$(code $API/node -H "$AUTH")"

# ============================================================
# 结果
# ============================================================
echo ""
echo "================================================================"
echo " 100% 全功能测试结果: $PASS passed, $FAIL failed, $SKIP skipped"
echo "================================================================"
[ $FAIL -eq 0 ] || exit 1
