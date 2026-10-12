#!/bin/bash
# Full E2E test for Prometheus Lifecycle Platform (local deploy)
# Usage: BASE=http://localhost:8080 bash test-scripts/e2e_full_test.sh
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
skip() { SKIP=$((SKIP+1)); echo "  - $1 (SKIP: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

mkdir -p /tmp/e2e-httpd
echo '{"title":"imported-dash","uid":"imp-uid","panels":[]}' > /tmp/e2e-httpd/dash.json
python3 -m http.server 8899 --directory /tmp/e2e-httpd >/tmp/e2e-httpd.log 2>&1 &
HTTPD_PID=$!
trap 'kill $HTTPD_PID 2>/dev/null' EXIT

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
TS=$(date +%s)
echo "=== Full E2E (TS=$TS) ==="

echo "--- A. Auth / Users ---"
checkme=$(code $API/auth/me -H "$AUTH")
[ "$checkme" = "200" ] && ok "/auth/me 200" || bad "/auth/me" "$checkme"
UNAME="e2e-change-$TS"
USER_PASS="change-${TS}-Aa9"
USER_ID=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$UNAME\",\"password\":\"$USER_PASS\",\"role\":\"change_admin\",\"email\":\"u@x.io\"}" | j "d['id']")
[ -n "$USER_ID" ] && ok "创建 change_admin id=$USER_ID" || bad "创建 change_admin" "$USER_ID"
UTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$UNAME\",\"password\":\"$USER_PASS\"}" | j "d['token']")
[ -n "$UTOKEN" ] && ok "change_admin 登录" || bad "change_admin 登录" "$UTOKEN"
checku=$(code $API/users -H "Authorization: Bearer $UTOKEN")
[ "$checku" = "403" ] && ok "change_admin 不能访问用户管理 403" || bad "change_admin 访问用户管理" "$checku"
UPD=$(curl -s -X PUT $API/users/$USER_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"email":"new@x.io","role":"viewer"}' | j "d['role']")
[ "$UPD" = "viewer" ] && ok "更新 user 角色" || bad "更新 user 角色" "$UPD"

echo "--- B. Node singleton ---"
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点 id=$NODE_ID" || bad "读取节点" "$NODE_ID"
codec=$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"e2e singleton"}')
[ "$codec" = "200" ] && ok "更新节点" || bad "更新节点" "$codec"
codeg=$(code $API/node -H "$AUTH")
[ "$codeg" = "200" ] && ok "查询节点" || bad "查询节点" "$codeg"
invalid_node=$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"status":"invalid"}')
[ "$invalid_node" = "400" ] && ok "非法节点状态 400" || bad "非法节点状态" "$invalid_node"

echo "--- C. Hosts ---"
HOST_ID=$(curl -s "$API/hosts" -H "$AUTH" | j "next((h['id'] for h in d['hosts'] if h['ip']=='127.0.0.20' and h['connection_type']=='local'), '')")
if [ -z "$HOST_ID" ]; then
  HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"e2e-host-$TS\",\"ip\":\"127.0.0.20\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
fi
[ -n "$HOST_ID" ] && ok "创建或复用本机连接主机 id=$HOST_ID" || bad "本机连接主机" "$HOST_ID"
codeh=$(code -X POST $API/hosts/$HOST_ID/test -H "$AUTH")
[ "$codeh" = "200" ] && ok "主机连接测试" || bad "主机连接测试" "$codeh"
coded=$(code $API/hosts/$HOST_ID/detect -H "$AUTH")
[ "$coded" = "200" ] && ok "组件探测" || bad "组件探测" "$coded"
codeb=$(code "$API/hosts/$HOST_ID/browse?path=/tmp" -H "$AUTH")
[ "$codeb" = "200" ] && ok "目录浏览 /tmp" || bad "目录浏览" "$codeb"
codeb2=$(code "$API/hosts/$HOST_ID/browse?path=/tmp;rm" -H "$AUTH")
[ "$codeb2" = "200" ] && ok "目录浏览(裸分号按 URL 分隔符处理→200)" || bad "目录浏览裸分号" "$codeb2"
codeb3=$(code "$API/hosts/$HOST_ID/browse?path=%2Ftmp%3Brm" -H "$AUTH")
[ "$codeb3" = "400" ] && ok "目录浏览拒绝编码危险字符 400" || bad "目录浏览编码危险字符" "$codeb3"

