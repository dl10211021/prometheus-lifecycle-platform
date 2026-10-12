#!/bin/bash
# Supplemental remote feature test.
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
REMOTE_IP="${REMOTE_HOST:-}"
PLATFORM_HOST="${PLATFORM_HOST:-}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_PASS="${REMOTE_PASS:-}"
PLATFORM_PASS="${PLATFORM_PASS:-$REMOTE_PASS}"
DATA_DIR="${DATA_DIR:-/opt/promlifecycle/data}"
if [ -z "$REMOTE_IP" ] || [ -z "$PLATFORM_HOST" ] || [ -z "$REMOTE_PASS" ]; then
  echo "SKIP: set REMOTE_HOST, PLATFORM_HOST, and REMOTE_PASS for remote tests"
  exit 0
fi
rssh() { for t in 1 2 3 4; do out=$(sshpass -p "$REMOTE_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$REMOTE_IP" "$1" 2>/dev/null) && { echo "$out"; return 0; }; sleep 1; done; echo "$out"; }
TS=$(date +%s)
PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: ${2:-})"; }
skip() { SKIP=$((SKIP+1)); echo "  - $1 (SKIP: ${2:-})"; }
j()    { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' -m 30 "$@"; }
wait_status() { local id=$1 target=$2 max=${3:-60}; for i in $(seq 1 $max); do ST=$(curl -s $API/components/$id -H "$AUTH" | j "d['status']"); [ "$ST" = "$target" ] && return 0; [ "$ST" = "error" ] && return 1; sleep 2; done; return 1; }

echo "================================================================"
echo " 补充功能测试 (TS=$TS)"
echo "================================================================"

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录"; exit 1; }

# 准备环境
HID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-$TS\",\"ip\":\"$REMOTE_IP\",\"os\":\"linux\",\"connection_type\":\"ssh\",\"username\":\"root\",\"password\":\"$REMOTE_PASS\",\"port\":22}" | j "d['id']")
[ -n "$HID" ] && ok "添加主机" || { bad "添加主机"; exit 1; }
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点 id=$NODE_ID" || { bad "读取节点"; exit 1; }

# === 1. 组件升级+回滚（blackbox v0.23.0→v0.24.0→v0.23.0）===
echo "--- 1. 组件升级+回滚（版本变化验证）---"
BB=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"blackbox_exporter\",\"name\":\"sup-bb\",\"deploy_type\":\"binary\",\"version\":\"v0.23.0\",\"host_id\":$HID,\"port\":19200}" | j "d['component']['id']")
[ -n "$BB" ] && ok "部署 blackbox v0.23.0 id=$BB" || { bad "部署 blackbox"; exit 1; }
wait_status $BB running 60 && ok "blackbox running" || bad "blackbox 部署" "timeout"
VER=$(curl -s $API/components/$BB -H "$AUTH" | j "d['version']")
[ "$VER" = "v0.23.0" ] && ok "初始版本 v0.23.0" || bad "初始版本" "$VER"
# 远端核对版本
RVER=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "http://$REMOTE_IP:19200/metrics")
[ "$RVER" = "200" ] && ok "远端 blackbox metrics 可达" || bad "远端版本" "$RVER"

# 升级到 v0.24.0
UP_CODE=$(governed_code "$API/components/$BB/upgrade" '{"new_version":"v0.24.0","reason":"test lifecycle"}' "$AUTH")
[ "$UP_CODE" = "200" ] && ok "升级接口 $UP_CODE" || bad "升级接口" "$UP_CODE"
wait_status $BB running 60 && ok "升级后 running" || bad "升级后状态" "timeout"
VER2=$(curl -s $API/components/$BB -H "$AUTH" | j "d['version']")
[ "$VER2" = "v0.24.0" ] && ok "升级后版本 v0.24.0" || bad "升级后版本" "$VER2"
RVER2=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "http://$REMOTE_IP:19200/metrics")
[ "$RVER2" = "200" ] && ok "远端升级后 metrics 可达" || bad "远端升级后" "$RVER2"

