package scheduler

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaintenanceReconcileDoesNotPostGlobalForDeletedScope(t *testing.T) {
	db := inspectionSchedulerDB(t)
	if err := db.AutoMigrate(&models.MaintenanceWindow{}, &models.MaintenanceSilence{}); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	cluster := models.Cluster{Name: "single-node"}
	db.Create(&cluster)
	am := models.Component{Name: "am", Type: models.ComponentAlertmanager, ClusterID: cluster.ID, Status: models.StatusRunning, ExternalURL: server.URL}
	db.Create(&am)
	missing := uint(900)
	for _, scope := range []*uint{nil, &missing} {
		window := models.MaintenanceWindow{Name: "unsafe", ComponentID: scope, StartAt: time.Now().Add(-time.Minute), EndAt: time.Now().Add(time.Hour)}
		if err := db.Create(&window).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := NewScheduler(db, t.TempDir()).syncMaintenanceWindows(); err == nil {
		t.Fatal("invalid maintenance scopes were not reported")
	}
	if posts.Load() != 0 {
		t.Fatalf("invalid scopes posted %d global silences", posts.Load())
	}
}

func TestHealthObservationCannotOverwriteIntentionalStop(t *testing.T) {
	db := inspectionSchedulerDB(t)
	comp := models.Component{Name: "prom", Type: models.ComponentPrometheus, Status: models.StatusRunning}
	db.Create(&comp)
	observed := comp
	if err := db.Model(&comp).Update("status", models.StatusStopped).Error; err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(db, t.TempDir())
	if err := scheduler.persistObservedHealth(&observed, models.StatusError, map[string]interface{}{"status": models.StatusError}); err != nil {
		t.Fatal(err)
	}
	db.First(&comp, comp.ID)
	if comp.Status != models.StatusStopped {
		t.Fatalf("stale health overwrote stop: %s", comp.Status)
	}
}

func TestScheduleRunNowSharesInspectionClaim(t *testing.T) {
	db := inspectionSchedulerDB(t)
	schedule := models.ScheduledInspection{Name: "scheduled", Cron: "* * * * *", KeepCount: 2, Enabled: true}
	db.Create(&schedule)
	_, release, err := services.ClaimInspection(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = NewScheduler(db, t.TempDir()).ExecuteInspectionSchedule(context.Background(), &schedule, "manual", time.Now())
	if !errors.Is(err, services.ErrInspectionBusy) {
		t.Fatalf("manual schedule bypassed running inspection: %v", err)
	}
	var count int64
	db.Model(&models.InspectionRun{}).Count(&count)
	if count != 0 {
		t.Fatalf("overlap created %d runs", count)
	}
}
