package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/deployer"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func seedRestoredConfigHistory(t *testing.T, db *gorm.DB, c *models.Component, old, current string, includeOld bool) {
	t.Helper()
	if err := db.AutoMigrate(&models.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	if includeOld {
		previous := models.ComponentConfig{ComponentID: c.ID, Name: filepath.Base(c.ConfigPath), ContentType: "yaml", Content: old, Version: 1, FilePath: c.ConfigPath}
		if err := db.Create(&previous).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&previous).Update("is_active", false).Error; err != nil {
			t.Fatal(err)
		}
	}
	latest := models.ComponentConfig{ComponentID: c.ID, Name: filepath.Base(c.ConfigPath), ContentType: "yaml", Content: current, Version: 2, IsActive: true, FilePath: c.ConfigPath}
	if err := db.Create(&latest).Error; err != nil {
		t.Fatal(err)
	}
}
func assertRestoredConfigActive(t *testing.T, db *gorm.DB, c *models.Component, content string, count int64) models.ComponentConfig {
	t.Helper()
	var active []models.ComponentConfig
	if err := db.Where("component_id = ? AND is_active = ?", c.ID, true).Find(&active).Error; err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Content != content || active[0].FilePath != c.ConfigPath {
		t.Fatalf("restored configuration is not the sole active history: %+v", active)
	}
	var total int64
	if err := db.Model(&models.ComponentConfig{}).Where("component_id = ?", c.ID).Count(&total).Error; err != nil {
		t.Fatal(err)
	}
	if total != count {
		t.Fatalf("history count=%d want %d", total, count)
	}
	return active[0]
}
func ordinaryRestoredConfigFixture(t *testing.T, scope string, includeOld bool) (*BackupService, *models.Component, *models.Backup, string, string) {
	t.Helper()
	s, db, _ := newBackupTestService(t)
	c := backupTestComponent(t)
	// This integration isolates generic config/data restoration from runtime-version admission.
	c.Type = models.ComponentNodeExporter
	persistBackupRestoreComponent(t, s, c)
	oldBytes, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	old, current := string(oldBytes), `global:
  scrape_interval: 5s
`
	b, _, err := s.CreateComponentBackup(context.Background(), c, "history", scope)
	if err != nil {
		t.Fatal(err)
	}
	seedRestoredConfigHistory(t, db, c, old, current, includeOld)
	if err := os.WriteFile(c.ConfigPath, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	return s, c, b, old, current
}
func assertNextRestoredConfigPublish(t *testing.T, s *BackupService, c *models.Component, restored string) {
	t.Helper()
	if err := s.db.AutoMigrate(&models.ChangeBaseline{}); err != nil {
		t.Fatal(err)
	}
	cs := &ChangeService{db: s.db, configs: config.NewManager(t.TempDir())}
	c.Status = models.StatusStopped
	c.Cluster = models.Cluster{Name: "restore-test"}
	desired := configChange{Name: filepath.Base(c.ConfigPath), ContentType: "json", Content: `{"scrape_interval":"10s"}`}
	raw, _ := json.Marshal(desired)
	before, after, _, _, _, err := cs.prepare(context.Background(), ChangePlanRequest{Kind: models.ChangeKindConfig, After: raw}, c)
	if err != nil {
		t.Fatal(err)
	}
	var baseline configChange
	if err := json.Unmarshal(before, &baseline); err != nil {
		t.Fatal(err)
	}
	if baseline.Content != restored {
		t.Fatal("new plan did not use restored configuration")
	}
	plan := &models.ChangePlan{ID: 991, Kind: models.ChangeKindConfig, Before: before, After: after}
	if err := s.db.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(c)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Omit("Host", "Cluster").Save(c).Error; err != nil {
		t.Fatal(err)
	}
	// validateBaseline reloads the owning host; retain local read semantics.
	if err := s.db.Save(&c.Host).Error; err != nil {
		t.Fatal(err)
	}
	c.HostID = c.Host.ID
	if err := s.db.Model(c).Update("host_id", c.HostID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.ChangeBaseline{}).Where("plan_id = ?", plan.ID).Update("fingerprint", componentFingerprint(c)).Error; err != nil {
		t.Fatal(err)
	}
	if err := cs.validateBaseline(context.Background(), plan, c); err != nil {
		t.Fatalf("legitimate next plan rejected: %v", err)
	}
	c.Status = models.StatusStopped
	if err := cs.apply(context.Background(), plan, c); err != nil {
		t.Fatalf("next config publication failed: %v", err)
	}
	live, err := os.ReadFile(c.ConfigPath)
	if err != nil || string(live) != desired.Content {
		t.Fatal("next publication did not change live bytes", err)
	}
}
func TestRestoredConfigOrdinaryThenPublication(t *testing.T) {
	for _, scope := range []string{"config", "full"} {
		for _, known := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/known_%t", scope, known), func(t *testing.T) {
				s, c, b, old, _ := ordinaryRestoredConfigFixture(t, scope, known)
				if err := s.RestoreComponentBackup(context.Background(), b, c); err != nil {
					t.Fatal(err)
				}
				count := int64(2)
				active := assertRestoredConfigActive(t, s.db, c, old, count)
				if known && active.Version != 1 {
					t.Fatal("matching existing row was not reused")
				}
				if !known && (active.Version != 3 || !strings.Contains(active.Description, fmt.Sprintf("backup:%d", b.ID))) {
					t.Fatal("new restored history lacks version/provenance")
				}
				assertNextRestoredConfigPublish(t, s, c, old)
			})
		}
	}
}
func TestRestoredConfigOrdinaryMetadataFailureCompensates(t *testing.T) {
	s, c, b, _, current := ordinaryRestoredConfigFixture(t, "config", true)
	injected := false
	if err := s.db.Callback().Update().Before("gorm:update").Register("test:restored-config-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "component_configs" && !injected {
			injected = true
			tx.AddError(errors.New("metadata write rejected"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreComponentBackup(context.Background(), b, c); err == nil {
		t.Fatal("metadata failure was reported successful")
	}
	if !injected {
		t.Fatal("restore never synchronized configuration history")
	}
	live, err := os.ReadFile(c.ConfigPath)
	if err != nil || string(live) != current {
		t.Fatal("metadata failure did not compensate live bytes", err)
	}
	assertRestoredConfigActive(t, s.db, c, current, 2)
	var applied int64
	s.db.Model(&ComponentRestoreCheckpoint{}).Where("status = ?", "applied").Count(&applied)
	if applied != 0 {
		t.Fatal("failed metadata transaction published applied checkpoint")
	}
}
func TestRestoredConfigPairedHistoryAndCompensation(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("compensate_%t", fail), func(t *testing.T) {
			t.Parallel()
			f, r, staged := executionFixture(t)
			old, current := `global:
  scrape_interval: 1s
`, `global:
  scrape_interval: 9s
`
			seedRestoredConfigHistory(t, f.db, f.c, old, current, true)
			r.failHistorical = fail
			row, err := executionAPI(t, f.s).ExecutePrometheusPairedRestore(context.Background(), staged.ID)
			want := old
			if fail {
				want = current
				if err == nil || row == nil || row.Phase != "restore_compensated" {
					t.Fatal("missing compensated result", err)
				}
			} else if err != nil || row == nil || row.Phase != "restore_completed" {
				t.Fatal("missing successful restore", err)
			}
			assertRestoredConfigActive(t, f.db, f.c, want, 2)
			assertExecutionFiles(t, f, !fail)
		})
	}
}

func TestRestoredConfigOrdinaryTerminalFailureIsAtomic(t *testing.T) {
	s, c, b, _, current := ordinaryRestoredConfigFixture(t, "config", false)
	injected := false
	if err := s.db.Callback().Update().Before("gorm:update").Register("test:restored-checkpoint-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "component_restore_checkpoints" && !injected {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok && values["status"] == "applied" {
				injected = true
				tx.AddError(errors.New("terminal write rejected"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreComponentBackup(context.Background(), b, c); err == nil || !injected {
		t.Fatal("terminal failure did not occur", err)
	}
	live, err := os.ReadFile(c.ConfigPath)
	if err != nil || string(live) != current {
		t.Fatal("failed terminal did not restore current file", err)
	}
	assertRestoredConfigActive(t, s.db, c, current, 1)
	var checkpoint ComponentRestoreCheckpoint
	if err := s.db.Last(&checkpoint).Error; err != nil || checkpoint.Status != "rolled_back" {
		t.Fatal("terminal failure not compensated", checkpoint, err)
	}
}
func TestRestoredConfigRejectsBadPathAndChangedSourceBeforeMutation(t *testing.T) {
	for _, mode := range []string{"name", "source"} {
		t.Run(mode, func(t *testing.T) {
			s, c, b, _, current := ordinaryRestoredConfigFixture(t, "config", true)
			if mode == "name" {
				if err := s.db.Model(&models.ComponentConfig{}).Where("component_id = ?", c.ID).Update("name", "../outside.yml").Error; err != nil {
					t.Fatal(err)
				}
			}
			sourceChanged := false
			stopped := false
			s.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
				return backupControlRunner{before: func(_ context.Context, command string) error {
					if strings.HasPrefix(command, "systemctl stop") {
						stopped = true
					}
					if mode == "source" && strings.Contains(command, "head -c 4194305") && !sourceChanged {
						sourceChanged = true
						return os.WriteFile(b.Path, []byte("changed-source"), 0600)
					}
					return nil
				}}, nil
			}
			if err := s.RestoreComponentBackup(context.Background(), b, c); err == nil {
				t.Fatal("unsafe source/path was accepted")
			}
			if stopped {
				t.Fatal("source/path rejection occurred after mutation boundary")
			}
			live, err := os.ReadFile(c.ConfigPath)
			if err != nil || string(live) != current {
				t.Fatal("source/path rejection changed live file", err)
			}
			var checkpoints int64
			s.db.Model(&ComponentRestoreCheckpoint{}).Count(&checkpoints)
			if checkpoints != 0 {
				t.Fatal("source/path rejection captured mutation checkpoint")
			}
		})
	}
}
func TestRestoredConfigDataOnlyKeepsConfigurationHistory(t *testing.T) {
	s, c, b, _, current := ordinaryRestoredConfigFixture(t, "data", true)
	if err := s.RestoreComponentBackup(context.Background(), b, c); err != nil {
		t.Fatal(err)
	}
	assertRestoredConfigActive(t, s.db, c, current, 2)
	live, err := os.ReadFile(c.ConfigPath)
	if err != nil || string(live) != current {
		t.Fatal("data-only restore changed config", err)
	}
}
func TestRestoredConfigPairedTerminalFailureIsAtomic(t *testing.T) {
	t.Parallel()
	f, _, staged := executionFixture(t)
	old, current := "global:\n  scrape_interval: 1s\n", "global:\n  scrape_interval: 9s\n"
	seedRestoredConfigHistory(t, f.db, f.c, old, current, false)
	injected := false
	if err := f.db.Callback().Update().Before("gorm:update").Register("test:restored-paired-terminal", func(tx *gorm.DB) {
		if tx.Statement.Table == "components" && !injected {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok && values["version"] == "2.55.0" {
				injected = true
				tx.AddError(errors.New("terminal component write rejected"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	row, err := executionAPI(t, f.s).ExecutePrometheusPairedRestore(context.Background(), staged.ID)
	if err == nil || !injected || row == nil || row.Phase != "restore_compensated" {
		t.Fatal("metadata failure did not compensate", err)
	}
	assertRestoredConfigActive(t, f.db, f.c, current, 1)
	assertExecutionFiles(t, f, false)
	journalAssertClaimsEmpty(t, f.db)
}

func TestRestoredConfigDirectoryBackupPredatesManagedFile(t *testing.T) {
	for _, scope := range []string{"config", "full"} {
		t.Run(scope, func(t *testing.T) {
			s, db, _ := newBackupTestService(t)
			c := backupTestComponent(t)
			c.Type = models.ComponentNodeExporter
			c.ConfigPath = filepath.Join(filepath.Dir(c.ConfigPath), "configs")
			if err := os.Mkdir(c.ConfigPath, 0700); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(c.ConfigPath, "base.yml")
			later := filepath.Join(c.ConfigPath, "later.yml")
			if err := os.WriteFile(base, []byte("before-directory-backup\n"), 0600); err != nil {
				t.Fatal(err)
			}
			persistBackupRestoreComponent(t, s, c)
			b, m, err := s.CreateComponentBackup(context.Background(), c, "directory-history", scope)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&models.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{base, later} {
				if err := os.WriteFile(path, []byte("current-config\n"), 0600); err != nil {
					t.Fatal(err)
				}
				row := models.ComponentConfig{ComponentID: c.ID, Name: filepath.Base(path), FilePath: path, Content: "current-config\n", ContentType: "yaml", Version: 2, IsActive: true}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			c.DeployType = "docker" // publication resolves directory roots by managed name.
			configs, err := s.prepareRestoredConfigs(context.Background(), backupControlRunner{}, c, b.Path, m, fmt.Sprintf("backup:%d", b.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.applyVerifiedBackup(context.Background(), b, m, c); err != nil {
				t.Fatal(err)
			}
			if err := verifyRestoredConfigs(context.Background(), backupControlRunner{}, configs); err != nil {
				t.Fatal(err)
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return activateRestoredConfigs(tx, configs) }); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(later); !os.IsNotExist(err) {
				t.Fatal("later file was not absent after historical directory restore", err)
			}
			var active []models.ComponentConfig
			if err := db.Where("component_id = ? AND is_active = ?", c.ID, true).Find(&active).Error; err != nil {
				t.Fatal(err)
			}
			if len(active) != 1 || active[0].Name != "base.yml" || active[0].Content != "before-directory-backup\n" {
				t.Fatal("directory history inconsistent")
			}
			var count int64
			db.Model(&models.ComponentConfig{}).Where("component_id = ?", c.ID).Count(&count)
			if count != 3 {
				t.Fatal("directory restore lost existing history", count)
			}
		})
	}
}

type restoredConfigReadFailureRunner struct {
	cause error
	calls int
}

func (r *restoredConfigReadFailureRunner) Run(context.Context, string) (string, error) {
	r.calls++
	return "credential=RESTORED_CONFIG_PRIVATE_SENTINEL", r.cause
}
func TestRestoredConfigReadFailureDoesNotExposeOutput(t *testing.T) {
	s, db, _ := newBackupTestService(t)
	c := backupTestComponent(t)
	row := models.ComponentConfig{ComponentID: c.ID, Name: filepath.Base(c.ConfigPath), FilePath: c.ConfigPath, ContentType: "yaml", Content: "old-config", Version: 1, IsActive: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	classes := []struct {
		name  string
		cause error
	}{
		{"ordinary", errors.New("read transport failed")},
		{"cancelled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"remote_termination", coressh.ErrRemoteTerminationUnconfirmed},
		{"binary_recovery", deployer.ErrBinaryRecoveryUnconfirmed},
		{"remote_cancelled", errors.Join(coressh.ErrRemoteTerminationUnconfirmed, context.Canceled)},
	}
	for _, phase := range []string{"source_file", "source_archive", "live", "absent"} {
		for _, class := range classes {
			t.Run(phase+"/"+class.name, func(t *testing.T) {
				injected := fmt.Errorf("command output credential=RESTORED_CONFIG_PRIVATE_SENTINEL: %w", class.cause)
				r := &restoredConfigReadFailureRunner{cause: injected}
				var got error
				if strings.HasPrefix(phase, "source_") {
					m := &models.BackupManifest{Scope: "config", Format: "file", SourcePaths: []string{c.ConfigPath}}
					if phase == "source_archive" {
						m.Scope = "full"
						m.Format = "tar.gz"
						m.SourcePaths = append(m.SourcePaths, c.DataPath)
					}
					_, got = s.prepareRestoredConfigs(context.Background(), r, c, "/private/backup/payload", m, "backup:1")
				} else {
					got = verifyRestoredConfigs(context.Background(), r, []restoredConfigActivation{{Present: phase == "live", ComponentID: c.ID, Name: row.Name, FilePath: c.ConfigPath, Content: "old-config"}})
				}
				if got == nil || r.calls != 1 {
					t.Fatal("read boundary did not fail once")
				}
				if strings.Contains(got.Error(), "RESTORED_CONFIG_PRIVATE_SENTINEL") {
					t.Fatal("read boundary exposed private command output")
				}
				if errors.Is(got, injected) {
					t.Fatal("raw command-output error remains reachable")
				}
				for _, safeClass := range []error{context.Canceled, context.DeadlineExceeded, coressh.ErrRemoteTerminationUnconfirmed, deployer.ErrBinaryRecoveryUnconfirmed} {
					if errors.Is(got, safeClass) != errors.Is(injected, safeClass) {
						t.Fatalf("read boundary changed safety class %v", safeClass)
					}
				}
			})
		}
	}
}
