package services

import (
	"context"
	"errors"
	"fmt"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

func validateExecutionTargets(c *models.Component, backup string, sets [2]*prometheusRecoveryPayload, manifests [2]*unitSnapshotManifest) error {
	paths := []string{backup, c.BinaryPath, c.ConfigPath, c.DataPath}
	for i, set := range sets {
		if set.Binary.File.Path != c.BinaryPath || !equalStringSlices(set.Archive.Sources, []string{c.ConfigPath, c.DataPath}) || set.Binding.Unit != makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)).Unit {
			return pointReject("execution roots differ from trusted component")
		}
		if len(manifests[i].Files) != len(manifests[0].Files) {
			return pointReject("startup path union changed")
		}
		for n, file := range manifests[i].Files {
			other := manifests[0].Files[n]
			if file.Path != other.Path || file.Role != other.Role || file.Optional != other.Optional {
				return pointReject("startup topology differs across execution sets")
			}
			if i == 0 {
				paths = append(paths, file.Path)
			}
		}
	}
	for i, path := range paths {
		if !validUnitSnapshotPath(path) {
			return pointReject("invalid execution target")
		}
		for _, previous := range paths[:i] {
			if recoveryPathsOverlap(path, previous) {
				return pointReject("overlapping execution targets")
			}
		}
	}
	if componentReadinessURL(c) == "" {
		return pointReject("recorded readiness endpoint required")
	}
	return nil
}
func (s *BackupService) executionSharedReferences(ctx context.Context, r hostCommandRunner, c *models.Component, files []unitSnapshotFile) error {
	refs, err := s.pairedRestoreRegisteredReferences(ctx, c)
	if err != nil {
		return err
	}
	refs, err = resolvePairedRestoreUnitReferences(ctx, r, refs)
	if err != nil {
		return err
	}
	units, err := readPairedRestoreUnitReferences(ctx, r)
	if err != nil {
		return err
	}
	paths := []string{s.backupRoot, c.BinaryPath, c.ConfigPath, c.DataPath}
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	if err := rejectPairedRestoreSharedIdentity(makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)).Unit, units, refs); err != nil {
		return err
	}
	if err := rejectPairedRestoreSharedPaths(paths, refs, ""); err != nil {
		return err
	}
	return rejectPairedRestoreSharedPaths(paths, units, makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)).Unit)
}
func replaceRestoreExecutionHost(ctx context.Context, r hostCommandRunner, row *models.PrometheusRestoreTransaction, p *restoreExecutionPayload, set *prometheusRecoveryPayload, manifest *unitSnapshotManifest, manifests [2]*unitSnapshotManifest, c *models.Component) error {
	q := utils.QuoteShell
	// Work and displaced live roots are unique to a committed journal revision.
	// No existing artifact or displaced path is adopted or recursively deleted.
	work := filepath.Join(filepath.Dir(filepath.Dir(set.Startup.Path)), fmt.Sprintf("execution-%d", row.Revision))
	script := "# promlifecycle:restore-execution-replace\nset -euo pipefail\numask 077\n" + executionHostPathChecks +
		"work=" + q(work) + "\nunit=" + q(set.Binding.Unit) + "\n" +
		"test ! -e \"$work\" && test ! -L \"$work\"\nmkdir -m 0700 -- \"$work\"\n" +
		"test \"$(sha256sum -- " + q(set.Archive.Path) + " | cut -d ' ' -f 1)\" = " + q(set.Archive.SHA256) + "\n" +
		"tar --same-owner --same-permissions --numeric-owner -xzf " + q(set.Archive.Path) + " -C \"$work\"\n" +
		"test -z \"$(find \"$work\" ! -type f ! -type d -print -quit)\"\n" +
		"test -z \"$(find \"$work\" -type f -links +1 -print -quit)\"\n" +
		"test -z \"$(find \"$work\" -perm /7000 -print -quit)\"\n"
	type replacement struct {
		target, source string
		file           *unitSnapshotFile
	}
	replacements := []replacement{
		{c.ConfigPath, filepath.Join(work, "config", filepath.Base(c.ConfigPath)), nil},
		{c.DataPath, filepath.Join(work, "data", filepath.Base(c.DataPath)), nil},
		{c.BinaryPath, filepath.Join(set.Binary.Directory, "prometheus"), &set.Binary.File},
	}
	for i := range manifest.Files {
		file := &manifest.Files[i]
		source := ""
		if file.Present {
			source = filepath.Join(set.Startup.Path, file.Payload)
		}
		replacements = append(replacements, replacement{file.Path, source, file})
	}
	for i, item := range replacements {
		holding := filepath.Join(filepath.Dir(item.target), fmt.Sprintf(".plc-restore-%s-%d-%d", row.Token[:24], row.Revision, i))
		script += "check_target " + q(item.target) + "\n"
		script += "test ! -e " + q(holding) + " && test ! -L " + q(holding) + "\nmkdir -m 0700 -- " + q(holding) + "\n"
		if item.source != "" {
			if item.file != nil {
				script += "test \"$(sha256sum -- " + q(item.source) + " | cut -d ' ' -f 1)\" = " + q(item.file.SHA256) + "\n"
			}
			script += "cp -a -- " + q(item.source) + " " + q(filepath.Join(holding, "replacement")) + "\n"
			if item.file != nil {
				script += fmt.Sprintf("chown %d:%d -- %s\nchmod %04o -- %s\n", item.file.UID, item.file.GID, q(filepath.Join(holding, "replacement")), item.file.Mode, q(filepath.Join(holding, "replacement")))
			}
			script += "find " + q(holding) + " -type f -exec sync -f -- {} +\nfind " + q(holding) + " -depth -type d -exec sync -f -- {} +\n"
		}
	}
	script += "systemctl stop \"$unit\"\n" +
		"test \"$(systemctl show --property=ActiveState --value \"$unit\")\" = inactive\n" +
		"test \"$(systemctl show --property=SubState --value \"$unit\")\" = dead\n" +
		"test \"$(systemctl show --property=MainPID --value \"$unit\")\" = 0\n" +
		"test \"$(systemctl show --property=ControlPID --value \"$unit\")\" = 0\n"
	for i, item := range replacements {
		holding := filepath.Join(filepath.Dir(item.target), fmt.Sprintf(".plc-restore-%s-%d-%d", row.Token[:24], row.Revision, i))
		script += "check_target " + q(item.target) + "\n" +
			"if [ -e " + q(item.target) + " ]; then mv -T -- " + q(item.target) + " " + q(filepath.Join(holding, "displaced")) + "; fi\n"
		if item.source != "" {
			script += "mv -T -- " + q(filepath.Join(holding, "replacement")) + " " + q(item.target) + "\n"
		}
		script += "sync -f -- " + q(holding) + "\nsync -f -- " + q(filepath.Dir(item.target)) + "\n"
	}
	return runRestoreStageFenced(ctx, r, p.Fence, script)
}

