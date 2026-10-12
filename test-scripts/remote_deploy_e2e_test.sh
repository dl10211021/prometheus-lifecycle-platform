#!/bin/bash
# Remote binary deployment lifecycle E2E.
#
# Requires a host explicitly supplied with REMOTE_HOST and REMOTE_PASS.
# sshpass is used only to independently verify remote state.
#
# 覆盖：
#   A. blackbox_exporter 远程部署（v0.24.0）：SSH 上传 + systemd 单元 + metrics
#   B. 远程升级 v0.24.0 → v0.26.0：版本核对 + 旧二进制 .bak 不残留
#   C. 远程回滚 auto-select：回到 v0.24.0
#   D. alertmanager 远程部署（带配置）：配置推送 + 数据目录
#   E. 远程配置变更：新内容落远端磁盘 + 服务 reload 后健康
#   F. 远程配置备份 → 变更 → 恢复：远端回到基线内容
#   G. 卸载清理：systemd 单元与安装目录从远端移除
#
# 注意：远端 sshd MaxStartups 对高频新连接会 reset，因此每个阶段把多项核对
# 合并进一次 SSH 调用（rssh），并在连接失败时退避重试。
set -u
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_PASS="${REMOTE_PASS:-}"
DATA_DIR="${DATA_DIR:-/opt/promlifecycle/data}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-/opt/promlifecycle/admin_initial_password}"
ADMIN_PASSWORD="${PROMLIFECYCLE_ADMIN_PASSWORD:-${ADMIN_PASSWORD:-}}"
if [ -z "$ADMIN_PASSWORD" ] && [ -r "$ADMIN_PASSWORD_FILE" ]; then
  ADMIN_PASSWORD="$(cat "$ADMIN_PASSWORD_FILE")"
fi
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
TS=$(date +%s)
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j()    { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' -m 30 "$@"; }
# rssh: independent SSH evidence channel with backoff covering strict host rate limits.
rssh() {
  local out rc=1 delay
  for delay in 0 2 4 8 12 20 30 45; do
    [ "$delay" -eq 0 ] || sleep "$delay"
    out=$(timeout 25 sshpass -e ssh -o StrictHostKeyChecking=yes -o ConnectTimeout=8 "$REMOTE_USER@$REMOTE_HOST" "$1" 2>/dev/null)
    rc=$?
    [ $rc -eq 0 ] && break
  done
  printf '%s' "$out"
  return "$rc"
}

if [ -z "$REMOTE_HOST" ] || [ -z "$REMOTE_PASS" ]; then
  echo "SKIP: set REMOTE_HOST and REMOTE_PASS for remote tests"
  exit 0
fi
if [ -z "$ADMIN_PASSWORD" ]; then
  echo "SKIP: set ADMIN_PASSWORD or a readable ADMIN_PASSWORD_FILE"
  exit 0
fi
command -v sshpass >/dev/null || { echo "SKIP: sshpass is not installed"; exit 0; }
export SSHPASS="$REMOTE_PASS"
rssh 'echo ok' >/dev/null 2>&1 || { echo "SKIP: cannot reach $REMOTE_HOST over SSH"; exit 0; }

cleanup() {
  if [ -n "${AUTH:-}" ]; then
    [ -n "${BB_ID:-}" ] && governed_action "$API/components/$BB_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null 2>&1
    [ -n "${AM_ID:-}" ] && governed_action "$API/components/$AM_ID/uninstall" '{"mode":"keep_data","reason":"test cleanup"}' "$AUTH" >/dev/null 2>&1
    [ "${CREATED_HOST:-0}" = "1" ] && curl -s -X DELETE "$API/hosts/$HOST_ID" -H "$AUTH" >/dev/null 2>&1
    [ "$(code "$API/node" -H "$AUTH")" = "200" ] && ok "单节点保持可用" || bad "单节点状态" "$(code "$API/node" -H "$AUTH")"
  fi
}
trap cleanup EXIT

LOGIN_PAYLOAD=$(ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["ADMIN_PASSWORD"]}))')
TOKEN=$(curl -s -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "$LOGIN_PAYLOAD" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
[ -n "$TOKEN" ] || { echo "登录失败"; exit 1; }

echo "=== 0. 准备（纳管或复用主机 $REMOTE_HOST）==="
CREATED_HOST=0
HOST_ID=$(curl -s "$API/hosts" -H "$AUTH" | j "next((x['id'] for x in d['hosts'] if x['ip']=='$REMOTE_HOST'), '')")
if [ -z "$HOST_ID" ]; then
  HOST_PAYLOAD=$(TEST_HOST_NAME="remote-deploy-$TS" REMOTE_HOST="$REMOTE_HOST" REMOTE_USER="$REMOTE_USER" REMOTE_PASS="$REMOTE_PASS" python3 "$(dirname "$0")/remote-host-payload.py")
  HOST_ID=$(curl -s -X POST "$API/hosts" -H "$AUTH" -H 'Content-Type: application/json' \
    -d "$HOST_PAYLOAD" | j "d.get('id','')")
  CREATED_HOST=1
fi
[ -n "$HOST_ID" ] || { echo "主机 $REMOTE_HOST 纳管失败"; exit 1; }
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
echo "  host=$HOST_ID node=$NODE_ID created=$CREATED_HOST"

echo "=== A. blackbox_exporter 远程部署 v0.24.0 ==="
BB=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"type\":\"blackbox_exporter\",\"name\":\"rde-bb-$TS\",\"deploy_type\":\"binary\",\"version\":\"v0.24.0\",\"host_id\":$HOST_ID,\"port\":19115}")
BB_ID=$(echo "$BB" | j "d['component']['id']")
[ -n "$BB_ID" ] && ok "组件创建 id=$BB_ID" || { bad "组件创建" "$BB"; exit 1; }
STATUS=pending
for i in $(seq 1 40); do
  STATUS=$(curl -s $API/components/$BB_ID -H "$AUTH" | j "d['status']")
  { [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ]; } && break
  sleep 3
