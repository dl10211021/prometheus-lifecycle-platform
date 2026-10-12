package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ErrPrometheusRecoveryPoint = errors.New("Prometheus paired recovery point rejected")

const recoveryPointMaxBinary int64 = 512 << 20
const recoveryPointMaxPayload = 256 << 10

var recoveryPointDirectory = regexp.MustCompile(`^point-[1-9][0-9]*-[0-9a-f]{32}$`)

type recoveryPointBinary struct {
	Directory string
	File      unitSnapshotFile
}
type recoveryPointArchive struct {
	Path, SHA256, Version string
	Size                  int64
	Sources               []string
}
type recoveryPointRuntime struct{ PID, StartTime, ArgvSHA256, ConfigIdentity, DataIdentity string }
type prometheusRecoveryPayload struct {
	Schema                    int
	BeforeSHA256, AfterSHA256 string
	Binding                   unitSnapshotBinding
	Startup                   UnitSnapshotReference
	Binary                    recoveryPointBinary
	Archive                   recoveryPointArchive
	Runtime                   recoveryPointRuntime
}
type PrometheusRecoveryPointVerification struct {
	PointID           uint   `json:"point_id"`
	PlanID            uint   `json:"plan_id"`
	ComponentID       uint   `json:"component_id"`
	HostID            uint   `json:"host_id"`
	BackupID          uint   `json:"backup_id"`
	HistoricalVersion string `json:"historical_version"`
	CaptureState      string `json:"capture_state"`
	StartupFiles      int    `json:"startup_files"`
	BinaryBytes       int64  `json:"binary_bytes"`
	ArchiveBytes      int64  `json:"archive_bytes"`
	Scope             string `json:"scope"`
}

func pointReject(reason string) error {
	return fmt.Errorf("%w: %s", ErrPrometheusRecoveryPoint, reason)
}
func pointDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func recoveryPathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func (s *BackupService) withRecoveryPointClaim(ctx context.Context, planID uint, readOnly bool, fn func(context.Context, *models.ChangePlan, *models.Component) error) error {
	if s == nil || s.db == nil || planID == 0 {
		return pointReject("valid database and plan required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var first models.ChangePlan
	if err := s.db.WithContext(ctx).First(&first, planID).Error; err != nil {
		return errors.Join(ErrPrometheusRecoveryPoint, err)
	}
	if first.Kind != models.ChangeKindUpgrade || first.ResourceType != "component" || first.ResourceID == 0 {
		return pointReject("component upgrade plan required")
	}
	var initial models.Component
	if err := s.db.WithContext(ctx).First(&initial, first.ResourceID).Error; err != nil {
		return errors.Join(ErrPrometheusRecoveryPoint, err)
	}
	if initial.HostID == 0 {
		return pointReject("owning host required")
	}
	if held, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext); held != nil {
		host, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext)
		if !ok || host.id != initial.HostID {
			return pointReject("host claim must precede component claim")
		}
	}
	ctx, releaseHost, err := ClaimHostOperation(ctx, s.db, initial.HostID, "prometheus-recovery-point")
	if err != nil {
		return err
	}
	defer releaseHost()
	kind := "prometheus-recovery-point"
	if readOnly {
		kind = "restore-recovery"
	}
	ctx, releaseComponent, err := ClaimComponentOperation(ctx, s.db, initial.ID, kind)
	if err != nil {
		return err
	}
	defer releaseComponent()
	var plan models.ChangePlan
	var c models.Component
	if err := s.db.WithContext(ctx).First(&plan, planID).Error; err != nil {
		return err
	}
	if plan.ResourceID != initial.ID || plan.Kind != models.ChangeKindUpgrade || plan.ResourceType != "component" {
		return pointReject("plan identity changed before claim")
	}
	if err := s.db.WithContext(ctx).Preload("Host").First(&c, initial.ID).Error; err != nil {
		return err
	}
	if c.HostID != initial.HostID || c.Host.ID != c.HostID || c.Host.IP == "" || c.Type != models.ComponentPrometheus || c.DeployType != "binary" {
		return pointReject("unsupported or changed owning component")
	}
	// Reentrant claim contexts may skip generic claim admission, so enforce
	// this operation's transition exclusion again under the held claims.
	var transitions int64
	if err := s.db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ? AND version_transition_plan_id <> 0", c.ID).Count(&transitions).Error; err != nil {
		return err
	}
	if transitions != 0 {
		return pointReject("cannot inspect or capture across an active version transition")
	}
	return fn(ctx, &plan, &c)
}

