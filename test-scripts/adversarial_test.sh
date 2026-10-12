#!/bin/bash
# Adversarial / security tests for Prometheus Lifecycle Platform (local deploy)
set -u
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-env.sh"
BASE="${BASE:-http://localhost:8080}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✓ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✗ $1 (got: $2)"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

TS=$(date +%s)
echo "=== Adversarial tests (TS=$TS) ==="

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"

echo "--- A. AuthN/AuthZ ---"
[ "$(code $API/auth/me)" = "401" ] && ok "无 token → 401" || bad "无 token" "$(code $API/auth/me)"
[ "$(code $API/auth/me -H 'Authorization: Bearer garbage')" = "401" ] && ok "伪 token → 401" || bad "伪 token" "$(code $API/auth/me -H 'Authorization: Bearer garbage')"
TAMPERED=$(python3 - "$TOKEN" <<'PY'
import sys
parts = sys.argv[1].split(".")
signature = list(parts[2])
signature[0] = "A" if signature[0] != "A" else "B"
parts[2] = "".join(signature)
print(".".join(parts))
PY
)
[ "$(code $API/auth/me -H "Authorization: Bearer $TAMPERED")" = "401" ] && ok "篡改 token → 401" || bad "篡改 token" "$(code $API/auth/me -H "Authorization: Bearer $TAMPERED")"

SECRET=$(cat "$JWT_SECRET_FILE" 2>/dev/null)
EXPIRED=$(python3 - "$SECRET" <<'PY'
import sys, base64, hmac, hashlib, json, time
def b64(b): return base64.urlsafe_b64encode(b).rstrip(b'=').decode()
secret = sys.argv[1].encode()
header = b64(json.dumps({"alg":"HS256","typ":"JWT"}).encode())
now = int(time.time())
payload = b64(json.dumps({"user_id":1,"username":"admin","role":"system_admin","exp":now-3600}).encode())
msg = f"{header}.{payload}".encode()
sig = b64(hmac.new(secret, msg, hashlib.sha256).digest())
print(f"{header}.{payload}.{sig}")
PY
)
[ "$(code $API/auth/me -H "Authorization: Bearer $EXPIRED")" = "401" ] && ok "过期 token → 401" || bad "过期 token" "$(code $API/auth/me -H "Authorization: Bearer $EXPIRED")"
WRONGKEY=$(python3 - <<'PY'
import base64, hmac, hashlib, json, time
def b64(b): return base64.urlsafe_b64encode(b).rstrip(b'=').decode()
header = b64(json.dumps({"alg":"HS256","typ":"JWT"}).encode())
payload = b64(json.dumps({"user_id":1,"username":"admin","role":"system_admin","exp":int(time.time())+3600}).encode())
msg = f"{header}.{payload}".encode()
sig = b64(hmac.new(b"wrong-secret", msg, hashlib.sha256).digest())
print(f"{header}.{payload}.{sig}")
PY
)
[ "$(code $API/auth/me -H "Authorization: Bearer $WRONGKEY")" = "401" ] && ok "错误密钥签名 → 401" || bad "错误密钥签名" "$(code $API/auth/me -H "Authorization: Bearer $WRONGKEY")"