echo "--- D. Components (target) CRUD ---"
TARGET_CREATE=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"e2e-node-$TS\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19200}")
COMP_ID=$(echo "$TARGET_CREATE" | j "d['component']['id']")
TARGET_OP=$(echo "$TARGET_CREATE" | j "d['operation']['id']")
[ -n "$COMP_ID" ] && ok "创建组件(id=$COMP_ID, target)" || bad "创建组件" "$COMP_ID"
TARGET_STATUS=pending
for i in $(seq 1 60); do
  TARGET_STATUS=$(curl -s "$API/operations/$TARGET_OP" -H "$AUTH" | j "d['status']")
  { [ "$TARGET_STATUS" = "success" ] || [ "$TARGET_STATUS" = "failed" ]; } && break
  sleep 1
done
[ "$TARGET_STATUS" = "success" ] && ok "target 注册与集成操作完成" || { bad "target 注册与集成" "$TARGET_STATUS"; exit 1; }
codeu=$(code -X PUT $API/components/$COMP_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"e2e-node-renamed"}')
[ "$codeu" = "200" ] && ok "更新组件" || bad "更新组件" "$codeu"
codeh2=$(code $API/components/$COMP_ID/health -H "$AUTH")
[ "$codeh2" = "200" ] && ok "组件健康检查接口" || bad "组件健康检查" "$codeh2"
codec2=$(code $API/topology -H "$AUTH")
[ "$codec2" = "200" ] && ok "拓扑" || bad "拓扑" "$codec2"

echo "--- E. Versions ---"
V_ID=$(curl -s -X POST $API/versions -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"version\":\"v9.9.9-test\",\"image\":\"prom/prometheus:v9.9.9-test\",\"source\":\"custom\"}" | j "d['id']")
[ -n "$V_ID" ] && ok "创建版本 id=$V_ID" || bad "创建版本" "$V_ID"
codev=$(code -X PUT $API/versions/$V_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"notes":"test note"}')
[ "$codev" = "200" ] && ok "更新版本" || bad "更新版本" "$codev"
codevt=$(code "$API/versions/by-type?type=prometheus" -H "$AUTH")
[ "$codevt" = "200" ] && ok "按类型查版本" || bad "按类型查版本" "$codevt"

