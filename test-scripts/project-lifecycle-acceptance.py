#!/usr/bin/env python3
"""One fail-closed, real Prometheus project lifecycle acceptance run.

The controller owns bootstrap and final cleanup. This runner owns only the
manifest-bound fixtures. It never persists API or SSH credentials.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shlex
import sys
import time
import traceback
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterable
from urllib.parse import urlsplit

import paramiko
import requests


REQUIRED_ENV = ("BASE", "PROMLIFECYCLE_ADMIN_PASSWORD", "REMOTE_PASS", "FIXTURE_MANIFEST")
SAFE_ID_KEYS = {
    "id", "component_id", "host_id", "plan_id", "point_id", "backup_id",
    "paired_recovery_point_id", "operation_id", "status", "stage", "phase",
    "eligible", "verified", "type", "version", "mode",
}
TERMINAL_OPERATION = {"success", "failed"}
TERMINAL_PLAN = {"verified", "failed", "rolled_back", "rollback_failed"}
TERMINAL_RESTORE = {"restore_completed", "restore_compensated"}


def require_env() -> dict[str, str]:
    missing = [name for name in REQUIRED_ENV if not os.environ.get(name)]
    if missing:
        raise RuntimeError("missing required environment names: " + ", ".join(missing))
    return {name: os.environ[name] for name in REQUIRED_ENV}


ENV = require_env()
MANIFEST_PATH = Path(ENV["FIXTURE_MANIFEST"]).resolve(strict=True)
MANIFEST = json.loads(MANIFEST_PATH.read_text())
OUT = Path(os.environ.get("ACCEPTANCE_OUT", os.environ.get(
    "PROJECT_ACCEPTANCE_OUT", str(MANIFEST_PATH.parent / "task-1-run")))).resolve()
OUT.mkdir(mode=0o700, parents=True, exist_ok=True)
API_LOG = OUT / "api-requests.jsonl"
SSH_LOG = OUT / "ssh-commands.jsonl"
EVENT_LOG = OUT / "events.jsonl"
RESULT_PATH = OUT / "result.json"
BASE = ENV["BASE"].rstrip("/")
RUN_ID = MANIFEST["run_id"]
FIXTURE_ROOT = MANIFEST["fixture_root"].rstrip("/")
SECRETS = tuple(value for key, value in ENV.items() if key.endswith("PASSWORD") or key.endswith("PASS"))

RESULT: dict[str, Any] = {
    "schema": 1,
    "run_id": RUN_ID,
    "base_commit": MANIFEST.get("base_commit"),
    "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    "status": "running",
    "stages": [],
    "owned": {"hosts": [], "components": [], "plans": [], "backups": [], "retained": []},
    "single_shot_posts": {},
}


def redact(value: Any) -> Any:
    if isinstance(value, str):
        result = value
        for secret in SECRETS:
            if secret:
                result = result.replace(secret, "<redacted>")
        result = re.sub(r"Bearer\s+[A-Za-z0-9._~-]+", "Bearer <redacted>", result)
        return result
    if isinstance(value, list):
        return [redact(v) for v in value]
    if isinstance(value, dict):
        return {k: redact(v) for k, v in value.items() if k.lower() not in {"token", "password", "private_key", "authorization"}}
    return value


def write_json(path: Path, value: Any) -> None:
    path.write_text(json.dumps(redact(value), indent=2, sort_keys=True, ensure_ascii=False) + "\n")


def append_jsonl(path: Path, value: Any) -> None:
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(redact(value), sort_keys=True, ensure_ascii=False) + "\n")


def safe_ids(value: Any, prefix: str = "") -> dict[str, Any]:
    found: dict[str, Any] = {}
    if isinstance(value, dict):
        for key, item in value.items():
            current = f"{prefix}.{key}" if prefix else key
            if key in SAFE_ID_KEYS and isinstance(item, (str, int, float, bool, type(None))):
                found[current] = item
            elif isinstance(item, (dict, list)):
                found.update(safe_ids(item, current))
    elif isinstance(value, list):
        for index, item in enumerate(value[:20]):
            found.update(safe_ids(item, f"{prefix}[{index}]"))
    return found


@contextmanager
def stage(name: str):
    record = {"name": name, "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "status": "running"}
    RESULT["stages"].append(record)
    try:
        yield record
    except Exception as exc:
        record.update(status="failed", error=redact(str(exc)), finished_at=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
        write_json(OUT / f"stage-{len(RESULT['stages']):02d}-{name}.json", record)
        raise
    else:
        record.update(status="passed", finished_at=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
        write_json(OUT / f"stage-{len(RESULT['stages']):02d}-{name}.json", record)


class API:
    def __init__(self) -> None:
        self.session = requests.Session()
        self.single_shot: dict[str, int] = {}

    def request(self, method: str, path: str, *, body: Any = None, expected: Iterable[int] = (200,),
                timeout: float = 30, token: str | None = None, single_shot: str | None = None,
                tolerate_transport: bool = False) -> tuple[int | None, Any | None]:
        if not path.startswith("/") or "//" in path:
            raise AssertionError(f"unsafe API path: {path!r}")
        if single_shot:
            self.single_shot[single_shot] = self.single_shot.get(single_shot, 0) + 1
            RESULT["single_shot_posts"][single_shot] = self.single_shot[single_shot]
            if self.single_shot[single_shot] != 1:
                raise AssertionError(f"single-shot mutation repeated: {single_shot}")
        headers = {}
        if token is not None:
            headers["Authorization"] = "Bearer " + token
        elif self.session.headers.get("Authorization"):
            headers["Authorization"] = self.session.headers["Authorization"]
        started = time.time()
        try:
            response = self.session.request(method, BASE + "/api/v1" + path, json=body, headers=headers,
                                            timeout=timeout)
            try:
                data = response.json() if response.content else {}
            except ValueError:
                data = {"non_json_bytes": len(response.content)}
            append_jsonl(API_LOG, {
                "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "method": method,
                "path": urlsplit(path).path, "status": response.status_code,
                "elapsed_ms": round((time.time() - started) * 1000), "identifiers": safe_ids(data),
            })
            if response.status_code not in set(expected):
                error = data.get("error") if isinstance(data, dict) else None
                raise AssertionError(f"{method} {path} returned {response.status_code}: {redact(error or 'unexpected response')}")
            return response.status_code, data
        except requests.RequestException as exc:
            append_jsonl(API_LOG, {
                "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "method": method,
                "path": urlsplit(path).path, "status": "transport_uncertain",
                "elapsed_ms": round((time.time() - started) * 1000), "error_type": type(exc).__name__,
            })
            if tolerate_transport:
                return None, None
            raise

    def login(self, username: str, password: str) -> str:
        _, data = self.request("POST", "/auth/login", body={"username": username, "password": password}, expected=(200,))
        token = data.get("token")
        if not isinstance(token, str) or len(token) < 20:
            raise AssertionError("login response omitted token")
        return token


class SSH:
    def __init__(self, host: str, expected_key: str):
        self.host = host
        self.expected_key = expected_key
        client = self._connect()
        client.close()

    def _connect(self) -> paramiko.SSHClient:
        for attempt in range(1, 4):
            client = paramiko.SSHClient()
            client.load_system_host_keys()
            client.set_missing_host_key_policy(paramiko.RejectPolicy())
            try:
                client.connect(self.host, port=22, username="root", password=ENV["REMOTE_PASS"], timeout=15,
                               auth_timeout=15, banner_timeout=15, look_for_keys=False, allow_agent=False)
                key = client.get_transport().get_remote_server_key().asbytes()
                actual = hashlib.sha256(key).hexdigest()
                if actual != self.expected_key:
                    raise AssertionError(f"host key mismatch for authorized host {self.host}")
                return client
            except Exception as exc:
                client.close()
                transient = isinstance(exc, (EOFError, ConnectionResetError)) or (
                    isinstance(exc, paramiko.SSHException)
                    and "Error reading SSH protocol banner" in str(exc)
                )
                if not transient or attempt == 3:
                    raise
                time.sleep(attempt)

    def close(self) -> None:
        pass

    def run(self, command: str, *, ok: Iterable[int] = (0,), timeout: int = 40) -> str:
        client = self._connect()
        try:
            stdin, stdout, stderr = client.exec_command(command, timeout=timeout)
            stdin.close()
            out = stdout.read().decode("utf-8", "replace")
            err = stderr.read().decode("utf-8", "replace")
            code = stdout.channel.recv_exit_status()
        finally:
            client.close()
        append_jsonl(SSH_LOG, {"host": self.host, "command": command, "exit": code,
                               "stdout_sha256": hashlib.sha256(out.encode()).hexdigest(),
                               "stderr_sha256": hashlib.sha256(err.encode()).hexdigest()})
        if code not in set(ok):
            raise AssertionError(f"remote command failed on {self.host} with exit {code}: {command}")
        return out

    def read(self, path: str) -> bytes:
        assert_owned_or_system(path)
        client = self._connect()
        try:
            with client.open_sftp() as sftp:
                with sftp.open(path, "rb") as handle:
                    return handle.read()
        finally:
            client.close()

    def write_owned(self, path: str, content: bytes, mode: int = 0o644) -> None:
        if not path.startswith(FIXTURE_ROOT + "/"):
            raise AssertionError(f"refusing nonfixture write: {path}")
        client = self._connect()
        try:
            with client.open_sftp() as sftp:
                with sftp.open(path, "wb") as handle:
                    handle.write(content)
                sftp.chmod(path, mode)
        finally:
            client.close()
        append_jsonl(SSH_LOG, {"host": self.host, "action": "sftp_write", "path": path,
                               "bytes": len(content), "sha256": hashlib.sha256(content).hexdigest()})


def assert_owned_or_system(path: str) -> None:
    allowed = (FIXTURE_ROOT + "/", "/opt/promlifecycle/components/",
               "/etc/promlifecycle-components/", "/etc/systemd/system/promlifecycle-")
    if not path.startswith(allowed):
        raise AssertionError(f"path is outside fixture-derived resources: {path}")


def only_owned_id(value: Any) -> int:
    number = int(value)
    if not (int(MANIFEST["id_min"]) <= number <= int(MANIFEST["id_max"])):
        raise AssertionError(f"allocated id {number} outside fixture range")
    return number


def unit_name(kind: str, component_id: int) -> str:
    return f"promlifecycle-{kind}-{component_id}.service"


def component_paths(kind: str, component_id: int) -> dict[str, str]:
    base = f"{FIXTURE_ROOT}/{kind}-{component_id}"
    result = {"install": f"/opt/promlifecycle/components/{kind}-{component_id}", "data": base + "/data",
              "unit": "/etc/systemd/system/" + unit_name(kind, component_id)}
    if kind == "prometheus":
        result["config"] = f"/etc/promlifecycle-components/prometheus-{component_id}/prometheus.yml"
    return result


def live_absence(ssh: SSH, kind: str, component_id: int, port: int) -> dict[str, str]:
    paths = component_paths(kind, component_id)
    quoted = " ".join(shlex.quote(p) for p in paths.values())
    command = f"for p in {quoted}; do test ! -e \"$p\" || exit 41; done; ! ss -ltn | awk '{{print $4}}' | grep -Eq '(^|:){port}$'"
    ssh.run(command)
    return paths


def wait_operation(api: API, operation_id: int, timeout: int = 660) -> dict[str, Any]:
    end = time.time() + timeout
    while time.time() < end:
        _, op = api.request("GET", f"/operations/{operation_id}")
        if op.get("status") in TERMINAL_OPERATION:
            if op["status"] != "success":
                raise AssertionError(f"deployment operation {operation_id} failed at {op.get('stage')}")
            return op
        time.sleep(2)
    raise TimeoutError(f"operation {operation_id} did not become terminal")


def wait_plan(api: API, plan_id: int, timeout: int = 900) -> dict[str, Any]:
    end = time.time() + timeout
    latest: dict[str, Any] = {}
    while time.time() < end:
        _, latest = api.request("GET", f"/change-plans/{plan_id}")
        if latest.get("status") in TERMINAL_PLAN:
            return latest
        time.sleep(3)
    raise TimeoutError(f"change plan {plan_id} did not become terminal; last={latest.get('status')}")


def wait_restore(api: API, plan_id: int, timeout: int = 900) -> dict[str, Any]:
    end = time.time() + timeout
    latest: dict[str, Any] = {}
    while time.time() < end:
        _, latest = api.request("GET", f"/change-plans/{plan_id}/paired-recovery")
        transaction = latest.get("transaction") or {}
        if transaction.get("phase") in TERMINAL_RESTORE:
            return latest
        time.sleep(3)
    raise TimeoutError(f"paired recovery {plan_id} did not become terminal")


def deploy(api: API, ssh: SSH, expected_id: int, payload: dict[str, Any],
           allow_integration_failure: bool = False) -> tuple[dict[str, Any], dict[str, Any]]:
    kind, port = payload["type"], int(payload["port"])
    paths = live_absence(ssh, kind, expected_id, port)
    _, preflight = api.request("POST", "/components/preflight", body=payload, expected=(200,))
    if not preflight.get("passed") or not preflight.get("preflight_token"):
        raise AssertionError(f"{kind} deployment preflight failed")
    create = dict(payload, preflight_token=preflight["preflight_token"])
    _, response = api.request("POST", "/components", body=create, expected=(202,), timeout=45)
    component_id = only_owned_id(response["component"]["id"])
    if component_id != expected_id:
        raise AssertionError(f"unexpected {kind} id {component_id}; absence proof covered {expected_id}")
    RESULT["owned"]["components"].append(component_id)
    operation_id = int(response["operation"]["id"])
    try:
        wait_operation(api, operation_id)
    except AssertionError:
        _, failed_operation = api.request("GET", f"/operations/{operation_id}")
        if not allow_integration_failure or failed_operation.get("stage") != "integration_failed":
            raise
        append_jsonl(EVENT_LOG, {
            "event": "accepted_fixture_integration_failure",
            "operation_id": operation_id,
            "component_id": component_id,
            "stage": failed_operation.get("stage"),
            "error": redact(str(failed_operation.get("error") or "")),
        })
    _, component = api.request("GET", f"/components/{component_id}?raw=1")
    if component.get("status") != "running" or int(component.get("port", 0)) != port:
        raise AssertionError(f"{kind} did not become running on selected port")
    if component.get("service_name") != unit_name(kind, component_id).removesuffix(".service"):
        raise AssertionError(f"{kind} service identity differs from owned unit")
    return component, paths


def resume_owned_component(api: API, ssh: SSH, expected_id: int, payload: dict[str, Any], existing: dict[str, Any]) -> tuple[dict[str, Any], dict[str, str]]:
    kind, port = payload["type"], int(payload["port"])
    if (int(existing.get("id", 0)) != expected_id or existing.get("name") != payload["name"]
            or existing.get("type") != kind or int(existing.get("host_id", 0)) != int(payload["host_id"])
            or int(existing.get("port", 0)) != port or existing.get("deploy_type") != "binary"):
        raise AssertionError("pre-existing component is not the exact interrupted owned fixture")
    _, component = api.request("GET", f"/components/{expected_id}?raw=1")
    paths = component_paths(kind, expected_id)
    if component.get("install_path") != paths["install"] or component.get("service_name") != unit_name(kind, expected_id).removesuffix(".service"):
        raise AssertionError("interrupted owned fixture paths or unit do not match the manifest-derived contract")
    if component.get("status") != "running":
        raise AssertionError("interrupted owned fixture is not running")
    RESULT["owned"]["components"].append(expected_id)
    independent_runtime(ssh, component, "/metrics" if kind == "node_exporter" else "/-/ready")
    return component, paths


def api_health(url: str, expected: int = 200) -> dict[str, Any]:
    response = requests.get(url, timeout=10)
    if response.status_code != expected:
        raise AssertionError(f"runtime health endpoint returned {response.status_code}: {url}")
    try:
        return response.json()
    except ValueError:
        return {"status": response.status_code, "bytes": len(response.content)}


def query(base: str, expression: str, at: float | None = None) -> list[dict[str, Any]]:
    params: dict[str, Any] = {"query": expression}
    if at is not None:
        params["time"] = f"{at:.3f}"
    response = requests.get(base.rstrip("/") + "/api/v1/query", params=params, timeout=15)
    response.raise_for_status()
    payload = response.json()
    if payload.get("status") != "success":
        raise AssertionError("Prometheus query failed")
    return payload["data"]["result"]


def wait_query(base: str, expression: str, predicate, timeout: int = 90) -> tuple[float, list[dict[str, Any]]]:
    end = time.time() + timeout
    while time.time() < end:
        rows = query(base, expression)
        if predicate(rows):
            evaluated_at = float(rows[0]["value"][0])
            fixed = query(base, expression, evaluated_at)
            if predicate(fixed):
                return evaluated_at, fixed
        time.sleep(2)
    raise TimeoutError(f"query did not satisfy acceptance: {expression}")


def fixed_sample(base: str, expression: str, evaluated_at: float, expected: float) -> list[dict[str, Any]]:
    rows = query(base, expression, evaluated_at)
    if len(rows) != 1 or float(rows[0]["value"][1]) != expected:
        raise AssertionError(f"fixed Prometheus sample differs for {expression}")
    if abs(float(rows[0]["value"][0]) - evaluated_at) > 0.001:
        raise AssertionError("Prometheus fixed query returned a different evaluation timestamp")
    return rows


def wait_target_absent(base: str, target: str, timeout: int = 90) -> dict[str, Any]:
    end = time.time() + timeout
    latest: list[dict[str, Any]] = []
    while time.time() < end:
        response = requests.get(base.rstrip("/") + "/api/v1/targets", timeout=15)
        response.raise_for_status()
        payload = response.json()
        latest = (payload.get("data") or {}).get("activeTargets") or []
        selected = [row for row in latest if (row.get("labels") or {}).get("instance") == target]
        if not selected:
            return {"selected_target": target, "selected_active_targets": 0,
                    "all_active_targets": len(latest)}
        time.sleep(2)
    raise TimeoutError(f"selected target remained active after configuration cleanup: {target}")


def create_plan(api: API, kind: str, component_id: int, after: dict[str, Any], reason: str) -> dict[str, Any]:
    def durable_drafts() -> list[dict[str, Any]]:
        _, listing = api.request("GET", f"/change-plans?kind={kind}&limit=100")
        matches: list[dict[str, Any]] = []
        for item in listing.get("plans", []):
            if (int(item.get("resource_id") or 0) != component_id or item.get("reason") != reason
                    or item.get("status") != "draft"):
                continue
            _, durable = api.request("GET", f"/change-plans/{int(item['id'])}")
            if durable.get("after") == after:
                matches.append(durable)
        return matches

    existing = durable_drafts()
    if len(existing) > 1:
        raise AssertionError("multiple matching durable draft plans exist")
    if existing:
        plan = existing[0]
        RESULT["owned"]["plans"].append(int(plan["id"]))
        return plan
    _, response = api.request("POST", "/change-plans", body={
        "kind": kind, "resource_type": "component", "resource_id": component_id,
        "reason": reason, "after": after,
    }, expected=(201,), timeout=240, single_shot=f"create-{kind}-{component_id}", tolerate_transport=True)
    if response is None:
        created = durable_drafts()
        if len(created) != 1:
            raise AssertionError("change-plan create response uncertain and no unique durable draft exists")
        plan = created[0]
    else:
        plan = response["plan"]
    only_owned_id(plan["resource_id"])
    RESULT["owned"]["plans"].append(int(plan["id"]))
    return plan


def execute_plan_once(api: API, plan_id: int, key: str, timeout: int = 930) -> dict[str, Any]:
    api.request("POST", f"/change-plans/{plan_id}/execute", body={}, expected=(200, 409), timeout=timeout,
                single_shot=key, tolerate_transport=True)
    plan = wait_plan(api, plan_id, timeout=timeout)
    if plan.get("status") != "verified":
        raise AssertionError(f"plan {plan_id} ended in {plan.get('status')}")
    return plan


def independent_runtime(ssh: SSH, component: dict[str, Any], ready_path: str) -> dict[str, Any]:
    unit = component["service_name"] + ("" if component["service_name"].endswith(".service") else ".service")
    binary = component.get("binary_path") or (component["install_path"].rstrip("/") + "/" + str(component["type"]))
    assert_owned_or_system(binary)
    assert_owned_or_system("/etc/systemd/system/" + unit)
    active = ssh.run("systemctl is-active " + shlex.quote(unit)).strip()
    main_pid_raw = ssh.run("systemctl show --property=MainPID --value " + shlex.quote(unit)).strip()
    if not main_pid_raw.isdigit() or int(main_pid_raw) <= 0:
        raise AssertionError("systemd unit has no live MainPID")
    main_pid = int(main_pid_raw)
    process_executable = ssh.run(f"readlink -f -- /proc/{main_pid}/exe").strip()
    if process_executable != binary:
        raise AssertionError("running process executable differs from recorded binary path")
    process_sha256 = ssh.run(f"sha256sum -- /proc/{main_pid}/exe").split()[0]
    binary_sha256 = ssh.run("sha256sum -- " + shlex.quote(binary)).split()[0]
    if process_sha256 != binary_sha256 or not re.fullmatch(r"[0-9a-f]{64}", process_sha256):
        raise AssertionError("running process executable digest differs from recorded binary")
    version = ssh.run(shlex.quote(binary) + " --version 2>&1 | head -1").strip()
    runtime_base = f"http://{ssh.host}:{component['port']}"
    health = api_health(runtime_base + ready_path)
    declared_version = str(component.get("version") or "").removeprefix("v")
    if component.get("type") == "prometheus":
        build = api_health(runtime_base + "/api/v1/status/buildinfo")
        build_version = str((build.get("data") or {}).get("version") or "").removeprefix("v")
    else:
        metrics = requests.get(runtime_base + "/metrics", timeout=15)
        metrics.raise_for_status()
        match = re.search(r'^node_exporter_build_info\{[^\n]*version="([^"]+)"', metrics.text, re.MULTILINE)
        build_version = match.group(1).removeprefix("v") if match else ""
    if not declared_version or build_version != declared_version or declared_version not in version:
        raise AssertionError("HTTP build info, component version and executable version disagree")
    return {"unit": unit, "active": active, "main_pid": main_pid,
            "process_executable": process_executable, "process_sha256": process_sha256,
            "binary_sha256": binary_sha256, "version_line": version,
            "http_build_version": build_version, "health": health}


def removed_runtime_evidence(ssh: SSH, paths: Iterable[str], unit: str, port: int) -> dict[str, Any]:
    paths = list(paths)
    load_state = ssh.run("systemctl show --property=LoadState --value " + shlex.quote(unit)).strip()
    if load_state != "not-found":
        raise AssertionError(f"removed unit load state is {load_state}")
    active_state = ssh.run("systemctl is-active " + shlex.quote(unit), ok=(0, 3, 4)).strip()
    if active_state == "active":
        raise AssertionError("removed service remains active")
    for path in paths:
        present = ssh.run(
            f"if test -e {shlex.quote(path)}; then printf present; else printf absent; fi").strip()
        if present != "absent":
            raise AssertionError(f"removed resource retained owned path: {path}")
    port_state = ssh.run(
        f"if ss -ltn | awk '{{print $4}}' | grep -Eq '(^|:){port}$'; "
        "then printf occupied; else printf absent; fi").strip()
    if port_state != "absent":
        raise AssertionError("removed resource retained the owned listening port")
    return {"unit_load_state": load_state, "unit_active_state": active_state,
            "port_state": port_state, "paths_absent": list(paths)}


def remove_plan(api: API, component_id: int, mode: str) -> dict[str, Any]:
    plan = create_plan(api, "remove", component_id, {"mode": mode}, f"isolated acceptance {mode}")
    return execute_plan_once(api, int(plan["id"]), f"remove-{mode}-{component_id}")


def assert_visible_paired_recovery(page, plan_id: int, timeout_ms: int = 30000) -> None:
    """Require the selected plan's rendered result, unobstructed by a modal."""
    from playwright.sync_api import expect

    blocker = page.get_by_role("dialog", name="首次登录必须修改密码", exact=True)
    if blocker.is_visible():
        raise AssertionError("mandatory password change blocks browser acceptance")
    drawer = page.get_by_role("dialog", name=f"变更计划 #{plan_id}", exact=True)
    title = drawer.get_by_text(f"变更计划 #{plan_id}", exact=True)
    panel = drawer.get_by_test_id("paired-recovery-panel")
    outcome = panel.get_by_role("alert").get_by_text("历史状态恢复成功", exact=True)
    phase = panel.locator("dl").filter(has=page.get_by_text("当前阶段", exact=True)).locator("dd")
    expect(phase).to_have_text("历史状态恢复成功", timeout=timeout_ms)
    for target in (title, outcome, phase):
        expect(target).to_be_visible(timeout=timeout_ms)
        # A visible bounding box alone does not reject an overlay or opacity:0.
        target.click(trial=True, timeout=timeout_ms)
        if not target.evaluate("""element => {
            for (let node = element; node; node = node.parentElement) {
                if (Number(getComputedStyle(node).opacity) === 0) return false;
            }
            return true;
        }"""):
            raise AssertionError("selected recovery result is transparent")
    if blocker.is_visible():
        raise AssertionError("mandatory password change blocks browser acceptance")


