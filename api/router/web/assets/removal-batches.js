(function(){
 'use strict';
 window.PromLifecycleRemovalBatches={
  data(){return {removalBatchDialog:false,removalMode:'',removalName:'已有组件批量移除',removalReason:'计划调整',removalSearch:'',removalSelected:[]};},
  computed:{
   removalCandidates(){const q=this.removalSearch.trim().toLowerCase();return this.components.filter(c=>c.status!=='removed'&&(!q||[c.name,c.type,this.hostName(c.host_id),c.port].join(' ').toLowerCase().includes(q)));},
   removalInputReady(){return ['keep_service','keep_data'].includes(this.removalMode)&&!!this.removalName.trim()&&!!this.removalReason.trim()&&this.removalSelected.length>0&&this.removalSelected.length<=200&&new Set(this.removalSelected.map(c=>c.host_id)).size<=50&&new Set(this.removalSelected.map(c=>c.id)).size===this.removalSelected.length&&this.removalSelected.every(c=>this.removalSelectable(c));}
  },
  watch:{
   removalBatchDialog(open){if(!open)this.clearDeploymentBatchDraft();},
   token(value){if(!value){this.removalBatchDialog=false;this.removalSelected=[];}}
  },
  methods:{
   removalEligibility(row){return !['binary','windows'].includes(row.deploy_type)?'仅受管服务可批量移除':row.status==='removed'?'已移除':['deploying','upgrading'].includes(row.status)?'正在部署或升级':row.ownership==='observe'?'观察模式只读':'';},
   removalSelectable(row){return !this.removalEligibility(row);},
   removalBatchPayload(){return {kind:'remove',mode:this.removalMode,name:this.removalName.trim(),reason:this.removalReason.trim(),component_ids:this.removalSelected.map(c=>c.id)};},
   async openBatchRemovalWizard(){
    if(!this.canChange||this.batchSubmitting||!await this.guardNewBatchDraft())return;
    this.clearDeploymentBatchDraft();this.batchDeployMode=false;this.removalSelected=[];this.removalMode='';this.removalName='已有组件批量移除';this.removalReason='计划调整';this.removalSearch='';
    await this.loadComponents();if(this.token)this.removalBatchDialog=true;
   }
  }
 };
})();