echo "--- F. Templates ---"
TPL_ID=$(curl -s -X POST $API/templates -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"e2e-tpl-$TS\",\"component_type\":\"prometheus\",\"content\":\"global:\\n  scrape_interval: 15s\",\"description\":\"e2e\"}" | j "d['id']")
[ -n "$TPL_ID" ] && ok "创建模板 id=$TPL_ID" || bad "创建模板" "$TPL_ID"
codeT=$(code -X PUT $API/templates/$TPL_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"updated"}')
[ "$codeT" = "200" ] && ok "更新模板" || bad "更新模板" "$codeT"
codetd=$(code $API/templates/$TPL_ID/download -H "$AUTH")
[ "$codetd" = "200" ] && ok "下载模板" || bad "下载模板" "$codetd"
printf 'groups:\n  - name: g\n    rules: []\n' > /tmp/e2e-tpl.yaml
codeTU=$(code -X POST $API/templates/upload -H "$AUTH" -F "name=e2e-tpl-up-$TS" -F "component_type=prometheus" -F "file=@/tmp/e2e-tpl.yaml")
[ "$codeTU" = "201" ] && ok "上传模板" || bad "上传模板" "$codeTU"
coder=$(code -X POST $API/templates/$TPL_ID/render -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
[ "$coder" = "200" ] && ok "模板渲染" || bad "模板渲染" "$coder"

echo "--- G. Packages ---"
head -c 2048 /dev/urandom > /tmp/e2e-pkg.tar.gz
codePU=$(code -X POST $API/packages/upload -H "$AUTH" -F "name=e2e-pkg-$TS" -F "type=node_exporter" -F "version=1.0.0" -F "file=@/tmp/e2e-pkg.tar.gz")
[ "$codePU" = "201" ] && ok "上传包" || bad "上传包" "$codePU"
PKG_ID=$(curl -s $API/packages -H "$AUTH" | j "[x for x in d['packages'] if 'e2e-pkg-$TS' in x['name']][0]['id']")
[ -n "$PKG_ID" ] && ok "包列表含新包 id=$PKG_ID" || bad "包列表" "$PKG_ID"
codeP=$(code $API/packages/$PKG_ID/download -H "$AUTH")
[ "$codeP" = "200" ] && ok "下载包" || bad "下载包" "$codeP"
codeC=$(code -X POST $API/packages -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"e2e-reg-$TS\",\"type\":\"prometheus\",\"version\":\"9.9.9\",\"format\":\"binary\",\"size\":123,\"checksum\":\"abc\",\"storage_path\":\"/tmp/nonexistent.tar.gz\",\"description\":\"reg\"}")
[ "$codeC" = "201" ] && ok "注册包记录" || bad "注册包记录" "$codeC"
codeDockerPkg=$(code -X POST $API/packages -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"no-docker-$TS\",\"type\":\"prometheus\",\"version\":\"1.0.0\",\"format\":\"docker\"}")
[ "$codeDockerPkg" = "400" ] && ok "拒绝 Docker 包格式" || bad "Docker 包格式仍可用" "$codeDockerPkg"
REG_PKG_ID=$(curl -s $API/packages -H "$AUTH" | j "[x for x in d['packages'] if 'e2e-reg-$TS' in x['name']][0]['id']")

echo "--- H. Alert / Scrape / Recording / Remote-write CRUD ---"
RULE_ID=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"E2EHighCPU-$TS\",\"expression\":\"cpu > 90\",\"severity\":\"critical\",\"duration\":\"5m\"}" | j "d['id']")
[ -n "$RULE_ID" ] && ok "创建告警规则 id=$RULE_ID" || bad "创建告警规则" "$RULE_ID"
codeR=$(code -X PUT $API/alerts/rules/$RULE_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"duration":"10m"}')
[ "$codeR" = "202" ] && ok "告警规则更新已受理（不代表已发布）" || bad "更新告警规则" "$codeR"
SC_ID=$(curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"e2e-sc-$TS\",\"job_name\":\"e2e_job_$TS\",\"targets\":\"[\\\"1.2.3.4:9100\\\"]\",\"scrape_interval\":\"30s\"}" | j "d['id']")
[ -n "$SC_ID" ] && ok "创建 scrape-config id=$SC_ID" || bad "创建 scrape-config" "$SC_ID"
codeS=$(code -X PUT $API/scrape-configs/$SC_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"scrape_interval":"45s"}')
[ "$codeS" = "202" ] && ok "scrape-config 更新已受理（不代表已发布）" || bad "更新 scrape-config" "$codeS"
RR_ID=$(curl -s -X POST $API/recording-rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"e2e:rec:$TS\",\"expression\":\"sum(rate(x[5m]))\"}" | j "d['id']")
[ -n "$RR_ID" ] && ok "创建 recording-rule id=$RR_ID" || bad "创建 recording-rule" "$RR_ID"
codeRR=$(code -X PUT $API/recording-rules/$RR_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"enabled":false}')
[ "$codeRR" = "202" ] && ok "recording-rule 更新已受理（不代表已发布）" || bad "更新 recording-rule" "$codeRR"
RW_ID=$(curl -s -X POST $API/remote-writes -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"e2e-rw-$TS\",\"url\":\"https://remote-write.invalid/api/v1/write\",\"remote_timeout\":\"45s\"}" | j "d['id']")
[ -n "$RW_ID" ] && ok "创建 remote-write id=$RW_ID" || bad "创建 remote-write" "$RW_ID"
RW_TEST_SECRET="remote-${TS}-Aa9"
codeRW=$(code -X PUT $API/remote-writes/$RW_ID -H "$AUTH" -H 'Content-Type: application/json' -d "{\"enabled\":false,\"basic_auth_user\":\"test-$TS\",\"basic_auth_password\":\"$RW_TEST_SECRET\"}")
[ "$codeRW" = "202" ] && ok "remote-write 更新已受理（不代表已发布）" || bad "更新 remote-write" "$codeRW"
RW_SECRET_SAFE=$(curl -s $API/remote-writes/$RW_ID -H "$AUTH" | j "d.get('id') == int('$RW_ID') and '$RW_TEST_SECRET' not in json.dumps(d)")
[ "$RW_SECRET_SAFE" = "True" ] && ok "remote-write 读取正确资源且不泄露密码" || bad "remote-write 读取或脱敏失败" "$RW_SECRET_SAFE"

