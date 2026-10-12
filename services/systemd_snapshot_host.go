package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/pkg/utils"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// Host stderr may contain secrets. Preserve typed cancellation/termination
// errors without returning command output through diagnostics.
func snapshotRun(ctx context.Context, r hostCommandRunner, cmd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Join(ErrUnitSnapshot, err)
	}
	out, err := r.Run(ctx, cmd)
	if err != nil {
		if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
			return "", errors.Join(ErrUnitSnapshot, coressh.ErrRemoteTerminationUnconfirmed)
		}
		if ctx.Err() != nil {
			return "", errors.Join(ErrUnitSnapshot, ctx.Err())
		}
		return "", snapshotReject("host command failed")
	}
	return out, nil
}
func readUnitSnapshotState(ctx context.Context, r hostCommandRunner, unit string) (unitSnapshotState, []unitSnapshotFile, error) {
	out, err := snapshotRun(ctx, r, "systemctl show --no-pager --all --property="+strings.Join(unitSnapshotProperties, ",")+" -- "+utils.QuoteShell(unit))
	if err != nil {
		return nil, nil, err
	}
	if !unitSnapshotPropertyPresent(out, "EnvironmentFiles") {
		if err := verifyEmptyUnitEnvironmentFiles(ctx, r, unit); err != nil {
			return nil, nil, err
		}
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "EnvironmentFiles=\n"
	}
	return parseUnitSnapshotProperties(unit, out)
}

func unitSnapshotPropertyPresent(output, name string) bool {
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		key, _, ok := strings.Cut(line, "=")
		if ok && key == name {
			return true
		}
	}
	return false
}

// systemd 239 exposes Service.EnvironmentFiles on D-Bus but systemctl show
// omits the property when its typed array is empty. Only that exact typed empty
// value may complete the inventory; every other missing-property case remains
// a fail-closed snapshot rejection.
func verifyEmptyUnitEnvironmentFiles(ctx context.Context, r hostCommandRunner, unit string) error {
	objectReply, err := snapshotRun(ctx, r, "busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager GetUnit s "+utils.QuoteShell(unit))
	if err != nil {
		return err
	}
	line := strings.TrimSuffix(objectReply, "\n")
	const objectPrefix = `o "`
	if strings.Contains(line, "\n") || !strings.HasPrefix(line, objectPrefix) || !strings.HasSuffix(line, `"`) {
		return snapshotReject("invalid systemd unit object")
	}
	object := strings.TrimSuffix(strings.TrimPrefix(line, objectPrefix), `"`)
	const pathPrefix = "/org/freedesktop/systemd1/unit/"
	if !strings.HasPrefix(object, pathPrefix) || len(object) == len(pathPrefix) ||
		strings.Contains(strings.TrimPrefix(object, pathPrefix), "/") {
		return snapshotReject("invalid systemd unit object")
	}
	for _, char := range object {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' && char != '/' {
			return snapshotReject("invalid systemd unit object")
		}
	}
	environmentFiles, err := snapshotRun(ctx, r, "busctl get-property org.freedesktop.systemd1 "+utils.QuoteShell(object)+" org.freedesktop.systemd1.Service EnvironmentFiles")
	if err != nil {
		return err
	}
	if environmentFiles != "a(sb) 0\n" {
		return snapshotReject("missing typed environment file inventory")
	}
	return nil
}

// Stat before/after hashing rejects ordinary concurrent replacement/writes.
// Claims exclude platform writers; they do not lock out external root writers.
func inspectUnitSnapshotFile(ctx context.Context, r hostCommandRunner, f unitSnapshotFile) (unitSnapshotFile, error) {
	return inspectBoundedSnapshotFile(ctx, r, f, unitSnapshotMaxFileSize)
}

