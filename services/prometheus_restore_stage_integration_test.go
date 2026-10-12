package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

// The wrapper always executes the real protected copy before losing its result.
// It never simulates copying or rewrites the production foreground command.
type stageRealCopyObserver struct {
	runner     hostCommandRunner
	beforeCopy func()
	interrupt  bool
	completed  int
}

func (r *stageRealCopyObserver) Run(ctx context.Context, command string) (string, error) {
	copying := strings.Contains(command, "# promlifecycle:restore-stage-copy")
	if copying && r.beforeCopy != nil {
		r.beforeCopy()
	}
	out, err := r.runner.Run(ctx, command)
	if copying && err == nil {
		r.completed++
		if r.interrupt {
			return "", errors.New("stage fixture lost completed copy response")
		}
	}
	return out, err
}

func TestPrometheusRestoreStageRealFixtureOptIn(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_PAIRED_RESTORE_STAGE", "")
	// Nil dependencies prove ordinary legacy/checkpoint runs return before use.
	pointRealRestoreStage(t, context.Background(), nil, nil, nil, nil, "", "", "", "")
}

func TestPrometheusRestoreStageRealProbeBuildInfo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		wantOK bool
	}{
		{"running_version", http.StatusOK, `{"status":"success","data":{"version":"2.55.0","revision":"probe-process"}}`, true},
		{"wrong_running_version", http.StatusOK, `{"status":"success","data":{"version":"3.1.0"}}`, false},
		{"api_error", http.StatusOK, `{"status":"error","data":{"version":"2.55.0"}}`, false},
		{"http_error", http.StatusServiceUnavailable, `{"status":"success","data":{"version":"2.55.0"}}`, false},
		{"malformed_json", http.StatusOK, `{`, false},
		{"missing_version", http.StatusOK, `{"status":"success","data":{}}`, false},
		{"archived_query_sample", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"prometheus_build_info","version":"2.55.0"},"value":[1,"1"]}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests <- request.Method + " " + request.URL.RequestURI()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			port, err := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = stageRealProbeBuildInfo(ctx, localBackupRunner{}, port, "2.55.0")
			if (err == nil) != tc.wantOK {
				t.Fatalf("running probe build response accepted=%t, want %t", err == nil, tc.wantOK)
			}
			select {
			case path := <-requests:
				if path != "GET /api/v1/status/buildinfo" {
					t.Fatalf("wrong process identity response path: %s", path)
				}
			default:
				t.Fatal("probe identity check did not reach the local HTTP response boundary")
			}
		})
	}
}