echo "--- I. Dashboards ---"
DASH_ID=$(curl -s -X POST $API/dashboards -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"e2e-dash-$TS\",\"description\":\"e2e\",\"content\":\"{}\"}" | j "d['id']")
[ -n "$DASH_ID" ] && ok "创建仪表盘 id=$DASH_ID" || bad "创建仪表盘" "$DASH_ID"
codeD=$(code -X PUT $API/dashboards/$DASH_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"{\"title\":\"t\"}"}')
[ "$codeD" = "202" ] && ok "仪表盘更新已受理（不代表已发布）" || bad "更新仪表盘" "$codeD"
codeD2=$(code $API/dashboards/$DASH_ID/export -H "$AUTH")
[ "$codeD2" = "200" ] && ok "导出仪表盘" || bad "导出仪表盘" "$codeD2"
codeDI=$(code -X POST "$API/dashboards/import?url=http://127.0.0.1:8899/dash.json" -H "$AUTH")
[ "$codeDI" -ge 400 ] && ok "SSRF 防护拒绝回环地址导入 ($codeDI)" || bad "SSRF 防护拒绝回环导入" "$codeDI"
codeDU=$(code -X POST $API/dashboards/upload -H "$AUTH" -F "name=e2e-up-$TS" -F "file=@/tmp/e2e-httpd/dash.json")
[ "$codeDU" = "202" ] && ok "仪表盘上传已受理（不代表已发布）" || bad "上传仪表盘" "$codeDU"

echo "--- J. Notification channels ---"
CH_ID=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"e2e-webhook-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"http://127.0.0.1:9/hook\\\"}\"}" | j "d['id']")
[ -n "$CH_ID" ] && ok "创建通知渠道 id=$CH_ID" || bad "创建通知渠道" "$CH_ID"
codeCH=$(code -X PUT $API/notifications/channels/$CH_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"enabled":false}')
[ "$codeCH" = "200" ] && ok "更新通知渠道" || bad "更新通知渠道" "$codeCH"
codeHIST=$(code $API/notifications/history -H "$AUTH")
[ "$codeHIST" = "200" ] && ok "通知历史" || bad "通知历史" "$codeHIST"

echo "--- K. Maintenance ---"
MW_ID=$(curl -s -X POST $API/maintenance -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"e2e-mw-$TS\",\"start_at\":\"2026-09-01T02:00:00Z\",\"end_at\":\"2026-09-02T02:00:00Z\",\"reason\":\"upgrade\"}" | j "d['id']")
[ -n "$MW_ID" ] && ok "创建维护窗口 id=$MW_ID" || bad "创建维护窗口" "$MW_ID"
codeMW=$(code "$API/maintenance/check" -H "$AUTH")
[ "$codeMW" = "200" ] && ok "维护窗口检查" || bad "维护窗口检查" "$codeMW"

echo "--- L. Scheduled backups ---"
codePlatformBackup=$(code -X POST $API/backups/platform -H "$AUTH")
[ "$codePlatformBackup" = "201" ] && ok "手动平台数据库快照" || bad "手动平台数据库快照" "$codePlatformBackup"
[ "$(code $API/backups/platform -H "$AUTH")" = "200" ] && ok "平台快照列表" || bad "平台快照列表"
codeSB=$(code -X POST $API/backups/scheduled -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"e2e-sb-$TS\",\"cron\":\"0 2 * * *\",\"type\":\"config\",\"keep_count\":3}")
[ "$codeSB" = "400" ] && ok "定时备份要求明确可管理组件" || bad "定时备份目标校验" "$codeSB"