const executionHostPathChecks = `
check_parent() {
 p="$1"
 test "$(readlink -e -- "$p")" = "$p"
 while :; do
  test -d "$p" && test ! -L "$p"
  test "$(stat -c %u -- "$p")" = 0
  test -z "$(find "$p" -maxdepth 0 \( -perm /022 -o -perm /7000 \) -print)"
  [ "$p" != / ] || break
  p=$(dirname -- "$p")
 done
}
check_target() {
 target="$1"
 test "$(readlink -m -- "$target")" = "$target"
 check_parent "$(dirname -- "$target")"
 test ! -L "$target"
 awk -v target="$target" '$5==target || index($5,target "/")==1 {bad=1} END {exit bad}' /proc/self/mountinfo
 if [ -e "$target" ]; then
  test -f "$target" || test -d "$target"
  test -z "$(find -P "$target" ! -type f ! -type d -print -quit)"
  test -z "$(find -P "$target" -type f -links +1 -print -quit)"
 fi
}
`

func startRestoreExecutionHost(ctx context.Context, r hostCommandRunner, f restoreStageFence, unit string) error {
	return runRestoreStageFenced(ctx, r, f, "# promlifecycle:restore-execution-start\nset -eu\nsystemctl daemon-reload\nsystemctl start "+utils.QuoteShell(unit)+"\n")
}
func sameExecutionFile(actual, expected unitSnapshotFile) bool {
	actual.Stamp = ""
	expected.Stamp = ""
	actual.Payload = ""
	expected.Payload = ""
	return reflect.DeepEqual(actual, expected)
}
func verifyRestoreExecutionRuntime(ctx context.Context, r hostCommandRunner, c *models.Component, set *prometheusRecoveryPayload, manifest *unitSnapshotManifest, fence restoreStageFence) (*recoveryPointRuntime, error) {
	if err := verifyRestoreStageFence(ctx, r, fence); err != nil {
		return nil, err
	}
	state, files, err := snapshotInventory(ctx, r, set.Binding.Unit)
	if err != nil {
		return nil, err
	}
	if len(files) != len(manifest.Files) {
		return nil, pointReject("restored startup topology differs")
	}
	for i, file := range files {
		if !sameExecutionFile(file, manifest.Files[i]) {
			return nil, pointReject("restored startup bytes or metadata differ")
		}
	}
	for key, value := range manifest.State {
		if key == "MainPID" || key == "ActiveState" || key == "SubState" {
			continue
		}
		if state[key] != value {
			return nil, pointReject("restored systemd identity differs")
		}
	}
	binary, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: c.BinaryPath, Role: "binary"}, recoveryPointMaxBinary)
	if err != nil {
		return nil, err
	}
	if !sameExecutionFile(binary, set.Binary.File) {
		return nil, pointReject("restored executable bytes or metadata differ")
	}
	runtime, err := readRecoveryPointRuntime(ctx, r, c, state, binary)
	if err != nil {
		return nil, err
	}
	if runtime.ArgvSHA256 != set.Runtime.ArgvSHA256 {
		return nil, pointReject("restored actual argv differs")
	}
	ready := componentReadinessURL(c)
	if ready == "" {
		return nil, pointReject("readiness endpoint unavailable")
	}
	if err := waitRestoreExecutionReadiness(ctx, r, ready, runtime.PID); err != nil {
		return nil, err
	}
	finalState, finalFiles, err := snapshotInventory(ctx, r, set.Binding.Unit)
	if err != nil {
		return nil, err
	}
	if len(finalFiles) != len(manifest.Files) {
		return nil, pointReject("startup changed during readiness")
	}
	for i, file := range finalFiles {
		if !sameExecutionFile(file, manifest.Files[i]) {
			return nil, pointReject("startup changed during readiness")
		}
	}
	finalBinary, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: c.BinaryPath, Role: "binary"}, recoveryPointMaxBinary)
	if err != nil || !sameExecutionFile(finalBinary, set.Binary.File) {
		return nil, pointReject("binary changed during readiness")
	}
	final, err := readRecoveryPointRuntime(ctx, r, c, finalState, binary)
	if err != nil || final != runtime {
		return nil, pointReject("runtime changed during readiness verification")
	}
	if err := verifyRestoreStageFence(ctx, r, fence); err != nil {
		return nil, err
	}
	return &runtime, nil
}
func verifyExecutionListener(ctx context.Context, r hostCommandRunner, ready, pid string) error {
	parsed, err := url.Parse(ready)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return pointReject("invalid readiness endpoint")
	}
	host := parsed.Hostname()
	if host != "localhost" && net.ParseIP(host) == nil {
		return pointReject("readiness requires recorded numeric or localhost endpoint")
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	address, wildcard, table := "0100007F", "00000000", "tcp"
	allowIPv4DualStack := false
	if host != "localhost" {
		ip := net.ParseIP(host)
		if v4 := ip.To4(); v4 != nil {
			address = fmt.Sprintf("%02X%02X%02X%02X", v4[3], v4[2], v4[1], v4[0])
			allowIPv4DualStack = !ip.IsLoopback()
		} else if ip.Equal(net.IPv6loopback) {
			address, wildcard, table = "00000000000000000000000001000000", "00000000000000000000000000000000", "tcp6"
		} else {
			return pointReject("execution readiness requires IPv4 or IPv6 loopback")
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return pointReject("invalid readiness port")
	}
	localRouteProof := ""
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		localRouteProof = "destination=" + utils.QuoteShell(host) + "\n" + `
route=$(timeout --signal=KILL 3s ip -4 route get "$destination" | head -c 4097)
test "${#route}" -le 4096
printf '%s\n' "$route" | awk -v destination="$destination" 'NR==1 && $1=="local" && $2==destination {found=1} END {exit !found}'
`
	}
	dualStackFallback := "0"
	if allowIPv4DualStack {
		dualStackFallback = "1"
	}
	script := "# promlifecycle:restore-execution-listener\nset -euo pipefail\npid=" + utils.QuoteShell(pid) + "\nport=" + utils.QuoteShell(fmt.Sprintf("%04X", n)) + "\naddress=" + utils.QuoteShell(address) + "\nwildcard=" + utils.QuoteShell(wildcard) + "\ntable=" + utils.QuoteShell(table) + "\ndual_stack_fallback=" + dualStackFallback + "\n" + `
process_namespace=$(readlink -- "/proc/$pid/ns/net")
current_namespace=$(readlink -- /proc/self/ns/net)
test -n "$process_namespace" && test "$process_namespace" = "$current_namespace"
` + localRouteProof + `
inodes=$(awk -v port="$port" -v address="$address" -v wildcard="$wildcard" '$4=="0A" {split($2,a,":");if(a[2]==port && (a[1]==address || a[1]==wildcard))print $10}' "/proc/$pid/net/$table")
found=0
for fd in /proc/"$pid"/fd/*; do
 link=$(readlink -- "$fd" || true)
 for inode in $inodes; do
  if [ "$link" = "socket:[$inode]" ]; then found=1; fi
 done
done
if [ "$found" != 1 ]; then
 test "$dual_stack_fallback" = 1
 # Any native IPv4 listener for the endpoint must be owned by the selected PID;
 # never bypass a competing socket with the IPv6 compatibility proof.
 test -z "$inodes"
 v6_inodes=$(awk -v port="$port" '$4=="0A" {split($2,a,":");if(a[2]==port && a[1]=="00000000000000000000000000000000")print $10}' "/proc/$pid/net/tcp6")
 set -- $v6_inodes
 test "$#" = 1
 inode="$1"
 owned=0
 for fd in /proc/"$pid"/fd/*; do
  link=$(readlink -- "$fd" || true)
  if [ "$link" = "socket:[$inode]" ]; then owned=1; fi
 done
 test "$owned" = 1
 decimal_port=$((16#$port))
 ss_output=$(
  set +e
  set -o pipefail
  timeout --signal=KILL 3s ss -H -O -6 -ltnpe --inet-sockopt "sport = :$decimal_port" | head -c 4097
  status=$?
  printf '\036'
  exit "$status"
 )
 sentinel=$'\036'
 case "$ss_output" in
  *"$sentinel") ;;
  *) exit 1 ;;
 esac
 ss_output=${ss_output%"$sentinel"}
 test "$(printf %s "$ss_output" | wc -c)" -le 4096
 case "$ss_output" in
  *$'\n') ;;
  *) exit 1 ;;
 esac
 printf %s "$ss_output" | awk -v inode="$inode" '
  {
   inode_fields=0
   inode_matches=0
   v6only_matches=0
   malformed=0
   for (i=1;i<=NF;i++) {
    if (index($i,"ino:")==1) {
     inode_fields++
     if ($i=="ino:" inode) inode_matches++
    }
    if ($i=="v6only:0") v6only_matches++
    else if (index($i,"v6only:")==1) malformed=1
   }
  }
  END {exit !(NR==1 && inode_fields==1 && inode_matches==1 && v6only_matches==1 && malformed==0)}
 '
fi
printf 'listener-owned\n'
`
	out, err := snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell(script))
	if err != nil {
		return err
	}
	if out != "listener-owned\n" {
		return pointReject("readiness listener is not owned by restored process")
	}
	return nil
}

