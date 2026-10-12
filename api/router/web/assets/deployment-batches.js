(function () {
  'use strict';
  const validID = value => Number.isSafeInteger(Number(value)) && Number(value) > 0;
  const requestID = () => 'batch-' + Array.from(crypto.getRandomValues(new Uint8Array(16)), v => v.toString(16).padStart(2, '0')).join('');
  const pendingKey='promlifecycle.unresolved-deployment-batch';
  const readPending=()=>{try{const value=JSON.parse(localStorage.getItem(pendingKey)||'null');return value&&/^batch-[a-f0-9]{32}$/.test(value.request_id)?value:null;}catch(error){return null;}};
  window.PromLifecycleDeploymentBatches = {
    data() { const pending=readPending();return {
      batchDeployMode: false, batchHostIDs: [], batchName: '', deploymentBatches: [],
      selectedBatch: null, batchDrawer: false, batchPollTimer: null, batchDetailEpoch: 0,
      batchPreflightReports: [], batchPreflightKey: '', batchPreflightLoading: false,
      batchPreflightController: null, batchDraftEpoch: 0, batchSubmitting: false,
      batchRequestID: pending?.request_id||'', batchPendingPayload: null, batchSubmissionUnknown: !!pending,
      batchUnresolved: pending, batchLookupMissing: false,
      batchError: '', batchRetrying: false
    }; },
    computed: {
      batchDraftKey() { return JSON.stringify({hosts:this.batchHostIDs,template:this.deployForm.templateKey,types:this.deployQueue,settings:this.deploySettings,name:this.batchName}); },
      batchPreflightReady() {
        return this.batchHostIDs.length > 0 && this.batchPreflightKey === this.batchDraftKey &&
          this.batchPreflightReports.length === this.batchHostIDs.length && this.batchPreflightReports.every(row => row.passed);
      },
      batchFinishedHosts() { const s=this.selectedBatch?.summary;return s?(s.success+s.failed+s.interrupted):0; }
    },
    watch: {
      deployDialog(value) { if (!value && this.batchDeployMode) this.clearDeploymentBatchDraft(); },
      batchDrawer(value) { if (!value) { this.batchDetailEpoch++;clearTimeout(this.batchPollTimer); } },
      token(value) {
        if (!value) { this.clearDeploymentBatchDraft();this.batchDrawer=false;this.selectedBatch=null;this.deploymentBatches=[]; }
      }
    },
    beforeUnmount() { this.clearDeploymentBatchDraft();clearTimeout(this.batchPollTimer); },
    methods: {
      batchStatusLabel(status) { return {pending:'等待执行',running:'执行中',success:'全部成功',partial:'部分失败',failed:'失败',interrupted:'已中断',unknown:'状态待核实'}[status]||'状态待核实'; },
      async guardNewBatchDraft() {
        const pending=readPending()||this.batchUnresolved;
        if(!pending)return true;
        this.batchUnresolved=pending;this.batchRequestID=pending.request_id;this.batchSubmissionUnknown=true;
        this.domain='environment';this.environmentTab='batches';await this.reconcileBatchSubmission();return false;
      },
      async openBatchDeployWizard() {
        if (!this.canChange || this.batchSubmitting) return;
        if(!await this.guardNewBatchDraft())return;
        this.clearDeploymentBatchDraft();this.batchHostIDs=[];this.batchName='';this.batchRequestID='';
        await this.openDeployWizard(true);
      },
      clearDeploymentBatchDraft() {
        this.batchDraftEpoch++;this.batchPreflightController?.abort();this.batchPreflightController=null;
        this.batchPreflightLoading=false;this.batchPreflightReports=[];this.batchPreflightKey='';
        this.batchPendingPayload=null;this.batchSubmissionUnknown=!!this.batchUnresolved;this.batchError='';
        if (this.batchDeployMode) this.deploySettings={};
      },
      batchComponentPayload(type, hostID) {
        const payload=JSON.parse(JSON.stringify(this.componentDeploymentPayload(type,this.deploySettings[type],null)));
        payload.host_id=Number(hostID);payload.replace_existing=false;
        payload.name=(payload.name||type)+'-h'+hostID;
        return payload;
      },
      deploymentBatchStacks() {
        return this.batchHostIDs.map(hostID=>({
          name:(this.selectedStack?.name||'组件部署')+' · '+this.hostName(hostID),
          host_id:Number(hostID),template_key:this.deployForm.templateKey,
          components:this.deployQueue.map(type=>this.batchComponentPayload(type,hostID))
        }));
      },
      async runBatchPreflight() {
        if(!this.canChange||this.batchPreflightLoading||this.batchSubmitting||this.batchSubmissionUnknown||
           !this.deployConfigurationReady||!this.batchHostIDs.length||this.batchHostIDs.length>50)return;
        this.batchPreflightController?.abort();
        const controller=new AbortController(),epoch=this.batchDraftEpoch,key=this.batchDraftKey;
        this.batchPreflightController=controller;this.batchPreflightLoading=true;this.batchError='';this.batchPreflightKey='';
        const stacks=this.deploymentBatchStacks();
        const reports=stacks.map(stack=>({host_id:stack.host_id,passed:false,status:'pending',checks:[],error:''}));
        this.batchPreflightReports=reports;const reactiveReports=this.batchPreflightReports;let next=0;
        const run=async()=>{
          while(next<stacks.length&&!controller.signal.aborted){
            const index=next++,stack=stacks[index],row=reactiveReports[index];row.status='checking';
            try{
              let passed=true;
              for(const component of stack.components){
                const response=await axios.post('/api/v1/components/preflight',component,{signal:controller.signal,suppressToast:true});
                if(controller.signal.aborted||epoch!==this.batchDraftEpoch)return;
                passed=passed&&response.data.passed===true;
                row.checks.push({type:component.type,passed:response.data.passed===true,checks:response.data.checks||[]});
              }
              row.passed=passed;row.status=passed?'pass':'failed';
            }catch(error){
              if(controller.signal.aborted)return;
              row.status='failed';row.error=error.response?.data?.error||'预检请求失败，请检查连接后重试';
            }
          }
        };
        try{
          await Promise.all(Array.from({length:Math.min(3,stacks.length)},run));
          if(!controller.signal.aborted&&epoch===this.batchDraftEpoch&&key===this.batchDraftKey)this.batchPreflightKey=key;
        }finally{
          if(epoch===this.batchDraftEpoch){this.batchPreflightLoading=false;this.batchPreflightController=null;}
        }
      },
      async submitDeploymentBatch() {
        if(!this.canChange||this.batchSubmitting||this.batchPreflightLoading||this.upgradePreflightLoading)return;
        const upgrade=this.upgradeBatchDialog===true,adopt=this.adoptionBatchDialog===true,power=this.powerBatchDialog===true,removal=this.removalBatchDialog===true;
        const powerAction=power?(this.batchPendingPayload?.kind||this.powerAction):"";
        const ready=()=>removal?this.removalInputReady:power?this.powerInputReady:adopt?this.adoptionInputReady:upgrade?this.upgradePreflightReady:this.batchPreflightReady;
        if(this.batchSubmissionUnknown&&!this.batchPendingPayload){await this.reconcileBatchSubmission();return;}
        if(!this.batchSubmissionUnknown&&!ready())return;
        // Guard before awaiting confirmation: a second click cannot start a second submission.
        this.batchSubmitting=true;const epoch=this.batchDraftEpoch;
        try{
          const code=removal?'REMOVE_BATCH':power?(powerAction==='start'?'START_BATCH':'STOP_BATCH'):adopt?'ADOPT_BATCH':upgrade?'UPGRADE_BATCH':'DEPLOY_BATCH';
          if(!await this.confirm(removal?(this.removalMode==='keep_service'?'将移出所选实例的管理记录，原服务继续运行；平台不再管理或配置这些实例。请输入 REMOVE_BATCH':'将停止并卸载所选实例服务，保留数据；监控或页面可能暂时不可用。请输入 REMOVE_BATCH'):power?(powerAction==='start'?'将启用并启动所选组件服务，启动后验证健康状态。请输入 START_BATCH':'将停止所选组件服务，对应监控或页面将暂时不可用。保留配置、数据和开机启动设置。请输入 STOP_BATCH'):adopt?'将接管所选已有实例，执行时重新核对进程身份，保留原服务运行与配置。已纳管实例核对一致后跳过。请输入 ADOPT_BATCH':upgrade?'将升级选定的已有组件实例，执行期间可能短暂停止对应服务。请输入 UPGRADE_BATCH':'将向所选主机提交持久部署任务。已有实例不会自动覆盖；失败任务需显式重试。请输入 DEPLOY_BATCH',code))return;
          if(epoch!==this.batchDraftEpoch||!(removal?this.removalBatchDialog:power?this.powerBatchDialog:adopt?this.adoptionBatchDialog:upgrade?this.upgradeBatchDialog:this.deployDialog))return;
          if(!this.batchPendingPayload){
            this.batchRequestID=requestID();
            this.batchPendingPayload=removal?{...this.removalBatchPayload(),request_id:this.batchRequestID}:power?{...this.powerBatchPayload(),request_id:this.batchRequestID}:adopt?{...this.adoptionBatchPayload(),request_id:this.batchRequestID}:upgrade?{...this.upgradeBatchPayload(),request_id:this.batchRequestID}:{request_id:this.batchRequestID,name:this.batchName.trim()||(this.selectedStack?.name||'组件部署')+' · 多主机批次',stacks:this.deploymentBatchStacks()};
          }
          // Persist only recovery identity before sending, never the credential-bearing draft.
          const pending={request_id:this.batchRequestID,name:this.batchPendingPayload.name,host_count:removal?new Set(this.removalSelected.map(row=>row.host_id)).size:power?new Set(this.powerSelected.map(row=>row.host_id)).size:adopt?new Set(this.batchPendingPayload.adoptions.map(row=>row.host_id)).size:upgrade?new Set(this.upgradeSelected.map(row=>row.host_id)).size:this.batchPendingPayload.stacks.length,created_at:new Date().toISOString()};
          try{localStorage.setItem(pendingKey,JSON.stringify(pending));}catch(error){if(!this.batchUnresolved){this.batchPendingPayload=null;this.batchRequestID='';}this.batchError='浏览器无法保存提交查询标记，请允许本站存储后再提交。';return;}
          this.batchUnresolved=pending;this.batchLookupMissing=false;
          const response=await axios.post('/api/v1/deployments/batches',this.batchPendingPayload,{signal:this.sessionController.signal,suppressToast:true});
          if(epoch!==this.batchDraftEpoch||!this.token)return;
          if(!validID(response.data.batch?.id))throw new Error('invalid accepted batch');
          this.resolveBatchSubmission();
          this.deployDialog=false;this.upgradeBatchDialog=false;this.adoptionBatchDialog=false;this.powerBatchDialog=false;this.removalBatchDialog=false;this.deploySettings={};
          await this.openDeploymentBatch(response.data.batch).catch(()=>{});
          ElementPlus.ElMessage.info('批次已保存，关闭页面不影响服务端执行');
        }catch(error){
          if(epoch!==this.batchDraftEpoch||!this.token)return;
          const uncertain=this.batchSubmissionUnknown||!error.response||error.response.status>=500;
          this.batchSubmissionUnknown=uncertain;
          this.batchError=uncertain?'提交结果待核实：先查回原批次，或用同一提交标识重试。':(error.response?.data?.error||'批次提交失败');
          if(!uncertain){this.resolveBatchSubmission();if(upgrade){this.upgradeChecks=error.response?.data?.checks||this.upgradeChecks;this.upgradePreflightKey='';}}
        }finally{this.batchSubmitting=false;}
      },
      resolveBatchSubmission() {
        if(readPending()?.request_id===this.batchRequestID){try{localStorage.removeItem(pendingKey);}catch(error){}}
        this.batchUnresolved=null;this.batchSubmissionUnknown=false;this.batchPendingPayload=null;this.batchRequestID='';this.batchLookupMissing=false;
      },
      async reconcileBatchSubmission() {
        if(!this.batchRequestID||this.batchSubmitting)return;
        const key=this.batchRequestID;this.batchLookupMissing=false;
        try{
          const response=await axios.get('/api/v1/deployments/batches',{params:{request_id:key},signal:this.sessionController.signal});
          if(key!==this.batchRequestID||!this.token)return;
          const batch=(response.data.batches||[]).find(item=>item.request_id===key);
          if(batch){this.resolveBatchSubmission();this.deployDialog=false;this.upgradeBatchDialog=false;this.adoptionBatchDialog=false;this.powerBatchDialog=false;this.removalBatchDialog=false;this.deploySettings={};await this.openDeploymentBatch(batch);}
          else {this.batchLookupMissing=true;this.batchError=this.batchPendingPayload?'暂未查到该提交，可使用原提交标识重试。':'暂未查到原批次。请恢复连接后再查询；只有确认没有提交成功，才能解除标记并重新创建。';}
        }catch(error){if(key===this.batchRequestID)this.batchError='查询失败，请恢复连接后再次查回批次。';}
      },
      async dismissUnresolvedBatch() {
        if(!this.canChange||!this.batchLookupMissing||this.batchSubmitting)return;
        if(!await this.confirm('原批次暂未查到，但此前请求仍可能在处理中。仅在已核实没有提交成功时解除标记。请输入 CLEAR_BATCH','CLEAR_BATCH'))return;
        this.resolveBatchSubmission();this.batchError='';this.deployDialog=false;
      },
      async loadDeploymentBatches() {
        try{const response=await this.latestGet('deploymentBatches','/api/v1/deployments/batches');if(response)this.deploymentBatches=response.data.batches||[];}catch(error){}
      },
      async openDeploymentBatch(row) {
        clearTimeout(this.batchPollTimer);
        const epoch=++this.batchDetailEpoch;
        const response=await this.latestGet('deploymentBatchDetail','/api/v1/deployments/batches/'+row.id);
        if(!response||!this.token||epoch!==this.batchDetailEpoch||Number(response.data.batch?.id)!==Number(row.id))return;
        this.selectedBatch=response.data.batch;this.stackDrawer=false;this.batchDrawer=true;
        this.domain='environment';this.environmentTab='batches';
        await this.loadDeploymentBatches();this.scheduleDeploymentBatchPoll();
      },
      scheduleDeploymentBatchPoll() {
        clearTimeout(this.batchPollTimer);
        if(!this.token||!this.batchDrawer||!this.selectedBatch||!['pending','running'].includes(this.selectedBatch.status))return;
        const id=this.selectedBatch.id,epoch=this.batchDetailEpoch;
        this.batchPollTimer=setTimeout(async()=>{
          try{
            const response=await this.latestGet('deploymentBatchDetail','/api/v1/deployments/batches/'+id);
            if(response&&this.batchDrawer&&epoch===this.batchDetailEpoch&&this.selectedBatch?.id===id){
              this.selectedBatch=response.data.batch;await this.loadDeploymentBatches();
            }
          }catch(error){}finally{if(epoch===this.batchDetailEpoch)this.scheduleDeploymentBatchPoll();}
        },2000);
      },
      async retryDeploymentBatch() {
        const batch=this.selectedBatch;
        if(!this.canChange||this.batchRetrying||!batch||!(batch.summary.failed+batch.summary.interrupted))return;
        this.batchRetrying=true;
        try{
          if(!await this.confirm('仅重试本批次失败或中断的主机任务；成功步骤保留。请输入 RETRY_BATCH','RETRY_BATCH'))return;
          await axios.post('/api/v1/deployments/batches/'+batch.id+'/retry',{});
          await this.openDeploymentBatch(batch);
        }finally{this.batchRetrying=false;}
      },
      async openBatchHostTask(stack) {this.batchDrawer=false;await this.openStackRun(stack);}
    }
  };
})();
