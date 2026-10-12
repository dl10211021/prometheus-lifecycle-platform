package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
)

type PrometheusPairedRestoreReplacement struct {
	Path              string `json:"path"`
	Role              string `json:"role"`
	Action            string `json:"action"`
	CurrentPresent    bool   `json:"current_present"`
	HistoricalPresent bool   `json:"historical_present"`
	ContentCompared   bool   `json:"content_compared"`
	ContentChanged    bool   `json:"content_changed"`
	MetadataChanged   bool   `json:"metadata_changed"`
}
type PrometheusPairedRestorePreview struct {
	PointID                   uint                                 `json:"point_id"`
	PlanID                    uint                                 `json:"plan_id"`
	BackupID                  uint                                 `json:"backup_id"`
	ComponentID               uint                                 `json:"component_id"`
	HostID                    uint                                 `json:"host_id"`
	CurrentVersion            string                               `json:"current_version"`
	HistoricalVersion         string                               `json:"historical_version"`
	Scope                     string                               `json:"scope"`
	RestoreAuthorized         bool                                 `json:"restore_authorized"`
	RequiresDowntime          bool                                 `json:"requires_downtime"`
	RequiresCurrentCheckpoint bool                                 `json:"requires_current_checkpoint"`
	Replacements              []PrometheusPairedRestoreReplacement `json:"replacements"`
	Dependencies              []DependentCompatibility             `json:"dependencies"`
	Checks                    []string                             `json:"checks"`
	ObservedAt                time.Time                            `json:"observed_at"`
}