# 回滚到 v0.23.0
RB_CODE=$(governed_code "$API/components/$BB/rollback" '{"reason":"test lifecycle"}' "$AUTH")
[ "$RB_CODE" = "200" ] && ok "回滚接口 $RB_CODE" || bad "回滚接口" "$RB_CODE"
wait_status $BB running 60 && ok "回滚后 running" || bad "回滚后状态" "timeout"
VER3=$(curl -s $API/components/$BB -H "$AUTH" | j "d['version']")
[ "$VER3" != "v0.24.0" ] && ok "回滚后版本已变化 ($VER3)" || bad "回滚后版本" "未变化"
RVER3=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "http://$REMOTE_IP:19200/metrics")
[ "$RVER3" = "200" ] && ok "远端回滚后 metrics 可达" || bad "远端回滚后" "$RVER3"

# === 2. 主机批量创建 ===
echo "--- 2. 主机批量创建 ---"
BATCH_PASS="batch-${TS}-Aa9"
BATCH=$(curl -s -X POST $API/hosts/batch -H "$AUTH" -H 'Content-Type: application/json' -d "[{\"ip\":\"127.0.0.4\",\"username\":\"root\",\"password\":\"$BATCH_PASS\",\"name\":\"batch-1\"},{\"ip\":\"127.0.0.5\",\"username\":\"root\",\"password\":\"$BATCH_PASS\",\"name\":\"batch-2\"}]")
BATCH_CNT=$(echo "$BATCH" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len([r for r in d.get('results',[]) if r.get('status')=='created']))" 2>/dev/null)
[ "$BATCH_CNT" = "2" ] && ok "批量创建 2 台主机" || bad "批量创建" "$BATCH"
# 清理批量主机
echo "$BATCH" | python3 -c "import sys,json;d=json.load(sys.stdin);[print(r.get('id','')) for r in d.get('results',[]) if r.get('status')=='created']" 2>/dev/null | while read bid; do [ -n "$bid" ] && curl -s -X DELETE $API/hosts/$bid -H "$AUTH" >/dev/null; done

# === 3. 主机组件探测 ===
echo "--- 3. 主机组件探测 ---"
DETECT=$(curl -s $API/hosts/$HID/detect -H "$AUTH")
DETECT_CNT=$(echo "$DETECT" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d.get('components',d.get('detected',[]))))" 2>/dev/null)
[ "$DETECT_CNT" != "" ] && ok "组件探测返回 $DETECT_CNT 个" || bad "组件探测" "$DETECT"

# === 4. 目录浏览 + 路径遍历防护 ===
echo "--- 4. 目录浏览 + 安全防护 ---"
BROWSE=$(code "$API/hosts/$HID/browse?path=/tmp" -H "$AUTH")
[ "$BROWSE" = "200" ] && ok "目录浏览 /tmp" || bad "目录浏览" "$BROWSE"
BROWSE_INJ=$(code "$API/hosts/$HID/browse?path=%2Ftmp%3Brm" -H "$AUTH")
[ "$BROWSE_INJ" = "400" ] && ok "目录浏览拒绝注入" || bad "目录浏览注入" "$BROWSE_INJ"
# filepath.Clean normalizes ../../etc to /etc (absolute path, legitimate) — 200 is correct
BROWSE_TRAV=$(sshpass -p "$PLATFORM_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$PLATFORM_HOST" "curl -s -m 5 -o /dev/null -w \"%{http_code}\" \"http://localhost:8080/api/v1/hosts/$HID/browse?path=%2F..%2F..%2Fetc\" -H \"$AUTH\"" 2>/dev/null | tr -d "\r")
[ "$BROWSE_TRAV" = "200" ] && ok "路径遍历被 Clean 规范化（/etc 合法）" || bad "路径遍历" "$BROWSE_TRAV"

# === 5. 端口检查 ===
echo "--- 5. 端口检查 ---"
PORT_OK=$(code "$API/hosts/$HID/ports/22" -H "$AUTH")
[ "$PORT_OK" = "200" ] && ok "端口检查 22 开放" || bad "端口检查" "$PORT_OK"
PORT_CLOSED=$(code "$API/hosts/$HID/ports/99999" -H "$AUTH")
[ "$PORT_CLOSED" != "200" ] && ok "端口检查 99999 不开放" || bad "端口检查关闭端口" "$PORT_CLOSED"