done
[ "$STATUS" = "running" ] && ok "远程部署 running" || { bad "远程部署" "$(curl -s $API/components/$BB_ID -H "$AUTH" | j "d.get('error','')")"; exit 1; }
BB_META=$(curl -s "$API/components/$BB_ID" -H "$AUTH")
BB_UNIT=$(echo "$BB_META" | j "d['service_name']")
BB_DIR=$(echo "$BB_META" | j "d['install_path']")
BB_BIN="$BB_DIR/blackbox_exporter"
printf -v BB_UNIT_Q '%q' "$BB_UNIT"
printf -v BB_BIN_Q '%q' "$BB_BIN"
# 一次 SSH 核对：单元状态 + 二进制存在；版本行取回本地解析（避免远端嵌套引号）
A_OUT=$(rssh "systemctl is-active $BB_UNIT_Q; systemctl is-enabled $BB_UNIT_Q; test -x $BB_BIN_Q && echo BIN=yes")
A_VER=$(rssh 'curl -s -m 5 http://127.0.0.1:19115/metrics' | grep ^blackbox_exporter_build_info | grep -o 'version="[0-9][^"]*"' | head -1)
[[ "$A_OUT" == *$'active\nenabled\nBIN=yes'* ]] && ok "远端服务 active、enabled 且二进制存在" || bad "远端服务或二进制" "$A_OUT"
[ "$A_VER" = 'version="0.24.0"' ] && ok "远端运行版本 0.24.0" || bad "远端版本" "$A_VER"

echo "=== B. 远程升级 v0.24.0 → v0.26.0 ==="
UP=$(governed_action "$API/components/$BB_ID/upgrade" '{"new_version":"v0.26.0","reason":"test lifecycle"}' "$AUTH")
sleep 10
STATUS=$(curl -s $API/components/$BB_ID -H "$AUTH" | j "d['status']")
[ "$STATUS" = "running" ] && ok "升级后 running" || bad "升级后状态" "$STATUS: $UP"
B_VER=$(rssh 'curl -s -m 5 http://127.0.0.1:19115/metrics' | grep ^blackbox_exporter_build_info | grep -o 'version="[0-9][^"]*"' | head -1)
[ "$B_VER" = 'version="0.26.0"' ] && ok "远端运行版本 0.26.0" || bad "升级后版本" "$B_VER"

