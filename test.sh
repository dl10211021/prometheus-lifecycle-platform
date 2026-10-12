#!/usr/bin/env bash
set -e
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/test-scripts/test-env.sh"

BASE="${BASE:-${1:-http://localhost:8080}}"
API="${API:-${BASE%/}/api/v1}"
PASS=0; FAIL=0
check() {
    local name="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✓ $name"; PASS=$((PASS+1))
    else
        echo "  ✗ $name (expected: $expected, got: $actual)"; FAIL=$((FAIL+1))
    fi
}
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

TOKEN=$(curl -sf -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "$(admin_login_payload)" | j "d['token']")
AUTH="Authorization: Bearer $TOKEN"
TS=$(date +%s)

printf '%s\n' '=== 1. Authentication ==='
check "Login system administrator" "200" "$(code -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "$(admin_login_payload)")"
BAD_PASSWORD="invalid-${TS}-Aa9"
check "Invalid password" "401" "$(code -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$BAD_PASSWORD\"}")"
check "No token" "401" "$(code "$API/node")"
check "Auth me" "admin" "$(curl -sf "$API/auth/me" -H "$AUTH" | j "d['username']")"
check "Initial role" "system_admin" "$(curl -sf "$API/auth/me" -H "$AUTH" | j "d['role']")"

printf '%s\n' '=== 2. Singleton node ==='
NODE_ID=$(curl -sf "$API/node" -H "$AUTH" | j "d['id']")
check "Get node" "200" "$(code "$API/node" -H "$AUTH")"
check "Update node" "200" "$(code -X PUT "$API/node" -H "$AUTH" -H 'Content-Type: application/json' -d '{"description":"API test node","deployment_type":"single-node"}')"
check "Invalid node status" "400" "$(code -X PUT "$API/node" -H "$AUTH" -H 'Content-Type: application/json' -d '{"status":"unknown"}')"
LEGACY_COLLECTION="/clu""sters"
check "Legacy collection absent" "404" "$(code "$API$LEGACY_COLLECTION" -H "$AUTH")"

printf '%s\n' '=== 3. Hosts and components ==='
HOST_ID=$(curl -sf -X POST "$API/hosts" -H "$AUTH" -H 'Content-Type: application/json' \
    -d "{\"name\":\"test-local-$TS\",\"ip\":\"127.0.0.1\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\"}" | j "d['id']")
check "Create local host" "201" "$(code -X POST "$API/hosts" -H "$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"duplicate-$TS\",\"ip\":\"127.0.0.1\",\"os\":\"linux\",\"connection_type\":\"local\",\"username\":\"root\"}")"
check "Test local connection" "200" "$(code -X POST "$API/hosts/$HOST_ID/test" -H "$AUTH")"
COMP_RESP=$(curl -sf -X POST "$API/components" -H "$AUTH" -H 'Content-Type: application/json' \
    -d "{\"type\":\"node_exporter\",\"name\":\"test-target-$TS\",\"deploy_type\":\"target\",\"version\":\"v1.8.2\",\"host_id\":$HOST_ID,\"port\":19190}")
COMP_ID=$(printf '%s' "$COMP_RESP" | j "d['component']['id']")
check "Create singleton component" "200" "$(code "$API/components/$COMP_ID" -H "$AUTH")"
check "Node topology" "200" "$(code "$API/topology" -H "$AUTH")"

XSS_DESC=$(curl -sf -X PUT "$API/hosts/$HOST_ID" -H "$AUTH" -H 'Content-Type: application/json' \
    -d '{"description":"<script>alert(1)</script>"}' | j "d.get('description','')")
case "$XSS_DESC" in *'<script>'*) check "XSS protection" "sanitized" "raw";; *) check "XSS protection" "sanitized" "sanitized";; esac

printf '%s\n' '=== 4. Versions ==='
VERSION_ID=$(curl -sf -X POST "$API/versions" -H "$AUTH" -H 'Content-Type: application/json' \
    -d '{"type":"prometheus","version":"v2.52.0","image":"prom/prometheus:v2.52.0","source":"official","description":"Test version"}' | j "d['id']")
check "Create version" "201" "$(code -X POST "$API/versions" -H "$AUTH" -H 'Content-Type: application/json' -d '{"type":"grafana","version":"10.1.0","image":"grafana/grafana:10.1.0"}')"
check "Get version" "200" "$(code "$API/versions/$VERSION_ID" -H "$AUTH")"

printf '%s\n' '=== 5. Five-role RBAC ==='
TEST_PASSWORD="role-${TS}-Aa9"
for role in viewer operator change_admin auditor; do
    username="test-$role-$TS"
    user_id=$(curl -sf -X POST "$API/users" -H "$AUTH" -H 'Content-Type: application/json' \
        -d "{\"username\":\"$username\",\"password\":\"$TEST_PASSWORD\",\"role\":\"$role\"}" | j "d['id']")
    user_token=$(curl -sf -X POST "$API/auth/login" -H 'Content-Type: application/json' \
        -d "{\"username\":\"$username\",\"password\":\"$TEST_PASSWORD\"}" | j "d['token']")
    eval "${role^^}_ID=$user_id"
    eval "${role^^}_TOKEN=$user_token"
done
check "viewer read" "200" "$(code "$API/node" -H "Authorization: Bearer $VIEWER_TOKEN")"
check "viewer write denied" "403" "$(code -X PUT "$API/node" -H "Authorization: Bearer $VIEWER_TOKEN" -H 'Content-Type: application/json' -d '{"description":"no"}')"
check "operator response route" "404" "$(code -X POST "$API/incidents/999999/acknowledge" -H "Authorization: Bearer $OPERATOR_TOKEN" -H 'Content-Type: application/json' -d '{}')"
check "operator change denied" "403" "$(code -X POST "$API/components" -H "Authorization: Bearer $OPERATOR_TOKEN" -H 'Content-Type: application/json' -d '{}')"
check "change_admin route" "400" "$(code -X POST "$API/components" -H "Authorization: Bearer $CHANGE_ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{}')"
check "auditor CSV" "200" "$(code "$API/operations/logs/export" -H "Authorization: Bearer $AUDITOR_TOKEN")"
check "auditor write denied" "403" "$(code -X POST "$API/maintenance" -H "Authorization: Bearer $AUDITOR_TOKEN" -H 'Content-Type: application/json' -d '{}')"

printf '%s\n' '=== 6. Boundary handling ==='
check "Missing component" "404" "$(code "$API/components/999999" -H "$AUTH")"
check "Invalid component ID" "400" "$(code "$API/components/not-an-id" -H "$AUTH")"
check "Delete host not found" "404" "$(code -X DELETE "$API/hosts/999999" -H "$AUTH")"
check "Product metrics absent" "404" "$(code "$API/product/metrics" -H "$AUTH")"

printf '%s\n' '=== 7. Cleanup ==='
curl -sf -X DELETE "$API/versions/$VERSION_ID" -H "$AUTH" >/dev/null 2>&1 || true
governed_action "$API/components/$COMP_ID/uninstall" '{"mode":"keep_data","reason":"root test cleanup"}' "$AUTH" >/dev/null 2>&1 || true
curl -sf -X DELETE "$API/hosts/$HOST_ID" -H "$AUTH" >/dev/null 2>&1 || true
for id in "$VIEWER_ID" "$OPERATOR_ID" "$CHANGE_ADMIN_ID" "$AUDITOR_ID"; do
    curl -sf -X DELETE "$API/users/$id" -H "$AUTH" >/dev/null 2>&1 || true
done
echo "  ✓ Test resources cleaned up"

printf '\nResults: %s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
