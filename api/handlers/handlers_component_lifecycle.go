package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// cleanupComponentFilesystem removes one component's on-disk artifacts
// (configs/compose/data subdirectories keyed by cluster + component name).
// Used by DeleteHost and component uninstall, where the cluster itself
// survives; empty parent directories are pruned best effort.
func (h *Handler) cleanupComponentFilesystem(comp *models.Component) {
	base := h.basePath
	if base == "" || comp.Cluster.Name == "" || comp.Name == "" {
		return
	}
	for _, clusterName := range nameVariants(comp.Cluster.Name) {
		for _, compName := range nameVariants(comp.Name) {
			for _, sub := range []string{"configs", "compose", "data"} {
				os.RemoveAll(filepath.Join(base, sub, clusterName, compName))
			}
		}
		// Compose files sit directly under compose/{cluster}/{name}.yaml.
		os.Remove(filepath.Join(base, "compose", clusterName, deployer.SafeName(comp.Name)+".yaml"))
		// Prune cluster dirs that are now empty.
		for _, sub := range []string{"configs", "compose", "data"} {
			os.Remove(filepath.Join(base, sub, clusterName))
		}
	}
}

// nameVariants returns the raw name plus its SafeName form when they differ.
// Raw names containing traversal segments are excluded so cleanup can never
// escape basePath.
func nameVariants(name string) []string {
	variants := []string{}
	if !strings.Contains(name, "..") {
		variants = append(variants, name)
	}
	if safe := deployer.SafeName(name); safe != name {
		variants = append(variants, safe)
	}
	return variants
}

// UninstallComponent stops the deployed service, removes its files and the
// platform record. It is the counterpart of DeleteComponent (record only).
func (h *Handler) UninstallComponent(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, claimed := h.claimComponentRequest(c, id, "uninstall")
	if !claimed {
		return
	}
	defer release()
	var component models.Component
	if err := h.db.First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	var req struct {
		Mode   string `json:"mode" binding:"required"`
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "移除必须包含 reason，并明确 mode=keep_service|keep_data|full_delete"})
		return
	}
	after, _ := json.Marshal(gin.H{"mode": req.Mode})
	svc := h.changeService()
	plan, err := svc.Create(c.Request.Context(), services.ChangePlanRequest{
		Kind: models.ChangeKindRemove, ResourceType: "component", ResourceID: id,
		Reason: req.Reason, After: after,
	}, changeActor(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	execCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
	defer cancel()
	plan, err = svc.Execute(execCtx, plan.ID, changeActor(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "plan": publicChangePlan(plan)})
		return
	}
	response := gin.H{"message": "组件移除已执行并验证", "plan": publicChangePlan(plan)}
	if component.Type != models.ComponentPrometheus {
		if syncErr := h.autoConfig.SyncPrometheusConfig(component.ClusterID, h.basePath); syncErr != nil {
			log.Printf("[Uninstall] prometheus config re-sync after %s removal failed: %v", component.Type, syncErr)
			response["integration_warning"] = "组件已移除，但 Prometheus 配置同步失败，请执行集成重新同步"
		}
	}
	if component.Type == models.ComponentLoki {
		var grafana models.Component
		if err := h.db.Preload("Host").
			Where("cluster_id = ? AND type = ? AND status = ?", component.ClusterID, models.ComponentGrafana, models.StatusRunning).
			First(&grafana).Error; err == nil {
			if syncErr := deployer.NewBinaryDeployer(h.db, h.basePath).SyncGrafanaDatasource(c.Request.Context(), &grafana); syncErr != nil {
				log.Printf("[Uninstall] Grafana datasource re-sync after Loki removal failed: %v", syncErr)
				response["grafana_warning"] = "Loki 已移除，但 Grafana 数据源同步失败，请执行集成重新同步"
			}
		}
	}
	c.JSON(http.StatusOK, response)
}

// uninstallComponentResources stops the deployed service and removes its files
// on the target host. A cleanup failure is returned so the caller can retain
// the platform record and let the operator retry safely.
func (h *Handler) uninstallComponentResources(ctx context.Context, component *models.Component) string {
	warn := ""
	if component.DeployType == "binary" {
		// SSH-deployed agent: stop service and remove files
		bd := deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath())
		if err := bd.Remove(ctx, component); err != nil {
			log.Printf("[DeleteComponent] binary cleanup warning: %v (proceeding)", err)
			warn = "远端清理失败（服务可能仍在运行）: " + err.Error()
		}
	} else if component.DeployType == "windows" {
		wd := deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath())
		if err := wd.Remove(ctx, component); err != nil {
			log.Printf("[DeleteComponent] Windows cleanup warning: %v (proceeding)", err)
			warn = "Windows 远端清理失败（服务可能仍在运行）: " + err.Error()
		}
	} else if component.DeployType == "target" {
		// Target-only registration: nothing to clean up on the host
	} else if component.ComposePath != "" {
		d := h.deployerFactory.Deployer()
		if err := d.Delete(ctx, component); err != nil {
			log.Printf("[DeleteComponent] docker cleanup warning: %v (proceeding)", err)
			warn = "容器清理失败: " + err.Error()
		}
		// Also force-remove container by service name
		if component.ServiceName != "" {
			exec.CommandContext(ctx, "docker", "rm", "-f", component.ServiceName).Run()
		}
	}
	// Remove the platform-side config/compose/data directories for this
	// component; the cluster directories survive for remaining components.
	h.cleanupComponentFilesystem(component)
	return warn
}

