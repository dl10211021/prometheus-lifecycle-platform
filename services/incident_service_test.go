package services

import (
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestIncidentLifecycleDeduplicatesAndRequiresVerification(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:incident-lifecycle?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Incident{}, &models.IncidentEvent{}); err != nil {
		t.Fatal(err)
	}
	service := NewIncidentService(db)
	observedAt := time.Now()
	observation := IncidentObservation{
		Kind: models.IncidentKindAlert, Severity: "critical", SourceKey: "alert-1",
		ResourceType: "component", ResourceID: 15, ResourceName: "AcceptanceAlert",
		Title: "AcceptanceAlert", Summary: "firing", Evidence: map[string]any{"value": 1},
		ObservedAt: observedAt,
	}
	first, err := service.Observe(observation)
	if err != nil {
		t.Fatal(err)
	}
	observation.Summary = "still firing"
	second, err := service.Observe(observation)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("active incident duplicated: first=%d second=%d", first.ID, second.ID)
	}
	ownerID := uint(7)
	acknowledged, err := service.Acknowledge(first.ID, &ownerID, "operator", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.Status != models.IncidentStatusAcknowledged || acknowledged.Owner != "operator" {
		t.Fatalf("unexpected acknowledged incident: %#v", acknowledged)
	}
	if _, err := service.Comment(first.ID, &ownerID, "operator", "根因调查记录"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(first.ID, &ownerID, "operator", "", nil); err == nil {
		t.Fatal("incident resolved without verification")
	}
	resolved, err := service.Resolve(first.ID, &ownerID, "operator", "Prometheus 已恢复且规则健康", map[string]any{"up": true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != models.IncidentStatusResolved || resolved.ActiveKey != nil {
		t.Fatalf("unexpected resolved incident: %#v", resolved)
	}
	if _, err := service.Acknowledge(first.ID, &ownerID, "operator", "operator"); err == nil {
		t.Fatal("resolved incident was acknowledged again")
	}
	var events []models.IncidentEvent
	if err := db.Where("incident_id = ?", first.ID).Order("created_at ASC").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Type != "action" || events[1].Type != "action" || events[2].Type != "comment" || events[3].Type != "verification" {
		t.Fatalf("unexpected timeline: %#v", events)
	}
}

func TestIncidentSourceKindsDeduplicateResolveAndRecur(t *testing.T) {
	for _, kind := range []models.IncidentKind{
		models.IncidentKindChange,
		models.IncidentKindDrift,
		models.IncidentKindDisk,
	} {
		t.Run(string(kind), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:incident-source-"+string(kind)+"?mode=memory&cache=shared"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&models.Incident{}, &models.IncidentEvent{}); err != nil {
				t.Fatal(err)
			}
			service := NewIncidentService(db)
			observation := IncidentObservation{
				Kind: kind, Severity: "warning", SourceKey: "source-key",
				ResourceType: "component", ResourceID: 17, ResourceName: "component",
				Title: string(kind) + " acceptance", ObservedAt: time.Now(),
			}
			first, err := service.Observe(observation)
			if err != nil {
				t.Fatal(err)
			}
			second, err := service.Observe(observation)
			if err != nil {
				t.Fatal(err)
			}
			if first.ID != second.ID {
				t.Fatalf("active %s incident duplicated: %d != %d", kind, first.ID, second.ID)
			}
			if err := service.ResolveMissing(kind, "component", 17, map[string]struct{}{}, time.Now(), "source recovered"); err != nil {
				t.Fatal(err)
			}
			var resolved models.Incident
			if err := db.First(&resolved, first.ID).Error; err != nil {
				t.Fatal(err)
			}
			if resolved.Status != models.IncidentStatusResolved || resolved.ActiveKey != nil {
				t.Fatalf("unexpected resolved %s incident: %#v", kind, resolved)
			}
			recurrence, err := service.Observe(observation)
			if err != nil {
				t.Fatal(err)
			}
			if recurrence.ID == first.ID {
				t.Fatalf("resolved %s incident was reused for a recurrence", kind)
			}
		})
	}
}
