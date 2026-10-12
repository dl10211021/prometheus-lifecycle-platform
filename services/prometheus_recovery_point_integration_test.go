package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in owns every unit, executable, config and TSDB it touches.
// The same fixture exercises local commands and the production SSH runner.
func TestRealPrometheusPairedRecoveryPoint(t *testing.T) {
	if os.Getenv("PROMLIFECYCLE_REAL_PAIRED_POINT") != "1" {
		t.Skip("requires explicit owned Prometheus fixture opt-in")
	}
	binary := os.Getenv("PROMLIFECYCLE_PAIRED_BINARY")
	if !validUnitSnapshotPath(binary) {
		t.Fatal("verified local fixture binary required")
	}
	f := newPointFixture(t)
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	root := fmt.Sprintf("/var/lib/plc-paired-real-%x", token[:])
	unit := fmt.Sprintf("plc-paired-real-%x.service", token[:])
	unitPath := "/etc/systemd/system/" + unit
	drop := unitPath + ".d"
	current := *f.c
	if host := os.Getenv("PROMLIFECYCLE_PAIRED_SSH_HOST"); host != "" {
		password := os.Getenv("PROMLIFECYCLE_PAIRED_SSH_PASSWORD")
		if password == "" {
			t.Fatal("SSH fixture password required")
		}
		current.Host.IP = host
		current.Host.Port = 22
		current.Host.Username = "root"
		current.Host.Password = password
		current.Host.ConnectionType = "ssh"
		if err := f.db.Model(&models.Host{}).Where("id = ?", current.HostID).Updates(map[string]any{"ip": host, "port": 22, "username": "root", "password": password, "connection_type": "ssh"}).Error; err != nil {
			t.Fatal("save temporary fixture host")
		}
	}
	s := NewBackupServiceWithRoot(f.db, root+"/backups")
	base, err := s.runnerFor(&current)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	r, release, err := acquireUnitSnapshotRunner(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	command := "set -eu\numask 077\ntest ! -e " + utils.QuoteShell(unitPath) + " && test ! -L " + utils.QuoteShell(unitPath) + "\ntest ! -e " + utils.QuoteShell(drop) + " && test ! -L " + utils.QuoteShell(drop) + "\nmkdir -m 0700 -- " + utils.QuoteShell(root)
	if _, err := snapshotRun(ctx, r, command); err != nil {
		t.Fatal("exclusive fixture ownership", err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 45*time.Second)
		defer done()
		cr, err := s.runnerFor(&current)
		if err != nil {
			t.Error(err)
			return
		}
		cr, closeRunner, err := acquireUnitSnapshotRunner(cleanup, cr)
		if err != nil {
			t.Error(err)
			return
		}
		defer closeRunner()
		command := "set -eu\nunit=" + utils.QuoteShell(unit) + "\npath=" + utils.QuoteShell(unitPath) + "\ndrop=" + utils.QuoteShell(drop) + "\nroot=" + utils.QuoteShell(root) + `
if test -e "$path"; then
 systemctl stop "$unit"
 test "$(systemctl show --property=MainPID --value "$unit")" = 0
 rm -- "$path"
fi
if test -d "$drop"; then rm -f -- "$drop/limits.conf"; rmdir -- "$drop"; fi
systemctl daemon-reload
test "$(readlink -m -- "$root")" = "$root"
rm -rf -- "$root"
test ! -e "$root" && test ! -e "$path" && test ! -e "$drop"
test "$(systemctl show --property=LoadState --value "$unit")" = not-found
`
		if _, err := snapshotRun(cleanup, cr, command); err != nil {
			t.Error("owned fixture cleanup", err)
		} else {
			t.Log("owned unit, drop-in, binary, TSDB and archive cleanup verified")
		}
	})
	portOut, err := snapshotRun(ctx, r, `python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'`)
	if err != nil {
		t.Fatal("fixture free port", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(portOut))
	if err != nil || port <= 0 {
		t.Fatal("invalid fixture port")
	}
	current.ServiceName = unit
	current.BinaryPath = root + "/prometheus"
	current.ConfigPath = root + "/prometheus.yml"
	current.DataPath = root + "/data"
	current.Port = port
	args := []string{"--config.file=" + current.ConfigPath, "--storage.tsdb.path=" + current.DataPath, fmt.Sprintf("--web.listen-address=127.0.0.1:%d", port)}
	argv, _ := json.Marshal(args)
	current.ImportedArgs = string(argv)
	reader, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	info, err := reader.Stat()
	if err != nil {
		reader.Close()
		t.Fatal(err)
	}
	if remote, ok := r.(unitSnapshotSSHRunner); ok {
		err = remote.client.CopyFileContext(ctx, reader, current.BinaryPath, 0755, info.Size())
	} else {
		var dst *os.File
		dst, err = os.OpenFile(current.BinaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err == nil {
			_, err = io.Copy(dst, reader)
			closeErr := dst.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	reader.Close()
	if err != nil {
		t.Fatal("copy private fixture executable", err)
	}
	unitContent := fmt.Sprintf("[Unit]\nDescription=Owned paired recovery point acceptance\n[Service]\nType=simple\nEnvironmentFile=%s/protected.env\nEnvironmentFile=-%s/optional.env\nExecStart=%s %s\n[Install]\nWantedBy=multi-user.target\n", root, root, current.BinaryPath, strings.Join(args, " "))
	config := fmt.Sprintf("global:\n  scrape_interval: 1s\nscrape_configs:\n  - job_name: paired-self\n    static_configs:\n      - targets: ['127.0.0.1:%d']\n", port)
	setup := "set -eu\numask 077\nmkdir -m 0755 -- " + utils.QuoteShell(drop) + "\nset -C\n"
	for path, content := range map[string]string{unitPath: unitContent, drop + "/limits.conf": "[Service]\nLimitNOFILE=65536\n", root + "/protected.env": "PAIRED_TEST_SECRET=private-acceptance-sentinel\n", current.ConfigPath: config} {
		setup += "printf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d > " + utils.QuoteShell(path) + "\n"
	}
	setup += "chmod 0644 -- " + utils.QuoteShell(unitPath) + " " + utils.QuoteShell(drop+"/limits.conf") + "\nsystemctl daemon-reload\nsystemctl start " + utils.QuoteShell(unit)
	if _, err := snapshotRun(ctx, r, setup); err != nil {
		t.Fatal("start owned real fixture", err)
	}
	if err := f.db.Model(f.c).Updates(map[string]any{"service_name": unit, "binary_path": current.BinaryPath, "config_path": current.ConfigPath, "data_path": current.DataPath, "port": port, "imported_args": current.ImportedArgs}).Error; err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(prometheusBefore{Component: &current, Version: current.Version, Image: current.Image, ImportedArgsSHA256: importedArgsHash(current.ImportedArgs)})
	if err := f.db.Model(f.plan).Update("before", before).Error; err != nil {
		t.Fatal(err)
	}
	sampleTime := pointRealQueryWait(t, ctx, r, port, `timestamp(up{job="paired-self"})`, "")
	historical := pointRealQueryWait(t, ctx, r, port, `up{job="paired-self"}`, sampleTime)
	if historical != "1" {
		t.Fatal("self-scrape was not healthy")
	}
	if got := pointRealQueryWait(t, ctx, r, port, `prometheus_build_info{version="2.55.0"}`, ""); got != "1" {
		t.Fatal("unexpected real fixture version")
	}
	pre, _, err := snapshotInventory(ctx, r, unit)
	if err != nil {
		t.Fatal(err)
	}
	// Re-loaded launch drift and lifecycle hooks must be rejected while the
	// original process continues running, before any stop/restart is issued.
	for _, changed := range []string{
		strings.Replace(unitContent, "ExecStart="+current.BinaryPath, "ExecStart="+root+"/uninstalled", 1),
		strings.Replace(unitContent, "Type=simple\n", "Type=simple\nExecStartPre=/bin/true\n", 1),
		strings.Replace(unitContent, "--storage.tsdb.path="+current.DataPath+" "+args[2], "\"--storage.tsdb.path="+current.DataPath+" "+args[2]+"\"", 1),
		strings.Replace(unitContent, "Type=simple\n", "Type=simple\nRootDirectory=/srv/other-root\n", 1),
		strings.Replace(unitContent, "ExecStart=", "ExecStart=+", 1),
	} {
		reload := "printf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString([]byte(changed))) + " | base64 -d > " + utils.QuoteShell(unitPath) + " && systemctl daemon-reload"
		if _, err := snapshotRun(ctx, r, reload); err != nil {
			t.Fatal(err)
		}
		if point, err := s.CreatePrometheusRecoveryPoint(ctx, f.plan.ID); point != nil || err == nil {
			t.Fatal("unsafe real loaded launch admitted")
		}
		state, _, err := snapshotInventory(ctx, r, unit)
		if err != nil || state["MainPID"] != pre["MainPID"] {
			t.Fatal("rejected real launch changed original process", err)
		}
	}
	reload := "printf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString([]byte(unitContent))) + " | base64 -d > " + utils.QuoteShell(unitPath) + " && systemctl daemon-reload"
	if _, err := snapshotRun(ctx, r, reload); err != nil {
		t.Fatal(err)
	}
	point, err := s.CreatePrometheusRecoveryPoint(ctx, f.plan.ID)
	if err != nil {
		t.Fatal("real paired capture", err)
	}
	after, _, err := snapshotInventory(ctx, r, unit)
	if err != nil {
		t.Fatal(err)
	}
	if point.State != "ready" || pre["MainPID"] == after["MainPID"] || pre["UnitFileState"] != after["UnitFileState"] {
		t.Fatal("real pause/resume or enablement evidence incomplete")
	}
	if got := pointRealQueryWait(t, ctx, r, port, `up{job="paired-self"}`, sampleTime); got != historical {
		t.Fatal("historical TSDB sample changed across capture")
	}
	fresh := NewBackupServiceWithRoot(f.db, s.backupRoot)
	proof, err := fresh.VerifyPrometheusRecoveryPoint(ctx, f.plan.ID)
	if err != nil || proof.StartupFiles != 4 || proof.BinaryBytes != info.Size() || proof.Scope != "paired_historical_artifact_evidence_only" {
		t.Fatalf("real historical evidence: %+v %v", proof, err)
	}
	if os.Getenv("PROMLIFECYCLE_PAIRED_RESTORE_EXECUTION") == "1" {
		pointRealRestoreExecution(t, ctx, fresh, r, &current, f.plan.ID, root, unit, sampleTime, historical)
		return
	}
	if os.Getenv("PROMLIFECYCLE_REAL_PAIRED_RESTORE_PREVIEW") == "1" {
		pointRealRestorePreview(t, ctx, fresh, r, &current, f.plan.ID, root, unit, port, sampleTime, historical)
		if os.Getenv("PROMLIFECYCLE_PAIRED_RESTORE_CHECKPOINT") == "1" {
			// Dedicated checkpoint acceptance retains the stopped transaction.
			// The original source-loss acceptance runs with this opt-in unset.
			return
		}
	}
	// Stop only our unit, remove original executable/config/data, then reopen
	// history through the production transport with changed current metadata.
	if _, err := snapshotRun(ctx, r, "systemctl stop "+utils.QuoteShell(unit)+" && test \"$(systemctl show --property=MainPID --value "+utils.QuoteShell(unit)+")\" = 0"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(f.c).Updates(map[string]any{"version": "3.1.0", "imported_args": "changed-after-capture"}).Error; err != nil {
		t.Fatal(err)
	}
	remove := "set -eu\nrm -- " + utils.QuoteShell(current.BinaryPath) + " " + utils.QuoteShell(current.ConfigPath) + " " + utils.QuoteShell(root+"/protected.env") + "\nrm -rf -- " + utils.QuoteShell(current.DataPath)
	if _, err := snapshotRun(ctx, r, remove); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.VerifyPrometheusRecoveryPoint(ctx, f.plan.ID); err != nil {
		t.Fatal("history depended on deleted live sources", err)
	}
	t.Logf("real paired recovery evidence: historical_sample=%s value=%s startup_files=%d binary_bytes=%d archive_bytes=%d state=%s; original sources removed and history reverified", sampleTime, historical, proof.StartupFiles, proof.BinaryBytes, proof.ArchiveBytes, proof.CaptureState)
}
func pointRealQueryWait(t *testing.T, ctx context.Context, r hostCommandRunner, port int, query, at string) string {
	t.Helper()
	address := fmt.Sprintf("http://127.0.0.1:%d/api/v1/query?query=%s", port, url.QueryEscape(query))
	if at != "" {
		address += "&time=" + url.QueryEscape(at)
	}
	for i := 0; i < 60; i++ {
		out, err := snapshotRun(ctx, r, "curl -fsS --max-time 3 -- "+utils.QuoteShell(address))
		if err == nil {
			var response struct {
				Status string
				Data   struct {
					Result []struct{ Value []json.RawMessage }
				}
			}
			if json.Unmarshal([]byte(out), &response) == nil && response.Status == "success" && len(response.Data.Result) == 1 && len(response.Data.Result[0].Value) == 2 {
				var value string
				if json.Unmarshal(response.Data.Result[0].Value[1], &value) == nil {
					return value
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("real query cancelled")
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("real query evidence unavailable")
	return ""
}
