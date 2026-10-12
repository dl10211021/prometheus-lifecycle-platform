const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const storage=new Map(),root=path.join(__dirname,'../api/router/web/assets');
const sandbox={window:{},localStorage:{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},crypto:require('node:crypto').webcrypto,AbortController,setTimeout,clearTimeout,ElementPlus:{ElMessage:{info(){}}}};
vm.createContext(sandbox);vm.runInContext(fs.readFileSync(path.join(root,'deployment-batches.js'),'utf8'),sandbox);
const common=sandbox.window.PromLifecycleDeploymentBatches;
function fixture(action='stop'){
 storage.clear();
 const ctx={...common.data(),canChange:true,token:'fixture',sessionController:new AbortController(),powerBatchDialog:true,powerAction:action,powerInputReady:true,powerSelected:[{id:11,host_id:1},{id:12,host_id:2}],confirm:async()=>true,openDeploymentBatch:async()=>{},powerBatchPayload:()=>({kind:action,name:'managed service control',reason:'maintenance',component_ids:[11,12]})};
 for(const [name,fn]of Object.entries(common.methods))ctx[name]=fn.bind(ctx);return ctx;
}
(async()=>{
 for(const action of ['start','stop']){
  const ctx=fixture(action);let submitted=[],confirmation;
  ctx.confirm=async(message,code)=>{confirmation=code;return true;};
  sandbox.axios={post:async(url,payload)=>{submitted.push(JSON.parse(JSON.stringify(payload)));return{data:{batch:{id:97}}};}};
  await ctx.submitDeploymentBatch();
  assert.equal(submitted.length,1,'power wizard must submit selected existing instances');
  assert.equal(submitted[0].kind,action);assert.deepEqual(submitted[0].component_ids,[11,12]);assert.equal(submitted[0].stacks,undefined);
  assert.equal(confirmation,action==='start'?'START_BATCH':'STOP_BATCH');
  assert.equal(ctx.powerBatchDialog,false);assert.equal(storage.size,0);
 }
 const uncertain=fixture();let submitted=[];
 sandbox.axios.post=async(url,payload)=>{submitted.push(JSON.parse(JSON.stringify(payload)));throw new Error('lost response');};
 await uncertain.submitDeploymentBatch();await uncertain.submitDeploymentBatch();
 assert.equal(submitted.length,2);assert.equal(submitted[0].request_id,submitted[1].request_id);
 assert.equal(uncertain.batchSubmissionUnknown,true);
 const marker=JSON.parse([...storage.values()][0]);assert.equal(marker.host_count,2);assert.equal(marker.component_ids,undefined);
 sandbox.axios.get=async()=>({data:{batches:[{id:98,request_id:submitted[0].request_id}]}});
 await uncertain.reconcileBatchSubmission();assert.equal(uncertain.powerBatchDialog,false);assert.equal(storage.size,0);
 const closed=fixture();closed.confirm=async()=>{closed.powerBatchDialog=false;closed.clearDeploymentBatchDraft();return true;};let posts=0;
 sandbox.axios.post=async()=>{posts++;return{data:{batch:{id:99}}};};
 await closed.submitDeploymentBatch();assert.equal(posts,0,'closed draft must not submit after confirmation');

 if(fs.existsSync(path.join(root,'power-batches.js')))vm.runInContext(fs.readFileSync(path.join(root,'power-batches.js'),'utf8'),sandbox);
 const mixin=sandbox.window.PromLifecyclePowerBatches||{data:()=>({}),methods:{},computed:{}};
 const form={...common.data(),...mixin.data(),canChange:true,token:'fixture',hostName:id=>'host-'+id,components:[
  {id:21,host_id:1,type:'node_exporter',port:9100,name:'one',status:'running',deploy_type:'binary'},
  {id:22,host_id:1,type:'node_exporter',port:19100,name:'two',status:'stopped',deploy_type:'binary'},
  {id:23,host_id:2,type:'node_exporter',port:9100,name:'observe',status:'running',deploy_type:'target'},
  {id:24,host_id:2,type:'node_exporter',port:19100,name:'busy',status:'upgrading',deploy_type:'binary'}]};
 for(const [name,fn]of Object.entries({...common.methods,...mixin.methods}))form[name]=fn.bind(form);
 for(const [name,fn]of Object.entries(mixin.computed))Object.defineProperty(form,name,{get:fn.bind(form)});
 form.powerSelected=form.components.slice(0,2);form.powerName='planned maintenance';form.powerReason='maintenance';form.powerAction='stop';
 assert.equal(form.powerInputReady,true,'valid managed selection must be ready');
 assert.deepEqual(Array.from(form.powerBatchPayload().component_ids),[21,22],'same-type instances retain their identities');
 assert.equal(form.powerSelectable(form.components[2]),false);assert.equal(form.powerSelectable(form.components[3]),false);
 form.powerSelected.push(form.components[2]);assert.equal(form.powerInputReady,false,'stale unmanageable selections must block submission');
 form.powerSelected=form.components.slice(0,2);form.powerAction='delete';assert.equal(form.powerInputReady,false,'only start and stop are supported');
 form.powerAction='stop';form.powerReason=' ';assert.equal(form.powerInputReady,false,'an empty reason must not be submitted');
 form.powerSearch='19100';assert.deepEqual(Array.from(form.powerCandidates,c=>c.id),[22,24]);
 console.log('power batch action, selection, confirmation, response recovery and stale draft tests passed');
})().catch(e=>{console.error(e);process.exit(1)});
