const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const root=path.join(__dirname,'../api/router/web/assets'),storage=new Map();
const sandbox={window:{},localStorage:{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},crypto:require('node:crypto').webcrypto,AbortController,setTimeout,clearTimeout,ElementPlus:{ElMessage:{info(){}}}};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(path.join(root,'deployment-batches.js'),'utf8'),sandbox);
if(fs.existsSync(path.join(root,'removal-batches.js')))vm.runInContext(fs.readFileSync(path.join(root,'removal-batches.js'),'utf8'),sandbox);
const common=sandbox.window.PromLifecycleDeploymentBatches,remove=sandbox.window.PromLifecycleRemovalBatches||{data:()=>({}),methods:{},computed:{}};
function fixture(mode){
 storage.clear();
 const ctx={...common.data(),...remove.data(),canChange:true,token:'fixture',sessionController:new AbortController(),removalBatchDialog:true,removalMode:mode,removalName:'batch remove',removalReason:'maintenance',removalSelected:[{id:11,host_id:1,name:'one',type:'node_exporter',port:9100,status:'running',deploy_type:'binary'},{id:12,host_id:2,name:'two',type:'node_exporter',port:19100,status:'stopped',deploy_type:'binary'}],confirm:async()=>true,openDeploymentBatch:async()=>{},removalBatchPayload:()=>({kind:'remove',name:'batch remove',mode,reason:'maintenance',component_ids:[11,12]})};
 for(const [name,fn]of Object.entries({...common.methods,...remove.methods}))ctx[name]=fn.bind(ctx);
 for(const [name,fn]of Object.entries(remove.computed))Object.defineProperty(ctx,name,{get:fn.bind(ctx)});
 return ctx;
}
(async()=>{
 for(const mode of ['keep_service','keep_data']){
  const ctx=fixture(mode);let submitted=[],confirmation;
  ctx.confirm=async(message,code)=>{confirmation=code;return true;};
  sandbox.axios={post:async(url,payload)=>{submitted.push(JSON.parse(JSON.stringify(payload)));return{data:{batch:{id:97}}};}};
  await ctx.submitDeploymentBatch();
  assert.equal(submitted.length,1,'removal must submit once');assert.equal(submitted[0].kind,'remove');assert.equal(submitted[0].mode,mode);
  assert.deepEqual(submitted[0].component_ids,[11,12]);assert.equal(confirmation,'REMOVE_BATCH');assert.equal(ctx.removalBatchDialog,false);assert.equal(storage.size,0);
 }
 const uncertain=fixture('keep_data');let submitted=[];
 sandbox.axios.post=async(url,payload)=>{submitted.push(JSON.parse(JSON.stringify(payload)));throw new Error('lost response');};
 await uncertain.submitDeploymentBatch();await uncertain.submitDeploymentBatch();
 assert.equal(submitted.length,2);assert.equal(submitted[0].request_id,submitted[1].request_id);
 assert.equal(JSON.parse([...storage.values()][0]).component_ids,undefined);
 sandbox.axios.get=async()=>({data:{batches:[{id:98,request_id:submitted[0].request_id}]}});
 await uncertain.reconcileBatchSubmission();assert.equal(uncertain.removalBatchDialog,false);assert.equal(storage.size,0);
 const closed=fixture('keep_data');closed.confirm=async()=>{closed.removalBatchDialog=false;closed.clearDeploymentBatchDraft();return true;};let posts=0;
 sandbox.axios.post=async()=>{posts++;return{data:{batch:{id:99}}};};
 await closed.submitDeploymentBatch();assert.equal(posts,0,'closed removal form submitted after confirmation');
 const form=fixture('keep_data');form.hostName=id=>'host-'+id;form.components=[{id:11,host_id:1,name:'one',type:'node_exporter',port:9100,status:'running',deploy_type:'binary'},{id:12,host_id:1,name:'two',type:'node_exporter',port:19100,status:'stopped',deploy_type:'binary'},{id:13,host_id:2,name:'busy',type:'node_exporter',port:9100,status:'upgrading',deploy_type:'binary'},{id:14,host_id:2,name:'observe',type:'node_exporter',port:19100,status:'running',deploy_type:'target'}];
 assert.equal(form.removalInputReady,true);assert.equal(form.removalSelectable(form.components[2]),false);assert.equal(form.removalSelectable(form.components[3]),false);
 form.removalMode='full_delete';assert.equal(form.removalInputReady,false);form.removalMode='keep_data';form.removalSelected.push(form.components[3]);assert.equal(form.removalInputReady,false);
 const html=fs.readFileSync(path.join(__dirname,'../api/router/web/index.html'),'utf8');
 assert.match(html,/label="keep_service"/);assert.match(html,/label="keep_data"/);assert.match(html,/window\.PromLifecycleRemovalBatches/);
 console.log('removal batch modes, selection, confirmation and response recovery passed');
})().catch(e=>{console.error(e);process.exit(1)});
