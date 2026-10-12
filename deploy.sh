#!/usr/bin/env bash
# Prometheus Lifecycle Platform single-node installer/upgrader.
set -euo pipefail
umask 077
PORT="${PORT:-8080}"
LISTEN_ADDRESS="${LISTEN_ADDRESS:-0.0.0.0}"
BASE="${BASE_DIR:-/opt/promlifecycle}"
DATA_DIR="${DATA_DIR:-}"
JWT_SECRET_FILE="${JWT_SECRET_FILE:-}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-}"
PREBUILT_BINARY=""
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
usage() {
  printf '%s\n' 'Usage: sudo bash deploy.sh [--binary FILE] [--port PORT] [--listen-address IP|localhost] [--base DIR] [--data DIR] [--jwt-secret-file FILE] [--admin-password-file FILE]' 'Without --binary, build from the vendored source using Go and a C compiler.' 'Upgrades preserve the effective existing unit, arguments, directories, transport and secrets. Path/listen/port options configure new installations only.'
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) PREBUILT_BINARY="${2:?missing binary path}"; shift 2 ;;
    --port) PORT="${2:?missing port}"; shift 2 ;;
    --listen-address) LISTEN_ADDRESS="${2:?missing listen address}"; shift 2 ;;
    --base) BASE="${2:?missing base}"; shift 2 ;;
    --data) DATA_DIR="${2:?missing data directory}"; shift 2 ;;
    --jwt-secret-file) JWT_SECRET_FILE="${2:?missing secret path}"; shift 2 ;;
    --admin-password-file) ADMIN_PASSWORD_FILE="${2:?missing password path}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[ "$(id -u)" -eq 0 ] || { echo 'Run this installer as root.' >&2; exit 1; }
for command_name in systemctl python3; do
  command -v "$command_name" >/dev/null || { echo "Missing command: $command_name" >&2; exit 1; }
done
if [ -n "$PREBUILT_BINARY" ]; then
  [ -f "$PREBUILT_BINARY" ] && [ -x "$PREBUILT_BINARY" ] || { echo 'Prebuilt binary must be an executable regular file.' >&2; exit 1; }
  PREBUILT_BINARY="$(realpath -- "$PREBUILT_BINARY")"
  "$PREBUILT_BINARY" --help >/dev/null 2>&1 || { echo 'Prebuilt binary cannot run on this host; check architecture and glibc compatibility.' >&2; exit 1; }
else
  command -v go >/dev/null || { echo 'Missing command: go' >&2; exit 1; }
  [ -f "$SRC_DIR/vendor/modules.txt" ] || { echo 'Committed vendor tree is required.' >&2; exit 1; }
fi
DATA_DIR="${DATA_DIR:-$BASE/data}"
SERVICE=promlifecycle-server.service
SERVICE_FILE=/etc/systemd/system/$SERVICE
EXECUTABLE="$BASE/promlifecycle-server"
TLS_ENABLED=0
EXISTING=0
if [ "$(systemctl show "$SERVICE" -p LoadState --value)" = loaded ]; then
  EXISTING=1
  # Read effective systemd arguments, including drop-ins. Keep the entire unit
  # unchanged rather than rebuilding it and silently discarding TLS/custom flags.
  discovery="$(python3 - "$SERVICE" <<'PY'
import os, re, shlex, subprocess, sys
service=sys.argv[1]
def show(key):
    return subprocess.check_output(['systemctl','show',service,'-p',key,'--value'],text=True).strip()
pid=show('MainPID')
if pid.isdigit() and int(pid)>0:
    with open('/proc/'+pid+'/cmdline','rb') as f:
        args=[os.fsdecode(a) for a in f.read().split(b'\0') if a]
else:
    raw=show('ExecStart')
    m=re.search(r'argv\[\]=(.*?) ; ignore_errors=',raw)
    if not m:
        raise SystemExit('Cannot safely resolve existing ExecStart; refusing upgrade')
    args=shlex.split(m.group(1))
    # systemctl's display is not an argv serialization. Reject ambiguous
    # whitespace rather than choosing the wrong DB while a service is stopped.
    i=1
    while i<len(args):
        if not args[i].startswith('-'):
            raise SystemExit('Ambiguous stopped-service arguments; start original service before upgrading')
        if '=' in args[i] or args[i] in ('--debug','-debug'):
            i+=1
        else:
            i+=2