// deleteComponentRecord removes the component row and its owned child records.
func (h *Handler) deleteComponentRecord(component *models.Component) error {
	// Cascade: clean up component-owned records
	h.db.Where("component_id = ?", component.ID).Delete(&models.ComponentConfig{})
	h.db.Where("component_id = ?", component.ID).Delete(&models.ResourceUsage{})
	h.db.Where("component_id = ?", component.ID).Delete(&models.Operation{})
	h.db.Where("component_id = ?", component.ID).Delete(&models.Backup{})
	h.db.Where("component_id = ?", component.ID).Delete(&models.ScheduledBackup{})
	h.db.Where("component_id = ?", component.ID).Delete(&models.ComponentVersion{})
	h.db.Where("component_id = ?", component.ID).Delete(&models.MaintenanceWindow{})
	h.db.Where("component_id = ?", component.ID).Delete(&models.InspectionNotificationState{})
	h.db.Where("source_id = ? OR target_id = ?", component.ID, component.ID).Delete(&models.TopologyEdge{})
	// Children were deleted above, so the component row can be removed with
	// foreign keys enabled; the connection-scoped PRAGMA would be unreliable
	// across GORM's connection pool anyway.
	result := h.db.Exec("DELETE FROM components WHERE id = ?", component.ID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ========== COMPONENT ACTIONS ==========

func (h *Handler) RestartComponent(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, claimed := h.claimComponentRequest(c, id, "restart")
	if !claimed {
		return
	}
	defer release()

	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}

	if componentBusy(&component) {
		c.JSON(http.StatusConflict, gin.H{"error": "组件正在部署/升级中，请等待操作完成后再操作"})
		return
	}

	if component.DeployType == "target" {
		c.JSON(http.StatusOK, gin.H{"message": "target 模式组件无需重启"})
		return
	}
	op := h.createOperation(component.ID, &component.ClusterID, string(models.OpRestart), "pending", "api", nil)
	if op.ID == 0 {
		c.JSON(500, gin.H{"error": "保存重启任务失败"})
		return
	}
	h.updateOperationProgress(op.ID, "preparing", "准备重启组件服务", 10)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	h.updateOperationProgress(op.ID, "service_control", "正在重启组件服务", 50)
	var err error
	switch component.DeployType {
	case "binary":
		err = deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath()).Restart(ctx, &component)
	case "windows":
		err = deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath()).Restart(ctx, &component)
	default:
		err = h.deployerFactory.Deployer().Restart(ctx, &component)
	}
	if err != nil {
		h.updateOperation(op.ID, false, "重启失败", err)
		h.db.Model(&component).Updates(map[string]interface{}{"status": models.StatusError, "error": err.Error()})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重启组件失败: " + err.Error(), "operation_id": op.ID})
		return
	}
	h.updateOperationProgress(op.ID, "verifying", "组件服务已重启并通过状态验证", 90)
	if err := persistComponentPowerState(h.db, &component, models.StatusRunning); err != nil {
		h.updateOperation(op.ID, false, "重启状态保存失败", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "组件已重启但状态保存失败", "operation_id": op.ID})
		return
	}
	h.updateOperation(op.ID, true, "组件已重启", nil)
	_ = h.db.First(op, op.ID).Error
	c.JSON(http.StatusOK, gin.H{"message": "组件已重启", "status": models.StatusRunning, "operation": op})
}

func persistComponentPowerState(db *gorm.DB, component *models.Component, status models.ComponentStatus) error {
	return db.Model(component).Updates(map[string]interface{}{"status": status, "error": ""}).Error
}

func (h *Handler) StartComponent(c *gin.Context) {
	h.setComponentPower(c, true)
}

func (h *Handler) StopComponent(c *gin.Context) {
	h.setComponentPower(c, false)
}

