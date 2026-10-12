/* Paired Prometheus historical recovery in the existing maintenance/change drawer. */
(function () {
  const validID = value => Number.isSafeInteger(Number(value)) && Number(value) > 0;
  const terminal = phase => ['restore_completed','restore_compensated'].includes(phase);
  const pending = phase => [
    'prepared','checkpoint_stopping','checkpoint_ready',
    'restore_stage_intent','restore_stage_copying','restore_stage_reclaim','restore_staged',
    'restore_executing','restore_starting','restore_compensating',
    'restore_compensation_starting','restore_execution_reclaim'
  ].includes(phase);
  const emptyState = () => ({eligible:false,preview:null,transaction:null,error:'',loaded:false});
  window.PromLifecycleMaintenanceRecovery = {
    data() {
      return {
        pairedRecovery:emptyState(),
        pairedRecoveryLoading:false,
        pairedRecoveryExecuting:false,
        pairedRecoveryEpoch:0
      };
    },
    computed: {
      pairedRecoveryUpgradeSupported() {
        const component=this.components.find(item=>Number(item.id)===Number(this.changeForm?.resource_id));
        return this.changeForm?.kind==='upgrade'&&component?.type==='prometheus'&&component?.deploy_type==='binary';
      },
      pairedRecoveryComponent() {
        return this.components.find(item=>Number(item.id)===Number(this.selectedPlan?.resource_id))||null;
      },
      pairedRecoveryOpenPlanID() {
        const planID=Number(this.selectedPlan?.id);
        return this.changeDrawer&&validID(planID)?planID:0;
      },
      pairedRecoveryBound() {
        return validID(this.selectedPlan?.paired_recovery_point_id)||this.pairedRecovery.eligible===true||
          validID(this.pairedRecovery.preview?.point_id)||validID(this.pairedRecovery.transaction?.point_id);
      },
      pairedRecoveryPending() {
        return pending(this.pairedRecovery.transaction?.phase);
      },
      pairedRecoveryCanExecute() {
        return this.canChange&&this.pairedRecovery.loaded&&this.pairedRecovery.eligible&&
          !!this.pairedRecovery.preview&&!this.pairedRecovery.transaction&&!this.pairedRecoveryLoading&&!this.pairedRecoveryExecuting;
      },
      pairedRecoveryOutcome() {
        const phase=this.pairedRecovery.transaction?.phase;
        if(phase==='restore_completed')return{kind:'success',text:'历史状态恢复成功'};
        if(phase==='restore_compensated')return{kind:'warning',text:'历史恢复失败；已恢复请求前状态'};
        if(phase==='aborted')return{kind:'warning',text:'历史恢复请求已中止，未完成恢复'};
        if(phase)return{kind:'warning',text:'恢复未终结，需要刷新或人工恢复'};
        if(this.pairedRecovery.preview)return{kind:'info',text:'已通过当前实例的配对历史恢复预览'};
        if(this.pairedRecovery.loaded&&!this.pairedRecovery.eligible)return{kind:'info',text:'该计划没有可用的配对历史恢复点'};
        return null;
      }
    },
    watch: {
      pairedRecoveryUpgradeSupported(value) {
        if(!value&&this.changeForm)this.changeForm.paired_recovery=false;
      },
      pairedRecoveryOpenPlanID(planID) {
        if(validID(planID))this.openPairedRecovery(planID);
        else this.clearPairedRecovery();
      }
    },
    mounted() {
      this.sessionController.signal.addEventListener('abort',()=>this.clearPairedRecovery(),{once:true});
    },
    beforeUnmount() {
      this.clearPairedRecovery();
    },
    methods: {
      clearPairedRecovery() {
        this.pairedRecoveryEpoch++;
        this.pairedRecoveryLoading=false;
        this.pairedRecoveryExecuting=false;
        this.pairedRecovery=emptyState();
      },
      validPairedRecoveryTransaction(value,planID) {
        return value&&validID(value.id)&&Number(value.plan_id)===Number(planID)&&validID(value.point_id)&&
          Number(value.component_id)===Number(this.selectedPlan?.resource_id)&&validID(value.host_id)&&typeof value.phase==='string'&&value.phase!==''&&
          Number.isSafeInteger(Number(value.revision));
      },
      validPairedRecoveryPreview(value,planID) {
        return value&&Number(value.plan_id)===Number(planID)&&validID(value.point_id)&&
          Number(value.component_id)===Number(this.selectedPlan?.resource_id)&&validID(value.host_id)&&
          typeof value.current_version==='string'&&typeof value.historical_version==='string'&&
          value.requires_downtime===true&&value.requires_current_checkpoint===true;
      },
      pairedRecoveryPhaseLabel(phase) {
        return {
          prepared:'已登记恢复意图',checkpoint_stopping:'正在停止并记录当前状态',checkpoint_ready:'当前状态检查点已就绪',
          restore_stage_intent:'已登记暂存意图',restore_stage_copying:'正在复制已验证恢复材料',
          restore_stage_reclaim:'暂存恢复需要独占恢复',restore_staged:'恢复材料已暂存',
          restore_executing:'正在替换为历史状态',restore_starting:'正在启动历史状态',
          restore_compensating:'历史恢复失败，正在恢复请求前状态',
          restore_compensation_starting:'正在启动请求前状态',restore_execution_reclaim:'执行中断，需要独占恢复',
          restore_completed:'历史状态恢复成功',restore_compensated:'历史恢复失败；请求前状态已恢复',aborted:'恢复请求已中止'
        }[phase]||phase||'尚未执行';
      },
      async openPairedRecovery(planID) {
        this.clearPairedRecovery();
        await this.loadPairedRecovery(planID);
      },
      async loadPairedRecovery(planID=this.selectedPlan?.id) {
        planID=Number(planID);
        if(!validID(planID)||!this.changeDrawer||Number(this.selectedPlan?.id)!==planID)return;
        const epoch=++this.pairedRecoveryEpoch,signal=this.sessionController.signal;
        this.pairedRecoveryLoading=true;
        this.pairedRecovery.error='';
        const current=()=>epoch===this.pairedRecoveryEpoch&&!signal.aborted&&this.changeDrawer&&Number(this.selectedPlan?.id)===planID;
        try {
          const response=await axios.get('/api/v1/change-plans/'+planID+'/paired-recovery',{signal,suppressToast:true});
          if(!current())return;
          const data=response.data||{},hasPreview=data.preview!==undefined&&data.preview!==null,hasTransaction=data.transaction!==undefined&&data.transaction!==null;
          if(typeof data.eligible!=='boolean'||(hasPreview&&!this.validPairedRecoveryPreview(data.preview,planID))||(hasTransaction&&!this.validPairedRecoveryTransaction(data.transaction,planID)))throw new Error('invalid paired recovery response');
          this.pairedRecovery={eligible:data.eligible===true,preview:hasPreview?data.preview:null,transaction:hasTransaction?data.transaction:null,error:'',loaded:true};
        } catch(error) {
          if(current())this.pairedRecovery={...emptyState(),error:error?.response?.status?'恢复状态读取失败（HTTP '+error.response.status+'），请稍后刷新。':'未收到恢复状态；请检查连接后只执行刷新，不要重复提交恢复。',loaded:true};
        } finally {
          if(current())this.pairedRecoveryLoading=false;
        }
      },
      async refreshPairedRecovery() {
        if(this.pairedRecoveryExecuting)return;
        await this.loadPairedRecovery(this.selectedPlan?.id);
      },
      async executePairedRecovery() {
        if(!this.pairedRecoveryCanExecute)return;
        const planID=Number(this.selectedPlan.id),component=this.pairedRecoveryComponent,preview=this.pairedRecovery.preview;
        const confirmation='将中断所选实例 '+(component?.name||('#'+preview.component_id))+
          '，从当前版本 '+preview.current_version+' 恢复到升级前历史版本 '+preview.historical_version+
          '。平台会先记录当前状态；若历史状态启动失败，将尝试恢复请求前状态。请输入 RESTORE_HISTORY';
        if(!await this.confirm(confirmation,'RESTORE_HISTORY'))return;
        if(!this.pairedRecoveryCanExecute||Number(this.selectedPlan?.id)!==planID)return;
        const epoch=this.pairedRecoveryEpoch,signal=this.sessionController.signal;
        this.pairedRecoveryExecuting=true;this.pairedRecovery.error='';
        const current=()=>epoch===this.pairedRecoveryEpoch&&!signal.aborted&&this.changeDrawer&&Number(this.selectedPlan?.id)===planID;
        try {
          const response=await axios.post('/api/v1/change-plans/'+planID+'/paired-recovery',{}, {signal,suppressToast:true});
          if(!current())return;
          const transaction=response.data?.transaction;
          if(!this.validPairedRecoveryTransaction(transaction,planID))throw new Error('invalid paired recovery result');
          this.pairedRecovery={eligible:true,preview:null,transaction,error:'',loaded:true};
        } catch(error) {
          if(!current())return;
          const transaction=error?.response?.data?.transaction;
          if(this.validPairedRecoveryTransaction(transaction,planID)) {
            this.pairedRecovery={eligible:true,preview:null,transaction,error:'',loaded:true};
          } else {
            this.pairedRecoveryExecuting=false;
            await this.loadPairedRecovery(planID);
            return;
          }
        } finally {
          if(current())this.pairedRecoveryExecuting=false;
        }
      },
      pairedRecoveryTerminal(phase) {
        return terminal(phase);
      }
    }
  };
}());
