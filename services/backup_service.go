package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

const defaultBackupRoot = "/backups"

var backupNameCleaner = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type hostCommandRunner interface {
	Run(context.Context, string) (string, error)
}

type localBackupRunner struct{}

func (localBackupRunner) Run(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	// The watchdog survives a platform crash; cancellation also kills the
	// local process group before compensation is allowed to start.
	cmd := exec.CommandContext(ctx, "timeout", "--signal=TERM", "--kill-after=5s", fmt.Sprintf("%.3fs", max(time.Until(deadline).Seconds(), 0.001)), "sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type remoteBackupRunner struct{ component *models.Component }

func (r remoteBackupRunner) Run(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	host := r.component.Host
	client, err := coressh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("连接备份主机失败: %w", err)
	}
	defer client.Close()
	stdout, stderr, err := client.RunCommandContext(ctx, command)
	if err != nil {
		return stdout, fmt.Errorf("远程命令执行失败: %w: %s", err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// BackupService is the single implementation used by manual and scheduled
// component backups. It owns artifact integrity, restore checkpoints, atomic
// replacement, readiness checks, and non-disruptive rehearsals.
type BackupService struct {
	db            *gorm.DB
	backupRoot    string
	runnerFactory func(*models.Component) (hostCommandRunner, error)
	now           func() time.Time
}

func NewBackupService(db *gorm.DB) *BackupService {
	root := filepath.Clean(strings.TrimSpace(os.Getenv("BACKUP_ROOT")))
	if !filepath.IsAbs(root) || root == string(filepath.Separator) || root == "." {
		root = defaultBackupRoot
	}
	service := &BackupService{db: db, backupRoot: root, now: time.Now}
	service.runnerFactory = service.runnerFor
	return service
}

// NewBackupServiceWithRoot is useful for installations that keep component
// artifacts outside /backups and for isolated recovery tests.
func NewBackupServiceWithRoot(db *gorm.DB, root string) *BackupService {
	service := NewBackupService(db)
	cleanRoot := filepath.Clean(strings.TrimSpace(root))
	if filepath.IsAbs(cleanRoot) && cleanRoot != string(filepath.Separator) {
		service.backupRoot = cleanRoot
	}
	return service
}

// RehearsalResult is structured evidence returned by a recovery exercise.
type RehearsalResult struct {
	BackupID                    uint     `json:"backup_id"`
	Scope                       string   `json:"scope"`
	Format                      string   `json:"format"`
	SourcePaths                 []string `json:"source_paths"`
	SHA256                      string   `json:"sha256"`
	Size                        int64    `json:"size"`
	ExtractedPaths              []string `json:"extracted_paths"`
	Checks                      []string `json:"checks"`
	EvidenceLevel               string   `json:"evidence_level"`
	ApplicationRecoveryVerified bool     `json:"application_recovery_verified"`
	Consistency                 string   `json:"consistency"`
}

// CreateComponentBackup creates a manual artifact and its integrity manifest.
func (s *BackupService) CreateComponentBackup(ctx context.Context, component *models.Component, name, scope string) (*models.Backup, *models.BackupManifest, error) {
	return s.createComponentBackup(ctx, component, name, scope, nil)
}

// CreateScheduledComponentBackup records the schedule that owns retention for
// the artifact. Manual backups and other schedules must never enter its rotation.
func (s *BackupService) CreateScheduledComponentBackup(ctx context.Context, component *models.Component, name, scope string, scheduledBackupID uint) (*models.Backup, *models.BackupManifest, error) {
	if scheduledBackupID == 0 {
		return nil, nil, errors.New("定时备份 ID 无效")
	}
	return s.createComponentBackup(ctx, component, name, scope, &scheduledBackupID)
}

// createComponentBackup creates the artifact, validates its format, calculates
// its checksum and size, and only then commits Backup and BackupManifest rows.
func (s *BackupService) createComponentBackup(ctx context.Context, component *models.Component, name, scope string, scheduledBackupID *uint) (*models.Backup, *models.BackupManifest, error) {
	if s.db == nil {
		return nil, nil, errors.New("备份数据库未配置，请先完成数据库配置")
	}
	if component == nil || component.ID == 0 {
		return nil, nil, errors.New("必须指定有效组件")
	}
	var release func()
	var err error
	ctx, release, err = ClaimComponentOperation(ctx, s.db, component.ID, "backup")
	if err != nil {
		return nil, nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	scope = strings.ToLower(strings.TrimSpace(scope))
	if err := validateBackupScope(scope); err != nil {
		return nil, nil, err
	}
	runner, err := s.runnerFactory(component)
	if err != nil {
		return nil, nil, err
	}
	sources, err := backupSourcePaths(component, scope)
	if err != nil {
		return nil, nil, err
	}
	for _, source := range sources {
		if err := validateSourcePath(source); err != nil {
			return nil, nil, err
		}
	}

	artifactName := safeBackupName(name) + "-" + strconv.FormatInt(s.now().UnixNano(), 10)
	containerPath := filepath.Join(s.backupRoot, artifactName)
	artifactPath := filepath.Join(containerPath, "payload")
	if scope == "data" || scope == "full" {
		artifactPath += ".tar.gz"
	}
	removeOnFailure := true
	defer func() {
		if removeOnFailure {
			s.cleanupPath(runner, containerPath)
		}
	}()
	// Data archives are only read while the managed application is stopped.
	// This covers declared config/data paths, not binaries or external stores.
	quiesced := scope == "data" || scope == "full"
	if quiesced {
		if err := s.beginComponentPause(ctx, component); err != nil {
			return nil, nil, err
		}
		if err := s.stopComponent(ctx, component); err != nil {
			if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
				s.blockComponentRecovery(component.ID, err)
				removeOnFailure = false
				return nil, nil, err
			}
			recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer recoveryCancel()
			return nil, nil, errors.Join(err, s.resumeComponent(recoveryCtx, component))
		}
	}
	format, artifactErr := s.createArtifact(ctx, runner, artifactPath, sources, scope)
	if errors.Is(artifactErr, coressh.ErrRemoteTerminationUnconfirmed) {
		s.blockComponentRecovery(component.ID, artifactErr)
		removeOnFailure = false
		return nil, nil, artifactErr
	}
	if quiesced {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		restartErr := s.resumeComponent(recoveryCtx, component)
		if errors.Is(restartErr, coressh.ErrRemoteTerminationUnconfirmed) {
			s.blockComponentRecovery(component.ID, restartErr)
		}
		recoveryCancel()
		artifactErr = errors.Join(artifactErr, restartErr)
	}
	if artifactErr != nil {
		return nil, nil, artifactErr
	}

	sha256, size, err := inspectArtifact(ctx, runner, artifactPath, scope, format, sources)
	if err != nil {
		return nil, nil, fmt.Errorf("备份校验失败: %w", err)
	}
	if sha256 == "" || size < 0 {
		return nil, nil, errors.New("备份校验未返回完整的完整性证据")
	}

	clusterID := component.ClusterID
	componentID := component.ID
	backup := &models.Backup{
		ScheduledBackupID: scheduledBackupID,
		ComponentID:       &componentID,
		ClusterID:         &clusterID,
		Name:              strings.TrimSpace(name),
		Path:              artifactPath,
		Size:              size,
		Type:              scope,
	}
	if backup.Name == "" {
		backup.Name = "backup-" + s.now().Format("20060102-150405")
	}
	verifiedAt := s.now()
	manifest := &models.BackupManifest{
		Scope:            scope,
		Format:           format,
		SourcePaths:      append([]string(nil), sources...),
		ComponentType:    component.Type,
		ComponentVersion: component.Version,
		SHA256:           sha256,
		Size:             size,
		VerifiedAt:       &verifiedAt,
		Consistency:      "filesystem_copy",
		ScopeLimit:       "Declared configuration path only; application recovery, binaries, service units, containers, and external stores are not included.",
	}
	if quiesced {
		manifest.Consistency = "stopped_application"
		manifest.ScopeLimit = "Declared paths copied while the managed application was stopped; binaries, service units, containers, and external stores are not included. Application query/ingestion recovery is not verified."
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(backup).Error; err != nil {
			return err
		}
		manifest.BackupID = backup.ID
		return tx.Create(manifest).Error
	}); err != nil {
		return nil, nil, fmt.Errorf("记录备份 manifest 失败: %w", err)
	}
	removeOnFailure = false
	return backup, manifest, nil
}

// VerifyComponentBackup refuses legacy, corrupt, cross-component, or
// scope/format-mismatched artifacts and refreshes VerifiedAt only on success.
func (s *BackupService) VerifyComponentBackup(ctx context.Context, backup *models.Backup, component *models.Component) (*models.BackupManifest, error) {
	if backup == nil || backup.ID == 0 || component == nil {
		return nil, errors.New("必须指定备份和组件")
	}
	if backup.ComponentID == nil || *backup.ComponentID != component.ID {
		return nil, errors.New("备份不属于目标组件")
	}
	var manifest models.BackupManifest
	if err := s.db.Where("backup_id = ?", backup.ID).First(&manifest).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("备份 manifest 缺失，已拒绝不安全恢复")
		}
		return nil, fmt.Errorf("加载备份 manifest 失败: %w", err)
	}
	if manifest.Scope != backup.Type {
		return nil, fmt.Errorf("备份 scope 不一致：记录=%s，manifest=%s", backup.Type, manifest.Scope)
	}
	if manifest.ComponentType != component.Type {
		return nil, fmt.Errorf("组件类型不一致：备份=%s，目标=%s", manifest.ComponentType, component.Type)
	}
	if err := validateScopeFormat(manifest.Scope, manifest.Format); err != nil {
		return nil, err
	}
	expectedSources, err := backupSourcePaths(component, manifest.Scope)
	if err != nil {
		return nil, err
	}
	if !equalStringSlices(manifest.SourcePaths, expectedSources) {
		return nil, fmt.Errorf("备份源路径与目标组件不一致：备份=%v，目标=%v", manifest.SourcePaths, expectedSources)
	}
	runner, err := s.runnerFactory(component)
	if err != nil {
		return nil, err
	}
	sha256, size, err := inspectArtifact(ctx, runner, backup.Path, manifest.Scope, manifest.Format, manifest.SourcePaths)
	if err != nil {
		return nil, err
	}
	if size != manifest.Size || size != backup.Size {
		return nil, fmt.Errorf("备份大小不一致：归档=%d，manifest=%d，记录=%d", size, manifest.Size, backup.Size)
	}
	if !strings.EqualFold(sha256, manifest.SHA256) {
		return nil, fmt.Errorf("备份校验和不一致：归档=%s，manifest=%s", sha256, manifest.SHA256)
	}
	now := s.now()
	if err := s.db.Model(&manifest).Update("verified_at", now).Error; err != nil {
		return nil, fmt.Errorf("记录校验证据失败: %w", err)
	}
	manifest.VerifiedAt = &now
	return &manifest, nil
}

// RestoreComponentBackup verifies the artifact, creates a same-scope
// pre_restore checkpoint, atomically replaces live paths, restarts and checks
// readiness. Any failure after the checkpoint automatically restores it.
func (s *BackupService) RestoreComponentBackup(ctx context.Context, backup *models.Backup, component *models.Component) error {
	return s.restoreComponentBackup(ctx, backup, component, nil)
}

// restoreRuntime, when supplied, installs, starts and verifies the matching
// application version only after its saved data has been restored.
func (s *BackupService) restoreComponentBackup(ctx context.Context, backup *models.Backup, component *models.Component, restoreRuntime func(context.Context) error) error {
	if err := s.rejectPairedBackupMutation(ctx, backup); err != nil {
		return err
	}
	if component == nil {
		return errors.New("必须指定组件")
	}
	ctx, release, err := ClaimComponentOperation(ctx, s.db, component.ID, "restore")
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if managedPrometheus(component) && restoreRuntime == nil {
		current, err := s.currentPrometheusRestoreIdentity(ctx, component)
		if err != nil {
			return err
		}
		component = current
		full := backup != nil && backup.Type != "config"
		if _, err := s.prometheusBackupBinding(backup, component, component.Version, full); err != nil {
			return err
		}
		if full {
			if err := s.verifyPrometheusResolvedRoots(ctx, component); err != nil {
				return errors.Join(ErrPrometheusRecoveryAdmission, err)
			}
		}
	}
	manifest, err := s.VerifyComponentBackup(ctx, backup, component)
	if err != nil {
		return fmt.Errorf("恢复前校验失败: %w", err)
	}
	if (manifest.Scope == "config" && manifest.Consistency != "filesystem_copy") ||
		(manifest.Scope != "config" && manifest.Consistency != "stopped_application") {
		return errors.New("备份缺少可信的原始权限或停机一致性证据，仅允许归档校验；请创建新的备份，或在隔离环境中人工恢复并验证旧归档")
	}
	runner, err := s.runnerFactory(component)
	if err != nil {
		return err
	}
	configs, err := s.prepareRestoredConfigs(ctx, runner, component, backup.Path, manifest, fmt.Sprintf("backup:%d", backup.ID))
	if err != nil {
		return err
	}
	if err := s.beginComponentPause(ctx, component); err != nil {
		return err
	}
	if err := s.stopComponent(ctx, component); err != nil {
		if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
			s.blockComponentRecovery(component.ID, err)
			return err
		}
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer recoveryCancel()
		return errors.Join(err, s.resumeComponent(recoveryCtx, component))
	}
	checkpoint, err := s.captureRestoreCheckpoint(ctx, component, manifest.SourcePaths)
	if err != nil {
		if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
			s.blockComponentRecovery(component.ID, err)
			return err
		}
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer recoveryCancel()
		return errors.Join(err, s.resumeComponent(recoveryCtx, component))
	}
	if restoreRuntime != nil {
		// A crash across a binary/schema transition cannot safely replay a
		// data-only checkpoint. Quarantine it until the matched runtime is ready.
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			message := "application version recovery is in progress"
			if err := tx.Model(checkpoint).Updates(map[string]any{"status": "termination_unconfirmed", "error": message}).Error; err != nil {
				return err
			}
			return tx.Model(&ComponentBackupPause{}).Where("component_id = ?", component.ID).
				Updates(map[string]any{"status": "termination_unconfirmed", "error": message}).Error
		}); err != nil {
			return err
		}
	}
	applyErr := s.applyVerifiedBackup(ctx, backup, manifest, component)
	if applyErr == nil {
		if restoreRuntime != nil {
			applyErr = restoreRuntime(ctx)
		} else {
			applyErr = s.restartAndVerify(ctx, component)
		}
	}
	if applyErr == nil {
		applyErr = verifyRestoredConfigs(ctx, runner, configs)
	}
	if applyErr == nil {
		applyErr = s.db.Transaction(func(tx *gorm.DB) error {
			if err := activateRestoredConfigs(tx, configs); err != nil {
				return err
			}
			if restoreRuntime != nil {
				if err := tx.Model(&ComponentBackupPause{}).Where("component_id = ?", component.ID).
					Updates(map[string]any{"status": "resumed", "error": ""}).Error; err != nil {
					return err
				}
			}
			return tx.Model(checkpoint).Updates(map[string]any{"status": "applied", "error": ""}).Error
		})
		if applyErr != nil {
			// A failed COMMIT may nevertheless be durable. Never compensate
			// already-published configuration history against restored files.
			readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
			var durable ComponentRestoreCheckpoint
			readErr := s.db.WithContext(readCtx).First(&durable, checkpoint.ID).Error
			readCancel()
			if readErr != nil {
				return errors.Join(applyErr, readErr, s.blockComponentRecovery(component.ID, applyErr))
			}
			if durable.Status == "applied" {
				return nil
			}
		}
	}
	if applyErr == nil {
		return nil
	}
	if errors.Is(applyErr, coressh.ErrRemoteTerminationUnconfirmed) || errors.Is(applyErr, deployer.ErrBinaryRecoveryUnconfirmed) {
		s.blockComponentRecovery(component.ID, applyErr)
		return errors.Join(applyErr, s.db.Model(checkpoint).Updates(map[string]any{"status": "termination_unconfirmed", "error": applyErr.Error()}).Error)
	}
	// Request cancellation must never prevent compensation after live mutation.
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer recoveryCancel()
	if rollbackErr := s.rollbackCheckpoint(recoveryCtx, checkpoint, component); rollbackErr != nil {
		return fmt.Errorf("恢复失败: %v；还原点 %d 自动回滚失败: %w", applyErr, checkpoint.ID, rollbackErr)
	}
	return fmt.Errorf("恢复失败，已自动回滚至还原点 %d: %w", checkpoint.ID, applyErr)
}

