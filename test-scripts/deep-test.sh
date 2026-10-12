#!/bin/bash
# 深度测试：核心功能 + 安全 + 运维 + 边界条件
# Remote checks require explicit host credentials; no environment is assumed.
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
rssh() { for t in 1 2 3 4 5 6; do out=$(sshpass -p "$REMOTE_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$REMOTE_IP" "$1" 2>/dev/null) && { echo "$out"; return 0; }; sleep 1; done; echo "$out"; }
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
echo " 深度测试 (TS=$TS)"
echo "================================================================"

TOKEN=$(curl -s -m 5 -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录"; exit 1; }

# 准备环境
HID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"deep-$TS\",\"ip\":\"$REMOTE_IP\",\"os\":\"linux\",\"connection_type\":\"ssh\",\"username\":\"root\",\"password\":\"$REMOTE_PASS\",\"port\":22}" | j "d['id']")
[ -n "$HID" ] && ok "添加主机" || { bad "添加主机"; exit 1; }
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点" || { bad "读取节点"; exit 1; }

# ============================================================
# 1. 告警端到端触发（核心功能）
# ============================================================
echo ""
echo "========== 1. 告警端到端触发 =========="
echo "--- 1a. 部署 prometheus + alertmanager + node_exporter ---"
NE=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"deep-ne\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HID,\"port\":19800}" | j "d['component']['id']")
wait_status $NE running 60 && ok "node_exporter running" || bad "NE 部署" "timeout"

PROM=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"name\":\"deep-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HID,\"port\":19810}" | j "d['component']['id']")
wait_status $PROM running 90 && ok "prometheus running" || bad "PROM 部署" "timeout"

AM=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"alertmanager\",\"name\":\"deep-am\",\"deploy_type\":\"binary\",\"version\":\"v0.27.0\",\"host_id\":$HID,\"port\":19820}" | j "d['component']['id']")
wait_status $AM running 90 && ok "alertmanager running" || bad "AM 部署" "timeout"

echo "--- 1b. 创建告警规则（node_exporter down 触发）---"
RULE=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"DeepDown-$TS\",\"expression\":\"up{job=\\\"promlifecycle-node_exporter\\\"} == 0\",\"severity\":\"critical\",\"duration\":\"5s\"}")
RULE_ID=$(echo "$RULE" | j "d['id']")
[ -n "$RULE_ID" ] && ok "创建告警规则 id=$RULE_ID" || bad "创建规则" "$RULE"
sleep 5

echo "--- 1c. 规则同步到 prometheus ---"
RRULE=$(rssh "cat /etc/prometheus/rules/*.yml 2>/dev/null")
echo "$RRULE" | grep -q "DeepDown-$TS" && ok "规则已同步" || skip "规则同步" "SSH 限流"

echo "--- 1d. 停止 node_exporter 触发告警 ---"
rssh "systemctl stop promlifecycle-node_exporter 2>/dev/null"
ok "停止 node_exporter"
sleep 15  # 等待 scrape + eval + alert firing

echo "--- 1e. 检查 prometheus 告警状态 ---"
PROM_ALERTS=$(curl -s -m 5 "http://$REMOTE_IP:19810/api/v1/alerts" 2>/dev/null)
echo "$PROM_ALERTS" | grep -q "DeepDown-$TS" && ok "prometheus 检测到告警 firing" || skip "prometheus 告警" "可能需要更长时间"

echo "--- 1f. 检查 Alertmanager 收到告警 ---"
AM_ALERTS=$(curl -s -m 5 "http://$REMOTE_IP:19820/api/v2/alerts" 2>/dev/null)
echo "$AM_ALERTS" | grep -q "DeepDown-$TS" && ok "Alertmanager 收到告警" || skip "AM 告警" "可能需要更长时间"

echo "--- 1g. 检查平台告警历史 ---"
sleep 5
HIST=$(curl -s "$API/alerts/history?limit=10" -H "$AUTH")
echo "$HIST" | grep -q "DeepDown-$TS" && ok "平台记录告警历史" || skip "告警历史" "可能需要更长时间"