VIEW_PASS="viewer-${TS}-Aa9"
CHANGE_PASS="change-${TS}-Aa9"
OPERATOR_PASS="operator-${TS}-Aa9"
AUDITOR_PASS="auditor-${TS}-Aa9"
DISABLED_PASS="disabled-${TS}-Aa9"
BAD_PASSWORD="invalid-${TS}-Aa9"
VNAME="adv-viewer-$TS"
VID=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$VNAME\",\"password\":\"$VIEW_PASS\",\"role\":\"viewer\"}" | j "d['id']")
VTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$VNAME\",\"password\":\"$VIEW_PASS\"}" | j "d['token']")
VAUTH="Authorization: Bearer $VTOKEN"
CNAME="adv-change-$TS"
UID2=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$CNAME\",\"password\":\"$CHANGE_PASS\",\"role\":\"change_admin\"}" | j "d['id']")
CTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$CNAME\",\"password\":\"$CHANGE_PASS\"}" | j "d['token']")
CAUTH="Authorization: Bearer $CTOKEN"
ONAME="adv-operator-$TS"
OID=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$ONAME\",\"password\":\"$OPERATOR_PASS\",\"role\":\"operator\"}" | j "d['id']")
OTOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$ONAME\",\"password\":\"$OPERATOR_PASS\"}" | j "d['token']")
OAUTH="Authorization: Bearer $OTOKEN"
ANAME="adv-auditor-$TS"
AID=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$ANAME\",\"password\":\"$AUDITOR_PASS\",\"role\":\"auditor\"}" | j "d['id']")
ATOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$ANAME\",\"password\":\"$AUDITOR_PASS\"}" | j "d['token']")
AAUTH="Authorization: Bearer $ATOKEN"
[ "$(code -X PUT $API/node -H "$VAUTH" -H 'Content-Type: application/json' -d '{"description":"forbidden"}')" = "403" ] && ok "viewer 写节点 → 403" || bad "viewer 写节点" "$(code -X PUT $API/node -H "$VAUTH" -H 'Content-Type: application/json' -d '{"description":"forbidden"}')"
[ "$(code $API/users -H "$VAUTH")" = "403" ] && ok "viewer 访问用户管理 → 403" || bad "viewer 用户管理" "$(code $API/users -H "$VAUTH")"
[ "$(code $API/users -H "$CAUTH")" = "403" ] && ok "change_admin 访问用户管理 → 403" || bad "change_admin 用户管理" "$(code $API/users -H "$CAUTH")"
[ "$(code $API/node -H "$VAUTH")" = "200" ] && ok "viewer 读节点 → 200" || bad "viewer 读节点" "$(code $API/node -H "$VAUTH")"
[ "$(code -X POST $API/components -H "$CAUTH" -H 'Content-Type: application/json' -d '{}')" = "400" ] && ok "change_admin 可达变更接口" || bad "change_admin 变更接口" "$(code -X POST $API/components -H "$CAUTH" -H 'Content-Type: application/json' -d '{}')"
[ "$(code -X POST $API/maintenance -H "$OAUTH" -H 'Content-Type: application/json' -d '{}')" = "400" ] && ok "operator 可达维护接口" || bad "operator 维护接口" "$(code -X POST $API/maintenance -H "$OAUTH" -H 'Content-Type: application/json' -d '{}')"
[ "$(code -X POST $API/components -H "$OAUTH" -H 'Content-Type: application/json' -d '{}')" = "403" ] && ok "operator 不可执行变更" || bad "operator 变更隔离" "$(code -X POST $API/components -H "$OAUTH" -H 'Content-Type: application/json' -d '{}')"
[ "$(code $API/operations/logs/export -H "$AAUTH")" = "200" ] && ok "auditor 可导出审计 CSV" || bad "auditor 审计导出" "$(code $API/operations/logs/export -H "$AAUTH")"
[ "$(code -X POST $API/maintenance -H "$AAUTH" -H 'Content-Type: application/json' -d '{}')" = "403" ] && ok "auditor 严格只读" || bad "auditor 维护隔离" "$(code -X POST $API/maintenance -H "$AAUTH" -H 'Content-Type: application/json' -d '{}')"
[ "$(code $API/stats -H "$VAUTH")" = "200" ] && ok "viewer 读 stats → 200" || bad "viewer stats" "$(code $API/stats -H "$VAUTH")"
[ "$(code -X POST $API/users -H "$CAUTH" -H 'Content-Type: application/json' -d "{\"username\":\"nope\",\"password\":\"$BAD_PASSWORD\"}")" = "403" ] && ok "change_admin 创建用户 → 403" || bad "change_admin 创建用户" "$(code -X POST $API/users -H "$CAUTH" -H 'Content-Type: application/json' -d "{\"username\":\"nope\",\"password\":\"$BAD_PASSWORD\"}")"

echo "--- B. Login hardening ---"
[ "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"' OR 1=1 --\",\"password\":\"$BAD_PASSWORD\"}")" = "401" ] && ok "SQLi 用户名 → 401" || bad "SQLi 用户名" "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"' OR 1=1 --\",\"password\":\"$BAD_PASSWORD\"}")"
[ "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"nobody\",\"password\":\"$BAD_PASSWORD\"}")" = "401" ] && ok "不存在用户 → 401" || bad "不存在用户" "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"nobody\",\"password\":\"$BAD_PASSWORD\"}")"
[ "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$BAD_PASSWORD\"}")" = "401" ] && ok "错误密码 → 401" || bad "错误密码" "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$BAD_PASSWORD\"}")"
[ "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d '{"username":"admin"}')" = "400" ] && ok "缺密码字段 → 400" || bad "缺密码字段" "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d '{"username":"admin"}')"
[ "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d 'not json')" = "400" ] && ok "非法 JSON → 400" || bad "非法 JSON" "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d 'not json')"
# disabled user login
DIS_NAME="adv-disabled-$TS"
DIS_ID=$(curl -s -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$DIS_NAME\",\"password\":\"$DISABLED_PASS\",\"role\":\"change_admin\"}" | j "d['id']")
curl -s -X PUT $API/users/$DIS_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"is_active":false}' >/dev/null
[ "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$DIS_NAME\",\"password\":\"$DISABLED_PASS\"}")" = "401" ] && ok "禁用用户登录 → 401" || bad "禁用用户登录" "$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$DIS_NAME\",\"password\":\"$DISABLED_PASS\"}")"
curl -s -X DELETE $API/users/$DIS_ID -H "$AUTH" >/dev/null

