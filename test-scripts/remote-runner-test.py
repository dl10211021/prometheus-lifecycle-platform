#!/usr/bin/env python3
import os
import subprocess
import unittest
import http.server
import threading
import tempfile
import shutil
from pathlib import Path

RUNNER = Path(__file__).with_name("remote-tests.sh")
class RemoteRunnerTest(unittest.TestCase):
    def run_case(self, **inputs):
        env = {k:v for k,v in os.environ.items() if k not in {
            "BASE", "API", "REMOTE_HOST", "REMOTE_PASS", "ADMIN_PASSWORD",
            "PROMLIFECYCLE_ADMIN_PASSWORD", "REMOTE_PLATFORM_BASE"}}
        env.update(ADMIN_PASSWORD_FILE="/nonexistent/fixture-password",
                   REMOTE_SCRIPTS="missing-fixture.sh")
        env.update(inputs)
        return subprocess.run(["bash", str(RUNNER)], env=env, text=True,
                              capture_output=True, timeout=5)
    def test_absent_ssh_credentials_skip(self):
        result=self.run_case()
        self.assertEqual(result.returncode, 0)
        self.assertIn("SKIP:", result.stdout)
    def test_configured_remote_requires_explicit_platform(self):
        result=self.run_case(REMOTE_HOST="192.0.2.1", REMOTE_PASS="fixture",
                             ADMIN_PASSWORD="fixture")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("BASE", result.stderr)
    def test_configured_remote_missing_platform_auth_fails(self):
        result=self.run_case(REMOTE_HOST="192.0.2.1", REMOTE_PASS="fixture",
                             BASE="http://192.0.2.2:8080")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ADMIN_PASSWORD", result.stderr)
    def test_wrong_admin_credentials_never_dispatch_remote_script(self):
        class RejectLogin(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200); self.end_headers(); self.wfile.write(b'{}')
            def do_POST(self):
                self.send_response(401); self.end_headers(); self.wfile.write(b'{"error":"unauthorized"}')
            def log_message(self, *args):
                pass
        server=http.server.ThreadingHTTPServer(("127.0.0.1", 0), RejectLogin)
        thread=threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                root=Path(directory)
                shutil.copyfile(RUNNER, root/"remote-tests.sh")
                (root/"dispatched.sh").write_text('printf dispatched > "$DISPATCH_MARKER"\n')
                # SSH is deliberately stubbed: this test targets HTTP authentication,
                # and must never contact even a documentation-only remote address.
                (root/"sshpass").write_text("#!/bin/sh\nexit 0\n")
                (root/"sshpass").chmod(0o755)
                env=os.environ.copy()
                env["PATH"]=str(root)+os.pathsep+env["PATH"]
                env.update(BASE=f"http://127.0.0.1:{server.server_port}",
                           API=f"http://127.0.0.1:{server.server_port}/api/v1",
                           REMOTE_HOST="192.0.2.1", REMOTE_PASS="fixture",
                           ADMIN_PASSWORD="wrong", PROMLIFECYCLE_ADMIN_PASSWORD="wrong",
                           REMOTE_SCRIPTS="dispatched.sh", DISPATCH_MARKER=str(root/"marker"))
                result=subprocess.run(["bash",str(root/"remote-tests.sh")],
                                      env=env,text=True,capture_output=True,timeout=5)
                self.assertNotEqual(result.returncode,0)
                self.assertIn("authentication",result.stderr.lower())
                self.assertFalse((root/"marker").exists())
        finally:
            server.shutdown();server.server_close();thread.join()

if __name__ == "__main__":
    unittest.main()
