package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/promlifecycle/platform/api/handlers"
	"github.com/promlifecycle/platform/api/router"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/scheduler"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"github.com/promlifecycle/platform/storage/sqlite"
	"github.com/spf13/viper"
	"gorm.io/gorm"
)

func main() {
	configFile := flag.String("config", "", "config file path (optional)")
	port := flag.Int("port", 8080, "HTTP server port")
	listenAddress := flag.String("listen-address", "127.0.0.1", "HTTP listen address")
	tlsCert := flag.String("tls-cert", "", "TLS certificate path (requires --tls-key)")
	tlsKey := flag.String("tls-key", "", "TLS private key path (requires --tls-cert)")
	redirectHTTPPort := flag.Int("redirect-http-port", 0, "optional HTTP port that redirects to HTTPS")
	redirectHost := flag.String("redirect-host", "", "public IP used by HTTP-to-HTTPS redirects")
	basePath := flag.String("base-path", "/opt/promlifecycle", "base path for data/configs/compose")
	dataDir := flag.String("data-dir", "./data", "data directory (for SQLite DB)")
	debug := flag.Bool("debug", false, "enable debug mode")
	recoverExecution := flag.String("recover-prometheus-restore-execution", "", "compensate one interrupted paired restore without starting the server")
	recoverStage := flag.String("recover-prometheus-restore-stage", "", "recover one fenced paired restore stage without starting the server")
	restoreBackupRoot := flag.String("restore-backup-root", "/backups", "artifact root for paired restore stage and execution recovery")
	flag.Parse()
	recoverExecutionRequested := false
	recoverStageRequested := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "recover-prometheus-restore-execution" {
			recoverExecutionRequested = true
		}
		if f.Name == "recover-prometheus-restore-stage" {
			recoverStageRequested = true
		}
	})
	if recoverExecutionRequested {
		if recoverStageRequested {
			fmt.Println("error: select one paired recovery command")
			os.Exit(1)
		}
		if err := recoverPrometheusRestoreExecutionCommand(context.Background(), filepath.Join(*dataDir, "promlifecycle.db"), *restoreBackupRoot, *recoverExecution); err != nil {
			fmt.Println("error: paired restore execution recovery rejected; durable recovery remains authoritative")
			os.Exit(1)
		}
		return
	}
	if recoverStageRequested {
		if err := recoverPrometheusRestoreStageCommand(context.Background(), filepath.Join(*dataDir, "promlifecycle.db"), *restoreBackupRoot, *recoverStage); err != nil {
			fmt.Println("error: paired restore stage recovery rejected; durable recovery remains authoritative")
			os.Exit(1)
		}
		return
	}

	if *configFile != "" {
		viper.SetConfigFile(*configFile)
		if err := viper.ReadInConfig(); err != nil {
			fmt.Printf("warning: could not read config: %v\n", err)
		}
	}

	if *debug {
		fmt.Println("Debug mode enabled")
	}
	if err := validateServerTransport(*listenAddress, *tlsCert, *tlsKey, *redirectHost, *redirectHTTPPort, allowInsecureHTTP()); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}

	// Ensure directories exist
	for _, dir := range []string{*basePath, *dataDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Printf("error: create dir %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	dbPath := filepath.Join(*dataDir, "promlifecycle.db")
	// Own the actual database before restore, migrations or recovery can mutate
	// shared state. The OS releases ownership even when startup exits abruptly.
	dbPath, processLock, err := acquireDatabaseProcessLock(dbPath)
	if err != nil {
		fmt.Printf("error: acquire database ownership: %v\n", err)
		os.Exit(1)
	}
	defer processLock.Close()
	fmt.Printf("Using SQLite database: %s\n", dbPath)
	if err := preparePairedRestoreStartup(context.Background(), dbPath); err != nil {
		fmt.Printf("error: paired restore startup guard: %v\n", err)
		os.Exit(1)
	}
	if err := sqlite.MigrateLegacyRestore(dbPath, filepath.Join(*basePath, "backups", "promlifecycle.db.restore")); err != nil {
		fmt.Printf("error: migrate pending database restore: %v\n", err)
		os.Exit(1)
	}

	restore, err := sqlite.ApplyPendingRestore(dbPath)
	if err != nil {
		fmt.Printf("error: prepare database restore: %v\n", err)
		os.Exit(1)
	}
	var db *gorm.DB
	startupFailure := func(cause error) {
		if db != nil {
			if connection, err := db.DB(); err == nil {
				_ = connection.Close()
			}
		}
		if err := restore.Rollback(cause); err != nil {
			fmt.Printf("error: restore original database: %v\n", err)
		}
		fmt.Printf("error: initialize database: %v\n", cause)
		os.Exit(1)
	}

	// Initialize SQLite
	db, err = sqlite.OpenDB(dbPath)
	if err != nil {
		startupFailure(err)
	}

	if err := db.AutoMigrate(
		&models.Host{},
		&models.Cluster{},
		&models.Component{},
		&models.ComponentConfig{},
		&models.Operation{},
		&models.Backup{},
		&models.User{},
		&models.ComponentVersion{},
		&models.OperationLog{},
		&models.AlertRule{},
		&models.AlertHistory{},
		&models.NotificationChannel{},
		&models.AlertNotification{},
		&models.ScheduledBackup{},
		&models.ResourceUsage{},
		&models.MaintenanceWindow{},
		&models.MaintenanceSilence{},
		&models.TopologyEdge{},
		&models.ConfigTemplate{},
		&models.Package{},
		&models.ScrapeConfigModel{},
		&models.RecordingRule{},
		&models.RemoteWriteModel{},
		&models.AlertmanagerRoute{},
		&models.AlertmanagerSettings{},
		&models.AlertSilence{},
		&models.Dashboard{},
		&models.ComponentDefinition{},
		&models.Incident{},
		&models.IncidentEvent{},
		&models.BackupManifest{},
		&models.PrometheusRecoveryPoint{},
		&models.PrometheusRestoreTransaction{},
		&models.ScheduledBackupStatus{},
		&models.RecoveryExercise{},
		&models.ComponentAdoption{},
		&models.OnboardingCheckRun{},
		&models.ChangePlan{},
		&models.InspectionRun{},
		&models.InspectionCheck{},
		&models.ScheduledInspection{},
		&models.ScheduledInspectionStatus{},
		&models.InspectionNotificationPolicy{},
		&models.InspectionNotificationState{},
		&models.AIConfig{},
		&models.AIInteraction{},
		&models.DeploymentPreflight{},
		&models.ComponentOperationClaim{},
		&models.HostOperationClaim{},
		&models.InspectionClaim{},
		&models.InspectionTransition{},
		&models.InspectionDelivery{},
		&models.InspectionDeliveryResolution{},
		&models.ChangeBaseline{},
		&models.DeploymentBatch{},
		&models.DeploymentStack{},
		&models.DeploymentStackStep{},
		&services.ComponentRestoreCheckpoint{},
		&services.ComponentBackupPause{},
		&scheduler.ScheduledBackupRun{},
	); err != nil {
		startupFailure(err)
	}
	if err := services.CheckPrometheusRestoreJournalBeforeStartup(context.Background(), db); err != nil {
		startupFailure(err)
	}
	if err := migrateLegacyHostStatus(db); err != nil {
		startupFailure(err)
	}
	if err := services.MigrateToSingleNode(db); err != nil {
		startupFailure(err)
	}
	if err := migrateLegacyUserRoles(db); err != nil {
		startupFailure(err)
	}
	if _, err := services.EnsureLocalHost(db, *basePath); err != nil {
		startupFailure(err)
	}
	fmt.Println("Database initialized")

	// Seed the only default account. Its password is configured explicitly or
	// generated once; the historical fixed admin123 credential is never used.
	var adminCount int64
	if err := db.Model(&models.User{}).Count(&adminCount).Error; err != nil {
		startupFailure(err)
	}
	if adminCount == 0 {
		password, generated, err := initialAdminPassword()
		if err != nil {
			startupFailure(err)
		}
		hashedPassword := utils.HashPassword(password)
		if hashedPassword == "" {
			startupFailure(fmt.Errorf("hash initial admin password"))
		}
		if err := db.Create(&models.User{
			Username:           "admin",
			Password:           hashedPassword,
			Email:              "admin@localhost",
			Role:               string(models.RoleSystemAdmin),
			IsActive:           true,
			MustChangePassword: true,
		}).Error; err != nil {
			startupFailure(err)
		}
		if generated {
			fmt.Printf("Default admin user created. One-time password for admin: %s\n", password)
		} else {
			fmt.Println("Default admin user created with configured one-time password")
		}
	}

	// Initialize deployer
	deployerFactory := deployer.NewFactory(*basePath)

	if err := deployerFactory.Deployer().Init(); err != nil {
		startupFailure(err)
	}
	fmt.Printf("Deployer initialized with base path: %s\n", *basePath)

	// Initialize config manager
	configManager := config.NewManager(filepath.Join(*basePath, "configs"))
	fmt.Println("Config manager initialized")

	// Reconcile durable work before any scheduler or HTTP handler can dispatch it.
	for _, recover := range []func() error{
		func() error { return services.RecoverInterruptedLifecycle(db) },
		func() error { return services.RecoverInterruptedComponentRestores(context.Background(), db) },
		func() error { return handlers.RecoverInterruptedExercises(db) },
		func() error { return scheduler.RecoverInterruptedBackupRuns(db) },
		func() error { return handlers.InitBackgroundJobs(db) },
		func() error { return services.RecoverInterruptedInspections(db) },
		func() error { return services.ReconcileInterruptedRuntime(context.Background(), db, *basePath) },
	} {
		if err := recover(); err != nil {
			startupFailure(err)
		}
	}
	if err := restore.Commit(); err != nil {
		startupFailure(err)
	}

	// Setup router and background scheduler.
	setup := router.Setup(db, deployerFactory, configManager, *basePath)

	// Create HTTP server
	server := &http.Server{
		Addr:         net.JoinHostPort(*listenAddress, strconv.Itoa(*port)),
		Handler:      setup.Engine,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	var redirectServer *http.Server
	if *redirectHTTPPort > 0 {
		redirectServer = &http.Server{
			Addr: net.JoinHostPort(*listenAddress, strconv.Itoa(*redirectHTTPPort)),
			Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				http.Redirect(w, req, httpsRedirectURL(*redirectHost, *port, req), http.StatusPermanentRedirect)
			}),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		go func() {
			if err := redirectServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Printf("error: start HTTP redirect server: %v\n", err)
			}
		}()
	}

	go func() {
		scheme := "http"
		if *tlsCert != "" {
			scheme = "https"
		}
		fmt.Printf("\n===========================================\n")
		fmt.Printf("Prometheus Lifecycle Platform\n")
		fmt.Printf("HTTP: %s://%s\n", scheme, server.Addr)
		fmt.Printf("API: %s://%s/api/v1\n", scheme, server.Addr)
		fmt.Printf("Health: %s://%s/health\n", scheme, server.Addr)
		fmt.Printf("===========================================\n\n")

		var err error
		if *tlsCert != "" {
			err = server.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			fmt.Printf("error: start server: %v\n", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	fmt.Println("\nShutting down server...")
	setup.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		fmt.Printf("error: graceful shutdown: %v\n", err)
		os.Exit(1)
	}
	if redirectServer != nil {
		if err := redirectServer.Shutdown(ctx); err != nil {
			fmt.Printf("error: graceful redirect shutdown: %v\n", err)
		}
	}

	fmt.Println("Server stopped")
}

// migrateLegacyUserRoles is a one-way startup migration. Authorization never
// treats legacy values as aliases, so all rows are converted before login.
func migrateLegacyUserRoles(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE users SET role = ? WHERE role = ?", models.RoleSystemAdmin, "admin").Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE users SET role = ? WHERE role = ?", models.RoleChangeAdmin, "user").Error
	})
}

