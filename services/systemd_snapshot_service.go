package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"time"
)

// UnitSnapshotVerification describes the checked artifact, never its contents.
// This proves startup-file integrity only, not runtime or data recoverability.
type UnitSnapshotVerification struct {
	ComponentID          uint   `json:"component_id"`
	HostID               uint   `json:"host_id"`
	Files                int    `json:"files"`
	PresentFiles         int    `json:"present_files"`
	Bytes                int64  `json:"bytes"`
	LiveBaselineVerified bool   `json:"live_baseline_verified"`
	Scope                string `json:"scope"`
}

// CaptureComponentUnitSnapshot is an internal service primitive. The caller
// must durably associate the returned reference with a recovery transaction
// before any later mutation; no change-plan or HTTP apply path invokes it yet.
func (s *BackupService) CaptureComponentUnitSnapshot(ctx context.Context, id uint) (*UnitSnapshotReference, error) {
	var ref *UnitSnapshotReference
	err := s.withUnitSnapshotComponent(ctx, id, func(ctx context.Context, r hostCommandRunner, b unitSnapshotBinding) error {
		var err error
		ref, err = captureUnitSnapshot(ctx, r, s.backupRoot, b)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ref, nil
}

// VerifyComponentUnitSnapshot reopens the pinned artifact and loads current DB
// identity under the same claims as capture. checkLive also requires the exact
// captured systemd process state and source file metadata.
func (s *BackupService) VerifyComponentUnitSnapshot(ctx context.Context, ref *UnitSnapshotReference, checkLive bool) (*UnitSnapshotVerification, error) {
	if ref == nil {
		return nil, snapshotReject("snapshot reference required")
	}
	var proof *UnitSnapshotVerification
	err := s.withUnitSnapshotComponent(ctx, ref.ComponentID, func(ctx context.Context, r hostCommandRunner, b unitSnapshotBinding) error {
		m, err := verifyUnitSnapshot(ctx, r, s.backupRoot, ref, b, checkLive)
		if err != nil {
			return err
		}
		proof = &UnitSnapshotVerification{ComponentID: b.ComponentID, HostID: b.HostID, Files: len(m.Files), LiveBaselineVerified: checkLive, Scope: "unit_fragment_dropins_environment_files"}
		for _, f := range m.Files {
			if f.Present {
				proof.PresentFiles++
				proof.Bytes += f.Size
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proof, nil
}

func (s *BackupService) withUnitSnapshotComponent(ctx context.Context, id uint, fn func(context.Context, hostCommandRunner, unitSnapshotBinding) error) error {
	if s == nil || s.db == nil || id == 0 {
		return snapshotReject("valid component and database required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var before models.Component
	if err := s.db.WithContext(ctx).First(&before, id).Error; err != nil {
		return snapshotReject("component unavailable")
	}
	if before.HostID == 0 {
		return snapshotReject("owning host required")
	}
	// Acquiring host after a component claim reverses the established lock order.
	if held, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext); held != nil {
		host, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext)
		if !ok || host.id != before.HostID {
			return snapshotReject("host claim must precede component claim")
		}
	}
	ctx, releaseHost, err := ClaimHostOperation(ctx, s.db, before.HostID, "systemd-snapshot")
	if err != nil {
		return err
	}
	defer releaseHost()
	ctx, releaseComponent, err := ClaimComponentOperation(ctx, s.db, id, "systemd-snapshot")
	if err != nil {
		return err
	}
	defer releaseComponent()
	var c models.Component
	if err := s.db.WithContext(ctx).Preload("Host").First(&c, id).Error; err != nil {
		return snapshotReject("component unavailable after claim")
	}
	if c.HostID != before.HostID || c.Host.ID != c.HostID || c.Host.IP == "" || c.Type != models.ComponentPrometheus || c.DeployType != "binary" {
		return snapshotReject("unsupported or changed component/host identity")
	}
	unit := deployer.CanonicalServiceName(c.ServiceName) + ".service"
	if c.ServiceName == "" || !unitSnapshotName.MatchString(unit) {
		return snapshotReject("valid recorded service name required")
	}
	binding := makeUnitSnapshotBinding(&c, importedArgsHash(c.ImportedArgs))
	r, err := s.runnerFactory(&c)
	if err != nil {
		return snapshotReject("host runner unavailable")
	}
	r, releaseRunner, err := acquireUnitSnapshotRunner(ctx, r)
	if err != nil {
		return err
	}
	defer releaseRunner()
	return fn(ctx, r, binding)
}

// argsHash can come from trusted historical plan evidence without retaining
// plaintext imported argv in the recovery-point record.
func makeUnitSnapshotBinding(c *models.Component, argsHash string) unitSnapshotBinding {
	unit := deployer.CanonicalServiceName(c.ServiceName) + ".service"
	runtimeBytes, _ := json.Marshal([]string{componentFingerprint(c), c.BinaryPath, c.ComposePath, argsHash})
	runtimeHash := sha256.Sum256(runtimeBytes)
	hostBytes, _ := json.Marshal(struct {
		ID                       uint
		IP                       string
		Port                     int
		Username, ConnectionType string
	}{c.Host.ID, c.Host.IP, c.Host.Port, c.Host.Username, c.Host.ConnectionType})
	hostHash := sha256.Sum256(hostBytes)
	return unitSnapshotBinding{ComponentID: c.ID, HostID: c.HostID, Unit: unit, RuntimeSHA256: hex.EncodeToString(runtimeHash[:]), HostSHA256: hex.EncodeToString(hostHash[:])}
}
