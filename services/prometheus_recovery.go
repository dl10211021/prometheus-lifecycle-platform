package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

var ErrPrometheusRecoveryAdmission = errors.New("Prometheus paired historical recovery rejected")

type prometheusBefore struct {
	Component          *models.Component `json:"component"`
	Version            string            `json:"version"`
	Image              string            `json:"image"`
	ImportedArgsSHA256 string            `json:"imported_args_sha256,omitempty"`
}

func importedArgsHash(args string) string {
	sum := sha256.Sum256([]byte(args))
	return hex.EncodeToString(sum[:])
}

func prometheusFullRecoveryScope(c *models.Component) error {
	// Relative/overlapping roots cannot identify two independent recoverable
	// live paths. Existing backup scope validation remains authoritative too.
	if (c.DeployType == "docker" || c.DeployType == "") && strings.TrimSpace(c.Image) == "" {
		return errors.New("unsupported-scope: Docker full recovery requires an exact historical image reference")
	}
	config, data := strings.TrimSpace(c.ConfigPath), strings.TrimSpace(c.DataPath)
	if config == "" || data == "" || !filepath.IsAbs(config) || !filepath.IsAbs(data) {
		return errors.New("managed Prometheus requires explicit absolute config and data paths for a stopped-application full recovery point")
	}
	for _, path := range []string{config, data} {
		if err := validateSourcePath(path); err != nil {
			return err
		}
	}
	if config != c.ConfigPath || data != c.DataPath {
		return errors.New("unsupported-scope: declared paths contain surrounding whitespace")
	}
	config, data = filepath.Clean(config), filepath.Clean(data)
	if config == data || strings.HasPrefix(config, data+string(filepath.Separator)) || strings.HasPrefix(data, config+string(filepath.Separator)) {
		return errors.New("full recovery requires separate non-overlapping config and data roots")
	}
	runtimeConfig, runtimeData, err := deployer.PrometheusRuntimePaths(c)
	if err != nil {
		return err
	}
	if runtimeConfig != config || runtimeData != data {
		return errors.New("unsupported-scope: effective runtime config/data paths differ from declared full backup roots")
	}
	if _, err := backupSourcePaths(c, "full"); err != nil {
		return err
	}
	return nil
}

func prometheusUpgradeSnapshot(plan *models.ChangePlan, current *models.Component) (*prometheusBefore, error) {
	reject := func(reason string) (*prometheusBefore, error) {
		return nil, fmt.Errorf("%w: %s", ErrPrometheusRecoveryAdmission, reason)
	}
	var before prometheusBefore
	if err := json.Unmarshal(plan.Before, &before); err != nil {
		return reject("malformed historical before snapshot")
	}
	previous := before.Component
	if previous == nil || previous.ID == 0 || previous.ID != current.ID || plan.ResourceID != current.ID || plan.ResourceType != "component" || previous.Type != models.ComponentPrometheus {
		return reject("missing or mismatched historical component identity; use a plan with a complete historical snapshot")
	}
	if !samePrometheusArtifactVersion(before.Version, previous.Version) || before.Image != previous.Image {
		return reject("historical scalar version/image does not match component snapshot")
	}
	if (previous.DeployType == "docker" || previous.DeployType == "") && strings.TrimSpace(before.Image) == "" {
		return reject("historical Docker snapshot lacks an exact image reference")
	}
	comparable := *current
	comparable.Version = previous.Version
	comparable.Image = previous.Image
	if componentFingerprint(&comparable) != componentFingerprint(previous) || current.BinaryPath != previous.BinaryPath || current.ComposePath != previous.ComposePath {
		return reject("historical runtime identity has drifted (host, paths, launch configuration or resources)")
	}
	if before.ImportedArgsSHA256 == "" {
		if current.ImportedArgs != "" {
			return reject("historical snapshot lacks imported runtime argv evidence; do not guess launch arguments")
		}
	} else if before.ImportedArgsSHA256 != importedArgsHash(current.ImportedArgs) {
		return reject("imported runtime argv evidence does not match historical snapshot")
	}
	// ImportedArgs is excluded from Component JSON. The hash above binds the
	// retained live argv to this historical snapshot; reconstruct it only in a
	// validation copy, rather than accidentally checking platform defaults.
	parameterCandidate := *previous
	parameterCandidate.ImportedArgs = current.ImportedArgs
	if err := deployer.ValidatePrometheusParameterVersion(&parameterCandidate, before.Version); err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	return &before, nil
}