func initialAdminPassword() (password string, generated bool, err error) {
	if password = os.Getenv("PROMLIFECYCLE_ADMIN_PASSWORD"); password != "" {
		return password, false, nil
	}
	if path := os.Getenv("PROMLIFECYCLE_ADMIN_PASSWORD_FILE"); path != "" {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", false, fmt.Errorf("read PROMLIFECYCLE_ADMIN_PASSWORD_FILE: %w", readErr)
		}
		password = strings.TrimRight(string(data), "\r\n")
		if password == "" {
			return "", false, fmt.Errorf("PROMLIFECYCLE_ADMIN_PASSWORD_FILE is empty")
		}
		return password, false, nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", false, fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), true, nil
}

func allowInsecureHTTP() bool {
	value := strings.TrimSpace(os.Getenv("PROMLIFECYCLE_ALLOW_INSECURE_HTTP"))
	return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
}

func validateServerTransport(listenAddress, tlsCert, tlsKey, redirectHost string, redirectHTTPPort int, allowInsecure bool) error {
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("--tls-cert 与 --tls-key 必须同时设置")
	}
	if tlsCert == "" && !isLoopbackAddress(listenAddress) && !allowInsecure {
		return fmt.Errorf("拒绝在非回环地址上提供明文 HTTP；请配置 --tls-cert/--tls-key，或明确设置 PROMLIFECYCLE_ALLOW_INSECURE_HTTP=true")
	}
	if redirectHTTPPort < 0 || redirectHTTPPort > 65535 {
		return fmt.Errorf("--redirect-http-port 必须是 1-65535")
	}
	if redirectHTTPPort > 0 {
		if tlsCert == "" {
			return fmt.Errorf("--redirect-http-port 只能与 TLS 服务一起使用")
		}
		if strings.TrimSpace(redirectHost) == "" {
			return fmt.Errorf("--redirect-http-port 需要显式设置 --redirect-host")
		}
	}
	return nil
}

func isLoopbackAddress(address string) bool {
	if strings.EqualFold(strings.TrimSpace(address), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(address))
	return ip != nil && ip.IsLoopback()
}

func httpsRedirectURL(host string, httpsPort int, req *http.Request) string {
	targetHost := strings.TrimSpace(host)
	if httpsPort != 443 {
		targetHost = net.JoinHostPort(targetHost, strconv.Itoa(httpsPort))
	}
	target := neturl.URL{
		Scheme:   "https",
		Host:     targetHost,
		Path:     req.URL.Path,
		RawPath:  req.URL.RawPath,
		RawQuery: req.URL.RawQuery,
	}
	return target.String()
}

// There is no durable connectivity job behind legacy pending host rows.
// Preserve historical successes and failures; do not invent a check timestamp.
func migrateLegacyHostStatus(db *gorm.DB) error {
	return db.Model(&models.Host{}).Where("status = ?", "pending").Update("status", "unknown").Error
}
