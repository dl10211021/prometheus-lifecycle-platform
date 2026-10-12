package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func inspectionSchedulerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inspection-scheduler.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Host{}, &models.Cluster{}, &models.Component{}, &models.Operation{},
		&models.InspectionRun{}, &models.InspectionCheck{},
		&models.ScheduledInspection{}, &models.ScheduledInspectionStatus{},
		&models.InspectionClaim{}, &models.InspectionNotificationPolicy{}, &models.InspectionNotificationState{}, &models.InspectionTransition{}, &models.InspectionDelivery{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestScheduledInspectionRunsOncePerMinuteAndRecordsStatus(t *testing.T) {
	db := inspectionSchedulerDB(t)
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()
	host := models.Host{Name: "target", IP: "192.0.2.50", OS: "linux", ConnectionType: "ssh"}
	cluster := models.Cluster{Name: "single-node"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{HostID: host.ID, ClusterID: cluster.ID, Name: "external", Type: models.ComponentNodeExporter, DeployType: "target", InternalURL: endpoint.URL}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	schedule := models.ScheduledInspection{Name: "every-minute", Cron: "* * * * *", KeepCount: 5, Enabled: true}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}

	scheduler := NewScheduler(db, t.TempDir())
	now := time.Date(2026, 9, 3, 10, 15, 0, 0, time.Local)
	if err := scheduler.executeDueInspections(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var run models.InspectionRun
	if err := db.Where("scheduled_inspection_id = ?", schedule.ID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.Result != "healthy" || run.Passed != 1 {
		t.Fatalf("unexpected run: %#v", run)
	}
	var status models.ScheduledInspectionStatus
	if err := db.Where("scheduled_inspection_id = ?", schedule.ID).First(&status).Error; err != nil {
		t.Fatal(err)
	}
	if status.LastStatus != "success" || status.LastResult != "healthy" || status.InspectionRunID == nil || *status.InspectionRunID != run.ID {
		t.Fatalf("unexpected status: %#v", status)
	}
	if err := scheduler.executeDueInspections(context.Background(), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.InspectionRun{}).Where("scheduled_inspection_id = ?", schedule.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("same minute produced %d runs", count)
	}
}

func TestScheduledInspectionRetentionDeletesChecksWithOldRuns(t *testing.T) {
	db := inspectionSchedulerDB(t)
	schedule := models.ScheduledInspection{Name: "retained", Cron: "0 * * * *", KeepCount: 2, Enabled: true}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		run := models.InspectionRun{ScheduledInspectionID: &schedule.ID, Status: "completed", Result: "healthy", StartedAt: time.Now().Add(time.Duration(index) * time.Minute)}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.InspectionCheck{RunID: run.ID, Component: "component", Status: "passed", CheckedAt: time.Now()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	NewScheduler(db, t.TempDir()).cleanupOldInspectionRuns(&schedule)
	var runs, checks int64
	if err := db.Model(&models.InspectionRun{}).Where("scheduled_inspection_id = ?", schedule.ID).Count(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.InspectionCheck{}).Count(&checks).Error; err != nil {
		t.Fatal(err)
	}
	if runs != 2 || checks != 2 {
		t.Fatalf("retention kept runs=%d checks=%d, want 2/2", runs, checks)
	}
}

func TestValidCronExpression(t *testing.T) {
	valid := []string{"* * * * *", "*/5 * * * *", "0 2 * * 1-5", "0,15,30,45 * * * *", "0-30/5 * * * *"}
	for _, expression := range valid {
		if !ValidCronExpression(expression) {
			t.Fatalf("valid cron rejected: %q", expression)
		}
	}
	invalid := []string{"", "* * *", "60 * * * *", "*/0 * * * *", "0 24 * * *", "0 * * * 7", "0-70 * * * *"}
	for _, expression := range invalid {
		if ValidCronExpression(expression) {
			t.Fatalf("invalid cron accepted: %q", expression)
		}
	}
}
