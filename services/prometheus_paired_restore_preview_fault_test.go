package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
)

func pairedPreviewBusinessState(t *testing.T, f *pointFixture) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, table := range []string{"components", "hosts", "change_plans", "prometheus_recovery_points", "backups", "backup_manifests", "component_backup_pauses"} {
		order := "id"
		if table == "component_backup_pauses" {
			order = "component_id"
		}
		var rows []map[string]any
		if err := f.db.Table(table).Order(order).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		result[table] = pointDigest(raw)
	}
	return result
}
func previewUpdate(t *testing.T, f *pointFixture, table, key string, id uint, column string, value any) {
	t.Helper()
	if err := f.db.Table(table).Where(key+" = ?", id).Update(column, value).Error; err != nil {
		t.Fatal(err)
	}
}
func TestPrometheusPairedRestorePreviewRejectsUnboundMetadataBeforeCommands(t *testing.T) {
	f, point, r := pairedPreviewFixture(t)
	var backup models.Backup
	if err := f.db.First(&backup, point.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, table, key, column string
		id                       uint
		bad, good                any
	}{
		{"draft", "change_plans", "id", "status", f.plan.ID, models.ChangeStatusDraft, models.ChangeStatusFailed},
		{"executing", "change_plans", "id", "status", f.plan.ID, models.ChangeStatusExecuting, models.ChangeStatusFailed},
		{"verified wrong current version", "change_plans", "id", "status", f.plan.ID, models.ChangeStatusVerified, models.ChangeStatusFailed},
		{"unfinished capture", "prometheus_recovery_points", "id", "state", point.ID, "captured", "ready"},
		{"missing backup association", "change_plans", "id", "backup_id", f.plan.ID, nil, point.BackupID},
		{"unknown current version", "components", "id", "version", f.c.ID, "4.0.0", "2.55.0"},
		{"unrelated current version", "components", "id", "version", f.c.ID, "2.56.0", "2.55.0"},
		{"argv changed", "components", "id", "imported_args", f.c.ID, "[]", f.c.ImportedArgs},
		{"config path changed", "components", "id", "config_path", f.c.ID, f.root + "/another.yml", f.c.ConfigPath},
		{"owning host changed", "hosts", "id", "ip", f.c.HostID, "127.0.0.2", "127.0.0.1"},
		{"history changed", "change_plans", "id", "before", f.plan.ID, []byte("{}"), f.plan.Before},
		{"payload changed", "prometheus_recovery_points", "id", "payload", point.ID, []byte("{}"), point.Payload},
		{"manifest changed", "backup_manifests", "backup_id", "component_version", point.BackupID, "3.1.0", "2.55.0"},
		{"backup replaced", "backups", "id", "size", point.BackupID, backup.Size + 1, backup.Size},
		{"unresolved recovery", "component_backup_pauses", "component_id", "status", f.c.ID, "termination_unconfirmed", "resumed"},
		{"version transition", "component_backup_pauses", "component_id", "version_transition_plan_id", f.c.ID, 999, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previewUpdate(t, f, tc.table, tc.key, tc.id, tc.column, tc.bad)
			defer previewUpdate(t, f, tc.table, tc.key, tc.id, tc.column, tc.good)
			state := pairedPreviewBusinessState(t, f)
			files := pairedPreviewFiles(t, f.root)
			pid := f.runner.pid
			r.calls = nil
			proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
			if proof != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("unbound metadata admitted: %+v %v", proof, err)
			}
			if len(r.calls) != 0 {
				t.Fatal("unbound metadata reached host commands")
			}
			if !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
				t.Fatal("preview modified durable business state")
			}
			assertPairedPreviewNoMutation(t, f, r, files, pid)
		})
	}
}
func TestPrometheusPairedRestorePreviewAdmitsPlanTargetAndChecksRetainedDependencies(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	previewUpdate(t, f, "components", "id", f.c.ID, "version", "3.1.0")
	previewUpdate(t, f, "change_plans", "id", f.plan.ID, "status", models.ChangeStatusVerified)
	r.version = "3.1.0"
	content, err := os.ReadFile(f.c.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.c.BinaryPath, append(content, []byte("-version-3.1.0")...), 0755); err != nil {
		t.Fatal(err)
	}
	thanos := models.Component{Name: "retained-sidecar", HostID: f.c.HostID, ClusterID: f.c.ClusterID, Type: models.ComponentThanos, DeployType: "binary", Version: "unknown", Status: models.StatusStopped}
	if err := f.db.Create(&thanos).Error; err != nil {
		t.Fatal(err)
	}
	state := pairedPreviewBusinessState(t, f)
	files := pairedPreviewFiles(t, f.root)
	pid := f.runner.pid
	r.calls = nil
	if proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID); proof != nil || !errors.Is(err, ErrDependencyCompatibility) {
		t.Fatalf("unknown dependency admitted: %+v %v", proof, err)
	}
	if len(r.calls) != 0 || !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
		t.Fatal("dependency rejection had effects")
	}
	assertPairedPreviewNoMutation(t, f, r, files, pid)
	previewUpdate(t, f, "components", "id", thanos.ID, "version", "0.36.0")
	state = pairedPreviewBusinessState(t, f)
	r.calls = nil
	proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
	if err != nil || proof.CurrentVersion != "3.1.0" || proof.HistoricalVersion != "2.55.0" || len(proof.Dependencies) != 1 || proof.Dependencies[0].Compatibility != "compatible" {
		t.Fatalf("matched historical dependency preview: %+v %v", proof, err)
	}
	if !proof.Replacements[0].ContentChanged {
		t.Fatal("binary replacement difference was lost")
	}
	if !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
		t.Fatal("successful preview updated version/history")
	}
	assertPairedPreviewNoMutation(t, f, r, files, pid)
}
func TestPrometheusPairedRestorePreviewRechecksReentrantPauseAndClaimOrder(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	t.Run("reentrant pause", func(t *testing.T) {
		ctx, releaseHost, err := ClaimHostOperation(context.Background(), f.db, f.c.HostID, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer releaseHost()
		ctx, releaseComponent, err := ClaimComponentOperation(ctx, f.db, f.c.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer releaseComponent()
		previewUpdate(t, f, "component_backup_pauses", "component_id", f.c.ID, "status", "termination_unconfirmed")
		defer previewUpdate(t, f, "component_backup_pauses", "component_id", f.c.ID, "status", "resumed")
		r.calls = nil
		if proof, err := f.s.PreviewPrometheusRecoveryPointRestore(ctx, f.plan.ID); proof != nil || err == nil || len(r.calls) != 0 {
			t.Fatalf("reentrant pause bypass: %+v %v", proof, err)
		}
	})
	t.Run("reversed claim order", func(t *testing.T) {
		ctx, release, err := ClaimComponentOperation(context.Background(), f.db, f.c.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		r.calls = nil
		if proof, err := f.s.PreviewPrometheusRecoveryPointRestore(ctx, f.plan.ID); proof != nil || err == nil || len(r.calls) != 0 {
			t.Fatalf("reversed claim order admitted: %+v %v", proof, err)
		}
	})
	t.Run("competing host operation", func(t *testing.T) {
		_, release, err := ClaimHostOperation(context.Background(), f.db, f.c.HostID, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		r.calls = nil
		if proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID); proof != nil || err == nil || len(r.calls) != 0 {
			t.Fatalf("competing operation admitted: %+v %v", proof, err)
		}
	})
	files := pairedPreviewFiles(t, f.root)
	assertPairedPreviewNoMutation(t, f, r, files, f.runner.pid)
}
func TestPrometheusPairedRestorePreviewCancellationAndHostErrorDoNotResume(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	for _, mode := range []string{"cancelled", "host failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			} else {
				r.before = func(context.Context, string) error { return errors.New("private-command-output") }
				defer func() { r.before = nil }()
			}
			files := pairedPreviewFiles(t, f.root)
			state := pairedPreviewBusinessState(t, f)
			pid := f.runner.pid
			r.calls = nil
			proof, err := f.s.PreviewPrometheusRecoveryPointRestore(ctx, f.plan.ID)
			if proof != nil || err == nil || strings.Contains(err.Error(), "private-command-output") {
				t.Fatalf("cancellation/failure contract: %+v %v", proof, err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation identity lost")
			}
			if !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
				t.Fatal("error resumed or rewrote business state")
			}
			assertPairedPreviewNoMutation(t, f, r, files, pid)
		})
	}
}

