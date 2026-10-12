package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

const (
	inspectionConcurrency = 6
	inspectionItemTimeout = 30 * time.Second
	inspectionRunTimeout  = 2 * time.Minute
)

type InspectionRequest struct {
	TriggeredBy           string
	ScheduledInspectionID *uint
}

type InspectionExecution struct {
	Run               models.InspectionRun
	Checks            []models.InspectionCheck
	Notifications     []InspectionNotificationEvent
	NotificationError string
}

type InspectionService struct {
	db         *gorm.DB
	basePath   string
	factory    *deployer.Factory
	operations *OperationService
}

func NewInspectionService(db *gorm.DB, basePath string) *InspectionService {
	return &InspectionService{
		db: db, basePath: basePath, factory: deployer.NewFactory(basePath),
		operations: NewOperationService(db),
	}
}

func (s *InspectionService) Run(ctx context.Context, request InspectionRequest) (InspectionExecution, error) {
	ctx, release, claimErr := ClaimInspection(ctx, s.db)
	if claimErr != nil {
		return InspectionExecution{}, claimErr
	}
	defer release()
	runStarted := time.Now()
	if strings.TrimSpace(request.TriggeredBy) == "" {
		request.TriggeredBy = "system"
	}
	run := models.InspectionRun{
		Status: "running", Result: "unknown", TriggeredBy: request.TriggeredBy,
		ScheduledInspectionID: request.ScheduledInspectionID, StartedAt: runStarted,
	}
	if err := s.db.Create(&run).Error; err != nil {
		return InspectionExecution{}, fmt.Errorf("create inspection run: %w", err)
	}

	var components []models.Component
	if err := s.db.Preload("Host").Where("status <> ?", "removed").Order("id ASC").Find(&components).Error; err != nil {
		s.failRun(&run, err)
		return InspectionExecution{Run: run}, fmt.Errorf("load inspection components: %w", err)
	}

	runContext, cancel := context.WithTimeout(ctx, inspectionRunTimeout)
	defer cancel()
	checks := make([]models.InspectionCheck, len(components))
	operationIDs := make([]uint, len(components))
	for index := range components {
		operation := s.operations.Create(components[index].ID, &components[index].ClusterID, string(models.OpHealthCheck), "pending", nil)
		if operation.ID == 0 {
			err := fmt.Errorf("create health-check task for component %d", components[index].ID)
			s.failRun(&run, err)
			return InspectionExecution{Run: run}, err
		}
		operationIDs[index] = operation.ID
		s.operations.Progress(operation.ID, "preparing", "准备组件健康巡检", 10)
		s.operations.Progress(operation.ID, "verifying", "检查服务状态与健康端点", 60)
	}
	semaphore := make(chan struct{}, inspectionConcurrency)
	var waitGroup sync.WaitGroup
	for index := range components {
		index := index
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-runContext.Done():
				checks[index] = models.InspectionCheck{
					RunID: run.ID, OperationID: operationIDs[index],
					ComponentID: components[index].ID, Component: components[index].Name,
					ComponentType: string(components[index].Type), Host: components[index].Host.Name,
					DeployType: components[index].DeployType, Status: "failed",
					Message: "巡检等待超时: " + runContext.Err().Error(), CheckedAt: time.Now(),
				}
				return
			}
			checks[index] = s.inspectComponent(runContext, &components[index], operationIDs[index])
			checks[index].RunID = run.ID
		}()
	}
	waitGroup.Wait()

	passed := 0
	for index := range checks {
		check := &checks[index]
		if check.Status == "passed" {
			passed++
			s.operations.Progress(check.OperationID, "verifying", "服务与健康端点均已通过", 90)
			s.operations.Complete(check.OperationID, true, "巡检通过", nil)
		} else {
			s.operations.Complete(check.OperationID, false, "巡检失败", fmt.Errorf("%s", check.Message))
		}
		evidence, _ := json.Marshal(check)
		_ = s.db.Model(&models.Operation{}).Where("id = ?", check.OperationID).Update("output", string(evidence)).Error
	}
	completedAt := time.Now()
	run.Status = "completed"
	run.Result = "healthy"
	run.Total = len(checks)
	run.Passed = passed
	run.Failed = len(checks) - passed
	run.DurationMS = completedAt.Sub(runStarted).Milliseconds()
	run.CompletedAt = &completedAt
	if run.Failed > 0 {
		run.Result = "issues"
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if len(checks) > 0 {
			if err := tx.Create(&checks).Error; err != nil {
				return err
			}
		}
		return tx.Save(&run).Error
	}); err != nil {
		s.failRun(&run, err)
		return InspectionExecution{Run: run}, fmt.Errorf("save inspection evidence: %w", err)
	}
	notifications, notificationErr := NewInspectionNotificationService(s.db).Process(run, checks)
	notificationError := ""
	if notificationErr != nil {
		notificationError = notificationErr.Error()
		run.Error = "巡检通知状态处理失败: " + notificationError
		_ = s.db.Model(&run).Update("error", run.Error).Error
	}
	return InspectionExecution{
		Run: run, Checks: checks, Notifications: notifications, NotificationError: notificationError,
	}, nil
}

func (s *InspectionService) failRun(run *models.InspectionRun, runErr error) {
	completedAt := time.Now()
	run.Status = "failed"
	run.Result = "unknown"
	run.Error = runErr.Error()
	run.CompletedAt = &completedAt
	run.DurationMS = completedAt.Sub(run.StartedAt).Milliseconds()
	_ = s.db.Save(run).Error
}

func (s *InspectionService) inspectComponent(parent context.Context, component *models.Component, operationID uint) models.InspectionCheck {
	started := time.Now()
	checkedAt := time.Now()
	ctx, cancel := context.WithTimeout(parent, inspectionItemTimeout)
	defer cancel()

	endpoint := ComponentInspectionEndpoint(component)
	check := models.InspectionCheck{
		OperationID: operationID, ComponentID: component.ID, Component: component.Name,
		ComponentType: string(component.Type), Host: component.Host.Name,
		DeployType: component.DeployType, Status: "passed", Message: "服务与健康端点正常",
		Endpoint: endpoint, CheckedAt: checkedAt,
	}

	var err error
	if component.Status != models.StatusRunning && component.DeployType != "target" {
		err = fmt.Errorf("组件平台状态为 %s，不是 running", component.Status)
	} else {
		switch component.DeployType {
		case "binary":
			err = deployer.NewBinaryDeployer(s.db, s.basePath).HealthCheck(ctx, component)
		case "windows":
			err = deployer.NewWindowsDeployer(s.db, s.basePath).HealthCheck(ctx, component)
		case "target":
			err = inspectHTTPEndpoint(ctx, endpoint)
		default:
			_, err = s.factory.Deployer().HealthCheck(ctx, component)
		}
	}
	check.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		check.Status = "failed"
		check.Message = err.Error()
	}
	return check
}

func ComponentInspectionEndpoint(component *models.Component) string {
	if component == nil {
		return ""
	}
	base := strings.TrimRight(component.InternalURL, "/")
	if base == "" && component.Host.IP != "" && component.Port > 0 {
		base = "http://" + net.JoinHostPort(component.Host.IP, strconv.Itoa(component.Port))
	}
	path := models.ManagedHealthPath(component)
	return base + path
}

func inspectHTTPEndpoint(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("组件缺少可巡检健康端点")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("健康端点不可达: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return fmt.Errorf("健康端点返回 HTTP %d", response.StatusCode)
	}
	return nil
}
