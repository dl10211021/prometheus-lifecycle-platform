package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type pointRunFunc func(context.Context, string) (string, error)

func (f pointRunFunc) Run(ctx context.Context, cmd string) (string, error) { return f(ctx, cmd) }
func pointNoRecords(t *testing.T, f *pointFixture) {
	t.Helper()
	for _, m := range []any{&models.Backup{}, &models.BackupManifest{}, &models.PrometheusRecoveryPoint{}, &models.HostOperationClaim{}, &models.ComponentOperationClaim{}} {
		var count int64
		if err := f.db.Model(m).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("unexpected durable/claim rows: %T %d %v", m, count, err)
		}
	}
	var plan models.ChangePlan
	if err := f.db.First(&plan, f.plan.ID).Error; err != nil || plan.BackupID != nil {
		t.Fatalf("plan acquired partial backup: %v", err)
	}
}
func pointPause(t *testing.T, f *pointFixture, want string) {
	t.Helper()
	var p ComponentBackupPause
	if err := f.db.First(&p, "component_id = ?", f.c.ID).Error; err != nil || p.Status != want {
		t.Fatalf("pause=%s want=%s: %v", p.Status, want, err)
	}
}
func TestPrometheusRecoveryPointAdmission(t *testing.T) {
	cases := map[string]func(*pointFixture){
		"wrong-status":    func(f *pointFixture) { f.db.Model(f.plan).Update("status", "pending") },
		"missing-history": func(f *pointFixture) { f.db.Model(f.plan).Update("before", json.RawMessage("{}")) },
		"missing-argv-hash": func(f *pointFixture) {
			var b prometheusBefore
			json.Unmarshal(f.plan.Before, &b)
			b.ImportedArgsSHA256 = ""
			raw, _ := json.Marshal(b)
			f.db.Model(f.plan).Update("before", raw)
		},
		"version-spelling-drift": func(f *pointFixture) { f.db.Model(f.c).Update("version", "v2.55.0") },
		"source-overlap":         func(f *pointFixture) { f.s.backupRoot = f.c.DataPath },
		"paused": func(f *pointFixture) {
			f.db.Create(&ComponentBackupPause{ComponentID: f.c.ID, Status: "paused", WasRunning: true})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPointFixture(t)
			setup(f)
			point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
			if point != nil || err == nil {
				t.Fatal("invalid capture admitted")
			}
			if len(f.runner.calls) != 0 {
				t.Fatal("invalid admission reached host")
			}
			pointNoRecords(t, f)
		})
	}
}
func TestPrometheusRecoveryPointFailureCompensation(t *testing.T) {
	for _, kind := range []string{"stop", "archive", "cancel", "unconfirmed", "startup-drift", "binary-drift", "resume", "ready-record"} {
		t.Run(kind, func(t *testing.T) {
			f := newPointFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hit := false
			if kind == "ready-record" {
				if err := f.db.Exec("CREATE TRIGGER fail_ready BEFORE UPDATE OF state ON prometheus_recovery_points WHEN NEW.state = 'ready' BEGIN SELECT RAISE(ABORT, 'injected ready failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			f.runner.before = func(ctx context.Context, cmd string) error {
				if strings.HasPrefix(cmd, "systemctl restart ") && kind == "resume" {
					hit = true
					return errors.New("private-error-sentinel")
				}
				if strings.HasPrefix(cmd, "systemctl stop ") && kind == "stop" {
					hit = true
					return errors.New("private-error-sentinel")
				}
				if strings.Contains(cmd, " && tar -czf ") && !hit {
					hit = true
					switch kind {
					case "archive":
						return errors.New("private-error-sentinel")
					case "cancel":
						cancel()
						return context.Canceled
					case "unconfirmed":
						return coressh.ErrRemoteTerminationUnconfirmed
					case "startup-drift":
						return os.WriteFile(f.runner.environment, []byte("changed\n"), 0600)
					case "binary-drift":
						return os.WriteFile(f.c.BinaryPath, []byte("changed\n"), 0755)
					}
				}
				return nil
			}
			point, err := f.s.CreatePrometheusRecoveryPoint(ctx, f.plan.ID)
			if err == nil || point != nil {
				t.Fatal("failed capture reported success")
			}
			if strings.Contains(err.Error(), "private-error-sentinel") {
				t.Fatal("host error leaked")
			}
			if kind == "resume" || kind == "ready-record" {
				var saved models.PrometheusRecoveryPoint
				if err := f.db.First(&saved, "plan_id = ?", f.plan.ID).Error; err != nil || saved.State != "captured" {
					t.Fatalf("lost published point: %+v %v", saved, err)
				}
				f.runner.before = nil
				proof, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID)
				if err != nil || proof.CaptureState != "captured" {
					t.Fatalf("captured history unavailable: %v", err)
				}
			} else {
				pointNoRecords(t, f)
			}
			blocked := kind == "unconfirmed" || kind == "startup-drift" || kind == "binary-drift" || kind == "resume"
			if blocked {
				pointPause(t, f, "termination_unconfirmed")
				if f.runner.running {
					t.Fatal("unsafe restart after uncertain/drifted baseline")
				}
			} else {
				pointPause(t, f, "resumed")
				if !f.runner.running {
					t.Fatal("consistent original not resumed")
				}
			}
		})
	}
}
func TestPrometheusRecoveryPointAtomicPublication(t *testing.T) {
	for _, table := range []string{"backups", "backup_manifests", "prometheus_recovery_points", "change_plans"} {
		t.Run(table, func(t *testing.T) {
			f := newPointFixture(t)
			event := "INSERT"
			if table == "change_plans" {
				event = "UPDATE OF backup_id"
			}
			sql := "CREATE TRIGGER fail_point BEFORE " + event + " ON " + table + " BEGIN SELECT RAISE(ABORT, 'injected publication failure'); END"
			if err := f.db.Exec(sql).Error; err != nil {
				t.Fatal(err)
			}
			if p, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); p != nil || err == nil {
				t.Fatal("partial publication accepted")
			}
			pointNoRecords(t, f)
			pointPause(t, f, "resumed")
			if !f.runner.running {
				t.Fatal("publication failure left original stopped")
			}
		})
	}
}
func TestPrometheusRecoveryPointPayloadStrict(t *testing.T) {
	f := newPointFixture(t)
	p := prometheusRecoveryPayload{Schema: 1, BeforeSHA256: pointDigest(f.plan.Before), AfterSHA256: pointDigest(f.plan.After), Binding: makeUnitSnapshotBinding(f.c, importedArgsHash(f.c.ImportedArgs)), Runtime: recoveryPointRuntime{PID: "1", StartTime: "1", ArgvSHA256: pointDigest([]byte("argv")), ConfigIdentity: "1:2", DataIdentity: "1:3"}}
	raw, _ := json.Marshal(p)
	if _, err := decodeRecoveryPointPayload(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRecoveryPointPayload(append(raw, []byte(" {}")...)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}
func TestPrometheusRecoveryPointNewProcess(t *testing.T) {
	if os.Getenv("PLC_POINT_CHILD") != "1" {
		return
	}
	db, err := platformsqlite.OpenDB(os.Getenv("PLC_POINT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	s := NewBackupServiceWithRoot(db, os.Getenv("PLC_POINT_ROOT"))
	proof, err := s.VerifyPrometheusRecoveryPoint(context.Background(), 1)
	if err != nil || proof.HistoricalVersion != "2.55.0" {
		t.Fatalf("fresh process verify: %+v %v", proof, err)
	}
}
func TestPrometheusRecoveryPointCorruptionAndReopen(t *testing.T) {
	f := newPointFixture(t)
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrometheusRecoveryPointNewProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "PLC_POINT_CHILD=1", "PLC_POINT_DB="+f.dbPath, "PLC_POINT_ROOT="+f.s.backupRoot)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("new process: %s %v", out, err)
	}
	p, err := decodeRecoveryPointPayload(point.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(p.Binary.Directory, "prometheus"), p.Archive.Path, filepath.Join(p.Startup.Path, "manifest.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("corrupt\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if proof, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID); proof != nil || err == nil {
			t.Fatal("corrupt historical artifact accepted")
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	extra := filepath.Join(p.Binary.Directory, "unexpected")
	os.WriteFile(extra, []byte("x"), 0600)
	if _, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID); err == nil {
		t.Fatal("extra artifact accepted")
	}
	os.Remove(extra)
	if err := os.Chmod(p.Archive.Path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID); err == nil {
		t.Fatal("unprotected archive accepted")
	}
	os.Chmod(p.Archive.Path, 0600)
	var original models.BackupManifest
	if err := f.db.First(&original, "backup_id = ?", point.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	f.db.Model(&original).Update("sha256", strings.Repeat("0", 64))
	f.runner.calls = nil
	if _, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID); err == nil {
		t.Fatal("reassociated manifest accepted")
	}
	if len(f.runner.calls) != 0 {
		t.Fatal("invalid DB association reached host")
	}
	f.db.Model(&original).Update("sha256", p.Archive.SHA256)
	if _, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID); err != nil {
		t.Fatal(err)
	}
	if point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); point != nil || err == nil {
		t.Fatal("immutable history recaptured")
	}
}

func TestPrometheusRecoveryPointCrashFenceBeforeStop(t *testing.T) {
	f := newPointFixture(t)
	observed := false
	f.runner.before = func(ctx context.Context, cmd string) error {
		if strings.HasPrefix(cmd, "systemctl stop ") {
			var pause ComponentBackupPause
			if err := f.db.First(&pause, "component_id = ?", f.c.ID).Error; err != nil {
				return err
			}
			observed = pause.Status == "termination_unconfirmed"
			return errors.New("simulate controller interruption before stop")
		}
		return nil
	}
	if _, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); err == nil {
		t.Fatal("injected interruption not reached")
	}
	if !observed {
		t.Fatal("service stop was reachable without durable crash fence")
	}
}
func TestPrometheusRecoveryPointObservedRuntimeRejected(t *testing.T) {
	for _, kind := range []string{"argv-path", "version", "pid-race", "oversized-argv", "nested-mount", "root-namespace", "old-process-survived"} {
		t.Run(kind, func(t *testing.T) {
			f := newPointFixture(t)
			wrapper := pointRunFunc(func(ctx context.Context, cmd string) (string, error) {
				if strings.Contains(cmd, "# promlifecycle:recovery-point-runtime") {
					out, err := f.runner.Run(ctx, cmd)
					if err != nil {
						return out, err
					}
					switch kind {
					case "nested-mount", "root-namespace":
						return "", errors.New("namespace evidence rejected")
					case "argv-path":
						return strings.Replace(out, base64.StdEncoding.EncodeToString([]byte(f.c.BinaryPath+"\x00--config.file="+f.c.ConfigPath+"\x00--storage.tsdb.path="+f.c.DataPath+"\x00")), base64.StdEncoding.EncodeToString([]byte(f.c.BinaryPath+"\x00--config.file=/wrong/config\x00--storage.tsdb.path="+f.c.DataPath+"\x00")), 1), nil
					case "pid-race":
						parts := strings.Split(out, "\n")
						parts[len(parts)-2] = strings.TrimSuffix(pointProcStat(f.runner.pid, f.runner.generation+1), "\n")
						return strings.Join(parts, "\n"), nil
					case "oversized-argv":
						parts := strings.Split(out, "\n")
						parts[1] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 65537)))
						return strings.Join(parts, "\n"), nil
					}
					return out, nil
				}
				if kind == "version" && strings.Contains(cmd, "# promlifecycle:recovery-point-version") {
					return "prometheus, version 3.1.0\n", nil
				}
				if kind == "old-process-survived" && strings.Contains(cmd, "# promlifecycle:recovery-point-gone") {
					return pointProcStat(1234, 100), nil
				}
				return f.runner.Run(ctx, cmd)
			})
			f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return wrapper, nil }
			if point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); point != nil || err == nil {
				t.Fatal("contradictory runtime evidence accepted")
			}
			pointNoRecords(t, f)
			if kind == "old-process-survived" {
				pointPause(t, f, "termination_unconfirmed")
				if f.runner.running {
					t.Fatal("surviving old process allowed restart")
				}
			} else {
				if !f.runner.running {
					t.Fatal("pre-admission runtime evidence stopped service")
				}
			}
		})
	}
}