echo "--- C. Input validation / traversal ---"
[ "$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"status":"../../etc"}')" = "400" ] && ok "节点状态非法值 → 400" || bad "节点状态校验" "$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"status":"../../etc"}')"
NODE_ID=$(curl -s $API/node -H "$AUTH" | j "d['id']")
HOST_ID=$(curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"adv-host-$TS\",\"ip\":\"127.0.0.1\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" | j "d['id']")
COMP_ID=$(curl -s -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"adv-node-$TS\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19300}" | j "d['component']['id']")
[ "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"../evil\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19301}")" = "400" ] && ok "组件名穿越 → 400" || bad "组件名穿越" "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d "{\"type\":\"node_exporter\",\"name\":\"../evil\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19301}")"
[ "$(code -X POST $API/components/$COMP_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"../x","content":"a: 1","content_type":"yaml"}')" = "400" ] && ok "配置名穿越 → 400" || bad "配置名穿越" "$(code -X POST $API/components/$COMP_ID/configs -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"../x","content":"a: 1","content_type":"yaml"}')"
[ "$(code -X PUT $API/gitops/config -H "$AUTH" -H 'Content-Type: application/json' -d '{}')" = "404" ] && ok "非生命周期 GitOps 路由已移除" || bad "GitOps 路由残留" "$(code -X PUT $API/gitops/config -H "$AUTH" -H 'Content-Type: application/json' -d '{}')"
[ "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"bogus","name":"x","deploy_type":"target","host_id":1}')" = "400" ] && ok "非法组件类型 → 400" || bad "非法组件类型" "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"bogus","name":"x","deploy_type":"target","host_id":1}')"
[ "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"node_exporter","name":"x","deploy_type":"bogus","host_id":1}')" = "400" ] && ok "非法部署类型 → 400" || bad "非法部署类型" "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"node_exporter","name":"x","deploy_type":"bogus","host_id":1}')"
[ "$(code $API/components/abc -H "$AUTH")" = "400" ] && ok "非法 ID(abc) → 400" || bad "非法 ID(abc)" "$(code $API/components/abc -H "$AUTH")"
[ "$(code $API/components/999999 -H "$AUTH")" = "404" ] && ok "不存在 ID → 404" || bad "不存在 ID" "$(code $API/components/999999 -H "$AUTH")"
[ "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{}')" = "400" ] && ok "组件缺必填字段 → 400" || bad "组件缺必填字段" "$(code -X POST $API/components -H "$AUTH" -H 'Content-Type: application/json' -d '{}')"
[ "$(code -X POST $API/dashboards -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"adv-dash\",\"content\":\"not json\"}")" = "400" ] && ok "仪表盘非法 JSON → 400" || bad "仪表盘非法 JSON" "$(code -X POST $API/dashboards -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"adv-dash\",\"content\":\"not json\"}")"
[ "$(code "$API/hosts/$HOST_ID/browse?path=%2Fnonexistent-xyz" -H "$AUTH")" = "400" ] && ok "浏览不存在目录 → 400" || bad "浏览不存在目录" "$(code "$API/hosts/$HOST_ID/browse?path=%2Fnonexistent-xyz" -H "$AUTH")"
[ "$(code "$API/hosts/$HOST_ID/browse?path=%2Ftmp%3Brm" -H "$AUTH")" = "400" ] && ok "浏览含编码分号 → 400" || bad "浏览编码分号" "$(code "$API/hosts/$HOST_ID/browse?path=%2Ftmp%3Brm" -H "$AUTH")"
[ "$(code "$API/versions/by-type?type=%27%20OR%20%271%27%3D%271" -H "$AUTH")" = "200" ] && ok "查询参数注入不报错 → 200" || bad "查询参数注入" "$(code "$API/versions/by-type?type=%27%20OR%20%271%27%3D%271" -H "$AUTH")"
[ "$(code "$API/operations?user_id=1%20OR%201%3D1" -H "$AUTH")" = "200" ] && ok "ops 参数注入不报错 → 200" || bad "ops 参数注入" "$(code "$API/operations?user_id=1%20OR%201%3D1" -H "$AUTH")"