type recoveryPointSafeRunner struct{ inner hostCommandRunner }

func (r recoveryPointSafeRunner) Run(ctx context.Context, command string) (string, error) {
	return snapshotRun(ctx, r.inner, command)
}
func (s *BackupService) pointRunner(ctx context.Context, c *models.Component) (hostCommandRunner, func(), error) {
	r, err := s.runnerFactory(c)
	if err != nil {
		return nil, nil, pointReject("host runner unavailable")
	}
	r, release, err := acquireUnitSnapshotRunner(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	return recoveryPointSafeRunner{r}, release, nil
}
func (s *BackupService) onPointRunner(r hostCommandRunner) *BackupService {
	copy := *s
	copy.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return r, nil }
	return &copy
}
func createRecoveryPointDirectory(ctx context.Context, r hostCommandRunner, root string, id uint) (string, error) {
	if !validUnitSnapshotPath(root) || id == 0 {
		return "", pointReject("invalid artifact root")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(root, fmt.Sprintf("point-%d-%x", id, random[:]))
	command := "set -eu\numask 077\nroot=" + utils.QuoteShell(root) + "\ndir=" + utils.QuoteShell(dir) + `
test "$(id -u)" = 0
test "$(readlink -m -- "$root")" = "$root"
mkdir -p -- "$root"
test -d "$root" && test ! -L "$root"
test "$(stat -c %u -- "$root")" = 0
test -z "$(find "$root" -maxdepth 0 -perm /022 -print)"
mkdir -m 0700 -- "$dir"
chown 0:0 -- "$dir"
sync -f "$root"
`
	if _, err := snapshotRun(ctx, r, command); err != nil {
		return "", err
	}
	return dir, snapshotPrivateDirectory(ctx, r, dir)
}
func decodeRecoveryPointPayload(raw json.RawMessage) (*prometheusRecoveryPayload, error) {
	if len(raw) == 0 || len(raw) > recoveryPointMaxPayload {
		return nil, pointReject("invalid recovery payload size")
	}
	var p prometheusRecoveryPayload
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, pointReject("invalid recovery payload")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, pointReject("trailing recovery payload")
	}
	if p.Schema != 1 || !unitSnapshotHash.MatchString(p.BeforeSHA256) || !unitSnapshotHash.MatchString(p.AfterSHA256) || !validUnitSnapshotBinding(p.Binding) || !unitSnapshotHash.MatchString(p.Runtime.ArgvSHA256) {
		return nil, pointReject("incomplete recovery payload")
	}
	return &p, nil
}
func pointRecordError(result *gorm.DB, label string) error {
	if result.Error != nil {
		return fmt.Errorf("%w: save %s: %w", ErrPrometheusRecoveryPoint, label, result.Error)
	}
	if result.RowsAffected != 1 {
		return pointReject("expected one durable " + label + " record")
	}
	return nil
}

// Paired artifacts stay pinned until a dedicated retirement/restore transaction
// exists. Older standalone-backup databases may not yet have the new table.
func (s *BackupService) rejectPairedBackupMutation(ctx context.Context, backup *models.Backup) error {
	if backup == nil {
		return nil
	}
	if recoveryPointDirectory.MatchString(filepath.Base(filepath.Dir(backup.Path))) {
		return pointReject("paired backup requires its dedicated recovery or retirement protocol")
	}
	if backup.ID == 0 || s.db == nil || !s.db.WithContext(ctx).Migrator().HasTable(&models.PrometheusRecoveryPoint{}) {
		return nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.PrometheusRecoveryPoint{}).Where("backup_id = ?", backup.ID).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return pointReject("backup is pinned by an immutable paired recovery point")
	}
	return nil
}
