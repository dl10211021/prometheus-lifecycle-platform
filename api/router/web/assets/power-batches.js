(function(){
 'use strict';
 window.PromLifecyclePowerBatches={
  data(){return {powerBatchDialog:false,powerAction:'start',powerName:'已有组件批量启停',powerReason:'计划维护',powerSearch:'',powerSelected:[]};},
  computed:{
   powerCandidates(){const q=this.powerSearch.trim().toLowerCase();return this.components.filter(c=>c.status!=='removed'&&(!q||[c.name,c.type,this.hostName(c.host_id),c.port].join(' ').toLowerCase().includes(q)));},
   powerInputReady(){return ['start','stop'].includes(this.powerAction)&&!!this.powerName.trim()&&!!this.powerReason.trim()&&this.powerSelected.length>0&&this.powerSelected.length<=200&&new Set(this.powerSelected.map(c=>c.host_id)).size<=50&&new Set(this.powerSelected.map(c=>c.id)).size===this.powerSelected.length&&this.powerSelected.every(c=>this.powerSelectable(c));}
  },
  watch:{
   powerBatchDialog(open){if(!open)this.clearDeploymentBatchDraft();},
   token(value){if(!value){this.powerBatchDialog=false;this.powerSelected=[];}}
  },
  methods:{
   powerEligibility(row){return !['binary','windows'].includes(row.deploy_type)?'需先托管，观察模式只读':row.status==='removed'?'已移除':['deploying','upgrading'].includes(row.status)?'正在部署或升级':'';},
   powerSelectable(row){return !this.powerEligibility(row);},
   powerBatchPayload(){return {kind:this.powerAction,name:this.powerName.trim(),reason:this.powerReason.trim(),component_ids:this.powerSelected.map(c=>c.id)};},
   async openBatchPowerWizard(){
    if(!this.canChange||this.batchSubmitting||!await this.guardNewBatchDraft())return;
    this.clearDeploymentBatchDraft();this.batchDeployMode=false;this.powerSelected=[];this.powerAction='start';this.powerName='已有组件批量启停';this.powerReason='计划维护';this.powerSearch='';
    await this.loadComponents();if(this.token)this.powerBatchDialog=true;
   }
  }
 };
})();