if not args or any('%' in a or '$' in a for a in args):
    raise SystemExit('Unresolved systemd argument expansion; refusing upgrade')
working=show('WorkingDirectory') or '/'
def argument(name,default):
    value=default
    for i,a in enumerate(args[1:],1):
        if a.lstrip('-')==name.lstrip('-'):
            if i+1==len(args): raise SystemExit('Missing argument value')
            value=args[i+1]
        elif a.lstrip('-').startswith(name.lstrip('-')+'='):
            value=a.split('=',1)[1]
    return value
def absolute(path): return os.path.abspath(os.path.join(working,path))
values={'BASE':absolute(argument('--base-path','/opt/promlifecycle')),
        'DATA_DIR':absolute(argument('--data-dir','./data')),
        'EXECUTABLE':absolute(args[0]),'SERVICE_FILE':show('FragmentPath'),
        'PORT':argument('--port','8080'),
        'LISTEN_ADDRESS':argument('--listen-address','127.0.0.1'),
        'TLS_ENABLED':'1' if argument('--tls-cert','') else '0'}
if not values['SERVICE_FILE'] or not os.path.isfile(values['EXECUTABLE']):
    raise SystemExit('Existing executable/unit unavailable; refusing upgrade')
for k,v in values.items(): print(k+'='+shlex.quote(v))
PY
)"
  eval "$discovery"
fi
JWT_SECRET_FILE="${JWT_SECRET_FILE:-$BASE/jwt_secret}"
ADMIN_PASSWORD_FILE="${ADMIN_PASSWORD_FILE:-$BASE/admin_initial_password}"
# Resolve new-install paths in the invocation directory before creating files.
# Existing service paths were already resolved against its WorkingDirectory.
paths="$(python3 - "$BASE" "$DATA_DIR" "$JWT_SECRET_FILE" "$ADMIN_PASSWORD_FILE" "$EXECUTABLE" <<'PY_PATHS'
import os, shlex, sys
for key,value in zip(('BASE','DATA_DIR','JWT_SECRET_FILE','ADMIN_PASSWORD_FILE','EXECUTABLE'),sys.argv[1:]):
    print(key+'='+shlex.quote(os.path.abspath(value)))
PY_PATHS
)"
eval "$paths"
if [ "$EXISTING" -eq 0 ]; then
  python3 - "$LISTEN_ADDRESS" <<'PY_LISTEN'
import ipaddress, sys
address=sys.argv[1]
try:
    if '%' in address:
        raise ValueError('scoped addresses require systemd expansion')
    if address != 'localhost':
        ipaddress.ip_address(address)
except ValueError:
    raise SystemExit('Invalid listen address: use an IP address or localhost')