echo "--- D. Arbitrary field updates (whitelist) ---"
RW_ID=$(curl -s -X POST $API/remote-writes -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"adv-rw\",\"url\":\"http://t:1/api/v1/write\"}" | j "d['id']")
curl -s -X PUT $API/remote-writes/$RW_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"adv-rw-new","id":999,"cluster_id":999,"created_at":"2020-01-01T00:00:00Z"}' >/dev/null
RW=$(curl -s $API/remote-writes/$RW_ID -H "$AUTH")
RWOK=$([ "$(echo "$RW" | j "d['id']")" = "$RW_ID" ] && [ "$(echo "$RW" | j "d['cluster_id']")" = "$NODE_ID" ] && echo y)
[ "$RWOK" = "y" ] && ok "remote-write 白名单(id/cluster_id 未变)" || bad "remote-write 白名单" "$(echo "$RW" | j "d['id'],d['cluster_id']")"
RULE_ID=$(curl -s -X POST $API/alerts/rules -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"AdvCPU\",\"expression\":\"cpu>90\"}" | j "d['id']")
RULE_UPD=$(curl -s -X PUT $API/alerts/rules/$RULE_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"cluster_id":999,"name":"AdvCPU2"}')
[ "$(echo "$RULE_UPD" | j "d['cluster_id']")" = "$NODE_ID" ] && ok "alert-rule 白名单(cluster_id 未变)" || bad "alert-rule 白名单" "$(echo "$RULE_UPD" | j "d['cluster_id']")"
SC_ID=$(curl -s -X POST $API/scrape-configs -H "$AUTH" -H 'Content-Type: application/json' -d "{\"cluster_id\":$NODE_ID,\"name\":\"adv-sc\",\"job_name\":\"adv_job\",\"targets\":\"[]\"}" | j "d['id']")
curl -s -X PUT $API/scrape-configs/$SC_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"cluster_id":999}' >/dev/null
[ "$(curl -s $API/scrape-configs/$SC_ID -H "$AUTH" | j "d['cluster_id']")" = "$NODE_ID" ] && ok "scrape-config 白名单(cluster_id 未变)" || bad "scrape-config 白名单" "$(curl -s $API/scrape-configs/$SC_ID -H "$AUTH" | j "d['cluster_id']")"
DASH_ID=$(curl -s -X POST $API/dashboards -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"adv-dash2\",\"content\":\"{}\"}" | j "d['id']")
curl -s -X PUT $API/dashboards/$DASH_ID -H "$AUTH" -H 'Content-Type: application/json' -d '{"cluster_id":999,"name":"adv-dash2b"}' >/dev/null
[ "$(curl -s $API/dashboards/$DASH_ID -H "$AUTH" | j "d['cluster_id']")" = "$NODE_ID" ] && ok "dashboard 白名单(cluster_id 未变)" || bad "dashboard 白名单" "$(curl -s $API/dashboards/$DASH_ID -H "$AUTH" | j "d['cluster_id']")"
SB_CODE=$(code -X POST $API/backups/scheduled -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"adv-sb","cron":"0 0 * * *"}')
[ "$SB_CODE" = "400" ] && ok "定时备份拒绝缺失组件" || bad "定时备份目标校验" "$SB_CODE"

echo "--- E. Secret exposure ---"
H=$(curl -s $API/hosts -H "$AUTH")
[ "$(echo "$H" | grep -c '"private_key"\|"password"')" = "0" ] && ok "hosts 不泄露密钥" || bad "hosts 泄露密钥" "$(echo "$H" | grep -o '"private_key"\|"password"' | head -2)"
[ "$(curl -s $API/notifications/channels -H "$AUTH" | grep -c '"secret"')" = "0" ] && ok "通知渠道不泄露 secret" || bad "通知渠道泄露" "$(curl -s $API/notifications/channels -H "$AUTH" | grep -c '"secret"')"
[ "$(curl -s $API/remote-writes/$RW_ID -H "$AUTH" | grep -c 'basic_auth_password\|bearer_token')" = "0" ] && ok "remote-write 不泄露凭据" || bad "remote-write 泄露" "$(curl -s $API/remote-writes/$RW_ID -H "$AUTH" | grep -o 'basic_auth_password\|bearer_token' | head -2)"
[ "$(curl -s $API/users -H "$AUTH" | grep -c '"password"')" = "0" ] && ok "users 不泄露密码" || bad "users 泄露密码" "$(curl -s $API/users -H "$AUTH" | grep -c '"password"')"

