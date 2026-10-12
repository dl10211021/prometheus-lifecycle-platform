"""Real HTTP/auth/database contract against an owned, isolated server instance."""
import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import subprocess
import time
import urllib.error
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--server", required=True)
parser.add_argument("--output", required=True)
args = parser.parse_args()
server_binary = Path(args.server).resolve(strict=True)
output = Path(args.output).resolve()
output.mkdir(mode=0o700, parents=True, exist_ok=False)
base_path = output / "base"
base_path.mkdir(mode=0o700)
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
base = f"http://127.0.0.1:{port}"
password = secrets.token_hex(20) + "Aa9!"
env = os.environ.copy()
env.update(PROMLIFECYCLE_ADMIN_PASSWORD=password, PROMLIFECYCLE_JWT_SECRET=secrets.token_hex(32),
           PROMLIFECYCLE_OFFLINE="1", PROMLIFECYCLE_ALLOW_PRIVATE_FETCH="1",
           PROMLIFECYCLE_ALLOW_INSECURE_HTTP="true", BACKUP_ROOT=str(base_path / "backups"))
# The server initialization may inspect service state. It must never operate on
# real units in this isolated API test. Mutating lifecycle calls leave a canary.
bin_dir = output / "test-bin"
bin_dir.mkdir()
control = bin_dir / "systemctl"
control.write_text('#!/bin/sh\ncase "$1" in start|stop|restart|enable|disable|daemon-reload) echo "$1" >> "' + str(output / "runtime-writes") + '";; esac\nexit 3\n')
control.chmod(0o700)
env["PATH"] = str(bin_dir) + os.pathsep + env["PATH"]
log = (output / "server.log").open("w")
process = subprocess.Popen([str(server_binary), "--listen-address", "127.0.0.1", "--port", str(port),
                            "--base-path", str(base_path), "--data-dir", str(output)], env=env, stdout=log, stderr=subprocess.STDOUT)
def call(method, path, token=None, body=None):
    headers = {}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None if body is None else json.dumps(body).encode()
    if data is not None:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(base + "/api/v1" + path, data=data, headers=headers, method=method)
    try:
        response = urllib.request.urlopen(request, timeout=8)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read()
        return response.status, json.loads(raw) if raw else {}, dict(response.headers)

db = None
try:
    for _ in range(100):
        if process.poll() is not None:
            raise AssertionError("isolated server exited before readiness; inspect private server.log")
        try:
            with urllib.request.urlopen(base + "/health", timeout=1) as response:
                if response.status == 200:
                    break
        except (OSError, urllib.error.URLError):
            time.sleep(0.1)
    else:
        raise AssertionError("isolated server readiness timeout")
    code, login, _ = call("POST", "/auth/login", body={"username": "admin", "password": password})
    assert code == 200, "administrator login failed"
    tokens = {"system_admin": login["token"]}
    ids = {}
    for role in ("viewer", "operator", "change_admin", "auditor"):
        code, user, _ = call("POST", "/users", tokens["system_admin"],
                            {"username": "preview-" + role, "password": password, "role": role})
        assert code == 201, "fixture user creation failed for " + role
        ids[role] = user["id"]
        code, login, _ = call("POST", "/auth/login", body={"username": "preview-" + role, "password": password})
        assert code == 200, "fixture login failed for " + role
        tokens[role] = login["token"]
    db = sqlite3.connect(output / "promlifecycle.db", timeout=8)
    node_id = db.execute("SELECT id FROM clusters LIMIT 1").fetchone()[0]
    host_id = db.execute("SELECT id FROM hosts LIMIT 1").fetchone()[0]
    imported = json.dumps(["--storage.tsdb.retention=045d", "--opaque=private-secret"])
    cursor = db.execute("""INSERT INTO components (name,type,version,deploy_type,status,cluster_id,host_id,imported_args,custom_env,created_at,updated_at)
                          VALUES (?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)""",
                        ("preview-only-fixture", "prometheus", "2.55.0", "binary", "stopped", node_id, host_id, imported, "PRIVATE=env-secret"))
    component_id = cursor.lastrowid
    db.commit()
    endpoint = f"/components/{component_id}/parameter-migration-preview"
    tables = ("components", "change_plans", "backups", "operations", "operation_logs")
    # Freeze writers while testing reads, making even hidden audit writes fail
    # the HTTP timeout instead of blending with scheduler activity.
    db.execute("BEGIN IMMEDIATE")
    before = {table: db.execute("SELECT * FROM " + table + " ORDER BY id").fetchall() for table in tables}
    canary = (output / "runtime-writes").read_bytes() if (output / "runtime-writes").exists() else b""
    code, _, _ = call("GET", endpoint + "?target_version=3.1.0")
    assert code == 401, ("unauthenticated", code)
    for role, token in tokens.items():
        code, result, headers = call("GET", endpoint + "?target_version=3.1.0", token)
        if role in ("system_admin", "change_admin"):
            assert code == 200, (role, code)
            p = result["preview"]
            assert p["status"] == "available" and p["preview_only"] and p["changes"][0]["value"] == "045d", p
            assert not p["current_compatible"] and p["converted_compatible"], p
            assert headers.get("Cache-Control") == "no-store"
            assert "secret" not in json.dumps(result) and "imported_args" not in result
        else:
            assert code == 403, (role, code)
    for query in ("", "?target_version=", "?target_version=3.1.0&target_version=2.55.0",
                  "?target_version=3.1.0&arguments=forged", "?target_version=3.1.0&source_version=2.55.0"):
        code, _, _ = call("GET", endpoint + query, tokens["system_admin"])
        assert code == 400, (query, code)
    code, result, _ = call("GET", endpoint + "?target_version=3.1.0-rc.0", tokens["system_admin"])
    assert code == 200 and result["preview"]["status"] == "blocked", code
    code, _, _ = call("GET", "/components/999999999/parameter-migration-preview?target_version=3.1.0", tokens["system_admin"])
    assert code == 404, code
    after = {table: db.execute("SELECT * FROM " + table + " ORDER BY id").fetchall() for table in tables}
    assert before == after, "preview changed persistent state"
    current_canary = (output / "runtime-writes").read_bytes() if (output / "runtime-writes").exists() else b""
    assert current_canary == canary, "preview reached runtime mutation"
    db.rollback()
    # Authorization comes from current DB state even with an existing JWT.
    db.execute("UPDATE users SET role='viewer' WHERE id=?", (ids["change_admin"],))
    db.execute("UPDATE components SET imported_args=? WHERE id=?",
               (json.dumps(["--storage.tsdb.retention=60d"]), component_id))
    db.commit()
    code, _, _ = call("GET", endpoint + "?target_version=3.1.0", tokens["change_admin"])
    assert code == 403, "old JWT retained revoked change permission"
    code, result, _ = call("GET", endpoint + "?target_version=3.1.0", tokens["system_admin"])
    assert code == 200 and result["preview"]["changes"][0]["value"] == "60d", "preview returned stale saved arguments"
    print("PASS: real router authentication, five roles, role revocation, strict queries, fresh saved state, no DB/audit/runtime writes")
finally:
    if db is not None:
        db.close()
    process.terminate()
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
    log.close()