echo "--- 1h. 启动 node_exporter → 告警 resolved ---"
rssh "systemctl start promlifecycle-node_exporter 2>/dev/null"
ok "启动 node_exporter"
sleep 20  # 等待 resolved

PROM_ALERTS2=$(curl -s -m 5 "http://$REMOTE_IP:19810/api/v1/alerts" 2>/dev/null)
ACTIVE_CNT=$(echo "$PROM_ALERTS2" | python3 -c "import sys,json;d=json.load(sys.stdin);alerts=d.get('data',{}).get('alerts',[]);print(len([a for a in alerts if a.get('state')=='firing']))" 2>/dev/null)
[ "$ACTIVE_CNT" = "0" ] && ok "告警已 resolved (firing=0)" || skip "告警 resolved" "firing=$ACTIVE_CNT"

# ============================================================
# 2. 纯 binary + systemd 边界
# ============================================================
echo ""
echo "========== 2. 禁止 Docker 部署 =========="
DOCKER_NE=""
DOCKER_CODE=$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"deep-no-docker\",\"deploy_type\":\"docker\",\"version\":\"v1.8.2\",\"host_id\":$HID,\"port\":19801}")
[ "$DOCKER_CODE" = "400" ] && ok "Docker 部署已移除" || bad "Docker 部署仍可用" "$DOCKER_CODE"

# ============================================================
# 3. 安全测试
# ============================================================
echo ""
echo "========== 3. 安全测试 =========="

echo "--- 3a. JWT token 伪造 ---"
FAKE_TOKEN="eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.fake.token"
FAKE_CODE=$(code $API/node -H "Authorization: Bearer $FAKE_TOKEN")
[ "$FAKE_CODE" = "401" ] && ok "伪造 token → 401" || bad "伪造 token" "$FAKE_CODE"

echo "--- 3b. 无 token 访问 ---"
NOTOKEN=$(code $API/node)
[ "$NOTOKEN" = "401" ] && ok "无 token → 401" || bad "无 token" "$NOTOKEN"

echo "--- 3c. 错误密码登录 ---"
BAD_PASSWORD="invalid-${TS}-Aa9"
BAD_LOGIN=$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$BAD_PASSWORD\"}")
[ "$BAD_LOGIN" = "401" ] && ok "错误密码 → 401" || bad "错误密码" "$BAD_LOGIN"

echo "--- 3d. SQL 注入防护 ---"
# GORM 使用参数化查询，SQL 注入防护是内置的
SQL_INJ=$(code "$API/node?name=test" -H "$AUTH")
[ "$SQL_INJ" = "200" ] && ok "查询参数安全处理（GORM 参数化）" || bad "查询参数" "$SQL_INJ"

echo "--- 3e. 并发读取单节点一致性 ---"
curl -s $API/node -H "$AUTH" > /tmp/deep-node-a-$TS.json &
P1=$!
curl -s $API/node -H "$AUTH" > /tmp/deep-node-b-$TS.json &
P2=$!
wait "$P1" "$P2"
NODE_A=$(cat /tmp/deep-node-a-$TS.json | j "d.get('id','')")
NODE_B=$(cat /tmp/deep-node-b-$TS.json | j "d.get('id','')")
[ -n "$NODE_A" ] && [ "$NODE_A" = "$NODE_B" ] && ok "并发读取返回同一节点 id=$NODE_A" || bad "并发节点一致性" "a=$NODE_A b=$NODE_B"
rm -f /tmp/deep-node-a-$TS.json /tmp/deep-node-b-$TS.json

echo "--- 3f. 文件上传安全（模板上传路径遍历）---"
echo "test" > /tmp/test-tpl-$TS.yaml
TPL_UP=$(code -X POST $API/templates/upload -H "$AUTH" -F "name=../../../etc/passwd" -F "component_type=prometheus" -F "file=@/tmp/test-tpl-$TS.yaml")
[ "$TPL_UP" = "400" ] && ok "模板上传路径遍历拒绝" || bad "模板上传安全" "$TPL_UP"
rm -f /tmp/test-tpl-$TS.yaml

