package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// ========== COMPONENT ==========

func (h *Handler) ListComponents(c *gin.Context) {
	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点范围失败: " + err.Error()})
		return
	}

	var components []models.Component
	h.db.Where("cluster_id = ? AND status <> ?", nodeID, "removed").Preload("Host").Find(&components)
	for index := range components {
		components[index] = publicComponent(components[index])
	}
	c.JSON(http.StatusOK, gin.H{"components": components})
}

func (h *Handler) GetComponent(c *gin.Context) {
	raw, allowed := configRawRead(c)
	if !allowed {
		return
	}
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var component models.Component
	if err := h.db.Preload("Cluster").Preload("Host").Preload("Configs").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	if raw {
		c.JSON(http.StatusOK, component)
		return
	}
	c.JSON(http.StatusOK, publicComponent(component))
}

func (h *Handler) CreateComponent(c *gin.Context) {
	clusterID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点范围失败: " + err.Error()})
		return
	}

	var req componentCreateRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if encoded, err := json.Marshal(req); err != nil || utils.HasRedactedSecrets(string(encoded)) {
		c.JSON(400, gin.H{"error": "脱敏预览不能作为部署输入，请重新输入真实配置"})
		return
	}
	if req.PreflightToken == "" {
		req.PreflightToken = strings.TrimSpace(c.GetHeader("X-PromLifecycle-Preflight-Token"))
	}
	if !utils.SafePathSegment(req.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组件名称不能包含路径分隔符或特殊字符"})
		return
	}

	// Built-in types and enabled custom definitions share the same component
	// lifecycle. Custom definitions are snapshotted into Component.Config so a
	// later definition edit cannot change an existing deployment.
	var customDefinition *models.ComponentDefinition
	if !utils.IsValidComponentType(req.Type) {
		definition, err := FindEnabledComponentDefinition(h.db, req.Type)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "组件类型无效或已停用，请选择可用类型"})
			return
		}
		customDefinition = &definition
	}

	deploymentDefinition, err := h.normalizeDeploymentRequest(&req, customDefinition)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "部署参数校验失败: " + err.Error()})
		return
	}
	switch req.DeployType {
	case "binary", "windows", "target":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "deploy_type 无效，仅支持 binary、windows 或 target；平台不使用 Docker"})
		return
	}
	if req.DeployType == "binary" && customDefinition == nil && !deployer.IsBinarySupported(models.ComponentType(req.Type)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该组件类型不支持 binary 部署，请更换部署方式"})
		return
	}
	if customDefinition != nil && req.DeployType != "binary" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "自定义组件定义仅支持 binary 部署"})
		return
	}
	if req.DeployType == "target" && req.Type != "node_exporter" && req.Type != "blackbox_exporter" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target 模式仅支持 node_exporter 或 blackbox_exporter"})
		return
	}
	if req.DeployType == "windows" && req.Type != string(models.ComponentWindowsExporter) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "windows 部署方式仅支持 windows_exporter"})
		return
	}
	if req.Type == string(models.ComponentWindowsExporter) && req.DeployType != "windows" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "windows_exporter 必须使用 windows 部署方式"})
		return
	}
	if req.DeployType == "windows" && req.Port == 0 {
		req.Port = 9182
	}
	componentConfig := ""
	if customDefinition != nil {
		configJSON := "{}"
		if req.Config != nil {
			encoded, err := json.Marshal(req.Config)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "组件配置无效，请检查配置格式后重试"})
				return
			}
			configJSON = string(encoded)
		}
		componentConfig, err = models.EmbedComponentDefinitionSnapshot(configJSON, *customDefinition)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
			return
		}
		if req.Port == 0 {
			req.Port = customDefinition.DefaultPort
		}
	}
	if req.DeployType == "windows" {
		if req.Version == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "windows_exporter 必须选择已入库的 MSI 版本"})
			return
		}
		if req.Config != nil {
			encoded, marshalErr := json.Marshal(req.Config)
			if marshalErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Windows Exporter 配置无效"})
				return
			}
			componentConfig = string(encoded)
		}
	}
	if len(req.StructuredParameters) > 0 {
		baseConfig := componentConfig
		if baseConfig == "" && req.Config != nil {
			encoded, marshalErr := json.Marshal(req.Config)
			if marshalErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "组件配置无效"})
				return
			}
			baseConfig = string(encoded)
		}
		componentConfig, err = models.EmbedDeploymentParameters(baseConfig, req.StructuredParameters)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "保存结构化部署参数失败: " + err.Error()})
			return
		}
	}

	// Binary deployments default to the built-in version when none is specified
	if req.DeployType == "binary" && req.Version == "" {
		req.Version = deployer.DefaultVersion(req.Type)
	}
	// Versions become filesystem path segments (binary cache keys); a strict
	// charset keeps user input out of the path.
	if req.Version != "" && !utils.ValidVersion(req.Version) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的版本号（仅允许字母数字与 . - _，可选 v 前缀）"})
		return
	}

	// These paths are later used by mkdir/chown and uninstall. Validate them
	// separately so the user sees which field is unsafe and why.
	if req.DeployType != "windows" {
		if !utils.SafeInstallPath(req.InstallPath) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("安装路径 %q 不安全：请使用组件专用绝对目录，例如 /opt/promlifecycle/components/%s；禁止 /、/etc、/usr、/var 等系统目录以及 ..、空格和引号", req.InstallPath, req.Type)})
			return
		}
		if !utils.SafeDataPath(req.DataPathOverride) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("数据路径 %q 不安全：请使用专用绝对目录，例如 /var/lib/%s 或 /data/%s；禁止系统根目录以及 ..、空格和引号", req.DataPathOverride, req.Type, req.Type)})
			return
		}
	}

	// Custom binary launch args must be -flag=value or --flag=value lines
	// with a safe character set (they land in the systemd ExecStart line).
	for _, line := range strings.Split(req.CustomArgs, "\n") {
		a := strings.TrimSpace(line)
		if a == "" {
			continue
		}
		if len(a) < 2 || len(a) > 512 || !strings.HasPrefix(a, "-") || a == "-" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "启动参数必须是 -flag=value 或 --flag=value 格式（每行一个）"})
			return
		}
		for _, r := range a {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '-' || r == '_' || r == '=' || r == '.' || r == ':' || r == '/' || r == ',':
			default:
				c.JSON(http.StatusBadRequest, gin.H{"error": "启动参数包含非法字符（仅允许字母数字与 -=_.:/,）"})
				return
			}
		}
	}
	if err := validateCustomEnvironment(req.CustomEnv); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	// Resolve host: use provided HostID, or find one from the cluster, or auto-create local
	var host models.Host
	if req.HostID != nil && *req.HostID > 0 {
		if err := h.db.First(&host, *req.HostID).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "主机不存在，请刷新主机列表后重试"})
			return
		}
	} else {
		// Try to find a host associated with this cluster
		var cluster models.Cluster
		if err := h.db.First(&cluster, clusterID).Error; err == nil {
			var clusterHost models.Host
			if err := h.db.Where("ip = ?", cluster.Host).First(&clusterHost).Error; err == nil {
				host = clusterHost
			}
		}
		// If still no host, check for existing local host or auto-create
		if host.ID == 0 {
			if err := h.db.Where("ip = ?", "127.0.0.1").First(&host).Error; err != nil {
				host = models.Host{
					Name:           "local",
					IP:             "127.0.0.1",
					OS:             "linux",
					ConnectionType: "local",
					Port:           22,
					Username:       "root",
					Status:         "online",
					HasDocker:      false,
				}
				h.db.Create(&host)
			}
		}
	}
	if err := validateDeploymentHost(deploymentDefinition, &req, &host); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Linux components use the systemd binary deployer. Windows hosts use the
	// dedicated MSI/Windows Service deployer and currently support
	// windows_exporter only.
	if host.ConnectionType == "winrm" && req.DeployType != "windows" && req.DeployType != "target" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Windows (WinRM) 主机只支持 windows_exporter 部署或仅注册监控目标"})
		return
	}
	if req.DeployType == "windows" && (host.ConnectionType != "winrm" || host.OS != "windows") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "windows 部署方式需要 Windows WinRM 主机"})
		return
	}

	hostCtx, releaseHost, claimErr := services.ClaimHostOperation(c.Request.Context(), h.db, host.ID, "deploy")
	if claimErr != nil {
		c.JSON(http.StatusConflict, gin.H{"error": claimErr.Error()})
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			releaseHost()
		}
	}()
	c.Request = c.Request.WithContext(hostCtx)
	if err := h.db.First(&host, host.ID).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "主机已移除"})
		return
	}
	var existingComponent *models.Component
	var previousComponent *models.Component
	retryTarget, retryBound := c.Request.Context().Value(deploymentRetryTargetContextKey{}).(deploymentRetryTarget)
	if req.DeployType != "target" || retryBound {
		var existing models.Component
		query := h.db.Preload("Host").Preload("Cluster")
		if retryBound {
			query = query.Where("id = ?", retryTarget.ComponentID)
		} else {
			query = query.Where("host_id = ? AND type = ? AND deploy_type <> ? AND status <> ?", host.ID, req.Type, "target", "removed")
		}
		err := query.First(&existing).Error
		switch {
		case err == nil && !req.ReplaceExisting:
			c.JSON(http.StatusConflict, gin.H{"error": "该主机已有同类型受管组件；勾选“重新部署已有组件”后可覆盖部署"})
			return
		case err == nil && componentBusy(&existing):
			c.JSON(http.StatusConflict, gin.H{"error": "该组件正在执行生命周期操作，请完成后再重新部署"})
			return
		case err == nil:
			existingComponent = &existing
			previous := existing
			previousComponent = &previous
		case err != nil && err != gorm.ErrRecordNotFound:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "检查主机已管理组件失败，请稍后重试"})
			return
		}
	}
	var releaseComponent func()
	if existingComponent != nil {
		claimed, release, err := services.ClaimComponentOperation(c.Request.Context(), h.db, existingComponent.ID, "deploy")
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		releaseComponent = release
		c.Request = c.Request.WithContext(claimed)
		if err := h.db.Preload("Host").Preload("Cluster").First(existingComponent, existingComponent.ID).Error; err != nil {
			release()
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		previous := *existingComponent
		previousComponent = &previous
	}
	defer func() {
		if !handedOff && releaseComponent != nil {
			releaseComponent()
		}
	}()
	// A queued retry is bound to its original instance. Recheck after both claims
	// and the fresh component read, before consuming preflight or writing data.
	if retryBound {
		if err := retryTarget.validate(existingComponent); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
	}
	if err := validatePrometheusDeploymentParameterVersion(&req, existingComponent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.validateAndConsumeDeploymentPreflight(req, deploymentDefinition, customDefinition, host); err != nil {
		status := http.StatusPreconditionFailed
		if strings.Contains(err.Error(), "缺少部署预检凭证") {
			status = http.StatusPreconditionRequired
		}
		c.JSON(status, gin.H{"error": err.Error(), "preflight_required": true})
		return
	}
	if req.Type == string(models.ComponentGrafana) {
		existingEnv := ""
		if existingComponent != nil {
			existingEnv = existingComponent.CustomEnv
		} else {
			retainedComponent := &models.Component{Type: models.ComponentGrafana, Name: req.Name, Host: host}
			existingEnv, err = deployer.NewBinaryDeployer(h.db, h.basePath).ReadRetainedEnvironment(c.Request.Context(), retainedComponent)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "读取保留的 Grafana 密钥失败: " + err.Error()})
				return
			}
		}
		req.CustomEnv, err = grafanaDeploymentEnvironment(req.CustomEnv, existingEnv)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Grafana 环境变量无效: " + err.Error()})
			return
		}
	}

	// Platform-owned duplicates are rejected above. Host probing only blocks
	// the requested port; same-type external processes on other ports are
	// reported by onboarding discovery and do not prevent an isolated instance.
	if req.DeployType == "binary" && host.ID > 0 && !req.ReplaceExisting {
		probe := &models.Component{
			Type:   models.ComponentType(req.Type),
			Port:   req.Port,
			HostID: host.ID,
			Host:   host,
			Config: componentConfig,
		}
		bd := deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath())
		detectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		found, desc := bd.DetectComponent(detectCtx, probe)
		cancel()
		if found {
			c.JSON(http.StatusConflict, gin.H{
				"error": "目标主机已存在该组件：" + desc + "；如需更换版本请使用「升级」，或先卸载/删除现有组件后再部署",
			})
			return
		}
	}

	component := existingComponent
	if component == nil {
		component = &models.Component{
			ClusterID: clusterID,
			HostID:    host.ID,
		}
	}
	component.Type = models.ComponentType(req.Type)
	component.Name = req.Name
	component.Version = req.Version
	component.Config = componentConfig
	component.Port = req.Port
	component.DeployType = req.DeployType
	component.Status = models.StatusPending
	component.Error = ""
	if customDefinition != nil {
		component.Config = componentConfig
	}
	component.ResourceLimits = req.ResourceLimits
	component.CustomEnv = req.CustomEnv
	component.ExtraVolumes = req.ExtraVolumes
	if req.DataPathOverride != "" {
		component.DataPathOverride = req.DataPathOverride
	}
	if req.InstallPath != "" {
		component.InstallPath = req.InstallPath
	}
	component.CustomArgs = req.CustomArgs
	if req.Type == string(models.ComponentPrometheus) || req.Type == string(models.ComponentAlertmanager) {
		component.ExternalURL, _ = req.StructuredParameters["external_url"].(string)
	}
	if req.Type == string(models.ComponentGrafana) {
		component.ExternalURL, _ = req.StructuredParameters["root_url"].(string)
	}
	if req.Type == string(models.ComponentBlackbox) {
		component.ExternalURL, _ = req.StructuredParameters["external_url"].(string)
	}
	if req.Type == string(models.ComponentPushgateway) {
		component.ExternalURL, _ = req.StructuredParameters["external_url"].(string)
	}
	if req.DeployType == "binary" && !deployer.IsBinarySupportedComponent(component) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该组件定义不支持 binary 部署，请更换部署方式"})
		return
	}
	if component.ID == 0 {
		err = h.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(component).Error; err != nil {
				return err
			}
			claimed, release, err := services.ClaimComponentOperation(c.Request.Context(), tx, component.ID, "deploy")
			if err != nil {
				return err
			}
			releaseComponent = release
			c.Request = c.Request.WithContext(claimed)
			return nil
		})
	} else {
		err = h.db.Save(component).Error
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存组件部署记录失败: " + err.Error()})
		return
	}
	if existingComponent == nil {
		releaseComponent = services.ComponentOperationRelease(c.Request.Context(), h.db, component.ID)
	}

	// Load host for deployer
	h.db.Preload("Host").Preload("Cluster").First(component)

	// Start deployment
	op := h.createOperation(component.ID, &clusterID, string(models.OpDeploy), "pending", "api", nil)
	if op.ID == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存部署任务失败"})
		return
	}
	if err := h.linkStackDeployment(c.Request.Context(), component.ID, op.ID); err != nil {
		h.updateOperation(op.ID, false, "保存部署队列关联失败", err)
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	deploymentCtx := context.WithoutCancel(c.Request.Context())
	handedOff = true

	acceptedComponent := publicComponent(*component)
	go func() {
		ctx, cancel := context.WithTimeout(deploymentCtx, 10*time.Minute)
		defer cancel()
		defer releaseHost()
		defer releaseComponent()
		var err error

		// Serialize lifecycle operations: while a deployment is in flight,
		// uninstall/delete/upgrade are rejected instead of racing the
		// deployer goroutine and leaving an orphaned service on the host.
		h.db.Model(component).Update("status", models.StatusDeploying)

		h.updateOperationProgress(op.ID, "preparing", "准备目标主机、离线安装包和服务配置", 2)
		progress := func(stage, message string, percent int) {
			h.updateOperationProgress(op.ID, stage, message, percent)
		}

		switch component.DeployType {
		case "binary":
			bd := deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath()).WithProgress(progress)
			err = bd.Deploy(ctx, component)
		case "windows":
			wd := deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath()).WithProgress(progress)
			err = wd.Deploy(ctx, component)
		case "target":
			component.InternalURL = fmt.Sprintf("http://%s:%d", host.IP, component.Port)
			h.updateOperationProgress(op.ID, "integrating", "注册外部监控目标并同步 Prometheus", 80)
		default:
			err = fmt.Errorf("不支持的部署方式 %q", component.DeployType)
		}
		restored := false
		if err != nil && previousComponent != nil && (component.DeployType == "binary" || component.DeployType == "windows") {
			h.updateOperationProgress(op.ID, "rolling_back", "部署失败，正在恢复原版本与参数", 96)
			recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
			defer recoveryCancel()
			var restoreErr error
			if component.DeployType == "windows" {
				restoreErr = deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath()).Deploy(recoveryCtx, previousComponent)
			} else {
				restoreErr = deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath()).Deploy(recoveryCtx, previousComponent)
			}
			if restoreErr == nil {
				restored = true
				err = fmt.Errorf("%w（重新部署失败，已恢复原版本与参数）", err)
				previousComponent.Status = models.StatusRunning
				previousComponent.Error = err.Error()
				_ = h.db.Save(previousComponent).Error
			} else {
				err = fmt.Errorf("%w；恢复原部署也失败: %v", err, restoreErr)
			}
		}
		if err != nil {
			h.updateOperation(op.ID, false, "部署失败", err)
		}
		if err != nil {
			if !restored {
				h.db.Model(component).Updates(map[string]interface{}{
					"status":     models.StatusError,
					"error":      err.Error(),
					"updated_at": time.Now(),
				})
			}
			return
		}
		if output := h.collectDeployOutput(ctx, component); output != "" {
			h.db.Model(&models.Operation{}).Where("id = ?", op.ID).Update("output", output)
		}

		h.db.Model(component).Updates(map[string]interface{}{
			"status":           models.StatusRunning,
			"image":            component.Image,
			"compose_path":     component.ComposePath,
			"config_path":      component.ConfigPath,
			"data_path":        component.DataPath,
			"install_path":     component.InstallPath,
			"service_name":     component.ServiceName,
			"internal_url":     component.InternalURL,
			"last_deployed_at": time.Now(),
			"updated_at":       time.Now(),
		})

		// Generate and push the managed configuration immediately after a
		// successful deployment so the new exporter is observable before the
		// scheduler's next reconciliation cycle.
		h.updateOperationProgress(op.ID, "synchronizing", "同步 Prometheus、告警规则和可视化集成", 97)
		if integrationErr := h.syncDeployedComponent(ctx, component); integrationErr != nil {
			failure := fmt.Errorf("服务已部署，但集成同步失败，可重试集成: %w", integrationErr)
			h.updateOperation(op.ID, false, "部署未完成：集成失败", failure)
			h.db.Model(component).Update("error", failure.Error())
			h.db.Model(&models.Operation{}).Where("id = ?", op.ID).Update("stage", "integration_failed")
			return
		}
		releaseComponent()
		releaseHost()
		h.updateOperation(op.ID, true, "部署完成", nil)
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"component": acceptedComponent,
		"operation": op,
		"message":   "部署已开始",
	})
}