// RehearseComponentBackup verifies and extracts/copies a backup under an
// isolated directory on the component host. It never writes a live path.
func (s *BackupService) RehearseComponentBackup(ctx context.Context, backup *models.Backup, component *models.Component) (RehearsalResult, error) {
	if component == nil {
		return RehearsalResult{}, errors.New("必须指定组件")
	}
	ctx, release, err := ClaimComponentOperation(ctx, s.db, component.ID, "backup-rehearsal")
	if err != nil {
		return RehearsalResult{}, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	manifest, err := s.VerifyComponentBackup(ctx, backup, component)
	if err != nil {
		return RehearsalResult{}, err
	}
	runner, err := s.runnerFactory(component)
	if err != nil {
		return RehearsalResult{}, err
	}
	tempDir := filepath.Join(s.backupRoot, fmt.Sprintf(".rehearse-%d-%d", backup.ID, s.now().UnixNano()))
	if _, err := runner.Run(ctx, "mkdir -p -- "+utils.QuoteShell(tempDir)); err != nil {
		return RehearsalResult{}, fmt.Errorf("创建恢复演练目录失败: %w", err)
	}
	defer s.cleanupPath(runner, tempDir)

	result := RehearsalResult{
		BackupID: backup.ID, Scope: manifest.Scope, Format: manifest.Format,
		SourcePaths: append([]string(nil), manifest.SourcePaths...), SHA256: manifest.SHA256,
		Size: manifest.Size, Checks: []string{"manifest", "checksum", "size", "format"},
		EvidenceLevel: "archive_validation", ApplicationRecoveryVerified: false,
		Consistency: "Archive integrity and isolated extraction only; no application startup, query, or ingestion was exercised.",
	}
	var command string
	switch manifest.Scope {
	case "config":
		destination := filepath.Join(tempDir, "config")
		command = "cp -a -- " + utils.QuoteShell(backup.Path) + " " + utils.QuoteShell(destination)
		result.ExtractedPaths = []string{destination}
	case "data":
		command = "tar --same-permissions -xzf " + utils.QuoteShell(backup.Path) + " -C " + utils.QuoteShell(tempDir)
		result.ExtractedPaths = []string{filepath.Join(tempDir, filepath.Base(manifest.SourcePaths[0]))}
	case "full":
		command = "tar --same-permissions -xzf " + utils.QuoteShell(backup.Path) + " -C " + utils.QuoteShell(tempDir)
		result.ExtractedPaths = []string{
			filepath.Join(tempDir, "config", filepath.Base(manifest.SourcePaths[0])),
			filepath.Join(tempDir, "data", filepath.Base(manifest.SourcePaths[1])),
		}
	}
	if _, err := runner.Run(ctx, command); err != nil {
		return result, fmt.Errorf("解压恢复演练归档失败: %w", err)
	}
	checks := make([]string, 0, len(result.ExtractedPaths))
	for _, extracted := range result.ExtractedPaths {
		checks = append(checks, "test -e "+utils.QuoteShell(extracted))
	}
	if manifest.Scope == "config" || manifest.Scope == "full" {
		configPath := result.ExtractedPaths[0]
		checks = append(checks, "if [ -f "+utils.QuoteShell(configPath)+" ]; then test -s "+utils.QuoteShell(configPath)+"; else test -n \"$(find "+utils.QuoteShell(configPath)+" -type f -size +0c -print -quit 2>/dev/null)\"; fi")
	}
	if _, err := runner.Run(ctx, strings.Join(checks, " && ")); err != nil {
		return result, fmt.Errorf("验证演练 scope 失败: %w", err)
	}
	result.Checks = append(result.Checks, "isolated_extract", "source_scope")
	return result, nil
}

// DeleteBackupArtifact removes both file and directory backup formats.
func (s *BackupService) DeleteBackupArtifact(ctx context.Context, backup *models.Backup, component *models.Component) error {
	if err := s.rejectPairedBackupMutation(ctx, backup); err != nil {
		return err
	}
	if backup == nil || backup.Path == "" {
		return nil
	}
	if backup.ComponentID != nil && component == nil {
		return errors.New("无法确定组件备份所在主机，已拒绝删除归档")
	}
	if component != nil {
		var release func()
		var err error
		ctx, release, err = ClaimComponentOperation(ctx, s.db, component.ID, "backup-delete")
		if err != nil {
			return err
		}
		defer release()
	}
	relative, err := filepath.Rel(s.backupRoot, filepath.Clean(backup.Path))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("备份归档路径 %q 超出备份根目录", backup.Path)
	}
	var runner hostCommandRunner
	if component == nil {
		runner = localBackupRunner{}
	} else {
		runner, err = s.runnerFactory(component)
		if err != nil {
			return err
		}
	}
	deletePath := backup.Path
	if filepath.Base(backup.Path) == "payload" || filepath.Base(backup.Path) == "payload.tar.gz" {
		deletePath = filepath.Dir(backup.Path)
	}
	_, err = runner.Run(ctx, "rm -rf -- "+utils.QuoteShell(deletePath))
	return err
}