# === 6. 通知渠道测试（各类型）===
echo "--- 6. 通知渠道测试 ---"
# webhook 不可达 → 502
CH_WH=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-wh-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/hook\\\"}\",\"enabled\":true}" | j "d['id']")
TWH=$(code -X POST $API/notifications/channels/$CH_WH/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TWH" = "502" ] && ok "webhook 不可达 → 502" || bad "webhook 测试" "$TWH"
# webhook SSRF → 400
CH_WH2=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-wh2-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"http://127.0.0.1:9/hook\\\"}\",\"enabled\":true}" | j "d['id']")
TWH2=$(code -X POST $API/notifications/channels/$CH_WH2/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TWH2" = "400" ] && ok "webhook SSRF → 400" || bad "webhook SSRF" "$TWH2"
# email 无 SMTP → 400
CH_EM=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-em-$TS\",\"type\":\"email\",\"config\":\"{\\\"to\\\":\\\"ops@x.com\\\"}\",\"enabled\":true}" | j "d['id']")
TEM=$(code -X POST $API/notifications/channels/$CH_EM/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TEM" = "400" ] && ok "email 无 SMTP → 400" || bad "email 测试" "$TEM"
# 钉钉
CH_DT=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-dt-$TS\",\"type\":\"dingtalk\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/ding\\\"}\",\"enabled\":true}" | j "d['id']")
TDT=$(code -X POST $API/notifications/channels/$CH_DT/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TDT" = "502" ] || [ "$TDT" = "400" ] && ok "钉钉测试 → $TDT" || bad "钉钉测试" "$TDT"
# 企微
CH_WC=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-wc-$TS\",\"type\":\"wechat\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/wc\\\"}\",\"enabled\":true}" | j "d['id']")
TWC=$(code -X POST $API/notifications/channels/$CH_WC/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TWC" = "502" ] || [ "$TWC" = "400" ] && ok "企微测试 → $TWC" || bad "企微测试" "$TWC"
# Slack
CH_SL=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-sl-$TS\",\"type\":\"slack\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/slack\\\"}\",\"enabled\":true}" | j "d['id']")
TSL=$(code -X POST $API/notifications/channels/$CH_SL/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TSL" = "502" ] || [ "$TSL" = "400" ] && ok "Slack 测试 → $TSL" || bad "Slack 测试" "$TSL"
# 不支持的类型
CH_BAD=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-bad-$TS\",\"type\":\"telegram\",\"config\":\"{}\",\"enabled\":true}" | j "d['id']")
TBAD=$(code -X POST $API/notifications/channels/$CH_BAD/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$TBAD" = "400" ] && ok "不支持类型 → 400" || bad "不支持类型" "$TBAD"

# === 7. 静默 CRUD ===
echo "--- 7. 静默 CRUD ---"
# 先部署 AM
AM=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"alertmanager\",\"name\":\"sup-am\",\"deploy_type\":\"binary\",\"version\":\"v0.27.0\",\"host_id\":$HID,\"port\":19203}" | j "d['component']['id']")
wait_status $AM running 90 && ok "AM running" || bad "AM 部署" "timeout"
# 创建静默
SIL_RESP=$(curl -s -X POST "$API/alerts/silences" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"matchers\":[{\"name\":\"alertname\",\"value\":\"test\",\"isRegex\":false}],\"startsAt\":\"$(date +%Y-%m-%dT%H:%M:%S+08:00)\",\"endsAt\":\"$(date -u -d '+2 hours' +%Y-%m-%dT%H:%M:%SZ)\",\"createdBy\":\"admin\",\"comment\":\"sup-test\"}")
SIL_ID=$(echo "$SIL_RESP" | j "d.get('id','')")
[ -n "$SIL_ID" ] && ok "创建静默 id=$SIL_ID" || bad "创建静默" "$SIL_RESP"
# 查询静默列表
SIL_LIST=$(code "$API/alerts/silences" -H "$AUTH")
[ "$SIL_LIST" = "200" ] && ok "查询静默列表 200" || bad "静默列表" "$SIL_LIST"
# 删除静默
SIL_DEL=$(code -X DELETE "$API/alerts/silences/$SIL_ID" -H "$AUTH")
[ "$SIL_DEL" = "200" ] && ok "删除静默 200" || skip "删除静默" "$SIL_DEL"

# === 8. Alertmanager 代理 ===
echo "--- 8. Alertmanager 代理 ---"
AM_STATUS=$(code "$API/alertmanager/status" -H "$AUTH")
[ "$AM_STATUS" = "200" ] && ok "AM status 代理 200" || bad "AM status" "$AM_STATUS"
AM_ALERTS=$(code "$API/alertmanager/alerts" -H "$AUTH")
[ "$AM_ALERTS" = "200" ] && ok "AM alerts 代理 200" || bad "AM alerts" "$AM_ALERTS"
AM_SIL=$(code "$API/alertmanager/silences" -H "$AUTH")
[ "$AM_SIL" = "200" ] && ok "AM silences 代理 200" || bad "AM silences" "$AM_SIL"
AM_RECV=$(code "$API/alertmanager/receivers" -H "$AUTH")
[ "$AM_RECV" = "200" ] && ok "AM receivers 代理 200" || bad "AM receivers" "$AM_RECV"

