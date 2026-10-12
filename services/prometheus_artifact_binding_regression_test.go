package services

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
)

type artifactBindingRegressionCase struct {
	name, recorded, observed string
	reject                   bool
}

// These are artifact identities, not numerical migration comparisons. Removing
// build metadata at any persistence boundary must fail the three drift cases.
func artifactBindingRegressionCases() []artifactBindingRegressionCase {
	return []artifactBindingRegressionCase{
		{"legacy_plain", "2.55.0", "2.55.0", false},
		{"legacy_v_prefix", "v2.55.0", "2.55.0", false},
		{"same_build_v_prefix", "v2.55.0+promlifecycle.1", "2.55.0+promlifecycle.1", false},
		{"different_build", "2.55.0+promlifecycle.1", "2.55.0+promlifecycle.2", true},
		{"build_removed", "2.55.0+promlifecycle.1", "2.55.0", true},
		{"build_added", "2.55.0", "2.55.0+promlifecycle.1", true},
	}
}

func artifactBindingRegressionJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func artifactBindingRegressionBefore(t *testing.T, c *models.Component, scalar string) json.RawMessage {
	t.Helper()
	return artifactBindingRegressionJSON(t, prometheusBefore{Component: c, Version: scalar, Image: c.Image, ImportedArgsSHA256: importedArgsHash(c.ImportedArgs)})
}

func artifactBindingRegressionResult(t *testing.T, err, rejection error, reject bool) {
	t.Helper()
	if reject {
		if !errors.Is(err, rejection) {
			t.Fatalf("artifact identity drift was not rejected at its admission boundary: %v", err)
		}
	} else if err != nil {
		t.Fatalf("compatible historical artifact identity rejected: %v", err)
	}
}

func artifactBindingRegressionUnchanged(t *testing.T, f *pointFixture, before map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, pairedPreviewBusinessState(t, f)) {
		t.Error("metadata admission changed durable business state")
	}
	if len(f.runner.calls) != 0 {
		t.Error("metadata admission dispatched host commands")
	}
}

func TestPrometheusArtifactBindingRegressionPlanSource(t *testing.T) {
	for _, tc := range artifactBindingRegressionCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newPointFixture(t)
			f.c.Version = tc.observed
			historical := *f.c
			historical.Version = tc.recorded
			f.plan.Before = artifactBindingRegressionBefore(t, &historical, tc.recorded)
			state := pairedPreviewBusinessState(t, f)
			err := admitPrometheusPlan(f.plan, f.c, "3.1.0")
			artifactBindingRegressionResult(t, err, ErrPrometheusMigrationAdmission, tc.reject)
			artifactBindingRegressionUnchanged(t, f, state)
		})
	}
}

func TestPrometheusArtifactBindingRegressionSnapshot(t *testing.T) {
	for _, tc := range artifactBindingRegressionCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newPointFixture(t)
			f.c.Version = tc.observed
			f.plan.Before = artifactBindingRegressionBefore(t, f.c, tc.recorded)
			state := pairedPreviewBusinessState(t, f)
			snapshot, err := prometheusUpgradeSnapshot(f.plan, f.c)
			artifactBindingRegressionResult(t, err, ErrPrometheusRecoveryAdmission, tc.reject)
			if !tc.reject && snapshot.Version != tc.recorded {
				t.Fatal("historical scalar identity was rewritten")
			}
			artifactBindingRegressionUnchanged(t, f, state)
		})
	}
}

func TestPrometheusArtifactBindingRegressionBackupManifest(t *testing.T) {
	for _, tc := range artifactBindingRegressionCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newPointFixture(t)
			id := f.c.ID
			backup := &models.Backup{ComponentID: &id, Type: "full", Path: filepath.Join(f.root, "history.tar.gz"), Size: 123}
			if err := f.db.Create(backup).Error; err != nil {
				t.Fatal(err)
			}
			sources, err := backupSourcePaths(f.c, "full")
			if err != nil {
				t.Fatal(err)
			}
			manifest := &models.BackupManifest{BackupID: backup.ID, ComponentType: models.ComponentPrometheus, ComponentVersion: tc.recorded, Scope: "full", Format: "tar.gz", Consistency: "stopped_application", Size: 123, SHA256: strings.Repeat("a", 64), SourcePaths: sources}
			if err := f.db.Create(manifest).Error; err != nil {
				t.Fatal(err)
			}
			state := pairedPreviewBusinessState(t, f)
			_, err = f.s.prometheusBackupBinding(backup, f.c, tc.observed, true)
			artifactBindingRegressionResult(t, err, ErrPrometheusRecoveryAdmission, tc.reject)
			artifactBindingRegressionUnchanged(t, f, state)
		})
	}
}

func TestPrometheusArtifactBindingRegressionRuntimeCallback(t *testing.T) {
	for _, tc := range artifactBindingRegressionCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newPointFixture(t)
			f.c.Version, f.c.Status = tc.recorded, models.StatusStopped
			if err := f.db.Omit("Host", "Cluster").Save(f.c).Error; err != nil {
				t.Fatal(err)
			}
			state := pairedPreviewBusinessState(t, f)
			service := NewChangeService(f.db, nil, nil, f.s, f.root)
			// BinaryDeployer writes its selected package identity back to the
			// component. This callback emulates that boundary, not --version.
			restore := service.prometheusRuntimeRecovery(f.c, func(context.Context) error {
				f.c.Version = tc.observed
				return nil
			})
			err := restore(context.Background())
			artifactBindingRegressionResult(t, err, deployer.ErrBinaryRecoveryUnconfirmed, tc.reject)
			if tc.reject {
				artifactBindingRegressionUnchanged(t, f, state)
			} else {
				var current models.Component
				if err := f.db.First(&current, f.c.ID).Error; err != nil {
					t.Fatal(err)
				}
				if current.Version != tc.recorded || current.Status != models.StatusRunning {
					t.Fatalf("successful restore lost exact historical metadata: version=%s status=%s", current.Version, current.Status)
				}
			}
		})
	}
}

