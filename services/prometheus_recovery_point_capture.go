package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/core/deployer"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
	"path/filepath"
	"time"
)

// CreatePrometheusRecoveryPoint intentionally quiesces and resumes the service.
// A completed point is historical evidence, not permission to mutate or restore.
func (s *BackupService) CreatePrometheusRecoveryPoint(ctx context.Context, planID uint) (*models.PrometheusRecoveryPoint, error) {
	var point *models.PrometheusRecoveryPoint
	err := s.withRecoveryPointClaim(ctx, planID, false, func(ctx context.Context, plan *models.ChangePlan, c *models.Component) error {
		var err error
		point, err = s.capturePrometheusRecoveryPoint(ctx, plan, c)
		return err
	})
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryPoint, err)
	}
	return point, nil
}
func (s *BackupService) admitRecoveryPointCapture(ctx context.Context, plan *models.ChangePlan, c *models.Component) error {
	if plan.Status != models.ChangeStatusExecuting || plan.BackupID != nil {
		return pointReject("executing plan without an existing recovery point required")
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.PrometheusRecoveryPoint{}).Where("plan_id = ?", plan.ID).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return pointReject("recovery point is immutable; do not recapture history")
	}
	if err := s.db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ? AND version_transition_plan_id <> 0", c.ID).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return pointReject("cannot capture across an active version transition")
	}
	var desired upgradeChange
	if err := json.Unmarshal(plan.After, &desired); err != nil {
		return pointReject("invalid planned upgrade")
	}
	if err := admitPrometheusPlan(plan, c, desired.NewVersion); err != nil {
		return err
	}
	before, err := prometheusUpgradeSnapshot(plan, c)
	if err != nil {
		return err
	}
	if !unitSnapshotHash.MatchString(before.ImportedArgsSHA256) {
		return pointReject("fresh historical argv hash required")
	}
	if !validUnitSnapshotPath(s.backupRoot) || !validUnitSnapshotPath(c.BinaryPath) {
		return pointReject("canonical artifact root and recorded executable required")
	}
	binding := makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs))
	// Capture and historical verification must derive exactly the same identity,
	// including version spelling and explicit executable path.
	before.Component.Host = c.Host
	if makeUnitSnapshotBinding(before.Component, before.ImportedArgsSHA256) != binding {
		return pointReject("historical component binding differs from current identity")
	}
	if c.ServiceName == "" || !validUnitSnapshotBinding(binding) {
		return pointReject("valid recorded systemd identity required")
	}
	sources := []string{c.BinaryPath, c.ConfigPath, c.DataPath}
	for i, path := range sources {
		if !validUnitSnapshotPath(path) || recoveryPathsOverlap(s.backupRoot, path) {
			return pointReject("artifact root overlaps or cannot bind source")
		}
		for _, other := range sources[:i] {
			if recoveryPathsOverlap(path, other) {
				return pointReject("executable and application roots overlap")
			}
		}
	}
	return nil
}
func (s *BackupService) capturePrometheusRecoveryPoint(ctx context.Context, plan *models.ChangePlan, c *models.Component) (point *models.PrometheusRecoveryPoint, resultErr error) {
	if err := s.admitRecoveryPointCapture(ctx, plan, c); err != nil {
		return nil, err
	}
	r, release, err := s.pointRunner(ctx, c)
	if err != nil {
		return nil, err
	}
	defer release()
	scoped := s.onPointRunner(r)
	scoped.db = s.db.WithContext(ctx)
	if err := scoped.verifyPrometheusResolvedRoots(ctx, c); err != nil {
		return nil, err
	}
	state, files, err := snapshotInventory(ctx, r, deployer.CanonicalServiceName(c.ServiceName)+".service")
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if recoveryPathsOverlap(s.backupRoot, file.Path) || recoveryPathsOverlap(c.BinaryPath, file.Path) {
			return nil, pointReject("startup source overlaps executable or artifacts")
		}
	}
	binary, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: c.BinaryPath, Role: "binary"}, recoveryPointMaxBinary)
	if err != nil {
		return nil, err
	}
	runtime, err := readRecoveryPointRuntime(ctx, r, c, state, binary)
	if err != nil {
		return nil, err
	}
	binding := makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs))
	startup, err := captureUnitSnapshot(ctx, r, s.backupRoot, binding)
	if err != nil {
		return nil, err
	}
	dir, err := createRecoveryPointDirectory(ctx, r, s.backupRoot, c.ID)
	if err != nil {
		return nil, err
	}
	if err := captureRecoveryPointBinary(ctx, r, dir, binary); err != nil {
		return nil, err
	}
	p := &prometheusRecoveryPayload{Schema: 1, BeforeSHA256: pointDigest(plan.Before), AfterSHA256: pointDigest(plan.After), Binding: binding, Startup: *startup, Binary: recoveryPointBinary{Directory: dir, File: binary}, Runtime: runtime}
	active, err := compareRecoveryPointBaseline(ctx, r, c, p, false)
	if err != nil {
		return nil, err
	}
	if active["MainPID"] != runtime.PID {
		return nil, pointReject("main process changed before pause")
	}

	paused, resumeAttempted := false, false
	defer func() {
		if resultErr == nil || !paused {
			return
		}
		if resumeAttempted || errors.Is(resultErr, coressh.ErrRemoteTerminationUnconfirmed) {
			resultErr = errors.Join(resultErr, s.blockRecoveryPoint(c.ID, resultErr))
			return
		}
		if cleanupErr := s.resumeRecoveryPointOriginal(c, p); cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr, s.blockRecoveryPoint(c.ID, cleanupErr))
		}
	}()
	if err := s.beginRecoveryPointPause(ctx, c, p, r); err != nil {
		return nil, err
	}
	paused = true
	if err := scoped.stopComponent(ctx, c); err != nil {
		return nil, err
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, p, true); err != nil {
		return nil, err
	}
	if err := scoped.verifyPrometheusResolvedRoots(ctx, c); err != nil {
		return nil, err
	}
	sources, err := backupSourcePaths(c, "full")
	if err != nil {
		return nil, err
	}
	archive := filepath.Join(dir, "payload.tar.gz")
	if _, err := scoped.createArchiveArtifact(ctx, r, archive, sources, "full"); err != nil {
		return nil, err
	}
	if _, err := snapshotRun(ctx, r, "sync -f "+utils.QuoteShell(archive)+" && sync -f "+utils.QuoteShell(dir)); err != nil {
		return nil, err
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, p, true); err != nil {
		return nil, err
	}
	digest, size, err := inspectArtifact(ctx, r, archive, "full", "tar.gz", sources)
	if err != nil {
		return nil, err
	}
	p.Archive = recoveryPointArchive{Path: archive, SHA256: digest, Version: c.Version, Size: size, Sources: sources}
	if err := verifyRecoveryPointBinary(ctx, r, p.Binary); err != nil {
		return nil, err
	}
	if err := checkRecoveryPointDirectory(ctx, r, dir); err != nil {
		return nil, err
	}
	point, err = s.persistRecoveryPoint(ctx, plan, c, p)
	if err != nil {
		return nil, err
	}
	// Artifact publication has completed. A later failure must retain this
	// captured record, while returning no successfully completed capture.
	resumeAttempted = true
	if err := s.resumeRecoveryPointOriginal(c, p); err != nil {
		return nil, err
	}
	paused = false
	if err := s.checkRecoveryPointCaptureIdentity(ctx, plan, c); err != nil {
		return nil, err
	}
	if err := pointRecordError(s.db.WithContext(ctx).Model(&models.PrometheusRecoveryPoint{}).Where("id = ? AND state = ?", point.ID, "captured").Update("state", "ready"), "ready point"); err != nil {
		return nil, err
	}
	point.State = "ready"
	return point, nil
}
func (s *BackupService) checkRecoveryPointCaptureIdentity(ctx context.Context, plan *models.ChangePlan, c *models.Component) error {
	var current models.Component
	var stored models.ChangePlan
	if err := s.db.WithContext(ctx).Preload("Host").First(&current, c.ID).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).First(&stored, plan.ID).Error; err != nil {
		return err
	}
	if stored.ResourceID != c.ID || stored.Kind != models.ChangeKindUpgrade || stored.ResourceType != "component" || stored.Status != models.ChangeStatusExecuting || pointDigest(stored.Before) != pointDigest(plan.Before) || pointDigest(stored.After) != pointDigest(plan.After) || makeUnitSnapshotBinding(&current, importedArgsHash(current.ImportedArgs)) != makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)) {
		return pointReject("plan or component identity changed during capture")
	}
	return nil
}
func (s *BackupService) persistRecoveryPoint(ctx context.Context, plan *models.ChangePlan, c *models.Component, p *prometheusRecoveryPayload) (*models.PrometheusRecoveryPoint, error) {
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > recoveryPointMaxPayload {
		return nil, pointReject("cannot encode recovery point")
	}
	cid, clusterID := c.ID, c.ClusterID
	backup := &models.Backup{ComponentID: &cid, ClusterID: &clusterID, Name: fmt.Sprintf("paired-change-%d", plan.ID), Path: p.Archive.Path, Size: p.Archive.Size, Type: "full"}
	now := s.now()
	manifest := &models.BackupManifest{Scope: "full", Format: "tar.gz", SourcePaths: p.Archive.Sources, ComponentType: c.Type, ComponentVersion: c.Version, SHA256: p.Archive.SHA256, Size: p.Archive.Size, Consistency: "stopped_application", ScopeLimit: "Stopped configuration/data archive. Executable and startup evidence are pinned separately by the paired recovery point; restore admission is not included.", VerifiedAt: &now}
	point := &models.PrometheusRecoveryPoint{PlanID: plan.ID, ComponentID: c.ID, HostID: c.HostID, State: "captured", Payload: raw}
	var desired upgradeChange
	if err := json.Unmarshal(plan.After, &desired); err != nil {
		return nil, pointReject("invalid planned upgrade")
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		check := *s
		check.db = tx
		if err := check.checkRecoveryPointCaptureIdentity(ctx, plan, c); err != nil {
			return err
		}
		if err := pointRecordError(tx.Create(backup), "full backup"); err != nil {
			return err
		}
		manifest.BackupID = backup.ID
		if err := pointRecordError(tx.Create(manifest), "full manifest"); err != nil {
			return err
		}
		point.BackupID = backup.ID
		if err := pointRecordError(tx.Create(point), "captured point"); err != nil {
			return err
		}
		association := tx.Model(&models.ChangePlan{}).Where("id = ? AND status = ? AND backup_id IS NULL AND paired_recovery_point_id IS NULL", plan.ID, models.ChangeStatusExecuting)
		if desired.PairedRecovery {
			return pointRecordError(association.Update("paired_recovery_point_id", point.ID), "paired plan association")
		}
		return pointRecordError(association.Update("backup_id", backup.ID), "plan association")
	})
	if err != nil {
		return nil, err
	}
	return point, nil
}