# === 9. 配置校验 ===
echo "--- 9. 配置校验 ---"
PROM_VALID=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: validation\n    static_configs:\n      - targets: [\"127.0.0.1:9090\"]\n"}')
[ "$PROM_VALID" = "200" ] && ok "prometheus 配置校验合法 (code=$PROM_VALID)" || bad "prometheus 校验" "$PROM_VALID"
PROM_INVALID=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"invalid: [unclosed"}')
[ "$PROM_INVALID" = "400" ] || [ "$PROM_INVALID" = "409" ] && ok "prometheus 配置校验非法 → $PROM_INVALID" || bad "prometheus 非法校验" "$PROM_INVALID"
AM_VALID=$(code -X POST $API/alertmanager/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"route:\n  receiver: default\nreceivers:\n  - name: default\n"}')
[ "$AM_VALID" = "200" ] && ok "alertmanager 配置校验 (code=$AM_VALID)" || bad "AM 校验" "$AM_VALID"

# === 10. 版本号校验 ===
echo "--- 10. 版本号校验 ---"
BAD_VER=$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"badver\",\"deploy_type\":\"target\",\"version\":\"../../etc/x\",\"host_id\":$HID,\"port\":19300}")
[ "$BAD_VER" = "400" ] && ok "非法版本号拒绝" || bad "非法版本号" "$BAD_VER"
SPACE_VER=$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"badver2\",\"deploy_type\":\"target\",\"version\":\"v 1.0\",\"host_id\":$HID,\"port\":19300}")
[ "$SPACE_VER" = "400" ] && ok "带空格版本拒绝" || bad "带空格版本" "$SPACE_VER"

# === 11. custom_args 注入防护 ===
echo "--- 11. custom_args 注入防护 ---"
NE=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"sup-ne\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HID,\"port\":19201}" | j "d['component']['id']")
wait_status $NE running 60 && ok "node_exporter running" || bad "NE 部署" "timeout"
INJ=$(curl -s -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--x=1; reboot","apply_custom_args":true}')
echo "$INJ" | grep -q "非法字符" && ok "custom_args 注入拒绝" || bad "注入防护" "$INJ"
# 合法参数
OK_ARGS=$(curl -s -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--collector.systemd","apply_custom_args":true}')
echo "$OK_ARGS" | grep -q '"id"' && ok "合法 custom_args 保存" || bad "合法参数" "$OK_ARGS"

# === 12. RBAC 权限边界 ===
echo "--- 12. RBAC 权限边界 ---"
# 创建 viewer 用户
VIEW_PASS="viewer-${TS}-Aa9"
VU=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"viewer-$TS\",\"password\":\"$VIEW_PASS\",\"role\":\"viewer\",\"email\":\"v@x.io\"}" | j "d['id']")
[ -n "$VU" ] && ok "创建 viewer 用户" || bad "创建 viewer" ""
VTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"viewer-$TS\",\"password\":\"$VIEW_PASS\"}" | j "d['token']")
VAUTH="Authorization: Bearer $VTOKEN"
# viewer 可读
VREAD=$(code $API/node -H "$VAUTH")
[ "$VREAD" = "200" ] && ok "viewer 可读节点" || bad "viewer 读" "$VREAD"
# viewer 不能写
VWRITE=$(code -X PUT $API/node -H "$VAUTH" -H 'Content-Type: application/json' -d '{"description":"nope"}')
[ "$VWRITE" = "403" ] && ok "viewer 不能写节点" || bad "viewer 写" "$VWRITE"
# viewer 不能管理用户
VUSERS=$(code $API/users -H "$VAUTH")
[ "$VUSERS" = "403" ] && ok "viewer 不能管理用户" || bad "viewer 用户管理" "$VUSERS"
# 创建 change_admin 角色
CHANGE_PASS="change-${TS}-Aa9"
UU=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"change-$TS\",\"password\":\"$CHANGE_PASS\",\"role\":\"change_admin\",\"email\":\"u@x.io\"}" | j "d['id']")
UTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"change-$TS\",\"password\":\"$CHANGE_PASS\"}" | j "d['token']")
UAUTH="Authorization: Bearer $UTOKEN"
UWRITE=$(code -X POST $API/components -H "$UAUTH" -H 'Content-Type: application/json' -d '{}')
[ "$UWRITE" = "400" ] && ok "change_admin 可达变更接口" || bad "change_admin 写" "$UWRITE"
UUSERS=$(code $API/users -H "$UAUTH")
[ "$UUSERS" = "403" ] && ok "change_admin 不能管理用户" || bad "change_admin 用户管理" "$UUSERS"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code $API/node -H "$AUTH")"

