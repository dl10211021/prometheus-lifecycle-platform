package router

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/api/handlers"
	"github.com/promlifecycle/platform/api/middleware"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/scheduler"
	winrmclient "github.com/promlifecycle/platform/core/winrm"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"log"
	"net/http"
	"os"
	"strings"
)

type SetupResult struct {
	Engine       *gin.Engine
	Scheduler    *scheduler.Scheduler
	cancelStacks context.CancelFunc
	stacksDone   <-chan struct{}
}

func (s *SetupResult) Stop() {
	s.cancelStacks()
	<-s.stacksDone
	s.Scheduler.Stop()
}

func Setup(db *gorm.DB, deployerFactory *deployer.Factory, configManager *config.Manager, basePath string) *SetupResult {
	gin.SetMode(gin.ReleaseMode)
	alertSync := services.NewAlertSyncService(db, basePath)

	// Initialize AutoConfig for Prometheus config management
	autoConfig := config.NewAutoConfig(db, basePath)

	handler := handlers.NewHandler(db, deployerFactory, configManager, alertSync, autoConfig, basePath)
	// Initialize default component versions
	if err := handler.InitDefaultVersions(); err != nil {
		log.Printf("warning: failed to init default versions: %v", err)
	}
	if err := handler.InitDefaultHostAlerts(); err != nil {
		log.Printf("warning: failed to init default host alerts: %v", err)
	}
	opsHandler := handlers.NewOperationsHandler(db)
	alertsHandler := handlers.NewAlertsHandler(db, alertSync)
	notificationHandler := handlers.NewNotificationHandler(db, autoConfig, basePath)
	opsOpsHandler := handlers.NewOpsHandler(db, basePath)
	templateHandler := handlers.NewTemplateHandler(db)

	// Load default templates on startup
	if err := templateHandler.EnsureDefaults(); err != nil {
		fmt.Printf("warning: failed to load default templates: %v\n", err)
	}

	// Offline binary package handler
	packageHandler := handlers.NewPackageHandler(db, basePath)

	// Scrape configs handler for Prometheus scrape configuration management
	scrapeConfigsHandler := handlers.NewScrapeConfigsHandler(db, autoConfig, basePath)

	// Recording rules handler for Prometheus recording rule management
	recordingRulesHandler := handlers.NewRecordingRulesHandler(db, alertSync)
	// Remote write handler for Prometheus remote write configuration management
	remoteWriteHandler := handlers.NewRemoteWriteHandler(db, autoConfig, basePath)
	// Alertmanager proxy handler for proxying requests to Alertmanager API
	alertmanagerProxyHandler := handlers.NewAlertmanagerProxyHandler(db)
	alertmanagerSettingsHandler := handlers.NewAlertmanagerSettingsHandler(db, autoConfig, basePath)

	// Alert silences handler for CRUD operations on Alertmanager silences
	silenceHandler := handlers.NewAlertSilenceHandler(db)

	// Dashboard handler for Grafana dashboard management and provisioning
	dashboardHandler := handlers.NewDashboardHandler(db, autoConfig, basePath)
	componentDefinitionHandler := handlers.NewComponentDefinitionHandler(db)
	aiHandler := handlers.NewAIHandler(db)
	incidentsHandler := handlers.NewIncidentsHandler(db)
	auditExportHandler := handlers.NewAuditExportHandler(db)
	backupService := services.NewBackupService(db)
	changeService := services.NewChangeService(db, deployerFactory, configManager, backupService, basePath)
	changePlanHandler := handlers.NewChangePlanHandler(changeService)
	pairedRecoveryHandler := handlers.NewPairedRecoveryHandler(backupService)
	recoveryExercisesHandler := handlers.NewRecoveryExercisesHandler(db)

	// Config validation handler for Prometheus and Alertmanager config validation
	configValidateHandler := &handlers.ConfigValidateHandler{}

	r := gin.New()
	// Never trust X-Forwarded-For from arbitrary clients: the login rate
	// limiter and audit IPs must reflect the real peer, otherwise the limit
	// is trivially bypassed by spoofing the header (no reverse proxy in the
	// default single-binary deployment; if one is added later, configure
	// SetTrustedProxies with its addresses explicitly).
	_ = r.SetTrustedProxies(nil)
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.CORS())
	// Management console: never let browsers keep stale HTML/JS (the SPA is
	// embedded in the binary, so a cached copy would silently run old code).
	r.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Next()
	})

	// Static assets for frontend: optional external override, otherwise the
	// embedded copy (so the SPA works regardless of the working directory).
	assetsPath := os.Getenv("PROMLIFECYCLE_ASSETS_PATH")
	if assetsPath != "" {
		if _, err := os.Stat(assetsPath); err == nil {
			r.Static("/assets", assetsPath)
		} else {
			log.Printf("warning: PROMLIFECYCLE_ASSETS_PATH %s not found, using embedded assets", assetsPath)
			r.StaticFS("/assets", http.FS(webAssetsFS()))
		}
	} else {
		r.StaticFS("/assets", http.FS(webAssetsFS()))
	}

	// Health check
	api := r.Group("/api/v1")
	api.GET("/winrm-transfers/:token", func(c *gin.Context) {
		winrmclient.ServeFileTransfer(c.Writer, c.Request, c.Param("token"))
	})
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	// Auth
	auth := api.Group("/auth")
	auth.POST("/login", middleware.Login(db))
	auth.GET("/me", middleware.AuthRequired(db), middleware.Me)
	auth.PUT("/password", middleware.AuthRequired(db), middleware.AuditLog(db), middleware.ChangePassword(db))

	// Every authenticated endpoint first rejects unknown/legacy roles. More
	// sensitive groups below narrow this read baseline by explicit capability.
	protected := api.Group("")
	protected.Use(middleware.AuthRequired(db))
	protected.Use(middleware.SingleNodeScope(db))
	protected.Use(middleware.RequireRole("viewer", "operator", "change_admin", "system_admin", "auditor"))

	operator := protected.Group("")
	operator.Use(middleware.AuditLog(db))
	operator.Use(middleware.RequireRole("operator", "change_admin", "system_admin"))

	change := protected.Group("")
	change.Use(middleware.AuditLog(db))
	change.Use(middleware.RequireRole("change_admin", "system_admin"))

	system := protected.Group("")
	system.Use(middleware.AuditLog(db))
	protected.POST("/ai/chat", middleware.AuditLog(db), aiHandler.Chat)
	protected.GET("/incidents", incidentsHandler.List)
	protected.GET("/incidents/:id", incidentsHandler.Get)
	system.Use(middleware.RequireRole("system_admin"))

	// System administration: platform-wide settings, extension inventories,
	// identity, and platform snapshot material.
	system.PUT("/node", handler.UpdateNode)
	system.PUT("/ai/config", aiHandler.PutConfig)
	system.POST("/ai/test", aiHandler.Test)
	system.POST("/component-definitions", componentDefinitionHandler.CreateComponentDefinition)
	system.PUT("/component-definitions/:id", componentDefinitionHandler.UpdateComponentDefinition)
	system.DELETE("/component-definitions/:id", componentDefinitionHandler.DeleteComponentDefinition)
	system.POST("/templates", templateHandler.CreateTemplate)
	system.PUT("/templates/:id", templateHandler.UpdateTemplate)
	system.DELETE("/templates/:id", templateHandler.DeleteTemplate)
	system.POST("/templates/upload", templateHandler.UploadTemplate)
	system.POST("/templates/:id/render", templateHandler.RenderTemplate)
	system.POST("/packages", packageHandler.CreatePackage)
	system.DELETE("/packages/:id", packageHandler.DeletePackage)
	// Scope parsing consumes multipart bodies, so authorize the upload and set
	// its bounded transport/body limits before entering SingleNodeScope.
	api.POST("/packages/upload",
		middleware.AuthRequired(db),
		middleware.AuditLog(db),
		middleware.RequireRole("system_admin"),
		packageUploadLimits(handlers.MaxUploadSize+(1<<20)),
		middleware.SingleNodeScope(db),
		packageHandler.UploadImage,
	)
	system.POST("/packages/official-download", packageHandler.DownloadOfficial)
	system.POST("/packages/download-all", packageHandler.DownloadAllOfficial)
	system.POST("/backups/platform", opsOpsHandler.CreatePlatformBackup)
	system.GET("/backups/platform/download", opsOpsHandler.DownloadPlatformBackup)
	system.POST("/backups/platform/restore", opsOpsHandler.RestorePlatformBackup)
	system.POST("/notifications/channels", notificationHandler.CreateChannel)
	system.PUT("/notifications/channels/:id", notificationHandler.UpdateChannel)
	system.DELETE("/notifications/channels/:id", notificationHandler.DeleteChannel)
	system.PUT("/inspections/notification-policy", handler.UpdateInspectionNotificationPolicy)
	system.POST("/notifications/channels/:id/test", notificationHandler.TestChannel)
	system.POST("/notifications/deliveries/:id/resolve", notificationHandler.ResolveDelivery)
	system.POST("/alertmanager/routes", alertmanagerSettingsHandler.CreateAlertmanagerRoute)
	system.PUT("/alertmanager/routes/:id", alertmanagerSettingsHandler.UpdateAlertmanagerRoute)
	system.DELETE("/alertmanager/routes/:id", alertmanagerSettingsHandler.DeleteAlertmanagerRoute)
	system.PUT("/alertmanager/settings", alertmanagerSettingsHandler.UpdateAlertmanagerSettings)
	system.POST("/versions", handler.CreateVersion)
	system.PUT("/versions/:id", handler.UpdateVersion)
	system.DELETE("/versions/:id", handler.DeleteVersion)

	// Planned and direct component/configuration changes. Direct legacy routes
	// retain the same change-admin boundary as the safer change-plan workflow.
	change.POST("/deployments/batches", handler.CreateDeploymentBatch)
	change.POST("/deployments/batches/preflight", handler.PreflightUpgradeBatch)
	change.POST("/deployments/batches/:id/retry", handler.RetryDeploymentBatch)
	change.POST("/deployments/stacks", handler.CreateDeploymentStack)
	change.POST("/deployments/stacks/:id/retry", handler.RetryDeploymentStack)
	change.POST("/components/:id/integration/retry", handlers.RequireManagedAdoption(db), handler.RetryComponentIntegration)
	change.POST("/backups", opsHandler.CreateBackup)
	change.POST("/backups/:id/verify", opsHandler.VerifyBackup)
	change.POST("/backups/:id/restore", opsHandler.RestoreBackup)
	change.DELETE("/backups/:id", opsHandler.DeleteBackup)
	change.POST("/backups/scheduled", opsOpsHandler.CreateScheduledBackup)
	change.PUT("/backups/scheduled/:id", opsOpsHandler.UpdateScheduledBackup)
	change.DELETE("/backups/scheduled/:id", opsOpsHandler.DeleteScheduledBackup)
	change.POST("/alerts/rules", alertsHandler.CreateAlertRule)
	change.PUT("/alerts/rules/:id", alertsHandler.UpdateAlertRule)
	change.DELETE("/alerts/rules/:id", alertsHandler.DeleteAlertRule)
	change.POST("/scrape-configs/sync", scrapeConfigsHandler.SyncPrometheusConfig)
	change.POST("/scrape-configs", scrapeConfigsHandler.CreateScrapeConfig)
	change.PUT("/scrape-configs/:id", scrapeConfigsHandler.UpdateScrapeConfig)
	change.DELETE("/scrape-configs/:id", scrapeConfigsHandler.DeleteScrapeConfig)
	change.POST("/recording-rules", recordingRulesHandler.CreateRecordingRule)
	change.PUT("/recording-rules/:id", recordingRulesHandler.UpdateRecordingRule)
	change.DELETE("/recording-rules/:id", recordingRulesHandler.DeleteRecordingRule)
	change.POST("/remote-writes", remoteWriteHandler.Create)
	change.PUT("/remote-writes/:id", remoteWriteHandler.Update)
	change.DELETE("/remote-writes/:id", remoteWriteHandler.Delete)
	change.POST("/dashboards", dashboardHandler.CreateDashboard)
	change.PUT("/dashboards/:id", dashboardHandler.UpdateDashboard)
	change.DELETE("/dashboards/:id", dashboardHandler.DeleteDashboard)
	change.POST("/dashboards/import", dashboardHandler.ImportDashboard)
	change.POST("/dashboards/upload", dashboardHandler.UploadDashboard)
	operator.POST("/prometheus/validate", configValidateHandler.ValidatePrometheusConfig)
	operator.POST("/alertmanager/validate", configValidateHandler.ValidateAlertmanagerConfig)
	operator.POST("/inspections/run", handler.RunComponentInspection)
	operator.POST("/incidents/:id/acknowledge", incidentsHandler.Acknowledge)
	operator.POST("/incidents/:id/comments", incidentsHandler.Comment)
	operator.POST("/incidents/:id/resolve", incidentsHandler.Resolve)
	protected.GET("/components/:id/parameter-migration-preview", middleware.RequireRole("change_admin", "system_admin"), handler.PreviewPrometheusParameterMigration)
	change.POST("/components/preflight", handler.PreflightComponentDeployment)
	operator.POST("/inspections/schedules/:id/run", handler.RunScheduledInspectionNow)
	change.POST("/components", handler.CreateComponent)
	change.POST("/monitoring/integration/reconcile", handler.ReconcileMonitoringIntegration)
	change.POST("/components/:id/configs", handler.UpdateComponentConfig)
	change.PUT("/components/:id", handler.UpdateComponent)
	change.DELETE("/components/:id", handlers.RequireManagedAdoption(db), handler.UninstallComponent)
	change.POST("/components/:id/best-practice/apply", handler.ApplyComponentBestPractice)
	change.POST("/components/:id/grafana/reset-admin-password", handler.ResetGrafanaAdminPassword)
	change.POST("/components/:id/upgrade", handlers.RequireManagedAdoption(db), handler.UpgradeComponent)
	change.POST("/components/:id/rollback", handlers.RequireManagedAdoption(db), handler.RollbackComponent)
	change.POST("/components/:id/uninstall", handlers.RequireManagedAdoption(db), handler.UninstallComponent)
	change.POST("/configs/:id/rollback", handler.RollbackConfig)
	change.POST("/hosts", handler.CreateHost)
	change.POST("/hosts/batch", handler.BatchCreateHosts)
	change.POST("/hosts/:id/adopt", handler.AdoptComponent)
	change.PUT("/hosts/:id", handler.UpdateHost)
	change.DELETE("/hosts/:id", handler.DeleteHost)
	change.POST("/inspections/schedules", handler.CreateScheduledInspection)
	change.PUT("/inspections/schedules/:id", handler.UpdateScheduledInspection)
	change.DELETE("/inspections/schedules/:id", handler.DeleteScheduledInspection)
	change.DELETE("/components/:id/adoption", handler.ReleaseAdoptedComponent)

	// Operations and response actions. Change and system administrators inherit
	// these capabilities; viewers and auditors remain strictly read-only.
	operator.POST("/alertmanager/alerts", alertmanagerProxyHandler.PostAlerts)
	operator.POST("/alertmanager/silences", func(c *gin.Context) {
		publicPath := c.Request.URL.Path
		c.Request.URL.Path = "/api/v1/alertmanager/" + c.Param("cluster_id") + "/silences"
		alertmanagerProxyHandler.Proxy(c)
		c.Request.URL.Path = publicPath
	})
	operator.DELETE("/alertmanager/silences/:silence_id", func(c *gin.Context) {
		publicPath := c.Request.URL.Path
		c.Request.URL.Path = "/api/v1/alertmanager/" + c.Param("cluster_id") + "/silences/" + c.Param("silence_id")
		alertmanagerProxyHandler.Proxy(c)
		c.Request.URL.Path = publicPath
	})
	operator.POST("/alerts/silences", silenceHandler.CreateSilence)
	operator.DELETE("/alerts/silences/:id", silenceHandler.DeleteSilence)
	operator.POST("/maintenance", opsOpsHandler.CreateMaintenanceWindow)
	operator.DELETE("/maintenance/:id", opsOpsHandler.DeleteMaintenanceWindow)
	operator.POST("/components/:id/start", handlers.RequireManagedAdoption(db), handler.StartComponent)
	operator.POST("/components/:id/stop", handlers.RequireManagedAdoption(db), handler.StopComponent)
	operator.POST("/components/:id/restart", handlers.RequireManagedAdoption(db), handler.RestartComponent)
	operator.GET("/hosts/:id/test", handler.TestHostConnection)
	operator.POST("/hosts/:id/test", handler.TestHostConnection)
	operator.GET("/hosts/:id/detect", handler.DetectComponents)
	operator.GET("/hosts/:id/browse", handler.BrowseDirectory)
	operator.GET("/hosts/:id/ports/:port", handler.CheckHostPort)

	// Read access is shared by every product role unless the resource itself is
	// a system-administration secret or inventory.
	protected.GET("/deployment-templates", templateHandler.ListDeploymentTemplates)
	protected.GET("/deployments/batches", handler.ListDeploymentBatches)
	protected.GET("/deployments/batches/:id", handler.GetDeploymentBatch)
	protected.GET("/deployments/stacks", handler.ListDeploymentStacks)
	protected.GET("/deployments/stacks/:id", handler.GetDeploymentStack)
	protected.GET("/deployments/stacks/:id/acceptance", handler.GetDeploymentStackAcceptance)
	protected.GET("/notifications/status", notificationHandler.Status)
	system.GET("/jobs", handlers.ListJobs)
	system.GET("/templates", templateHandler.ListTemplates)
	system.GET("/templates/:id", templateHandler.GetTemplate)
	system.GET("/templates/default/:type", templateHandler.GetDefaultTemplateByType)
	system.GET("/templates/:id/download", templateHandler.DownloadTemplate)
	system.GET("/packages", packageHandler.ListPackages)
	system.GET("/packages/:id", packageHandler.GetPackage)
	system.GET("/packages/:id/download", packageHandler.DownloadImage)

	// Backups (read)
	protected.GET("/backups", opsHandler.ListBackups)
	protected.GET("/backups/scheduled", opsOpsHandler.ListScheduledBackups)

	// Alerts (read)
	protected.GET("/alerts/rules", alertsHandler.ListAlertRules)
	protected.GET("/alerts/rules/:id", alertsHandler.GetAlertRule)
	// Scrape configs (read)
	protected.GET("/scrape-configs", scrapeConfigsHandler.ListScrapeConfigs)
	protected.GET("/scrape-configs/:id", scrapeConfigsHandler.GetScrapeConfig)

	// Recording rules (read)
	protected.GET("/recording-rules", recordingRulesHandler.ListRecordingRules)
	protected.GET("/recording-rules/:id", recordingRulesHandler.GetRecordingRule)

	// Remote writes (read)
	protected.GET("/remote-writes", remoteWriteHandler.List)
	protected.GET("/remote-writes/:id", remoteWriteHandler.Get)

	// Dashboard exports are product data, while notification and Alertmanager
	// routing settings are restricted platform configuration.
	protected.GET("/dashboards", dashboardHandler.ListDashboards)
	protected.GET("/dashboards/:id", dashboardHandler.GetDashboard)
	protected.GET("/dashboards/:id/export", dashboardHandler.ExportDashboard)
	system.GET("/notifications/channels", notificationHandler.ListChannels)
	system.GET("/notifications/history", notificationHandler.ListAlertNotifications)
	protected.GET("/alertmanager/status", alertmanagerProxyHandler.GetStatus)
	protected.GET("/alertmanager/alerts", alertmanagerProxyHandler.GetAlerts)
	protected.GET("/alertmanager/silences", func(c *gin.Context) {
		publicPath := c.Request.URL.Path
		c.Request.URL.Path = "/api/v1/alertmanager/" + c.Param("cluster_id") + "/silences"
		alertmanagerProxyHandler.Proxy(c)
		c.Request.URL.Path = publicPath
	})
	protected.GET("/alertmanager/receivers", alertmanagerProxyHandler.GetReceivers)
	system.GET("/alertmanager/routes", alertmanagerSettingsHandler.ListAlertmanagerRoutes)
	system.GET("/alertmanager/settings", alertmanagerSettingsHandler.GetAlertmanagerSettings)

	// Alert silences (read)
	protected.GET("/alerts/silences", silenceHandler.ListSilences)
	protected.GET("/alerts/silences/:id", silenceHandler.GetSilence)

	// Resource monitoring
	protected.GET("/components/:id/resource", opsOpsHandler.GetResourceUsage)
	protected.GET("/disk/check", opsOpsHandler.CheckDiskUsage)
	protected.GET("/node/resource", func(c *gin.Context) {
		c.Params = append(c.Params, gin.Param{Key: "id", Value: c.Param("cluster_id")})
		opsOpsHandler.GetClusterResourceUsage(c)
	})
	// Maintenance (read)
	protected.GET("/maintenance", opsOpsHandler.ListMaintenanceWindows)
	protected.GET("/maintenance/check", opsOpsHandler.IsMaintenance)

	// Topology / Drift
	protected.GET("/config/drift", middleware.AuditLog(db), opsOpsHandler.CheckConfigDrift)
	protected.GET("/topology", func(c *gin.Context) {
		c.Params = append(c.Params, gin.Param{Key: "id", Value: c.Param("cluster_id")})
		opsOpsHandler.GetTopology(c)
	})
	protected.GET("/components/:id/logs/stream", opsOpsHandler.StreamLogs)

	// Components (read)
	protected.GET("/components/:id/configs", handler.ListComponentConfigs)
	protected.GET("/components/:id/config/current", handler.GetCurrentComponentConfig)
	protected.GET("/components/:id/best-practice", handler.GetComponentBestPractice)

	// Configs (read)
	protected.GET("/monitoring/integration/status", handler.GetMonitoringIntegrationStatus)
	protected.GET("/monitoring/heartbeat", handler.GetHeartbeatDashboard)
	protected.GET("/configs/:id", handler.GetComponentConfig)

	// Hosts (read)
	protected.GET("/hosts", handler.ListHosts)
	protected.GET("/hosts/:id", handler.GetHost)
	protected.GET("/hosts/:id/monitoring-status", handler.GetHostMonitoringStatus)

	// Deployment roles need the filtered version inventory to choose a locally
	// cached artifact. Full version administration remains a system setting.
	system.GET("/versions", handler.ListVersions)
	protected.GET("/versions/by-type", handler.ListVersionsByType)
	system.POST("/versions/refresh", handler.RefreshVersions)
	system.GET("/versions/:id", handler.GetVersion)
	protected.GET("/jobs/:id", handlers.GetJob)

	protected.GET("/me", middleware.Me)
	protected.GET("/node", handler.GetNode)
	protected.GET("/deployment-definitions", handler.ListDeploymentDefinitions)
	system.GET("/component-definitions", componentDefinitionHandler.ListComponentDefinitions)
	system.GET("/component-definitions/:id", componentDefinitionHandler.GetComponentDefinition)

	// User administration is system-only; AuditLog is inherited from system.
	system.GET("/users", handler.ListUsers)
	system.GET("/users/:id", handler.GetUser)
	system.POST("/users", handler.CreateUser)
	system.PUT("/users/:id", handler.UpdateUser)
	system.DELETE("/users/:id", handler.DeleteUser)
	protected.GET("/onboarding/status", handler.GetOnboardingStatus)
	protected.GET("/onboarding/preflight", handler.GetOnboardingPreflight)
	protected.GET("/onboarding/discovery", handler.GetOnboardingDiscovery)
	protected.GET("/stack-templates", handler.ListStackTemplates)
	protected.GET("/components/:id/adoption", handler.GetComponentAdoption)
	operator.POST("/onboarding/acceptance", handler.RunOnboardingAcceptance)
	change.DELETE("/hosts/:id/discovery", handler.DeleteOnboardingDiscovery)
	protected.GET("/change-plans", changePlanHandler.List)
	protected.GET("/change-plans/:id", changePlanHandler.Get)
	handlers.RegisterPairedRecoveryRoutes(protected, change, pairedRecoveryHandler)
	change.POST("/change-plans", changePlanHandler.Create)
	change.PUT("/change-plans/:id", changePlanHandler.Update)
	change.DELETE("/change-plans/:id", changePlanHandler.Delete)
	change.POST("/change-plans/:id/execute", changePlanHandler.Execute)
	change.POST("/change-plans/:id/verify", changePlanHandler.Verify)
	change.POST("/change-plans/:id/rollback", changePlanHandler.Rollback)

	protected.GET("/recovery-exercises", recoveryExercisesHandler.List)
	protected.GET("/recovery-exercises/:id", recoveryExercisesHandler.Get)
	change.POST("/recovery-exercises", recoveryExercisesHandler.Create)
	change.PUT("/recovery-exercises/:id", recoveryExercisesHandler.Update)
	change.DELETE("/recovery-exercises/:id", recoveryExercisesHandler.Delete)
	change.POST("/recovery-exercises/:id/run", recoveryExercisesHandler.Run)

	protected.GET("/stats", opsHandler.GetPlatformStats)
	protected.GET("/stats/enhanced", opsOpsHandler.GetEnhancedStats)
	protected.GET("/operations", opsHandler.GetRecentOperations)
	protected.GET("/operations/logs", opsHandler.ListOperationLogs)
	protected.GET("/inspections", handler.ListInspectionRuns)
	protected.GET("/inspections/schedules", handler.ListScheduledInspections)
	protected.GET("/inspections/notification-policy", handler.GetInspectionNotificationPolicy)
	protected.GET("/ai/config", aiHandler.GetConfig)
	protected.GET("/inspections/notification-states", handler.ListInspectionNotificationStates)
	protected.GET("/inspections/:id", handler.GetInspectionRun)
	protected.GET("/operations/:id", opsHandler.GetOperation)
	protected.GET("/components", handler.ListComponents)
	protected.GET("/components/:id", handler.GetComponent)
	protected.GET("/components/:id/health", handler.HealthCheck)
	protected.GET("/components/:id/logs", handler.GetLogs)
	protected.GET("/alerts/history", alertsHandler.ListAlertHistory)
	protected.GET("/alerts/active", alertsHandler.GetActiveAlerts)
	protected.GET("/backups/platform", opsOpsHandler.ListPlatformBackups)
	protected.GET("/backups/scheduled/statuses", opsHandler.ListScheduledBackupStatuses)

	auditExport := protected.Group("")
	auditExport.Use(middleware.RequireRole("auditor", "system_admin"))
	auditExport.GET("/operations/logs/export", auditExportHandler.ExportCSV)

	// serveIndex serves the SPA index.html: an external PROMLIFECYCLE_WEB_PATH
	// override wins when it exists, otherwise the embedded copy is used.
	serveIndex := func(c *gin.Context) {
		if p := os.Getenv("PROMLIFECYCLE_WEB_PATH"); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				c.Data(http.StatusOK, "text/html; charset=utf-8", data)
				return
			}
			log.Printf("warning: PROMLIFECYCLE_WEB_PATH %s not found, using embedded frontend", p)
		}
		if data, err := webIndex(); err == nil {
			c.Data(http.StatusOK, "text/html; charset=utf-8", data)
			return
		}
		// Last-resort fallback so the endpoint is still self-describing.
		c.JSON(http.StatusOK, gin.H{
			"name":    "Prometheus Lifecycle Platform",
			"version": "1.0.0",
			"status":  "running",
			"api":     "/api/v1",
		})
	}

	// Root endpoint - serve frontend SPA
	r.GET("/", func(c *gin.Context) {
		serveIndex(c)
	})

	// Platform's own metrics in Prometheus exposition format (public: counts
	// only, lets the managed Prometheus instances scrape the platform itself).
	r.GET("/metrics", MetricsHandler(db))

	// SPA fallback for client-side routing
	r.NoRoute(func(c *gin.Context) {
		// API routes return proper errors, others serve SPA
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "endpoint not found", "path": c.Request.URL.Path})
			return
		}
		serveIndex(c)
	})
	// Start background scheduler (health checks, backups, alert collection)
	sched := scheduler.NewScheduler(db, basePath)
	sched.Start()
	stackContext, cancelStacks := context.WithCancel(context.Background())
	stacksDone := handler.StartDeploymentStacks(stackContext)

	// Register official archives already on disk but missing from the database
	// (e.g. after rebuilding the DB separately from the package files), then
	// pre-download any remaining built-in packages in the background so
	// deployments work offline afterwards.
	deployer.ScanLocalPackages(db, basePath)
	deployer.EnsureBuiltinPackages(db, basePath)

	return &SetupResult{Engine: r, Scheduler: sched, cancelStacks: cancelStacks, stacksDone: stacksDone}
}
