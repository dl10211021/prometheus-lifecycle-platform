#!/bin/bash
# Supplemental lifecycle, restore, maintenance, validation, offline, metrics, and cascade tests.
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
DATA_DIR="${DATA_DIR:-/tmp/e2e-data}"
BASE_PATH="${BASE_PATH:-/tmp/e2e-base}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

TS=$(date +%s)
echo "=== Round-3 补充测试 (TS=$TS) ==="
TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录" "rate limited?"; exit 1; }

# 准备环境
HID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"r3-host-$TS\",\"ip\":\"127.0.0.30\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")

echo "--- A. 版本号校验 ---"
[ "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"node_exporter","name":"badver","deploy_type":"target","version":"../../etc/x","host_id":'"$HID"',"port":19300}')" = "400" ] && ok "非法版本号拒绝" || bad "非法版本号拒绝" "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"node_exporter","name":"badver","deploy_type":"target","version":"../../etc/x","host_id":'"$HID"',"port":19300}')"
[ "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"node_exporter","name":"badver2","deploy_type":"target","version":"v 1.0","host_id":'"$HID"',"port":19300}')" = "400" ] && ok "带空格版本拒绝" || bad "带空格版本拒绝"

echo "--- B. 组件编辑全参数（需要 node_exporter v1.8.2 离线包） ---"
BIN_182=$(curl -s "$API/packages?type=node_exporter" -H "$AUTH" | j "len([p for p in d.get('packages',[]) if p.get('version') in ('v1.8.2','1.8.2') and p.get('format') in ('binary','zip')])")
if [ "${BIN_182:-0}" -gt 0 ]; then
  COMP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"r3-ne\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HID,\"port\":19301}" | j "d['component']['id']")
  ST="pending"
  for i in $(seq 1 40); do ST=$(curl -s $API/components/$COMP -H "$AUTH" | j "d['status']"); [ "$ST" = "running" ] || [ "$ST" = "error" ] && break; sleep 3; done
  [ "$ST" = "running" ] && ok "binary 部署 running" || bad "binary 部署" "$ST"
  R=$(curl -s -X PUT $API/components/$COMP -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--x=1; reboot","apply_custom_args":true}')
  echo "$R" | grep -q "非法字符" && ok "编辑注入参数拒绝" || bad "编辑注入参数拒绝" "$R"
  R=$(curl -s -X PUT $API/components/$COMP -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--collector.systemd","apply_custom_args":true}')
  echo "$R" | grep -q '"id"' && ok "编辑 custom_args 保存" || bad "编辑 custom_args" "$R"
  sleep 4
  grep -q "collector.systemd" /etc/systemd/system/promlifecycle-node_exporter.service && ok "ApplyUnit 单元含新参数" || bad "ApplyUnit 单元参数" "$(grep ExecStart /etc/systemd/system/promlifecycle-node_exporter.service)"
  curl -s -X PUT $API/components/$COMP -H "$AUTH" -H 'Content-Type: application/json' -d '{"port":19302,"apply_custom_args":true}' >/dev/null
  sleep 4
  grep -q "listen-address=:19302" /etc/systemd/system/promlifecycle-node_exporter.service && ok "端口编辑触发单元重写" || bad "端口编辑" "$(grep ExecStart /etc/systemd/system/promlifecycle-node_exporter.service)"
  [ "$(code http://127.0.0.1:19302/metrics)" = "200" ] && ok "新端口指标可达" || bad "新端口指标" "$(code http://127.0.0.1:19302/metrics)"
  curl -s -X PUT $API/components/$COMP -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"","apply_custom_args":true}' >/dev/null
  sleep 4
  grep -q "collector.systemd" /etc/systemd/system/promlifecycle-node_exporter.service && bad "清空 custom_args" "仍在单元中" || ok "清空 custom_args 生效"
  python3 - "$DATA_DIR/promlifecycle.db" "$COMP" <<'PY'
import sqlite3,sys
with sqlite3.connect(sys.argv[1]) as con:
    con.execute("UPDATE components SET status='upgrading' WHERE id=?", (int(sys.argv[2]),))
PY
  [ "$(code -X POST $API/components/$COMP/restart -H "$AUTH")" = "409" ] && ok "upgrading 中重启拒绝" || bad "upgrading 重启拒绝" "$(code -X POST $API/components/$COMP/restart -H "$AUTH")"
  python3 - "$DATA_DIR/promlifecycle.db" "$COMP" <<'PY'
import sqlite3,sys
with sqlite3.connect(sys.argv[1]) as con:
    con.execute("UPDATE components SET status='running' WHERE id=?", (int(sys.argv[2]),))
PY
else
  COMP=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"r3-target-$TS\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HID,\"port\":19301}" | j "d['component']['id']")
  [ -n "$COMP" ] && ok "创建 target 组件用于后续边界测试" || bad "创建 target 组件" "$COMP"
  echo "  - binary 参数应用 (SKIP: 缺少 node_exporter v1.8.2 离线包)"