func (s *BackupService) createArtifact(ctx context.Context, runner hostCommandRunner, artifactPath string, sources []string, scope string) (string, error) {
	root := filepath.Dir(artifactPath)
	rootCommand := "mkdir -p -- " + utils.QuoteShell(root) + " && chmod 0700 -- " + utils.QuoteShell(s.backupRoot) + " " + utils.QuoteShell(root)
	if _, err := runner.Run(ctx, rootCommand); err != nil {
		return "", fmt.Errorf("创建备份目录失败: %w", err)
	}
	switch scope {
	case "config":
		kindOut, err := runner.Run(ctx, "if [ -f "+utils.QuoteShell(sources[0])+" ]; then printf file; elif [ -d "+utils.QuoteShell(sources[0])+" ]; then printf directory; else exit 2; fi")
		if err != nil {
			return "", fmt.Errorf("配置源不存在：%s: %w", sources[0], err)
		}
		format := strings.TrimSpace(kindOut)
		temp := artifactPath + ".tmp"
		command := "rm -rf -- " + utils.QuoteShell(temp) + " " + utils.QuoteShell(artifactPath) +
			" && cp -aL -- " + utils.QuoteShell(sources[0]) + " " + utils.QuoteShell(temp) +
			" && mv -- " + utils.QuoteShell(temp) + " " + utils.QuoteShell(artifactPath)
		if _, err := runner.Run(ctx, command); err != nil {
			return "", fmt.Errorf("复制配置备份失败: %w", err)
		}
		return format, nil
	case "data", "full":
		return s.createArchiveArtifact(ctx, runner, artifactPath, sources, scope)
	default:
		return "", fmt.Errorf("不支持的备份 scope %q", scope)
	}
}

