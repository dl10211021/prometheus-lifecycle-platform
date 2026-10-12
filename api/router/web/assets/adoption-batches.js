(function(){
 'use strict';
 window.PromLifecycleAdoptionBatches={
  data(){return {adoptionBatchDialog:false,adoptionHostIDs:[],adoptionScans:[],adoptionSelected:[],adoptionName:'已有实例批量接管',adoptionScanning:false,adoptionScanEpoch:0,adoptionScanController:null};},
  computed:{
   adoptionRows(){return this.adoptionScans.flatMap(scan=>scan.components||[]);},
   adoptionFailedHosts(){return this.adoptionScans.filter(s=>s.status==='failed').length;},
   adoptionInputReady(){const items=this.adoptionBatchPayload().adoptions;return !this.adoptionScanning&&!!this.adoptionName.trim()&&items.length>0&&items.length<=200&&new Set(items.map(x=>x.host_id)).size<=50;}
  },
  watch:{
   adoptionHostIDs(){this.resetAdoptionScan();},
   adoptionBatchDialog(open){if(!open){this.resetAdoptionScan();this.clearDeploymentBatchDraft();}},
   token(value){if(!value){this.adoptionBatchDialog=false;this.resetAdoptionScan();}}
  },
  beforeUnmount(){this.resetAdoptionScan();},
  methods:{
   resetAdoptionScan(){this.adoptionScanEpoch++;this.adoptionScanController?.abort();this.adoptionScanController=null;this.adoptionScanning=false;this.adoptionScans=[];this.adoptionSelected=[];},
   adoptionSelectable(row){return !row.ownership&&!row.ownership_conflict&&!!row.fingerprint;},
   adoptionBatchPayload(){
    const current=new Map(this.adoptionRows.map(row=>[row.host_id+':'+row.fingerprint,row]));
    const adoptions=this.adoptionSelected.map(row=>current.get(row.host_id+':'+row.fingerprint)).filter(row=>row&&this.adoptionSelectable(row));
    return {kind:'adopt',name:this.adoptionName.trim(),adoptions:adoptions.map(row=>({host_id:Number(row.host_id),type:row.type,port:row.port,name:row.name||row.type,fingerprint:row.fingerprint,ownership:row.selectedOwnership||'managed'}))};
   },
   async openBatchAdoptionWizard(){
    if(!this.canChange||this.batchSubmitting||!await this.guardNewBatchDraft())return;
    this.clearDeploymentBatchDraft();this.batchDeployMode=false;this.adoptionHostIDs=[];this.resetAdoptionScan();this.adoptionName='已有实例批量接管';this.adoptionBatchDialog=true;
   },
   async scanAdoptionHosts(failedOnly=false){
    if(this.adoptionScanning||this.batchSubmitting||this.batchSubmissionUnknown||!this.token)return;
    if(!this.adoptionHostIDs.length||this.adoptionHostIDs.length>50)return;
    if(!failedOnly){
     this.resetAdoptionScan();
     this.adoptionScans=this.adoptionHostIDs.map(id=>({host_id:Number(id),status:'pending',error:'',components:[]}));
    }
    const controller=new AbortController(),epoch=++this.adoptionScanEpoch;
    this.adoptionScanController=controller;this.adoptionScanning=true;
    const rows=this.adoptionScans.filter(row=>!failedOnly||row.status==='failed');let next=0;
    const run=async()=>{while(next<rows.length&&!controller.signal.aborted){
     const row=rows[next++];row.status='scanning';row.error='';
     try{
      const response=await axios.get('/api/v1/hosts/'+row.host_id+'/detect',{params:{versions:1},signal:controller.signal,suppressToast:true});
      if(controller.signal.aborted||epoch!==this.adoptionScanEpoch||!this.token)return;
      if(Number(response.data.host_id)!==row.host_id||!Array.isArray(response.data.components))throw new Error('invalid scan response');
      row.components=response.data.components.map(item=>({...item,host_id:row.host_id,selectedOwnership:item.ownership||'managed',instanceKey:row.host_id+':'+item.fingerprint}));row.status='success';
     }catch(error){
      if(controller.signal.aborted||epoch!==this.adoptionScanEpoch)return;
      row.status='failed';row.components=[];row.error='扫描失败，请检查主机连接后重试';
     }
    }};
    try{await Promise.all(Array.from({length:Math.min(3,rows.length)},run));}
    finally{if(epoch===this.adoptionScanEpoch){this.adoptionScanning=false;this.adoptionScanController=null;}}
   },
   adoptionScanLabel(status){return {pending:'等待扫描',scanning:'扫描中',success:'扫描完成',failed:'扫描失败'}[status]||status;},
   batchKindLabel(kind){return {adopt:'接管',upgrade:'升级',start:'启动',stop:'停止',remove:'移除'}[kind]||'部署';},
   adoptionResultLabel(result){return {adopted:'已接管',already_owned:'已纳管，跳过',started:'已启动并验证',stopped:'已停止',released:'已解除纳管，服务保留',uninstalled_keep_data:'服务已移除，数据保留'}[result]||'';}
  }
 };
})();
