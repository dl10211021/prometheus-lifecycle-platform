"""Real Vue payload/invalidation contract with isolated API fixtures."""
import functools
import os
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading
from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parents[1]
class QuietHandler(SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass

server = ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=str(ROOT / 'api/router/web')))
threading.Thread(target=server.serve_forever, daemon=True).start()
submitted = []
def api(route):
    if route.request.method == 'POST' and route.request.url.endswith('/deployments/stacks'):
        submitted.append(route.request.post_data_json)
        route.fulfill(json={'stack': {'id': 1, 'status': 'pending', 'steps': []}})
    else:
        route.fulfill(json={})
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, executable_path=os.environ.get('PLAYWRIGHT_CHROMIUM_EXECUTABLE'))
        page = browser.new_page()
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        page.route('**/api/**', api)
        page.goto(f'http://127.0.0.1:{server.server_port}/')
        page.evaluate('''async () => {
          window.vm = document.querySelector('#app').__vue_app__._container._vnode.component.proxy;
          vm.role = 'system_admin'; vm.token = 'fixture-token';
          vm.stackTemplates = [{key:'fixture',name:'fixture',component_order:['prometheus','thanos']}];
          vm.deployForm = {templateKey:'fixture', host_id:7, selected_types:['prometheus','thanos']};
          vm.deploymentDefinitions = ['prometheus','thanos'].map(type=>({type,platform:'linux',deploy_type:'binary',parameters:[{field:'version',required:true}]}));
          vm.componentVersions = [{type:'prometheus',version:'2.55.0',package_available:true},{type:'prometheus',version:'3.0.0',package_available:true},{type:'thanos',version:'0.37.0',package_available:true}];
          await Vue.nextTick();
          vm.deploySettings = {prometheus:{deployment_name:'prom',version:'2.55.0'},thanos:{deployment_name:'thanos',version:'0.37.0'}};
          await Vue.nextTick();
        }''')
        payload = page.evaluate("vm.componentDeploymentPayload('thanos',vm.deploySettings.thanos)")
        assert payload.get('planned_versions') == {'prometheus':'2.55.0','thanos':'0.37.0'}, payload
        page.evaluate('''async () => {
          vm.deploymentPreflightReports = [{passed:true},{passed:true}];
          vm.deploySettings.prometheus.version = '3.0.0';
          await Vue.nextTick();
        }''')
        assert page.evaluate('vm.deploymentPreflightReports.length') == 0, 'version change kept stale preflight'
        updated = page.evaluate("vm.componentDeploymentPayload('thanos',vm.deploySettings.thanos)")
        assert updated['planned_versions']['prometheus'] == '3.0.0'
        assert payload['planned_versions']['prometheus'] == '2.55.0'
        page.evaluate('''() => {
          vm.deploymentPreflightReports = [{passed:true},{passed:true}];
          vm.confirm = async () => true;
          vm.openStackRun = async () => {};
        }''')
        assert page.evaluate('vm.deployConfigurationReady && vm.deploymentPreflightPassed')
        page.evaluate('vm.deploySelectedComponents()')
        assert len(submitted) == 1, submitted
        for component in submitted[0]['components']:
            assert not {'planned_versions','planned_components','preflight_token'} & component.keys(), component
        assert submitted[0]['components'][0]['version'] == '3.0.0'
        assert not errors, errors
        browser.close()
        print('PASS: planned version payload, version-change invalidation, queue payload cleanup; no page errors')
finally:
    server.shutdown()
    server.server_close()
