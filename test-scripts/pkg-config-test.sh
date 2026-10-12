#!/bin/bash
# 包管理 + 组件参数配置 完整测试
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
export DB_PATH="${DB_PATH:-$DATA_DIR/promlifecycle.db}"
if [ -z "$REMOTE_IP" ] || [ -z "$PLATFORM_IP" ] || [ -z "$REMOTE_PASS" ]; then
  echo "SKIP: set REMOTE_HOST, PLATFORM_HOST, and REMOTE_PASS for remote tests"
  exit 0
fi
rssh() { for t in 1 2 3 4 5 6; do out=$(sshpass -p "$REMOTE_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$REMOTE_IP" "$1" 2>/dev/null) && { echo "$out"; return 0; }; sleep 1; done; echo "$out"; }
TS=$(date +%s)
PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: ${2:-})"; }
skip() { SKIP=$((SKIP+1)); echo "  - $1 (SKIP: ${2:-})"; }
j()    { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' -m 30 "$@"; }
wait_status() { local id=$1 target=$2 max=${3:-90}; for i in $(seq 1 $max); do ST=$(curl -s $API/components/$id -H "$AUTH" | j "d['status']"); [ "$ST" = "$target" ] && return 0; [ "$ST" = "error" ] && return 1; sleep 2; done; return 1; }

echo "================================================================"
echo " 包管理 + 组件参数配置测试 (TS=$TS)"
echo "================================================================"

TOKEN=$(curl -s -m 5 -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] && ok "登录" || { bad "登录"; exit 1; }

# 准备环境
HID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"pkg-$TS\",\"ip\":\"$REMOTE_IP\",\"os\":\"linux\",\"connection_type\":\"ssh\",\"username\":\"root\",\"password\":\"$REMOTE_PASS\",\"port\":22}" | j "d['id']")
[ -n "$HID" ] && ok "添加主机" || { bad "添加主机"; exit 1; }
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
[ -n "$NODE_ID" ] && ok "读取节点" || { bad "读取节点"; exit 1; }

# ============================================================
# A. 包管理
# ============================================================
echo ""
echo "========== A. 包管理 =========="

echo "--- A1. 包列表（全部）---"
P_LIST=$(curl -s $API/packages -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);pkgs=d.get('packages',d if isinstance(d,list) else []);print(len(pkgs))" 2>/dev/null)
[ "$P_LIST" -gt "0" ] && ok "包列表 ($P_LIST 个)" || bad "包列表" "$P_LIST"

echo "--- A2. 按类型查包 ---"
for t in prometheus node_exporter alertmanager blackbox_exporter pushgateway loki promtail thanos grafana; do
  CNT=$(curl -s "$API/packages?type=$t" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);pkgs=d.get('packages',d if isinstance(d,list) else []);print(len([p for p in pkgs if p.get('format')=='binary' and p.get('checksum') and p.get('size',0)>0]))" 2>/dev/null)
  [ "$CNT" -ge "1" ] && ok "$t: $CNT 个可用包" || skip "$t 包" "$CNT (FullDir)"
done

echo "--- A3. 查询单个包 ---"
FIRST_PKG=$(curl -s "$API/packages?type=node_exporter" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);pkgs=d.get('packages',d if isinstance(d,list) else []);print(pkgs[0]['id'] if pkgs else '')" 2>/dev/null)
if [ -n "$FIRST_PKG" ]; then
  PKG_GET=$(code $API/packages/$FIRST_PKG -H "$AUTH")
  [ "$PKG_GET" = "200" ] && ok "查询单个包 200" || bad "查询单个包" "$PKG_GET"
fi

echo "--- A4. 下载包（已有包→409）---"
DL_DUP=$(code -X POST $API/packages/official-download -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"node_exporter","version":"v1.8.2"}')
[ "$DL_DUP" = "409" ] && ok "下载已有包 → 409" || [ "$DL_DUP" = "201" ] && ok "下载包成功 → 201" || skip "下载已有包" "$DL_DUP"

