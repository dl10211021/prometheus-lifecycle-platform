const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const storage=new Map(),root=path.join(__dirname,'../api/router/web/assets');
const sandbox={window:{},localStorage:{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},crypto:require('node:crypto').webcrypto,AbortController,setTimeout,clearTimeout,ElementPlus:{ElMessage:{info(){}}}};
vm.createContext(sandbox);
for(const f of ['deployment-batches.js','adoption-batches.js'])vm.runInContext(fs.readFileSync(path.join(root,f),'utf8'),sandbox);
const common=sandbox.window.PromLifecycleDeploymentBatches,adopt=sandbox.window.PromLifecycleAdoptionBatches;
function fixture(){
 storage.clear();
 const ctx={...common.data(),...adopt.data(),canChange:true,token:'fixture',adoptionBatchDialog:true,sessionController:new AbortController(),confirm:async()=>true,hostName:id=>'host-'+id,openDeploymentBatch:async()=>{}};
 for(const [name,fn]of Object.entries({...common.methods,...adopt.methods}))ctx[name]=fn.bind(ctx);
 for(const [name,fn]of Object.entries(adopt.computed))Object.defineProperty(ctx,name,{get:fn.bind(ctx)});
 ctx.adoptionName='existing services';return ctx;
}
(async()=>{
 const ctx=fixture();ctx.adoptionHostIDs=[1,2,3,4];
 let active=0,maxActive=0,requests=[],failed=true;
 sandbox.axios={get:async url=>{
  const id=Number(url.match(/hosts\/(\d+)/)[1]);requests.push(id);active++;maxActive=Math.max(maxActive,active);
  await new Promise(r=>setTimeout(r,5));active--;
  if(id===2&&failed)throw new Error('offline');
  return {data:{host_id:id,components:[{type:'node_exporter',port:9100,fingerprint:'a'+id},{type:'node_exporter',port:19100,fingerprint:'b'+id},{type:'prometheus',port:9090,fingerprint:'c'+id,ownership:'managed'}]}};
 }};
 await ctx.scanAdoptionHosts();
 assert.equal(maxActive,3,'host scan concurrency must be bounded');
 assert.equal(ctx.adoptionScans.filter(x=>x.status==='failed').length,1);
 assert.equal(ctx.adoptionRows.length,9);
 const first=ctx.adoptionRows.filter(x=>x.host_id===1&&x.type==='node_exporter');
 ctx.adoptionSelected=first;
 assert.equal(ctx.adoptionInputReady,true);
 assert.deepEqual(Array.from(ctx.adoptionBatchPayload().adoptions,x=>x.port),[9100,19100],'same-type instances must not collapse');
 assert.equal(ctx.adoptionSelectable(ctx.adoptionRows.find(x=>x.ownership)),false,'owned instances are excluded');
 requests=[];failed=false;await ctx.scanAdoptionHosts(true);
 assert.deepEqual(requests,[2],'retry must scan only failed hosts');
 assert.equal(ctx.adoptionSelected.length,2,'successful host selections survive failed-host retry');
 assert.equal(ctx.adoptionRows.length,12);
 // Ownership learned by a subsequent scan invalidates stale selection.
 first[0].ownership='managed';assert.equal(ctx.adoptionBatchPayload().adoptions.length,1);
 first[0].ownership='';
 first[0].ownership_conflict=true;assert.equal(ctx.adoptionSelectable(first[0]),false,'port identity conflict is not adoptable');assert.equal(ctx.adoptionBatchPayload().adoptions.length,1);first[0].ownership_conflict=false;
 let ids=[];sandbox.axios.post=async(url,payload)=>{ids.push(payload.request_id);assert.equal(payload.kind,'adopt');assert.equal(payload.adoptions.length,2);throw new Error('response lost')};
 await ctx.submitDeploymentBatch();await ctx.submitDeploymentBatch();
 assert.equal(ids.length,2);assert.equal(ids[0],ids[1]);assert.equal(ctx.batchSubmissionUnknown,true);
 const saved=JSON.parse([...storage.values()][0]);assert.equal(saved.host_count,1);assert.equal(saved.adoptions,undefined);
 const late=fixture();late.adoptionHostIDs=[1];let release;
 sandbox.axios.get=async()=>{await new Promise(r=>release=r);return{data:{host_id:1,components:[{type:'node_exporter',port:9100,fingerprint:'late'}]}}};
 const running=late.scanAdoptionHosts();late.resetAdoptionScan();release();await running;
 assert.equal(late.adoptionRows.length,0,'old results must not repopulate reset scans');
 assert.equal(late.adoptionScanning,false);
 console.log('batch discovery concurrency, partial retry, instance identity, stale responses, and submission recovery passed');
})().catch(e=>{console.error(e);process.exit(1)});
