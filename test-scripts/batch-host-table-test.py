"""Browser contract test; uses real Vue UI and a controlled HTTP API boundary.
Run: python3 test-scripts/batch-host-table-test.py
Requires Python Playwright and Chromium; never contacts production hosts.
"""
import functools
import json
import os
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading

from playwright.sync_api import sync_playwright, expect

ROOT = Path(__file__).resolve().parents[1]


class QuietHandler(SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


server = ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(
    QuietHandler, directory=str(ROOT / "api/router/web")))
threading.Thread(target=server.serve_forever, daemon=True).start()
requests = []
deferred = []


def api(route):
    if route.request.method == "POST" and route.request.url.endswith("/hosts/batch"):
        payload = route.request.post_data_json
        requests.append(payload)
        if len(requests) == 4:
            deferred.append(route)
            return
        if len(requests) == 1:
            results = [dict(index=0, status="created", id=11),
                       dict(index=1, status="failed", error="Connection refused")]
        elif len(requests) == 2:
            results = [dict(index=0, status="created", id=12)]
        else:
            results = []  # Missing results must never become automatically retryable.
        route.fulfill(json={"results": results, "total": len(payload)})
    else:
        route.fulfill(json={"hosts": []})


try:
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, executable_path=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE"))
        page = browser.new_page(viewport={"width": 1440, "height": 1000})
        errors = []
        page.on("pageerror", lambda e: errors.append(str(e)))
        page.route("**/api/**", api)
        page.goto(os.environ.get("PROMLIFECYCLE_UI_TEST_URL") or f"http://127.0.0.1:{server.server_port}/")
        page.evaluate("""() => {
          window.vm = document.querySelector('#app').__vue_app__._container._vnode.component.proxy;
          vm.token = 'fixture-token'; vm.role = 'system_admin'; vm.openBatchHostDialog();
        }""")
        dialog = page.get_by_role("dialog", name="批量添加远程主机", exact=True)
        expect(dialog.get_by_label("第 1 行 IP", exact=True)).to_be_visible(timeout=3000)
        dialog.get_by_label("第 1 行 IP", exact=True).fill("192.0.2.10")
        dialog.get_by_label("第 1 行 用户名", exact=True).fill("root")
        secret = ' p,a ss\\word! $ " '
        dialog.get_by_label("第 1 行 密码", exact=True).fill(secret)
        expect(dialog.get_by_label("第 1 行 密码", exact=True)).to_have_attribute("type", "password")
        dialog.get_by_role("button", name="添加一行", exact=True).click()
        dialog.get_by_label("第 2 行 IP", exact=True).fill("999.0.2.11")
        dialog.get_by_label("第 2 行 用户名", exact=True).fill("deploy")
        dialog.get_by_label("第 2 行 密码", exact=True).fill("fixture-second")
        dialog.get_by_role("button", name="提交待添加行", exact=True).click()
        expect(page.get_by_text("第 2 行：IP 地址格式无效", exact=False)).to_be_visible()
        assert requests == []
        dialog.get_by_label("第 2 行 IP", exact=True).fill("192.0.2.11")

        def submit(button):
            dialog.get_by_role("button", name=button, exact=True).click()
            prompt = page.get_by_role("dialog", name="需要明确确认")
            prompt.get_by_role("textbox").fill("ADD_HOSTS")
            prompt.get_by_role("button", name="确认", exact=True).click()

        submit("提交待添加行")
        expect(dialog.get_by_text("Connection refused", exact=True)).to_be_visible()
        assert len(requests) == 1 and requests[0][0]["password"] == secret
        expect(dialog.get_by_label("第 1 行 密码", exact=True)).to_have_value("")
        expect(dialog.get_by_label("第 1 行 IP", exact=True)).to_be_disabled()
        dialog.get_by_label("第 2 行 密码", exact=True).fill("fixture-corrected")
        submit("仅重试失败行")
        expect(dialog.get_by_role("button", name="仅重试失败行", exact=True)).to_be_disabled()
        assert len(requests) == 2 and len(requests[1]) == 1
        assert requests[1][0]["ip"] == "192.0.2.11"
        assert requests[1][0]["password"] == "fixture-corrected"

        dialog.get_by_role("button", name="粘贴 Excel / CSV", exact=True).click()
        dialog.get_by_label("批量粘贴内容", exact=True).fill('192.0.2.12\troot\t" p,a ss! "\t2222\tnode-c')
        dialog.get_by_role("button", name="填入表格", exact=True).click()
        expect(dialog.get_by_label("第 3 行 IP", exact=True)).to_have_value("192.0.2.12")
        expect(dialog.get_by_label("第 3 行 密码", exact=True)).to_have_value(" p,a ss! ")
        submit("提交待添加行")
        expect(dialog.get_by_text("结果未知", exact=True)).to_be_visible()
        expect(dialog.get_by_role("button", name="仅重试失败行", exact=True)).to_be_disabled()
        expect(dialog.get_by_label("第 3 行 密码", exact=True)).to_have_value("")
        dialog.get_by_role("button", name="添加一行", exact=True).click()
        dialog.get_by_label("第 4 行 密码", exact=True).fill("discard-me")
        dialog.get_by_role("button", name="删除第 4 行", exact=True).click()
        expect(dialog.get_by_label("第 4 行 密码", exact=True)).to_have_count(0)
        dialog.get_by_role("button", name="添加一行", exact=True).click()
        dialog.get_by_label("第 4 行 密码", exact=True).fill("discard-on-close")
        page.screenshot(path="/tmp/promlifecycle-batch-host-table.png", full_page=True)
        dialog.get_by_role("button", name="关闭并清除凭据", exact=True).click()
        expect(dialog).not_to_be_visible()
        page.evaluate("vm.openBatchHostDialog()")
        expect(dialog.get_by_label("第 1 行 密码", exact=True)).to_have_value("")
        assert "discard-on-close" not in page.evaluate("JSON.stringify(vm.$data)")
        # A request already sent can finish after closing/reopening, but must
        # never restore cleared passwords, including echoed server errors.
        dialog.get_by_label("第 1 行 IP", exact=True).fill("192.0.2.20")
        dialog.get_by_label("第 1 行 用户名", exact=True).fill("root")
        dialog.get_by_label("第 1 行 密码", exact=True).fill("background-fixture-secret")
        submit("提交待添加行")
        expect(dialog.get_by_label("第 1 行 IP", exact=True)).to_be_disabled()
        dialog.get_by_role("button", name="后台继续，关闭窗口", exact=True).click()
        expect(dialog).not_to_be_visible()
        page.evaluate("vm.openBatchHostDialog()")
        expect(dialog.get_by_label("第 1 行 密码", exact=True)).to_have_value("")
        assert len(deferred) == 1
        deferred.pop().fulfill(json={"results": [dict(index=0, status="failed", error="bad background-fixture-secret")]})
        expect(dialog.get_by_text("bad [已隐藏凭据]", exact=True)).to_be_visible()
        expect(dialog.get_by_label("第 1 行 密码", exact=True)).to_have_value("")
        assert "background-fixture-secret" not in page.evaluate("JSON.stringify(vm.$data)")
        dialog.get_by_role("button", name="关闭并清除凭据", exact=True).click()
        expect(dialog).not_to_be_visible()
        page.evaluate("vm.openBatchHostDialog()")
        # Cancel confirmation: rows stay editable and no API request is sent.
        dialog.get_by_label("第 1 行 IP", exact=True).fill("192.0.2.30")
        dialog.get_by_label("第 1 行 用户名", exact=True).fill("root")
        dialog.get_by_label("第 1 行 密码", exact=True).fill("cancel-fixture")
        dialog.get_by_role("button", name="提交待添加行", exact=True).click()
        page.get_by_role("dialog", name="需要明确确认").get_by_role("button", name="取消", exact=True).click()
        expect(dialog.get_by_label("第 1 行 IP", exact=True)).to_be_enabled()
        assert len(requests) == 4
        assert not errors, errors
        browser.close()
        print("PASS: editable rows, validation, exact passwords, per-row results, failed-only retry, paste import, unknown-result safety, removal and credential cleanup")
finally:
    server.shutdown()
    server.server_close()