func TestPrometheusArtifactBindingRegressionCurrentRestoreIdentity(t *testing.T) {
	for _, tc := range artifactBindingRegressionCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newPointFixture(t)
			if err := f.db.Model(f.c).Update("version", tc.recorded).Error; err != nil {
				t.Fatal(err)
			}
			supplied := *f.c
			supplied.Version = tc.observed
			state := pairedPreviewBusinessState(t, f)
			_, err := f.s.currentPrometheusRestoreIdentity(context.Background(), &supplied)
			artifactBindingRegressionResult(t, err, ErrPrometheusRecoveryAdmission, tc.reject)
			artifactBindingRegressionUnchanged(t, f, state)
		})
	}
}

func artifactBindingRegressionPoint(t *testing.T, version, target string) (*pointFixture, *models.PrometheusRecoveryPoint) {
	t.Helper()
	f := newPointFixture(t)
	f.c.Version = version
	if err := f.db.Model(f.c).Update("version", version).Error; err != nil {
		t.Fatal(err)
	}
	f.plan.Before = artifactBindingRegressionBefore(t, f.c, version)
	f.plan.After = artifactBindingRegressionJSON(t, upgradeChange{NewVersion: target})
	if err := f.db.Save(f.plan).Error; err != nil {
		t.Fatal(err)
	}
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.First(f.plan, f.plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.runner.calls = nil
	return f, point
}

// Reuse a captured fixture; these boundaries validate persisted metadata before
// host I/O. Rebind its independent records to model an internally consistent
// history, then vary only the scalar identity under review.
func artifactBindingRegressionRebind(t *testing.T, f *pointFixture, point *models.PrometheusRecoveryPoint, historical, scalar, target, current string, status models.ChangeStatus) {
	t.Helper()
	prior := *f.c
	prior.Version = historical
	f.plan.Before = artifactBindingRegressionBefore(t, &prior, scalar)
	f.plan.After = artifactBindingRegressionJSON(t, upgradeChange{NewVersion: target})
	f.plan.Status = status
	if err := f.db.Save(f.plan).Error; err != nil {
		t.Fatal(err)
	}
	var payload prometheusRecoveryPayload
	if err := json.Unmarshal(point.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.BeforeSHA256, payload.AfterSHA256 = pointDigest(f.plan.Before), pointDigest(f.plan.After)
	payload.Binding = makeUnitSnapshotBinding(&prior, importedArgsHash(prior.ImportedArgs))
	payload.Archive.Version = historical
	point.Payload = artifactBindingRegressionJSON(t, payload)
	if err := f.db.Model(point).Update("payload", point.Payload).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&models.BackupManifest{}).Where("backup_id = ?", point.BackupID).Update("component_version", historical).Error; err != nil {
		t.Fatal(err)
	}
	f.c.Version = current
	if err := f.db.Model(f.c).Update("version", current).Error; err != nil {
		t.Fatal(err)
	}
	f.runner.calls = nil
}

func TestPrometheusArtifactBindingRegressionRecoveryPointHistory(t *testing.T) {
	f, point := artifactBindingRegressionPoint(t, "2.55.0", "3.1.0")
	for _, tc := range artifactBindingRegressionCases() {
		t.Run(tc.name, func(t *testing.T) {
			artifactBindingRegressionRebind(t, f, point, tc.observed, tc.recorded, "3.1.0", tc.observed, models.ChangeStatusFailed)
			state := pairedPreviewBusinessState(t, f)
			_, _, err := f.s.loadRecoveryPointHistory(context.Background(), f.plan, f.c)
			artifactBindingRegressionResult(t, err, ErrPrometheusRecoveryPoint, tc.reject)
			artifactBindingRegressionUnchanged(t, f, state)
		})
	}
}

func TestPrometheusArtifactBindingRegressionPairedPreview(t *testing.T) {
	f, point := artifactBindingRegressionPoint(t, "2.55.0", "3.1.0")
	for _, phase := range []string{"verified_target", "failed_historical"} {
		t.Run(phase, func(t *testing.T) {
			for _, tc := range artifactBindingRegressionCases() {
				t.Run(tc.name, func(t *testing.T) {
					historical, target, current := tc.recorded, "3.1.0", tc.observed
					status := models.ChangeStatusFailed
					if phase == "verified_target" {
						historical = "2.55.0"
						target = strings.Replace(tc.recorded, "2.55.0", "3.1.0", 1)
						current = strings.Replace(tc.observed, "2.55.0", "3.1.0", 1)
						status = models.ChangeStatusVerified
					}
					artifactBindingRegressionRebind(t, f, point, historical, historical, target, current, status)
					state := pairedPreviewBusinessState(t, f)
					_, _, _, _, err := f.s.admitPairedRestorePreview(context.Background(), f.plan, f.c)
					artifactBindingRegressionResult(t, err, ErrPrometheusRecoveryPoint, tc.reject)
					artifactBindingRegressionUnchanged(t, f, state)
				})
			}
		})
	}
}