fi
echo "--- C. 维护窗口 E2E ---"
START=$(date -d "-1 min" +"%Y-%m-%dT%H:%M:%S%z" | sed 's/\(..\)$/:\1/')
END=$(date -d "+10 min" +"%Y-%m-%dT%H:%M:%S%z" | sed 's/\(..\)$/:\1/')
MW=$(curl -s -X POST $API/maintenance -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"r3-mw-$TS\",\"start_at\":\"$START\",\"end_at\":\"$END\",\"reason\":\"round3\"}" | j "d['id']")
[ -n "$MW" ] && ok "创建维护窗口 id=$MW" || bad "创建维护窗口" "$MW"
# 窗口校验：end<=start 拒绝
[ "$(code -X POST $API/maintenance -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"bad-mw\",\"start_at\":\"$END\",\"end_at\":\"$START\"}")" = "400" ] && ok "时间倒置拒绝" || bad "时间倒置拒绝"
# 等待 watcher 建 silence（需 AM 组件——无 AM 时不应 500）
sleep 33
curl -s $API/maintenance -H "$AUTH" | grep -q '"status":' && ok "维护窗口返回实际收敛状态" || bad "维护窗口状态契约"
curl -s -X DELETE $API/maintenance/$MW -H "$AUTH" | grep -q "已删除" && ok "删除维护窗口" || bad "删除维护窗口"

echo "--- D. 平台备份恢复链路 ---"
NODE_BEFORE_BACKUP=$(curl -s $API/node -H "$AUTH" | j "d.get('id','')")
[ -n "$NODE_BEFORE_BACKUP" ] && ok "平台快照前节点可读" || bad "平台快照前节点" "$NODE_BEFORE_BACKUP"
# 列表
[ "$(code $API/backups/platform -H "$AUTH")" = "200" ] && ok "平台备份列表 200" || bad "平台备份列表"
# 非法恢复名
[ "$(code -X POST "$API/backups/platform/restore?name=../../etc/passwd" -H "$AUTH")" = "400" ] && ok "恢复路径穿越拒绝" || bad "恢复路径穿越" "$(code -X POST "$API/backups/platform/restore?name=../../etc/passwd" -H "$AUTH")"
# 坏文件拒绝（伪造快照）
echo "not sqlite" > "$BASE_PATH/backups/platform-db-fake.db"
[ "$(code -X POST "$API/backups/platform/restore?name=platform-db-fake.db" -H "$AUTH")" = "400" ] && ok "非 SQLite 快照拒绝" || bad "非 SQLite 快照" "$(code -X POST "$API/backups/platform/restore?name=platform-db-fake.db" -H "$AUTH")"
rm -f "$BASE_PATH/backups/platform-db-fake.db"
# viewer 越权
VIEW_PASS="viewer-${TS}-Aa9"
VID=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"r3-viewer-$TS\",\"password\":\"$VIEW_PASS\",\"role\":\"viewer\"}" | j "d['id']")
VTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"r3-viewer-$TS\",\"password\":\"$VIEW_PASS\"}" | j "d['token']")
[ "$(code -X POST "$API/backups/platform/restore?name=x" -H "Authorization: Bearer $VTOKEN")" = "403" ] && ok "viewer 恢复 403" || bad "viewer 恢复 403" "$(code -X POST "$API/backups/platform/restore?name=x" -H "Authorization: Bearer $VTOKEN")"
[ "$(code "$API/backups/platform/download?name=x" -H "Authorization: Bearer $VTOKEN")" = "403" ] && ok "viewer 下载 403" || bad "viewer 下载 403"

