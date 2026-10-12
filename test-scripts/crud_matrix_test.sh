#!/bin/bash
# 全功能 CRUD 矩阵测试：模块 × 操作 全覆盖
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: ${2:-})"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

TS=$(date +%s)
echo "=== 全功能 CRUD 矩阵测试 (TS=$TS) ==="
TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录" "rate limited?"; exit 1; }

echo "--- 1. 主机模块 ---"
HID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-host-$TS\",\"ip\":\"127.0.0.10\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
[ -n "$HID" ] && ok "C 创建主机" || bad "C 创建主机" "$HID"
[ "$(code $API/hosts -H "$AUTH")" = "200" ] && ok "R 主机列表" || bad "R 主机列表"
R=$(curl -s -X PUT $API/hosts/$HID -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-host-renamed-$TS\"}")
echo "$R" | grep -q "m-host-renamed" && ok "U 编辑主机" || bad "U 编辑主机" "$R"
[ "$(code -X POST $API/hosts/$HID/test -H "$AUTH" -H 'Content-Type: application/json' -d '{}')" != "500" ] && ok "S 测试连接" || bad "S 测试连接"
[ "$(code "$API/hosts/$HID/ports/22" -H "$AUTH")" = "200" ] && ok "S 端口检查" || bad "S 端口检查"
BATCH_PASS="batch-${TS}-Aa9"
R=$(curl -s -X POST $API/hosts/batch -H "$AUTH" -H 'Content-Type: application/json' -d "[{\"ip\":\"127.0.0.2\",\"username\":\"r\",\"password\":\"$BATCH_PASS\",\"name\":\"b1\"},{\"ip\":\"127.0.0.3\",\"username\":\"r\",\"password\":\"$BATCH_PASS\"}]")
echo "$R" | grep -q '"status":"created"' && ok "S 批量添加" || bad "S 批量添加" "$R"
BATCH_IDS=$(echo "$R" | j '" ".join(str(x.get("id","")) for x in d.get("results",[]) if x.get("status")=="created")')

echo "--- 2. 节点模块 ---"
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "C 读取节点" || bad "C 读取节点" "$NODE_ID"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "R 节点详情" || bad "R 节点详情"
curl -s -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"production node"}' >/dev/null
[ "$(curl -s $API/node -H "$AUTH" | j "d['description']")" = "production node" ] && ok "U 编辑节点" || bad "U 编辑节点"
[ "$(code "$API/topology" -H "$AUTH")" = "200" ] && ok "S 拓扑图" || bad "S 拓扑图"

echo "--- 3. 组件模块（target 快速） ---"
COMP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"m-ne\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HID,\"port\":19400}" | j "d['component']['id']")
[ -n "$COMP" ] && ok "C 创建组件" || bad "C 创建组件" "$COMP"
[ "$(code $API/components/$COMP -H "$AUTH")" = "200" ] && ok "R 组件详情" || bad "R 组件详情"
[ "$(code $API/components/$COMP/health -H "$AUTH")" = "200" ] && ok "S 健康检查" || bad "S 健康检查"
[ "$(code "$API/components/$COMP/logs" -H "$AUTH")" = "200" ] && ok "S 日志" || bad "S 日志"
R=$(curl -s -X PUT $API/components/$COMP -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-ne-renamed\"}")
echo "$R" | grep -q "m-ne-renamed" && ok "U 编辑组件" || bad "U 编辑组件" "$R"
[ "$(code -X POST $API/components/$COMP/restart -H "$AUTH")" = "200" ] && ok "S 重启(target)" || bad "S 重启"