echo "--- M. Lifecycle inventory ---"
codeSTATUS=$(code $API/stats -H "$AUTH")
[ "$codeSTATUS" = "200" ] && ok "生命周期运行状态" || bad "生命周期运行状态" "$codeSTATUS"
codeSTACKS=$(code $API/stack-templates -H "$AUTH")
[ "$codeSTACKS" = "200" ] && ok "技术栈模板" || bad "技术栈模板" "$codeSTACKS"

echo "--- N. Config validation ---"
codeV=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: node\n    static_configs:\n      - targets: [\"1.2.3.4:9100\"]\n"}')
[ "$codeV" = "200" ] && ok "Prometheus 配置校验(合法)" || bad "Prometheus 配置校验" "$codeV"
codeVI=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"::: not yaml :::"}')
{ [ "$codeVI" -ge 400 ] && [ "$codeVI" -lt 500 ]; } && ok "Prometheus 配置校验(非法→4xx: $codeVI)" || bad "非法配置校验" "$codeVI"
codeVA=$(code -X POST $API/alertmanager/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"route:\n  receiver: default\nreceivers:\n  - name: default\n"}')
[ "$codeVA" = "200" ] && ok "Alertmanager 配置校验" || bad "Alertmanager 配置校验" "$codeVA"

echo "--- O. Ops / stats / drift / resource ---"
for ep in stats stats/enhanced disk/check operations operations/logs; do
  c=$(code "$API/$ep" -H "$AUTH")
  [ "$c" = "200" ] && ok "GET /$ep" || bad "GET /$ep" "$c"
done
[ "$(code ${BASE%/}/metrics)" = "200" ] && ok "GET /metrics" || bad "GET /metrics"
codeDR=$(code $API/config/drift -H "$AUTH")
[ "$codeDR" = "200" ] && ok "配置漂移" || bad "配置漂移" "$codeDR"
codeRES=$(code $API/components/$COMP_ID/resource -H "$AUTH")
[ "$codeRES" = "200" ] && ok "组件资源监控" || bad "组件资源监控" "$codeRES"
codeST=$(code "$API/components/999999/logs/stream" -H "$AUTH")
[ "$codeST" = "404" ] && ok "日志流(不存在组件→404)" || bad "日志流404" "$codeST"

echo "--- P. Governed config safety ---"
CONFIG_TARGET_CODE=$(code -X POST $API/components/$COMP_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"node.yml","content":"k: v1","content_type":"yaml","description":"v1","reason":"target config safety"}')
[ "$CONFIG_TARGET_CODE" = "400" ] && ok "target 配置变更安全拒绝" || bad "target 配置变更" "$CONFIG_TARGET_CODE"
[ "$(code $API/change-plans -H "$AUTH")" = "200" ] && ok "治理变更计划可读" || bad "治理变更计划"
codeTD=$(governed_code "$API/components/$COMP_ID/uninstall" '{"mode":"keep_service","reason":"release target before lifecycle tests"}' "$AUTH")
[ "$codeTD" = "200" ] && ok "治理删除 target 组件" || bad "治理删除 target 组件" "$codeTD"