echo "--- E. 通知渠道配置状态 ---"
R=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"r3-mail-'"$TS"'","type":"email","config":"{\"to\":\"ops@x.com\"}","enabled":true}')
echo "$R" | grep -q "SMTP" && ok "无 SMTP 配置给出可操作提示" || bad "邮件提示" "$R"
R=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"r3-mail2-'"$TS"'","type":"email","config":"{}","enabled":true}')
{ echo "$R" | grep -q "SMTP" || echo "$R" | grep -q "收件人"; } && ok "邮件配置缺失提示（SMTP/收件人）" || bad "邮件提示" "$R"
CH3_BODY=$(curl -s -X POST $API/notifications/channels -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"r3-wh-$TS\",\"type\":\"webhook\",\"config\":\"{\\\"webhook\\\":\\\"https://example.invalid/hook\\\"}\",\"enabled\":false}")
CH3=$(echo "$CH3_BODY" | j "d.get('id','')")
[ -n "$CH3" ] && [ "$(echo "$CH3_BODY" | j "d.get('apply_status','')")" = "draft" ] && ok "webhook 草稿可预配置" || bad "webhook 草稿" "$CH3_BODY"

echo "--- F. 资源监控字段 ---"
R=$(curl -s $API/components/$COMP/resource -H "$AUTH")
echo "$R" | grep -q '"name"' && ok "资源接口含 name 字段" || bad "资源接口 name 字段" "$R"
echo "$R" | python3 -c "import sys,json;d=json.load(sys.stdin);print('  cpu:',d.get('cpu_usage'),'disk%:',d.get('disk_usage'))" 2>/dev/null

echo "--- G. 组件级联删除 ---"
[ "$(code -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"r3-bak-$TS\",\"type\":\"config\",\"component_id\":$COMP}")" = "400" ] && ok "无配置路径备份安全拒绝" || bad "无配置路径备份"
# 单组件删除应级联 Backup
governed_action "$API/components/$COMP/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null
python3 -c "
import sqlite3
con = sqlite3.connect('$DATA_DIR/promlifecycle.db')
n = con.execute('SELECT COUNT(*) FROM backups WHERE component_id=$COMP').fetchone()[0]
print('组件删除后残留 backups:', n)
" > /tmp/r3-cascade.txt
grep -q "残留 backups: 0" /tmp/r3-cascade.txt && ok "单组件删除级联清 backups" || bad "组件删除级联" "$(cat /tmp/r3-cascade.txt)"
[ "$(code $API/components/$COMP -H "$AUTH")" = "404" ] && ok "删除后组件记录不存在" || bad "组件记录清理" "$(code $API/components/$COMP -H "$AUTH")"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "组件清理后节点保持可用" || bad "组件清理后节点" "$(code $API/node -H "$AUTH")"
[ "$(code -X DELETE $API/hosts/$HID -H "$AUTH")" = "200" ] && ok "删除主机" || bad "删除主机" "$(code -X DELETE $API/hosts/$HID -H "$AUTH")"
curl -s -X DELETE $API/users/$VID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/notifications/channels/$CH3 -H "$AUTH" >/dev/null

echo "--- H. /metrics 内容 ---"
curl -s $BASE/metrics | grep -q "promlifecycle_hosts" && ok "/metrics 含业务指标" || bad "/metrics 内容" "$(curl -s $BASE/metrics | head -3)"
curl -s $BASE/metrics | grep -qE "^promlifecycle_hosts [0-9]+" && ok "/metrics 数值格式正确" || bad "/metrics 格式"

echo ""
echo "Round-3 补充测试: $PASS passed, $FAIL failed"
echo ""
[ $FAIL -eq 0 ]
