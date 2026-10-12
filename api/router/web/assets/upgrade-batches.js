(function(){
 'use strict';
 window.PromLifecycleUpgradeBatches={
  data(){return {upgradeBatchDialog:false,upgradeSelected:[],upgradeTargets:{},upgradeReason:'统一升级已有组件',upgradeName:'',upgradeSearch:'',upgradeChecks:[],upgradePreflightKey:'',upgradePreflightLoading:false,upgradePreflightController:null};},
  computed:{
   upgradeCandidates(){const q=this.upgradeSearch.trim().toLowerCase();return this.components.filter(c=>c.status!=='removed'&&(!q||[c.name,c.type,this.hostName(c.host_id),c.port].join(' ').toLowerCase().includes(q)));},
   upgradeTypes(){return [...new Set(this.upgradeSelected.map(c=>c.type))];},
   upgradeDraftKey(){return JSON.stringify(this.upgradeBatchPayload());},
   upgradeInputReady(){return this.upgradeSelected.length>0&&this.upgradeSelected.length<=200&&new Set(this.upgradeSelected.map(c=>c.host_id)).size<=50&&!!this.upgradeName.trim()&&!!this.upgradeReason.trim()&&this.upgradeTypes.every(t=>(this.upgradeTargets[t]||'').trim());},
   upgradePreflightReady(){return this.upgradeInputReady&&this.upgradePreflightKey===this.upgradeDraftKey&&this.upgradeChecks.length===this.upgradeSelected.length&&this.upgradeChecks.every(c=>c.passed);}
  },
  watch:{
   upgradeBatchDialog(open){if(!open){this.upgradePreflightController?.abort();this.upgradePreflightController=null;this.upgradePreflightLoading=false;this.upgradeChecks=[];this.upgradePreflightKey='';this.clearDeploymentBatchDraft();}},
   token(value){if(!value){this.upgradeBatchDialog=false;this.upgradeSelected=[];this.upgradeTargets={};}}
  },
  beforeUnmount(){this.upgradePreflightController?.abort();},
  methods:{
   upgradeEligibility(row){return row.deploy_type!=='binary'?'需先托管接管':row.status!=='running'?'需先启动并恢复健康':!row.version?'当前版本未知':'';},
   upgradeSelectable(row){return !this.upgradeEligibility(row);},
   upgradeVersions(type){return [...new Set((this.componentVersions||[]).filter(v=>v.type===type).map(v=>v.version))];},
   upgradeBatchPayload(){return {kind:'upgrade',name:this.upgradeName.trim(),reason:this.upgradeReason.trim(),upgrades:this.upgradeSelected.map(c=>({component_id:c.id,current_version:c.version,new_version:(this.upgradeTargets[c.type]||'').trim()}))};},
   async openBatchUpgradeWizard(){
    if(!this.canChange||this.batchSubmitting||!await this.guardNewBatchDraft())return;
    this.clearDeploymentBatchDraft();this.batchDeployMode=false;this.upgradeSelected=[];this.upgradeTargets={};this.upgradeName='已有组件批量升级';this.upgradeReason='统一升级已有组件';this.upgradeSearch='';this.upgradeChecks=[];this.upgradePreflightKey='';
    await Promise.all([this.loadComponents(),this.loadVersions()]);if(this.token)this.upgradeBatchDialog=true;
   },
   async runUpgradePreflight(){
    if(!this.canChange||!this.upgradeInputReady||this.upgradePreflightLoading||this.batchSubmitting||this.batchSubmissionUnknown)return;
    const controller=new AbortController(),epoch=this.batchDraftEpoch,key=this.upgradeDraftKey,payload=this.upgradeBatchPayload();
    this.upgradePreflightController?.abort();this.upgradePreflightController=controller;this.upgradePreflightLoading=true;this.upgradePreflightKey='';this.batchError='';
    this.upgradeChecks=payload.upgrades.map(item=>({...item,passed:false,state:'pending',error:''}));
    const rows=this.upgradeChecks;let next=0;
    const run=async()=>{while(next<payload.upgrades.length&&!controller.signal.aborted){
     const i=next++,row=rows[i];row.state='checking';
     try{
      const response=await axios.post('/api/v1/deployments/batches/preflight',{...payload,upgrades:[payload.upgrades[i]]},{signal:controller.signal,suppressToast:true});
      if(controller.signal.aborted||epoch!==this.batchDraftEpoch)return;
      Object.assign(row,response.data.checks?.[0]||{passed:false,error:'未返回有效检查结果'});row.state=row.passed?'pass':'failed';
     }catch(error){if(controller.signal.aborted)return;row.state='failed';row.error=error.response?.data?.error||'预检请求失败，请检查连接后重试';}
    }};
    try{await Promise.all(Array.from({length:Math.min(3,payload.upgrades.length)},run));if(!controller.signal.aborted&&epoch===this.batchDraftEpoch&&key===this.upgradeDraftKey)this.upgradePreflightKey=key;}
    finally{if(epoch===this.batchDraftEpoch){this.upgradePreflightLoading=false;this.upgradePreflightController=null;}}
   },
   async openBatchChange(planID){this.batchDrawer=false;await this.openChangePlan({id:planID});}
  }
 };
})();