echo "--- 4. 配置模块 ---"
SID=$(curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"m-scrape\",\"job_name\":\"m_job\",\"targets\":\"[\\\"1.2.3.4:9100\\\"]\"}" | j "d['id']")
[ -n "$SID" ] && ok "C 采集目标" || bad "C 采集目标" "$SID"
curl -s -X PUT $API/scrape-configs/$SID -H "$AUTH" -H 'Content-Type: application/json' -d '{"scrape_interval":"30s"}' >/dev/null
[ "$(curl -s $API/scrape-configs/$SID -H "$AUTH" | j "d['scrape_interval']")" = "30s" ] && ok "U 采集目标" || bad "U 采集目标"
RID=$(curl -s -X POST $API/recording-rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"m_rec\",\"expression\":\"sum(up)\"}" | j "d['id']")
[ -n "$RID" ] && ok "C 记录规则" || bad "C 记录规则" "$RID"
RID2=$(curl -s -X POST $API/remote-writes -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"m-rw\",\"url\":\"https://remote-write.invalid/api/v1/write\"}" | j "d['id']")
[ -n "$RID2" ] && ok "C 远端写入" || bad "C 远端写入" "$RID2"
[ "$(code "$API/remote-writes" -H "$AUTH")" = "200" ] && ok "R 远端写入列表" || bad "R 远端写入列表"
CFG_CODE=$(code -X POST $API/components/$COMP/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"node.yml","content":"a: 1","reason":"matrix target safety check"}')
[ "$CFG_CODE" = "400" ] && ok "S target组件配置变更安全拒绝" || bad "S target组件配置变更" "$CFG_CODE"
[ "$(code $API/change-plans -H "$AUTH")" = "200" ] && ok "R 配置变更计划" || bad "R 配置变更计划"
[ "$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: a\n    static_configs:\n      - targets: [\"1.2.3.4:9100\"]\n"}')" = "200" ] && ok "S 配置校验" || bad "S 配置校验"
[ "$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":":::bad"}')" != "200" ] && ok "S 校验非法拒绝" || bad "S 校验非法拒绝"

echo "--- 5. 告警模块 ---"
AR=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"m-rule-$TS\",\"expression\":\"up == 0\",\"severity\":\"warning\"}" | j "d['id']")
[ -n "$AR" ] && ok "C 告警规则" || bad "C 告警规则" "$AR"
curl -s -X PUT $API/alerts/rules/$AR -H "$AUTH" -H 'Content-Type: application/json' -d '{"severity":"critical"}' >/dev/null
curl -s "$API/alerts/rules" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);print([r['severity'] for r in d['rules'] if r['id']==$AR][0])" | grep -q critical && ok "U 告警规则" || bad "U 告警规则"
[ "$(code "$API/alerts/history?limit=5" -H "$AUTH")" = "200" ] && ok "R 告警历史" || bad "R 告警历史"
CH_BODY=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-ch-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/hook\\\"}\",\"enabled\":false}")
CH=$(echo "$CH_BODY" | j "d['id']")
[ -n "$CH" ] && ok "C 通知渠道草稿" || bad "C 通知渠道草稿" "$CH_BODY"
[ "$(echo "$CH_BODY" | j "d.get('apply_status','')")" = "draft" ] && ok "S 渠道草稿状态" || bad "S 渠道草稿状态" "$CH_BODY"
curl -s -X PUT $API/notifications/channels/$CH -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"m-ch-updated","enabled":false}' >/dev/null
curl -s "$API/notifications/channels" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);items=d.get('channels',d if isinstance(d,list) else []);print([c['name'] for c in items if c['id']==int($CH)][0])" | grep -q m-ch-updated && ok "U 更新渠道草稿" || bad "U 更新渠道草稿"
[ "$(code $API/notifications/history -H "$AUTH")" = "200" ] && ok "R 通知历史" || bad "R 通知历史"

echo "--- 6. 备份模块 ---"
BKR_CODE=$(code -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-bak-$TS\",\"type\":\"config\",\"component_id\":$COMP}")
[ "$BKR_CODE" = "400" ] && ok "S target组件备份合理拒绝" || bad "S target组件备份" "$BKR_CODE"
BK=0
[ "$(code $API/backups -H "$AUTH")" = "200" ] && ok "R 备份列表" || bad "R 备份列表"
[ "$(code $API/backups/platform -H "$AUTH")" = "200" ] && ok "R 平台快照" || bad "R 平台快照"
SBR_CODE=$(code -X POST $API/backups/scheduled -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-sb-$TS\",\"cron\":\"0 4 * * *\",\"type\":\"config\",\"keep_count\":3,\"enabled\":true,\"component_id\":$COMP}")
[ "$SBR_CODE" = "400" ] && ok "S target组件定时备份合理拒绝" || bad "S target组件定时备份" "$SBR_CODE"
SB=0

