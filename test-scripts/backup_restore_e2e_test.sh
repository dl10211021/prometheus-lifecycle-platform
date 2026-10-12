#!/bin/bash
# Backup/restore lifecycle end-to-end test.
#
# 覆盖（此前仅状态码级检查，无内容级验证）：
#   A. 组件版本回滚 auto-select：升级后不带 target_version 回滚 → 自动选上一版本
#   B. 配置版本回滚：两次改配置产生 v2/v3 → 回滚到 v2 → 磁盘内容与激活版本核对
#   C. 配置备份→恢复：备份基线 → 改配置 → 恢复 → 内容回到基线且服务健康
#   D. 数据备份→恢复：埋标记文件 → 备份 → 删标记 → 恢复 → 标记复现且服务健康
#   E. 负路径：无目标 400 / 空节点 400 / 恢复不存在 404 / 删除备份同时清磁盘产物
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
DATA_DIR="${DATA_DIR:-/tmp/e2e-data}"
TS=$(date +%s)
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j()    { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' -m 30 "$@"; }

cleanup() {
  [ -n "${COMP_ID:-}" ] && governed_action "$API/components/$COMP_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null 2>&1
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code $API/node -H "$AUTH")"
[ "$(code $API/node -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code $API/node -H "$AUTH")"
  [ -n "${HOST_ID:-}" ] && curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" >/dev/null 2>&1
  rm -f /backups/bre-$TS* /var/lib/prometheus/bre-marker-$TS.txt 2>/dev/null
}
trap cleanup EXIT

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] || { echo "登录失败"; exit 1; }
PROM_253=$(curl -s "$API/packages?type=prometheus" -H "$AUTH" | j "len([p for p in d.get('packages',[]) if p.get('version') in ('v2.53.0','2.53.0') and p.get('format') in ('binary','zip')])")
PROM_255=$(curl -s "$API/packages?type=prometheus" -H "$AUTH" | j "len([p for p in d.get('packages',[]) if p.get('version') in ('v2.55.0','2.55.0') and p.get('format') in ('binary','zip')])")
if [ "${PROM_253:-0}" -eq 0 ] || [ "${PROM_255:-0}" -eq 0 ]; then
  echo "SKIP: backup/restore binary E2E requires Prometheus v2.53.0 and v2.55.0 offline packages"
  exit 0
fi

echo "=== 0. 准备（主机/节点/Prometheus 二进制部署）==="
HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"bre-host-$TS\",\"ip\":\"127.0.0.40\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"type\":\"prometheus\",\"name\":\"bre-prom-$TS\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HOST_ID,\"port\":19090}" | j "d['component']['id']")
[ -n "$COMP_ID" ] && ok "组件创建 id=$COMP_ID" || { bad "组件创建" "empty"; exit 1; }
STATUS=pending
for i in $(seq 1 60); do
  STATUS=$(curl -s $API/components/$COMP_ID -H "$AUTH" | j "d['status']")
  { [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ]; } && break
  sleep 3
done
[ "$STATUS" = "running" ] && ok "部署 running" || { bad "部署" "$STATUS"; exit 1; }
HEALTH=$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:19090/-/healthy)
[ "$HEALTH" = "200" ] && ok "Prometheus 健康" || bad "Prometheus 健康" "$HEALTH"