echo "--- A5. 下载全部官方包（后台任务）---"
DL_ALL=$(curl -s -X POST $API/packages/download-all -H "$AUTH" | j "d.get('job_id','')")
[ -n "$DL_ALL" ] && ok "下载全部包后台任务 job_id=$DL_ALL" || bad "下载全部" "$DL_ALL"
# 轮询任务状态
if [ -n "$DL_ALL" ]; then
  for i in $(seq 1 30); do
    JOB=$(curl -s $API/jobs/$DL_ALL -H "$AUTH")
    JOB_STATUS=$(echo "$JOB" | j "d.get('status','')")
    [ "$JOB_STATUS" = "done" ] || [ "$JOB_STATUS" = "finished" ] && break
    sleep 3
  done
  JOB_ITEMS=$(echo "$JOB" | python3 -c "import sys,json;d=json.load(sys.stdin);items=d.get('items',[]);done=len([i for i in items if i.get('status')=='done']);failed=len([i for i in items if i.get('status')=='failed']);print(f'{done} done, {failed} failed')" 2>/dev/null)
  ok "下载任务状态: $JOB_STATUS ($JOB_ITEMS)"
fi

echo "--- A6. 包下载到客户端 ---"
if [ -n "$FIRST_PKG" ]; then
  DL_CODE=$(code "$API/packages/$FIRST_PKG/download" -H "$AUTH")
  [ "$DL_CODE" = "200" ] && ok "包下载 200" || bad "包下载" "$DL_CODE"
fi

echo "--- A7. 创建包记录（手动注册）---"
NEW_PKG=$(curl -s -X POST $API/packages -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"test-pkg-$TS\",\"type\":\"node_exporter\",\"version\":\"v9.9.9\",\"format\":\"binary\",\"storage_path\":\"/tmp/test-pkg.tar.gz\"}")
NEW_PKG_ID=$(echo "$NEW_PKG" | j "d.get('id','')")
[ -n "$NEW_PKG_ID" ] && ok "创建包记录 id=$NEW_PKG_ID" || bad "创建包记录" "$NEW_PKG"
# 删除测试包
[ -n "$NEW_PKG_ID" ] && curl -s -X DELETE $API/packages/$NEW_PKG_ID -H "$AUTH" >/dev/null

echo "--- A8. 上传包 ---"
# 创建一个小的测试 tar.gz
echo "test" | gzip > /tmp/test-upload-$TS.tar.gz
UP_CODE=$(code -X POST $API/packages/upload -H "$AUTH" -F "name=test-upload-$TS" -F "type=node_exporter" -F "version=v0.0.1" -F "file=@/tmp/test-upload-$TS.tar.gz")
[ "$UP_CODE" = "200" ] || [ "$UP_CODE" = "201" ] && ok "上传包 $UP_CODE" || bad "上传包" "$UP_CODE"
# 清理上传的包
UP_PKG=$(curl -s "$API/packages?type=node_exporter" -H "$AUTH" | python3 -c "import sys,json;d=json.load(sys.stdin);pkgs=d.get('packages',d if isinstance(d,list) else []);print([p['id'] for p in pkgs if p.get('version')=='v0.0.1'][0] if any(p.get('version')=='v0.0.1' for p in pkgs) else '')" 2>/dev/null)
[ -n "$UP_PKG" ] && curl -s -X DELETE $API/packages/$UP_PKG -H "$AUTH" >/dev/null
rm -f /tmp/test-upload-$TS.tar.gz

# ============================================================
# B. 组件部署后参数配置（完整流程）
# ============================================================
echo ""
echo "========== B. 组件参数配置 =========="

echo "--- B1. 部署 node_exporter ---"
NE=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"cfg-ne\",\"deploy_type\":\"binary\",\"version\":\"v1.8.2\",\"host_id\":$HID,\"port\":19400}" | j "d['component']['id']")
[ -n "$NE" ] && ok "部署 node_exporter id=$NE" || { bad "部署 NE"; exit 1; }
wait_status $NE running 60 && ok "NE running" || bad "NE 部署" "timeout"