echo "=== C. 远程回滚 auto-select（不带 target_version）==="
RB_CODE=$(governed_code "$API/components/$BB_ID/rollback" '{"reason":"remote auto-select rollback"}' "$AUTH")
RBV=$(curl -s $API/components/$BB_ID -H "$AUTH" | j "d.get('version','')")
[ "$RB_CODE" = "200" ] && [ "$RBV" = "v0.24.0" ] && ok "auto-select 目标 v0.24.0" || bad "auto-select 目标" "code=$RB_CODE version=$RBV"
sleep 10
C_VER=$(rssh 'curl -s -m 5 http://127.0.0.1:19115/metrics' | grep ^blackbox_exporter_build_info | grep -o 'version="[0-9][^"]*"' | head -1)
[ "$C_VER" = 'version="0.24.0"' ] && ok "回滚后远端运行 0.24.0" || bad "回滚后版本" "$C_VER"

echo "=== D. alertmanager 远程部署（带配置）==="
AM=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"type\":\"alertmanager\",\"name\":\"rde-am-$TS\",\"deploy_type\":\"binary\",\"version\":\"v0.27.0\",\"host_id\":$HOST_ID,\"port\":19093}")
AM_ID=$(echo "$AM" | j "d['component']['id']")
[ -n "$AM_ID" ] && ok "组件创建 id=$AM_ID" || { bad "AM 创建" "$AM"; }
STATUS=pending
for i in $(seq 1 40); do
  STATUS=$(curl -s $API/components/$AM_ID -H "$AUTH" | j "d['status']")
  { [ "$STATUS" = "running" ] || [ "$STATUS" = "error" ]; } && break
  sleep 3
done
[ "$STATUS" = "running" ] && ok "AM 远程部署 running" || bad "AM 部署" "$(curl -s $API/components/$AM_ID -H "$AUTH" | j "d.get('error','')")"
AM_META=$(curl -s "$API/components/$AM_ID" -H "$AUTH")
AM_UNIT=$(echo "$AM_META" | j "d['service_name']")
AM_DIR=$(echo "$AM_META" | j "d['install_path']")
AM_CFG=$(echo "$AM_META" | j "d['config_path']")
printf -v AM_UNIT_Q '%q' "$AM_UNIT"
printf -v AM_DIR_Q '%q' "$AM_DIR"
printf -v AM_CFG_Q '%q' "$AM_CFG"
D_OUT=$(rssh "curl -fsS -m 5 http://127.0.0.1:19093/-/healthy >/dev/null && echo HEALTH=200; test -s $AM_CFG_Q && echo CFG=yes")
echo "$D_OUT" | grep -q 'HEALTH=200' && ok "AM 远端健康" || bad "AM 健康" "$D_OUT"
echo "$D_OUT" | grep -q 'CFG=yes' && ok "远端配置文件已推送" || bad "远端配置" "$D_OUT"

echo "=== E. 远程配置变更（marker 内容级验证）==="
BASE_CFG=$(rssh "cat $AM_CFG_Q")
governed_request "$API/components/$AM_ID/configs" \
  "{\"name\":\"alertmanager.yml\",\"content\":$(python3 -c "import json,sys;print(json.dumps(sys.stdin.read()+'\n# RDE_MARKER_$TS'))" <<< "$BASE_CFG"),\"reason\":\"remote alertmanager baseline\"}" "$AUTH"
CONFIG_CODE=$GOVERNED_CODE
R2=$(curl -s "$API/components/$AM_ID/configs" -H "$AUTH" | j "d.get('configs',[{}])[0].get('version','')")
[ "$CONFIG_CODE" = "200" ] && [ -n "$R2" ] && ok "治理配置 v$R2 已执行" || bad "配置创建" "code=$CONFIG_CODE version=$R2"
sleep 5
E_OUT=$(rssh "echo MARK=\$(grep -c 'RDE_MARKER_$TS' $AM_CFG_Q); curl -fsS -m 5 http://127.0.0.1:19093/-/healthy >/dev/null && echo HEALTH=200")
echo "$E_OUT" | grep -q 'MARK=[1-9]' && ok "marker 已落远端磁盘" || bad "远端 marker" "$E_OUT"
echo "$E_OUT" | grep -q 'HEALTH=200' && ok "配置变更后 AM 健康" || bad "变更后健康" "$E_OUT"