// Metadata binding precedes any runner command and VerifiedAt update. The live
// archive is still inspected after this check; old VerifiedAt is not admission.
func (s *BackupService) prometheusBackupBinding(backup *models.Backup, c *models.Component, target string, full bool) (*models.BackupManifest, error) {
	reject := func(reason string) (*models.BackupManifest, error) {
		return nil, fmt.Errorf("%w: %s", ErrPrometheusRecoveryAdmission, reason)
	}
	if backup == nil || backup.ID == 0 || backup.ComponentID == nil || *backup.ComponentID != c.ID {
		return reject("missing or unrelated backup")
	}
	var stored models.Backup
	if err := s.db.First(&stored, backup.ID).Error; err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	if stored.ComponentID == nil || *stored.ComponentID != c.ID || stored.Type != backup.Type || stored.Path != backup.Path || stored.Size != backup.Size {
		return reject("backup record identity does not match supplied artifact")
	}
	var manifest models.BackupManifest
	if err := s.db.First(&manifest, "backup_id = ?", backup.ID).Error; err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	if manifest.BackupID != backup.ID || manifest.ComponentType != models.ComponentPrometheus || manifest.Scope != backup.Type || !samePrometheusArtifactVersion(manifest.ComponentVersion, target) {
		return reject("backup manifest must match component, scope and known historical version")
	}
	if full {
		if err := prometheusFullRecoveryScope(c); err != nil {
			return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
		}
		if manifest.Scope != "full" || manifest.Format != "tar.gz" || manifest.Consistency != "stopped_application" {
			return reject("stateful Prometheus recovery requires a stopped-application full config/data backup")
		}
	}
	if err := validateScopeFormat(manifest.Scope, manifest.Format); err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	sources, err := backupSourcePaths(c, manifest.Scope)
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	if !equalStringSlices(sources, manifest.SourcePaths) {
		return reject("backup source paths do not match historical runtime paths")
	}
	if backup.Path == "" || manifest.Size <= 0 || backup.Size != manifest.Size || len(manifest.SHA256) != 64 {
		return reject("backup archive integrity metadata is incomplete")
	}
	return &manifest, nil
}