func (s *BackupService) createArchiveArtifact(ctx context.Context, runner hostCommandRunner, artifactPath string, sources []string, scope string) (string, error) {
	temp := artifactPath + ".tmp"
	if scope == "data" {
		command := "rm -f -- " + utils.QuoteShell(temp) + " " + utils.QuoteShell(artifactPath) +
			" && test -d " + utils.QuoteShell(sources[0]) +
			" && tar -chzf " + utils.QuoteShell(temp) + " -C " + utils.QuoteShell(filepath.Dir(sources[0])) + " " + utils.QuoteShell(filepath.Base(sources[0])) +
			" && chmod 0600 -- " + utils.QuoteShell(temp) +
			" && mv -- " + utils.QuoteShell(temp) + " " + utils.QuoteShell(artifactPath)
		if _, err := runner.Run(ctx, command); err != nil {
			return "", fmt.Errorf("归档数据备份失败: %w", err)
		}
		return "tar.gz", nil
	}

	buildDir := artifactPath + ".build"
	configEntry := filepath.Join(buildDir, "config")
	dataEntry := filepath.Join(buildDir, "data")
	command := "rm -rf -- " + utils.QuoteShell(buildDir) + " " + utils.QuoteShell(temp) + " " + utils.QuoteShell(artifactPath) +
		" && test -e " + utils.QuoteShell(sources[0]) + " && test -d " + utils.QuoteShell(sources[1]) +
		" && mkdir -p -- " + utils.QuoteShell(configEntry) + " " + utils.QuoteShell(dataEntry) +
		" && cp -aL -- " + utils.QuoteShell(sources[0]) + " " + utils.QuoteShell(configEntry) + "/" +
		" && cp -aL -- " + utils.QuoteShell(sources[1]) + " " + utils.QuoteShell(dataEntry) + "/" +
		" && tar -czf " + utils.QuoteShell(temp) + " -C " + utils.QuoteShell(buildDir) + " ." +
		" && chmod 0600 -- " + utils.QuoteShell(temp) +
		" && mv -- " + utils.QuoteShell(temp) + " " + utils.QuoteShell(artifactPath) +
		" && rm -rf -- " + utils.QuoteShell(buildDir)
	if _, err := runner.Run(ctx, command); err != nil {
		return "", fmt.Errorf("归档完整备份失败: %w", err)
	}
	return "tar.gz", nil
}

