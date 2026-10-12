package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"net/http"
	"time"
)

type stackStepContextKey struct{}

func (h *Handler) stackService() *services.DeploymentStackService {
	return services.NewDeploymentStackService(h.db, h.runStackStep)
}
func (h *Handler) StartDeploymentStacks(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); h.stackService().Run(ctx) }()
	return done
}

type deploymentStackRequest struct {
	Name        string                   `json:"name"`
	HostID      uint                     `json:"host_id"`
	TemplateKey string                   `json:"template_key"`
	Components  []componentCreateRequest `json:"components"`
}

type stackRequestError struct {
	Status  int
	Message string
	Checks  []deploymentPreflightCheck
}

func (h *Handler) CreateDeploymentStack(c *gin.Context) {
	var req deploymentStackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	stack, err := h.prepareDeploymentStack(req)
	if err != nil {
		c.JSON(err.Status, gin.H{"error": err.Message, "checks": err.Checks})
		return
	}
	if err := h.db.Create(stack).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"stack": stack})
}

func (h *Handler) prepareDeploymentStack(req deploymentStackRequest) (*models.DeploymentStack, *stackRequestError) {
	reject := func(message string) (*models.DeploymentStack, *stackRequestError) {
		return nil, &stackRequestError{Status: 400, Message: message}
	}
	if req.Name == "" || req.HostID == 0 || len(req.Components) == 0 || len(req.Components) > 64 {
		return reject("name、host_id 和 1–64 个组件为必填")
	}
	var host models.Host
	if err := h.db.First(&host, req.HostID).Error; err != nil {
		return reject("主机不存在")
	}
	plannedTypes := make([]string, 0, len(req.Components))
	plannedVersions := make(map[string]string, len(req.Components))
	for i := range req.Components {
		request := &req.Components[i]
		if request.Type == "" || !utils.SafePathSegment(request.Name) {
			return reject("组件类型和安全名称为必填")
		}
		if request.HostID != nil && *request.HostID != req.HostID {
			return reject("堆栈的所有组件必须使用同一主机")
		}
		if _, duplicate := plannedVersions[request.Type]; duplicate {
			return reject("部署计划包含重复组件类型，无法确定依赖实例: " + request.Type)
		}
		var custom *models.ComponentDefinition
		if !utils.IsValidComponentType(request.Type) {
			definition, err := FindEnabledComponentDefinition(h.db, request.Type)
			if err != nil {
				return reject("组件类型无效、已停用或没有部署定义")
			}
			custom = &definition
		}
		request.HostID = &req.HostID
		definition, err := h.normalizeDeploymentRequest(request, custom)
		if err != nil {
			return reject(request.Type + " 参数无效: " + err.Error())
		}
		if err := validateDeploymentHost(definition, request, &host); err != nil {
			return reject(request.Type + " 主机不兼容: " + err.Error())
		}
		plannedTypes = append(plannedTypes, request.Type)
		plannedVersions[request.Type] = request.Version
	}
	for i := range req.Components {
		request := &req.Components[i]
		request.PlannedComponents = plannedTypes
		request.PlannedVersions = plannedVersions
		var checks []deploymentPreflightCheck
		h.preflightDependencies(request, &host, &checks)
		for _, check := range checks {
			if check.Blocking && check.Status == "fail" {
				return nil, &stackRequestError{Status: 400, Message: request.Type + " 依赖预检失败: " + check.Message, Checks: checks}
			}
		}
	}
	stack := &models.DeploymentStack{Name: req.Name, HostID: req.HostID, TemplateKey: req.TemplateKey, Status: "pending", Total: len(req.Components)}
	for i, request := range req.Components {
		request.PreflightToken = ""
		request.PlannedComponents = nil
		request.PlannedVersions = nil
		raw, err := json.Marshal(request)
		if err != nil {
			return reject(err.Error())
		}
		encrypted, err := credentials.Encrypt(string(raw))
		if err != nil {
			return nil, &stackRequestError{Status: 500, Message: "无法加密部署队列"}
		}
		stack.Steps = append(stack.Steps, models.DeploymentStackStep{Position: i, Type: request.Type, Status: "pending", EncryptedRequest: encrypted})
	}
	return stack, nil
}