echo "--- 7. 包/版本/模板/维护窗口 ---"
[ "$(code $API/packages -H "$AUTH")" = "200" ] && ok "R 包列表" || bad "R 包列表"
[ "$(code $API/versions?type=prometheus -H "$AUTH")" = "200" ] && ok "R 版本列表" || bad "R 版本列表"
[ "$(code $API/operations -H "$AUTH")" = "200" ] && ok "R 生命周期操作记录" || bad "R 生命周期操作记录"
[ "$(code $API/stack-templates -H "$AUTH")" = "200" ] && ok "R 技术栈模板" || bad "R 技术栈模板"
TPL=$(curl -s -X POST $API/templates -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-tpl-$TS\",\"component_type\":\"prometheus\",\"content\":\"job: {{.job_name}}\"}" | j "d['id']")
[ -n "$TPL" ] && ok "C 模板" || bad "C 模板" "$TPL"
[ "$(code -X POST $API/templates/$TPL/render -H "$AUTH" -H 'Content-Type: application/json' -d '{"job_name":"x"}')" = "200" ] && ok "S 模板渲染" || bad "S 模板渲染"
MW=$(curl -s -X POST $API/maintenance -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-mw-$TS\",\"start_at\":\"$(date -d '+1 hour' +'%Y-%m-%dT%H:%M:%S%z' | sed 's/\(..\)$/:\1/')\",\"end_at\":\"$(date -d '+2 hour' +'%Y-%m-%dT%H:%M:%S%z' | sed 's/\(..\)$/:\1/')\"}" | j "d['id']")
[ -n "$MW" ] && ok "C 维护窗口" || bad "C 维护窗口" "$MW"
[ "$(code $API/maintenance -H "$AUTH")" = "200" ] && ok "R 维护窗口列表" || bad "R 维护窗口列表"

echo "--- 7b. 模板模块（新 UI） ---"
TPL2=$(curl -s -X POST $API/templates -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"mt2-$TS\",\"component_type\":\"prometheus\",\"content\":\"job: {{.job_name}}\"}" | j "d['id']")
[ -n "$TPL2" ] && ok "C 模板(component_type)" || bad "C 模板" "$TPL2"
R=$(curl -s -X POST $API/templates/$TPL2/render -H "$AUTH" -H 'Content-Type: application/json' -d '{"job_name":"render-check"}')
echo "$R" | grep -q "render-check" && ok "S 模板渲染" || bad "S 模板渲染" "$R"
curl -s -X PUT $API/templates/$TPL2 -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"job2: {{.job_name}}"}' >/dev/null
[ "$(curl -s $API/templates -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);items=d if isinstance(d,list) else d.get('templates',[]);print([t['content'] for t in items if t['id']==int($TPL2)][0])")" = "job2: {{.job_name}}" ] && ok "U 模板" || bad "U 模板"

echo "--- 7c. 仪表盘模块（新 UI） ---"
DASH=$(curl -s -X POST $API/dashboards -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"m-dash-$TS\",\"content\":\"{\\\"title\\\":\\\"test\\\"}\",\"enabled\":true}" | j "d['id']")
[ -n "$DASH" ] && ok "C 仪表盘" || bad "C 仪表盘" "$DASH"
[ "$(code "$API/dashboards" -H "$AUTH")" = "200" ] && ok "R 仪表盘列表" || bad "R 仪表盘列表"
[ "$(code -X PUT $API/dashboards/$DASH -H "$AUTH" -H 'Content-Type: application/json' -d '{"enabled":false}')" = "202" ] && ok "U 仪表盘停用已受理" || bad "U 仪表盘停用受理"
[ "$(code -X DELETE $API/dashboards/$DASH -H "$AUTH")" = "202" ] && ok "D 删除仪表盘已排队" || bad "D 删除仪表盘受理"
[ "$(code -X DELETE $API/templates/$TPL2 -H "$AUTH")" = "200" ] && ok "D 删除模板" || bad "D 删除模板"