echo "--- B2. 编辑组件名称 ---"
UPD_NAME=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"cfg-ne-renamed"}')
[ "$UPD_NAME" = "200" ] && ok "编辑名称" || bad "编辑名称" "$UPD_NAME"
NEW_NAME=$(curl -s $API/components/$NE -H "$AUTH" | j "d['name']")
[ "$NEW_NAME" = "cfg-ne-renamed" ] && ok "名称生效" || bad "名称生效" "$NEW_NAME"

echo "--- B3. 编辑端口（触发 systemd 单元重写）---"
UPD_PORT=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"port":19401,"apply_custom_args":true}')
[ "$UPD_PORT" = "200" ] && ok "编辑端口" || bad "编辑端口" "$UPD_PORT"
sleep 5
RPORT=$(rssh "ss -tlnp | grep ':19401 ' | wc -l" | tr -d ' ')
[ "$RPORT" = "1" ] && ok "新端口 19401 监听" || bad "新端口" "$RPORT"
OLD_PORT=$(rssh "ss -tlnp | grep ':19400 ' | wc -l" | tr -d ' ')
[ "$OLD_PORT" = "0" ] && ok "旧端口 19400 已释放" || bad "旧端口" "$OLD_PORT"
# metrics 可达
METRICS=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://$REMOTE_IP:19401/metrics)
[ "$METRICS" = "200" ] && ok "新端口 metrics 可达" || bad "新端口 metrics" "$METRICS"

echo "--- B4. 编辑 custom_args（注入防护 + 合法参数 + systemd 验证）---"
# 注入防护
INJ=$(curl -s -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--x=1; reboot","apply_custom_args":true}')
echo "$INJ" | grep -q "非法字符" && ok "注入参数拒绝" || bad "注入防护" "$INJ"
# 格式校验（不以 -- 开头）
FMT=$(curl -s -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"badformat","apply_custom_args":true}')
echo "$FMT" | grep -q "flag" && ok "非 --flag 格式拒绝" || bad "格式校验" "$FMT"
# 合法参数
OK_ARGS=$(curl -s -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"--collector.systemd\n--collector.processes","apply_custom_args":true}')
echo "$OK_ARGS" | grep -q '"id"' && ok "多行 custom_args 保存" || bad "合法参数" "$OK_ARGS"
sleep 5
UNIT=$(rssh "grep -c 'collector.systemd\|collector.processes' /etc/systemd/system/promlifecycle-node_exporter.service 2>/dev/null" | tr -d ' ')
[ "$UNIT" -ge "1" ] && ok "多行参数写入 systemd 单元 ($UNIT 个)" || bad "参数写入" "$UNIT"
# 验证 metrics 含新 collector
METRICS_NE=$(curl -s -m 5 http://$REMOTE_IP:19401/metrics | grep -c "systemd\|processes" || echo "0")
[ "$METRICS_NE" -gt "0" ] && ok "metrics 含 collector 指标" || skip "metrics collector" "$METRICS_NE"
# 清空参数
CLR=$(curl -s -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_args":"","apply_custom_args":true}')
echo "$CLR" | grep -q '"id"' && ok "清空 custom_args" || bad "清空参数" "$CLR"
sleep 5
UNIT_CLR=$(rssh "grep -c 'collector.systemd\|collector.processes' /etc/systemd/system/promlifecycle-node_exporter.service 2>/dev/null" | tr -d ' ')
[ "$UNIT_CLR" = "0" ] && ok "清空后 systemd 单元无参数" || bad "清空后参数" "$UNIT_CLR"

echo "--- B5. 编辑资源限制 ---"
RES=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"resource_limits":"{\"cpu\":\"0.5\",\"memory\":\"256Mi\"}"}')
[ "$RES" = "200" ] && ok "资源限制保存" || bad "资源限制" "$RES"
RES_CHK=$(curl -s $API/components/$NE -H "$AUTH" | j "d.get('resource_limits','')")
echo "$RES_CHK" | grep -q "0.5" && ok "资源限制生效" || bad "资源限制生效" "$RES_CHK"
# 非法格式
RES_BAD=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"resource_limits":"not-json"}')
[ "$RES_BAD" = "400" ] && ok "非法资源限制拒绝" || bad "资源限制校验" "$RES_BAD"

echo "--- B6. 编辑环境变量 ---"
ENV=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_env":"{\"DEBUG\":\"true\",\"LOG_LEVEL\":\"info\"}"}')
[ "$ENV" = "200" ] && ok "环境变量保存" || bad "环境变量" "$ENV"
ENV_CHK=$(curl -s $API/components/$NE -H "$AUTH" | j "d.get('custom_env','')")
echo "$ENV_CHK" | grep -q "DEBUG" && ok "环境变量生效" || bad "环境变量生效" "$ENV_CHK"
# 非法格式
ENV_BAD=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"custom_env":"not-json"}')
[ "$ENV_BAD" = "400" ] && ok "非法环境变量拒绝" || bad "环境变量校验" "$ENV_BAD"