func (s *BackupService) applyVerifiedBackup(ctx context.Context, backup *models.Backup, manifest *models.BackupManifest, component *models.Component) error {
	runner, err := s.runnerFactory(component)
	if err != nil {
		return err
	}
	stamp := strconv.FormatInt(s.now().UnixNano(), 10)
	atomicReplace := func(source, target string) error {
		parent := filepath.Dir(target)
		fresh := filepath.Join(parent, "."+filepath.Base(target)+".restore-new-"+stamp)
		old := filepath.Join(parent, "."+filepath.Base(target)+".restore-old-"+stamp)
		command := "mkdir -p -- " + utils.QuoteShell(parent) +
			" && rm -rf -- " + utils.QuoteShell(fresh) + " " + utils.QuoteShell(old) +
			" && cp -a -- " + utils.QuoteShell(source) + " " + utils.QuoteShell(fresh) +
			" && { if [ -e " + utils.QuoteShell(target) + " ] || [ -L " + utils.QuoteShell(target) + " ]; then mv -- " + utils.QuoteShell(target) + " " + utils.QuoteShell(old) + "; fi; }" +
			" && mv -- " + utils.QuoteShell(fresh) + " " + utils.QuoteShell(target) +
			" && rm -rf -- " + utils.QuoteShell(old)
		_, err := runner.Run(ctx, command)
		return err
	}

	switch manifest.Scope {
	case "config":
		return atomicReplace(backup.Path, component.ConfigPath)
	case "data":
		return s.extractAndReplaceData(ctx, runner, backup.Path, component.DataPath, stamp)
	case "full":
		extractDir := filepath.Join(s.backupRoot, ".restore-full-"+stamp)
		if _, err := runner.Run(ctx, "mkdir -p -- "+utils.QuoteShell(extractDir)+" && tar --same-permissions -xzf "+utils.QuoteShell(backup.Path)+" -C "+utils.QuoteShell(extractDir)); err != nil {
			return fmt.Errorf("解压完整备份失败: %w", err)
		}
		defer s.cleanupPath(runner, extractDir)
		configSource := filepath.Join(extractDir, "config", filepath.Base(component.ConfigPath))
		dataSource := filepath.Join(extractDir, "data", filepath.Base(component.DataPath))
		if err := atomicReplace(configSource, component.ConfigPath); err != nil {
			return fmt.Errorf("替换配置失败: %w", err)
		}
		if err := atomicReplace(dataSource, component.DataPath); err != nil {
			return fmt.Errorf("替换数据失败: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("不支持的恢复 scope %q", manifest.Scope)
	}
}

func (s *BackupService) extractAndReplaceData(ctx context.Context, runner hostCommandRunner, artifactPath, target, stamp string) error {
	parent := filepath.Dir(target)
	extractDir := filepath.Join(parent, "."+filepath.Base(target)+".restore-extract-"+stamp)
	fresh := filepath.Join(parent, "."+filepath.Base(target)+".restore-new-"+stamp)
	old := filepath.Join(parent, "."+filepath.Base(target)+".restore-old-"+stamp)
	extracted := filepath.Join(extractDir, filepath.Base(target))
	command := "rm -rf -- " + utils.QuoteShell(extractDir) + " " + utils.QuoteShell(fresh) + " " + utils.QuoteShell(old) +
		" && mkdir -p -- " + utils.QuoteShell(extractDir) +
		" && tar --same-permissions -xzf " + utils.QuoteShell(artifactPath) + " -C " + utils.QuoteShell(extractDir) +
		" && mv -- " + utils.QuoteShell(extracted) + " " + utils.QuoteShell(fresh) +
		" && { if [ -e " + utils.QuoteShell(target) + " ] || [ -L " + utils.QuoteShell(target) + " ]; then mv -- " + utils.QuoteShell(target) + " " + utils.QuoteShell(old) + "; fi; }" +
		" && { if mv -- " + utils.QuoteShell(fresh) + " " + utils.QuoteShell(target) +
		"; then rm -rf -- " + utils.QuoteShell(old) + " " + utils.QuoteShell(extractDir) +
		"; else restore_status=$?; mv -- " + utils.QuoteShell(old) + " " + utils.QuoteShell(target) +
		"; rm -rf -- " + utils.QuoteShell(fresh) + " " + utils.QuoteShell(extractDir) +
		"; exit $restore_status; fi; }"
	if _, err := runner.Run(ctx, command); err != nil {
		return fmt.Errorf("原子替换数据目录失败: %w", err)
	}
	return nil
}

func (s *BackupService) stopComponent(ctx context.Context, component *models.Component) error {
	runner, err := s.runnerFactory(component)
	if err != nil {
		return err
	}
	var command string
	switch component.DeployType {
	case "binary":
		if component.ServiceName == "" {
			return errors.New("组件未记录 systemd 服务名称")
		}
		command = "systemctl stop " + utils.QuoteShell(component.ServiceName)
	case "docker", "":
		if component.ServiceName == "" {
			return errors.New("组件未记录容器名称")
		}
		command = "docker stop " + utils.QuoteShell(component.ServiceName)
	default:
		return fmt.Errorf("组件部署类型 %q 不支持恢复前停止", component.DeployType)
	}
	if _, err := runner.Run(ctx, command); err != nil {
		return fmt.Errorf("停止组件失败: %w", err)
	}
	return nil
}

func (s *BackupService) restartAndVerify(ctx context.Context, component *models.Component) (err error) {
	defer func() {
		if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
			err = errors.Join(err, s.blockComponentRecovery(component.ID, err))
		}
	}()
	runner, err := s.runnerFactory(component)
	if err != nil {
		return err
	}
	var restart, processReady string
	switch component.DeployType {
	case "binary":
		if component.ServiceName == "" {
			return errors.New("组件未记录 systemd 服务名称")
		}
		restart = "systemctl restart " + utils.QuoteShell(component.ServiceName)
		processReady = "systemctl is-active --quiet " + utils.QuoteShell(component.ServiceName)
	case "docker", "":
		if component.ServiceName == "" {
			return errors.New("组件未记录容器名称")
		}
		restart = "docker restart " + utils.QuoteShell(component.ServiceName)
		processReady = "test \"$(docker inspect -f '{{.State.Running}}' " + utils.QuoteShell(component.ServiceName) + ")\" = true"
	default:
		return fmt.Errorf("组件部署类型 %q 不支持恢复", component.DeployType)
	}
	if _, err := runner.Run(ctx, restart); err != nil {
		return fmt.Errorf("重启组件失败: %w", err)
	}

	readyURL := componentReadinessURL(component)
	var lastErr error
	for range 10 {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, lastErr = runner.Run(ctx, processReady)
		if lastErr == nil && readyURL != "" {
			_, lastErr = runner.Run(ctx, "curl -fsS --max-time 3 -- "+utils.QuoteShell(readyURL)+" >/dev/null")
		}
		if lastErr == nil {
			return s.db.Model(&ComponentBackupPause{}).Where("component_id = ?", component.ID).Updates(map[string]any{"status": "resumed", "error": ""}).Error
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("组件就绪检查失败: %w", lastErr)
}

func (s *BackupService) runnerFor(component *models.Component) (hostCommandRunner, error) {
	if component == nil {
		return nil, errors.New("必须指定组件")
	}
	if coressh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		return localBackupRunner{}, nil
	}
	if component.Host.ID == 0 || strings.TrimSpace(component.Host.IP) == "" {
		return nil, errors.New("组件未关联有效主机")
	}
	return remoteBackupRunner{component: component}, nil
}

func inspectArtifact(ctx context.Context, runner hostCommandRunner, artifactPath, scope, format string, sourcePaths []string) (string, int64, error) {
	if err := validateScopeFormat(scope, format); err != nil {
		return "", 0, err
	}
	var validateCommand, checksumCommand, sizeCommand string
	switch format {
	case "file":
		validateCommand = "test -f " + utils.QuoteShell(artifactPath)
		checksumCommand = "sha256sum -- " + utils.QuoteShell(artifactPath)
		sizeCommand = "stat -c%s -- " + utils.QuoteShell(artifactPath)
	case "directory":
		validateCommand = "test -d " + utils.QuoteShell(artifactPath) + " && test -z \"$(find " + utils.QuoteShell(artifactPath) + " ! -type f ! -type d -print -quit)\""
		checksumCommand = "cd " + utils.QuoteShell(artifactPath) + " && find . -type f -print0 | sort -z | xargs -0 -r sha256sum | sha256sum"
		sizeCommand = "du -sb -- " + utils.QuoteShell(artifactPath) + " | cut -f1"
	case "tar.gz":
		validateCommand = "test -f " + utils.QuoteShell(artifactPath) + " && tar -tzf " + utils.QuoteShell(artifactPath) +
			" && ! tar -tvzf " + utils.QuoteShell(artifactPath) + " | grep -Eq '^[^-d]'"
		checksumCommand = "sha256sum -- " + utils.QuoteShell(artifactPath)
		sizeCommand = "stat -c%s -- " + utils.QuoteShell(artifactPath)
	}
	validationOut, err := runner.Run(ctx, validateCommand)
	if err != nil {
		return "", 0, fmt.Errorf("归档格式 %s 无效: %w", format, err)
	}
	if format == "tar.gz" {
		entryLines := strings.Split(strings.TrimSpace(validationOut), "\n")
		entries := make([]string, 0, len(entryLines))
		for _, entry := range entryLines {
			if entry = strings.TrimSpace(entry); entry != "" {
				entries = append(entries, entry)
			}
		}
		if err := validateArchiveEntries(entries, scope, sourcePaths); err != nil {
			return "", 0, err
		}
	}
	checksumOut, err := runner.Run(ctx, checksumCommand)
	if err != nil {
		return "", 0, fmt.Errorf("计算 SHA256 失败: %w", err)
	}
	checksumFields := strings.Fields(checksumOut)
	if len(checksumFields) == 0 || len(checksumFields[0]) != 64 {
		return "", 0, fmt.Errorf("SHA256 输出无效：%q", strings.TrimSpace(checksumOut))
	}
	sizeOut, err := runner.Run(ctx, sizeCommand)
	if err != nil {
		return "", 0, fmt.Errorf("计算归档大小失败: %w", err)
	}
	sizeFields := strings.Fields(sizeOut)
	if len(sizeFields) == 0 {
		return "", 0, errors.New("归档大小输出为空")
	}
	size, err := strconv.ParseInt(sizeFields[0], 10, 64)
	if err != nil || size < 0 {
		return "", 0, fmt.Errorf("归档大小无效：%q", strings.TrimSpace(sizeOut))
	}
	return strings.ToLower(checksumFields[0]), size, nil
}

func validateArchiveEntries(entries []string, scope string, sourcePaths []string) error {
	if len(entries) == 0 {
		return errors.New("归档为空")
	}
	normalized := make([]string, 0, len(entries))
	for _, entry := range entries {
		clean := filepath.Clean(strings.TrimPrefix(entry, "./"))
		if filepath.IsAbs(entry) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("归档包含不安全路径 %q", entry)
		}
		normalized = append(normalized, clean)
	}
	within := func(entry, prefix string) bool {
		return entry == prefix || strings.HasPrefix(entry, prefix+string(filepath.Separator))
	}
	containsPrefix := func(prefix string) bool {
		for _, entry := range normalized {
			if within(entry, prefix) {
				return true
			}
		}
		return false
	}
	switch scope {
	case "data":
		if len(sourcePaths) != 1 {
			return errors.New("data 归档的源路径证据无效")
		}
		prefix := filepath.Base(sourcePaths[0])
		for _, entry := range normalized {
			if entry != "." && !within(entry, prefix) {
				return fmt.Errorf("data 归档包含超出声明 scope 的路径：%q", entry)
			}
		}
		if !containsPrefix(prefix) {
			return errors.New("data 归档未包含声明的数据 scope")
		}
	case "full":
		if len(sourcePaths) != 2 {
			return errors.New("full 归档的源路径证据无效")
		}
		configRoot, dataRoot := "config", "data"
		configPrefix := filepath.Join(configRoot, filepath.Base(sourcePaths[0]))
		dataPrefix := filepath.Join(dataRoot, filepath.Base(sourcePaths[1]))
		for _, entry := range normalized {
			if entry != "." && entry != configRoot && entry != dataRoot &&
				!within(entry, configPrefix) && !within(entry, dataPrefix) {
				return fmt.Errorf("full 归档包含超出声明 config/data scope 的路径：%q", entry)
			}
		}
		if !containsPrefix(configPrefix) || !containsPrefix(dataPrefix) {
			return errors.New("full 归档未包含声明的 config 和 data scope")
		}
	}
	return nil
}

func backupSourcePaths(component *models.Component, scope string) ([]string, error) {
	configPath := filepath.Clean(strings.TrimSpace(component.ConfigPath))
	dataPath := filepath.Clean(strings.TrimSpace(component.DataPath))
	switch scope {
	case "config":
		if strings.TrimSpace(component.ConfigPath) == "" {
			return nil, errors.New("组件未配置配置路径")
		}
		return []string{configPath}, nil
	case "data":
		if strings.TrimSpace(component.DataPath) == "" {
			return nil, errors.New("组件未配置数据路径")
		}
		return []string{dataPath}, nil
	case "full":
		if strings.TrimSpace(component.ConfigPath) == "" || strings.TrimSpace(component.DataPath) == "" {
			return nil, errors.New("完整备份要求组件同时配置配置路径和数据路径")
		}
		return []string{configPath, dataPath}, nil
	default:
		return nil, fmt.Errorf("不支持的备份 scope %q", scope)
	}
}

func validateBackupScope(scope string) error {
	if scope != "config" && scope != "data" && scope != "full" {
		return fmt.Errorf("不支持的备份 scope %q；可选值为 config、data 或 full", scope)
	}
	return nil
}

func validateScopeFormat(scope, format string) error {
	if err := validateBackupScope(scope); err != nil {
		return err
	}
	if scope == "config" && format != "file" && format != "directory" {
		return fmt.Errorf("配置备份格式不匹配：%s", format)
	}
	if (scope == "data" || scope == "full") && format != "tar.gz" {
		return fmt.Errorf("%s 备份格式不匹配：%s", scope, format)
	}
	return nil
}

func validateSourcePath(path string) error {
	if !filepath.IsAbs(path) || path == string(filepath.Separator) || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("备份源路径不安全：%q", path)
	}
	return nil
}