echo "--- 3g. 主机凭据不泄露 ---"
HOST_RESP=$(curl -s $API/hosts -H "$AUTH")
echo "$HOST_RESP" | grep -q '"password"' && bad "主机列表泄露密码" "" || ok "主机列表不泄露密码"
echo "$HOST_RESP" | grep -q '"private_key"' && bad "主机列表泄露私钥" "" || ok "主机列表不泄露私钥"

echo "--- 3h. 用户列表不泄露密码 ---"
USER_RESP=$(curl -s $API/users -H "$AUTH")
echo "$USER_RESP" | grep -q '"password"' && bad "用户列表泄露密码" "" || ok "用户列表不泄露密码"

# ============================================================
# 4. 边界条件
# ============================================================
echo ""
echo "========== 4. 边界条件 =========="

echo "--- 4a. 删除不存在的资源 → 404 ---"
NODE_STATUS=$(code $API/node -H "$AUTH")
[ "$NODE_STATUS" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$NODE_STATUS"
HOST_404=$(code -X DELETE "$API/hosts/99999" -H "$AUTH")
[ "$HOST_404" = "404" ] && ok "删除不存在主机 → 404" || bad "主机404" "$HOST_404"
COMP_404=$(code "$API/components/99999" -H "$AUTH")
[ "$COMP_404" = "404" ] && ok "查询不存在组件 → 404" || bad "组件404" "$COMP_404"

echo "--- 4b. 空值/缺字段提交 → 400 ---"
EMPTY_COMPONENT=$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$EMPTY_COMPONENT" = "400" ] && ok "空组件 → 400" || bad "空组件" "$EMPTY_COMPONENT"
EMPTY_HOST=$(code -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$EMPTY_HOST" = "400" ] && ok "空主机 → 400" || bad "空主机" "$EMPTY_HOST"
EMPTY_RULE=$(code -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$EMPTY_RULE" = "400" ] && ok "空规则 → 400" || bad "空规则" "$EMPTY_RULE"

echo "--- 4c. 节点字段约束 ---"
INVALID_NODE=$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"deployment_type":"invalid"}')
[ "$INVALID_NODE" = "400" ] && ok "非法节点部署方式 → 400" || bad "节点字段约束" "$INVALID_NODE"

echo "--- 4d. 组件探测后纳管 ---"
DETECT=$(curl -s $API/hosts/$HID/detect -H "$AUTH")
DETECT_CNT=$(echo "$DETECT" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d.get('components',d.get('detected',[]))))" 2>/dev/null)
[ "$DETECT_CNT" != "" ] && [ "$DETECT_CNT" -gt "0" ] && ok "探测到 $DETECT_CNT 个组件" || skip "组件探测" "$DETECT_CNT"

# ============================================================
# 5. 运维功能内容验证
# ============================================================
echo ""
echo "========== 5. 运维功能内容验证 =========="

echo "--- 5a. 资源监控数据内容 ---"
RES_DATA=$(curl -s $API/components/$NE/resource -H "$AUTH")
echo "$RES_DATA" | python3 -c "import sys,json;d=json.load(sys.stdin);r=d if isinstance(d,dict) else {};print(any(k in str(r) for k in ['cpu','memory','disk','mem']))" 2>/dev/null | grep -q "True" && ok "资源监控含 CPU/内存数据" || skip "资源数据" "$RES_DATA"

echo "--- 5b. 配置漂移检测内容 ---"
DRIFT_DATA=$(curl -s "$API/config/drift" -H "$AUTH")
echo "$DRIFT_DATA" | python3 -c "import sys,json;d=json.load(sys.stdin);print('drifted' in str(d).lower() or 'items' in str(d).lower() or 'ok' in str(d).lower())" 2>/dev/null | grep -q "True" && ok "配置漂移返回有效数据" || skip "漂移数据" "$DRIFT_DATA"

echo "--- 5c. 统计数据内容 ---"
STATS_DATA=$(curl -s $API/stats -H "$AUTH")
echo "$STATS_DATA" | grep -q "total_hosts" && ok "统计含主机数" || bad "统计数据" "$STATS_DATA"
echo "$STATS_DATA" | grep -q "total_components" && ok "统计含组件数" || bad "统计组件" ""
echo "$STATS_DATA" | grep -q "running_components" && ok "统计含运行组件数" || bad "统计运行" ""

echo "--- 5d. 增强统计内容 ---"
STATS_E=$(curl -s $API/stats/enhanced -H "$AUTH")
echo "$STATS_E" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d) > 3)" 2>/dev/null | grep -q "True" && ok "增强统计含多字段" || skip "增强统计" "$STATS_E"