// collectDeployOutput fetches recent service logs for a just-deployed component
// so the operation record carries real deployment output for the UI.
func (h *Handler) collectDeployOutput(ctx context.Context, comp *models.Component) string {
	switch comp.DeployType {
	case "binary":
		unit := comp.ServiceName
		if unit == "" {
			return ""
		}
		if comp.Host.IP == "127.0.0.1" || comp.Host.IP == "localhost" || comp.Host.ConnectionType == "local" {
			if out, err := exec.CommandContext(ctx, "journalctl", "-u", unit, "-n", "40", "--no-pager").CombinedOutput(); err == nil {
				return strings.TrimSpace(string(out))
			}
			return ""
		}
		client, err := ssh.NewClient(comp.Host.IP, comp.Host.Port, comp.Host.Username,
			comp.Host.Password, comp.Host.PrivateKey)
		if err != nil {
			return ""
		}
		defer client.Close()
		if out, _, err := client.RunCommand("journalctl -u " + utils.QuoteShell(unit) + " -n 40 --no-pager"); err == nil {
			return strings.TrimSpace(out)
		}
		return ""
	case "windows":
		logs, err := deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath()).GetLogs(ctx, comp, 40)
		if err != nil {
			return ""
		}
		return strings.Join(logs, "\n")
	case "target":
		return "已注册为监控目标（未执行实际部署）"
	default:
		if comp.ServiceName == "" {
			return ""
		}
		if out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", "40", comp.ServiceName).CombinedOutput(); err == nil {
			return strings.TrimSpace(string(out))
		}
		return ""
	}
}