// Called only after checkpoint probe cleanup and stopped-baseline assertions.
func pointRealRestoreStage(t *testing.T, ctx context.Context, s *BackupService, r hostCommandRunner, c *models.Component, ready *models.PrometheusRestoreTransaction, root, unit, sampleTime, sample string) {
	t.Helper()
	if os.Getenv("PROMLIFECYCLE_PAIRED_RESTORE_STAGE") != "1" {
		return
	}
	business := transactionBusinessState(t, s.db)
	admission, err := decodeRestoreJournal(ready)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := decodeRestoreCheckpoint(ready, admission)
	if err != nil {
		t.Fatal(err)
	}
	var point models.PrometheusRecoveryPoint
	if err := s.db.First(&point, ready.PointID).Error; err != nil {
		t.Fatal(err)
	}
	historical, err := decodeRecoveryPointPayload(point.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sources := []*prometheusRecoveryPayload{checkpoint.Evidence, historical}
	beforeSources := stageRealInventory(t, ctx, r, []string{sources[0].Startup.Path, sources[0].Binary.Directory, sources[1].Startup.Path, sources[1].Binary.Directory})
	assertStable := func(row *models.PrometheusRestoreTransaction) {
		t.Helper()
		if row == nil || row.Phase != "restore_staged" || row.ActiveHostID == nil || *row.ActiveHostID != row.HostID || row.ActiveComponentID == nil || *row.ActiveComponentID != row.ComponentID || row.FinishedAt != nil || !reflect.DeepEqual(row.Payload, ready.Payload) || row.PayloadSHA256 != ready.PayloadSHA256 || !reflect.DeepEqual(row.Checkpoint, ready.Checkpoint) || row.CheckpointSHA256 != ready.CheckpointSHA256 {
			t.Fatal("stage changed immutable admission/checkpoint or active journal fence")
		}
		if _, err := compareRecoveryPointBaseline(ctx, r, c, checkpoint.Evidence, true); err != nil {
			t.Fatal("stage original stopped baseline", err)
		}
		if business != transactionBusinessState(t, s.db) {
			t.Fatal("stage changed business/history rows")
		}
		if beforeSources != stageRealInventory(t, ctx, r, []string{sources[0].Startup.Path, sources[0].Binary.Directory, sources[1].Startup.Path, sources[1].Binary.Directory}) {
			t.Fatal("stage changed immutable source bytes or stamps")
		}
		journalAssertClaimsEmpty(t, s.db)
		stageRealAssertRedacted(t, row, c)
	}
	staged, err := s.StagePrometheusPairedRestore(ctx, ready.ID)
	if err != nil {
		t.Fatal("real production stage", err)
	}
	if staged.Revision != 7 {
		t.Fatal("initial stage revision")
	}
	assertStable(staged)
	p := stageRealProof(t, staged)
	stageRealInspectSets(t, ctx, r, p, sources)
	t.Log("real paired restore staging: restore_staged")

	// Reopen the actual main SQLite file; never create a second synthetic DB.
	var databases []struct {
		Name string
		File string
	}
	if err := s.db.Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		t.Fatal(err)
	}
	dbPath := ""
	for _, db := range databases {
		if db.Name == "main" {
			dbPath = db.File
		}
	}
	if !filepath.IsAbs(dbPath) {
		t.Fatal("stage fixture requires actual file-backed SQLite main")
	}
	reopened, err := platformsqlite.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := reopened.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	fresh := NewBackupServiceWithRoot(reopened, s.backupRoot)
	base, err := fresh.runnerFor(c)
	if err != nil {
		t.Fatal(err)
	}
	transport, release, err := acquireUnitSnapshotRunner(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	observer := &checkpointRealObserver{runner: transport}
	fresh.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return observer, nil }
	beforeRow := checkpointJournal(t, reopened, staged.ID)
	beforeTree := stageRealInventory(t, ctx, r, []string{s.backupRoot})
	for _, op := range []func(context.Context, uint) (*models.PrometheusRestoreTransaction, error){fresh.VerifyPrometheusPairedRestoreStage, fresh.StagePrometheusPairedRestore} {
		got, err := op(ctx, staged.ID)
		if err != nil || got == nil || !sameRestoreStageRow(got, &beforeRow) || !got.CreatedAt.Equal(beforeRow.CreatedAt) || !got.UpdatedAt.Equal(beforeRow.UpdatedAt) {
			t.Fatal("fresh stage read-only repeat", err)
		}
	}
	afterRow := checkpointJournal(t, reopened, staged.ID)
	if !reflect.DeepEqual(beforeRow, afterRow) || beforeTree != stageRealInventory(t, ctx, r, []string{s.backupRoot}) {
		t.Fatal("ready observation changed row/evidence/stamps")
	}
	stageRealAssertReadOnly(t, observer.calls)
	assertStable(&afterRow)

	canonical, ownership, err := platformsqlite.AcquireDatabaseProcessLock(dbPath)
	if err != nil {
		t.Fatal("actual database process ownership", err)
	}
	defer ownership.Close()
	if canonical != dbPath {
		t.Fatal("SQLite main filename differs from canonical ownership")
	}
	if err := platformsqlite.ValidateRestoreStageRecoveryState(canonical); err != nil {
		t.Fatal(err)
	}
	oldTree := stageRealInventory(t, ctx, r, []string{filepath.Join(p.Fence.Root, "g-"+p.Fence.Generation)})
	fault := &stageRealCopyObserver{runner: transport, interrupt: true}
	fresh.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return fault, nil }
	if got, err := fresh.RecoverPrometheusPairedRestoreStageForStartup(ctx, staged.ID, ownership); err == nil || got != nil || fault.completed != 1 {
		t.Fatal("real completed-copy interruption boundary not reached")
	}
	partial := checkpointJournal(t, reopened, staged.ID)
	partialProof := stageRealProof(t, &partial)
	if partial.Phase != "restore_stage_copying" || partial.Revision != staged.Revision+2 || len(partialProof.Generations) != 2 {
		t.Fatal("copy interruption lost durable partial phase")
	}
	stageRealAssertTakeover(t, p, partialProof)
	stageRealInspectSets(t, ctx, r, partialProof, sources)
	if _, err := compareRecoveryPointBaseline(ctx, r, c, checkpoint.Evidence, true); err != nil {
		t.Fatal("interrupted stage changed original stopped baseline", err)
	}
	journalAssertClaimsEmpty(t, reopened)
	partialTree := stageRealInventory(t, ctx, r, []string{filepath.Join(partialProof.Fence.Root, "g-"+partialProof.Fence.Generation)})
	checkedClaims := false
	fault.interrupt = false
	fault.beforeCopy = func() {
		row := checkpointJournal(t, reopened, staged.ID)
		proof := stageRealProof(t, &row)
		if row.Phase != "restore_stage_copying" || proof.Claims == partialProof.Claims {
			t.Fatal("takeover copy lacks fresh durable claims")
		}
		// A delayed old exact-token release cannot remove either new owner.
		for _, result := range []*gorm.DB{
			reopened.Where("host_id = ? AND token = ?", row.HostID, partialProof.Claims.Host).Delete(&models.HostOperationClaim{}),
			reopened.Where("component_id = ? AND token = ?", row.ComponentID, partialProof.Claims.Component).Delete(&models.ComponentOperationClaim{}),
		} {
			if result.Error != nil || result.RowsAffected != 0 {
				t.Fatal("old claim release touched new owner")
			}
		}
		var host models.HostOperationClaim
		var component models.ComponentOperationClaim
		if err := reopened.First(&host, "host_id = ?", row.HostID).Error; err != nil || host.Token != proof.Claims.Host {
			t.Fatal("new exact host claim missing")
		}
		if err := reopened.First(&component, "component_id = ?", row.ComponentID).Error; err != nil || component.Token != proof.Claims.Component {
			t.Fatal("new exact component claim missing")
		}
		checkedClaims = true
	}
	recovered, err := fresh.RecoverPrometheusPairedRestoreStageForStartup(ctx, staged.ID, ownership)
	if err != nil || recovered == nil || !checkedClaims || fault.completed != 2 {
		t.Fatal("real explicit stage takeover", err)
	}
	finalProof := stageRealProof(t, recovered)
	if recovered.Revision != partial.Revision+3 || len(finalProof.Generations) != 3 {
		t.Fatal("takeover generation/revision")
	}
	stageRealAssertTakeover(t, partialProof, finalProof)
	assertStable(recovered)
	stageRealInspectSets(t, ctx, r, finalProof, sources)
	if oldTree != stageRealInventory(t, ctx, r, []string{filepath.Join(p.Fence.Root, "g-"+p.Fence.Generation)}) || partialTree != stageRealInventory(t, ctx, r, []string{filepath.Join(partialProof.Fence.Root, "g-"+partialProof.Fence.Generation)}) {
		t.Fatal("takeover modified retained generations")
	}
	t.Log("real paired restore staging takeover: new generation verified")
	for _, stale := range []*prometheusRestoreStagePayload{p, partialProof} {
		path := filepath.Join(stale.Fence.Root, "g-"+stale.Fence.Generation, "stale-write")
		if err := runRestoreStageFenced(ctx, r, stale.Fence, "printf stale > "+utils.QuoteShell(path)); err == nil {
			t.Fatal("stale generation writer accepted")
		}
		if _, err := snapshotRun(ctx, r, "test ! -e "+utils.QuoteShell(path)+" && test ! -L "+utils.QuoteShell(path)); err != nil {
			t.Fatal("stale writer changed old artifacts")
		}
	}
	t.Log("real paired restore staging: stale writer rejected")
	observer.calls = nil
	fresh.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return observer, nil }
	if got, err := fresh.VerifyPrometheusPairedRestoreStage(ctx, recovered.ID); err != nil || got == nil || !sameRestoreStageRow(got, recovered) {
		t.Fatal("fresh post-takeover verification", err)
	}
	stageRealAssertReadOnly(t, observer.calls)
	for i, source := range sources {
		role := []string{"current", "historical"}[i]
		copy := derivedRestoreStageSet(filepath.Join(finalProof.Fence.Root, "g-"+finalProof.Fence.Generation, role), source)
		stageRealArchiveProbe(t, ctx, s, r, c, checkpoint.Evidence, copy, root, unit, role, sampleTime, sample)
	}
	assertStable(recovered)
	stageRealInspectSets(t, ctx, r, finalProof, sources)
	t.Log("real paired restore staging: current and historical archive probes verified")
}