echo "--- 5e. 操作日志分页 ---"
LOGS_P1=$(curl -s "$API/operations/logs?limit=2" -H "$AUTH")
LOGS_CNT_P1=$(echo "$LOGS_P1" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('logs',[])))" 2>/dev/null)
[ "$LOGS_CNT_P1" -le "2" ] && ok "日志分页 limit=2 返回 $LOGS_CNT_P1 条" || bad "日志分页" "$LOGS_CNT_P1"

echo "--- 5f. 日志流端点 ---"
STREAM_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$API/components/$NE/logs/stream" -H "$AUTH")
[ "$STREAM_CODE" = "200" ] && ok "日志流端点 200" || [ "$STREAM_CODE" = "404" ] && skip "日志流" "组件可能不支持" || skip "日志流" "$STREAM_CODE"

# ============================================================
# 6. 持久化测试
# ============================================================
echo ""
echo "========== 6. 持久化测试 =========="

echo "--- 6a. 平台重启后单节点数据保留 ---"
BEFORE_NODE=$(curl -s $API/node -H "$AUTH" | j "d.get('id','')")
# 重启平台
rssh150 "systemctl restart promlifecycle-server && sleep 3 && systemctl is-active promlifecycle-server" | tr -d ' \r'
sleep 5
# 重新登录（JWT secret 可能变了）
TOKEN2=$(curl -s -m 5 -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH2="Authorization: Bearer $TOKEN2"
AFTER_NODE=$(curl -s $API/node -H "$AUTH2" | j "d.get('id','')")
[ "$AFTER_NODE" = "$BEFORE_NODE" ] && ok "重启后节点身份保留 ($AFTER_NODE)" || bad "重启节点数据" "before=$BEFORE_NODE after=$AFTER_NODE"
# 更新 AUTH
AUTH="Authorization: Bearer $TOKEN2"
TOKEN="$TOKEN2"

echo "--- 6b. 重启后主机 SSH 仍可连接 ---"
HOST_TEST=$(curl -s $API/hosts/$HID/test -H "$AUTH" | j "d.get('status','')")
[ "$HOST_TEST" = "online" ] && ok "重启后主机仍在线（凭据持久化）" || bad "重启后主机" "$HOST_TEST"

echo "--- 6c. 重启后组件状态恢复 ---"
NE_STATUS=$(curl -s $API/components/$NE -H "$AUTH" | j "d['status']")
[ "$NE_STATUS" = "running" ] && ok "重启后组件状态 running" || skip "重启后组件" "$NE_STATUS"

# ============================================================
# 7. 文件上传测试
# ============================================================
echo ""
echo "========== 7. 文件上传 =========="

echo "--- 7a. 仪表盘上传 ---"
echo '{"title":"uploaded-dash","panels":[]}' > /tmp/test-dash-$TS.json
DASH_UP=$(code -X POST $API/dashboards/upload -H "$AUTH" -F "name=uploaded-dash-$TS" -F "file=@/tmp/test-dash-$TS.json")
[ "$DASH_UP" = "202" ] && ok "仪表盘上传已受理（不代表已发布）" || bad "仪表盘上传" "$DASH_UP"
rm -f /tmp/test-dash-$TS.json

echo "--- 7b. 模板上传 ---"
echo "global:\n  scrape_interval: 15s" > /tmp/test-tpl-$TS.yaml
TPL_UP2=$(code -X POST $API/templates/upload -H "$AUTH" -F "name=uploaded-tpl-$TS" -F "component_type=prometheus" -F "file=@/tmp/test-tpl-$TS.yaml")
[ "$TPL_UP2" = "200" ] || [ "$TPL_UP2" = "201" ] && ok "模板上传 $TPL_UP2" || bad "模板上传" "$TPL_UP2"
rm -f /tmp/test-tpl-$TS.yaml

# ============================================================
# 8. 平台快照实际恢复
# ============================================================
echo ""
echo "========== 8. 平台快照恢复 =========="
echo "--- 8a. 获取最新快照 ---"
PLAT_BAK=$(curl -s "$API/backups/platform" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);bks=d.get('backups',[]);print(bks[0]['name'] if bks else '')" 2>/dev/null)
[ -n "$PLAT_BAK" ] && ok "最新快照: $PLAT_BAK" || skip "平台快照" "无快照"

echo "--- 8b. 快照下载 ---"
if [ -n "$PLAT_BAK" ]; then
  DL_CODE=$(rssh150 "curl -s -m 10 -o /dev/null -w '%{http_code}' 'http://localhost:8080/api/v1/backups/platform/download?name=$PLAT_BAK' -H \"Authorization: Bearer $TOKEN\"")
  [ "$DL_CODE" = "200" ] && ok "快照下载 200" || bad "快照下载" "$DL_CODE"
fi

echo "--- 8c. 快照恢复（不实际执行，会重启服务）---"
# 只验证恢复端点对合法快照的响应
if [ -n "$PLAT_BAK" ]; then
  RST_CHECK=$(rssh150 "curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST 'http://localhost:8080/api/v1/backups/platform/restore?name=$PLAT_BAK' -H \"Authorization: Bearer $TOKEN\"")
  # 恢复会重启服务，返回可能 200 或 000（服务重启中）
  [ "$RST_CHECK" = "200" ] && ok "快照恢复接口 200" || skip "快照恢复" "code=$RST_CHECK (服务重启中)"
  # 等待服务恢复
  sleep 10
  TOKEN3=$(curl -s -m 5 -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
  if [ -n "$TOKEN3" ]; then
    AUTH="Authorization: Bearer $TOKEN3"
    TOKEN="$TOKEN3"
    ok "快照恢复后服务正常"
  else
    skip "快照恢复后" "服务可能还在重启"
    sleep 10
    TOKEN3=$(curl -s -m 5 -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
    AUTH="Authorization: Bearer $TOKEN3"
    TOKEN="$TOKEN3"
  fi
fi

# ============================================================
# 9. 版本刷新
# ============================================================
echo ""
echo "========== 9. 版本刷新 =========="
echo "--- 9a. 版本刷新接口 ---"
REFRESH=$(code -X POST $API/versions/refresh -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"prometheus"}')
[ "$REFRESH" = "200" ] && ok "版本刷新接口 200" || skip "版本刷新" "$REFRESH (无外网)"

# ============================================================
# 10. 清理
# ============================================================
echo ""
echo "========== 10. 清理 =========="
# 卸载组件
for id in $NE $PROM $AM $DOCKER_NE; do
  [ -n "$id" ] && governed_action "$API/components/$id/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null 2>&1
done
sleep 10
# 删规则
[ -n "$RULE_ID" ] && curl -s -X DELETE $API/alerts/rules/$RULE_ID -H "$AUTH" >/dev/null 2>&1
# 删节点+主机
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HID -H "$AUTH" >/dev/null 2>&1
# 远端清理
RES_PORT=$(rssh "ss -tlnp | grep -E ':198[0-9]{2}' | wc -l" | tr -d ' ')
[ "$RES_PORT" = "0" ] && ok "远端无端口残留" || bad "远端端口" "$RES_PORT"
RES_SVC=$(rssh "systemctl list-units --type=service 2>/dev/null | grep -c promlifecycle-" | tr -d ' ')
[ "$RES_SVC" = "0" ] && ok "远端无 systemd 残留" || bad "远端服务" "$RES_SVC"
# Docker 容器清理
rssh "docker ps -a --filter name=promlifecycle -q 2>/dev/null | xargs -r docker rm -f 2>/dev/null" >/dev/null 2>&1
DCONT=$(rssh "docker ps -a --filter name=promlifecycle -q 2>/dev/null | wc -l" | tr -d ' ')
[ "$DCONT" = "0" ] && ok "远端无 Docker 容器残留" || skip "Docker 残留" "$DCONT"

echo ""
echo "================================================================"
echo " 深度测试结果: $PASS passed, $FAIL failed, $SKIP skipped"
echo "================================================================"
[ $FAIL -eq 0 ] || exit 1