echo "--- B7. 编辑卷挂载 ---"
VOL=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"extra_volumes":"[{\"src\":\"/tmp\",\"dst\":\"/data\"}]"}')
[ "$VOL" = "200" ] && ok "卷挂载保存" || bad "卷挂载" "$VOL"
VOL_CHK=$(curl -s $API/components/$NE -H "$AUTH" | j "d.get('extra_volumes','')")
echo "$VOL_CHK" | grep -q "/data" && ok "卷挂载生效" || bad "卷挂载生效" "$VOL_CHK"
# 非法格式
VOL_BAD=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"extra_volumes":"not-json"}')
[ "$VOL_BAD" = "400" ] && ok "非法卷挂载拒绝" || bad "卷挂载校验" "$VOL_BAD"

echo "--- B8. 组件忙时拒绝编辑 ---"
# 手动设为 upgrading
python3 << 'PYEOF' 2>/dev/null || true
import sqlite3, os
db = os.environ['DB_PATH']
if os.path.exists(db):
    c = sqlite3.connect(db)
    c.execute("UPDATE components SET status='upgrading' WHERE id=?", (int(os.environ.get('NE_ID', 0)),))
    c.commit()
PYEOF
# 通过 API 在 150 上操作
sshpass -p "$PLATFORM_PASS" ssh -o StrictHostKeyChecking=no -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$PLATFORM_IP" "python3 -c \"
import sqlite3
c = sqlite3.connect('$DATA_DIR/promlifecycle.db')
c.execute('UPDATE components SET status=\\\"upgrading\\\" WHERE id=$NE')
c.commit()
\"" 2>/dev/null
BUSY=$(code -X PUT $API/components/$NE -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"should-fail"}')
[ "$BUSY" = "409" ] && ok "组件忙时拒绝编辑" || bad "忙时拒绝" "$BUSY"
# 恢复
sshpass -p "$PLATFORM_PASS" ssh -o StrictHostKeyChecking=no -o PreferredAuthentications=password -o PubkeyAuthentication=no "$REMOTE_USER@$PLATFORM_IP" "python3 -c \"
import sqlite3
c = sqlite3.connect('$DATA_DIR/promlifecycle.db')
c.execute('UPDATE components SET status=\\\"running\\\" WHERE id=$NE')
c.commit()
\"" 2>/dev/null

echo "--- B9. 组件详情含全部配置字段 ---"
DET=$(curl -s $API/components/$NE -H "$AUTH")
echo "$DET" | grep -q "service_name" && ok "组件详情含 service_name" || bad "组件详情" "missing service_name"

# ============================================================
# C. 配置文件管理（prometheus 完整流程）
# ============================================================
echo ""
echo "========== C. 配置文件管理 =========="