echo "--- Q. Binary deploy node_exporter (requires local offline packages) ---"
BIN_182=$(curl -s "$API/packages?type=node_exporter" -H "$AUTH" | j "len([p for p in d.get('packages',[]) if p.get('version') in ('v1.8.2','1.8.2') and p.get('format') in ('binary','zip')])")
BIN_170=$(curl -s "$API/packages?type=node_exporter" -H "$AUTH" | j "len([p for p in d.get('packages',[]) if p.get('version') in ('v1.7.0','1.7.0') and p.get('format') in ('binary','zip')])")
if [ "${BIN_182:-0}" -gt 0 ] && [ "${BIN_170:-0}" -gt 0 ]; then
  NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
  BCOMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"e2e-bin-node\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19100}" | j "d['component']['id']")
  [ -n "$BCOMP_ID" ] && ok "创建二进制组件 id=$BCOMP_ID" || bad "创建二进制组件" "$BCOMP_ID"
  STATUS="pending"
  for i in $(seq 1 40); do STATUS=$(curl -s $API/components/$BCOMP_ID -H "$AUTH" | j "d['status']"); [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ] && break; sleep 3; done
  [ "$STATUS" = "running" ] && ok "二进制部署 running" || bad "二进制部署" "$STATUS"
  BIN_UNIT=$(curl -s "$API/components/$BCOMP_ID" -H "$AUTH" | j "d['service_name']")
  [ "$(systemctl is-active "$BIN_UNIT" 2>&1)" = "active" ] && ok "systemd 单元 active" || bad "systemd 单元" "$(systemctl is-active "$BIN_UNIT" 2>&1)"
  [ "$(systemctl is-enabled "$BIN_UNIT" 2>&1)" = "enabled" ] && ok "systemd 单元 enabled 自启动" || bad "systemd 自启动" "$(systemctl is-enabled "$BIN_UNIT" 2>&1)"
  codeSTOP=$(code -X POST $API/components/$BCOMP_ID/stop -H "$AUTH")
  [ "$codeSTOP" = "200" ] && [ "$(systemctl is-active "$BIN_UNIT" 2>&1)" != "active" ] && [ "$(systemctl is-enabled "$BIN_UNIT" 2>&1)" = "enabled" ] && ok "二进制 stop 保留自启动" || bad "二进制 stop" "$codeSTOP"
  codeSTART=$(code -X POST $API/components/$BCOMP_ID/start -H "$AUTH")
  [ "$codeSTART" = "200" ] && [ "$(systemctl is-active "$BIN_UNIT" 2>&1)" = "active" ] && [ "$(systemctl is-enabled "$BIN_UNIT" 2>&1)" = "enabled" ] && ok "二进制 start 恢复运行" || bad "二进制 start" "$codeSTART"
  codeBR=$(code -X POST $API/components/$BCOMP_ID/restart -H "$AUTH")
  [ "$codeBR" = "200" ] && ok "二进制 restart" || bad "二进制 restart" "$codeBR"
  codeBU=$(governed_code "$API/components/$BCOMP_ID/upgrade" '{"new_version":"v1.7.0","reason":"test lifecycle"}' "$AUTH")
  [ "$codeBU" = "200" ] && ok "二进制 upgrade" || bad "二进制 upgrade" "$codeBU"
  curl -s -m 5 http://127.0.0.1:19100/metrics 2>/dev/null | grep '^node_exporter_build_info' | grep -q 'version="1\.7\.0"' && ok "升级后版本 1.7.0" || bad "升级版本" "$(curl -s -m 5 http://127.0.0.1:19100/metrics | head -1)"
  codeBD=$(governed_code "$API/components/$BCOMP_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH")
  [ "$codeBD" = "200" ] && ok "删除二进制组件" || bad "删除二进制组件" "$codeBD"
else
  skip "二进制生命周期" "需要 node_exporter v1.8.2 与 v1.7.0 离线包"
fi

echo "--- R. Docker deployment is removed ---"
codeDocker=$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"no-docker\",\"deploy_type\":\"docker\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19110}")
[ "$codeDocker" = "400" ] && ok "拒绝 Docker 组件部署" || bad "Docker 部署仍可用" "$codeDocker"

echo "--- T. Cascade cleanup ---"
curl -s -X DELETE "$API/templates/$TPL_ID" -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE "$API/versions/$V_ID" -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE "$API/packages/$PKG_ID" -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE "$API/packages/$REG_PKG_ID" -H "$AUTH" >/dev/null 2>&1
NODE_STATUS=$(code $API/node -H "$AUTH")
[ "$NODE_STATUS" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$NODE_STATUS"
codeHD=$(code -X DELETE "$API/hosts/$HOST_ID" -H "$AUTH")
[ "$codeHD" = "409" ] && ok "本机连接主机受删除保护" || bad "本机删除保护" "$codeHD"
codeUD=$(code -X DELETE "$API/users/$USER_ID" -H "$AUTH")
[ "$codeUD" = "200" ] && ok "删除用户" || bad "删除用户" "$codeUD"

echo ""
echo "=========================================="
echo "Full E2E Result: $PASS passed, $FAIL failed, $SKIP skipped"
echo "=========================================="
[ $FAIL -eq 0 ]