def main() -> None:
    runner_path = Path(__file__).resolve()
    execution = {
        "entrypoint": "main", "runner_path": str(runner_path),
        "runner_sha256": hashlib.sha256(runner_path.read_bytes()).hexdigest(),
        "python_version": sys.version.split()[0], "pid": os.getpid(),
        "started_at": RESULT["started_at"],
    }
    write_json(OUT / "execution.json", execution)
    RESULT["execution"] = execution
    baseline_path = MANIFEST_PATH.parent / "business-baseline-and-isolation.json"
    baseline = json.loads(baseline_path.read_text())
    remote_baseline_path = MANIFEST_PATH.parent / "remote-baseline.json"
    remote_baseline = json.loads(remote_baseline_path.read_text())
    api = API()
    ssh_clients: dict[str, SSH] = {}
    admin_token = ""
    prom: dict[str, Any]
    exporter: dict[str, Any]
    prom_paths: dict[str, str]
    exporter_paths: dict[str, str]
    upgrade_plan: dict[str, Any]
    try:
        with stage("isolation") as evidence:
            if MANIFEST.get("schema") != 1 or not MANIFEST.get("remote_ready"):
                raise AssertionError("controller fixture manifest is not ready")
            if MANIFEST.get("base_commit") != "a92ddcc0aa6833ca890bc173c19fe3a84941a9d0":
                raise AssertionError("fixture manifest base commit mismatch")
            if not FIXTURE_ROOT.startswith("/var/lib/plc-project-pa-") or RUN_ID not in FIXTURE_ROOT:
                raise AssertionError("fixture root does not bind run id")
            if baseline.get("prepare") is not True:
                raise AssertionError("controller isolation proof is incomplete")
            baseline_hosts = {item["host"]: item for item in baseline["hosts"]}
            host_keys = {item["host"]: item["host_key_sha256"] for item in remote_baseline["hosts"]}
            for role, host in MANIFEST["hosts"].items():
                proof = baseline_hosts.get(host)
                if not proof or proof.get("fixture_ports_occupied") or proof.get("fixture_paths_present"):
                    raise AssertionError(f"controller absence proof failed for {role}")
                if host not in host_keys:
                    raise AssertionError(f"pinned host key proof missing for {role}")
                ssh_clients[role] = SSH(host, host_keys[host])
                owner = ssh_clients[role].read(FIXTURE_ROOT + "/.acceptance-owner").decode().strip()
                if owner != RUN_ID:
                    raise AssertionError(f"fixture owner marker mismatch for {role}")
            evidence["manifest_sha256"] = hashlib.sha256(MANIFEST_PATH.read_bytes()).hexdigest()
            evidence["baseline_sha256"] = hashlib.sha256(baseline_path.read_bytes()).hexdigest()
            evidence["remote_baseline_sha256"] = hashlib.sha256(remote_baseline_path.read_bytes()).hexdigest()

        with stage("authenticate-and-hosts") as evidence:
            admin_token = api.login("admin", ENV["PROMLIFECYCLE_ADMIN_PASSWORD"])
            api.session.headers["Authorization"] = "Bearer " + admin_token
            _, current = api.request("GET", "/components")
            existing_components = current.get("components") or []
            if len(existing_components) > 2:
                raise AssertionError("isolated platform contains unexpected component rows")
            host_ids: dict[str, int] = {}
            for index, role in enumerate(("prometheus", "exporter")):
                host_status, created = api.request("POST", "/hosts", body={
                    "name": f"{RUN_ID}-{role}", "ip": MANIFEST["hosts"][role], "os": "linux",
                    "connection_type": "ssh", "port": 22, "username": "root",
                    "password": ENV["REMOTE_PASS"], "verify_connection": True,
                    "description": f"owned fixture {RUN_ID}",
                }, expected=(200, 201))
                host_row = created.get("host", created)
                host_id = only_owned_id(created.get("id", host_row.get("id")))
                expected = int(MANIFEST["id_min"]) + index
                if host_id != expected:
                    raise AssertionError(f"unexpected deterministic host id {host_id}, wanted {expected}")
                if host_status == 200 and (created.get("status") != "exists" or host_row.get("ip") != MANIFEST["hosts"][role]
                                           or host_row.get("name") != f"{RUN_ID}-{role}"):
                    raise AssertionError(f"existing host row is not the owned {role} fixture")
                host_ids[role] = host_id
                RESULT["owned"]["hosts"].append(host_id)
            evidence["host_ids"] = host_ids

        with stage("deploy") as evidence:
            component_seed = int(MANIFEST["id_min"])
            exporter_payload = {
                "type": "node_exporter", "name": f"{RUN_ID}-exporter", "host_id": host_ids["exporter"],
                "version": MANIFEST["versions"]["exporter"], "port": MANIFEST["ports"]["exporter"],
                "deploy_type": "binary", "install_path": component_paths("node_exporter", component_seed)["install"],
                "data_path_override": component_paths("node_exporter", component_seed)["data"],
                "custom_args": f"--collector.textfile.directory={FIXTURE_ROOT}/textfile", "replace_existing": False,
            }
            existing_by_type = {item.get("type"): item for item in existing_components}
            existing_exporter = existing_by_type.get("node_exporter")
            if existing_exporter:
                exporter, exporter_paths = resume_owned_component(
                    api, ssh_clients["exporter"], component_seed, exporter_payload, existing_exporter)
                evidence["resumed_exporter_after_integration_failure"] = True
            else:
                exporter, exporter_paths = deploy(api, ssh_clients["exporter"], component_seed, exporter_payload)
            existing_prom = existing_by_type.get("prometheus")
            prom_id = int(existing_prom["id"]) if existing_prom else component_seed + 1
            prom_payload = {
                "type": "prometheus", "name": f"{RUN_ID}-prometheus", "host_id": host_ids["prometheus"],
                "version": MANIFEST["versions"]["historical"], "port": MANIFEST["ports"]["prometheus"],
                "deploy_type": "binary", "install_path": component_paths("prometheus", prom_id)["install"],
                "data_path_override": component_paths("prometheus", prom_id)["data"], "replace_existing": False,
            }
            if existing_prom:
                prom, prom_paths = resume_owned_component(
                    api, ssh_clients["prometheus"], prom_id, prom_payload, existing_prom)
                if not prom.get("binary_path"):
                    blocked_id = int(prom["id"])
                    cleanup = remove_plan(api, blocked_id, "full_delete")
                    blocker_cleanup_state = removed_runtime_evidence(
                        ssh_clients["prometheus"], prom_paths.values(),
                        prom["service_name"] + ".service",
                        int(MANIFEST["ports"]["prometheus"]))
                    prom_id = max(int(item["id"]) for item in existing_components) + 1
                    only_owned_id(prom_id)
                    prom_payload.update(
                        install_path=component_paths("prometheus", prom_id)["install"],
                        data_path_override=component_paths("prometheus", prom_id)["data"],
                    )
                    prom, prom_paths = deploy(api, ssh_clients["prometheus"], prom_id, prom_payload)
                    if prom.get("binary_path") != prom_paths["install"] + "/prometheus":
                        raise AssertionError("public redeployment did not persist selected Prometheus executable")
                    evidence.update(identity_blocker_component_id=blocked_id,
                                    identity_blocker_cleanup_plan=int(cleanup["id"]),
                                    identity_blocker_cleanup_state=blocker_cleanup_state,
                                    redeployed_after_identity_fix=True)
                else:
                    evidence["resumed_prometheus_after_config_timeout"] = True
            else:
                prom, prom_paths = deploy(api, ssh_clients["prometheus"], prom_id, prom_payload)
            if existing_exporter and not existing_prom:
                _, retried = api.request("POST", f"/components/{exporter['id']}/integration/retry", body={}, timeout=300)
                retry_operation = int(retried["operation_id"])
                _, retry_state = api.request("GET", f"/operations/{retry_operation}")
                if retry_state.get("status") != "success":
                    raise AssertionError("public exporter integration retry did not repair the deployment operation")
            runtime_prom = independent_runtime(ssh_clients["prometheus"], prom, "/-/ready")
            runtime_exporter = independent_runtime(ssh_clients["exporter"], exporter, "/metrics")
            if MANIFEST["versions"]["historical"] not in runtime_prom["version_line"]:
                raise AssertionError("independent historical Prometheus version mismatch")
            if MANIFEST["versions"]["exporter"] not in runtime_exporter["version_line"]:
                raise AssertionError("independent node exporter version mismatch")
            evidence.update(prometheus_id=prom["id"], exporter_id=exporter["id"],
                            prometheus_runtime=runtime_prom, exporter_runtime=runtime_exporter)

        prom_base = f"http://{MANIFEST['hosts']['prometheus']}:{MANIFEST['ports']['prometheus']}"
        target = f"{MANIFEST['hosts']['exporter']}:{MANIFEST['ports']['exporter']}"
        baseline_config = ("global:\n  scrape_interval: 1s\n  evaluation_interval: 1s\n"
                           "scrape_configs:\n  - job_name: project-acceptance-exporter\n"
                           f"    static_configs:\n      - targets: ['{target}']\n")
        config_name = "prometheus.yml"
        with stage("configuration-backup-permission") as evidence:
            remote = ssh_clients["prometheus"].read(prom["config_path"])
            _, backup_list = api.request("GET", "/backups?limit=500")
            prior_backups = [item for item in backup_list.get("backups", [])
                             if item.get("name") == f"{RUN_ID}-config-baseline" and int(item.get("component_id") or 0) == int(prom["id"])]
            if remote == baseline_config.encode() and prior_backups:
                backup_id = int(prior_backups[-1]["id"])
                _, config_list = api.request("GET", f"/components/{prom['id']}/configs?raw=1")
                baseline_versions = [item for item in config_list.get("configs", []) if item.get("content") == baseline_config]
                if not baseline_versions:
                    raise AssertionError("durable baseline config history missing during resume")
                baseline_config_version = int(baseline_versions[-1]["version"])
                current_hash = hashlib.sha256(remote).hexdigest()
                RESULT["owned"]["backups"].append(backup_id)
                prior_stage = None
                for candidate in sorted(MANIFEST_PATH.parent.glob(
                        "run-attempt-*/stage-04-configuration-backup-permission.json"), reverse=True):
                    prior = json.loads(candidate.read_text())
                    if prior.get("status") == "passed" and int(prior.get("backup_id") or 0) == backup_id:
                        prior_stage = candidate
                        break
                if prior_stage is None:
                    raise AssertionError("matching prior configuration stage evidence missing during resume")
                evidence["resumed_from_attempt"] = prior_stage.parent.name
                evidence["prior_stage_sha256"] = hashlib.sha256(prior_stage.read_bytes()).hexdigest()
            else:
                _, published = api.request("POST", f"/components/{prom['id']}/configs", body={
                    "name": config_name, "content": baseline_config, "content_type": "yaml",
                    "description": "project acceptance baseline", "reason": "isolated project acceptance",
                }, timeout=240)
                baseline_config_version = int(published["config"]["version"])
                remote = ssh_clients["prometheus"].read(prom["config_path"])
                if remote != baseline_config.encode():
                    raise AssertionError("published configuration bytes differ remotely")
                _, backup_response = api.request("POST", "/backups", body={
                    "name": f"{RUN_ID}-config-baseline", "component_id": prom["id"], "type": "config",
                }, expected=(201,), timeout=180)
                backup_id = int(backup_response["backup"]["id"])
                RESULT["owned"]["backups"].append(backup_id)
                mutated = baseline_config + f"# temporary-{RUN_ID}\n"
                api.request("POST", f"/components/{prom['id']}/configs", body={
                    "name": config_name, "content": mutated, "content_type": "yaml",
                    "reason": "prove generic backup restore",
                }, timeout=180)
                if ssh_clients["prometheus"].read(prom["config_path"]) != mutated.encode():
                    raise AssertionError("valid configuration mutation was not published exactly")
                api.request("POST", f"/backups/{backup_id}/restore", body={}, timeout=240)
                if ssh_clients["prometheus"].read(prom["config_path"]) != baseline_config.encode():
                    raise AssertionError("configuration backup restore did not restore exact bytes")
                before_invalid = hashlib.sha256(ssh_clients["prometheus"].read(prom["config_path"])).hexdigest()
                api.request("POST", f"/components/{prom['id']}/configs", body={
                    "name": config_name, "content": "global: [invalid", "content_type": "yaml",
                    "reason": "prove invalid configuration rejection",
                }, expected=(400,))
                after_invalid = hashlib.sha256(ssh_clients["prometheus"].read(prom["config_path"])).hexdigest()
                if before_invalid != after_invalid or api_health(prom_base + "/-/ready") == {}:
                    raise AssertionError("invalid configuration changed the healthy runtime")
                viewer_name = "viewer-" + re.sub(r"[^a-z0-9]", "", RUN_ID.lower())[-18:]
                viewer_password = hashlib.sha256((RUN_ID + "-viewer").encode()).hexdigest()[:24] + "Aa9!"
                api.request("POST", "/users", body={"username": viewer_name, "password": viewer_password,
                                                    "role": "viewer", "is_active": True}, expected=(201, 409))
                viewer_token = api.login(viewer_name, viewer_password)
                api.request("POST", f"/components/{prom['id']}/configs", token=viewer_token, body={
                    "name": config_name, "content": mutated, "content_type": "yaml", "reason": "must be denied",
                }, expected=(403,))
                current_hash = hashlib.sha256(ssh_clients["prometheus"].read(prom["config_path"])).hexdigest()
                if current_hash != before_invalid:
                    raise AssertionError("viewer write altered remote config")
            _, sample = wait_query(prom_base, f'up{{instance="{target}"}}', lambda rows: rows and float(rows[0]["value"][1]) == 1)
            evidence.update(config_version=baseline_config_version, backup_id=backup_id,
                            config_sha256=current_hash, selected_target=target, target_sample=sample)

        metric_path = FIXTURE_ROOT + "/textfile/project_acceptance.prom"
        pre_expression = f'project_acceptance_stage{{run_id="{RUN_ID}",phase="pre"}}'
        post_expression = f'project_acceptance_stage{{run_id="{RUN_ID}",phase="post"}}'
        with stage("upgrade-with-paired-capture") as evidence:
            pre_metric = f'# TYPE project_acceptance_stage gauge\nproject_acceptance_stage{{run_id="{RUN_ID}",phase="pre"}} 1\n'.encode()
            ssh_clients["exporter"].write_owned(metric_path, pre_metric)
            pre_ts, _ = wait_query(
                prom_base, pre_expression, lambda rows: len(rows) == 1 and float(rows[0]["value"][1]) == 1)
            expected_upgrade = {
                "new_version": MANIFEST["versions"]["upgrade"], "paired_recovery": True,
            }
            upgrade_plan = create_plan(api, "upgrade", int(prom["id"]), expected_upgrade,
                                       "isolated project acceptance")
            upgrade_plan = execute_plan_once(api, int(upgrade_plan["id"]), "upgrade-execute")
            if not upgrade_plan.get("paired_recovery_point_id") or not upgrade_plan.get("backup_id"):
                raise AssertionError("verified upgrade omitted paired or ordinary backup association")
            _, current_component = api.request("GET", f"/components/{prom['id']}?raw=1")
            runtime = independent_runtime(ssh_clients["prometheus"], current_component, "/-/ready")
            if MANIFEST["versions"]["upgrade"] not in runtime["version_line"]:
                raise AssertionError("independent upgraded Prometheus version mismatch")
            if MANIFEST["versions"]["upgrade"] not in str(current_component.get("version")):
                raise AssertionError("component durable version disagrees after upgrade")
            post_metric = f'# TYPE project_acceptance_stage gauge\nproject_acceptance_stage{{run_id="{RUN_ID}",phase="post"}} 2\n'.encode()
            ssh_clients["exporter"].write_owned(metric_path, post_metric)
            post_ts, _ = wait_query(
                prom_base, post_expression, lambda rows: len(rows) == 1 and float(rows[0]["value"][1]) == 2)
            fixed_pre_rows = fixed_sample(prom_base, pre_expression, pre_ts, 1)
            fixed_post_rows = fixed_sample(prom_base, post_expression, post_ts, 2)
            evidence.update(plan_id=upgrade_plan["id"], backup_id=upgrade_plan["backup_id"],
                            paired_recovery_point_id=upgrade_plan["paired_recovery_point_id"],
                            component_id=prom["id"], pre_evaluation_time=pre_ts,
                            post_evaluation_time=post_ts, pre_sample=fixed_pre_rows, post_sample=fixed_post_rows,
                            runtime=runtime)

        with stage("paired-recovery") as evidence:
            _, preview = api.request("GET", f"/change-plans/{upgrade_plan['id']}/paired-recovery")
            if not preview.get("eligible") or not preview.get("preview"):
                raise AssertionError("paired recovery preview is not eligible")
            api.request("POST", f"/change-plans/{upgrade_plan['id']}/paired-recovery", body={},
                        expected=(200, 409), timeout=930, single_shot="paired-recovery", tolerate_transport=True)
            durable = wait_restore(api, int(upgrade_plan["id"]), timeout=930)
            transaction = durable.get("transaction") or {}
            if transaction.get("phase") != "restore_completed":
                raise AssertionError(f"paired recovery ended in {transaction.get('phase')}")
            _, restored_component = api.request("GET", f"/components/{prom['id']}?raw=1")
            runtime = independent_runtime(ssh_clients["prometheus"], restored_component, "/-/ready")
            if MANIFEST["versions"]["historical"] not in runtime["version_line"]:
                raise AssertionError("paired recovery did not restore historical binary/runtime")
            if ssh_clients["prometheus"].read(restored_component["config_path"]) != baseline_config.encode():
                raise AssertionError("paired recovery did not restore historical config bytes")
            restored_pre_rows = fixed_sample(prom_base, pre_expression, pre_ts, 1)
            if query(prom_base, post_expression, post_ts):
                raise AssertionError("post-capture fixed sample still exists after paired recovery")
            evidence.update(transaction_id=transaction.get("id"), phase=transaction.get("phase"),
                            plan_id=upgrade_plan["id"], component_id=prom["id"],
                            historical_version=restored_component.get("version"), runtime=runtime,
                            pre_evaluation_time=pre_ts, post_evaluation_time=post_ts,
                            restored_pre_sample=restored_pre_rows, restored_post_sample=[])

        with stage("real-browser") as evidence:
            from playwright.sync_api import sync_playwright
            screenshot = OUT / "real-browser-paired-recovery.png"
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(headless=True, executable_path=os.environ.get(
                    "PLAYWRIGHT_CHROMIUM_EXECUTABLE", "/root/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome"))
                page = browser.new_page(viewport={"width": 1440, "height": 1080})
                errors: list[str] = []
                page.on("pageerror", lambda error: errors.append(str(error)))
                page.goto(BASE, wait_until="domcontentloaded")
                page.get_by_placeholder("账号").fill("admin")
                page.get_by_placeholder("密码").fill(ENV["PROMLIFECYCLE_ADMIN_PASSWORD"])
                page.get_by_role("button", name="登录管理平台", exact=True).click()
                page.locator(".identity").get_by_text("admin", exact=True).wait_for(state="visible", timeout=30000)
                page.goto(BASE + "#domain=changes&changes=plans&resource=plan:" + str(int(upgrade_plan["id"])),
                          wait_until="domcontentloaded")
                try:
                    assert_visible_paired_recovery(page, int(upgrade_plan["id"]))
                finally:
                    page.screenshot(path=str(screenshot), full_page=True, animations="disabled")
                if errors:
                    raise AssertionError("real browser page errors: " + "; ".join(errors))
                browser.close()
            evidence.update(screenshot=str(screenshot), screenshot_sha256=hashlib.sha256(screenshot.read_bytes()).hexdigest())

        with stage("retirement") as evidence:
            keep_service = remove_plan(api, int(exporter["id"]), "keep_service")
            exporter_unit = exporter["service_name"] + ".service"
            if ssh_clients["exporter"].run("systemctl is-active " + shlex.quote(exporter_unit)).strip() != "active":
                raise AssertionError("keep_service stopped retained exporter")
            if not ssh_clients["exporter"].run("test -d " + shlex.quote(exporter_paths["install"]) + " && printf present").strip() == "present":
                raise AssertionError("keep_service removed retained install data")
            retained_runtime = independent_runtime(ssh_clients["exporter"], exporter, "/metrics")
            retained_textfile = ssh_clients["exporter"].read(metric_path)
            if retained_textfile != post_metric:
                raise AssertionError("keep_service did not retain exact owned exporter textfile content")
            cleanup_config = ("global:\n  scrape_interval: 1s\n  evaluation_interval: 1s\n"
                              "scrape_configs:\n  - job_name: project-acceptance-self\n"
                              f"    static_configs:\n      - targets: ['{MANIFEST['hosts']['prometheus']}:{MANIFEST['ports']['prometheus']}']\n")
            _, cleanup_published = api.request("POST", f"/components/{prom['id']}/configs", body={
                "name": config_name, "content": cleanup_config, "content_type": "yaml",
                "description": "project acceptance retirement target cleanup",
                "reason": "remove retired owned exporter target",
            }, timeout=240)
            if ssh_clients["prometheus"].read(prom["config_path"]) != cleanup_config.encode():
                raise AssertionError("retirement configuration cleanup bytes differ remotely")
            target_cleanup = wait_target_absent(prom_base, target)
            cleanup_runtime = independent_runtime(ssh_clients["prometheus"],
                                                  api.request("GET", f"/components/{prom['id']}?raw=1")[1],
                                                  "/-/ready")
            _, detected = api.request("GET", f"/hosts/{host_ids['exporter']}/detect")
            candidates = [row for row in detected.get("components", [])
                          if row.get("type") == "node_exporter" and int(row.get("port", 0)) == int(MANIFEST["ports"]["exporter"])]
            if len(candidates) != 1 or not candidates[0].get("fingerprint"):
                raise AssertionError("retained owned exporter was not uniquely rediscovered")
            _, adopted = api.request("POST", f"/hosts/{host_ids['exporter']}/adopt", body={
                "type": "node_exporter", "port": MANIFEST["ports"]["exporter"],
                "name": f"{RUN_ID}-retained-observation", "ownership": "observe",
                "fingerprint": candidates[0]["fingerprint"],
            }, expected=(201,), timeout=90)
            adopted_id = only_owned_id(adopted["component"]["id"])
            if adopted["adoption"].get("ownership") != "observe" or adopted["component"].get("deploy_type") != "target":
                raise AssertionError("retained fixture was not adopted in observation mode")
            RESULT["owned"]["components"].append(adopted_id)
            RESULT["owned"]["retained"].append({"component_id": adopted_id, "reason": "observed keep_service fixture"})
            if ssh_clients["exporter"].read(metric_path) != retained_textfile:
                raise AssertionError("observation adoption changed retained exporter textfile content")

            extra_id = adopted_id + 1
            extra_payload = {
                "type": "node_exporter", "name": f"{RUN_ID}-full-delete", "host_id": host_ids["exporter"],
                "version": MANIFEST["versions"]["exporter"], "port": MANIFEST["ports"]["extra_exporter"],
                "deploy_type": "binary", "install_path": component_paths("node_exporter", extra_id)["install"],
                "data_path_override": component_paths("node_exporter", extra_id)["data"], "replace_existing": False,
            }
            extra, extra_paths = deploy(api, ssh_clients["exporter"], extra_id, extra_payload,
                                        allow_integration_failure=True)
            full_delete = remove_plan(api, int(extra["id"]), "full_delete")
            extra_unit = extra["service_name"] + ".service"
            full_delete_state = removed_runtime_evidence(
                ssh_clients["exporter"], extra_paths.values(), extra_unit,
                int(MANIFEST["ports"]["extra_exporter"]))

            retain_marker_path = prom_paths["data"] + "/.acceptance-retain-marker"
            retain_marker = (RUN_ID + f"\ncomponent={prom['id']}\nplan={upgrade_plan['id']}\n").encode()
            ssh_clients["prometheus"].write_owned(retain_marker_path, retain_marker, mode=0o600)
            data_identity_before = ssh_clients["prometheus"].run(
                "stat -Lc '%d:%i' -- " + shlex.quote(prom_paths["data"])).strip()
            keep_data = remove_plan(api, int(prom["id"]), "keep_data")
            prom_unit = prom["service_name"] + ".service"
            prom_load_state = ssh_clients["prometheus"].run(
                "systemctl show --property=LoadState --value " + shlex.quote(prom_unit)).strip()
            if prom_load_state != "not-found":
                raise AssertionError(f"keep_data unit load state is {prom_load_state}")
            prom_unit_path = ssh_clients["prometheus"].run(
                f"if test -e {shlex.quote(prom_paths['unit'])}; then printf present; else printf absent; fi").strip()
            if prom_unit_path != "absent":
                raise AssertionError("keep_data retained the owned unit file")
            prom_active = ssh_clients["prometheus"].run(
                "systemctl is-active " + shlex.quote(prom_unit), ok=(0, 3, 4)).strip()
            if prom_active == "active":
                raise AssertionError("keep_data retained an active Prometheus service")
            if ssh_clients["prometheus"].run("test -d " + shlex.quote(prom_paths["data"]) + " && printf present").strip() != "present":
                raise AssertionError("keep_data did not retain Prometheus data")
            data_identity_after = ssh_clients["prometheus"].run(
                "stat -Lc '%d:%i' -- " + shlex.quote(prom_paths["data"])).strip()
            if data_identity_after != data_identity_before:
                raise AssertionError("keep_data replaced the retained Prometheus data directory")
            if ssh_clients["prometheus"].read(retain_marker_path) != retain_marker:
                raise AssertionError("keep_data did not retain the exact owned data marker")
            RESULT["owned"]["retained"].extend([
                {"component_id": int(exporter["id"]), "unit": exporter_unit, "reason": "keep_service"},
                {"component_id": int(prom["id"]), "path": prom_paths["data"], "reason": "keep_data"},
            ])
            evidence.update(keep_service_plan=keep_service["id"], adopted_component_id=adopted_id,
                            full_delete_plan=full_delete["id"], keep_data_plan=keep_data["id"],
                            retained_exporter_runtime=retained_runtime,
                            cleanup_config_version=cleanup_published["config"]["version"],
                            cleanup_config_sha256=hashlib.sha256(cleanup_config.encode()).hexdigest(),
                            selected_target_cleanup=target_cleanup,
                            prometheus_after_target_cleanup=cleanup_runtime,
                            retained_textfile_sha256=hashlib.sha256(retained_textfile).hexdigest(),
                            full_delete_runtime=full_delete_state,
                            keep_data_unit_load_state=prom_load_state,
                            keep_data_unit_active_state=prom_active,
                            keep_data_path_identity=data_identity_after,
                            keep_data_marker_path=retain_marker_path,
                            keep_data_marker_sha256=hashlib.sha256(retain_marker).hexdigest(),
                            retained=RESULT["owned"]["retained"])

        RESULT["status"] = "passed"
    except Exception as exc:
        RESULT["status"] = "failed"
        RESULT["error"] = redact(str(exc))
        RESULT["error_type"] = type(exc).__name__
        (OUT / "failure-traceback.log").write_text(redact(traceback.format_exc()))
        raise
    finally:
        for client in ssh_clients.values():
            try:
                client.close()
            except Exception:
                pass
        RESULT["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        RESULT["runner_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        write_json(RESULT_PATH, RESULT)


if __name__ == "__main__":
    main()
