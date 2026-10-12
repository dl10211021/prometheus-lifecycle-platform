"""Real Vue/Chromium deployment onboarding behavior at controlled HTTP boundaries."""
import functools
import json
import os
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import sync_playwright, expect

ROOT = Path(__file__).resolve().parents[1]
OUT = Path(os.environ.get('TEST_OUTPUT_DIR', '/root/promlifecycle-dist/deployment-onboarding-20260917'))
OUT.mkdir(parents=True, exist_ok=True)
class QuietHandler(SimpleHTTPRequestHandler):
    def log_message(self, *args): pass
server = ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=str(ROOT/'api/router/web')))
threading.Thread(target=server.serve_forever, daemon=True).start()
calls, held = [], []
hosts = [dict(id=i, name='node-'+str(i), ip='192.0.2.'+str(i), os='linux', connection_type='ssh') for i in range(11,20)]
hosts.append(dict(id=20, name='windows-node', ip='192.0.2.20', os='windows', connection_type='winrm'))
components = [
    dict(id=201, type='prometheus', host_id=11, name='Prometheus A', port=9090, status='running', cluster_id=1),
    dict(id=202, type='prometheus', host_id=12, name='Prometheus B', port=9090, status='running', cluster_id=1),
    dict(id=101, type='node_exporter', host_id=11, name='existing-node', port=9100, status='running', cluster_id=1),
    dict(id=171, type='node_exporter', host_id=17, name='node-first', port=9100, status='running', cluster_id=1),
    dict(id=172, type='node_exporter', host_id=17, name='node-second', port=19100, status='running', cluster_id=1)]
stacks = {
    901: dict(id=901, name='bound stack', host_id=11, status='success', total=2, current_index=2, steps=[dict(type='prometheus', component_id=201, status='success'),dict(type='node_exporter',component_id=101,status='success')]),
    902: dict(id=902, name='exporter stack', host_id=11, status='success', total=1, current_index=1, steps=[dict(type='node_exporter',component_id=101,status='success')])}
attempts = {}
hold_acceptance = False
def report(host=11, prom=202, component=101, stack=None):
    return dict(passed=False, message='采集需要处理', checked_at='2026-09-17T00:00:00Z',
        scope=dict(kind='stack' if stack else 'host', host_id=host, prometheus_id=prom, stack_id=stack),
        checks=[dict(key='prometheus_ready',status='pass',reason='实例可用'),dict(key='targets',status='fail',reason='所选目标未被采集')],
        targets=[dict(component_id=component, host_id=host, type='node_exporter',address='http://192.0.2.%s:9100/metrics'%host,status='missing',reason='目标缺失',last_scrape=None)])