func safeBackupName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "backup"
	}
	return strings.Trim(backupNameCleaner.ReplaceAllString(name, "_"), ".")
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func componentReadinessURL(component *models.Component) string {
	base := strings.TrimRight(strings.TrimSpace(component.InternalURL), "/")
	if base == "" && component.Port > 0 {
		base = "http://127.0.0.1:" + strconv.Itoa(component.Port)
	}
	if base == "" {
		return ""
	}
	if parsed, err := url.Parse(base); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return base + models.ManagedHealthPath(component)
}

// ComponentRestoreCheckpoint retains original path absence as well as payload
// metadata. A running row is written before the first live-path mutation.
type ComponentRestoreCheckpoint struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	ComponentID uint      `gorm:"index;not null" json:"component_id"`
	Path        string    `json:"-"`
	Paths       []string  `gorm:"serializer:json;type:text" json:"paths"`
	Present     []bool    `gorm:"serializer:json;type:text" json:"present"`
	Status      string    `gorm:"index" json:"status"`
	Error       string    `json:"error"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *BackupService) cleanupPath(runner hostCommandRunner, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = runner.Run(ctx, "rm -rf -- "+utils.QuoteShell(path))
}

func (s *BackupService) captureRestoreCheckpoint(ctx context.Context, component *models.Component, paths []string) (*ComponentRestoreCheckpoint, error) {
	runner, err := s.runnerFactory(component)
	if err != nil {
		return nil, err
	}
	checkpoint := &ComponentRestoreCheckpoint{
		ComponentID: component.ID, Paths: append([]string(nil), paths...),
		Path:   filepath.Join(s.backupRoot, fmt.Sprintf(".checkpoint-%d-%d", component.ID, s.now().UnixNano())),
		Status: "running",
	}
	if _, err := runner.Run(ctx, "mkdir -p -- "+utils.QuoteShell(checkpoint.Path)+" && chmod 0700 -- "+utils.QuoteShell(checkpoint.Path)); err != nil {
		return nil, err
	}
	for i, path := range paths {
		if err := validateSourcePath(path); err != nil {
			s.cleanupPath(runner, checkpoint.Path)
			return nil, err
		}
		out, err := runner.Run(ctx, "if [ -e "+utils.QuoteShell(path)+" ] || [ -L "+utils.QuoteShell(path)+" ]; then printf present; else printf absent; fi")
		if err != nil {
			s.cleanupPath(runner, checkpoint.Path)
			return nil, err
		}
		present := strings.TrimSpace(out) == "present"
		if !present && strings.TrimSpace(out) != "absent" {
			s.cleanupPath(runner, checkpoint.Path)
			return nil, errors.New("invalid checkpoint path evidence")
		}
		checkpoint.Present = append(checkpoint.Present, present)
		if present {
			dest := filepath.Join(checkpoint.Path, strconv.Itoa(i))
			if _, err := runner.Run(ctx, "cp -a -- "+utils.QuoteShell(path)+" "+utils.QuoteShell(dest)); err != nil {
				s.cleanupPath(runner, checkpoint.Path)
				return nil, err
			}
		}
	}
	if err := s.db.Create(checkpoint).Error; err != nil {
		s.cleanupPath(runner, checkpoint.Path)
		return nil, err
	}
	return checkpoint, nil
}

func (s *BackupService) rollbackCheckpoint(ctx context.Context, checkpoint *ComponentRestoreCheckpoint, component *models.Component) (err error) {
	defer func() {
		if err != nil {
			status := "recovery_required"
			if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
				status = "termination_unconfirmed"
				err = errors.Join(err, s.blockComponentRecovery(component.ID, err))
			}
			err = errors.Join(err, s.db.Model(checkpoint).Updates(map[string]any{"status": status, "error": err.Error()}).Error)
		}
	}()
	if len(checkpoint.Paths) != len(checkpoint.Present) {
		return errors.New("invalid restore checkpoint")
	}
	if err = s.db.Model(&ComponentBackupPause{}).Where("component_id = ?", component.ID).Update("updated_at", s.now()).Error; err != nil {
		return err
	}
	if err = s.stopComponent(ctx, component); err != nil {
		return err
	}
	runner, err := s.runnerFactory(component)
	if err != nil {
		return err
	}
	for i, target := range checkpoint.Paths {
		if err = validateSourcePath(target); err != nil {
			return err
		}
		if checkpoint.Present[i] {
			source := filepath.Join(checkpoint.Path, strconv.Itoa(i))
			fresh := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".rollback-"+strconv.FormatUint(uint64(checkpoint.ID), 10))
			_, err = runner.Run(ctx, "mkdir -p -- "+utils.QuoteShell(filepath.Dir(target))+
				" && rm -rf -- "+utils.QuoteShell(fresh)+" && cp -a -- "+utils.QuoteShell(source)+" "+utils.QuoteShell(fresh)+
				" && rm -rf -- "+utils.QuoteShell(target)+" && mv -- "+utils.QuoteShell(fresh)+" "+utils.QuoteShell(target))
		} else {
			_, err = runner.Run(ctx, "rm -rf -- "+utils.QuoteShell(target))
		}
		if err != nil {
			return err
		}
	}
	if err = s.resumeComponent(ctx, component); err != nil {
		return err
	}
	return s.db.Model(checkpoint).Updates(map[string]any{"status": "rolled_back", "error": ""}).Error
}

// RecoverInterruptedComponentRestores runs after abandoned lifecycle claims are
// released and before accepting traffic. Failed recovery retains the checkpoint.
func RecoverInterruptedComponentRestores(ctx context.Context, db *gorm.DB) error {
	if err := rejectPendingPairedRestoreJournals(ctx, db); err != nil {
		return err
	}
	var transitions int64
	if err := db.Model(&ComponentBackupPause{}).Where("version_transition_plan_id <> 0").Count(&transitions).Error; err != nil {
		return err
	}
	if transitions != 0 {
		return errors.New("Prometheus version transition is unresolved; verify the paired runtime and data and explicitly resolve recovery state before startup")
	}
	var unconfirmed int64
	if err := db.Model(&ComponentBackupPause{}).Where("status = ?", "termination_unconfirmed").Count(&unconfirmed).Error; err != nil {
		return err
	}
	if unconfirmed != 0 {
		return errors.New("remote backup command termination is unconfirmed; verify remote processes stopped and explicitly resolve recovery state before startup")
	}
	var pendingPauses []ComponentBackupPause
	if err := db.Where("status = ?", "paused").Find(&pendingPauses).Error; err != nil {
		return err
	}
	for _, pause := range pendingPauses {
		// After a platform crash there is no command waiter left to confirm
		// exit. Do not race the prior bounded command's surviving watchdog.
		if time.Now().Before(pause.UpdatedAt.Add(10*time.Minute + 10*time.Second)) {
			return fmt.Errorf("component %d backup watchdog may still be active; retry startup after %s", pause.ComponentID, pause.UpdatedAt.Add(10*time.Minute+10*time.Second).Format(time.RFC3339))
		}
	}
	var checkpoints []ComponentRestoreCheckpoint
	if err := db.Where("status IN ?", []string{"running", "recovery_required"}).Order("id DESC").Find(&checkpoints).Error; err != nil {
		return err
	}
	for i := range checkpoints {
		checkpoint := &checkpoints[i]
		var component models.Component
		if err := db.Preload("Host").First(&component, checkpoint.ComponentID).Error; err != nil {
			return fmt.Errorf("checkpoint %d original component metadata unavailable: %w", checkpoint.ID, err)
		}
		claimedCtx, release, err := ClaimComponentOperation(ctx, db, component.ID, "restore-recovery")
		if err != nil {
			return err
		}
		recoveryCtx, cancel := context.WithTimeout(claimedCtx, 3*time.Minute)
		err = NewBackupService(db).rollbackCheckpoint(recoveryCtx, checkpoint, &component)
		cancel()
		release()
		if err != nil {
			return fmt.Errorf("checkpoint %d recovery failed: %w", checkpoint.ID, err)
		}
	}
	var pauses []ComponentBackupPause
	if err := db.Where("status = ?", "paused").Find(&pauses).Error; err != nil {
		return err
	}
	for _, pause := range pauses {
		var component models.Component
		if err := db.Preload("Host").First(&component, pause.ComponentID).Error; err != nil {
			return err
		}
		claimedCtx, release, err := ClaimComponentOperation(ctx, db, component.ID, "restore-recovery")
		if err != nil {
			return err
		}
		recoveryCtx, cancel := context.WithTimeout(claimedCtx, 2*time.Minute)
		err = NewBackupService(db).resumeComponent(recoveryCtx, &component)
		cancel()
		release()
		if err != nil {
			return fmt.Errorf("resume component %d after interrupted backup: %w", component.ID, err)
		}
	}
	return nil
}

// ComponentBackupPause records quiescence before stopping the application, so a
// crash before archive/checkpoint completion does not silently strand it stopped.
type ComponentBackupPause struct {
	ComponentID uint `gorm:"primaryKey;autoIncrement:false"`
	// Independent of backup pause/resume: only the owning version change may clear it.
	VersionTransitionPlanID uint `gorm:"not null;default:0"`
	Status                  string
	Error                   string
	WasRunning              bool
	UpdatedAt               time.Time
}

func (s *BackupService) beginComponentPause(ctx context.Context, component *models.Component) error {
	var existing ComponentBackupPause
	err := s.db.First(&existing, "component_id = ?", component.ID).Error
	if err == nil && existing.Status != "resumed" {
		return errors.New("component has unresolved backup recovery; recover it before another operation")
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if component.ServiceName == "" {
		return errors.New("component has no managed service identity")
	}
	runner, err := s.runnerFactory(component)
	if err != nil {
		return err
	}
	var command string
	switch component.DeployType {
	case "binary":
		command = "state=$(systemctl show --property=LoadState --value " + utils.QuoteShell(component.ServiceName) +
			"); [ \"$state\" = loaded ] || exit 2; if systemctl is-active --quiet " + utils.QuoteShell(component.ServiceName) + "; then printf true; else printf false; fi"
	case "docker", "":
		command = "docker inspect -f '{{.State.Running}}' " + utils.QuoteShell(component.ServiceName)
	default:
		return errors.New("component has no supported managed runtime")
	}
	out, err := runner.Run(ctx, command)
	if err != nil {
		return fmt.Errorf("capture original component runtime state: %w", err)
	}
	state := strings.TrimSpace(out)
	if state != "true" && state != "false" {
		return errors.New("invalid component runtime state evidence")
	}
	return s.db.Save(&ComponentBackupPause{ComponentID: component.ID, Status: "paused", WasRunning: state == "true", VersionTransitionPlanID: existing.VersionTransitionPlanID}).Error
}

func (s *BackupService) resumeComponent(ctx context.Context, component *models.Component) error {
	var pause ComponentBackupPause
	if err := s.db.First(&pause, "component_id = ?", component.ID).Error; err != nil {
		return err
	}
	if pause.WasRunning {
		return s.restartAndVerify(ctx, component)
	}
	return s.db.Model(&pause).Updates(map[string]any{"status": "resumed", "error": ""}).Error
}

func (s *BackupService) blockComponentRecovery(componentID uint, cause error) error {
	pause := ComponentBackupPause{ComponentID: componentID}
	if err := s.db.First(&pause, "component_id = ?", componentID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	pause.Status, pause.Error = "termination_unconfirmed", cause.Error()
	return s.db.Save(&pause).Error
}