func (h *Handler) UpdateComponent(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, claimed := h.claimComponentRequest(c, id, "update")
	if !claimed {
		return
	}
	defer release()

	var req struct {
		Name             string  `json:"name"`
		Port             int     `json:"port"`
		CustomArgs       *string `json:"custom_args"` // nil = keep, "" = clear
		ResourceLimits   *string `json:"resource_limits"`
		CustomEnv        *string `json:"custom_env"`
		ExtraVolumes     *string `json:"extra_volumes"`
		DataPathOverride string  `json:"data_path_override"`
		InstallPath      string  `json:"install_path"`
		ApplyCustomArgs  bool    `json:"apply_custom_args"` // restart to apply binary args now
		DeployType       string  `json:"deploy_type"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}

	if componentBusy(&component) {
		c.JSON(http.StatusConflict, gin.H{"error": "组件正在部署/升级中，请等待操作完成后再修改"})
		return
	}

	changedArgs := false
	changedDeploy := false
	if req.Name != "" {
		if !utils.SafePathSegment(req.Name) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "组件名称不能包含路径分隔符或特殊字符"})
			return
		}
		component.Name = req.Name
	}
	if req.Port != 0 && req.Port != component.Port {
		component.Port = req.Port
		changedDeploy = true
	}
	if component.DeployType != "windows" {
		if !utils.SafeInstallPath(req.InstallPath) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("安装路径 %q 不安全：请使用组件专用绝对目录，例如 /opt/promlifecycle/components/%s；禁止系统目录、..、空格和引号", req.InstallPath, component.Type)})
			return
		}
		if !utils.SafeDataPath(req.DataPathOverride) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("数据路径 %q 不安全：请使用专用绝对目录，例如 /var/lib/%s 或 /data/%s；禁止系统根目录、..、空格和引号", req.DataPathOverride, component.Type, component.Type)})
			return
		}
	}
	if (req.InstallPath != "" && req.InstallPath != component.InstallPath) || (req.DataPathOverride != "" && req.DataPathOverride != component.DataPathOverride) {
		c.JSON(http.StatusConflict, gin.H{"error": "路径变更需要显式迁移；简单编辑不会移动安装文件或数据", "migration_required": true})
		return
	}
	if req.CustomArgs != nil && *req.CustomArgs != component.CustomArgs {
		for _, line := range strings.Split(*req.CustomArgs, "\n") {
			a := strings.TrimSpace(line)
			if a == "" {
				continue
			}
			if len(a) < 2 || len(a) > 512 || !strings.HasPrefix(a, "-") || a == "-" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "启动参数必须是 -flag=value 或 --flag=value 格式（每行一个）"})
				return
			}
			for _, r := range a {
				switch {
				case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				case r == '-' || r == '_' || r == '=' || r == '.' || r == ':' || r == '/' || r == ',':
				default:
					c.JSON(http.StatusBadRequest, gin.H{"error": "启动参数包含非法字符（仅允许字母数字与 -=_.:/,）"})
					return
				}
			}
		}
		component.CustomArgs = *req.CustomArgs
		changedArgs = true
	}
	if req.ResourceLimits != nil && *req.ResourceLimits != component.ResourceLimits {
		var limits struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		}
		if *req.ResourceLimits != "" {
			if err := json.Unmarshal([]byte(*req.ResourceLimits), &limits); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "资源限制必须是 JSON 格式 {cpu, memory}"})
				return
			}
		}
		component.ResourceLimits = *req.ResourceLimits
		changedDeploy = true
	}
	if req.CustomEnv != nil && *req.CustomEnv != component.CustomEnv {
		if err := validateCustomEnvironment(*req.CustomEnv); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
			return
		}
		component.CustomEnv = *req.CustomEnv
		changedDeploy = true
	}
	if req.ExtraVolumes != nil && *req.ExtraVolumes != component.ExtraVolumes {
		var vols []struct {
			Src string `json:"src"`
			Dst string `json:"dst"`
		}
		if *req.ExtraVolumes != "" {
			if err := json.Unmarshal([]byte(*req.ExtraVolumes), &vols); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "卷挂载必须是 JSON 数组 [{src, dst}]"})
				return
			}
		}
		component.ExtraVolumes = *req.ExtraVolumes
	}

	if req.CustomEnv != nil && utils.HasRedactedSecrets(*req.CustomEnv) {
		c.JSON(400, gin.H{"error": "脱敏环境变量不能作为编辑输入"})
		return
	}
	if (changedArgs || changedDeploy) && component.DeployType == "binary" {
		if err := deployer.ValidatePrometheusParameterVersion(&component, component.Version); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if err := h.db.Save(&component).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新组件失败"})
		return
	}

	// Apply binary changes immediately: regenerate the systemd unit (a plain
	// restart would keep the old ExecStart) and restart the service. Triggered
	// by launch args, port, resource limits or environment changes. Allowed for
	// running AND error components - editing parameters is the recovery path
	// for a component that failed to start.
	if (changedArgs || changedDeploy) && req.ApplyCustomArgs && !componentBusy(&component) && component.DeployType == "binary" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
		defer cancel()
		bd := deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath())
		if err := bd.ApplyUnit(ctx, &component); err != nil {
			h.db.Model(&component).Updates(map[string]interface{}{"status": models.StatusError, "error": err.Error()})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "已保存，但重启应用参数失败: " + err.Error()})
			return
		}
	}
	if changedDeploy && req.ApplyCustomArgs && !componentBusy(&component) && component.DeployType == "windows" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
		defer cancel()
		if err := deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath()).Deploy(ctx, &component); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "已保存，但重新应用 Windows Exporter 参数失败: " + err.Error()})
			return
		}
		if err := h.autoConfig.SyncPrometheusConfig(component.ClusterID, h.basePath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Windows Exporter 已更新，但 Prometheus 目标同步失败: " + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, publicComponent(component))
}

func (h *Handler) DeleteComponent(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, claimed := h.claimComponentRequest(c, id, "delete")
	if !claimed {
		return
	}
	defer release()

	// Default: delete the platform record only. Pass ?uninstall=1 (or use the
	// dedicated uninstall endpoint) to also stop the service and remove files.
	uninstall := c.Query("uninstall") == "1" || c.Query("uninstall") == "true"

	var component models.Component
	result := h.db.Preload("Host").Preload("Cluster").First(&component, id)
	if result.Error != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}

	if componentBusy(&component) {
		c.JSON(http.StatusConflict, gin.H{"error": "组件正在部署/升级中，请等待操作完成后再删除"})
		return
	}

	warn := ""
	if uninstall {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
		warn = h.uninstallComponentResources(ctx, &component)
		cancel()
	}

	if uninstall && warn != "" {
		// Same policy as the dedicated uninstall endpoint: keep the record
		// for retry instead of orphaning a still-running service.
		h.db.Model(&component).Updates(map[string]interface{}{"status": models.StatusError, "error": warn})
		c.JSON(http.StatusOK, gin.H{
			"message": "远端清理失败，组件记录已保留，可重试卸载: " + warn,
			"warning": warn,
		})
		return
	}

	if err := h.deleteComponentRecord(&component); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除组件记录失败: " + err.Error()})
		return
	}

	if uninstall {
		c.JSON(http.StatusOK, gin.H{"message": "组件已卸载，服务已停止且文件已删除"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "仅删除了平台记录，已部署服务未停止"})
}

var grafanaManagedEnvironmentKeys = []string{
	"GF_SERVER_DOMAIN", "GF_SERVER_ROOT_URL", "GF_SERVER_SERVE_FROM_SUB_PATH",
	"GF_ANALYTICS_REPORTING_ENABLED", "GF_ANALYTICS_CHECK_FOR_UPDATES",
	"GF_SECURITY_DISABLE_GRAVATAR", "GF_SECURITY_COOKIE_SECURE",
	"GF_USERS_ALLOW_SIGN_UP", "GF_USERS_ALLOW_ORG_CREATE", "GF_USERS_AUTO_ASSIGN_ORG_ROLE", "GF_USERS_DEFAULT_LANGUAGE",
	"GF_AUTH_ANONYMOUS_ENABLED", "GF_AUTH_ANONYMOUS_ORG_ROLE",
	"GF_DASHBOARDS_VERSIONS_TO_KEEP", "GF_DASHBOARDS_MIN_REFRESH_INTERVAL",
	"GF_DATE_FORMATS_DEFAULT_TIMEZONE", "GF_UNIFIED_ALERTING_ENABLED",
	"GF_LOG_MODE", "GF_LOG_LEVEL", "GF_LOG_FILE_MAX_DAYS",
}

func publicComponent(component models.Component) models.Component {
	if component.CustomArgs != "" {
		component.CustomArgs = utils.RedactedSecret
	}
	component.Configs = append([]models.ComponentConfig(nil), component.Configs...)
	component.Config = string(utils.SanitizeConfigJSON(json.RawMessage(component.Config)))
	for i := range component.Configs {
		component.Configs[i].Content = utils.SanitizeConfig(string(component.Type), component.Configs[i].ContentType, component.Configs[i].Content)
	}
	component.CustomEnv = string(utils.SanitizeConfigJSON(json.RawMessage(component.CustomEnv)))
	if component.Type != models.ComponentGrafana {
		return component
	}
	environment := map[string]string{}
	if json.Unmarshal([]byte(component.CustomEnv), &environment) != nil {
		component.CustomEnv = ""
		return component
	}
	for _, key := range grafanaManagedEnvironmentKeys {
		delete(environment, key)
	}
	for key := range environment {
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "TOKEN") ||
			strings.Contains(upper, "SECRET") || strings.Contains(upper, "KEY") {
			delete(environment, key)
		}
	}
	encoded, err := json.Marshal(environment)
	if err != nil || len(environment) == 0 {
		component.CustomEnv = ""
	} else {
		component.CustomEnv = string(encoded)
	}
	return component
}

func grafanaDeploymentEnvironment(raw, existingRaw string) (string, error) {
	env := map[string]string{}
	if strings.TrimSpace(existingRaw) != "" {
		if err := json.Unmarshal([]byte(existingRaw), &env); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(raw) != "" {
		var requested map[string]string
		if err := json.Unmarshal([]byte(raw), &requested); err != nil {
			return "", err
		}
		for key, value := range requested {
			env[key] = value
		}
	}
	if env["GF_SECURITY_SECRET_KEY"] == "" {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return "", fmt.Errorf("生成安全密钥失败: %w", err)
		}
		env["GF_SECURITY_SECRET_KEY"] = hex.EncodeToString(secret[:])
	}
	for _, key := range grafanaManagedEnvironmentKeys {
		delete(env, key)
	}
	data, err := json.Marshal(env)
	return string(data), err
}

func (h *Handler) ensureDefaultHostAlertRules(clusterID uint) error {
	defaults := []models.AlertRule{
		{Name: "NodeExporterDown", Expression: `up{component="node_exporter"} == 0`, Duration: "2m", Severity: "critical", Group: "infrastructure", Labels: `{"scope":"host"}`, Annotations: `{"summary":"Node Exporter 不可达","description":"主机 {{ $labels.instance }} 的 Node Exporter 已连续 2 分钟不可达"}`, Enabled: true},
		{Name: "HostHighCPU", Expression: `(1 - avg by(instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))) * 100 > 90`, Duration: "10m", Severity: "warning", Group: "infrastructure", Labels: `{"scope":"host"}`, Annotations: `{"summary":"主机 CPU 使用率过高","description":"主机 {{ $labels.instance }} CPU 使用率持续高于 90%"}`, Enabled: true},
		{Name: "HostHighMemory", Expression: `(1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) * 100 > 90`, Duration: "10m", Severity: "warning", Group: "infrastructure", Labels: `{"scope":"host"}`, Annotations: `{"summary":"主机内存使用率过高","description":"主机 {{ $labels.instance }} 内存使用率持续高于 90%"}`, Enabled: true},
		{Name: "HostDiskSpaceLow", Expression: `(node_filesystem_avail_bytes{fstype!~"tmpfs|overlay"} / node_filesystem_size_bytes{fstype!~"tmpfs|overlay"}) * 100 < 10`, Duration: "10m", Severity: "critical", Group: "infrastructure", Labels: `{"scope":"host"}`, Annotations: `{"summary":"主机磁盘剩余空间不足","description":"主机 {{ $labels.instance }} 挂载点 {{ $labels.mountpoint }} 剩余空间低于 10%"}`, Enabled: true},
	}
	for i := range defaults {
		defaults[i].ClusterID = clusterID
		var existing models.AlertRule
		err := h.db.Where("cluster_id = ? AND name = ?", clusterID, defaults[i].Name).First(&existing).Error
		if err == nil {
			continue
		}
		if err := h.db.Create(&defaults[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) ensureDefaultWindowsAlertRules(clusterID uint) error {
	defaults := []models.AlertRule{
		{Name: "WindowsExporterDown", Expression: `up{component="windows_exporter"} == 0`, Duration: "2m", Severity: "critical", Group: "windows", Labels: `{"scope":"windows_host"}`, Annotations: `{"summary":"Windows Exporter 不可达","description":"Windows 主机 {{ $labels.instance }} 已连续 2 分钟不可达"}`, Enabled: true},
		{Name: "WindowsHighCPU", Expression: `(1 - avg by(instance) (rate(windows_cpu_time_total{mode="idle"}[5m]))) * 100 > 90`, Duration: "10m", Severity: "warning", Group: "windows", Labels: `{"scope":"windows_host"}`, Annotations: `{"summary":"Windows CPU 使用率过高","description":"Windows 主机 {{ $labels.instance }} CPU 使用率持续高于 90%"}`, Enabled: true},
		{Name: "WindowsHighMemory", Expression: `(1 - windows_memory_available_bytes / windows_cs_physical_memory_bytes) * 100 > 90`, Duration: "10m", Severity: "warning", Group: "windows", Labels: `{"scope":"windows_host"}`, Annotations: `{"summary":"Windows 内存使用率过高","description":"Windows 主机 {{ $labels.instance }} 内存使用率持续高于 90%"}`, Enabled: true},
		{Name: "WindowsDiskSpaceLow", Expression: `(windows_logical_disk_free_bytes{volume!~"HarddiskVolume.*"} / windows_logical_disk_size_bytes{volume!~"HarddiskVolume.*"}) * 100 < 10`, Duration: "10m", Severity: "critical", Group: "windows", Labels: `{"scope":"windows_host"}`, Annotations: `{"summary":"Windows 磁盘剩余空间不足","description":"Windows 主机 {{ $labels.instance }} 磁盘 {{ $labels.volume }} 剩余空间低于 10%"}`, Enabled: true},
	}
	for i := range defaults {
		defaults[i].ClusterID = clusterID
		var existing models.AlertRule
		err := h.db.Where("cluster_id = ? AND name = ?", clusterID, defaults[i].Name).First(&existing).Error
		if err == nil {
			continue
		}
		if err := h.db.Create(&defaults[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

// InitDefaultHostAlerts ensures upgraded installations with existing
// Node Exporters receive the same infrastructure alert baseline as new
// deployments.
func (h *Handler) InitDefaultHostAlerts() error {
	var count int64
	if err := h.db.Model(&models.Component{}).Where("type = ? AND status <> ?", models.ComponentNodeExporter, "removed").Count(&count).Error; err != nil || count == 0 {
		return err
	}
	clusterID, err := services.SingleNodeID(h.db)
	if err != nil {
		return err
	}
	if err := h.ensureDefaultHostAlertRules(clusterID); err != nil {
		return err
	}
	go func() {
		if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
			log.Printf("[Startup] default host alert sync deferred/failed: %v", err)
		}
	}()
	return nil
}
