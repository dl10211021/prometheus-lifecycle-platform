package services

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
	"path/filepath"
	"strconv"
	"strings"
)

var ErrPrometheusPairedRecoveryPointNotFound = errors.New("Prometheus paired recovery point not found")

func (s *BackupService) VerifyPrometheusRecoveryPoint(ctx context.Context, planID uint) (*PrometheusRecoveryPointVerification, error) {
	var proof *PrometheusRecoveryPointVerification
	err := s.withRecoveryPointClaim(ctx, planID, true, func(ctx context.Context, plan *models.ChangePlan, current *models.Component) error {
		point, p, err := s.loadRecoveryPointHistory(ctx, plan, current)
		if err != nil {
			return err
		}
		r, release, err := s.pointRunner(ctx, current)
		if err != nil {
			return err
		}
		defer release()
		manifest, err := verifyUnitSnapshot(ctx, r, s.backupRoot, &p.Startup, p.Binding, false)
		if err != nil {
			return err
		}
		if err := verifyRecoveryPointBinary(ctx, r, p.Binary); err != nil {
			return err
		}
		if err := checkRecoveryPointDirectory(ctx, r, p.Binary.Directory); err != nil {
			return err
		}
		path := utils.QuoteShell(p.Archive.Path)
		command := "set -eu\np=" + path + `
test "$(readlink -m -- "$p")" = "$p"
test -f "$p" && test ! -L "$p"
test "$(stat -c '%a:%u:%g:%h' -- "$p")" = '600:0:0:1'
`
		if _, err := snapshotRun(ctx, r, command); err != nil {
			return err
		}
		sha, size, err := inspectArtifact(ctx, r, p.Archive.Path, "full", "tar.gz", p.Archive.Sources)
		if err != nil {
			return err
		}
		if sha != p.Archive.SHA256 || size != p.Archive.Size {
			return pointReject("paired full archive failed integrity")
		}
		proof = &PrometheusRecoveryPointVerification{PointID: point.ID, PlanID: point.PlanID, ComponentID: point.ComponentID, HostID: point.HostID, BackupID: point.BackupID, HistoricalVersion: p.Archive.Version, CaptureState: point.State, StartupFiles: len(manifest.Files), BinaryBytes: p.Binary.File.Size, ArchiveBytes: size, Scope: "paired_historical_artifact_evidence_only"}
		return nil
	})
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryPoint, err)
	}
	return proof, nil
}

// Reconstruct historical binding from the trusted plan and pinned payload,
// never from current launch parameters or a caller-supplied checksum.
func (s *BackupService) loadRecoveryPointHistory(ctx context.Context, plan *models.ChangePlan, current *models.Component) (*models.PrometheusRecoveryPoint, *prometheusRecoveryPayload, error) {
	reject := func(reason string) (*models.PrometheusRecoveryPoint, *prometheusRecoveryPayload, error) {
		return nil, nil, pointReject(reason)
	}
	var point models.PrometheusRecoveryPoint
	if err := s.db.WithContext(ctx).First(&point, "plan_id = ?", plan.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, errors.Join(ErrPrometheusPairedRecoveryPointNotFound, err)
		}
		return nil, nil, err
	}
	associated := false
	if plan.PairedRecoveryPointID != nil {
		associated = *plan.PairedRecoveryPointID == point.ID
	} else {
		associated = plan.BackupID != nil && *plan.BackupID == point.BackupID
	}
	if point.ID == 0 || point.ComponentID != current.ID || point.HostID != current.HostID || point.BackupID == 0 || !associated || (point.State != "captured" && point.State != "ready") {
		return reject("point association or capture state changed")
	}
	p, err := decodeRecoveryPointPayload(point.Payload)
	if err != nil {
		return nil, nil, err
	}
	if p.BeforeSHA256 != pointDigest(plan.Before) || p.AfterSHA256 != pointDigest(plan.After) {
		return reject("historical plan was modified")
	}
	var before prometheusBefore
	if err := json.Unmarshal(plan.Before, &before); err != nil {
		return reject("invalid historical component")
	}
	historical := before.Component
	if historical == nil || historical.ID != point.ComponentID || historical.HostID != point.HostID || historical.Type != models.ComponentPrometheus || historical.DeployType != "binary" || !samePrometheusArtifactVersion(before.Version, historical.Version) || before.Image != historical.Image || !unitSnapshotHash.MatchString(before.ImportedArgsSHA256) {
		return reject("incomplete historical identity")
	}
	historical.Host = current.Host
	if makeUnitSnapshotBinding(historical, before.ImportedArgsSHA256) != p.Binding {
		return reject("historical runtime or owning host binding changed")
	}
	sources, err := backupSourcePaths(historical, "full")
	if err != nil {
		return nil, nil, err
	}
	if !validUnitSnapshotPath(s.backupRoot) || !validUnitSnapshotPath(p.Binary.Directory) || filepath.Dir(p.Binary.Directory) != s.backupRoot || !recoveryPointDirectory.MatchString(filepath.Base(p.Binary.Directory)) || !strings.HasPrefix(filepath.Base(p.Binary.Directory), "point-"+strconv.FormatUint(uint64(point.ComponentID), 10)+"-") {
		return reject("invalid owning-host artifact directory")
	}
	if p.Binary.File.Path != historical.BinaryPath || p.Startup.ComponentID != point.ComponentID || p.Startup.HostID != point.HostID || filepath.Dir(p.Startup.Path) != s.backupRoot || p.Archive.Path != filepath.Join(p.Binary.Directory, "payload.tar.gz") || !validUnitSnapshotPath(p.Archive.Path) || p.Archive.Size <= 0 || !unitSnapshotHash.MatchString(p.Archive.SHA256) || p.Archive.Version != historical.Version || !equalStringSlices(p.Archive.Sources, sources) {
		return reject("historical artifact paths or identity changed")
	}
	for _, source := range append(append([]string{}, sources...), historical.BinaryPath) {
		if !validUnitSnapshotPath(source) || recoveryPathsOverlap(s.backupRoot, source) {
			return reject("historical artifact root overlaps source")
		}
	}
	var backup models.Backup
	var manifest models.BackupManifest
	if err := s.db.WithContext(ctx).First(&backup, point.BackupID).Error; err != nil {
		return nil, nil, err
	}
	if err := s.db.WithContext(ctx).First(&manifest, "backup_id = ?", point.BackupID).Error; err != nil {
		return nil, nil, err
	}
	if backup.ComponentID == nil || *backup.ComponentID != point.ComponentID || backup.Type != "full" || backup.Path != p.Archive.Path || backup.Size != p.Archive.Size || manifest.BackupID != point.BackupID || manifest.ComponentType != models.ComponentPrometheus || manifest.ComponentVersion != p.Archive.Version || manifest.Scope != "full" || manifest.Format != "tar.gz" || manifest.Consistency != "stopped_application" || manifest.SHA256 != p.Archive.SHA256 || manifest.Size != p.Archive.Size || !equalStringSlices(manifest.SourcePaths, p.Archive.Sources) {
		return reject("full backup manifest was replaced or reassociated")
	}
	return &point, p, nil
}