echo "=== A. 版本回滚 auto-select（不带 target_version）==="
governed_action "$API/components/$COMP_ID/upgrade" '{"new_version":"v2.55.0","reason":"test lifecycle"}' "$AUTH" >/dev/null
sleep 8
V=$(curl -s -m 5 http://127.0.0.1:19090/metrics 2>/dev/null | grep '^prometheus_build_info' | grep -o 'version="2\.55\.0"')
[ -n "$V" ] && ok "升级到 v2.55.0" || bad "升级 v2.55.0" "$(curl -s -m 5 http://127.0.0.1:19090/metrics | grep '^prometheus_build_info' | head -1)"
RB_CODE=$(governed_code "$API/components/$COMP_ID/rollback" '{"reason":"backup rollback verification"}' "$AUTH")
RBV=$(curl -s $API/components/$COMP_ID -H "$AUTH" | j "d.get('version','')")
[ "$RB_CODE" = "200" ] && [ "$RBV" = "v2.53.0" ] && ok "auto-select 回滚目标 v2.53.0" || bad "auto-select 目标" "code=$RB_CODE version=$RBV"
sleep 8
V2=$(curl -s -m 5 http://127.0.0.1:19090/metrics 2>/dev/null | grep '^prometheus_build_info' | grep -o 'version="2\.53\.0"')
[ -n "$V2" ] && ok "回滚后实际运行 v2.53.0" || bad "回滚后版本" "$(curl -s -m 5 http://127.0.0.1:19090/metrics | grep '^prometheus_build_info' | head -1)"
ST=$(curl -s $API/components/$COMP_ID -H "$AUTH" | j "d['status']")
[ "$ST" = "running" ] && ok "回滚后状态 running" || bad "回滚后状态" "$ST"

echo "=== B. 配置版本回滚（内容级验证）==="
BASELINE=$(cat /etc/prometheus/prometheus.yml)
governed_action "$API/components/$COMP_ID/configs" "{\"name\":\"prometheus.yml\",\"content\":$(python3 -c "import json,sys;print(json.dumps(sys.stdin.read()+'\n# BRE_MARKER_A_$TS'))" <<< "$BASELINE"),\"reason\":\"backup restore marker A\"}" "$AUTH" >/dev/null
R2=$(curl -s $API/components/$COMP_ID/configs -H "$AUTH" | j "max([x['version'] for x in d.get('configs',[]) if x.get('name')=='prometheus.yml'], default=0)")
[ "$R2" -gt 0 ] && ok "配置 v$R2（marker A）" || bad "配置 marker A" "$R2"
governed_action "$API/components/$COMP_ID/configs" "{\"name\":\"prometheus.yml\",\"content\":$(python3 -c "import json,sys;print(json.dumps(sys.stdin.read()+'\n# BRE_MARKER_B_$TS'))" <<< "$BASELINE"),\"reason\":\"backup restore marker B\"}" "$AUTH" >/dev/null
R3=$(curl -s $API/components/$COMP_ID/configs -H "$AUTH" | j "max([x['version'] for x in d.get('configs',[]) if x.get('name')=='prometheus.yml'], default=0)")
[ "$R3" -gt "$R2" ] && ok "配置 v$R3（marker B）" || bad "配置 marker B" "$R3"
grep -q "BRE_MARKER_B_$TS" /etc/prometheus/prometheus.yml && ok "磁盘为 marker B 内容" || bad "磁盘 marker B" "$(tail -1 /etc/prometheus/prometheus.yml)"
sleep 3
CRB=$(governed_code "$API/configs/$COMP_ID/rollback" '{"config_name":"prometheus.yml","version":1,"reason":"test rollback"}' "$AUTH")
[ "$CRB" = "200" ] && ok "配置回滚到 v1 → 200" || bad "配置回滚" "$CRB"
sleep 3
grep -q "BRE_MARKER_A_$TS" /etc/prometheus/prometheus.yml && ! grep -q "BRE_MARKER_B_$TS" /etc/prometheus/prometheus.yml \
  && ok "磁盘回到 marker A 内容" || bad "磁盘 marker A" "$(tail -1 /etc/prometheus/prometheus.yml)"
ACTIVE_V=$(sqlite3 "$DATA_DIR/promlifecycle.db" "SELECT version FROM component_configs WHERE component_id=$COMP_ID AND name='prometheus.yml' AND is_active=1" 2>/dev/null)
ACTIVE_CONTENT=$(sqlite3 "$DATA_DIR/promlifecycle.db" "SELECT content FROM component_configs WHERE component_id=$COMP_ID AND name='prometheus.yml' AND is_active=1" 2>/dev/null)
[ "$ACTIVE_V" -gt "$R3" ] && echo "$ACTIVE_CONTENT" | grep -q "BRE_MARKER_A_$TS" && ok "回滚生成新活动版本 v$ACTIVE_V（内容来自 v$R2）" || bad "激活版本" "v=$ACTIVE_V"
HEALTH=$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:19090/-/healthy)
[ "$HEALTH" = "200" ] && ok "配置回滚后服务健康" || bad "配置回滚后健康" "$HEALTH"

echo "=== C. 配置备份 → 变更 → 恢复（内容级验证）==="
BAK=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"bre-$TS-cfg\",\"type\":\"config\",\"component_id\":$COMP_ID}")
BAK_ID=$(echo "$BAK" | j "d['backup']['id']")
BAK_PATH=$(echo "$BAK" | j "d['backup']['path']")
[ -n "$BAK_ID" ] && [ -f "$BAK_PATH" ] && ok "配置备份落盘 $BAK_PATH" || bad "配置备份" "$BAK"
grep -q "BRE_MARKER_A_$TS" "$BAK_PATH" && ok "备份内容 = 当前(marker A)" || bad "备份内容" "$(tail -1 "$BAK_PATH")"
governed_action "$API/components/$COMP_ID/configs" "{\"name\":\"prometheus.yml\",\"content\":$(python3 -c "import json,sys;print(json.dumps(sys.stdin.read()+'\n# BRE_MARKER_C_$TS'))" <<< "$BASELINE"),\"reason\":\"backup restore marker C\"}" "$AUTH" >/dev/null
sleep 3
grep -q "BRE_MARKER_C_$TS" /etc/prometheus/prometheus.yml && ok "配置已变更为 marker C" || bad "变更 marker C" "$(tail -1 /etc/prometheus/prometheus.yml)"
RST=$(curl -s -X POST $API/backups/$BAK_ID/restore -H "$AUTH")
echo "$RST" | grep -q "恢复完成" && ok "配置恢复接口 200" || bad "配置恢复" "$RST"
sleep 3
grep -q "BRE_MARKER_A_$TS" /etc/prometheus/prometheus.yml && ! grep -q "BRE_MARKER_C_$TS" /etc/prometheus/prometheus.yml \
  && ok "恢复后磁盘回到 marker A" || bad "恢复后内容" "$(tail -1 /etc/prometheus/prometheus.yml)"
