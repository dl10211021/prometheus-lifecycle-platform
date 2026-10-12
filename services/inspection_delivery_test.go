package services

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func deliveryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "delivery.db")+"?_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Component{}, &models.NotificationChannel{}, &models.AlertNotification{}, &models.InspectionRun{}, &models.InspectionCheck{}, &models.InspectionNotificationPolicy{}, &models.InspectionNotificationState{}, &models.InspectionTransition{}, &models.InspectionDelivery{}, &models.InspectionClaim{}, &models.ScheduledInspection{}, &models.ScheduledInspectionStatus{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestInspectionRetryAcknowledgementsArePerTransitionAndChannel(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_ALLOW_PRIVATE_FETCH", "1")
	db := deliveryDB(t)
	component := models.Component{Name: "prom", Type: models.ComponentPrometheus}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	var hitCounts [2]atomic.Int32
	hits := func() [2]int32 { return [2]int32{hitCounts[0].Load(), hitCounts[1].Load()} }
	channels := []models.NotificationChannel{}
	for index := range 2 {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hitCounts[index].Add(1)
			if index == 1 && fail.Load() {
				w.WriteHeader(503)
			} else {
				w.WriteHeader(204)
			}
		}))
		defer server.Close()
		channel := models.NotificationChannel{Name: server.URL, Type: "webhook", Config: `{"url":"` + server.URL + `"}`, Enabled: true}
		if err := db.Create(&channel).Error; err != nil {
			t.Fatal(err)
		}
		channels = append(channels, channel)
	}
	policy := models.InspectionNotificationPolicy{Enabled: true, ConsecutiveFailures: 1, NotifyRecovery: true, ChannelIDs: []uint{channels[0].ID, channels[1].ID}}
	if err := db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	service := NewInspectionNotificationService(db)
	process := func(status string) models.InspectionRun {
		run := models.InspectionRun{Status: "completed", StartedAt: time.Now()}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.InspectionCheck{RunID: run.ID, ComponentID: component.ID, Status: status}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := service.Process(run, nil); err != nil {
			t.Fatal(err)
		}
		return run
	}
	old := process("failed")
	if hits() != [2]int32{1, 1} {
		t.Fatalf("initial partial attempt: %v", hits())
	}
	if _, err := service.Process(old, nil); err != nil {
		t.Fatal(err)
	}
	if hits() != [2]int32{1, 1} {
		t.Fatalf("replayed evidence sent duplicates: %v", hits())
	}
	fail.Store(false)
	if _, err := service.RetryPending(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if hits() != [2]int32{1, 2} {
		t.Fatalf("successful channel retried: %v", hits())
	}
	fail.Store(true)
	process("passed")
	if hits() != [2]int32{2, 3} {
		t.Fatalf("recovery was not attempted independently: %v", hits())
	}
	process("failed")
	if hits() != [2]int32{3, 3} {
		t.Fatalf("new outage missing or overtook failed recovery: %v", hits())
	}
	fail.Store(false)
	if _, err := service.RetryPending(time.Now().Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if hits() != [2]int32{3, 5} {
		t.Fatalf("recovery and next outage retries: %v", hits())
	}
	if _, err := service.Process(old, nil); err != nil {
		t.Fatal(err)
	}
	var state models.InspectionNotificationState
	if err := db.First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.LastInspectionRunID == old.ID || state.ConsecutiveFailures != 1 {
		t.Fatalf("stale evidence mutated state: %+v", state)
	}
	if _, err := service.RetryPending(time.Now().Add(4 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if hits() != [2]int32{3, 5} {
		t.Fatalf("acknowledged transitions duplicated: %v", hits())
	}
}

func TestInspectionClaimConcurrentAndAbandonedRecovery(t *testing.T) {
	db := deliveryDB(t)
	start := make(chan struct{})
	releaseWinner := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, release, err := ClaimInspection(context.Background(), db)
			results <- err
			if err == nil {
				<-releaseWinner
				release()
			}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, ErrInspectionBusy)) || (second == nil && errors.Is(first, ErrInspectionBusy))) {
		t.Fatalf("claim results: %v / %v", first, second)
	}
	close(releaseWinner)
	workers.Wait()
	if _, _, err := ClaimInspection(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	run := models.InspectionRun{Status: "running"}
	db.Create(&run)
	delivery := models.InspectionDelivery{Status: "sending"}
	db.Create(&delivery)
	if err := RecoverInterruptedInspections(db); err != nil {
		t.Fatal(err)
	}
	db.First(&run, run.ID)
	db.First(&delivery, delivery.ID)
	if run.Status != "interrupted" || delivery.Status != "unknown" {
		t.Fatalf("unsafe recovery: run=%s delivery=%s", run.Status, delivery.Status)
	}
	_, release, err := ClaimInspection(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestMaintenanceScopeNeverBroadensMissingComponent(t *testing.T) {
	db := deliveryDB(t)
	cluster := models.Cluster{Name: "single-node"}
	db.Create(&cluster)
	component := models.Component{Name: "prom-a", Type: models.ComponentPrometheus, ClusterID: cluster.ID}
	db.Create(&component)
	matchers, err := MaintenanceMatchers(db, &models.MaintenanceWindow{ComponentID: &component.ID})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"component": "prometheus", "name": "prom-a", "node": "single-node"}
	for _, m := range matchers {
		if labels[m["name"].(string)] != m["value"] {
			t.Fatalf("wrong label matcher: %v", m)
		}
	}
	if len(matchers) != 3 {
		t.Fatalf("component identity is incomplete: %v", matchers)
	}
	nodeMatchers, err := MaintenanceMatchers(db, &models.MaintenanceWindow{ClusterID: &cluster.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeMatchers) != 1 || nodeMatchers[0]["name"] != "node" || nodeMatchers[0]["value"] != "single-node" {
		t.Fatalf("maintenance uses absent cluster label: %v", nodeMatchers)
	}
	missing := component.ID + 100
	for _, window := range []models.MaintenanceWindow{{}, {ComponentID: &missing}, {ClusterID: &missing}} {
		if m, err := MaintenanceMatchers(db, &window); err == nil || len(m) > 0 {
			t.Fatalf("missing scope broadened: %+v %v", window, m)
		}
	}
}

func TestInspectionRecoveryWaitsForEntirelyFailedOutage(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_ALLOW_PRIVATE_FETCH", "1")
	db := deliveryDB(t)
	var offline atomic.Bool
	offline.Store(true)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if offline.Load() {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	component := models.Component{Name: "target", Type: models.ComponentPrometheus}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	channel := models.NotificationChannel{Name: "ops", Type: "webhook", Config: `{"url":"` + server.URL + `"}`, Enabled: true}
	if err := db.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	policy := models.InspectionNotificationPolicy{Enabled: true, ConsecutiveFailures: 1, NotifyRecovery: true, ChannelIDs: []uint{channel.ID}}
	if err := db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}
	service := NewInspectionNotificationService(db)
	for _, status := range []string{"failed", "passed", "passed"} {
		run := models.InspectionRun{Status: "completed"}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.InspectionCheck{RunID: run.ID, ComponentID: component.ID, Status: status}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := service.Process(run, nil); err != nil {
			t.Fatal(err)
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("recovery overtook unacknowledged failure: %d attempts", attempts.Load())
	}
	offline.Store(false)
	events, err := service.RetryPending(time.Now().Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 || len(events) != 2 || events[0].Event != "failure" || events[1].Event != "recovery" {
		t.Fatalf("failed outage or its recovery lost: attempts=%d events=%+v", attempts.Load(), events)
	}
	if _, err := service.RetryPending(time.Now().Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("stable recovery retried acknowledged sends: %d", attempts.Load())
	}
}