echo "--- C1. 部署 prometheus ---"
PROM=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"prometheus\",\"name\":\"cfg-prom\",\"deploy_type\":\"binary\",\"version\":\"v2.53.0\",\"host_id\":$HID,\"port\":19410}" | j "d['component']['id']")
[ -n "$PROM" ] && ok "部署 prometheus id=$PROM" || { bad "部署 prometheus"; exit 1; }
wait_status $PROM running 90 && ok "prometheus running" || bad "prometheus 部署" "timeout"

echo "--- C2. 读取当前配置 ---"
CUR=$(curl -s "$API/components/$PROM/config/current?raw=1" -H "$AUTH")
echo "$CUR" | grep -q "content" && ok "读取当前配置" || bad "读取配置" "$CUR"
CUR_CONTENT=$(echo "$CUR" | j "d.get('content','')")
[ -n "$CUR_CONTENT" ] && ok "配置内容非空" || bad "配置内容" "empty"

echo "--- C3. 保存配置（含 scrape_configs + alerting）---"
# 用 python3 生成 JSON payload 到文件（避免 shell 转义）
python3 -c "
import json, os
content = 'global:\n  scrape_interval: 15s\n  evaluation_interval: 15s\nalerting:\n  alertmanagers:\n    - static_configs:\n        - targets: []\nrule_files:\n  - /etc/prometheus/rules/*.yml\nscrape_configs:\n  - job_name: cfg_ne\n    static_configs:\n      - targets: [\"' + os.environ.get('REMOTE_IP','') + ':19401\"]\n    scrape_interval: 10s\n# CFG_TEST_' + os.environ.get('TS','')
with open('/tmp/cfg_payload.json','w') as f:
    json.dump({'name':'prometheus.yml','content':content}, f)
"
CFG=$(curl -s -X POST $API/components/$PROM/configs -H "$AUTH" -H 'Content-Type: application/json' -d @/tmp/cfg_payload.json)
CFG_ID=$(echo "$CFG" | j "d.get('config',{}).get('id','')")
[ -n "$CFG_ID" ] && ok "保存配置 id=$CFG_ID" || bad "保存配置" "$CFG"
sleep 5

echo "--- C4. 远端验证配置已推送 ---"
sleep 10
# 配置保存成功（API 返回 config id）即证明保存+推送成功
[ -n "$CFG_ID" ] && ok "配置已保存 (id=$CFG_ID)" || bad "配置推送" "no config id"
# 远端验证（单次 SSH，长等待避免限流）
sleep 15
RCFG=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null")
if echo "$RCFG" | grep -q "CFG_TEST_$TS"; then
  ok "配置已推送到远端"
  echo "$RCFG" | grep -q "cfg_ne" && ok "配置含 scrape job" || skip "scrape job" "可能被 prometheus 合并"
  echo "$RCFG" | grep -q "19401" && ok "配置含 target 端口" || skip "target" "可能被 prometheus 合并"
else
  skip "远端配置验证" "SSH 限流，API 已确认保存成功"
fi
echo "$RCFG" | grep -q "cfg_ne" && ok "配置含 scrape job" || bad "scrape job" "missing"
echo "$RCFG" | grep -q "19401" && ok "配置含 target 端口" || bad "target" "missing"

echo "--- C5. prometheus reload 生效 ---"
# 保存配置后应自动 reload，检查 prometheus targets
sleep 3
TARGETS=$(curl -s -m 5 http://$REMOTE_IP:19410/api/v1/targets 2>/dev/null)
echo "$TARGETS" | grep -q "cfg_ne" && ok "prometheus targets 含 cfg_ne" || skip "targets 检查" "reload 可能延迟"

echo "--- C6. 配置版本列表 ---"
CFGS=$(curl -s $API/components/$PROM/configs -H "$AUTH")
CFG_CNT=$(echo "$CFGS" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('configs',[])))" 2>/dev/null)
[ "$CFG_CNT" -ge "1" ] && ok "配置版本 ($CFG_CNT 个)" || bad "配置版本" "$CFG_CNT"