func stageRealProof(t *testing.T, row *models.PrometheusRestoreTransaction) *prometheusRestoreStagePayload {
	t.Helper()
	p, err := decodeRestoreStage(row)
	if err != nil || p == nil {
		t.Fatal("invalid real stage evidence", err)
	}
	return p
}

func stageRealAssertTakeover(t *testing.T, old, next *prometheusRestoreStagePayload) {
	t.Helper()
	if old.Fence.Root != next.Fence.Root || old.Fence.RootIdentity != next.Fence.RootIdentity || old.Fence.LockIdentity != next.Fence.LockIdentity || old.Fence.Generation == next.Fence.Generation || old.Claims.Host == next.Claims.Host || old.Claims.Component == next.Claims.Component || !reflect.DeepEqual(old.Generations, next.Generations[:len(next.Generations)-1]) {
		t.Fatal("takeover failed permanent identity/fresh generation binding")
	}
}

func stageRealAssertReadOnly(t *testing.T, calls []string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("stage observation did not inspect host")
	}
	var other []string
	for _, command := range calls {
		if strings.Contains(command, "# promlifecycle:restore-stage-fence") {
			marker := strings.Trim(utils.QuoteShell("operation='verify'\nbootstrap=0\n"), "'")
			if !strings.Contains(command, marker) || strings.Contains(command, "# promlifecycle:restore-stage-copy") {
				t.Fatal("ready observation used writing fence")
			}
		} else {
			other = append(other, command)
		}
	}
	checkpointAssertReadOnly(t, other)
}