// Preview is a current observation, never a token for a later restore. The
// future mutation transaction must repeat admission and preserve a complete
// current-state checkpoint before changing any historical/live file.
func (s *BackupService) PreviewPrometheusRecoveryPointRestore(ctx context.Context, planID uint) (*PrometheusPairedRestorePreview, error) {
	var proof *PrometheusPairedRestorePreview
	err := s.withRecoveryPointClaim(ctx, planID, false, func(ctx context.Context, plan *models.ChangePlan, c *models.Component) error {
		point, p, historical, dependencies, err := s.admitPairedRestorePreview(ctx, plan, c)
		if err != nil {
			return err
		}
		registered, err := s.pairedRestoreRegisteredReferences(ctx, c)
		if err != nil {
			return err
		}
		r, release, err := s.pointRunner(ctx, c)
		if err != nil {
			return err
		}
		defer release()
		scoped := s.onPointRunner(r)
		scoped.db = s.db.WithContext(ctx)
		if err := scoped.verifyPrometheusResolvedRoots(ctx, c); err != nil {
			return err
		}
		saved, err := verifyUnitSnapshot(ctx, r, s.backupRoot, &p.Startup, p.Binding, false)
		if err != nil {
			return err
		}
		state, files, err := snapshotInventory(ctx, r, p.Binding.Unit)
		if err != nil {
			return err
		}
		protected, err := pairedRestoreProtectedPaths(c, s.backupRoot, saved, state, files)
		if err != nil {
			return err
		}
		binary, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: c.BinaryPath, Role: "binary"}, recoveryPointMaxBinary)
		if err != nil {
			return err
		}
		runtime, err := readRecoveryPointRuntime(ctx, r, c, state, binary)
		if err != nil {
			return err
		}
		if runtime.ArgvSHA256 != p.Runtime.ArgvSHA256 {
			return pointReject("current actual argv differs from the paired history")
		}
		resolvedRegistered, err := resolvePairedRestoreUnitReferences(ctx, r, registered)
		if err != nil {
			return err
		}
		if err := rejectPairedRestoreSharedPaths(protected, resolvedRegistered, ""); err != nil {
			return err
		}
		refs, err := readPairedRestoreUnitReferences(ctx, r)
		if err != nil {
			return err
		}
		if err := rejectPairedRestoreSharedIdentity(p.Binding.Unit, refs, resolvedRegistered); err != nil {
			return err
		}
		if err := checkPairedRestoreOwnReferences(c, files, refs); err != nil {
			return err
		}
		if err := rejectPairedRestoreSharedPaths(protected, refs, p.Binding.Unit); err != nil {
			return err
		}
		// This reopens all three pinned artifacts, including the stopped archive.
		if _, err := s.VerifyPrometheusRecoveryPoint(ctx, plan.ID); err != nil {
			// Legacy artifact parsers include malformed stdout in errors.
			// Preserve operational identities without exposing that text.
			safe := []error{pointReject("historical artifact verification failed")}
			for _, kind := range []error{context.Canceled, context.DeadlineExceeded, coressh.ErrRemoteTerminationUnconfirmed} {
				if errors.Is(err, kind) {
					safe = append(safe, kind)
				}
			}
			return errors.Join(safe...)
		}

		finalRefs, err := readPairedRestoreUnitReferences(ctx, r)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(refs, finalRefs) {
			return pointReject("direct service references changed during restore preview")
		}
		finalRegistered, err := s.pairedRestoreRegisteredReferences(ctx, c)
		if err != nil {
			return err
		}
		finalRegistered, err = resolvePairedRestoreUnitReferences(ctx, r, finalRegistered)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(resolvedRegistered, finalRegistered) {
			return pointReject("registered runtime references changed during restore preview")
		}
		if err := scoped.verifyPrometheusResolvedRoots(ctx, c); err != nil {
			return err
		}
		finalState, finalFiles, err := snapshotInventory(ctx, r, p.Binding.Unit)
		if err != nil {
			return err
		}
		finalBinary, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: c.BinaryPath, Role: "binary"}, recoveryPointMaxBinary)
		if err != nil {
			return err
		}
		finalRuntime, err := readRecoveryPointRuntime(ctx, r, c, finalState, finalBinary)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(state, finalState) || !reflect.DeepEqual(files, finalFiles) || !reflect.DeepEqual(binary, finalBinary) || runtime != finalRuntime {
			return pointReject("current runtime or startup files changed during restore preview")
		}

		var finalPlan models.ChangePlan
		var finalComponent models.Component
		if err := s.db.WithContext(ctx).First(&finalPlan, plan.ID).Error; err != nil {
			return err
		}
		if err := s.db.WithContext(ctx).Preload("Host").First(&finalComponent, c.ID).Error; err != nil {
			return err
		}
		finalPoint, _, _, finalDependencies, err := s.admitPairedRestorePreview(ctx, &finalPlan, &finalComponent)
		if err != nil {
			return err
		}
		if finalPoint.ID != point.ID || !bytes.Equal(point.Payload, finalPoint.Payload) || finalPlan.Status != plan.Status || !reflect.DeepEqual(dependencies, finalDependencies) || makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)) != makeUnitSnapshotBinding(&finalComponent, importedArgsHash(finalComponent.ImportedArgs)) {
			return pointReject("trusted identity changed during restore preview")
		}
		replacements := []PrometheusPairedRestoreReplacement{pairedRestoreReplacement(binary, p.Binary.File)}
		for i, file := range files {
			replacements = append(replacements, pairedRestoreReplacement(file, saved.Files[i]))
		}
		for i, path := range p.Archive.Sources {
			role := "config"
			if i == 1 {
				role = "data"
			}
			replacements = append(replacements, PrometheusPairedRestoreReplacement{Path: path, Role: role, Action: "replace_from_stopped_archive", CurrentPresent: true, HistoricalPresent: true})
		}
		proof = &PrometheusPairedRestorePreview{PointID: point.ID, PlanID: plan.ID, BackupID: point.BackupID, ComponentID: c.ID, HostID: c.HostID, CurrentVersion: c.Version, HistoricalVersion: historical.Version, Scope: "paired_restore_admission_preview_only", RestoreAuthorized: false, RequiresDowntime: true, RequiresCurrentCheckpoint: true, Replacements: replacements, Dependencies: dependencies, Checks: []string{"trusted_paired_history", "current_plan_identity", "retained_same_host_dependencies", "actual_runtime_and_loaded_launch", "unchanged_startup_topology_and_enablement", "registered_component_paths", "direct_systemd_file_references", "fresh_three_artifact_integrity", "stable_runtime_and_reference_inventory"}, ObservedAt: s.now()}
		return nil
	})
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	return proof, nil
}

