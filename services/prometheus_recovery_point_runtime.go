package services

import (
	"context"
	"encoding/base64"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var recoveryRootIdentity = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

func recoveryPointStartTime(stat, pid string) (string, error) {
	if len(stat) > 4096 || !strings.HasPrefix(stat, pid+" (") {
		return "", pointReject("invalid main process evidence")
	}
	end := strings.LastIndex(stat, ") ")
	if end < 0 {
		return "", pointReject("invalid main process stat")
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) < 20 {
		return "", pointReject("incomplete main process stat")
	}
	n, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || n == 0 {
		return "", pointReject("invalid main process start time")
	}
	return fields[19], nil
}
func readRecoveryPointRuntime(ctx context.Context, r hostCommandRunner, c *models.Component, state unitSnapshotState, binary unitSnapshotFile) (recoveryPointRuntime, error) {
	var proof recoveryPointRuntime
	if state["ActiveState"] != "active" || state["SubState"] != "running" || state["MainPID"] == "0" {
		return proof, pointReject("paired capture requires an active running service")
	}
	pid, err := strconv.ParseUint(state["MainPID"], 10, 32)
	if err != nil || pid == 0 {
		return proof, pointReject("invalid main process PID")
	}
	if !validUnitSnapshotPath(c.BinaryPath) || c.BinaryPath != binary.Path || !unitSnapshotHash.MatchString(binary.SHA256) {
		return proof, pointReject("recorded canonical executable required")
	}
	proc := "/proc/" + strconv.FormatUint(pid, 10)
	command := "# promlifecycle:recovery-point-runtime\nset -eu\nproc=" + utils.QuoteShell(proc) + "\nbin=" + utils.QuoteShell(c.BinaryPath) + "\nconfig=" + utils.QuoteShell(c.ConfigPath) + "\ndata=" + utils.QuoteShell(c.DataPath) + "\ndigest=" + utils.QuoteShell(binary.SHA256) + `
test "$(readlink -e -- "$proc/exe")" = "$bin"
test "$(readlink -e -- "$proc/root")" = /
test "$(head -c 536870913 -- "$proc/exe" | sha256sum | cut -d ' ' -f 1)" = "$digest"
for p in "$config" "$data"; do
 test "$(stat -Lc '%d:%i' -- "$p")" = "$(stat -Lc '%d:%i' -- "$proc/root$p")"
done
awk -v config="$config" -v data="$data" 'index($5,config "/")==1 || index($5,data "/")==1 {bad=1} END {exit bad}' "$proc/mountinfo" /proc/self/mountinfo
head -c 4097 -- "$proc/stat"
head -c 65537 -- "$proc/cmdline" | base64 -w0
printf '\n'
stat -Lc '%d:%i' -- "$config" "$data"
head -c 4097 -- "$proc/stat"
for p in "$config" "$data"; do
 test "$(stat -Lc '%d:%i' -- "$p")" = "$(stat -Lc '%d:%i' -- "$proc/root$p")"
done
`
	out, err := snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell(command))
	if err != nil {
		return proof, err
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 5 {
		return proof, pointReject("incomplete runtime evidence")
	}
	start, err := recoveryPointStartTime(lines[0], state["MainPID"])
	if err != nil {
		return proof, err
	}
	finish, err := recoveryPointStartTime(lines[4], state["MainPID"])
	if err != nil || start != finish {
		return proof, pointReject("main process changed during inspection")
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) == 0 || len(raw) > 64<<10 || raw[len(raw)-1] != 0 {
		return proof, pointReject("invalid or oversized observed argv")
	}
	args := strings.Split(string(raw[:len(raw)-1]), "\x00")
	if len(args) > 1024 || args[0] == "" {
		return proof, pointReject("unsupported observed argv")
	}
	config, data, err := deployer.PrometheusArgvPaths(args)
	if err != nil || config != c.ConfigPath || data != c.DataPath {
		return proof, pointReject("actual process paths do not match archive roots")
	}
	if !recoveryRootIdentity.MatchString(lines[2]) || !recoveryRootIdentity.MatchString(lines[3]) {
		return proof, pointReject("invalid runtime root identity")
	}
	command = "# promlifecycle:recovery-point-version\nset -eu\n" + "timeout --signal=KILL 10s " + utils.QuoteShell(c.BinaryPath) + " --version 2>&1 | head -c 4097"
	version, err := snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell(command))
	if err != nil {
		return proof, err
	}
	fields := strings.Fields(strings.SplitN(version, "\n", 2)[0])
	if len(version) > 4096 || len(fields) < 3 || fields[0] != "prometheus," || fields[1] != "version" || !samePrometheusVersion(fields[2], c.Version) {
		return proof, pointReject("actual executable version does not match historical component")
	}
	if err := verifyRecoveryPointLaunch(ctx, r, c, pointDigest(raw)); err != nil {
		return proof, err
	}
	return recoveryPointRuntime{PID: state["MainPID"], StartTime: start, ArgvSHA256: pointDigest(raw), ConfigIdentity: lines[2], DataIdentity: lines[3]}, nil
}
func requireRecoveryPointProcessGone(ctx context.Context, r hostCommandRunner, proof recoveryPointRuntime) error {
	pid, err := strconv.ParseUint(proof.PID, 10, 32)
	if err != nil || pid == 0 {
		return pointReject("invalid original process identity")
	}
	path := "/proc/" + strconv.FormatUint(pid, 10) + "/stat"
	out, err := snapshotRun(ctx, r, "# promlifecycle:recovery-point-gone\nif [ -e "+utils.QuoteShell(path)+" ]; then head -c 4097 -- "+utils.QuoteShell(path)+"; fi")
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	start, err := recoveryPointStartTime(strings.TrimSuffix(out, "\n"), proof.PID)
	if err != nil {
		return err
	}
	if start == proof.StartTime {
		return pointReject("original process is still present")
	}
	return nil
}
func compareRecoveryPointBaseline(ctx context.Context, r hostCommandRunner, c *models.Component, p *prometheusRecoveryPayload, stopped bool) (unitSnapshotState, error) {
	saved, err := verifyUnitSnapshot(ctx, r, filepath.Dir(p.Startup.Path), &p.Startup, p.Binding, false)
	if err != nil {
		return nil, err
	}
	state, files, err := snapshotInventory(ctx, r, p.Binding.Unit)
	if err != nil {
		return nil, err
	}
	expected := append([]unitSnapshotFile(nil), saved.Files...)
	for i := range expected {
		expected[i].Payload = ""
	}
	if !reflect.DeepEqual(expected, files) {
		return nil, pointReject("startup files changed during paired capture")
	}
	for key, value := range saved.State {
		if key == "MainPID" || key == "ActiveState" || key == "SubState" {
			continue
		}
		if state[key] != value {
			return nil, pointReject("systemd startup state drifted")
		}
	}
	binary, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: c.BinaryPath, Role: "binary"}, recoveryPointMaxBinary)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(binary, p.Binary.File) {
		return nil, pointReject("historical executable changed during capture")
	}
	if stopped {
		if err := verifyRecoveryPointLaunch(ctx, r, c, p.Runtime.ArgvSHA256); err != nil {
			return nil, err
		}
		mountCheck := "awk -v config=" + utils.QuoteShell(c.ConfigPath) + " -v data=" + utils.QuoteShell(c.DataPath) + " " + utils.QuoteShell(`index($5,config "/")==1 || index($5,data "/")==1 {bad=1} END {exit bad}`) + " /proc/self/mountinfo"
		if _, err := snapshotRun(ctx, r, mountCheck); err != nil {
			return nil, err
		}
		if state["ActiveState"] != "inactive" || state["SubState"] != "dead" || state["MainPID"] != "0" || state["ControlPID"] != "0" {
			return nil, pointReject("service stop was not confirmed")
		}
		if err := requireRecoveryPointProcessGone(ctx, r, p.Runtime); err != nil {
			return nil, err
		}
		out, err := snapshotRun(ctx, r, "stat -Lc '%d:%i' -- "+utils.QuoteShell(c.ConfigPath)+" "+utils.QuoteShell(c.DataPath))
		if err != nil {
			return nil, err
		}
		if out != p.Runtime.ConfigIdentity+"\n"+p.Runtime.DataIdentity+"\n" {
			return nil, pointReject("archive root identity changed while stopping")
		}
	} else {
		actual, err := readRecoveryPointRuntime(ctx, r, c, state, binary)
		if err != nil {
			return nil, err
		}
		if actual.ArgvSHA256 != p.Runtime.ArgvSHA256 || actual.ConfigIdentity != p.Runtime.ConfigIdentity || actual.DataIdentity != p.Runtime.DataIdentity {
			return nil, pointReject("resumed runtime no longer matches historical baseline")
		}
	}
	return state, nil
}
func captureRecoveryPointBinary(ctx context.Context, r hostCommandRunner, dir string, source unitSnapshotFile) error {
	if source.Size <= 0 || source.Mode&0100 == 0 || !validUnitSnapshotPath(source.Path) {
		return pointReject("protected executable required")
	}
	target := filepath.Join(dir, "prometheus")
	command := "set -eu\numask 077\nsrc=" + utils.QuoteShell(source.Path) + "\ndst=" + utils.QuoteShell(target) + `
test "$(readlink -m -- "$src")" = "$src"
test -f "$src" && test ! -L "$src"
test "$(stat -c '%u:%h' -- "$src")" = '0:1'
test -z "$(find "$src" -maxdepth 0 \( -perm /022 -o -perm /7000 \) -print)"
test ! -e "$dst" && test ! -L "$dst"
set -C
head -c 536870913 -- "$src" > "$dst"
test "$(stat -c %s -- "$dst")" -le 536870912
chown 0:0 -- "$dst"
chmod 0600 -- "$dst"
sync -f "$dst"
sync -f "$(dirname -- "$dst")"
`
	if _, err := snapshotRun(ctx, r, command); err != nil {
		return err
	}
	return verifyRecoveryPointBinary(ctx, r, recoveryPointBinary{Directory: dir, File: source})
}
func verifyRecoveryPointBinary(ctx context.Context, r hostCommandRunner, b recoveryPointBinary) error {
	if b.File.Size <= 0 || b.File.Size > recoveryPointMaxBinary || !b.File.Present || b.File.Optional || b.File.Role != "binary" || b.File.Payload != "" || b.File.UID != 0 || b.File.Mode > 0777 || b.File.Mode&0022 != 0 || b.File.Mode&0100 == 0 || !unitSnapshotHash.MatchString(b.File.SHA256) || !validUnitSnapshotPath(b.File.Path) || b.File.Stamp == "" {
		return pointReject("invalid historical executable metadata")
	}
	if err := snapshotPrivateDirectory(ctx, r, b.Directory); err != nil {
		return err
	}
	saved, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: filepath.Join(b.Directory, "prometheus")}, recoveryPointMaxBinary)
	if err != nil {
		return err
	}
	if saved.Mode != 0600 || saved.GID != 0 || saved.Size != b.File.Size || saved.SHA256 != b.File.SHA256 {
		return pointReject("historical executable artifact failed integrity")
	}
	return nil
}