func (s *ChangeService) admitPrometheusRecovery(ctx context.Context, plan *models.ChangePlan, backup *models.Backup, c *models.Component) error {
	before, err := prometheusUpgradeSnapshot(plan, c)
	if err != nil {
		return err
	}
	if plan.BackupID == nil || backup == nil || *plan.BackupID != backup.ID {
		return fmt.Errorf("%w: plan has no associated full recovery point", ErrPrometheusRecoveryAdmission)
	}
	var storedPlan models.ChangePlan
	if plan.ID == 0 {
		return fmt.Errorf("%w: missing historical plan identity", ErrPrometheusRecoveryAdmission)
	}
	if err := s.db.First(&storedPlan, plan.ID).Error; err != nil {
		return errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	if storedPlan.ResourceID != c.ID || storedPlan.ResourceType != "component" || storedPlan.Kind != models.ChangeKindUpgrade || storedPlan.BackupID == nil || *storedPlan.BackupID != backup.ID {
		return fmt.Errorf("%w: stored plan does not bind this component and backup", ErrPrometheusRecoveryAdmission)
	}
	if _, err = s.backups.prometheusBackupBinding(backup, c, before.Version, true); err != nil {
		return err
	}
	if err = s.backups.verifyPrometheusResolvedRoots(ctx, c); err != nil {
		return errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	if _, err = s.backups.VerifyComponentBackup(ctx, backup, c); err != nil {
		return errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	return nil
}

// At this boundary historical data has already been applied. The callback
// cannot prove that any error left a matching runtime, including early errors.
// Persist historical metadata while the restore checkpoint still quarantines
// crashes; only its successful return permits BackupService to clear the pause.
func (s *ChangeService) prometheusRuntimeRecovery(c *models.Component, restore func(context.Context) error) func(context.Context) error {
	historicalVersion, historicalImage := c.Version, c.Image
	return func(ctx context.Context) error {
		if err := restore(ctx); err != nil {
			return errors.Join(deployer.ErrBinaryRecoveryUnconfirmed, err)
		}
		if !samePrometheusArtifactVersion(c.Version, historicalVersion) || c.Image != historicalImage {
			return errors.Join(deployer.ErrBinaryRecoveryUnconfirmed, errors.New("historical runtime version/image changed during recovery; pairing is unconfirmed"))
		}
		result := s.db.Model(&models.Component{}).Where("id = ?", c.ID).Updates(map[string]interface{}{"version": historicalVersion, "image": historicalImage, "status": models.StatusRunning, "error": ""})
		err := result.Error
		if err == nil && result.RowsAffected != 1 {
			err = gorm.ErrRecordNotFound
		}
		if err != nil {
			return errors.Join(deployer.ErrBinaryRecoveryUnconfirmed, fmt.Errorf("persist historical runtime metadata: %w", err))
		}
		return nil
	}
}

// A handler can load a component before another operation finishes. Re-read
// under the acquired restore claim; a stale supplied identity cannot authorize
// replaying an archive against the newly installed runtime.
func (s *BackupService) currentPrometheusRestoreIdentity(ctx context.Context, supplied *models.Component) (*models.Component, error) {
	var current models.Component
	if err := s.db.WithContext(ctx).Preload("Host").Preload("Cluster").First(&current, supplied.ID).Error; err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	comparable := *supplied
	comparable.Version = current.Version
	if !managedPrometheus(&current) || !samePrometheusArtifactVersion(supplied.Version, current.Version) || componentFingerprint(&comparable) != componentFingerprint(&current) || supplied.BinaryPath != current.BinaryPath || supplied.ComposePath != current.ComposePath || supplied.ImportedArgs != current.ImportedArgs {
		return nil, fmt.Errorf("%w: component changed before restore acquired its claim; reload current runtime identity", ErrPrometheusRecoveryAdmission)
	}
	return &current, nil
}

// Resolve the declared binary roots on the owning host before any stop/write.
// In particular a /srv alias into PrivateTmp must not be treated as the same
// host-visible data tree as systemd's private namespace. Requiring canonical
// declared roots also avoids silently changing archive meaning through links.
// readlink -m permits an absent restore target while resolving existing parents.
func (s *BackupService) verifyPrometheusResolvedRoots(ctx context.Context, c *models.Component) error {
	if !managedPrometheus(c) || c.DeployType != "binary" {
		return nil
	}
	copy := *c
	if copy.Host.ID == 0 && copy.HostID != 0 {
		if err := s.db.WithContext(ctx).First(&copy.Host, copy.HostID).Error; err != nil {
			return err
		}
	}
	runner, err := s.runnerFactory(&copy)
	if err != nil {
		return err
	}
	for _, path := range []string{c.ConfigPath, c.DataPath} {
		resolved, err := runner.Run(ctx, "readlink -m -- "+utils.QuoteShell(path))
		if err != nil {
			return fmt.Errorf("unsupported-scope: cannot resolve declared runtime root: %w", err)
		}
		resolved = strings.TrimSuffix(strings.TrimSuffix(resolved, "\n"), "\r")
		if resolved != filepath.Clean(path) {
			return errors.New("unsupported-scope: binary full recovery requires canonical host roots without symlink aliases")
		}
		// Full backup uses cp -aL. A nested link into PrivateTmp would copy a
		// different host-visible WAL/config tree; do not dereference such links.
		links, err := runner.Run(ctx, "if [ -e "+utils.QuoteShell(path)+" ]; then find -P "+utils.QuoteShell(path)+" -type l -print -quit; fi")
		if err != nil {
			return fmt.Errorf("unsupported-scope: cannot inspect declared runtime tree: %w", err)
		}
		if links != "" {
			return errors.New("unsupported-scope: binary full recovery cannot dereference symlinks inside config/data trees")
		}
	}
	return nil
}