def api(route):
    global hold_acceptance
    req=route.request; path=urlparse(req.url).path; query=parse_qs(urlparse(req.url).query)
    body=req.post_data_json if req.method=='POST' else None
    calls.append((req.method,path,query,body))
    if path.endswith('/hosts/batch'):
        route.fulfill(json={'results':[dict(index=0,status='created',id=11),dict(index=1,status='exists',id=12),dict(index=2,status='failed',id=13,error='no connection'),dict(index=3,status='created',id=-1)]})
    elif path.endswith('/hosts'): route.fulfill(json={'hosts':hosts})
    elif path.endswith('/components') and req.method=='GET': route.fulfill(json={'components':components})
    elif path.endswith('/components') and req.method=='POST':
        route.fulfill(json={'component':dict(id=101,status='running'), 'operation':dict(id=55,status='success')})
    elif path.endswith('/detect'):
        hid=int(path.split('/')[-2])
        route.fulfill(json={'components':[dict(type='node_exporter',port=9100,ownership='',fingerprint='discovered')] if hid==18 else []})
    elif path.endswith('/versions/by-type'): route.fulfill(json={'versions':[dict(version='1.9.1',package_available=True),dict(version='1.10.0',package_available=False)]})
    elif path.endswith('/components/preflight'): route.fulfill(json={'passed':True,'checks':[],'preflight_token':'fixture'})
    elif path.endswith('/deployments/stacks') and req.method=='POST':
        hid=body['host_id']; attempts[hid]=attempts.get(hid,0)+1
        if hid==15 and attempts[hid]==1: route.fulfill(status=409,json={'error':'明确拒绝'});return
        if hid==16: route.abort('failed');return
        sid=500+hid
        stacks[sid]=dict(id=sid,name='Node Exporter '+str(hid),status='failed' if hid==14 else 'pending',host_id=hid,total=1,current_index=0,steps=[])
        route.fulfill(json={'stack':stacks[sid]})
    elif path.endswith('/retry'):
        sid=int(path.split('/')[-2]);stacks[sid]['status']='pending';route.fulfill(json={'stack':stacks[sid]})
    elif path.endswith('/acceptance') or path.endswith('/monitoring-status'):
        if hold_acceptance:
            held.append(route);return
        hid=int(path.split('/')[-2]) if path.endswith('/monitoring-status') else 11
        sid=int(path.split('/')[-2]) if path.endswith('/acceptance') else None
        route.fulfill(json=report(host=hid,prom=int(query.get('prometheus_id',[201 if sid==901 else 202])[0]),component=int(query.get('component_id',[101])[0]),stack=sid))
    elif '/deployments/stacks/' in path: route.fulfill(json={'stack':stacks[int(path.split('/')[-1])]})
    elif path.endswith('/deployments/stacks'): route.fulfill(json={'stacks':list(stacks.values())})
    else: route.fulfill(json={})