// A separate transport outlives a cancelled capture only for this bounded
// compensation. The refreshed pause timestamp fences old startup watchdogs.
func (s *BackupService) resumeRecoveryPointOriginal(c *models.Component, p *prometheusRecoveryPayload) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := pointRecordError(s.db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ?", c.ID).Update("updated_at", s.now()), "pause timestamp"); err != nil {
		return err
	}
	r, release, err := s.pointRunner(ctx, c)
	if err != nil {
		return err
	}
	defer release()
	var pause ComponentBackupPause
	if err := s.db.WithContext(ctx).First(&pause, "component_id = ?", c.ID).Error; err != nil {
		return err
	}
	state, _, err := readUnitSnapshotState(ctx, r, p.Binding.Unit)
	if err != nil {
		return err
	}
	if state["ActiveState"] == "active" {
		if _, err := compareRecoveryPointBaseline(ctx, r, c, p, false); err != nil {
			return err
		}
		return pointRecordError(s.db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ?", c.ID).Updates(map[string]any{"status": "resumed", "error": ""}), "resumed pause")
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, p, true); err != nil {
		return err
	}
	if !pause.WasRunning {
		return pointRecordError(s.db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ?", c.ID).Updates(map[string]any{"status": "resumed", "error": ""}), "originally stopped pause")
	}
	if err := restartRecoveryPointOriginal(ctx, r, c); err != nil {
		return err
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, p, false); err != nil {
		return err
	}
	return pointRecordError(s.db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ?", c.ID).Updates(map[string]any{"status": "resumed", "error": ""}), "verified resumed pause")
}
func checkRecoveryPointDirectory(ctx context.Context, r hostCommandRunner, dir string) error {
	if err := snapshotPrivateDirectory(ctx, r, dir); err != nil {
		return err
	}
	out, err := snapshotRun(ctx, r, "find "+utils.QuoteShell(dir)+" -mindepth 1 -maxdepth 1 -printf '%f\\n' | LC_ALL=C sort")
	if err != nil {
		return err
	}
	if out != "payload.tar.gz\nprometheus\n" {
		return pointReject("unexpected paired artifact entries")
	}
	return nil
}

func (s *BackupService) blockRecoveryPoint(id uint, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	scoped := *s
	scoped.db = s.db.WithContext(ctx)
	return scoped.blockComponentRecovery(id, cause)
}
func restartRecoveryPointOriginal(ctx context.Context, r hostCommandRunner, c *models.Component) error {
	if _, err := snapshotRun(ctx, r, "systemctl restart "+utils.QuoteShell(c.ServiceName)); err != nil {
		return err
	}
	ready := componentReadinessURL(c)
	for attempt := 0; attempt < 10; attempt++ {
		_, err := snapshotRun(ctx, r, "systemctl is-active --quiet "+utils.QuoteShell(c.ServiceName))
		if err == nil && ready != "" {
			_, err = snapshotRun(ctx, r, "curl -fsS --max-time 3 -- "+utils.QuoteShell(ready)+" >/dev/null")
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return pointReject("original service readiness was not confirmed")
}

func (s *BackupService) beginRecoveryPointPause(ctx context.Context, c *models.Component, p *prometheusRecoveryPayload, r hostCommandRunner) error {
	var previous ComponentBackupPause
	err := s.db.WithContext(ctx).First(&previous, "component_id = ?", c.ID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && (previous.Status != "resumed" || previous.VersionTransitionPlanID != 0) {
		return pointReject("unresolved recovery blocks a new pause")
	}
	state, _, err := readUnitSnapshotState(ctx, r, p.Binding.Unit)
	if err != nil {
		return err
	}
	if state["ActiveState"] != "active" || state["SubState"] != "running" || state["MainPID"] != p.Runtime.PID {
		return pointReject("original runtime changed before durable pause")
	}
	// The very first durable pause is fail-closed: there is no intermediate
	// generic paused row that a controller crash could automatically restart.
	pause := ComponentBackupPause{ComponentID: c.ID, Status: "termination_unconfirmed", Error: "paired capture requires verified recovery if interrupted", WasRunning: true, UpdatedAt: s.now()}
	return pointRecordError(s.db.WithContext(ctx).Save(&pause), "paired capture crash fence")
}
