package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func journalGuardFixture(t *testing.T) (*gorm.DB, []*models.Component) {
	t.Helper()
	db := lifecycleQueueDB(t)
	if err := db.AutoMigrate(&models.PrometheusRestoreTransaction{}, &ComponentRestoreCheckpoint{}); err != nil {
		t.Fatal(err)
	}
	hosts := []models.Host{{Name: "journal-host-a", IP: "127.0.0.1"}, {Name: "journal-host-b", IP: "127.0.0.2"}}
	for i := range hosts {
		if err := db.Create(&hosts[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	cluster := models.Cluster{Name: "journal-cluster"}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	components := []*models.Component{
		{Name: "journal-owner", HostID: hosts[0].ID, ClusterID: cluster.ID, Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0"},
		{Name: "journal-neighbour", HostID: hosts[0].ID, ClusterID: cluster.ID, Type: models.ComponentThanos, DeployType: "binary", Version: "0.37.2"},
		{Name: "journal-independent", HostID: hosts[1].ID, ClusterID: cluster.ID, Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0"},
	}
	for _, c := range components {
		if err := db.Create(c).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, components
}

func journalGuardRecord(t *testing.T, db *gorm.DB, c *models.Component) *models.PrometheusRestoreTransaction {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"Schema": 1, "PlanID": 7, "PointID": 8, "BackupID": 9,
		"Current":     unitSnapshotBinding{ComponentID: c.ID, HostID: c.HostID, Unit: "journal-prometheus.service", RuntimeSHA256: strings.Repeat("1", 64), HostSHA256: strings.Repeat("2", 64)},
		"PointSHA256": strings.Repeat("3", 64), "BeforeSHA256": strings.Repeat("4", 64), "AfterSHA256": strings.Repeat("5", 64),
		"PlanStatus": "failed", "CurrentVersion": "2.55.0", "HistoricalVersion": "2.55.0",
		"ObservedAt": time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	cid, hid := c.ID, c.HostID
	row := &models.PrometheusRestoreTransaction{
		PlanID: 7, PointID: 8, BackupID: 9, ComponentID: cid, HostID: hid,
		ActiveComponentID: &cid, ActiveHostID: &hid, Token: strings.Repeat("a", 64),
		Phase: "prepared", Revision: 1, Payload: raw, PayloadSHA256: pointDigest(raw),
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func journalAssertClaimsEmpty(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, model := range []any{&models.ComponentOperationClaim{}, &models.HostOperationClaim{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("leaked claims: %d %v", count, err)
		}
	}
}

func TestPrometheusRestoreJournalFencesNormalAndRecoveryClaims(t *testing.T) {
	for _, kind := range []string{"restart", "upgrade", "backup", "restore", "restore-recovery", "paired-restore-recovery", "caller-invented"} {
		t.Run(kind, func(t *testing.T) {
			db, cs := journalGuardFixture(t)
			journalGuardRecord(t, db, cs[0])
			for _, c := range cs[:2] {
				if _, release, err := ClaimComponentOperation(context.Background(), db, c.ID, kind); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
					if release != nil {
						release()
					}
					t.Fatalf("component %d bypassed pending paired restore journal: %v", c.ID, err)
				}
			}
			if _, release, err := ClaimHostOperation(context.Background(), db, cs[0].HostID, kind); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				if release != nil {
					release()
				}
				t.Fatalf("host bypassed pending paired restore journal: %v", err)
			}
			_, release, err := ClaimComponentOperation(context.Background(), db, cs[2].ID, kind)
			if err != nil {
				t.Fatalf("unrelated host blocked: %v", err)
			}
			release()
			_, release, err = ClaimHostOperation(context.Background(), db, cs[2].HostID, kind)
			if err != nil {
				t.Fatalf("unrelated host claim blocked: %v", err)
			}
			release()
			journalAssertClaimsEmpty(t, db)
		})
	}
}

func TestPrometheusRestoreJournalFencesReentrantClaims(t *testing.T) {
	db, cs := journalGuardFixture(t)
	ctx, hostRelease, err := ClaimHostOperation(context.Background(), db, cs[0].HostID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer hostRelease()
	ctx, componentRelease, err := ClaimComponentOperation(ctx, db, cs[0].ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer componentRelease()
	journalGuardRecord(t, db, cs[0])
	if _, release, err := ClaimComponentOperation(ctx, db, cs[0].ID, "restore-recovery"); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		if release != nil {
			release()
		}
		t.Fatalf("reentrant component bypassed journal: %v", err)
	}
	if _, release, err := ClaimHostOperation(ctx, db, cs[0].HostID, "owner"); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		if release != nil {
			release()
		}
		t.Fatalf("reentrant host bypassed journal: %v", err)
	}
	var count int64
	if err := db.Model(&models.ComponentOperationClaim{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("rejection removed original component claim", err)
	}
	if err := db.Model(&models.HostOperationClaim{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("rejection removed original host claim", err)
	}
}

func TestPrometheusRestoreJournalUnknownAndCorruptRecordsStayFenced(t *testing.T) {
	cases := []struct {
		name    string
		updates map[string]any
	}{
		{"later-phase", map[string]any{"phase": "applying"}},
		{"unknown-phase", map[string]any{"phase": "unrecognized"}},
		{"payload", map[string]any{"payload": "not-json"}},
		{"checksum", map[string]any{"payload_sha256": strings.Repeat("0", 64)}},
		{"revision", map[string]any{"revision": uint64(2)}},
		{"lost-active-host", map[string]any{"active_host_id": nil}},
		{"lost-active-component", map[string]any{"active_component_id": nil}},
		{"mismatched-host", map[string]any{"host_id": 999}},
		{"mismatched-plan", map[string]any{"plan_id": 999}},
		{"mismatched-point", map[string]any{"point_id": 999}},
		{"mismatched-backup", map[string]any{"backup_id": 999}},
		{"aborted-with-keys", map[string]any{"phase": "aborted", "revision": 2, "finished_at": time.Now()}},
		{"aborted-without-finish", map[string]any{"phase": "aborted", "revision": 2, "active_host_id": nil, "active_component_id": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cs := journalGuardFixture(t)
			row := journalGuardRecord(t, db, cs[0])
			if err := db.Model(row).Updates(tc.updates).Error; err != nil {
				t.Fatal(err)
			}
			if _, release, err := ClaimHostOperation(context.Background(), db, cs[0].HostID, "restart"); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				if release != nil {
					release()
				}
				t.Fatalf("invalid journal failed open: %v", err)
			}
			if err := RecoverInterruptedComponentRestores(context.Background(), db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("startup bypassed invalid journal: %v", err)
			}
			journalAssertClaimsEmpty(t, db)
		})
	}
}

func TestPrometheusRestoreJournalPreparedSurvivesGenericStartup(t *testing.T) {
	db, cs := journalGuardFixture(t)
	row := journalGuardRecord(t, db, cs[0])
	if err := RecoverInterruptedLifecycle(db); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedComponentRestores(context.Background(), db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("generic startup bypassed prepared paired journal: %v", err)
	}
	var got models.PrometheusRestoreTransaction
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Phase != "prepared" || got.Revision != 1 || got.ActiveHostID == nil || got.ActiveComponentID == nil || string(got.Payload) != string(row.Payload) {
		t.Fatal("startup changed the durable journal")
	}
}

func TestPrometheusRestoreJournalAbortedHistoryReleasesUniqueOwnership(t *testing.T) {
	db, cs := journalGuardFixture(t)
	row := journalGuardRecord(t, db, cs[0])
	duplicate := *row
	duplicate.ID = 0
	duplicate.Token = strings.Repeat("b", 64)
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("two active transactions owned one component/host")
	}
	if err := db.Model(row).Updates(map[string]any{"phase": "aborted", "revision": 2, "active_host_id": nil, "active_component_id": nil, "finished_at": time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if _, release, err := ClaimHostOperation(context.Background(), db, cs[0].HostID, "restart"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	duplicate.ID = 0
	if err := db.Create(&duplicate).Error; err != nil {
		t.Fatal("terminal history retained live unique ownership", err)
	}
	var count int64
	if err := db.Model(&models.PrometheusRestoreTransaction{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatal("immutable terminal history lost", err)
	}
}

func TestPrometheusRestoreJournalPublicJSONHidesPrivateEvidence(t *testing.T) {
	db, cs := journalGuardFixture(t)
	row := journalGuardRecord(t, db, cs[0])
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{row.Token, row.PayloadSHA256, "RuntimeSHA256", "active_host_id", "active_component_id", "payload"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("public transaction exposed private evidence")
		}
	}
}
