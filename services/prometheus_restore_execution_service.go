package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
)

func (s *BackupService) ExecutePrometheusPairedRestore(ctx context.Context, id uint) (result *models.PrometheusRestoreTransaction, resultErr error) {
	s = s.quietRestoreStageService()
	if err := s.requireRestoreStageDatabase(ctx, id); err != nil {
		return nil, executionSafeFailure(ctx, err)
	}
	callerContext := ctx
	defer func() {
		if resultErr != nil {
			read, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var latest models.PrometheusRestoreTransaction
			if s.db.WithContext(read).First(&latest, id).Error == nil {
				if _, err := decodeRestoreJournal(&latest); err == nil {
					result = &latest
				}
			}
			resultErr = executionSafeFailure(callerContext, resultErr)
		}
	}()
	var row models.PrometheusRestoreTransaction
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	if _, err := decodeRestoreJournal(&row); err != nil {
		return nil, err
	}
	result = &row
	if row.Phase == "restore_completed" {
		return &row, nil
	}
	if row.Phase == "restore_compensated" {
		return &row, restoreJournalReject("requested restore failed; previous state recovered")
	}
	if row.Phase != "restore_staged" {
		return &row, restoreJournalReject("pending execution requires exclusive startup recovery")
	}
	// Public execution cannot borrow a reentrant or surviving foreground owner.
	if ctx.Value(hostClaimContextKey{}) != nil || ctx.Value(componentClaimContextKey{}) != nil {
		return &row, restoreJournalReject("fresh execution claims required")
	}
	if !s.db.Migrator().HasColumn(&models.PrometheusRestoreTransaction{}, "execution") || !s.db.Migrator().HasColumn(&models.PrometheusRestoreTransaction{}, "execution_sha256") {
		return &row, restoreJournalReject("execution migration required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, restoreExecutionAccessKey{}, restoreExecutionAccess{row})
	ctx, releaseH, err := ClaimHostOperation(ctx, s.db, row.HostID, "paired-restore-execution")
	if err != nil {
		return &row, err
	}
	defer releaseH()
	ctx, releaseC, err := ClaimComponentOperation(ctx, s.db, row.ComponentID, "paired-restore-execution")
	if err != nil {
		return &row, err
	}
	defer releaseC()
	c, err := s.executionOwner(ctx, &row)
	if err != nil {
		return &row, err
	}
	r, release, err := s.pointRunner(ctx, c)
	if err != nil {
		return &row, err
	}
	defer release()
	stage, err := decodeRestoreStage(&row)
	if err != nil {
		return &row, err
	}
	scoped := s.onPointRunner(r)
	if err := scoped.verifyRestoreStageHost(ctx, r, &row, c, stage); err != nil {
		return &row, err
	}
	var point models.PrometheusRecoveryPoint
	if err := s.db.WithContext(ctx).First(&point, row.PointID).Error; err != nil {
		return &row, err
	}
	claims, err := heldRestoreStageClaims(ctx, &row)
	if err != nil {
		return &row, err
	}
	p := &restoreExecutionPayload{Schema: 1, TransactionID: row.ID, StageRevision: row.Revision, StageSHA256: row.StageSHA256, Historical: append(json.RawMessage(nil), point.Payload...), Claims: claims, Fence: stage.Fence, Generations: []string{stage.Fence.Generation}, StartedAt: s.now().UTC()}
	// Validate all target roots and copied manifests before persisting permission
	// to switch. After publication, only execution evidence is used.
	if _, _, _, err := s.executionSets(ctx, r, &row, p, c); err != nil {
		return &row, err
	}
	next, err := s.advanceRestoreExecution(ctx, &row, "restore_executing", p)
	if err != nil {
		return &row, err
	}
	result = next
	result, resultErr = scoped.performRestoreExecution(ctx, r, next, c, false)
	if resultErr == nil {
		return
	}
	if errors.Is(resultErr, coressh.ErrRemoteTerminationUnconfirmed) || errors.Is(resultErr, errRestoreStageFenceBusy) {
		return
	}
	// Re-read after any failed COMMIT. Never compensate an outcome that actually
	// committed, or race an owner whose identity cannot be established.
	read, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	var durable models.PrometheusRestoreTransaction
	readErr := s.db.WithContext(read).First(&durable, id).Error
	cancelRead()
	if readErr != nil {
		return result, resultErr
	}
	if _, err := decodeRestoreJournal(&durable); err != nil {
		return &durable, resultErr
	}
	if durable.Phase == "restore_completed" {
		return &durable, nil
	}
	if durable.Phase != "restore_executing" && durable.Phase != "restore_starting" {
		return &durable, resultErr
	}
	compensation, cancelCompensation := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancelCompensation()
	compensated, compErr := s.compensateRestoreExecution(compensation, &durable)
	if compErr != nil {
		return compensated, errors.Join(resultErr, compErr)
	}
	return compensated, resultErr
}

func (s *BackupService) compensateRestoreExecution(ctx context.Context, row *models.PrometheusRestoreTransaction) (*models.PrometheusRestoreTransaction, error) {
	c, err := s.executionOwner(ctx, row)
	if err != nil {
		return row, err
	}
	p, _, err := decodeRestoreExecution(row)
	if err != nil {
		return row, err
	}
	if row.Phase != "restore_compensating" {
		next, err := s.advanceRestoreExecution(ctx, row, "restore_compensating", p)
		if err != nil {
			return row, err
		}
		row = next
	}
	r, release, err := s.pointRunner(ctx, c)
	if err != nil {
		return row, err
	}
	defer release()
	return s.onPointRunner(r).performRestoreExecution(ctx, r, row, c, true)
}
func (s *BackupService) performRestoreExecution(ctx context.Context, r hostCommandRunner, row *models.PrometheusRestoreTransaction, c *models.Component, current bool) (*models.PrometheusRestoreTransaction, error) {
	p, _, err := decodeRestoreExecution(row)
	if err != nil {
		return row, err
	}
	oldSet, historical, manifests, err := s.executionSets(ctx, r, row, p, c)
	if err != nil {
		return row, err
	}
	desired := historical
	manifest := manifests[1]
	if current {
		desired = oldSet
		manifest = manifests[0]
	}
	configManifest := &models.BackupManifest{
		Scope: "full", Format: "tar.gz", SourcePaths: desired.Archive.Sources,
		SHA256: desired.Archive.SHA256, Size: desired.Archive.Size,
	}
	configs, err := s.prepareRestoredConfigs(ctx, r, c, desired.Archive.Path, configManifest, fmt.Sprintf("paired-restore:%d:compensation:%t", row.ID, current))
	if err != nil {
		return row, err
	}
	if _, err := s.executionOwner(ctx, row); err != nil {
		return row, err
	}
	if err := replaceRestoreExecutionHost(ctx, r, row, p, desired, manifest, manifests, c); err != nil {
		return row, err
	}
	phase := "restore_starting"
	terminal := "restore_completed"
	if current {
		phase = "restore_compensation_starting"
		terminal = "restore_compensated"
	}
	next, err := s.advanceRestoreExecution(ctx, row, phase, p)
	if err != nil {
		return row, err
	}
	row = next
	if _, err := s.executionOwner(ctx, row); err != nil {
		return row, err
	}
	if err := startRestoreExecutionHost(ctx, r, p.Fence, desired.Binding.Unit); err != nil {
		return row, err
	}
	target := *c
	target.Version = desired.Archive.Version
	runtime, err := verifyRestoreExecutionRuntime(ctx, r, &target, desired, manifest, p.Fence)
	if err != nil {
		return row, err
	}
	// Reopen immutable sources after verification; live data may now legitimately
	// evolve, but the runtime and source authority cannot silently change.
	if _, _, _, err := s.executionSets(ctx, r, row, p, c); err != nil {
		return row, err
	}
	if err := verifyRestoredConfigs(ctx, r, configs); err != nil {
		return row, err
	}
	p.VerifiedAt = s.now().UTC()
	p.Runtime = runtime
	next, err = s.advanceRestoreExecution(ctx, row, terminal, p, configs...)
	if err != nil {
		return row, err
	}
	return next, nil
}
func (s *BackupService) executionSets(ctx context.Context, r hostCommandRunner, row *models.PrometheusRestoreTransaction, p *restoreExecutionPayload, c *models.Component) (*prometheusRecoveryPayload, *prometheusRecoveryPayload, [2]*unitSnapshotManifest, error) {
	var manifests [2]*unitSnapshotManifest
	frozen := frozenExecutionStage(row, p)
	admission, err := decodeRestoreJournal(&frozen)
	if err != nil {
		return nil, nil, manifests, err
	}
	cp, err := decodeRestoreCheckpoint(&frozen, admission)
	if err != nil {
		return nil, nil, manifests, err
	}
	history, err := decodeRecoveryPointPayload(p.Historical)
	if err != nil || pointDigest(p.Historical) != admission.PointSHA256 {
		return nil, nil, manifests, restoreJournalReject("historical execution evidence changed")
	}
	stage, err := decodeRestoreStage(&frozen)
	if err != nil {
		return nil, nil, manifests, err
	}
	if p.Fence.BackupRoot != s.backupRoot {
		return nil, nil, manifests, restoreJournalReject("execution backup root changed")
	}
	if err := verifyRestoreStageFence(ctx, r, p.Fence); err != nil {
		return nil, nil, manifests, err
	}
	gen := filepath.Join(stage.Fence.Root, "g-"+stage.Fence.Generation)
	sets := [2]*prometheusRecoveryPayload{}
	for i, original := range []*prometheusRecoveryPayload{cp.Evidence, history} {
		root := filepath.Join(gen, []string{"current", "historical"}[i])
		sets[i] = derivedRestoreStageSet(root, original)
		if err := restoreStageEntries(ctx, r, root, filepath.Base(original.Startup.Path), filepath.Base(original.Binary.Directory)); err != nil {
			return nil, nil, manifests, err
		}
		if err := verifyRestoreStageSet(ctx, r, root, sets[i]); err != nil {
			return nil, nil, manifests, err
		}
		manifests[i], err = verifyUnitSnapshot(ctx, r, root, &sets[i].Startup, sets[i].Binding, false)
		if err != nil {
			return nil, nil, manifests, err
		}
	}
	if err := validateExecutionTargets(c, s.backupRoot, sets, manifests); err != nil {
		return nil, nil, manifests, err
	}
	if err := s.executionSharedReferences(ctx, r, c, manifests[0].Files); err != nil {
		return nil, nil, manifests, err
	}
	return sets[0], sets[1], manifests, nil
}
