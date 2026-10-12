"""Exercise real Vue rendering and request invalidation with isolated API fixtures."""
import functools
import os
import threading
from urllib.parse import urlparse, parse_qs
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

ROOT = Path(__file__).resolve().parents[1]
class QuietHandler(SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass

def preview(value="045d", status="available"):
    return {"component_id": 41, "preview": {
        "preview_only": True, "scope": "known_cli_parameter_rules_only",
        "source_version": "2.55.0", "target_version": "3.1.0",
        "status": status, "reason": "仅预览，尚未应用", "arguments_source": "imported_snapshot",
        "source_compatible": True, "current_compatible": False, "converted_compatible": status == "available",
        "changes": [{"from": "--storage.tsdb.retention", "to": "--storage.tsdb.retention.time",
                     "value": value, "required": True, "evidence": "https://prometheus.io/docs/prometheus/3.0/migration/"}],
        "blockers": [] if status == "available" else [{"code": "conflicting_retention", "message": "新旧参数冲突"}],
    }}

server = ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(QuietHandler, directory=str(ROOT / "api/router/web")))
threading.Thread(target=server.serve_forever, daemon=True).start()
requests, pending, errors, mutations = [], [], [], []
mode = {"hold": False, "error": False, "value": "045d"}
def api(route):
    request = route.request
    if request.method not in ("GET", "HEAD"):
        mutations.append(request.method)
    if "/parameter-migration-preview" in request.url:
        requests.append((request.method, request.url, request.post_data))
        if mode["hold"]:
            pending.append(route)
        elif mode["error"]:
            route.fulfill(status=500, json={"error": "读取组件失败"})
        else:
            response = preview(mode["value"])
            response["preview"]["target_version"] = parse_qs(urlparse(request.url).query)["target_version"][0]
            route.fulfill(json=response)
    else:
        route.fulfill(json={})

try:
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, executable_path=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE"))
        page = browser.new_page(viewport={"width": 1440, "height": 1000})
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.route("**/api/**", api)
        page.goto(f"http://127.0.0.1:{server.server_port}/")
        page.evaluate("""async () => {
            window.vm = document.querySelector('#app').__vue_app__._container._vnode.component.proxy;
            vm.role='system_admin'; vm.token='fixture-token';
            vm.components=[
              {id:41,name:'旧版本实例',type:'prometheus',deploy_type:'binary',version:'2.55.0',updated_at:'one'},
              {id:42,name:'其他实例',type:'prometheus',deploy_type:'binary',version:'2.55.0',updated_at:'one'}
            ];
            vm.openChangeDialog();
            vm.changeForm.resource_id=41; vm.changeForm.kind='upgrade'; vm.changeForm.new_version='3.1.0';
            await Vue.nextTick();
        }""")
        button = page.get_by_role("button", name="预览参数迁移", exact=True)
        expect(button).to_be_visible()
        button.click()
        expect(page.get_by_test_id("parameter-migration-preview")).to_contain_text("045d")
        assert requests[-1][0] == "GET" and requests[-1][2] is None
        assert requests[-1][1].endswith("/components/41/parameter-migration-preview?target_version=3.1.0")
        payload = page.evaluate("vm.changeAfter()")
        assert payload == {"new_version": "3.1.0", "target_image": ""}, payload

        # Source changes, target changes, component switches, role changes and closing
        # the dialog all invalidate results even before another request is sent.
        for mutation, restore in [
            ("vm.changeForm.new_version='3.0.0'", "vm.changeForm.new_version='3.1.0'"),
            ("vm.components[0].updated_at='two'", "vm.components[0].updated_at='one'"),
            ("vm.changeForm.resource_id=42", "vm.changeForm.resource_id=41"),
            ("vm.role='viewer'", "vm.role='system_admin'"),
            ("vm.changeDialog=false", "vm.changeDialog=true"),
            ("vm.changeForm.kind='config'", "vm.changeForm.kind='upgrade'"),
            ("vm.token='new-session'", "vm.token='fixture-token'"),
        ]:
            page.evaluate(f"async () => {{ {mutation}; await Vue.nextTick(); }}")
            assert page.evaluate("vm.parameterMigrationPreview===null"), mutation
            page.evaluate(f"async () => {{ {restore}; await Vue.nextTick(); }}")
            page.evaluate("vm.previewParameterMigration()")
            assert page.evaluate("vm.parameterMigrationPreview?.status") == "available"

        # An old response must never overwrite a newer target's result.
        mode["hold"] = True
        page.evaluate("void vm.previewParameterMigration()")
        page.wait_for_function("vm.parameterMigrationLoading")
        page.wait_for_timeout(100)
        assert len(pending) == 1
        page.evaluate("async () => { vm.changeForm.new_version='3.0.0'; await Vue.nextTick(); }")
        mode["hold"] = False
        mode["value"] = "NEW"
        page.evaluate("vm.previewParameterMigration()")
        pending.pop().fulfill(json=preview("STALE"))
        page.wait_for_timeout(100)
        assert page.evaluate("vm.parameterMigrationPreview.changes[0].value") == "NEW"
        assert not page.evaluate("vm.parameterMigrationLoading")

        # Errors replace prior evidence; untrusted text is escaped by interpolation.
        mode["error"] = True
        page.evaluate("vm.previewParameterMigration()")
        assert page.evaluate("vm.parameterMigrationPreview===null")
        expect(page.get_by_test_id("parameter-migration-error")).to_contain_text("读取组件失败")
        mode["error"] = False
        mode["value"] = "<img src=x onerror=window.previewXSS=1>"
        page.evaluate("vm.previewParameterMigration()")
        card = page.get_by_test_id("parameter-migration-preview")
        expect(card).to_contain_text(mode["value"])
        assert card.locator("img").count() == 0
        assert not page.evaluate("Boolean(window.previewXSS)")
        mode["value"] = "045d"
        page.evaluate("async () => { vm.changeForm.new_version='3.1.0'; await Vue.nextTick(); }")
        page.evaluate("vm.previewParameterMigration()")
        assert not mutations, mutations
        if os.environ.get("PREVIEW_SCREENSHOT"):
            page.screenshot(path=os.environ["PREVIEW_SCREENSHOT"], full_page=True)
        assert not errors, errors
        browser.close()
        print("PASS: readonly request, value display, unchanged upgrade payload, seven invalidations, stale response, errors and escaped rendering")
finally:
    server.shutdown()
    server.server_close()