// Binary evidence uses the same strict file checks with its separate size cap.
func inspectBoundedSnapshotFile(ctx context.Context, r hostCommandRunner, f unitSnapshotFile, maxSize int64) (unitSnapshotFile, error) {
	if maxSize <= 0 || maxSize > 512<<20 {
		return f, snapshotReject("unsupported file evidence size limit")
	}
	cmd := "set -eu\nlimit=" + strconv.FormatInt(maxSize, 10) + "\nreadLimit=" + strconv.FormatInt(maxSize+1, 10) + "\np=" + utils.QuoteShell(f.Path) + `
test "$(readlink -m -- "$p")" = "$p"
if [ ! -e "$p" ] && [ ! -L "$p" ]; then printf 'missing\n'; exit 0; fi
test -f "$p" && test ! -L "$p"
test "$(stat -c '%u:%h' -- "$p")" = '0:1'
test -z "$(find "$p" -maxdepth 0 \( -perm /022 -o -perm /7000 \) -print)"
before=$(stat -c '%a|%u|%g|%s|%h|%d|%i|%y|%z' -- "$p")
size=$(stat -c %s -- "$p")
test "$size" -le "$limit"
digest=$(head -c "$readLimit" -- "$p" | sha256sum)
after=$(stat -c '%a|%u|%g|%s|%h|%d|%i|%y|%z' -- "$p")
test "$before" = "$after"
printf '%s\n%s\n' "$before" "$digest"
`
	out, err := snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell(cmd))
	if err != nil {
		return f, err
	}
	if out == "missing\n" {
		if !f.Optional {
			return f, snapshotReject("required source missing")
		}
		return f, nil
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		return f, snapshotReject("invalid file evidence")
	}
	parts := strings.Split(lines[0], "|")
	if len(parts) != 9 || parts[4] != "1" {
		return f, snapshotReject("invalid file metadata or hard link")
	}
	mode, e1 := strconv.ParseUint(parts[0], 8, 32)
	uid, e2 := strconv.ParseUint(parts[1], 10, 32)
	gid, e3 := strconv.ParseUint(parts[2], 10, 32)
	size, e4 := strconv.ParseInt(parts[3], 10, 64)
	digest := strings.Fields(lines[1])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || mode > 0777 || mode&0022 != 0 || uid != 0 || size < 0 || size > maxSize || len(digest) != 2 || !unitSnapshotHash.MatchString(digest[0]) {
		return f, snapshotReject("unsupported ownership, permissions, size or checksum")
	}
	f.Present = true
	f.Mode = uint32(mode)
	f.UID = uint32(uid)
	f.GID = uint32(gid)
	f.Size = size
	f.SHA256 = digest[0]
	f.Stamp = lines[0]
	return f, nil
}
func snapshotInventory(ctx context.Context, r hostCommandRunner, unit string) (unitSnapshotState, []unitSnapshotFile, error) {
	state, files, err := readUnitSnapshotState(ctx, r, unit)
	if err != nil {
		return nil, nil, err
	}
	var total int64
	for i, f := range files {
		files[i], err = inspectUnitSnapshotFile(ctx, r, f)
		if err != nil {
			return nil, nil, err
		}
		total += files[i].Size
		if total > unitSnapshotMaxTotalSize {
			return nil, nil, snapshotReject("snapshot exceeds total size limit")
		}
	}
	return state, files, nil
}
func snapshotPrivateDirectory(ctx context.Context, r hostCommandRunner, path string) error {
	_, err := snapshotRun(ctx, r, "set -eu\np="+utils.QuoteShell(path)+`
test "$(readlink -m -- "$p")" = "$p"
test -d "$p" && test ! -L "$p"
test "$(stat -c '%a:%u:%g' -- "$p")" = '700:0:0'
`)
	return err
}
func captureUnitSnapshot(ctx context.Context, r hostCommandRunner, root string, b unitSnapshotBinding) (*UnitSnapshotReference, error) {
	if !validUnitSnapshotBinding(b) || !validUnitSnapshotPath(root) {
		return nil, snapshotReject("invalid capture identity or root")
	}
	state, files, err := snapshotInventory(ctx, r, b.Unit)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if root == f.Path || strings.HasPrefix(root, f.Path+"/") {
			return nil, snapshotReject("artifact root overlaps source")
		}
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, snapshotReject("generate artifact identity")
	}
	dir := filepath.Join(root, fmt.Sprintf("systemd-%d-%x", b.ComponentID, random[:]))
	cmd := "set -eu\numask 077\nroot=" + utils.QuoteShell(root) + "\ndir=" + utils.QuoteShell(dir) + `
test "$(id -u)" = 0
test "$(readlink -m -- "$root")" = "$root"
mkdir -p -- "$root"
test -d "$root" && test ! -L "$root"
test "$(stat -c %u -- "$root")" = 0
test -z "$(find "$root" -maxdepth 0 -perm /022 -print)"
mkdir -m 0700 -- "$dir"
chown 0:0 -- "$dir"
`
	if _, err = snapshotRun(ctx, r, cmd); err != nil {
		return nil, err
	}
	if err = snapshotPrivateDirectory(ctx, r, dir); err != nil {
		return nil, err
	}
	for i := range files {
		f := &files[i]
		if !f.Present {
			continue
		}
		f.Payload = fmt.Sprintf("file-%02d", i)
		target := filepath.Join(dir, f.Payload)
		cmd = "set -eu\numask 077\nsrc=" + utils.QuoteShell(f.Path) + "\ndst=" + utils.QuoteShell(target) + `
test "$(readlink -m -- "$src")" = "$src"
test -f "$src" && test ! -L "$src"
test ! -e "$dst" && test ! -L "$dst"
test "$(stat -c '%u:%h' -- "$src")" = '0:1'
test -z "$(find "$src" -maxdepth 0 \( -perm /022 -o -perm /7000 \) -print)"
set -C
head -c 1048577 -- "$src" > "$dst"
test -f "$dst" && test ! -L "$dst"
test "$(stat -c %s -- "$dst")" -le 1048576
chown 0:0 -- "$dst"
chmod 0600 -- "$dst"
sync -f "$dst"
`
		if _, err = snapshotRun(ctx, r, cmd); err != nil {
			return nil, err
		}
		saved, err := inspectUnitSnapshotFile(ctx, r, unitSnapshotFile{Path: target})
		if err != nil {
			return nil, err
		}
		if saved.SHA256 != f.SHA256 || saved.Size != f.Size || saved.Mode != 0600 || saved.GID != 0 {
			return nil, snapshotReject("copied file does not match source")
		}
	}
	again, latest, err := snapshotInventory(ctx, r, b.Unit)
	if err != nil {
		return nil, err
	}
	compare := append([]unitSnapshotFile(nil), files...)
	for i := range compare {
		compare[i].Payload = ""
	}
	if !reflect.DeepEqual(state, again) || !reflect.DeepEqual(compare, latest) {
		return nil, snapshotReject("live baseline changed during capture")
	}
	raw, err := json.Marshal(unitSnapshotManifest{Schema: 1, Binding: b, State: state, Files: files})
	if err != nil || len(raw) > unitSnapshotMaxManifest {
		return nil, snapshotReject("manifest exceeds supported format")
	}
	digest := sha256.Sum256(raw)
	cmd = "set -eu\numask 077\ncd -- " + utils.QuoteShell(dir) + "\ntest ! -e manifest.json && test ! -L manifest.json\nprintf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString(raw)) + ` | base64 -d > manifest.pending
