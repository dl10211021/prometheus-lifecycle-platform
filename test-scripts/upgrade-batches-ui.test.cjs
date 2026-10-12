const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const storage=new Map(),root=path.join(__dirname,'../api/router/web/assets');
const sandbox={window:{},localStorage:{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},crypto:require('node:crypto').webcrypto,AbortController,setTimeout,clearTimeout,ElementPlus:{ElMessage:{info(){}}}};
vm.createContext(sandbox);vm.runInContext(fs.readFileSync(path.join(root,'deployment-batches.js'),'utf8'),sandbox);
const mixin=sandbox.window.PromLifecycleDeploymentBatches;
function fixture(){
 const ctx={...mixin.data(),canChange:true,token:'fixture',sessionController:new AbortController(),batchPreflightReady:false,upgradeBatchDialog:true,upgradePreflightReady:true,upgradeSelected:[{host_id:1},{host_id:2}],confirm:async()=>true,openDeploymentBatch:async()=>{},upgradeBatchPayload:()=>({kind:'upgrade',name:'existing nodes',reason:'version update',upgrades:[{component_id:21,current_version:'1.8.2',new_version:'1.9.0'},{component_id:22,current_version:'1.8.2',new_version:'1.9.0'}]})};
 for(const [name,fn]of Object.entries(mixin.methods))ctx[name]=fn.bind(ctx);return ctx;
}
(async()=>{
 const ctx=fixture();let submitted=[];
 sandbox.axios={post:async(url,payload)=>{submitted.push(JSON.parse(JSON.stringify(payload)));return {data:{batch:{id:77}}}}};
 await ctx.submitDeploymentBatch();
 assert.equal(submitted.length,1,'upgrade wizard must submit its selected existing instances');
 assert.equal(submitted[0].kind,'upgrade');assert.equal(submitted[0].upgrades[0].component_id,21);assert.equal(submitted[0].stacks,undefined);
 assert.equal(ctx.upgradeBatchDialog,false);assert.equal(storage.size,0);
 storage.clear();const unknown=fixture();let ids=[];
 sandbox.axios.post=async(url,payload)=>{ids.push(payload.request_id);if(ids.length===1)throw new Error('lost response');throw {response:{status:409,data:{error:'another request is still checking'}}};};
 await unknown.submitDeploymentBatch();await unknown.submitDeploymentBatch();
 assert.equal(ids.length,2);assert.equal(ids[0],ids[1]);assert.equal(unknown.batchSubmissionUnknown,true,'an uncertain earlier request stays unresolved after a retry is rejected');
 assert.ok(storage.size>0,'uncertain upgrade must survive page refresh');

 vm.runInContext(fs.readFileSync(path.join(root,'upgrade-batches.js'),'utf8'),sandbox);
 storage.clear();const upgrade=sandbox.window.PromLifecycleUpgradeBatches;
 const form={...mixin.data(),...upgrade.data(),canChange:true,token:'fixture',batchDrawer:true,
  components:[{id:21,host_id:1,type:'node_exporter',version:'1.7.0',status:'running',deploy_type:'binary'},
  {id:22,host_id:1,type:'node_exporter',version:'1.8.0',status:'running',deploy_type:'binary'}],hostName:id=>'host-'+id};
 for(const [name,fn]of Object.entries({...mixin.methods,...upgrade.methods}))form[name]=fn.bind(form);
 for(const [name,fn]of Object.entries(upgrade.computed))Object.defineProperty(form,name,{get:fn.bind(form)});
 form.upgradeSelected=form.components;form.upgradeTargets={node_exporter:'1.8.2'};form.upgradeName='multiple existing instances';
 const payload=form.upgradeBatchPayload();
 assert.deepEqual(Array.from(payload.upgrades,x=>x.component_id),[21,22]);
 assert.equal(payload.upgrades[1].current_version,'1.8.0');
 sandbox.axios.post=async(url,payload)=>({data:{checks:[{component_id:payload.upgrades[0].component_id,passed:true}]}});
 await form.runUpgradePreflight();assert.equal(form.upgradePreflightReady,true);
 form.upgradeTargets.node_exporter='1.9.0';assert.equal(form.upgradePreflightReady,false,'target edits invalidate preflight');
 let opened;form.openChangePlan=async row=>{opened=row.id};await form.openBatchChange(91);
 assert.equal(opened,91);assert.equal(form.batchDrawer,false);
 let release;const gate=new Promise(resolve=>{release=resolve});
 sandbox.axios.post=async()=>{await gate;return{data:{checks:[{passed:true}]}}};
 const pending=form.runUpgradePreflight();upgrade.watch.upgradeBatchDialog.call(form,false);release();await pending;
 assert.equal(form.upgradeChecks.length,0);assert.equal(form.upgradePreflightKey,'','closed dialog must ignore late results');
 console.log('upgrade batch submission, preflight, instance selection, and plan navigation tests passed');

})().catch(e=>{console.error(e);process.exit(1)});