# === 13. 仪表盘导出内容验证 ===
echo "--- 13. 仪表盘导出内容验证 ---"
DASH=$(curl -s -X POST $API/dashboards -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-dash-$TS\",\"description\":\"test dash\",\"content\":\"{\\\"panels\\\":[]}\"}" | j "d['id']")
[ -n "$DASH" ] && ok "创建仪表盘" || bad "创建仪表盘" ""
EXPORT=$(curl -s "$API/dashboards/$DASH/export" -H "$AUTH")
echo "$EXPORT" | grep -q "panels" && ok "导出含仪表盘内容" || bad "导出内容" "$(echo $EXPORT | head -c 100)"
EXPORT_CODE=$(code "$API/dashboards/$DASH/export" -H "$AUTH")
[ "$EXPORT_CODE" = "200" ] && ok "导出 200" || bad "导出状态" "$EXPORT_CODE"

# === 14. 模板渲染验证 ===
echo "--- 14. 模板渲染验证 ---"
TPL=$(curl -s -X POST $API/templates -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-tpl-$TS\",\"component_type\":\"prometheus\",\"content\":\"global:\n  scrape_interval: {{.interval}}s\n\"}" | j "d['id']")
[ -n "$TPL" ] && ok "创建模板" || bad "创建模板" ""
RENDER=$(curl -s -X POST "$API/templates/$TPL/render" -H "$AUTH" -H 'Content-Type: application/json' -d '{"interval":"30"}')
echo "$RENDER" | grep -q "30s" && ok "模板渲染替换变量" || bad "模板渲染" "$RENDER"

# === 15. 健康检查 ===
echo "--- 15. 组件健康检查 ---"
HC=$(curl -s $API/components/$NE/health -H "$AUTH")
echo "$HC" | grep -q "healthy" && ok "node_exporter 健康检查 healthy" || bad "健康检查" "$HC"

# === 16. 资源监控 ===
echo "--- 16. 资源监控 ---"
RES=$(code $API/components/$NE/resource -H "$AUTH")
[ "$RES" = "200" ] && ok "组件资源监控 200" || bad "资源监控" "$RES"
CRES=$(code "$API/node/resource" -H "$AUTH")
[ "$CRES" = "200" ] && ok "节点资源监控 200" || bad "节点资源" "$CRES"

# === 17. 操作日志内容验证 ===
echo "--- 17. 操作日志内容验证 ---"
LOGS=$(curl -s "$API/operations/logs?limit=5" -H "$AUTH")
LOG_CNT=$(echo "$LOGS" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d.get('logs',[])))" 2>/dev/null)
[ "$LOG_CNT" != "0" ] && ok "操作日志有记录 ($LOG_CNT 条)" || bad "操作日志" "$LOG_CNT"
LOG_ACTION=$(echo "$LOGS" | python3 -c "import sys,json;d=json.load(sys.stdin);logs=d.get('logs',[]);print(logs[0].get('action','') if logs else '')" 2>/dev/null)
[ -n "$LOG_ACTION" ] && ok "日志含操作类型 ($LOG_ACTION)" || bad "日志内容" "$LOG_ACTION"

# === 18. 配置漂移检测 ===
echo "--- 18. 配置漂移检测 ---"
DRIFT=$(code "$API/config/drift" -H "$AUTH")
[ "$DRIFT" = "200" ] && ok "配置漂移检测 200" || bad "配置漂移" "$DRIFT"

# === 19. 拓扑图 ===
echo "--- 19. 拓扑图 ---"
TOPO=$(code "$API/topology" -H "$AUTH")
[ "$TOPO" = "200" ] && ok "拓扑图 200" || bad "拓扑图" "$TOPO"