PY_LISTEN
fi
DB_FILE="$DATA_DIR/promlifecycle.db"
mkdir -p "$BASE" "$DATA_DIR" "$BASE"/{configs,packages,bin,backups,compose,uploads}
BUILD_DIR="$(mktemp -d)"
EXEC_BACKUP=""
UNIT_BACKUP=""
DB_BACKUP=""
TRANSACTION=0
STOPPED_FOR_BACKUP=0
WAS_ACTIVE=0
WAS_ENABLED=0
HAD_EXEC=0
HAD_UNIT=0
HAD_DB=0
INIT_PID=""
rollback() {
  local failed=0
  if [ -e "$SERVICE_FILE" ] && ! systemctl stop "$SERVICE"; then
    # Invalid new units cannot be stopped, but an explicitly inactive unit with
    # no process is safe to remove. Unknown or active state must retain recovery.
    if [ "$(systemctl show "$SERVICE" -p ActiveState --value)" != inactive ] ||
       [ "$(systemctl show "$SERVICE" -p MainPID --value)" != 0 ]; then
      echo "Cannot stop service safely; original recovery files retained in $DB_BACKUP" >&2
      return 1
    fi
  fi
  if [ -n "$INIT_PID" ]; then kill "$INIT_PID" 2>/dev/null || true; wait "$INIT_PID" 2>/dev/null || true; INIT_PID=""; fi
  if [ "$HAD_EXEC" -eq 1 ]; then
    cp -a "$EXEC_BACKUP/original" "$EXEC_BACKUP/rollback" && mv -f "$EXEC_BACKUP/rollback" "$EXECUTABLE" || failed=1
  else rm -f "$EXECUTABLE" || failed=1; fi
  if [ "$HAD_UNIT" -eq 1 ]; then
    cp -a "$UNIT_BACKUP/original" "$UNIT_BACKUP/rollback" && mv -f "$UNIT_BACKUP/rollback" "$SERVICE_FILE" || failed=1
  else rm -f "$SERVICE_FILE" || failed=1; fi
  rm -f "$DB_FILE-wal" "$DB_FILE-shm" || failed=1
  if [ "$HAD_DB" -eq 1 ]; then
    cp -a "$DB_BACKUP/original.db" "$DB_BACKUP/rollback.db" && mv -f "$DB_BACKUP/rollback.db" "$DB_FILE" || failed=1
  else rm -f "$DB_FILE" || failed=1; fi
  rm -f "$DB_FILE".restore* || failed=1
  for file in "$DB_BACKUP"/restore/*; do
    [ -f "$file" ] || continue
    cp -a "$file" "$DATA_DIR/$(basename "$file")" || failed=1
  done
  systemctl daemon-reload || failed=1
  if [ "$WAS_ENABLED" -eq 0 ]; then systemctl disable "$SERVICE" >/dev/null 2>&1 || true; fi
  if [ "$WAS_ACTIVE" -eq 1 ] && [ "$failed" -eq 0 ]; then systemctl start "$SERVICE" || failed=1; fi
  if [ "$failed" -ne 0 ]; then
    echo "ROLLBACK FAILED; service left stopped. Recovery files: $EXEC_BACKUP $UNIT_BACKUP $DB_BACKUP" >&2
    return 1
  fi
  echo 'Upgrade failed; original executable, unit and consistent database restored.' >&2
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [ "$TRANSACTION" -eq 1 ]; then
    rollback || status=1
  elif [ "$status" -ne 0 ] && [ "$STOPPED_FOR_BACKUP" -eq 1 ] && [ "$WAS_ACTIVE" -eq 1 ]; then
    systemctl start "$SERVICE" || status=1
  fi
  if [ -n "$INIT_PID" ]; then kill "$INIT_PID" 2>/dev/null || true; wait "$INIT_PID" 2>/dev/null || true; fi
  rm -rf "$BUILD_DIR"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [ -n "$PREBUILT_BINARY" ]; then
  printf '%s\n' 'Installing supplied release binary...'
  install -m 0755 "$PREBUILT_BINARY" "$BUILD_DIR/promlifecycle-server"
else
  printf '%s\n' 'Building with committed vendor dependencies...'
  (cd "$SRC_DIR"; GOFLAGS=-mod=vendor go build -trimpath -o "$BUILD_DIR/promlifecycle-server" ./cmd/server)
fi
EXEC_BACKUP="$(mktemp -d "$(dirname "$EXECUTABLE")/.promlifecycle-upgrade-XXXXXX")"
UNIT_BACKUP="$(mktemp -d "$(dirname "$SERVICE_FILE")/.promlifecycle-upgrade-XXXXXX")"
DB_BACKUP="$(mktemp -d "$DATA_DIR/.promlifecycle-upgrade-XXXXXX")"
if [ -e "$EXECUTABLE" ]; then HAD_EXEC=1; cp -a "$EXECUTABLE" "$EXEC_BACKUP/original"; fi
if [ -e "$SERVICE_FILE" ]; then HAD_UNIT=1; cp -a "$SERVICE_FILE" "$UNIT_BACKUP/original"; fi
if systemctl is-active --quiet "$SERVICE"; then WAS_ACTIVE=1; fi
if systemctl is-enabled --quiet "$SERVICE"; then WAS_ENABLED=1; fi
# No live database copy: stop every writer before sqlite backup. If checkpoint or
# snapshot fails, no install mutation has happened and the old service restarts.
if [ "$EXISTING" -eq 1 ]; then
  systemctl stop "$SERVICE"
  STOPPED_FOR_BACKUP=1
fi
if [ -s "$DB_FILE" ]; then
  HAD_DB=1
  if ! python3 - "$DB_FILE" "$DB_BACKUP/original.db" <<'PY'
import os, sqlite3, sys
from pathlib import Path
source,dest=sys.argv[1:]
reserve=int(os.environ.get('PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES',str(64<<20)))
st=os.statvfs(os.path.dirname(dest))
if reserve<0 or st.f_bavail*st.f_frsize < os.stat(source).st_size*2+reserve:
    raise SystemExit('Insufficient disk space for reversible database upgrade')
src=sqlite3.connect(Path(source).resolve().as_uri()+'?mode=rw',uri=True)
if src.execute('PRAGMA wal_checkpoint(TRUNCATE)').fetchone()[0]:
    raise SystemExit('Database still has active writers; refusing upgrade')
out=sqlite3.connect(dest)
src.backup(out)
if out.execute('PRAGMA integrity_check').fetchone()[0]!='ok':
    raise SystemExit('Original database failed integrity validation')
out.close();src.close()
os.chmod(dest,0o600)
with open(dest,'rb') as f: os.fsync(f.fileno())
PY
  then
    if [ "$WAS_ACTIVE" -eq 1 ]; then systemctl start "$SERVICE"; fi
    exit 1
  fi
  # The private backup directory protects the snapshot. Restore the original
  # owner, mode/ACL and xattrs so cp -a rollback remains usable by its service.
  cp --attributes-only --preserve=mode,ownership,timestamps,xattr -- "$DB_FILE" "$DB_BACKUP/original.db"
fi
mkdir -p "$DB_BACKUP/restore"
for file in "$DB_FILE".restore*; do
  [ -f "$file" ] || continue
  if ! cp -a "$file" "$DB_BACKUP/restore/"; then
    if [ "$WAS_ACTIVE" -eq 1 ]; then systemctl start "$SERVICE"; fi
    exit 1
  fi
done
TRANSACTION=1
# New installs alone create secrets. Upgrades do not rewrite or relocate any
# secret, EnvironmentFile, drop-in or argument (including insecure HTTP choice).
if [ "$EXISTING" -eq 0 ]; then
  mkdir -p "$(dirname "$JWT_SECRET_FILE")" "$(dirname "$ADMIN_PASSWORD_FILE")"
  if [ "$HAD_DB" -eq 1 ] && [ ! -s "$JWT_SECRET_FILE" ]; then
    echo 'Existing DB needs its original JWT/encryption secret; refusing install.' >&2; exit 1
  fi
  if [ ! -s "$JWT_SECRET_FILE" ]; then python3 -c 'import secrets; print(secrets.token_hex(32))' > "$JWT_SECRET_FILE"; fi
  chmod 0600 "$JWT_SECRET_FILE"
  if [ "$HAD_DB" -eq 0 ] && [ ! -s "$ADMIN_PASSWORD_FILE" ]; then python3 -c 'import secrets; print(secrets.token_hex(16))' > "$ADMIN_PASSWORD_FILE"; fi
  [ ! -e "$ADMIN_PASSWORD_FILE" ] || chmod 0600 "$ADMIN_PASSWORD_FILE"
fi
install -m 0755 "$BUILD_DIR/promlifecycle-server" "$EXEC_BACKUP/candidate"
mv -f "$EXEC_BACKUP/candidate" "$EXECUTABLE"
if [ "$EXISTING" -eq 0 ]; then
  # Reject systemd expansion/newlines rather than creating an ambiguous unit.
  for value in "$BASE" "$DATA_DIR" "$JWT_SECRET_FILE" "$EXECUTABLE" "$PORT" "$LISTEN_ADDRESS"; do
    case "$value" in *[$'\n\r\t"\\%$']*) echo 'Unsupported character in installation path/port.' >&2; exit 1;; esac
  done
  cat > "$UNIT_BACKUP/candidate" <<SERVICE_UNIT
[Unit]
Description=Prometheus Lifecycle Platform
After=network.target
[Service]
Type=simple
User=root
WorkingDirectory=$BASE
Environment="PROMLIFECYCLE_ALLOW_INSECURE_HTTP=true"
Environment="PROMLIFECYCLE_JWT_SECRET_FILE=$JWT_SECRET_FILE"
ExecStart="$EXECUTABLE" --listen-address "$LISTEN_ADDRESS" --port "$PORT" --base-path "$BASE" --data-dir "$DATA_DIR"
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
SERVICE_UNIT
  chmod 0644 "$UNIT_BACKUP/candidate"
  mv -f "$UNIT_BACKUP/candidate" "$SERVICE_FILE"
fi
if [ "$HAD_DB" -eq 0 ]; then
  PROMLIFECYCLE_ALLOW_INSECURE_HTTP=true PROMLIFECYCLE_JWT_SECRET_FILE="$JWT_SECRET_FILE" PROMLIFECYCLE_ADMIN_PASSWORD_FILE="$ADMIN_PASSWORD_FILE" \
    "$EXECUTABLE" --listen-address 127.0.0.1 --port 0 --base-path "$BASE" --data-dir "$DATA_DIR" >"$BUILD_DIR/init.log" 2>&1 &
  INIT_PID=$!
  initialized=0
  for _ in $(seq 1 60); do
    if grep -q 'Default admin user created' "$BUILD_DIR/init.log"; then initialized=1; break; fi
    if ! kill -0 "$INIT_PID" 2>/dev/null; then break; fi
    sleep 1
  done
  kill "$INIT_PID" 2>/dev/null || true; wait "$INIT_PID" 2>/dev/null || true; INIT_PID=""
  if [ "$initialized" -ne 1 ]; then cat "$BUILD_DIR/init.log" >&2; exit 1; fi
fi
systemctl daemon-reload
if [ "$EXISTING" -eq 0 ]; then systemctl enable "$SERVICE" >/dev/null; fi
systemctl start "$SERVICE"
# Readiness must reach the application, not merely a stable process blocked in
# initialization. Probe only the existing local transport without changing it.
started_pid=""
healthy=0
for _ in $(seq 1 60); do
  sleep 1
  systemctl is-active --quiet "$SERVICE" || { journalctl -u "$SERVICE" --no-pager -n 30 >&2; exit 1; }
  pid="$(systemctl show "$SERVICE" -p MainPID --value)"
  [ "$pid" != 0 ] || exit 1
  if [ -n "$started_pid" ] && [ "$pid" != "$started_pid" ]; then echo 'Service restarted during upgrade verification.' >&2; exit 1; fi
  started_pid="$pid"
  if python3 - "$LISTEN_ADDRESS" "$PORT" "$TLS_ENABLED" <<'PY'
import json, ssl, sys, urllib.request
host,port,tls=sys.argv[1:]
if host in ('','0.0.0.0'): host='127.0.0.1'
if host=='::': host='::1'
if ':' in host: host='['+host+']'
url=('https' if tls=='1' else 'http')+'://'+host+':'+port+'/health'
# Local readiness carries no credentials. The installed certificate can be
# self-signed or issued for a public name, so trust is not evaluated by this probe.
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPSHandler(context=ssl._create_unverified_context()))
try:
    with opener.open(url,timeout=2) as response:
        if response.status!=200 or json.load(response).get('status')!='ok': raise ValueError('not healthy')
except Exception:
    raise SystemExit(1)
PY
  then healthy=$((healthy+1)); else healthy=0; fi
  if [ "$healthy" -ge 3 ]; then break; fi
done
[ "$healthy" -ge 3 ] || { echo 'Application health check failed; rolling back upgrade.' >&2; exit 1; }
TRANSACTION=0
printf 'Deployment complete. Existing transport and service arguments preserved on upgrades.\n'
printf 'Rollback artifacts retained: %s %s %s\n' "$EXEC_BACKUP" "$UNIT_BACKUP" "$DB_BACKUP"
if [ "$HAD_DB" -eq 0 ]; then printf 'Initial admin password: %s\n' "$ADMIN_PASSWORD_FILE"; fi
printf 'Service: systemctl status %s\n' "$SERVICE"