func TestPrometheusPairedRestorePreviewRedactsSuccessfulMalformedHostReplies(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	for _, kind := range []string{"checksum", "size", "archive entries"} {
		t.Run(kind, func(t *testing.T) {
			injected := false
			r.override = func(cmd string) (string, bool) {
				match := (kind == "checksum" && strings.HasPrefix(cmd, "sha256sum -- ")) || (kind == "size" && strings.HasPrefix(cmd, "stat -c%s -- ")) || (kind == "archive entries" && strings.Contains(cmd, " && tar -tzf "))
				if match {
					injected = true
					return "/private-host-success-sentinel\n", true
				}
				return "", false
			}
			defer func() { r.override = nil }()
			before := pairedPreviewFiles(t, f.root)
			state := pairedPreviewBusinessState(t, f)
			pid := f.runner.pid
			r.calls = nil
			proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
			if !injected || proof != nil || err == nil || strings.Contains(err.Error(), "private-host-success-sentinel") {
				t.Fatalf("malformed reply leaked or admitted: injected=%t proof=%t err=%v", injected, proof != nil, err)
			}
			if !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
				t.Fatal("malformed reply changed business state")
			}
			assertPairedPreviewNoMutation(t, f, r, before, pid)
		})
	}
}

func TestPrometheusPairedRestorePreviewPreservesVerifierOperationalErrors(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	for _, kind := range []error{context.Canceled, coressh.ErrRemoteTerminationUnconfirmed} {
		t.Run(kind.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := false
			r.before = func(_ context.Context, cmd string) error {
				if strings.HasPrefix(cmd, "sha256sum -- ") {
					injected = true
					if kind == context.Canceled {
						cancel()
						return kind
					}
					return errors.Join(kind, errors.New("private-transport-detail"))
				}
				return nil
			}
			defer func() { r.before = nil }()
			files := pairedPreviewFiles(t, f.root)
			pid := f.runner.pid
			r.calls = nil
			proof, err := f.s.PreviewPrometheusRecoveryPointRestore(ctx, f.plan.ID)
			if proof != nil || !errors.Is(err, kind) || strings.Contains(err.Error(), "private-transport-detail") || !injected {
				t.Fatalf("operational identity lost: %v", err)
			}
			assertPairedPreviewNoMutation(t, f, r, files, pid)
		})
	}
}