echo "=== F. 远程配置备份 → 变更 → 恢复 ==="
BAK=$(curl -s -X POST $API/backups -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"rde-$TS-cfg\",\"type\":\"config\",\"component_id\":$AM_ID}")
BAK_ID=$(echo "$BAK" | j "d['backup']['id']")
[ -n "$BAK_ID" ] && ok "配置备份创建 id=$BAK_ID" || bad "配置备份" "$BAK"
governed_request "$API/components/$AM_ID/configs" \
  "{\"name\":\"alertmanager.yml\",\"content\":$(python3 -c "import json,sys;print(json.dumps(sys.stdin.read()+'\n# RDE_MARKER2_$TS'))" <<< "$BASE_CFG"),\"reason\":\"remote backup restore mutation\"}" "$AUTH"
SECOND_CONFIG_CODE=$GOVERNED_CODE
sleep 5
F1=$(rssh "grep -c 'RDE_MARKER2_$TS' $AM_CFG_Q")
[ "$SECOND_CONFIG_CODE" = "200" ] && [ "${F1:-0}" -ge 1 ] && ok "第二次治理变更已落盘" || bad "第二次变更" "code=$SECOND_CONFIG_CODE marker=$F1"
RST_CODE=$(governed_code "$API/backups/$BAK_ID/restore" '{}' "$AUTH")
[ "$RST_CODE" = "200" ] && ok "恢复接口完成" || bad "恢复接口" "$RST_CODE"
sleep 5
F2=$(rssh "echo M1=\$(grep -c 'RDE_MARKER_$TS' $AM_CFG_Q); echo M2=\$(grep -c 'RDE_MARKER2_$TS' $AM_CFG_Q)")
echo "$F2" | grep -q 'M1=[1-9]' && echo "$F2" | grep -q 'M2=0' && ok "远端回到备份基线（marker 1）" || bad "恢复后内容" "$F2"
curl -s -X DELETE $API/backups/$BAK_ID -H "$AUTH" >/dev/null

echo "=== G. 治理卸载清理（远端单元与目录移除）==="
# 卸载前静置，避开前面高频 SSH 触发的 MaxStartups 窗口。
sleep 20
REMOVE_BB=$(governed_code "$API/components/$BB_ID/uninstall" '{"mode":"keep_data","reason":"remote cleanup"}' "$AUTH")
REMOVE_AM=$(governed_code "$API/components/$AM_ID/uninstall" '{"mode":"keep_data","reason":"remote cleanup"}' "$AUTH")
[ "$REMOVE_BB" = "200" ] && [ "$REMOVE_AM" = "200" ] && ok "治理卸载计划已执行" || bad "治理卸载计划" "bb=$REMOVE_BB am=$REMOVE_AM"
sleep 5
G_OUT=$(rssh "echo U1=\$(systemctl is-active $BB_UNIT_Q 2>&1); echo U2=\$(systemctl is-active $AM_UNIT_Q 2>&1); echo E1=\$(systemctl is-enabled $BB_UNIT_Q 2>/dev/null || echo not-found); echo E2=\$(systemctl is-enabled $AM_UNIT_Q 2>/dev/null || echo not-found); test -e $BB_BIN_Q && echo D1=yes || echo D1=no; test -e $AM_DIR_Q && echo D2=yes || echo D2=no")
echo "$G_OUT" | grep -qE 'U1=(inactive|unknown)' && ok "blackbox 单元已停止" || bad "blackbox 单元" "$G_OUT"
echo "$G_OUT" | grep -qE 'U2=(inactive|unknown)' && ok "alertmanager 单元已停止" || bad "alertmanager 单元" "$G_OUT"
echo "$G_OUT" | grep -qE 'E1=(disabled|not-found|unknown)' && ok "blackbox 自启动已禁用" || bad "blackbox 自启动残留" "$G_OUT"
echo "$G_OUT" | grep -qE 'E2=(disabled|not-found|unknown)' && ok "alertmanager 自启动已禁用" || bad "alertmanager 自启动残留" "$G_OUT"
echo "$G_OUT" | grep -q 'D1=no' && ok "blackbox 二进制已从远端移除" || bad "blackbox 残留" "$G_OUT"
echo "$G_OUT" | grep -q 'D2=no' && ok "alertmanager 二进制已从远端移除" || bad "alertmanager 残留" "$G_OUT"
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
NODE_ID=""

echo ""
echo "Remote Deploy E2E: $PASS passed, $FAIL failed"
[ $FAIL -eq 0 ]