// Each attempt pins HTTP to a numeric endpoint proved by the selected PID.
// For saved local loopback endpoints prefer its IPv4 socket. If absent, an owned
// IPv6 loopback/wildcard socket may prove ::1; this supports Go's default dual-
// stack listener without treating IPV6_V6ONLY as IPv4 coverage. Non-loopback
// destinations stay exact. --connect-to retains the saved URL/Host/TLS identity.
func waitRestoreExecutionReadiness(ctx context.Context, r hostCommandRunner, ready, pid string) error {
	parsed, err := url.Parse(ready)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return pointReject("invalid readiness endpoint")
	}
	host := parsed.Hostname()
	if host != "localhost" && net.ParseIP(host) == nil {
		return pointReject("readiness requires recorded numeric or localhost endpoint")
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	address := host
	if host == "localhost" {
		address = "127.0.0.1"
	}
	var readyErr error
	for attempt := 0; attempt < 10; attempt++ {
		selectedAddress, selectedURL, family := address, ready, "--ipv4"
		readyErr = verifyExecutionListener(ctx, r, selectedURL, pid)
		if errors.Is(readyErr, coressh.ErrRemoteTerminationUnconfirmed) {
			return readyErr
		}
		// Fallback is based only on absent IPv4 ownership, never failed IPv4 HTTP.
		// The selected PID must independently own the IPv6 socket as well.
		if readyErr != nil && (host == "localhost" || host == "127.0.0.1") {
			selectedAddress, selectedURL, family = "::1", "http://"+net.JoinHostPort("::1", port), "--ipv6"
			readyErr = verifyExecutionListener(ctx, r, selectedURL, pid)
		} else if host == "::1" {
			family = "--ipv6"
		}
		if readyErr == nil {
			connection := net.JoinHostPort(host, port) + ":" + net.JoinHostPort(selectedAddress, port)
			command := "curl -fsS " + family + " --noproxy '*' --connect-to " + utils.QuoteShell(connection) + " --max-time 3 -- " + utils.QuoteShell(ready) + " >/dev/null"
			_, readyErr = snapshotRun(ctx, r, command)
			if readyErr == nil {
				readyErr = verifyExecutionListener(ctx, r, selectedURL, pid)
			}
		}
		if readyErr == nil {
			return nil
		}
		if errors.Is(readyErr, coressh.ErrRemoteTerminationUnconfirmed) {
			return readyErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return readyErr
}
