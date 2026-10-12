#!/usr/bin/env python3
"""Local process/systemd boundary for deploy.sh integration tests.

Database initialization, file metadata, the installer, and HTTP readiness are real.
Only systemctl is substituted, and the unit lives inside the disposable fixture.
"""
import argparse
import json
import os
from pathlib import Path
import shlex
import signal
import sqlite3
import subprocess
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

root = Path(os.environ["INSTALLER_TEST_ROOT"])
unit = root / "promlifecycle-server.service"
state_path = root / "service-state.json"


def read_unit():
    settings = {}
    for line in unit.read_text().splitlines():
        if "=" in line:
            key, value = line.split("=", 1)
            if key == "Environment":
                settings.setdefault("environment", {}).update(
                    dict(item.split("=", 1) for item in shlex.split(value)))
            else:
                settings[key] = value
    return settings


def systemctl():
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    command = sys.argv[1]
    if command == "show":
        key = sys.argv[sys.argv.index("-p") + 1]
        settings = read_unit() if unit.exists() else {}
        values = {
            "LoadState": "loaded" if unit.exists() else "not-found",
            "MainPID": str(state.get("pid", 0)),
            "FragmentPath": str(unit),
            "WorkingDirectory": settings.get("WorkingDirectory", ""),
            "ActiveState": "active" if state.get("active") else "inactive",
            "ExecStart": "{ argv[]=" + settings.get("ExecStart", "") + " ; ignore_errors=no ; }",
        }
        print(values[key])
        return 0
    if command == "is-active":
        return 0 if state.get("active") else 3
    if command == "is-enabled":
        return 0 if state.get("enabled") else 1
    if command == "stop":
        pid = state.get("pid", 0)
        if pid:
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            for _ in range(100):
                proc = Path("/proc") / str(pid) / "stat"
                if not proc.exists() or proc.read_text().split()[2] == "Z":
                    break
                time.sleep(0.01)
        state.update(active=False, pid=0)
    elif command in ("enable", "disable"):
        state["enabled"] = command == "enable"
    elif command == "start":
        settings = read_unit()
        args = shlex.split(settings["ExecStart"])
        if os.environ.get("INSTALLER_TEST_MODE") == "rollback":
            db_path = args[args.index("--data-dir") + 1] + "/promlifecycle.db"
            if Path(args[0]).read_text().splitlines()[1] != "# installer-original":
                with sqlite3.connect(db_path) as db:
                    db.execute("UPDATE marker SET value='candidate'")
                os.chown(db_path, 0, 0)
                os.chmod(db_path, 0o600)
                os.setxattr(db_path, "user.installer-test", b"candidate")
                return 1  # Candidate start fails after changing the database.
            probe = subprocess.run([
                sys.executable, "-c",
                "import os,sqlite3,sys; os.setgid(65534); os.setuid(65534); "
                "db=sqlite3.connect(sys.argv[1]); db.execute('PRAGMA user_version=7'); "
                "db.close()", db_path
            ], capture_output=True)
            if probe.returncode:
                sys.stderr.buffer.write(probe.stderr)
                return 1
            (root / "original-user-reopened").write_text("ok")
            state.update(active=True, pid=0)
        else:
            env = dict(os.environ)
            env.update(settings.get("environment", {}))
            process = subprocess.Popen(
                args, cwd=settings["WorkingDirectory"], env=env,
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL, start_new_session=True)
            state.update(active=True, pid=process.pid)
    elif command != "daemon-reload":
        raise RuntimeError("Unsupported systemctl command: " + command)
    state_path.write_text(json.dumps(state))
    return 0


def server():
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen-address", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--base-path", required=True)
    parser.add_argument("--data-dir", required=True)
    args = parser.parse_args()
    data = Path(args.data_dir).absolute()
    data.mkdir(parents=True, exist_ok=True)
    db_path = data / "promlifecycle.db"
    initializing = bool(os.environ.get("PROMLIFECYCLE_ADMIN_PASSWORD_FILE"))
    with sqlite3.connect(str(db_path)) as db:
        db.execute("CREATE TABLE IF NOT EXISTS marker(value TEXT)")
        if not db.execute("SELECT COUNT(*) FROM marker").fetchone()[0]:
            db.execute("INSERT INTO marker VALUES (?)",
                       ("initialized" if initializing else "unexpected-new-database",))
        marker = db.execute("SELECT value FROM marker").fetchone()[0]
    if initializing:
        print("Default admin user created", flush=True)
        while True:
            signal.pause()
    (root / "service-observation.json").write_text(json.dumps({
        "database": str(db_path), "marker": marker,
        "base": args.base_path, "listen": args.listen_address,
        "jwt": os.environ.get("PROMLIFECYCLE_JWT_SECRET_FILE"),
    }))

    class Health(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')

        def log_message(self, *args):
            pass

    HTTPServer((args.listen_address, args.port), Health).serve_forever()


if Path(sys.argv[0]).name == "systemctl":
    sys.exit(systemctl())
server()