echo "--- 8. 用户模块 ---"
USER_PASS="matrix-${TS}-Aa9"
U2=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"m-user-$TS\",\"password\":\"$USER_PASS\",\"role\":\"change_admin\"}" | j "d['id']")
[ -n "$U2" ] && ok "C 用户" || bad "C 用户" "$U2"
curl -s -X PUT $API/users/$U2 -H "$AUTH" -H 'Content-Type: application/json' -d '{"role":"viewer"}' >/dev/null
[ "$(curl -s $API/users/$U2 -H "$AUTH" | j "d['role']")" = "viewer" ] && ok "U 用户角色" || bad "U 用户角色"
curl -s -X PUT $API/users/$U2 -H "$AUTH" -H 'Content-Type: application/json' -d '{"is_active":false}' >/dev/null
curl -s "$API/users" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);print([u['is_active'] for u in d['users'] if u['id']==$U2][0])" | grep -q False && ok "U 禁用用户" || bad "U 禁用用户"

echo "--- 9. 删除矩阵 ---"
[ "$(code -X DELETE $API/scrape-configs/$SID -H "$AUTH")" = "202" ] && ok "D 删除采集目标已排队" || bad "D 删除采集目标受理"
[ "$(code -X DELETE $API/recording-rules/$RID -H "$AUTH")" = "202" ] && ok "D 删除记录规则已排队" || bad "D 删除记录规则受理"
[ "$(code -X DELETE $API/remote-writes/$RID2 -H "$AUTH")" = "202" ] && ok "D 删除远端写入已排队" || bad "D 删除远端写入受理"
[ "$(code -X DELETE $API/alerts/rules/$AR -H "$AUTH")" = "202" ] && ok "D 删除告警规则已排队" || bad "D 删除告警规则受理"
[ "$(code -X DELETE $API/notifications/channels/$CH -H "$AUTH")" = "200" ] && ok "D 删除渠道" || bad "D 删除渠道"
[ "$BK" = "0" ] && ok "D 删除备份（跳过：无备份）" || { [ "$(code -X DELETE $API/backups/$BK -H "$AUTH")" = "200" ] && ok "D 删除备份" || bad "D 删除备份"; }
[ "$SB" = "0" ] && ok "D 删除定时备份（跳过：无有效计划）" || { [ "$(code -X DELETE $API/backups/scheduled/$SB -H "$AUTH")" = "200" ] && ok "D 删除定时备份" || bad "D 删除定时备份"; }
[ "$(code $API/stats/enhanced -H "$AUTH")" = "200" ] && ok "R 清理后运行状态" || bad "R 清理后运行状态"
[ "$(code -X DELETE $API/templates/$TPL -H "$AUTH")" = "200" ] && ok "D 删除模板" || bad "D 删除模板"
[ "$(code -X DELETE $API/maintenance/$MW -H "$AUTH")" = "200" ] && ok "D 删除维护窗口" || bad "D 删除维护窗口"
[ "$(code -X DELETE $API/users/$U2 -H "$AUTH")" = "200" ] && ok "D 删除用户" || bad "D 删除用户"
[ "$(governed_code "$API/components/$COMP/uninstall" '{"mode":"keep_data","reason":"matrix cleanup"}' "$AUTH")" = "200" ] && ok "D 治理删除组件" || bad "D 治理删除组件"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code $API/node -H "$AUTH")"
# 只清理本套件创建的主机（勿删其他主机：纳管的远程主机会被误删）
for h in $HID $BATCH_IDS; do [ -n "$h" ] && curl -s -X DELETE "$API/hosts/$h?force=1" -H "$AUTH" >/dev/null; done
LEFT=$(curl -s $API/hosts -H "$AUTH" | python3 -c "import sys,json;print(len([x for x in json.load(sys.stdin)['hosts'] if x['id'] in [$HID]+[int(i) for i in '$BATCH_IDS'.split() if i.isdigit()]]))" 2>/dev/null)
[ "$LEFT" = "0" ] && ok "D 删除主机" || bad "D 删除主机" "残留 $LEFT"

echo ""
echo "===== 矩阵结果: $PASS passed, $FAIL failed ====="
echo ""
[ $FAIL -eq 0 ]
