"""Real Chromium contract for paired maintenance recovery."""
import functools,json,os,threading
from http.server import SimpleHTTPRequestHandler,ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect
ROOT=Path(__file__).resolve().parents[1]
OUT=Path(os.environ.get("MAINTENANCE_RECOVERY_TEST_OUT",str(ROOT/".superpowers/sdd/2026-09-17-maintenance-recovery/test-output/maintenance-recovery")))
OUT.mkdir(parents=True,exist_ok=True)
class QuietHandler(SimpleHTTPRequestHandler):
 def log_message(self,*args): pass
server=ThreadingHTTPServer(("127.0.0.1",0),functools.partial(QuietHandler,directory=str(ROOT/"api/router/web")))
threading.Thread(target=server.serve_forever,daemon=True).start()
component=dict(id=4,name="Prometheus prod",type="prometheus",deploy_type="binary",version="2.55.0",status="running",host_id=3)
def plan(pid,status="verified",point=9):
 return dict(id=pid,kind="upgrade",resource_type="component",resource_id=4,resource_name="Prometheus prod",status=status,risk="high",reason="upgrade",requested_by="admin",paired_recovery_point_id=point,diff="safe",verification={},rollback_evidence={})
plans={pid:plan(pid) for pid in [7,8,9,10,12,13]};plans[11]=plan(11,point=None)
preview=dict(point_id=9,plan_id=7,component_id=4,host_id=3,current_version="2.55.0",historical_version="2.54.1",scope="paired_restore_admission_preview_only",restore_authorized=False,requires_downtime=True,requires_current_checkpoint=True,replacement_count=5,dependencies=[],checks=["trusted_paired_history"],observed_at="2026-09-17T04:00:00Z")
def tx(pid,phase,ident):
 return dict(id=ident,plan_id=pid,point_id=9,component_id=4,host_id=3,phase=phase,revision=12,created_at="2026-09-17T04:00:00Z",updated_at="2026-09-17T04:01:00Z",finished_at="2026-09-17T04:01:00Z" if phase in ("restore_completed","restore_compensated") else None)
calls=[];held=[];initial_preview=[];paired_gets={};created_payloads=[]
def paired_response(pid):
 if pid==8:return dict(eligible=True,transaction=tx(8,"restore_completed",18))
 if pid==9:return dict(eligible=True,transaction=tx(9,"restore_compensated",19))
 if pid==10:return dict(eligible=True,transaction=tx(10,"restore_execution_reclaim",20))
 if pid==11:return dict(eligible=False)
 if pid==12 and paired_gets[pid]>1:return dict(eligible=True,transaction=tx(12,"restore_starting",22))
 value=dict(preview);value["plan_id"]=pid
 return dict(eligible=True,preview=value)
def api(route):
 req=route.request;path=urlparse(req.url).path;calls.append((req.method,path))
 if path.endswith("/paired-recovery"):
  pid=int(path.split("/")[-2])
  if req.method=="GET":
   paired_gets[pid]=paired_gets.get(pid,0)+1
   if pid==7 and paired_gets[pid]==1:initial_preview.append(route);return
   if pid==7 and initial_preview:route.fulfill(json=dict(eligible=False));return
   if pid==13:held.append(route);return
   route.fulfill(json=paired_response(pid));return
  if pid==7:route.fulfill(json=dict(transaction=tx(7,"restore_completed",17)));return
  if pid==12:route.abort("failed");return
 if path=="/api/v1/components" and req.method=="GET":
  route.fulfill(json=dict(components=[component]));return
 if path=="/api/v1/change-plans" and req.method=="POST":
  created_payloads.append(req.post_data_json);created=plan(20,status="draft");created["after"]=req.post_data_json["after"];plans[20]=created;route.fulfill(status=201,json=dict(plan=created));return
 if path.endswith("/execute") and req.method=="POST":
  pid=int(path.split("/")[-2]);plans[pid]["status"]="verified";route.fulfill(json=dict(plan=plans[pid]));return
 if "/api/v1/change-plans/" in path and req.method=="GET":
  route.fulfill(json=plans[int(path.split("/")[-1])]);return
 route.fulfill(json={})