func stageRealAssertRedacted(t *testing.T, row *models.PrometheusRestoreTransaction, c *models.Component) {
	t.Helper()
	p := stageRealProof(t, row)
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{row.Token, string(row.Payload), string(row.Checkpoint), string(row.Stage), p.Fence.Root, p.Fence.Generation, p.Claims.Host, p.Claims.Component, c.ImportedArgs, c.Host.Password, "private-acceptance-sentinel"} {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Fatal("public stage row exposed private evidence")
		}
	}
}

// Independent filesystem inventory includes inode, metadata stamps and literal
// hashes. Atime is deliberately excluded because verification reads the files.
func stageRealInventory(t *testing.T, ctx context.Context, r hostCommandRunner, roots []string) string {
	t.Helper()
	raw, _ := json.Marshal(roots)
	script := `import os,stat,hashlib,json,sys
result={}
def visit(p):
 s=os.lstat(p)
 assert not stat.S_ISLNK(s.st_mode)
 if stat.S_ISDIR(s.st_mode):
  for n in sorted(os.listdir(p)): visit(os.path.join(p,n))
 else: assert stat.S_ISREG(s.st_mode) and s.st_nlink==1
 result[p]=[s.st_dev,s.st_ino,s.st_mode,s.st_uid,s.st_gid,s.st_nlink,s.st_size,s.st_mtime_ns,s.st_ctime_ns,hashlib.sha256(open(p,'rb').read()).hexdigest() if stat.S_ISREG(s.st_mode) else '']
for p in json.loads(sys.argv[1]): visit(p)
print(json.dumps(result,sort_keys=True,separators=(',',':')))
`
	out, err := snapshotRun(ctx, r, "python3 -c "+utils.QuoteShell(script)+" "+utils.QuoteShell(string(raw)))
	if err != nil || out == "" {
		t.Fatal("independent stage artifact inventory failed")
	}
	return out
}