func (h *Handler) setComponentPower(c *gin.Context, start bool) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, claimed := h.claimComponentRequest(c, id, "power")
	if !claimed {
		return
	}
	defer release()
	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	if componentBusy(&component) {
		c.JSON(http.StatusConflict, gin.H{"error": "组件正在部署/升级中，请等待操作完成后再操作"})
		return
	}
	if component.DeployType != "binary" && component.DeployType != "windows" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只有受管 Linux binary 或 Windows 服务支持启停"})
		return
	}
	opType := models.OpStop
	action := "停止"
	if start {
		opType = models.OpStart
		action = "启动"
	}
	op, prepareErr := h.preparePowerOperation(c.Request.Context(), &component, string(opType))
	if prepareErr != nil {
		c.JSON(409, gin.H{"error": prepareErr.Error()})
		return
	}
	if op.ID == 0 {
		c.JSON(500, gin.H{"error": "保存启停任务失败"})
		return
	}
	h.updateOperationProgress(op.ID, "preparing", "准备"+action+"组件服务", 10)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	var err error
	status := models.StatusStopped
	h.updateOperationProgress(op.ID, "service_control", "正在"+action+"组件服务", 50)
	if component.DeployType == "windows" {
		windowsDeployer := deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath())
		if start {
			err = windowsDeployer.Start(ctx, &component)
			status, action = models.StatusRunning, "启动"
		} else {
			err = windowsDeployer.Stop(ctx, &component)
		}
	} else {
		binaryDeployer := deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath())
		if start {
			err = binaryDeployer.Start(ctx, &component)
			status, action = models.StatusRunning, "启动"
		} else {
			err = binaryDeployer.Stop(ctx, &component)
		}
	}
	if err != nil {
		h.updateOperation(op.ID, false, action+"失败", err)
		_ = h.db.Model(&component).Updates(map[string]interface{}{"status": models.StatusError, "error": err.Error()}).Error
		c.JSON(http.StatusInternalServerError, gin.H{"error": action + "组件失败: " + err.Error(), "operation_id": op.ID})
		return
	}
	h.updateOperationProgress(op.ID, "verifying", "服务状态已更新，正在保存平台状态", 90)
	if err := h.completePowerOperation(c.Request.Context(), &component, op, status, action); err != nil {
		h.updateOperation(op.ID, false, action+"状态保存失败", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": action + "成功但状态保存失败", "operation_id": op.ID})
		return
	}
	_ = h.db.First(op, op.ID).Error
	c.JSON(http.StatusOK, gin.H{"message": "组件已" + action, "status": status, "operation": op})
}

func (h *Handler) UpgradeComponent(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, claimed := h.claimComponentRequest(c, id, "upgrade")
	if !claimed {
		return
	}
	defer release()
	var req struct {
		NewVersion  string `json:"new_version" binding:"required"`
		TargetImage string `json:"target_image"`
		Reason      string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "升级必须包含 new_version 和 reason: " + err.Error()})
		return
	}
	after, _ := json.Marshal(gin.H{"new_version": req.NewVersion, "target_image": req.TargetImage})
	svc := h.changeService()
	plan, err := svc.Create(c.Request.Context(), services.ChangePlanRequest{
		Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: id,
		Reason: req.Reason, After: after,
	}, changeActor(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	execCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
	defer cancel()
	plan, err = svc.Execute(execCtx, plan.ID, changeActor(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "plan": publicChangePlan(plan)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "升级已执行并验证", "version": req.NewVersion, "plan": publicChangePlan(plan)})
}

func (h *Handler) RollbackComponent(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, claimed := h.claimComponentRequest(c, id, "rollback")
	if !claimed {
		return
	}
	defer release()
	var req struct {
		TargetVersion string `json:"target_version"`
		TargetImage   string `json:"target_image"`
		Reason        string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "回滚必须包含 reason: " + err.Error()})
		return
	}
	var component models.Component
	if err := h.db.First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	if req.TargetVersion == "" {
		var previous models.ComponentVersion
		if err := h.db.Where("component_id = ? AND version <> ?", id, component.Version).
			Order("created_at DESC").First(&previous).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有可回滚的历史版本"})
			return
		}
		req.TargetVersion = previous.Version
		if req.TargetImage == "" {
			req.TargetImage = previous.Image
		}
	}
	after, _ := json.Marshal(gin.H{"new_version": req.TargetVersion, "target_image": req.TargetImage})
	svc := h.changeService()
	plan, err := svc.Create(c.Request.Context(), services.ChangePlanRequest{
		Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: id,
		Reason: req.Reason, After: after,
	}, changeActor(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	execCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
	defer cancel()
	plan, err = svc.Execute(execCtx, plan.ID, changeActor(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "plan": publicChangePlan(plan)})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "版本已回滚并验证", "version": req.TargetVersion, "previous_version": component.Version, "plan": publicChangePlan(plan),
	})
}

func (h *Handler) HealthCheck(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if component.DeployType == "windows" {
		if err := deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath()).HealthCheck(ctx, &component); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Windows Exporter 健康检查失败: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"healthy": true, "status": "running", "service": "windows_exporter", "endpoint": component.InternalURL + "/metrics"})
		return
	}
	d := h.deployerFactory.Deployer()
	health, err := d.HealthCheck(ctx, &component)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, health)
}

func (h *Handler) GetLogs(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if component.DeployType == "binary" {
		bd := deployer.NewBinaryDeployer(h.db, h.deployerFactory.BasePath())
		logs, err := bd.GetLogs(ctx, &component, 100)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "操作失败: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"logs": logs})
		return
	}
	if component.DeployType == "windows" {
		logs, err := deployer.NewWindowsDeployer(h.db, h.deployerFactory.BasePath()).GetLogs(ctx, &component, 100)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 Windows Exporter 日志失败: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"logs": logs})
		return
	}
	if component.DeployType == "target" {
		c.JSON(http.StatusOK, gin.H{"logs": []string{}, "message": "target 模式组件无日志"})
		return
	}
	d := h.deployerFactory.Deployer()
	logs, err := d.GetLogs(ctx, &component, 100)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"logs": logs})
}