try:
    with sync_playwright() as p:
        browser=p.chromium.launch(headless=True, executable_path=os.environ.get('PLAYWRIGHT_CHROMIUM_EXECUTABLE'))
        page=browser.new_page(viewport={'width':1440,'height':1080})
        errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.route('**/api/**',api)
        page.goto(f'http://127.0.0.1:{server.server_port}/')
        page.evaluate("""() => {
          window.vm=document.querySelector('#app').__vue_app__._container._vnode.component.proxy;
          vm.token='fixture';vm.role='system_admin';vm.confirm=async()=>true;
          vm.domain='environment';vm.environmentTab='hosts';vm.openBatchHostDialog();
          vm.batchHostRows=[];for(let i=11;i<=15;i++) vm.addBatchHostRow({ip:'192.0.2.'+i,username:'root',password:'secret-'+i,port:22,name:'node-'+i,os:'linux',connection_type:'ssh'});
        }""")
        batch=page.get_by_role('dialog',name='批量添加远程主机',exact=True)
        batch.get_by_role('button',name='提交待添加行',exact=True).click()
        page.wait_for_function("!vm.batchHostLoading")
        assert page.evaluate("vm.batchHostRows.map(r=>r.host_id||null)") == [11,12,None,None,None], 'created/exists valid IDs were discarded or unsafe rows accepted'
        batch.get_by_role('button',name='接续监控接入',exact=True).click()
        dialog=page.get_by_role('dialog',name='Linux 主机监控接入',exact=True)
        expect(dialog).to_be_visible()
        page.wait_for_function("!vm.monitoringLoading")
        assert page.evaluate("vm.monitoringRows.map(r=>r.host_id)") == [11,12]
        assert 'secret-' not in page.evaluate("JSON.stringify(vm.$data)")
        dialog.get_by_label('验收 Prometheus',exact=True).select_option('202')
        expect(dialog.get_by_text('复用 #101',exact=False)).to_be_visible()
        dialog.get_by_role('combobox',name='Node Exporter 版本',exact=True).select_option('1.9.1')
        assert dialog.get_by_role('combobox',name='Node Exporter 版本',exact=True).locator('option[value="1.10.0"]').count()==0
        page.screenshot(path=str(OUT/'monitoring-form.png'),full_page=True,animations='disabled')
        dialog.get_by_role('button',name='预检并部署缺失项',exact=True).click()
        page.wait_for_function("!vm.monitoringSubmitting")
        creates=[x[3] for x in calls if x[0]=='POST' and x[1].endswith('/deployments/stacks')]
        assert [x['host_id'] for x in creates]==[12], creates
        payload=creates[0]['components'][0]
        assert payload['type']=='node_exporter' and payload['replace_existing'] is False
        assert not {'preflight_token','planned_versions','planned_components'} & payload.keys()
        assert page.evaluate("vm.monitoringRows[1].stack_id")==512
        stacks[512].update(status='success',steps=[dict(type='node_exporter',component_id=112,status='success')])
        dialog.get_by_role('button',name='刷新任务状态',exact=True).click()
        page.wait_for_function("vm.monitoringRows[1].status==='success'")
        dialog.get_by_role('button',name='验收所选实例',exact=True).click()
        page.wait_for_function("!vm.monitoringAccepting")
        status_calls=[x for x in calls if x[1].endswith('/monitoring-status')]
        assert [(x[2]['prometheus_id'],x[2]['component_id']) for x in status_calls]==[(['202'],['101']),(['202'],['112'])],status_calls
        expect(dialog.get_by_text('目标缺失',exact=True).first).to_be_visible()
        assert page.evaluate("vm.monitoringRows.every(r=>r.report && r.report.passed===false)")
        page.screenshot(path=str(OUT/'monitoring-acceptance.png'),full_page=True,animations='disabled')
        dialog.get_by_label('验收 Prometheus',exact=True).select_option('201')
        assert page.evaluate("vm.monitoringRows.every(r=>r.report===null)")
        dialog.get_by_role('button',name='关闭接入窗口',exact=True).click()
        # Existing-host selection uses the same UI, including explicit multi-exporter selection.
        page.evaluate("vm.openMonitoringOnboarding([17,18,20])")
        page.wait_for_function("!vm.monitoringLoading")
        assert page.evaluate("vm.monitoringRows.map(r=>r.status)") == ['choose','discovered','unsupported']
        dialog.get_by_label('主机 17 Exporter',exact=True).select_option('172')
        assert page.evaluate("vm.monitoringRows[0].component_id")==172
        assert dialog.get_by_role('button',name='发现并接管',exact=True).count()==1
        dialog.get_by_role('button',name='关闭接入窗口',exact=True).click()
        # Explicit failure retries and persisted task IDs; transport-unknown is never replayed.
        page.evaluate("vm.openMonitoringOnboarding([14,15,16])")
        page.wait_for_function("!vm.monitoringLoading")
        dialog.get_by_label('验收 Prometheus',exact=True).select_option('202')
        dialog.get_by_role('combobox',name='Node Exporter 版本',exact=True).select_option('1.9.1')
        page.evaluate("vm.monitoringForm.custom_env='{\"API_TOKEN\":\"temporary-secret\"}'")
        dialog.get_by_role('button',name='预检并部署缺失项',exact=True).click()
        page.wait_for_function("!vm.monitoringSubmitting")
        assert page.evaluate("vm.monitoringRows.map(r=>r.status)") == ['failed','failed','unknown']
        assert page.evaluate("vm.monitoringRows.map(r=>r.stack_id)") == [514,None,None]
        dialog.get_by_role('button',name='仅重试明确失败项',exact=True).click()
        page.wait_for_function("!vm.monitoringSubmitting")
        assert attempts[14]==1 and attempts[15]==2 and attempts[16]==1,attempts
        assert any(x[1].endswith('/514/retry') for x in calls)
        dialog.get_by_role('button',name='关闭接入窗口',exact=True).click()
        expect(dialog).not_to_be_visible()
        assert 'temporary-secret' not in page.evaluate("JSON.stringify(vm.$data)")
        assert page.evaluate("vm.monitoringRows[0].stack_id")==514
        # Stack-bound Prometheus cannot be overridden; global wizard endpoint is untouched.
        page.evaluate("vm.openStackRun({id:901})")
        page.wait_for_function("vm.stackDrawer && vm.selectedStackRun.id===901")
        drawer=page.get_by_role('dialog',name='整栈任务 #901',exact=True)
        expect(drawer.get_by_label('任务验收 Prometheus',exact=True)).to_be_disabled()
        drawer.get_by_role('button',name='执行实时验收',exact=True).click()
        page.wait_for_function("vm.stackAcceptanceResult!==null")
        assert page.evaluate("vm.stackAcceptanceResult.scope.prometheus_id")==201
        assert not any(x[0]=='POST' and x[1].endswith('/onboarding/acceptance') for x in calls)
        # Superseded acceptance responses cannot appear under a different stack.
        hold_acceptance=True
        drawer.get_by_role('button',name='执行实时验收',exact=True).click()
        page.wait_for_function("vm.stackAcceptanceLoading")
        page.evaluate("vm.openStackRun({id:902})")
        page.wait_for_function("vm.selectedStackRun.id===902")
        assert page.evaluate("vm.stackAcceptanceResult") is None
        for route in held:
            try: route.fulfill(json=report(stack=901,prom=201))
            except Exception: pass
        held.clear();hold_acceptance=False
        page.wait_for_timeout(100)
        assert page.evaluate("vm.stackAcceptanceResult") is None
        # Real stack JSON has host_id and bound component IDs, never cluster_id.
        drawer=page.get_by_role('dialog',name='整栈任务 #902',exact=True)
        assert page.evaluate("vm.stackAcceptancePrometheusID") is None, 'multiple same-cluster candidates must remain explicit'
        drawer.get_by_label('任务验收 Prometheus',exact=True).select_option('201')
        assert page.evaluate("vm.stackAcceptancePrometheusID")==201
        # A different-cluster candidate must not prevent the unique matching choice.
        page.evaluate("vm.components=vm.components.map(c=>c.id===201?{...c,cluster_id:2}:c)")
        stacks[903]=dict(stacks[902],id=903,name='unique candidate')
        page.evaluate("vm.openStackRun({id:903})")
        page.wait_for_function("vm.selectedStackRun.id===903")
        drawer=page.get_by_role('dialog',name='整栈任务 #903',exact=True)
        expect(drawer.get_by_label('任务验收 Prometheus',exact=True)).to_have_value('202')
        drawer.get_by_role('button',name='执行实时验收',exact=True).click()
        page.wait_for_function("vm.stackAcceptanceResult!==null")
        scoped=[x for x in calls if x[1].endswith('/903/acceptance')]
        assert len(scoped)==1 and scoped[0][2]['prometheus_id']==['202'],scoped
        expect(drawer.get_by_text('目标缺失',exact=True)).to_be_visible()
        page.screenshot(path=str(OUT/'unique-prometheus-candidate.png'),full_page=True,animations='disabled')
        # Missing or inconsistent binding information must not guess a scope.
        stacks[904]=dict(stacks[902],id=904,name='missing binding',steps=[dict(type='node_exporter',component_id=999,status='success')])
        page.evaluate("vm.openStackRun({id:904})")
        page.wait_for_function("vm.selectedStackRun.id===904")
        assert page.evaluate("vm.stackAcceptancePrometheusID") is None
        stacks[905]=dict(stacks[902],id=905,name='inconsistent cluster',steps=[dict(type='node_exporter',component_id=101,status='success'),dict(type='node_exporter',component_id=102,status='success')])
        page.evaluate("vm.components.push({id:102,host_id:11,type:'node_exporter',status:'running',cluster_id:2})")
        page.evaluate("vm.openStackRun({id:905})")
        page.wait_for_function("vm.selectedStackRun.id===905")
        assert page.evaluate("vm.stackAcceptancePrometheusID") is None
        # Restore the component inventory for the remaining existing-host scenarios.
        page.evaluate("vm.components=vm.components.filter(c=>c.id!==102).map(c=>c.id===201?{...c,cluster_id:1}:c)")
        # Existing-host table selection enters the same dialog; reuse needs no package/version.
        page.evaluate("vm.stackDrawer=false;vm.domain='environment';vm.environmentTab='hosts'")
        before_versions=len([x for x in calls if x[1].endswith('/versions/by-type')])
        page.get_by_role('row').filter(has_text='node-11').locator('label.el-checkbox').click()
        page.get_by_role('button',name='接入所选主机监控',exact=True).click()
        page.wait_for_function("!vm.monitoringLoading")
        assert page.evaluate("vm.monitoringRows.map(r=>r.host_id)")==[11]
        assert len([x for x in calls if x[1].endswith('/versions/by-type')])==before_versions
        expect(dialog.get_by_role('combobox',name='Node Exporter 版本',exact=True)).to_have_count(0)
        dialog.get_by_label('验收 Prometheus',exact=True).select_option('202')
        dialog.get_by_role('button',name='验收所选实例',exact=True).click()
        page.wait_for_function("!vm.monitoringAccepting")
        assert page.evaluate("vm.monitoringRows[0].report.scope.prometheus_id")==202
        # Closing a dialog clears secrets and invalidates in-flight reports without replaying work.
        hold_acceptance=True
        dialog.get_by_role('button',name='验收所选实例',exact=True).click()
        page.wait_for_function("vm.monitoringAccepting")
        page.evaluate("vm.monitoringForm.custom_env='temporary-close-secret'")
        dialog.get_by_role('button',name='关闭接入窗口',exact=True).click()
        for route in held:
            try: route.fulfill(json=report(prom=202))
            except Exception: pass
        held.clear();hold_acceptance=False
        page.wait_for_timeout(100)
        assert page.evaluate("vm.monitoringRows[0].report") is None
        assert 'temporary-close-secret' not in page.evaluate("JSON.stringify(vm.$data)")
        # Legacy single host flow must use the exact created Exporter ID and show scoped reports.
        page.evaluate("""async () => {
          vm.pollDelay=async()=>{};
          vm.waitForComponentDeployment=async()=>({component:{id:101,status:'running'},operation:{status:'success'}});
          vm.loadComponents=vm.loadConfigCenter=vm.loadAlertRules=async()=>{};
          await vm.autoOnboardHost({id:11},{connection_type:'ssh',exporter_version:'1.9.1',exporter_port:9100},new AbortController().signal,{},()=>true);
        }""")
        legacy=[x for x in calls if x[1].endswith('/hosts/11/monitoring-status')][-1]
        assert legacy[2].get('component_id')==['101'],legacy
        assert page.evaluate("vm.monitoringOnboardingResult.scope.host_id")==11
        # Session cancellation clears new form secrets and prevents late acceptance publication.
        page.evaluate("vm.openMonitoringOnboarding([11])")
        page.wait_for_function("!vm.monitoringLoading")
        dialog.get_by_label('验收 Prometheus',exact=True).select_option('202')
        hold_acceptance=True
        dialog.get_by_role('button',name='验收所选实例',exact=True).click()
        page.wait_for_function("vm.monitoringAccepting")
        page.evaluate("vm.monitoringForm.custom_args='session-secret';vm.sessionController.abort()")
        for route in held:
            try: route.fulfill(json=report(prom=202))
            except Exception: pass
        held.clear();hold_acceptance=False
        assert page.evaluate("vm.monitoringRows.length")==0
        assert 'session-secret' not in page.evaluate("JSON.stringify(vm.$data)")
        assert not errors,errors
        browser.close()
        print('PASS: batch valid IDs, explicit Prometheus/Exporter, missing-only persisted deployment, failed-only retry, unknown non-replay, discovery/Windows, scoped failed reports, stale-response rejection, credential cleanup and legacy contract')
finally:
    server.shutdown();server.server_close()