func stageRealInspectSets(t *testing.T, ctx context.Context, r hostCommandRunner, p *prometheusRestoreStagePayload, sources []*prometheusRecoveryPayload) {
	t.Helper()
	gen := filepath.Join(p.Fence.Root, "g-"+p.Fence.Generation)
	type set struct {
		Source, Copy *prometheusRecoveryPayload
		Current      bool
	}
	sets := []set{}
	for i, source := range sources {
		sets = append(sets, set{source, derivedRestoreStageSet(filepath.Join(gen, []string{"current", "historical"}[i]), source), i == 0})
	}
	input, _ := json.Marshal(sets)
	fence, _ := json.Marshal(p)
	// Read manifests and archive contents independently of production verifiers.
	script := `import os,sys,json,stat,hashlib,tarfile
def directory(p):
 s=os.lstat(p); assert stat.S_ISDIR(s.st_mode) and stat.S_IMODE(s.st_mode)==0o700 and s.st_uid==s.st_gid==0
def file(p,digest=None,size=None):
 s=os.lstat(p); assert stat.S_ISREG(s.st_mode) and stat.S_IMODE(s.st_mode)==0o600 and s.st_uid==s.st_gid==0 and s.st_nlink==1
 raw=open(p,'rb').read(); assert digest is None or hashlib.sha256(raw).hexdigest()==digest; assert size is None or len(raw)==size
 return raw
def compare(a,b):
 sa=os.lstat(a); sb=os.lstat(b); assert (sa.st_dev,sa.st_ino)!=(sb.st_dev,sb.st_ino)
 if stat.S_ISDIR(sa.st_mode):
  directory(b); assert sorted(os.listdir(a))==sorted(os.listdir(b))
  for n in os.listdir(a): compare(os.path.join(a,n),os.path.join(b,n))
 else: assert file(a)==file(b)
sets=json.loads(sys.argv[1]); gen=os.path.dirname(os.path.dirname(sets[0]['Copy']['Startup']['path']))
proof=json.loads(sys.argv[2]); fence=proof['Fence']; directory(fence['Root'])
for path,identity in [(fence['Root'],fence['RootIdentity']),(fence['Root']+'/lock',fence['LockIdentity'])]:
 s=os.lstat(path); assert str(s.st_dev)+':'+str(s.st_ino)==identity
file(fence['Root']+'/lock')
assert file(fence['Root']+'/generation')==(fence['Generation']+'\n').encode()
assert sorted(os.listdir(fence['Root']))==sorted(['lock','generation']+['g-'+g for g in proof['Generations']])
directory(gen); assert sorted(os.listdir(gen))==['current','historical']
for item in sets:
 a,b=item['Source'],item['Copy']; root=os.path.dirname(b['Startup']['path']); directory(root)
 assert sorted(os.listdir(root))==sorted([os.path.basename(b['Startup']['path']),os.path.basename(b['Binary']['Directory'])])
 for old,new in [(a['Startup']['path'],b['Startup']['path']),(a['Binary']['Directory'],b['Binary']['Directory'])]: compare(old,new)
 startup=b['Startup']['path']; manifest=json.loads(file(startup+'/manifest.json',a['Startup']['sha256']))
 names=['manifest.json']
 for f in manifest['Files']:
  if f['Present']: names.append(f['Payload']); file(startup+'/'+f['Payload'],f['SHA256'],f['Size'])
 assert sorted(os.listdir(startup))==sorted(names)
 binary=b['Binary']; file(binary['Directory']+'/prometheus',a['Binary']['File']['SHA256'],a['Binary']['File']['Size'])
 archive=b['Archive']; file(archive['Path'],a['Archive']['SHA256'],a['Archive']['Size'])
 assert sorted(os.listdir(binary['Directory']))==sorted(['prometheus',os.path.basename(archive['Path'])])
 with tarfile.open(archive['Path'],'r:gz') as tar:
  matches=[m for m in tar.getmembers() if m.name.lstrip('./')=='data/data/checkpoint-current-marker']
  if item['Current']: assert len(matches)==1 and tar.extractfile(matches[0]).read()==b'current-stopped-checkpoint'
  else: assert matches==[]
print('independent sets verified')
`
	out, err := snapshotRun(ctx, r, "python3 -c "+utils.QuoteShell(script)+" "+utils.QuoteShell(string(input))+" "+utils.QuoteShell(string(fence)))
	if err != nil || strings.TrimSpace(out) != "independent sets verified" {
		t.Fatal("independent current/historical clone verification failed")
	}
}