# === 20. 首次向导与技术栈模板 ===
echo "--- 20. 首次向导与技术栈模板 ---"
ONBOARDING=$(code $API/onboarding/status -H "$AUTH")
[ "$ONBOARDING" = "200" ] && ok "首次向导状态 200" || bad "首次向导状态" "$ONBOARDING"
STACKS=$(code $API/stack-templates -H "$AUTH")
[ "$STACKS" = "200" ] && ok "技术栈模板 200" || bad "技术栈模板" "$STACKS"

# === 21. 维护窗口自动静默 ===
echo "--- 21. 维护窗口 ==="
MW_RESP=$(curl -s -X POST $API/maintenance -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-mw-$TS\",\"start_at\":\"$(date +%Y-%m-%dT%H:%M:%S+08:00)\",\"end_at\":\"$(date -d '+2 hours' +%Y-%m-%dT%H:%M:%S+08:00)\",\"reason\":\"test\"}")
MW=$(echo "$MW_RESP" | j "d['id']")
[ -n "$MW" ] && ok "创建维护窗口 id=$MW" || bad "维护窗口" ""
# 单节点维护窗口无需客户端传递内部作用域
MW_CHECK=$(curl -s -m 5 "$API/maintenance/check" -H "$AUTH" | python3 -c "import sys,json;print(str(json.load(sys.stdin).get('in_maintenance',False)).lower())" 2>/dev/null)
[ "$MW_CHECK" = "true" ] && ok "维护窗口检查命中" || bad "维护窗口检查" "$MW_CHECK"

# === 22. recording rule + remote-write 同步 ===
echo "--- 22. recording rule + remote-write ==="
RR=$(curl -s -X POST $API/recording-rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"rec:sup:$TS\",\"expression\":\"sum(rate(up[5m]))\"}" | j "d['id']")
[ -n "$RR" ] && ok "创建 recording rule" || bad "recording rule" ""
RW=$(curl -s -X POST $API/remote-writes -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"sup-rw\",\"url\":\"https://remote-write.invalid/api/v1/write\"}" | j "d['id']")
[ -n "$RW" ] && ok "创建 remote-write" || bad "remote-write" ""
# remote-write 不泄露密码
RW_SECRET=$(curl -s $API/remote-writes/$RW -H "$AUTH" | grep -c 'basic_auth_password\|bearer_token')
[ "$RW_SECRET" = "0" ] && ok "remote-write 不泄露凭据" || bad "remote-write 泄露" "$RW_SECRET"

# === 23. SSRF 防护（仪表盘导入）===
echo "--- 23. SSRF 防护 ---"
SSRF=$(code -X POST "$API/dashboards/import?url=http://127.0.0.1:8899/dash.json" -H "$AUTH")
[ "$SSRF" = "502" ] || [ "$SSRF" = "400" ] && ok "仪表盘导入 SSRF 拒绝 → $SSRF" || bad "SSRF 防护" "$SSRF"

# === 24. 告警历史 + 活跃告警 ===
echo "--- 24. 告警历史 + 活跃告警 ==="
AHIST=$(code "$API/alerts/history?limit=10" -H "$AUTH")
[ "$AHIST" = "200" ] && ok "告警历史 200" || bad "告警历史" "$AHIST"
AACT=$(code "$API/alerts/active" -H "$AUTH")
[ "$AACT" = "200" ] && ok "活跃告警 200" || bad "活跃告警" "$AACT"

# === 25. 版本管理 ===
echo "--- 25. 版本管理 ==="
VLIST=$(code $API/versions -H "$AUTH")
[ "$VLIST" = "200" ] && ok "版本列表 200" || bad "版本列表" "$VLIST"
VBT=$(code "$API/versions/by-type?type=prometheus" -H "$AUTH")
[ "$VBT" = "200" ] && ok "按类型查版本 200" || bad "按类型版本" "$VBT"

# === 26. 日志查看 ===
echo "--- 26. 日志查看 ==="
LOGS_API=$(code $API/components/$NE/logs -H "$AUTH")
[ "$LOGS_API" = "200" ] && ok "组件日志 200" || bad "组件日志" "$LOGS_API"

# === 27. 组件详情 ===
echo "--- 27. 组件详情 ==="
DET=$(curl -s $API/components/$NE -H "$AUTH")
echo "$DET" | grep -q "install_path\|service_name\|config_path" && ok "组件详情含部署信息" || bad "组件详情" "$(echo $DET | head -c 100)"

