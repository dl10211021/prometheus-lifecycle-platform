#!/usr/bin/env bash
# Shared test authentication input. This file is sourced by test scripts.
DATA_DIR="${DATA_DIR:-}"
ADMIN_PASSWORD="${PROMLIFECYCLE_ADMIN_PASSWORD:-${ADMIN_PASSWORD:-}}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-/opt/promlifecycle/admin_initial_password}"
JWT_SECRET_FILE="${PROMLIFECYCLE_JWT_SECRET_FILE:-/opt/promlifecycle/jwt_secret}"
if [ -z "$ADMIN_PASSWORD" ] && [ -r "$ADMIN_PASSWORD_FILE" ]; then
  ADMIN_PASSWORD="$(cat "$ADMIN_PASSWORD_FILE")"
fi
if [ -z "$ADMIN_PASSWORD" ]; then
  echo "SKIP: set PROMLIFECYCLE_ADMIN_PASSWORD or a readable ADMIN_PASSWORD_FILE"
  exit 0
fi

admin_login_payload() {
  ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["ADMIN_PASSWORD"]}))'
}

require_packages() {
  local spec component version response count missing=""
  for spec in "$@"; do
    component="${spec%%:*}"
    version="${spec#*:}"
    if [ -z "$component" ] || [ -z "$version" ] || [ "$component" = "$version" ]; then
      echo "Invalid package requirement: $spec" >&2
      exit 2
    fi
    response=$(curl -s "$API/packages?type=$component" -H "$AUTH")
    count=$(PACKAGE_RESPONSE="$response" PACKAGE_VERSION="$version" python3 -c 'import json,os
data=json.loads(os.environ["PACKAGE_RESPONSE"])
want=os.environ["PACKAGE_VERSION"].lstrip("v")
items=data.get("packages", data if isinstance(data,list) else [])
print(sum(1 for p in items if str(p.get("version","")).lstrip("v")==want and p.get("format") in ("binary","zip")))' 2>/dev/null)
    if [ "${count:-0}" -eq 0 ]; then
      missing="$missing $component:$version"
    fi
  done
  if [ -n "$missing" ]; then
    echo "SKIP: missing required offline packages:$missing"
    exit 0
  fi
}

# Run a lifecycle mutation. The endpoint performs preflight, recovery-point
# creation, execution and verification directly; no approval workflow exists.
governed_request() {
  local url="$1" body="$2" auth="${3:-$AUTH}" raw
  raw=$(curl -s -w $'\n%{http_code}' -X POST "$url" -H "$auth" -H 'Content-Type: application/json' -d "$body")
  GOVERNED_CODE=${raw##*$'\n'}
  GOVERNED_BODY=${raw%$'\n'*}
}

governed_action() {
  governed_request "$@"
  printf '%s' "$GOVERNED_BODY"
}

governed_code() {
  governed_request "$@"
  printf '%s' "$GOVERNED_CODE"
}

# Component creation is bound to a short-lived preflight credential. Keep all
# product-closure scripts on the public API contract without duplicating token
# plumbing in every scenario.
curl() {
  local args=("$@") endpoint_index=-1 output_mode=0 index token preflight
  for index in "${!args[@]}"; do
    if [ "${args[$index]}" = "-o" ] || [[ "${args[$index]}" == --output* ]]; then
      output_mode=1
    fi
    if [ -n "${API:-}" ] && [ "${args[$index]}" = "${API%/}/components" ]; then
      endpoint_index=$index
    fi
  done
  if [ "$endpoint_index" -lt 0 ] || [ "$output_mode" -eq 1 ]; then
    command curl "${args[@]}"
    return
  fi
  local preflight_args=("${args[@]}")
  preflight_args[$endpoint_index]="${API%/}/components/preflight"
  preflight=$(command curl "${preflight_args[@]}")
  token=$(printf '%s' "$preflight" | python3 -c 'import json,sys
try:
 print(json.load(sys.stdin).get("preflight_token",""))
except Exception:
 print("")' 2>/dev/null)
  if [ -z "$token" ]; then
    printf '%s' "$preflight"
    return
  fi
  command curl "${args[@]}" -H "X-PromLifecycle-Preflight-Token: $token"
}