func stageRealArchiveProbe(t *testing.T, ctx context.Context, s *BackupService, r hostCommandRunner, c *models.Component, baseline, copied *prometheusRecoveryPayload, root, unit, role, sampleTime, sample string) {
	t.Helper()
	if !strings.HasPrefix(root, "/var/lib/plc-paired-real-") || filepath.Clean(root) != root || filepath.Base(root)+".service" != unit || (role != "current" && role != "historical") {
		t.Fatal("invalid exclusively owned stage probe scope")
	}
	probe := root + "/stage-" + role + "-probe"
	probeUnit := strings.TrimSuffix(unit, ".service") + "-stage-" + role + "-probe.service"
	probePath := "/etc/systemd/system/" + probeUnit
	out, err := snapshotRun(ctx, r, "python3 -c "+utils.QuoteShell("import socket; s=socket.socket(); s.bind(('127.0.0.1',0)); print(s.getsockname()[1]); s.close()"))
	if err != nil {
		t.Fatal("stage probe port")
	}
	port, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || port <= 0 || port > 65535 {
		t.Fatal("invalid stage probe port")
	}
	content := fmt.Sprintf("[Service]\nType=simple\nExecStart=%s/prometheus --config.file=%s/config/prometheus.yml --storage.tsdb.path=%s/data/data --web.listen-address=127.0.0.1:%d\n", probe, probe, probe, port)
	cleaned := false
	cleanup := func() error {
		clean, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		// Construct a fresh service/transport, independent of observation or fault wrappers.
		fresh := NewBackupServiceWithRoot(s.db, s.backupRoot)
		base, err := fresh.runnerFor(c)
		if err != nil {
			return err
		}
		runner, release, err := acquireUnitSnapshotRunner(clean, base)
		if err != nil {
			return err
		}
		defer release()
		_, err = snapshotRun(clean, runner, stageRealProbeCleanupCommand(probePath, probeUnit, pointDigest([]byte(content))))
		return err
	}
	t.Cleanup(func() {
		if !cleaned {
			if err := cleanup(); err != nil {
				t.Error("owned stage probe cleanup failed", err)
			}
		}
	})
	setup := "set -eu\numask 077\ntest ! -e " + utils.QuoteShell(probePath) + " && test ! -L " + utils.QuoteShell(probePath) + "\ntest ! -e " + utils.QuoteShell(probePath+".d") + " && test ! -L " + utils.QuoteShell(probePath+".d") + "\nmkdir -m 0700 -- " + utils.QuoteShell(probe) +
		"\ntar -xzf " + utils.QuoteShell(copied.Archive.Path) + " -C " + utils.QuoteShell(probe) + "\ncp -- " + utils.QuoteShell(copied.Binary.Directory+"/prometheus") + " " + utils.QuoteShell(probe+"/prometheus") + "\nchmod 0755 -- " + utils.QuoteShell(probe+"/prometheus") +
		"\nset -C\nprintf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d > " + utils.QuoteShell(probePath) + "\nchmod 0644 -- " + utils.QuoteShell(probePath) + "\nsystemctl daemon-reload\nsystemctl start " + utils.QuoteShell(probeUnit)
	if _, err := snapshotRun(ctx, r, setup); err != nil {
		t.Fatal("start owned stage archive probe", err)
	}
	if got := pointRealQueryWait(t, ctx, r, port, `up{job="paired-self"}`, sampleTime); got != sample {
		t.Fatal("cloned archive lost fixed historical TSDB sample")
	}
	if err := stageRealProbeBuildInfo(ctx, r, port, "2.55.0"); err != nil {
		t.Fatal("cloned binary probe build identity", err)
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, baseline, true); err != nil {
		t.Fatal("probe changed original stopped baseline", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal("owned stage probe cleanup failed", err)
	}
	cleaned = true
	if _, err := compareRecoveryPointBaseline(ctx, r, c, baseline, true); err != nil {
		t.Fatal("probe cleanup changed original stopped baseline", err)
	}
	t.Log("owned stage probe cleanup verified")
}

// The status endpoint identifies the serving process. PromQL build samples
// belong to the restored TSDB and may describe the stopped original process.
func stageRealProbeBuildInfo(ctx context.Context, r hostCommandRunner, port int, expectedVersion string) error {
	address := fmt.Sprintf("http://127.0.0.1:%d/api/v1/status/buildinfo", port)
	out, err := snapshotRun(ctx, r, "curl -fsS --max-time 3 -- "+utils.QuoteShell(address))
	if err != nil {
		return err
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(out), &response) != nil || response.Status != "success" || response.Data.Version != expectedVersion {
		return errors.New("running probe build identity mismatch")
	}
	return nil
}

// Check both the exact exclusive unit file and the freshly loaded systemd
// fragment before stopping. An absent or foreign unit cannot grant ownership.
func stageRealProbeCleanupCommand(probePath, probeUnit, digest string) string {
	return "set -eu\npath=" + utils.QuoteShell(probePath) + "\nunit=" + utils.QuoteShell(probeUnit) + "\nexpected=" + utils.QuoteShell(digest) + `
if test -e "$path" || test -L "$path"; then
 test "$(readlink -m -- "$path")" = "$path"
 test -f "$path" && test ! -L "$path"
 test "$(stat -c '%u:%g:%h' -- "$path")" = '0:0:1'
 case "$(stat -c '%a' -- "$path")" in 600|644) ;; *) exit 1 ;; esac
 test "$(sha256sum -- "$path" | cut -d ' ' -f 1)" = "$expected"
 test ! -e "$path.d" && test ! -L "$path.d"
 systemctl daemon-reload
 test "$(systemctl show --property=FragmentPath --value "$unit")" = "$path"
 test -z "$(systemctl show --property=DropInPaths --value "$unit")"
 systemctl stop "$unit"
 test "$(systemctl show --property=MainPID --value "$unit")" = 0
 rm -- "$path"
 systemctl daemon-reload
fi
test ! -e "$path" && test ! -L "$path"
test "$(systemctl show --property=LoadState --value "$unit")" = not-found
`
}