ACTIVE=$(systemctl is-active promlifecycle-prometheus 2>&1)
[ "$ACTIVE" = "active" ] && ok "恢复后 systemd active" || bad "恢复后 systemd" "$ACTIVE"

echo "=== D. 数据备份 → 删标记 → 恢复（内容级验证）==="
touch /var/lib/prometheus/bre-marker-$TS.txt
chown nobody:$(id -gn nobody) /var/lib/prometheus/bre-marker-$TS.txt
DBAK=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"bre-$TS-data\",\"type\":\"data\",\"component_id\":$COMP_ID}")
DBAK_ID=$(echo "$DBAK" | j "d['backup']['id']")
DBAK_PATH=$(echo "$DBAK" | j "d['backup']['path']")
DSIZE=$(echo "$DBAK" | j "d['backup']['size']")
[ -n "$DBAK_ID" ] && [ -s "$DBAK_PATH" ] && [ "${DSIZE:-0}" -gt 0 ] && ok "数据备份落盘 size=$DSIZE" || bad "数据备份" "$DBAK"
tar -tzf "$DBAK_PATH" 2>/dev/null | grep -q "bre-marker-$TS.txt" && ok "归档包含标记文件" || bad "归档标记" "$(tar -tzf "$DBAK_PATH" | head -3)"
rm -f /var/lib/prometheus/bre-marker-$TS.txt
RST2=$(curl -s -X POST $API/backups/$DBAK_ID/restore -H "$AUTH")
echo "$RST2" | grep -q "恢复完成" && ok "数据恢复接口 200" || bad "数据恢复" "$RST2"
sleep 3
[ -f /var/lib/prometheus/bre-marker-$TS.txt ] && ok "标记文件已复现" || bad "标记复现" "missing"
HEALTH=$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:19090/-/healthy)
[ "$HEALTH" = "200" ] && ok "数据恢复后服务健康" || bad "数据恢复后健康" "$HEALTH"

echo "=== E. 负路径与清理 ==="
AUTO_BAK_RESP=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d '{}')
AUTO_BAK_ID=$(echo "$AUTO_BAK_RESP" | j "d.get('backup',{}).get('id','')")
[ -n "$AUTO_BAK_ID" ] && ok "单组件工作区自动选择备份目标" || bad "自动选择备份目标" "$AUTO_BAK_RESP"
[ -z "$AUTO_BAK_ID" ] || curl -s -X DELETE $API/backups/$AUTO_BAK_ID -H "$AUTH" >/dev/null
[ "$(code -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"component_id\":$COMP_ID,\"type\":\"invalid\"}")" = "400" ] && ok "非法备份范围 → 400" || bad "非法备份范围" "$(code -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"component_id\":$COMP_ID,\"type\":\"invalid\"}")"
[ "$(code -X POST $API/backups/999999/restore -H "$AUTH")" = "404" ] && ok "恢复不存在备份 → 404" || bad "恢复不存在" "$(code -X POST $API/backups/999999/restore -H "$AUTH")"
[ "$(code -X DELETE $API/backups/999999 -H "$AUTH")" = "404" ] && ok "删除不存在备份 → 404" || bad "删除不存在" "$(code -X DELETE $API/backups/999999 -H "$AUTH")"
LISTED=$(curl -s $API/backups -H "$AUTH" | j "len([x for x in d['backups'] if x['id'] in ($BAK_ID,$DBAK_ID)])")
[ "$LISTED" = "2" ] && ok "备份列表包含两条记录" || bad "备份列表" "$LISTED"
curl -s -X DELETE $API/backups/$BAK_ID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/backups/$DBAK_ID -H "$AUTH" >/dev/null
[ ! -f "$BAK_PATH" ] && [ ! -f "$DBAK_PATH" ] && ok "删除备份同时清理磁盘产物" || bad "磁盘产物清理" "$(ls -la "$BAK_PATH" "$DBAK_PATH" 2>&1)"

echo ""
echo "Backup/Restore E2E: $PASS passed, $FAIL failed"
[ $FAIL -eq 0 ]