p=None
browser=None
try:
 p=sync_playwright().start()
 browser=p.chromium.launch(headless=True,executable_path=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE"))
 page=browser.new_page(viewport={"width":1440,"height":1080});errors=[]
 page.on("pageerror",lambda error:errors.append(str(error)));page.route("**/api/**",api)
 page.goto(f"http://127.0.0.1:{server.server_port}/")
 page.evaluate("""([component,plans])=>{window.vm=document.querySelector('#app').__vue_app__._container._vnode.component.proxy;vm.token='fixture';vm.role='change_admin';vm.components=[component];vm.changePlans=plans}""",[component,list(plans.values())])
 page.evaluate("vm.openChangeDialog({resource_id:4,kind:'upgrade',reason:'paired upgrade'})")
 dialog=page.get_by_role("dialog",name="新建生命周期变更",exact=True)
 checkbox=dialog.get_by_text("升级前创建历史恢复点",exact=False)
 expect(checkbox).to_be_visible();expect(dialog.get_by_text("额外停机",exact=False)).to_be_visible()
 checkbox.click();page.evaluate("vm.changeForm.new_version='2.56.0'")
 dialog.get_by_role("button",name="检查并执行",exact=True).click()
 page.wait_for_function("vm.changeDialog===false&&vm.changeDrawer&&vm.selectedPlan?.id===20")
 assert created_payloads[-1]["after"]["paired_recovery"] is True and created_payloads[-1]["after"]["new_version"]=="2.56.0"
 page.evaluate("vm.changeDrawer=false")
 page.wait_for_function("!vm.changeDrawer")
 page.evaluate("vm.openChangePlan({id:7})")
 drawer=page.get_by_role("dialog",name="变更计划 #7",exact=True);expect(drawer).to_be_visible()
 page.wait_for_timeout(150)
 assert paired_gets.get(7)==1,paired_gets
 initial_preview.pop(0).fulfill(json=paired_response(7))
 page.wait_for_function("vm.pairedRecovery.loaded&&!vm.pairedRecoveryLoading")
 expect(drawer.get_by_text("2.55.0 → 2.54.1",exact=True)).to_be_visible()
 recovery=drawer.get_by_role("button",name="恢复升级前历史状态",exact=True)
 recovery.click()
 confirmation=page.get_by_role("dialog",name="需要明确确认",exact=True)
 expect(confirmation).to_be_visible();expect(confirmation).to_contain_text("中断")
 confirmation.get_by_role("button",name="取消",exact=True).click()
 expect(confirmation).to_be_hidden()
 assert len([c for c in calls if c==("POST","/api/v1/change-plans/7/paired-recovery")])==0
 recovery.click();expect(confirmation).to_be_visible()
 confirmation.get_by_role("textbox").fill("WRONG")
 confirmation.get_by_role("button",name="确认",exact=True).click()
 expect(confirmation.get_by_text("请输入 RESTORE_HISTORY",exact=True)).to_be_visible()
 assert len([c for c in calls if c==("POST","/api/v1/change-plans/7/paired-recovery")])==0
 confirmation.get_by_role("button",name="取消",exact=True).click()
 expect(confirmation).to_be_hidden()
 recovery.click();expect(confirmation).to_be_visible()
 confirmation.get_by_role("textbox").fill("RESTORE_HISTORY")
 confirmation.get_by_role("button",name="确认",exact=True).click()
 page.wait_for_function("vm.pairedRecovery.transaction?.phase==='restore_completed'")
 assert len([c for c in calls if c==("POST","/api/v1/change-plans/7/paired-recovery")])==1
 expect(drawer.get_by_role("alert").get_by_text("历史状态恢复成功",exact=True)).to_be_visible()
 assert drawer.get_by_role("button",name="回滚",exact=True).count()==0
 for pid,phrase in [(8,"历史状态恢复成功"),(9,"历史恢复失败；已恢复请求前状态"),(10,"恢复未终结，需要刷新或人工恢复")]:
  page.evaluate("pid=>vm.openChangePlan({id:pid})",pid)
  page.wait_for_function("pid=>vm.pairedRecovery.transaction?.plan_id===pid",arg=pid)
  expect(page.get_by_role("dialog",name=f"变更计划 #{pid}",exact=True).get_by_role("alert").get_by_text(phrase,exact=True)).to_be_visible()
 page.evaluate("vm.openChangePlan({id:11})");page.wait_for_function("vm.pairedRecovery.loaded&&!vm.pairedRecoveryLoading")
 ineligible=page.get_by_role("dialog",name="变更计划 #11",exact=True)
 expect(ineligible.get_by_role("alert").get_by_text("该计划没有可用的配对历史恢复点",exact=True)).to_be_visible()
 assert ineligible.get_by_text("恢复状态读取失败",exact=False).count()==0
 page.evaluate("vm.role='viewer';vm.openChangePlan({id:7})");page.wait_for_function("vm.pairedRecovery.loaded")
 viewer=page.get_by_role("dialog",name="变更计划 #7",exact=True)
 assert viewer.get_by_role("button",name="恢复升级前历史状态",exact=True).count()==0
 expect(viewer.get_by_role("button",name="刷新恢复状态",exact=True)).to_be_visible();page.evaluate("vm.role='change_admin'")
 page.evaluate("vm.openChangePlan({id:12})");page.wait_for_function("vm.pairedRecovery.preview?.plan_id===12")
 uncertain=page.get_by_role("dialog",name="变更计划 #12",exact=True)
 uncertain.get_by_role("button",name="恢复升级前历史状态",exact=True).click()
 expect(confirmation).to_be_visible()
 confirmation.get_by_role("textbox").fill("RESTORE_HISTORY")
 confirmation.get_by_role("button",name="确认",exact=True).click()
 page.wait_for_function("vm.pairedRecovery.transaction?.plan_id===12")
 assert len([c for c in calls if c==("POST","/api/v1/change-plans/12/paired-recovery")])==1 and paired_gets[12]==2
 page.evaluate("vm.openChangePlan({id:13})");page.wait_for_function("vm.pairedRecoveryLoading")
 stale=page.get_by_role("dialog",name="变更计划 #13",exact=True);stale.locator(".el-drawer__close-btn").click()
 page.wait_for_function("!vm.changeDrawer")
 for pending in held[:]:pending.fulfill(json=paired_response(13));held.remove(pending)
 page.wait_for_timeout(100);assert page.evaluate("vm.pairedRecovery.loaded") is False
 page.evaluate("vm.openChangePlan({id:13})");page.wait_for_function("vm.pairedRecoveryLoading")
 page.evaluate("vm.openChangePlan({id:8})");page.wait_for_function("vm.pairedRecovery.transaction?.plan_id===8")
 for pending in held[:]:pending.fulfill(json=paired_response(13));held.remove(pending)
 page.wait_for_timeout(100);assert page.evaluate("vm.pairedRecovery.transaction.plan_id")==8
 page.screenshot(path=str(OUT/"paired-recovery-outcomes.png"),full_page=True,animations="disabled")
 page.evaluate("vm.openChangePlan({id:13})");page.wait_for_function("vm.pairedRecoveryLoading")
 page.evaluate("setTimeout(()=>vm.logout(false),0)");page.wait_for_load_state("domcontentloaded")
 for pending in held[:]:
  try:pending.fulfill(json=paired_response(13))
  except Exception:pass
  held.remove(pending)
 page.wait_for_timeout(100)
 assert page.evaluate("localStorage.getItem('token')") is None
 assert page.evaluate("document.querySelector('#app').__vue_app__._container._vnode.component.proxy.pairedRecovery.loaded") is False
 assert not errors,errors
 print("PASS: paired upgrade opt-in, no-point ineligible state, explicit preview-confirm-single POST, durable outcomes, GET-only uncertainty refresh, stale close/switch/logout and viewer permissions")
finally:
 for pending in held+initial_preview:
  try:pending.abort()
  except Exception:pass
 if browser is not None:browser.close()
 if p is not None:p.stop()
 server.shutdown();server.server_close()