func TestPrometheusRecoveryPointPinnedBackupCannotBeDeleted(t *testing.T) {
	f := newPointFixture(t)
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	var backup models.Backup
	if err := f.db.First(&backup, point.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	f.runner.calls = nil
	if err := f.s.DeleteBackupArtifact(context.Background(), &backup, f.c); err == nil {
		t.Fatal("pinned backup deletion accepted")
	}
	if len(f.runner.calls) != 0 {
		t.Fatal("pinned deletion reached host")
	}
	if _, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID); err != nil {
		t.Fatal(err)
	}
}
func TestPrometheusRecoveryPointReentrantTransitionBlocked(t *testing.T) {
	f := newPointFixture(t)
	ctx, releaseHost, err := ClaimHostOperation(context.Background(), f.db, f.c.HostID, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHost()
	ctx, release, err := ClaimComponentOperation(ctx, f.db, f.c.ID, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := f.db.Create(&ComponentBackupPause{ComponentID: f.c.ID, Status: "paused", VersionTransitionPlanID: f.plan.ID}).Error; err != nil {
		t.Fatal(err)
	}
	reached := false
	err = f.s.withRecoveryPointClaim(ctx, f.plan.ID, true, func(context.Context, *models.ChangePlan, *models.Component) error { reached = true; return nil })
	if err == nil || reached {
		t.Fatal("reentrant owner bypassed transition exclusion")
	}
}

func TestPrometheusRecoveryPointPinnedBackupCannotUseGenericRestore(t *testing.T) {
	f := newPointFixture(t)
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	var backup models.Backup
	if err := f.db.First(&backup, point.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	for _, callback := range []func(context.Context) error{nil, func(context.Context) error { t.Error("generic runtime callback invoked"); return nil }} {
		f.runner.calls = nil
		err := f.s.restoreComponentBackup(context.Background(), &backup, f.c, callback)
		if !errors.Is(err, ErrPrometheusRecoveryPoint) || len(f.runner.calls) != 0 {
			t.Fatalf("paired archive reached generic restore: %v commands=%d", err, len(f.runner.calls))
		}
	}
}

func TestPrometheusRecoveryPointLoadedLaunchDriftBeforeStop(t *testing.T) {
	f := newPointFixture(t)
	wrapper := pointRunFunc(func(ctx context.Context, cmd string) (string, error) {
		if strings.Contains(cmd, "# promlifecycle:recovery-point-launch") {
			return "{ path=/other/prometheus ; argv[]=/other/prometheus --config.file=" + f.c.ConfigPath + " --storage.tsdb.path=" + f.c.DataPath + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n", nil
		}
		return f.runner.Run(ctx, cmd)
	})
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return wrapper, nil }
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if point != nil || err == nil {
		t.Fatal("reloaded unit pointing at another binary admitted")
	}
	for _, cmd := range f.runner.calls {
		if strings.HasPrefix(cmd, "systemctl stop ") {
			t.Fatal("contradictory loaded launch stopped original")
		}
	}
}
func TestPrometheusRecoveryPointFenceHeldThroughPostStartValidation(t *testing.T) {
	f := newPointFixture(t)
	checked := false
	f.runner.before = func(ctx context.Context, cmd string) error {
		if f.runner.generation > 100 && strings.Contains(cmd, "# promlifecycle:recovery-point-runtime") {
			checked = true
			var pause ComponentBackupPause
			if err := f.db.First(&pause, "component_id = ?", f.c.ID).Error; err != nil {
				return err
			}
			if pause.Status != "termination_unconfirmed" {
				t.Error("paired validation ran after crash fence was cleared")
			}
		}
		return nil
	}
	if _, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("post-start validation not reached")
	}
}

func TestPrometheusRecoveryPointPauseNeverTemporarilyUnfenced(t *testing.T) {
	f := newPointFixture(t)
	if err := f.db.Exec("CREATE TRIGGER reject_unfenced BEFORE INSERT ON component_backup_pauses WHEN NEW.status = 'paused' BEGIN SELECT RAISE(ABORT, 'unfenced intermediate pause'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); err != nil {
		t.Fatal("paired capture persisted a crash-recoverable intermediate pause", err)
	}
}
func TestPrometheusRecoveryPointClaimExclusion(t *testing.T) {
	for _, kind := range []string{"host-held", "reverse-order", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := newPointFixture(t)
			ctx := context.Background()
			switch kind {
			case "host-held":
				_, release, err := ClaimHostOperation(ctx, f.db, f.c.HostID, "other")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "reverse-order":
				claimed, release, err := ClaimComponentOperation(ctx, f.db, f.c.ID, "reverse")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				ctx = claimed
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if point, err := f.s.CreatePrometheusRecoveryPoint(ctx, f.plan.ID); point != nil || err == nil {
				t.Fatal("claim exclusion bypassed")
			}
			if len(f.runner.calls) != 0 {
				t.Fatal("excluded call reached host")
			}
		})
	}
}