func (s *BackupService) admitPairedRestorePreview(ctx context.Context, plan *models.ChangePlan, c *models.Component) (*models.PrometheusRecoveryPoint, *prometheusRecoveryPayload, *models.Component, []DependentCompatibility, error) {
	reject := func(reason string) (*models.PrometheusRecoveryPoint, *prometheusRecoveryPayload, *models.Component, []DependentCompatibility, error) {
		return nil, nil, nil, nil, pointReject(reason)
	}
	if plan.Kind != models.ChangeKindUpgrade || plan.ResourceType != "component" || plan.ResourceID != c.ID {
		return reject("component upgrade plan identity changed")
	}
	if plan.Status != models.ChangeStatusFailed && plan.Status != models.ChangeStatusVerified {
		return reject("failed or verified historical upgrade plan required")
	}
	var paused int64
	if err := s.db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ? AND (status <> ? OR version_transition_plan_id <> 0)", c.ID, "resumed").Count(&paused).Error; err != nil {
		return nil, nil, nil, nil, err
	}
	if paused != 0 {
		return reject("unresolved recovery cannot authorize a new restore preview")
	}
	point, p, err := s.loadRecoveryPointHistory(ctx, plan, c)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if point.State != "ready" {
		return reject("a fully completed paired capture is required")
	}
	before, err := prometheusUpgradeSnapshot(plan, c)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var after upgradeChange
	if err := json.Unmarshal(plan.After, &after); err != nil {
		return reject("invalid historical upgrade target")
	}
	if _, ok := stablePrometheusVersion(c.Version); !ok {
		return reject("known current Prometheus version required")
	}
	if _, ok := stablePrometheusVersion(after.NewVersion); !ok {
		return reject("known planned Prometheus version required")
	}
	if !samePrometheusArtifactVersion(c.Version, after.NewVersion) && (plan.Status != models.ChangeStatusFailed || !samePrometheusArtifactVersion(c.Version, before.Version)) {
		return reject("current version does not belong to this historical plan")
	}
	historical := *before.Component
	historical.Host = c.Host
	historical.ImportedArgs = c.ImportedArgs
	if err := prometheusFullRecoveryScope(&historical); err != nil {
		return nil, nil, nil, nil, err
	}
	dependencies, err := CheckComponentTargetVersion(s.db.WithContext(ctx), c, historical.Version)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return point, p, &historical, dependencies, nil
}