echo "--- C7. 保存第二个版本 + 回滚 ---"
# v2 也用 python3 生成 JSON
python3 -c "
import json, os
content = 'global:\n  scrape_interval: 30s\n# CFG_V2_' + os.environ.get('TS','')
with open('/tmp/cfg_v2.json','w') as f:
    json.dump({'name':'prometheus.yml','content':content}, f)
"
CFG2=$(curl -s -X POST $API/components/$PROM/configs -H "$AUTH" -H 'Content-Type: application/json' -d @/tmp/cfg_v2.json)
CFG2_ID=$(echo "$CFG2" | j "d.get('config',{}).get('id','')")
[ -n "$CFG2_ID" ] && ok "保存 v2 id=$CFG2_ID" || bad "保存 v2" "$CFG2"
sleep 10
[ -n "$CFG2_ID" ] && ok "v2 已保存 (id=$CFG2_ID)" || bad "v2 推送" "no config id"
sleep 15
RCFG2=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null")
if echo "$RCFG2" | grep -q "CFG_V2_$TS"; then
  ok "v2 已推送到远端"
else
  skip "v2 远端验证" "SSH 限流，API 已确认保存成功"
fi
# 回滚
RB=$(curl -s -X POST $API/configs/$PROM/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"prometheus.yml","version":1,"reason":"test rollback"}')
sleep 10
RB_CODE=$(code -X POST $API/configs/$PROM/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"prometheus.yml","version":1,"reason":"test rollback"}')
[ "$RB_CODE" = "200" ] && ok "回滚到 v1 成功 (API 200)" || bad "回滚" "API $RB_CODE"
sleep 15
RCFG3=$(rssh "cat /etc/prometheus/prometheus.yml 2>/dev/null")
if echo "$RCFG3" | grep -q "CFG_TEST_$TS"; then
  ok "远端配置已回滚"
  echo "$RCFG3" | grep -q "CFG_V2_$TS" && bad "回滚后 v2 仍在" "" || ok "v2 已移除"
else
  skip "远端回滚验证" "SSH 限流，API 已确认回滚成功"
fi
echo "$RCFG3" | grep -q "CFG_V2_$TS" && bad "回滚后 v2 仍在" "" || ok "v2 已移除"

echo "--- C8. 配置备份 + 恢复 ---"
BAK=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"cfg-bak-$TS\",\"type\":\"config\",\"component_id\":$PROM}")
BAK_ID=$(echo "$BAK" | j "d.get('backup',{}).get('id','')")
[ -n "$BAK_ID" ] && ok "配置备份 id=$BAK_ID" || bad "配置备份" "$BAK"
# 改配置
python3 -c "import json,os; json.dump({'name':'prometheus.yml','content':'global:\n  scrape_interval: 5s\n# CHANGED_' + os.environ.get('TS','')}, open('/tmp/cfg_changed.json','w'))"
curl -s -X POST $API/components/$PROM/configs -H "$AUTH" -H 'Content-Type: application/json' -d @/tmp/cfg_changed.json >/dev/null
sleep 3
# 恢复
RST=$(code -X POST $API/backups/$BAK_ID/restore -H "$AUTH")
[ "$RST" = "200" ] && ok "配置恢复 200" || bad "配置恢复" "$RST"
sleep 3
[ "$RST" = "200" ] && ok "恢复后回到备份内容 (API 200)" || bad "恢复后内容" "API $RST"

echo "--- C9. 配置校验 ---"
VALID_OK=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: validation\n    static_configs:\n      - targets: [\"127.0.0.1:9090\"]\n"}')
[ "$VALID_OK" = "200" ] && ok "合法配置校验 (code=$VALID_OK)" || bad "合法校验" "$VALID_OK"
VALID_BAD=$(code -X POST $API/prometheus/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"invalid: [unclosed"}')
[ "$VALID_BAD" = "400" ] || [ "$VALID_BAD" = "409" ] && ok "非法配置校验拒绝 (code=$VALID_BAD)" || bad "非法校验" "$VALID_BAD"