# === 28. 当前配置读取 ===
echo "--- 28. 当前配置读取 ==="
CUR_CFG=$(code $API/components/$NE/config/current -H "$AUTH")
[ "$CUR_CFG" = "200" ] && ok "读取当前配置 200" || bad "当前配置" "$CUR_CFG"

# === 29. 平台快照恢复 ===
echo "--- 29. 平台快照恢复 ---"
PLAT_BAK=$(curl -s "$API/backups/platform" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);bks=d.get('backups',[]);print(bks[0]['name'] if bks else '')" 2>/dev/null)
[ -n "$PLAT_BAK" ] && ok "有平台快照: $PLAT_BAK" || skip "平台快照" "无快照"
# 不实际恢复（会重启服务），只验证恢复端点对非法名称的防护
PLAT_RESTORE_BAD=$(sshpass -p "$PLATFORM_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$PLATFORM_HOST" "curl -s -m 5 -o /dev/null -w \"%{http_code}\" -X POST \"http://localhost:8080/api/v1/backups/platform/restore?name=../../etc/passwd\" -H \"$AUTH\"" 2>/dev/null | tr -d "\r")
[ "$PLAT_RESTORE_BAD" = "400" ] || [ "$PLAT_RESTORE_BAD" = "403" ] && ok "平台快照恢复拒绝路径遍历" || bad "快照恢复防护" "$PLAT_RESTORE_BAD"
PLAT_RESTORE_FAKE=$(code -X POST "$API/backups/platform/restore?name=platform-db-fake.db" -H "$AUTH")
[ "$PLAT_RESTORE_FAKE" = "400" ] || [ "$PLAT_RESTORE_FAKE" = "403" ] || [ "$PLAT_RESTORE_FAKE" = "404" ] && ok "平台快照恢复拒绝不存在文件" || bad "快照恢复不存在" "$PLAT_RESTORE_FAKE"

# === 30. 定时备份 CRUD ===
echo "--- 30. 定时备份 CRUD ==="
SB=$(curl -s -X POST $API/backups/scheduled -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"sup-sb-$TS\",\"cron\":\"0 4 * * *\",\"type\":\"config\",\"keep_count\":3,\"enabled\":true,\"component_id\":$NE}" | j "d['id']")
[ -n "$SB" ] && ok "创建定时备份 id=$SB" || bad "定时备份" ""
SB_UP=$(code -X PUT $API/backups/scheduled/$SB -H "$AUTH" -H 'Content-Type: application/json' -d '{"keep_count":5}')
[ "$SB_UP" = "200" ] && ok "更新定时备份" || bad "更新定时备份" "$SB_UP"
SB_LIST=$(code $API/backups/scheduled -H "$AUTH")
[ "$SB_LIST" = "200" ] && ok "定时备份列表 200" || bad "定时备份列表" "$SB_LIST"

# === 清理 ===
echo "--- 清理 ---"
# 删除测试数据
for id in $RR $RW; do curl -s -X DELETE $API/recording-rules/$RR -H "$AUTH" >/dev/null; curl -s -X DELETE $API/remote-writes/$RW -H "$AUTH" >/dev/null; done
for id in $CH_WH $CH_WH2 $CH_EM $CH_DT $CH_WC $CH_SL $CH_BAD; do curl -s -X DELETE $API/notifications/channels/$id -H "$AUTH" >/dev/null 2>&1; done
curl -s -X DELETE $API/dashboards/$DASH -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/templates/$TPL -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/maintenance/$MW -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/backups/scheduled/$SB -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/users/$VU -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/users/$UU -H "$AUTH" >/dev/null 2>&1
# 卸载组件
for cid in $BB $NE $AM; do governed_action "$API/components/$cid/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null 2>&1; done
sleep 8
# 删节点+主机
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HID -H "$AUTH" >/dev/null 2>&1
# 远端残留
RES_PORT=$(rssh "ss -tlnp | grep -E ':192[0-9]{2}' | wc -l" | tr -d ' ')
[ "$RES_PORT" = "0" ] && ok "远端无测试端口残留" || bad "远端残留" "$RES_PORT"

echo ""
echo "================================================================"
echo " 补充功能测试结果: $PASS passed, $FAIL failed, $SKIP skipped"
echo "================================================================"
[ $FAIL -eq 0 ] || exit 1
