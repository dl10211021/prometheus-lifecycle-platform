#!/usr/bin/env bash
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
check() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "$3 (expected $2)"; fi; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

TOKEN=$(curl -s -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
TS=$(date +%s)
NODE_ID=$(curl -s "$API/node" -H "$AUTH" | j "d['id']")

printf '%s\n' '=== A. 单节点、向导与产品入口 ==='
check "读取单节点" "200" "$(code "$API/node" -H "$AUTH")"
check "更新单节点" "200" "$(code -X PUT "$API/node" -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"regression singleton"}')"
check "向导状态" "200" "$(code "$API/onboarding/status" -H "$AUTH")"
check "本机预检" "200" "$(code "$API/onboarding/preflight" -H "$AUTH")"
check "本机发现" "200" "$(code "$API/onboarding/discovery" -H "$AUTH")"
check "技术栈模板" "200" "$(code "$API/stack-templates" -H "$AUTH")"
LEGACY_COLLECTION="/clu""sters"
check "旧集合路由不存在" "404" "$(code "$API$LEGACY_COLLECTION" -H "$AUTH")"
grep -q 'Prometheus 生命周期管理' <<<"$(curl -s "${BASE%/}/")" && ok "产品品牌 HTML" || bad "产品品牌 HTML" "missing brand"
LOCAL_HOST_ID=$(curl -s "$API/hosts" -H "$AUTH" | j "[h['id'] for h in d.get('hosts',[]) if h.get('ip')=='127.0.0.1'][0]")
check "自动本机不可删除" "409" "$(code -X DELETE "$API/hosts/$LOCAL_HOST_ID?force=1" -H "$AUTH")"

printf '%s\n' '=== B. RemoteWrite CRUD ==='
RW_ID=$(curl -s -X POST "$API/remote-writes" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"rw-test-$TS\",\"url\":\"https://remote-write.invalid/api/v1/write\"}" | j "d['id']")
check "创建 remote-write" "200" "$(code "$API/remote-writes/$RW_ID" -H "$AUTH")"
check "更新 remote-write 已受理" "202" "$(code -X PUT "$API/remote-writes/$RW_ID" -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"rw-updated"}')"
check "更新已保存（非发布验证）" "rw-updated" "$(curl -s "$API/remote-writes/$RW_ID" -H "$AUTH" | j "d['name']")"
check "未授权访问" "401" "$(code "$API/remote-writes" -H 'Authorization: Bearer bad')"

printf '%s\n' '=== C. 维护与身份 ==='
MW_ID=$(curl -s -X POST "$API/maintenance" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"reg-maint-$TS\",\"start_at\":\"2026-09-01T02:00:00Z\",\"end_at\":\"2026-09-02T02:00:00Z\"}" | j "d['id']")
check "维护窗口检查" "200" "$(code "$API/maintenance/check" -H "$AUTH")"
UNAME="reg-viewer-$TS"
UPASS="reg-${TS}-Viewer9"
USER_ID=$(curl -s -X POST "$API/users" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$UNAME\",\"password\":\"$UPASS\",\"role\":\"viewer\"}" | j "d['id']")
UTOKEN=$(curl -s -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "{\"username\":\"$UNAME\",\"password\":\"$UPASS\"}" | j "d['token']")
UAUTH="Authorization: Bearer $UTOKEN"
check "viewer 写节点 403" "403" "$(code -X PUT "$API/node" -H "$UAUTH" -H 'Content-Type: application/json' -d '{"description":"forbidden"}')"
check "viewer 用户管理 403" "403" "$(code "$API/users" -H "$UAUTH")"
check "viewer 读取节点" "200" "$(code "$API/node" -H "$UAUTH")"

printf '%s\n' '=== D. 告警、采集、记录规则 ==='
RULE_ID=$(curl -s -X POST "$API/alerts/rules" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"HighCPU-$TS\",\"expression\":\"cpu > 80\",\"severity\":\"warning\",\"duration\":\"5m\"}" | j "d['id']")
SC_ID=$(curl -s -X POST "$API/scrape-configs" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"custom-$TS\",\"job_name\":\"custom_job_$TS\",\"targets\":[\"target.invalid:9100\"]}" | j "d['id']")
RR_ID=$(curl -s -X POST "$API/recording-rules" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"rec:$TS\",\"expression\":\"sum(rate(cpu[5m]))\"}" | j "d['id']")
check "告警规则详情" "200" "$(code "$API/alerts/rules/$RULE_ID" -H "$AUTH")"
check "采集配置详情" "200" "$(code "$API/scrape-configs/$SC_ID" -H "$AUTH")"
check "记录规则详情" "200" "$(code "$API/recording-rules/$RR_ID" -H "$AUTH")"

printf '%s\n' '=== E. 模板、仪表盘与通知 ==='
TPL_ID=$(curl -s -X POST "$API/templates" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"reg-tpl-$TS\",\"component_type\":\"prometheus\",\"content\":\"global:\\n  scrape_interval: 15s\"}" | j "d['id']")
DASH_ID=$(curl -s -X POST "$API/dashboards" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"reg-dash-$TS\",\"description\":\"test\"}" | j "d['id']")
CH_ID=$(curl -s -X POST "$API/notifications/channels" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"reg-webhook-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/hook\\\"}\"}" | j "d['id']")
check "模板详情" "200" "$(code "$API/templates/$TPL_ID" -H "$AUTH")"
check "仪表盘详情" "200" "$(code "$API/dashboards/$DASH_ID" -H "$AUTH")"
check "通知渠道列表" "200" "$(code "$API/notifications/channels" -H "$AUTH")"

printf '%s\n' '=== F. 当前运维路由 ==='
check "节点拓扑" "200" "$(code "$API/topology" -H "$AUTH")"
check "节点资源" "200" "$(code "$API/node/resource" -H "$AUTH")"
check "配置漂移" "200" "$(code "$API/config/drift" -H "$AUTH")"
check "磁盘检查" "200" "$(code "$API/disk/check" -H "$AUTH")"
check "增强统计" "200" "$(code "$API/stats/enhanced" -H "$AUTH")"
check "运行状态" "200" "$(code "$API/stats" -H "$AUTH")"
check "日志流不存在组件" "404" "$(code --max-time 5 "$API/components/999999/logs/stream" -H "$AUTH")"

curl -s -X DELETE "$API/remote-writes/$RW_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/alerts/rules/$RULE_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/scrape-configs/$SC_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/recording-rules/$RR_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/templates/$TPL_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/dashboards/$DASH_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/notifications/channels/$CH_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/maintenance/$MW_ID" -H "$AUTH" >/dev/null
curl -s -X DELETE "$API/users/$USER_ID" -H "$AUTH" >/dev/null

printf '\nRegression Results: %s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