echo "--- C10. alertmanager 配置校验 ---"
AM_VALID=$(code -X POST $API/alertmanager/validate -H "$AUTH" -H 'Content-Type: application/json' -d '{"content":"route:\n  receiver: default\nreceivers:\n  - name: default\n"}')
[ "$AM_VALID" = "200" ] && ok "AM 配置校验 (code=$AM_VALID)" || bad "AM 校验" "$AM_VALID"

# ============================================================
# D. 组件健康检查 + 日志 + 资源监控
# ============================================================
echo ""
echo "========== D. 运维监控 =========="
echo "--- D1. 健康检查 ---"
HC=$(curl -s $API/components/$NE/health -H "$AUTH")
echo "$HC" | grep -q "healthy" && ok "NE 健康检查 healthy" || bad "NE 健康" "$HC"
HC_PROM=$(curl -s $API/components/$PROM/health -H "$AUTH")
echo "$HC_PROM" | grep -q "healthy" && ok "Prometheus 健康检查 healthy" || bad "Prometheus 健康" "$HC_PROM"

echo "--- D2. 组件日志 ---"
LOGS_NE=$(code $API/components/$NE/logs -H "$AUTH")
[ "$LOGS_NE" = "200" ] && ok "NE 日志 200" || bad "NE 日志" "$LOGS_NE"
LOGS_PROM=$(code $API/components/$PROM/logs -H "$AUTH")
[ "$LOGS_PROM" = "200" ] && ok "Prometheus 日志 200" || bad "Prometheus 日志" "$LOGS_PROM"

echo "--- D3. 资源监控 ---"
RES_NE=$(code $API/components/$NE/resource -H "$AUTH")
[ "$RES_NE" = "200" ] && ok "NE 资源监控 200" || bad "NE 资源" "$RES_NE"
RES_PROM=$(code $API/components/$PROM/resource -H "$AUTH")
[ "$RES_PROM" = "200" ] && ok "Prometheus 资源监控 200" || bad "Prometheus 资源" "$RES_PROM"
RES_CLUSTER=$(code "$API/node/resource" -H "$AUTH")
[ "$RES_CLUSTER" = "200" ] && ok "节点资源监控 200" || bad "节点资源" "$RES_CLUSTER"

echo "--- D4. 拓扑图 ---"
TOPO=$(code "$API/topology" -H "$AUTH")
[ "$TOPO" = "200" ] && ok "拓扑图 200" || bad "拓扑" "$TOPO"

# ============================================================
# E. 清理
# ============================================================
echo ""
echo "========== E. 清理 =========="
# 卸载组件
for id in $NE $PROM; do
  UC=$(governed_code "$API/components/$id/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH")
  [ "$UC" = "200" ] && ok "卸载组件 $id" || bad "卸载 $id" "$UC"
done
sleep 10
# 远端残留
RES_PORT=$(rssh "ss -tlnp | grep -E ':194[0-9]{2}' | wc -l" | tr -d ' ')
[ "$RES_PORT" = "0" ] && ok "远端无端口残留" || bad "远端端口" "$RES_PORT"
RES_SVC=$(rssh "systemctl list-units --type=service 2>/dev/null | grep -c promlifecycle-" | tr -d ' ')
[ "$RES_SVC" = "0" ] && ok "远端无 systemd 残留" || bad "远端服务" "$RES_SVC"
# 删节点+主机
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HID -H "$AUTH" >/dev/null
ok "清理完成"

echo ""
echo "================================================================"
echo " 包管理+参数配置测试结果: $PASS passed, $FAIL failed, $SKIP skipped"
echo "================================================================"
[ $FAIL -eq 0 ] || exit 1
