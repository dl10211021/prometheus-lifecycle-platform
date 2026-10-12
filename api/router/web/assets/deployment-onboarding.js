/* Scoped deployment onboarding. Installation continues through the existing persistent stack API. */
(function () {
  const validID = value => Number.isSafeInteger(value) && value > 0;
  const emptyForm = () => ({version:'',port:9100,install_path:'',data_path_override:'',custom_args:'',custom_env:'',resource_limits:''});
  const activeComponent = component => !['removed','uninstalled'].includes(component.status);
  window.PromLifecycleScopedReport = {
    props: {report:Object},
    template: `<div v-if="report" class="card scoped-acceptance">
      <div class="card-title"><strong>{{ report.message || (report.passed ? '运行采集验收通过' : '运行采集验收未通过') }}</strong>
        <span class="pill" :class="report.passed?'ok':'bad'">{{ report.passed?'通过':'需要处理' }}</span></div>
      <template v-if="report.scope">
        <div class="subtle">本次范围：{{ report.scope.kind==='stack'?'整栈任务 #'+report.scope.stack_id:'主机 #'+report.scope.host_id }} · Prometheus #{{ report.scope.prometheus_id || '未选定' }} · {{ report.checked_at || '' }}</div>
        <div class="subtle">仅核对本次组件运行与采集；不代表通知送达、备份或告警规则已发布。</div>
        <div v-for="(check,index) in report.checks || []" :key="check.key+'-'+index" style="margin-top:8px">
          <span class="pill" :class="check.status==='pass'?'ok':check.status==='fail'?'bad':'warn'">{{ check.status }}</span>
          {{ check.key }} · {{ check.reason }}
        </div>
        <el-table :data="report.targets || []" empty-text="本次没有适用的采集目标">
          <el-table-column label="目标" min-width="240"><template #default="{row}">组件 #{{ row.component_id }} · 主机 #{{ row.host_id }}<div class="mono">{{ row.address }}</div></template></el-table-column>
          <el-table-column prop="status" label="采集状态" width="100"></el-table-column>
          <el-table-column prop="reason" label="证据 / 原因" min-width="170"></el-table-column>
          <el-table-column label="最近采集" min-width="155"><template #default="{row}">{{ row.last_scrape || '尚无有效采集' }}</template></el-table-column>
        </el-table>
      </template>
      <div v-else-if="report.error" class="text-danger">{{ report.error }}</div>
    </div>`
  };
  window.PromLifecycleDeploymentOnboarding = {
    data() { return {
      selectedMonitoringHosts:[], monitoringDialog:false, monitoringLoading:false,
      monitoringSubmitting:false, monitoringAccepting:false, monitoringRows:[],
      monitoringForm:emptyForm(), monitoringVersions:[], monitoringPrometheusID:null,
      monitoringError:'', monitoringEpoch:0, monitoringLoadEpoch:0,
      stackAcceptanceResult:null, stackAcceptanceError:'', stackAcceptanceLoading:false,
      stackAcceptancePrometheusID:null, stackAcceptanceEpoch:0
    }; },
    computed: {
      batchMonitoringHostIDs() {
        return [...new Set(this.batchHostRows.filter(row=>['created','exists'].includes(row.status)&&validID(row.host_id)).map(row=>row.host_id))];
      },
      monitoringPrometheusOptions() { return this.components.filter(c=>c.type==='prometheus'&&activeComponent(c)); },
      monitoringMissingRows() { return this.monitoringRows.filter(r=>!r.stack_id&&r.status==='missing'); },
      monitoringFailedRows() { return this.monitoringRows.filter(r=>r.status==='failed'); },
      monitoringReusableRows() { return this.monitoringRows.filter(r=>validID(r.component_id)&&['reuse','success'].includes(r.status)); },
      stackBoundPrometheusID() {
        const steps=(this.selectedStackRun?.steps||[]).filter(s=>s.type==='prometheus');
        return steps.length===1 && validID(steps[0].component_id) ? steps[0].component_id : null;
      }
    },
    watch: {
      monitoringPrometheusID() { this.invalidateMonitoringAcceptance(); },
      'selectedStackRun.id'() { this.prepareStackAcceptance(); },
      stackAcceptancePrometheusID() { this.invalidateStackAcceptance(); },
      stackDrawer(value) { if(!value)this.invalidateStackAcceptance(); }
    },
    mounted() {
      this.sessionController.signal.addEventListener('abort',()=>this.clearMonitoringSession(),{once:true});
    },
    beforeUnmount() { this.clearMonitoringSession(); },
    methods: {
      monitoringSelectHosts(rows) { this.selectedMonitoringHosts=rows.map(row=>row.id).filter(validID); },
      continueBatchMonitoring() {
        const ids=[...this.batchMonitoringHostIDs];
        if(!ids.length || this.batchHostLoading)return;
        this.batchHostDialog=false; this.closeBatchHosts();
        this.openMonitoringOnboarding(ids);
      },
      invalidateMonitoringAcceptance() {
        this.monitoringEpoch++; this.monitoringAccepting=false;
        for(const row of this.monitoringRows){row.report=null;row.acceptance_error='';}
      },
      invalidateStackAcceptance() {
        this.stackAcceptanceEpoch++;this.stackAcceptanceResult=null;this.stackAcceptanceError='';this.stackAcceptanceLoading=false;
      },
      prepareStackAcceptance() {
        this.invalidateStackAcceptance();
        const bound=this.stackBoundPrometheusID;
        const stack=this.selectedStackRun;
        const linked=(stack?.steps||[]).map(step=>this.components.find(c=>
          c.id===step.component_id&&c.type===step.type&&c.host_id===stack.host_id&&activeComponent(c)));
        const cluster=linked[0]?.cluster_id;
        const same=linked.length&&validID(cluster)&&linked.every(c=>c&&c.cluster_id===cluster)
          ? this.monitoringPrometheusOptions.filter(c=>c.cluster_id===cluster) : [];
        this.stackAcceptancePrometheusID=bound || (same.length===1?same[0].id:null);
      },
      clearMonitoringSession() {
        this.monitoringLoadEpoch++;this.invalidateMonitoringAcceptance();this.invalidateStackAcceptance();
        this.monitoringForm=emptyForm();this.monitoringRows=[];this.selectedMonitoringHosts=[];
        this.monitoringDialog=false;this.monitoringLoading=false;
      },
      closeMonitoringOnboarding() {
        this.monitoringDialog=false;this.monitoringLoadEpoch++;
        this.monitoringForm=emptyForm();this.invalidateMonitoringAcceptance();
      },
      monitoringErrorText(error) {
        // Do not surface reflected deployment payloads, which can contain environment secrets.
        const status=error?.response?.status;
        return status ? '请求失败（HTTP '+status+'），请核对任务或选择；可在任务详情查看服务端证据。' : '未收到完整结果，请到整栈任务列表核对；禁止自动重放。';
      },
      monitoringRowLabel(status) {
        return {loading:'检查中',missing:'缺失，待部署',reuse:'复用已有实例',choose:'请选择 Exporter',discovered:'发现未纳管实例',unsupported:'仅支持 Linux 批量接入',failed:'明确失败',unknown:'结果未知',pending:'任务已接受',running:'部署中',success:'安装成功',interrupted:'任务中断'}[status]||status;
      },
      async openMonitoringOnboarding(ids) {
        if(!this.canChange||this.monitoringSubmitting)return;
        ids=[...new Set((ids||[]).filter(validID))];if(!ids.length)return;
        this.invalidateMonitoringAcceptance();this.monitoringForm=emptyForm();this.monitoringError='';
        this.monitoringRows=[];this.monitoringPrometheusID=null;this.monitoringDialog=true;this.monitoringLoading=true;
        const epoch=++this.monitoringLoadEpoch,signal=this.sessionController.signal;
        const current=()=>epoch===this.monitoringLoadEpoch&&!signal.aborted;
        try {
          const [hostResponse,componentResponse]=await Promise.all([
            axios.get('/api/v1/hosts',{signal,suppressToast:true}),
            axios.get('/api/v1/components',{signal,suppressToast:true})
          ]);
          if(!current())return;
          if(!Array.isArray(hostResponse.data.hosts)||!Array.isArray(componentResponse.data.components))throw new Error('invalid inventory');
          this.hosts=hostResponse.data.hosts;this.components=componentResponse.data.components;
          const options=this.monitoringPrometheusOptions;
          this.monitoringPrometheusID=options.length===1?options[0].id:null;
          this.monitoringRows=ids.map(id=>{
            const host=this.hosts.find(h=>h.id===id),linux=host && host.os!=='windows'&&host.connection_type!=='winrm';
            const exporters=this.components.filter(c=>c.host_id===id&&c.type==='node_exporter'&&activeComponent(c));
            return {host_id:id,name:host?.name||'主机 #'+id,ip:host?.ip||'',status:!host?'unknown':!linux?'unsupported':exporters.length===1?'reuse':exporters.length>1?'choose':'loading',
              exporters,component_id:linux&&exporters.length===1?exporters[0].id:null,stack_id:null,report:null,acceptance_error:'',message:!host?'主机不在当前列表，请刷新核对':!linux?'Windows 请使用现有单机 Windows Exporter 接入':'',preflight:null};
          });
          for(const row of this.monitoringRows.filter(r=>r.status==='loading')) {
            try {
              const response=await axios.get('/api/v1/hosts/'+row.host_id+'/detect',{signal,suppressToast:true});
              if(!current())return;
              if(!Array.isArray(response.data.components))throw new Error('invalid discovery');
              row.status=response.data.components.some(c=>c.type==='node_exporter')?'discovered':'missing';
              row.message=row.status==='discovered'?'已有运行实例，请先到发现接管核对，接管后重新选择主机。':'仅部署缺失的 Node Exporter';
            } catch(error) {
              if(!current())return;row.status='unknown';row.message='发现检查失败，不能确认组件缺失。请先到发现接管核对。';
            }
          }
          if(this.monitoringMissingRows.length) {
            try {
              const response=await axios.get('/api/v1/versions/by-type?type=node_exporter',{signal,suppressToast:true});
              if(!current())return;
              this.monitoringVersions=(response.data.versions||[]).filter(v=>v.package_available===true);
            } catch(error) { if(current())this.monitoringError='版本清单读取失败，请到软件包管理检查已入库版本。'; }
          } else this.monitoringVersions=[];
        } catch(error) { if(current())this.monitoringError='主机或组件清单读取失败，不能安全判断复用与部署；请重新打开。'; }
        finally { if(current())this.monitoringLoading=false; }
      },
      chooseMonitoringExporter(row) {
        const selected=row.exporters.find(c=>c.id===Number(row.component_id));
        row.component_id=selected?.id||null;row.status=selected?'reuse':'choose';this.invalidateMonitoringAcceptance();
      },
      monitoringDiscovery(row) {
        this.closeMonitoringOnboarding();this.discoveryHostId=row.host_id;this.domain='environment';this.environmentTab='discovery';this.loadDiscovery();
      },
      monitoringDeployStack() { this.closeMonitoringOnboarding();this.openDeployWizard(); },
      monitoringPayload(row) {
        const form=this.monitoringForm;
        return {host_id:row.host_id,type:'node_exporter',name:'node-exporter-'+row.host_id,deploy_type:'binary',version:form.version,port:Number(form.port),
          install_path:form.install_path,data_path_override:form.data_path_override,custom_args:form.custom_args,custom_env:form.custom_env,resource_limits:form.resource_limits,
          parameters:{version:form.version,port:Number(form.port),install_path:form.install_path,data_path_override:form.data_path_override,custom_args:form.custom_args,custom_env:form.custom_env,resource_limits:form.resource_limits},
          replace_existing:false};
      },
      monitoringFormValid() {
        const form=this.monitoringForm;
        if(!this.monitoringVersions.some(v=>v.version===form.version)){this.monitoringError='请选择已入库的 Node Exporter 版本。';return false;}
        if(!Number.isInteger(Number(form.port))||Number(form.port)<1||Number(form.port)>65535){this.monitoringError='端口必须为 1–65535 的整数。';return false;}
        for(const key of ['custom_env','resource_limits'])if(form[key]) {
          try { const value=JSON.parse(form[key]);if(!value||Array.isArray(value)||typeof value!=='object')throw new Error(); }
          catch(error){this.monitoringError='环境变量和资源限制必须为 JSON 对象。';return false;}
        }
        return true;
      },
      applyMonitoringStack(row,stack) {
        row.stack_id=stack.id;
        row.status=['pending','running','success','failed','interrupted'].includes(stack.status)?stack.status:'unknown';
        if(row.status==='interrupted')row.status='failed';
        row.message='持久任务 #'+stack.id+' · '+this.taskStatusLabel(stack.status);
        const exporters=(stack.steps||[]).filter(s=>s.type==='node_exporter'&&s.host_id!==undefined?Number(s.host_id)===row.host_id:s.type==='node_exporter');
        if(row.status==='success'&&exporters.length===1&&validID(exporters[0].component_id))row.component_id=exporters[0].component_id;
      },
      async submitMonitoringDeployments(retry=false) {
        if(!this.canChange||this.monitoringSubmitting||this.monitoringLoading||!this.monitoringDialog)return;
        if(!validID(Number(this.monitoringPrometheusID))){this.monitoringError='请明确选择用于验收的 Prometheus。';return;}
        const rows=this.monitoringRows.filter(r=>retry?r.status==='failed':r.status==='missing'&&!r.stack_id);
        if(!rows.length)return;
        if(rows.some(r=>!r.stack_id)&&!this.monitoringFormValid())return;
        this.monitoringSubmitting=true;this.monitoringError='';
        const signal=this.sessionController.signal;
        try {
          if(!await this.confirm(retry?'仅重试明确失败项；已接受任务按原 ID 重试。请输入 RETRY_STACK':'仅为缺失的 Linux 主机部署 Node Exporter，任务会持久保存。请输入 DEPLOY_STACK',retry?'RETRY_STACK':'DEPLOY_STACK'))return;
          for(const row of rows) {
            if(signal.aborted||!this.monitoringDialog)break;
            let creating=false;
            try {
              row.report=null;row.acceptance_error='';
              if(row.stack_id) {
                const response=await axios.post('/api/v1/deployments/stacks/'+row.stack_id+'/retry',{}, {signal,suppressToast:true});
                if(signal.aborted)return;
                if(!validID(response.data.stack?.id)||response.data.stack.id!==row.stack_id)throw new Error('invalid stack response');
                this.applyMonitoringStack(row,response.data.stack);
              } else {
                const payload=this.monitoringPayload(row);
                const preflight=await axios.post('/api/v1/components/preflight',payload,{signal,suppressToast:true});
                if(signal.aborted)return;
                row.preflight={passed:preflight.data.passed===true};
                if(!row.preflight.passed){row.status='failed';row.message='部署预检未通过；请检查版本、端口和高级参数，修正后显式重试。';continue;}
                if(!this.monitoringDialog)break;
                creating=true;
                const response=await axios.post('/api/v1/deployments/stacks',{name:'主机监控接入 · '+row.name,host_id:row.host_id,components:[payload]}, {signal,suppressToast:true});
                if(signal.aborted)return;
                if(!validID(response.data.stack?.id))throw new Error('invalid accepted stack');
                this.applyMonitoringStack(row,response.data.stack);
              }
            } catch(error) {
              if(signal.aborted)return;
              // A known stack always remains addressable, even if its retry response is lost.
              row.status=(!error.response&&creating)||error.response?.status>=500||row.stack_id?'unknown':'failed';
              row.message=this.monitoringErrorText(error);
            }
          }
          if(!this.monitoringRows.some(r=>r.status==='failed'&&!r.stack_id)) {
            this.monitoringForm.custom_args='';this.monitoringForm.custom_env='';this.monitoringForm.resource_limits='';
          }
        } finally { this.monitoringSubmitting=false; }
      },
      async refreshMonitoringTasks() {
        const signal=this.sessionController.signal;
        for(const row of this.monitoringRows.filter(r=>validID(r.stack_id))) {
          try {
            const response=await axios.get('/api/v1/deployments/stacks/'+row.stack_id,{signal,suppressToast:true});
            if(signal.aborted)return;
            if(response.data.stack?.id!==row.stack_id)throw new Error('invalid task');
            this.applyMonitoringStack(row,response.data.stack);
          } catch(error) { if(!signal.aborted)row.message='任务状态读取失败；已保存任务 #'+row.stack_id+'，请稍后刷新。'; }
        }
      },
      async acceptMonitoringRows() {
        if(this.monitoringAccepting||!validID(Number(this.monitoringPrometheusID)))return;
        this.invalidateMonitoringAcceptance();this.monitoringAccepting=true;
        const epoch=this.monitoringEpoch,prom=Number(this.monitoringPrometheusID),signal=this.sessionController.signal;
        try {
          for(const row of this.monitoringReusableRows) {
            const componentID=row.component_id;
            try {
              const response=await axios.get('/api/v1/hosts/'+row.host_id+'/monitoring-status',{params:{prometheus_id:prom,component_id:componentID},signal,suppressToast:true});
              if(epoch!==this.monitoringEpoch||signal.aborted)return;
              row.report=response.data;
            } catch(error) { if(epoch!==this.monitoringEpoch||signal.aborted)return;row.acceptance_error=this.monitoringErrorText(error); }
          }
        } finally { if(epoch===this.monitoringEpoch)this.monitoringAccepting=false; }
      },
      async runStackAcceptance() {
        if(!this.selectedStackRun||this.selectedStackRun.status!=='success')return;
        const id=this.selectedStackRun.id,prom=this.stackBoundPrometheusID||Number(this.stackAcceptancePrometheusID);
        if(!validID(prom)){this.stackAcceptanceError='请明确选择用于本任务验收的 Prometheus。';return;}
        this.invalidateStackAcceptance();this.stackAcceptanceLoading=true;
        const epoch=this.stackAcceptanceEpoch,signal=this.sessionController.signal;
        try {
          const response=await axios.get('/api/v1/deployments/stacks/'+id+'/acceptance',{params:{prometheus_id:prom},signal,suppressToast:true});
          if(epoch!==this.stackAcceptanceEpoch||signal.aborted||this.selectedStackRun?.id!==id)return;
          this.stackAcceptanceResult=response.data;
        } catch(error) { if(epoch===this.stackAcceptanceEpoch&&!signal.aborted)this.stackAcceptanceError=this.monitoringErrorText(error); }
        finally { if(epoch===this.stackAcceptanceEpoch)this.stackAcceptanceLoading=false; }
      }
    }
  };
}());