func (h *Handler) ListDeploymentStacks(c *gin.Context) {
	var stacks []models.DeploymentStack
	if err := h.db.Preload("Steps", func(tx *gorm.DB) *gorm.DB { return tx.Order("position ASC") }).Order("id DESC").Find(&stacks).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"stacks": stacks})
}
func (h *Handler) GetDeploymentStack(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	stack, err := h.stackService().Get(id)
	if err != nil {
		c.JSON(404, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"stack": stack})
}
func (h *Handler) RetryDeploymentStack(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error { return retryDeploymentStack(tx, id) }); err != nil {
		c.JSON(409, gin.H{"error": err.Error()})
		return
	}
	stack, err := h.stackService().Get(id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(202, gin.H{"stack": stack})
}

func (h *Handler) linkStackDeployment(ctx context.Context, componentID, operationID uint) error {
	stepID, _ := ctx.Value(stackStepContextKey{}).(uint)
	if stepID == 0 {
		return nil
	}
	result := h.db.Model(&models.DeploymentStackStep{}).Where("id = ? AND status = ?", stepID, "running").Updates(map[string]interface{}{"component_id": componentID, "operation_id": operationID})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("stack step is no longer running")
	}
	return nil
}

// invokeDeploymentHandler deliberately uses the same validation/admission path
// as interactive deployments. It performs no HTTP/network round trip and never
// manufactures a preflight token or bypasses host/package checks.
type deploymentResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *deploymentResponse) Header() http.Header    { return r.header }
func (r *deploymentResponse) WriteHeader(status int) { r.status = status }
func (r *deploymentResponse) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.body.Write(p)
}
func invokeDeploymentHandler(ctx context.Context, request componentCreateRequest, handler gin.HandlerFunc) (*deploymentResponse, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://internal/deployment", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response := &deploymentResponse{header: make(http.Header)}
	engine := gin.New()
	engine.POST("/deployment", handler)
	engine.ServeHTTP(response, req)
	if response.status < 200 || response.status >= 300 {
		return response, fmt.Errorf("deployment admission failed (%d)", response.status)
	}
	return response, nil
}
func (h *Handler) runStackStep(ctx context.Context, step *models.DeploymentStackStep) error {
	if step.Action == "remove" {
		return h.runRemovalStackStep(ctx, step)
	}
	if step.Action == "start" || step.Action == "stop" {
		return h.runPowerStackStep(ctx, step)
	}
	if step.Action == "adopt" {
		return h.runAdoptionStackStep(ctx, step)
	}
	if step.Action == "upgrade" {
		return h.runUpgradeStackStep(ctx, step)
	}
	if step.Action != "" {
		return errors.New("unknown queued lifecycle action")
	}
	plain, err := credentials.Decrypt(step.EncryptedRequest)
	if err != nil {
		return err
	}
	var request componentCreateRequest
	if err := json.Unmarshal([]byte(plain), &request); err != nil {
		return err
	}
	op, err := deploymentRetryOperation(h.db, step)
	if err != nil {
		return err
	}
	if op != nil {
		if op.Status == "success" {
			return nil
		}
		if op.Status == "running" || op.Status == "pending" {
			return h.waitStackOperation(ctx, op.ID)
		}
	}
	if step.ComponentID != nil {
		target, err := deploymentRetryTargetForStep(step, &request)
		if err != nil {
			return err
		}
		component, err := target.load(h.db)
		if err != nil {
			return err
		}
		if op != nil && op.Stage == "integration_failed" {
			claimed, release, err := services.ClaimComponentOperation(ctx, h.db, component.ID, "integration-retry")
			if err != nil {
				return err
			}
			defer release()
			// Reload after acquiring the claim: removal may have won the race.
			component, err = target.load(h.db)
			if err != nil {
				return err
			}
			if err := h.syncDeployedComponent(claimed, component); err != nil {
				return err
			}
			if err := h.db.Model(component).Update("error", "").Error; err != nil {
				return err
			}
			h.updateOperation(op.ID, true, "部署集成已重试完成", nil)
			return nil
		}
		request.ReplaceExisting = true
		ctx = context.WithValue(ctx, deploymentRetryTargetContextKey{}, target)
	}
	response, err := invokeDeploymentHandler(ctx, request, h.PreflightComponentDeployment)
	if err != nil {
		return err
	}
	var preflight struct {
		Passed bool                       `json:"passed"`
		Token  string                     `json:"preflight_token"`
		Checks []deploymentPreflightCheck `json:"checks"`
	}
	if err := json.Unmarshal(response.body.Bytes(), &preflight); err != nil {
		return err
	}
	if preflight.Token == "" {
		failedChecks := make([]string, 0)
		for _, check := range preflight.Checks {
			if check.Blocking && check.Status == "fail" {
				failedChecks = append(failedChecks, check.Key)
			}
		}
		return &services.StackPreflightFailure{Checks: failedChecks}
	}
	request.PreflightToken = preflight.Token
	response, err = invokeDeploymentHandler(context.WithValue(ctx, stackStepContextKey{}, step.ID), request, h.CreateComponent)
	if err != nil {
		return err
	}
	var accepted struct {
		Operation models.Operation `json:"operation"`
	}
	if err := json.Unmarshal(response.body.Bytes(), &accepted); err != nil {
		return err
	}
	if accepted.Operation.ID == 0 {
		return errors.New("deployment operation was not persisted")
	}
	return h.waitStackOperation(ctx, accepted.Operation.ID)
}
func (h *Handler) waitStackOperation(ctx context.Context, id uint) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var op models.Operation
		if err := h.db.First(&op, id).Error; err != nil {
			return err
		}
		switch op.Status {
		case "success":
			return nil
		case "failed", "interrupted":
			return errors.New("linked deployment did not complete")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (h *Handler) syncDeployedComponent(ctx context.Context, component *models.Component) error {
	clusterID := component.ClusterID
	switch component.Type {
	case models.ComponentPrometheus:
		if err := h.autoConfig.SyncPrometheusConfig(clusterID, h.basePath); err != nil {
			return err
		}
		if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
			return err
		}
	case models.ComponentAlertmanager:
		if err := h.autoConfig.SyncAlertmanagerConfig(clusterID, h.basePath); err != nil {
			return err
		}
	case models.ComponentNodeExporter:
		if err := h.ensureDefaultHostAlertRules(clusterID); err != nil {
			return err
		}
		if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
			return err
		}
	case models.ComponentWindowsExporter:
		if err := h.ensureDefaultWindowsAlertRules(clusterID); err != nil {
			return err
		}
		if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
			return err
		}
	case models.ComponentLoki:
		var grafana models.Component
		err := h.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentGrafana, models.StatusRunning).First(&grafana).Error
		if err == nil {
			if err := deployer.NewBinaryDeployer(h.db, h.basePath).SyncGrafanaDatasource(ctx, &grafana); err != nil {
				return err
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	if component.Type != models.ComponentPrometheus {
		return h.autoConfig.SyncPrometheusConfig(clusterID, h.basePath)
	}
	return nil
}

func (h *Handler) RetryComponentIntegration(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	release, ok := h.claimComponentRequest(c, id, "integration-retry")
	if !ok {
		return
	}
	defer release()
	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		c.JSON(404, gin.H{"error": err.Error()})
		return
	}
	var operation models.Operation
	if err := h.db.Where("component_id = ? AND stage = ?", id, "integration_failed").Order("id DESC").First(&operation).Error; err != nil {
		c.JSON(409, gin.H{"error": "没有可重试的部署集成失败"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	if err := h.syncDeployedComponent(ctx, &component); err != nil {
		c.JSON(500, gin.H{"error": "集成重试失败", "operation_id": operation.ID})
		return
	}
	if err := h.db.Model(&component).Update("error", "").Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	h.updateOperation(operation.ID, true, "部署集成已重试完成", nil)
	c.JSON(200, gin.H{"message": "部署集成已完成", "component": publicComponent(component), "operation_id": operation.ID})
}