chmod 0600 manifest.pending
chown 0:0 manifest.pending
sync -f manifest.pending
mv -T -- manifest.pending manifest.json
sync -f .
`
	if _, err = snapshotRun(ctx, r, cmd); err != nil {
		return nil, err
	}
	ref := &UnitSnapshotReference{ComponentID: b.ComponentID, HostID: b.HostID, Path: dir, SHA256: hex.EncodeToString(digest[:])}
	if _, err = verifyUnitSnapshot(ctx, r, root, ref, b, true); err != nil {
		return nil, err
	}
	return ref, nil
}

// Historical verification does not require live source availability. Live
// verification additionally checks the exact capture baseline, including PID.
func verifyUnitSnapshot(ctx context.Context, r hostCommandRunner, root string, ref *UnitSnapshotReference, b unitSnapshotBinding, live bool) (*unitSnapshotManifest, error) {
	if ref == nil || !validUnitSnapshotBinding(b) || !validUnitSnapshotPath(root) || ref.ComponentID != b.ComponentID || ref.HostID != b.HostID || !unitSnapshotHash.MatchString(ref.SHA256) || !validUnitSnapshotPath(ref.Path) || filepath.Dir(ref.Path) != root || !unitSnapshotDirectory.MatchString(filepath.Base(ref.Path)) {
		return nil, snapshotReject("invalid snapshot reference")
	}
	if err := snapshotPrivateDirectory(ctx, r, ref.Path); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(ref.Path, "manifest.json")
	meta, err := inspectUnitSnapshotFile(ctx, r, unitSnapshotFile{Path: manifestPath})
	if err != nil {
		return nil, err
	}
	if meta.Mode != 0600 || meta.GID != 0 || meta.Size > unitSnapshotMaxManifest || meta.SHA256 != ref.SHA256 {
		return nil, snapshotReject("manifest integrity failed")
	}
	raw, err := snapshotRun(ctx, r, "head -c 262145 -- "+utils.QuoteShell(manifestPath))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(raw))
	if len(raw) > unitSnapshotMaxManifest || hex.EncodeToString(hash[:]) != ref.SHA256 {
		return nil, snapshotReject("manifest changed while reading")
	}
	var m unitSnapshotManifest
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil || m.Schema != 1 || m.Binding != b || len(m.Files) == 0 || len(m.Files) > unitSnapshotMaxFiles {
		return nil, snapshotReject("invalid or unrelated manifest")
	}
	var properties strings.Builder
	for _, key := range unitSnapshotProperties {
		value, ok := m.State[key]
		if !ok {
			return nil, snapshotReject("incomplete saved properties")
		}
		fmt.Fprintf(&properties, "%s=%s\n", key, value)
	}
	state, expected, err := parseUnitSnapshotProperties(b.Unit, properties.String())
	if err != nil || !reflect.DeepEqual(state, m.State) || len(expected) != len(m.Files) {
		return nil, snapshotReject("invalid saved file inventory")
	}
	names := []string{}
	var total int64
	for i, f := range m.Files {
		e := expected[i]
		if f.Path != e.Path || f.Role != e.Role || f.Optional != e.Optional {
			return nil, snapshotReject("file inventory mismatch")
		}
		if !f.Present {
			if !f.Optional || f.Mode != 0 || f.UID != 0 || f.GID != 0 || f.Size != 0 || f.SHA256 != "" || f.Stamp != "" || f.Payload != "" {
				return nil, snapshotReject("invalid absent file")
			}
			continue
		}
		if f.Payload != fmt.Sprintf("file-%02d", i) || f.Mode > 0777 || f.Mode&0022 != 0 || f.UID != 0 || f.Size < 0 || f.Size > unitSnapshotMaxFileSize || !unitSnapshotHash.MatchString(f.SHA256) || f.Stamp == "" {
			return nil, snapshotReject("invalid source metadata")
		}
		total += f.Size
		if total > unitSnapshotMaxTotalSize {
			return nil, snapshotReject("snapshot exceeds total size limit")
		}
		saved, err := inspectUnitSnapshotFile(ctx, r, unitSnapshotFile{Path: filepath.Join(ref.Path, f.Payload)})
		if err != nil {
			return nil, err
		}
		if saved.Mode != 0600 || saved.GID != 0 || saved.Size != f.Size || saved.SHA256 != f.SHA256 {
			return nil, snapshotReject("payload integrity failed")
		}
		names = append(names, f.Payload)
	}
	list, err := snapshotRun(ctx, r, "find "+utils.QuoteShell(ref.Path)+" -mindepth 1 -maxdepth 1 -printf '%f\\n' | LC_ALL=C sort")
	if err != nil {
		return nil, err
	}
	names = append(names, "manifest.json")
	if list != strings.Join(names, "\n")+"\n" {
		return nil, snapshotReject("artifact has unexpected entries")
	}
	if live {
		current, files, err := snapshotInventory(ctx, r, b.Unit)
		if err != nil {
			return nil, err
		}
		compare := append([]unitSnapshotFile(nil), m.Files...)
		for i := range compare {
			compare[i].Payload = ""
		}
		if !reflect.DeepEqual(current, m.State) || !reflect.DeepEqual(files, compare) {
			return nil, snapshotReject("live unit baseline drifted")
		}
	}
	return &m, nil
}