echo "--- F. XSS sanitization ---"
curl -s -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"<script>alert(1)</script>ok"}' >/dev/null
XC=$(curl -s $API/node -H "$AUTH")
[ "$(echo "$XC" | grep -c '<script>')" = "0" ] && ok "节点描述 XSS 已清洗" || bad "节点描述 XSS" "$XC"

echo "--- G. Oversize / duplicates ---"
dd if=/dev/zero of=/tmp/big-tpl.yaml bs=1M count=11 2>/dev/null
[ "$(code -X POST $API/templates/upload -H "$AUTH" -F "name=big" -F "component_type=prometheus" -F "file=@/tmp/big-tpl.yaml")" = "400" ] && ok "模板上传 >10MB → 400" || bad "模板上传超大" "$(code -X POST $API/templates/upload -H "$AUTH" -F "name=big" -F "component_type=prometheus" -F "file=@/tmp/big-tpl.yaml")"
[ "$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"deployment_type":"invalid"}')" = "400" ] && ok "非法节点部署模式 → 400" || bad "节点部署模式校验" "$(code -X PUT $API/node -H "$AUTH" -H 'Content-Type: application/json' -d '{"deployment_type":"invalid"}')"
[ "$(code -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$CNAME\",\"password\":\"$BAD_PASSWORD\"}")" = "409" ] && ok "重名用户 → 409" || bad "重名用户" "$(code -X POST $API/users -H "$AUTH" -H 'Content-Type: application/json' -d "{\"username\":\"$CNAME\",\"password\":\"$BAD_PASSWORD\"}")"
HCNT_BEFORE=$(curl -s $API/hosts -H "$AUTH" | j "len(d['hosts'])")
curl -s -X POST $API/hosts -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"dup-ip-$TS\",\"ip\":\"127.0.0.1\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\",\"port\":22}" >/dev/null
HCNT_AFTER=$(curl -s $API/hosts -H "$AUTH" | j "len(d['hosts'])")
[ "$HCNT_AFTER" = "$HCNT_BEFORE" ] && ok "重复 IP 主机不新增记录" || bad "重复 IP 主机" "before=$HCNT_BEFORE after=$HCNT_AFTER"

echo "--- H. Rollback adversarial ---"
[ "$(code -X POST $API/configs/999999/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"x","version":1,"reason":"test rollback"}')" = "404" ] && ok "回滚不存在组件 → 404" || bad "回滚不存在组件" "$(code -X POST $API/configs/999999/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"x","version":1,"reason":"test rollback"}')"
[ "$(code -X POST $API/configs/$COMP_ID/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"nope","version":1,"reason":"test rollback"}')" = "404" ] && ok "回滚不存在配置 → 404" || bad "回滚不存在配置" "$(code -X POST $API/configs/$COMP_ID/rollback -H "$AUTH" -H 'Content-Type: application/json' -d '{"config_name":"nope","version":1,"reason":"test rollback"}')"

echo "--- I. Cleanup ---"
curl -s -X DELETE $API/remote-writes/$RW_ID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/alerts/rules/$RULE_ID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/scrape-configs/$SC_ID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/dashboards/$DASH_ID -H "$AUTH" >/dev/null
governed_code "$API/components/$COMP_ID/uninstall" '{"mode":"keep_service","reason":"security test cleanup"}' "$AUTH" >/dev/null
curl -s $API/node -H "$AUTH" >/dev/null 2>&1
curl -s -X DELETE $API/hosts/$HOST_ID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/users/$VID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/users/$UID2 -H "$AUTH" >/dev/null
curl -s -X DELETE $API/users/$OID -H "$AUTH" >/dev/null
curl -s -X DELETE $API/users/$AID -H "$AUTH" >/dev/null

echo "--- J. Login rate limiting (LAST - locks IP for 60s) ---"
RL=0
for i in $(seq 1 25); do
  RL=$(code -X POST $API/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$BAD_PASSWORD\"}")
  [ "$RL" = "429" ] && break
done
[ "$RL" = "429" ] && ok "连续失败登录触发 429（第 ${i} 次）" || bad "登录限流" "$RL"

echo ""
echo "=========================================="
echo "Adversarial Result: $PASS passed, $FAIL failed"
echo "=========================================="
[ $FAIL -eq 0 ]
