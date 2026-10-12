package services

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	"strings"
	"testing"
)

func snapshotServiceFixture(t *testing.T) (*BackupService, *models.Component, *snapshotFixtureRunner) {
	t.Helper()
	r, root, _, _ := snapshotFixture(t)
	s, db, _ := newBackupTestService(t)
	s.backupRoot = root
	if err := db.AutoMigrate(&models.Component{}, &models.Host{}, &models.HostOperationClaim{}); err != nil {
		t.Fatal(err)
	}
	h := models.Host{ID: 7, Name: "snapshot-host", IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&h).Error; err != nil {
		t.Fatal(err)
	}
	c := &models.Component{ID: 42, HostID: h.ID, Host: h, Name: "prom-test", Type: models.ComponentPrometheus, DeployType: "binary", ServiceName: "prom-test.service", Version: "2.55.0", ImportedArgs: "private-argv-sentinel"}
	if err := db.Create(c).Error; err != nil {
		t.Fatal(err)
	}
	s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return r, nil }
	return s, c, r
}

func TestComponentUnitSnapshotUsesClaimsAndFreshIdentity(t *testing.T) {
	s, c, _ := snapshotServiceFixture(t)
	ctx, release, err := ClaimHostOperation(context.Background(), s.db, c.HostID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := s.CaptureComponentUnitSnapshot(context.Background(), c.ID); ref != nil || !errors.Is(err, ErrComponentBusy) {
		t.Fatalf("host lock bypass: %v", err)
	}
	ref, err := s.CaptureComponentUnitSnapshot(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	proof, err := s.VerifyComponentUnitSnapshot(context.Background(), ref, true)
	if err != nil || !proof.LiveBaselineVerified || proof.PresentFiles != 3 {
		t.Fatalf("verification: %v %v", proof, err)
	}
	fresh := NewBackupServiceWithRoot(s.db, s.backupRoot)
	fresh.runnerFactory = s.runnerFactory
	if _, err := fresh.VerifyComponentUnitSnapshot(context.Background(), ref, false); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(c).Update("imported_args", "changed-argv").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.VerifyComponentUnitSnapshot(context.Background(), ref, false); err == nil {
		t.Fatal("DB identity drift admitted")
	}
	var count int64
	for _, model := range []any{&models.ComponentOperationClaim{}, &models.HostOperationClaim{}} {
		s.db.Model(model).Count(&count)
		if count != 0 {
			t.Fatal("claim leaked")
		}
	}
}

func TestComponentUnitSnapshotAdmissionBeforeHostCommands(t *testing.T) {
	for _, name := range []string{"missing", "unsupported", "component-busy", "lock-order", "cancelled", "host-changed"} {
		t.Run(name, func(t *testing.T) {
			s, c, r := snapshotServiceFixture(t)
			ctx := context.Background()
			id := c.ID
			switch name {
			case "missing":
				id++
			case "unsupported":
				s.db.Model(c).Update("deploy_type", "docker")
			case "component-busy":
				_, release, err := ClaimComponentOperation(ctx, s.db, id, "test")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "lock-order":
				var release func()
				var err error
				ctx, release, err = ClaimComponentOperation(ctx, s.db, id, "test")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "host-changed":
				if err := s.db.Model(&models.Component{}).Where("id = ?", c.ID).Update("host_id", uint(999)).Error; err != nil {
					t.Fatal(err)
				}
				var actual models.Component
				if err := s.db.First(&actual, c.ID).Error; err != nil {
					t.Fatal(err)
				}
				if actual.HostID != 999 {
					t.Fatal("fixture did not change host association")
				}
			}
			ref, err := s.CaptureComponentUnitSnapshot(ctx, id)
			if err == nil || ref != nil || r.showCount != 0 {
				t.Fatalf("unsafe request reached host: %v", err)
			}
			if strings.Contains(err.Error(), "private-argv-sentinel") {
				t.Fatal("secret in error")
			}
		})
	}
}