func pairedRestoreProtectedPaths(c *models.Component, root string, saved *unitSnapshotManifest, state unitSnapshotState, files []unitSnapshotFile) ([]string, error) {
	if len(saved.Files) != len(files) {
		return nil, pointReject("startup file topology differs from historical recovery point")
	}
	for _, key := range []string{"Id", "FragmentPath", "DropInPaths", "EnvironmentFiles", "UnitFileState"} {
		if saved.State[key] != state[key] {
			return nil, pointReject("startup topology or enablement requires a different restore protocol")
		}
	}
	paths := []string{c.BinaryPath, c.ConfigPath, c.DataPath, root}
	for i, file := range files {
		old := saved.Files[i]
		if file.Path != old.Path || file.Role != old.Role || file.Optional != old.Optional {
			return nil, pointReject("startup path roles changed since capture")
		}
		paths = append(paths, file.Path)
	}
	for i, path := range paths {
		if !validUnitSnapshotPath(path) {
			return nil, pointReject("unsupported live or artifact path")
		}
		for _, previous := range paths[:i] {
			if recoveryPathsOverlap(path, previous) {
				return nil, pointReject("startup, executable, archive and application paths overlap")
			}
		}
	}
	return paths, nil
}
func (s *BackupService) pairedRestoreRegisteredReferences(ctx context.Context, c *models.Component) ([]pairedRestoreUnitReference, error) {
	var others []models.Component
	if err := s.db.WithContext(ctx).Where("host_id = ? AND id <> ? AND (status IS NULL OR status <> ?)", c.HostID, c.ID, "removed").Order("id ASC").Find(&others).Error; err != nil {
		return nil, err
	}
	refs := make([]pairedRestoreUnitReference, 0, len(others))
	for _, other := range others {
		if other.ServiceName != "" && deployer.CanonicalServiceName(other.ServiceName) == deployer.CanonicalServiceName(c.ServiceName) {
			return nil, pointReject("service identity is shared with another retained component")
		}
		paths := []string{}
		for _, path := range []string{other.BinaryPath, other.ComposePath, other.ConfigPath, other.DataPath} {
			if path == "" {
				continue
			}
			if !pairedRestoreLiteralReference(path) {
				return nil, pointReject("another retained component has unresolved runtime paths")
			}
			paths = append(paths, path)
		}
		var names []string
		if other.ServiceName != "" {
			name := deployer.CanonicalServiceName(other.ServiceName) + ".service"
			if len(name) > 255 || !pairedRestoreUnitName.MatchString(name) || strings.HasSuffix(name, "@.service") {
				return nil, pointReject("another retained component has unresolved service identity")
			}
			names = []string{name}
		}
		refs = append(refs, pairedRestoreUnitReference{ID: fmt.Sprintf("component:%d", other.ID), Names: names, Paths: uniquePairedRestorePaths(paths)})
	}
	return refs, nil
}
func rejectPairedRestoreSharedIdentity(ownUnit string, units, registered []pairedRestoreUnitReference) error {
	names := map[string]bool{}
	for _, unit := range units {
		if unit.ID == ownUnit {
			for _, name := range unit.Names {
				names[name] = true
			}
		}
	}
	for _, other := range registered {
		for _, name := range other.Names {
			if names[name] {
				return pointReject("service alias identity is shared with another retained component")
			}
		}
	}
	return nil
}
func rejectPairedRestoreSharedPaths(protected []string, refs []pairedRestoreUnitReference, ownUnit string) error {
	for _, ref := range refs {
		if ownUnit != "" && ref.ID == ownUnit {
			continue
		}
		for _, path := range ref.Paths {
			for _, target := range protected {
				if recoveryPathsOverlap(path, target) {
					return pointReject("restore paths are shared with another retained component or direct service reference")
				}
			}
		}
	}
	return nil
}
func checkPairedRestoreOwnReferences(c *models.Component, files []unitSnapshotFile, refs []pairedRestoreUnitReference) error {
	unit := deployer.CanonicalServiceName(c.ServiceName) + ".service"
	wanted := []string{c.BinaryPath}
	for _, file := range files {
		wanted = append(wanted, file.Path)
	}
	wanted = uniquePairedRestorePaths(wanted)
	for _, ref := range refs {
		if ref.ID != unit {
			continue
		}
		if !reflect.DeepEqual(ref.Paths, wanted) {
			return pointReject("owning service reference inventory differs from observed startup")
		}
		return nil
	}
	return pointReject("owning service absent from direct reference inventory")
}
func pairedRestoreReplacement(current, historical unitSnapshotFile) PrometheusPairedRestoreReplacement {
	action := "replace"
	if !historical.Present {
		action = "remove"
		if !current.Present {
			action = "retain_absence"
		}
	}
	return PrometheusPairedRestoreReplacement{Path: historical.Path, Role: historical.Role, Action: action, CurrentPresent: current.Present, HistoricalPresent: historical.Present, ContentCompared: true, ContentChanged: current.Present != historical.Present || current.SHA256 != historical.SHA256, MetadataChanged: current.Mode != historical.Mode || current.UID != historical.UID || current.GID != historical.GID}
}
