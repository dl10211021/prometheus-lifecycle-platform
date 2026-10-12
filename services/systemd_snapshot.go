package services

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var ErrUnitSnapshot = errors.New("systemd startup snapshot rejected")

const unitSnapshotMaxFiles = 64
const unitSnapshotMaxFileSize = 1 << 20
const unitSnapshotMaxTotalSize = 8 << 20
const unitSnapshotMaxManifest = 256 << 10

var unitSnapshotName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]*\.service$`)
var unitSnapshotPath = regexp.MustCompile(`^/[A-Za-z0-9_./:-]+$`)
var unitSnapshotHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var unitSnapshotDirectory = regexp.MustCompile(`^systemd-[1-9][0-9]*-[0-9a-f]{32}$`)
var unitSnapshotProperties = []string{"Id", "LoadState", "FragmentPath", "DropInPaths", "EnvironmentFiles", "UnitFileState", "ActiveState", "SubState", "MainPID", "ControlPID", "NeedDaemonReload", "Transient"}

// UnitSnapshotReference must be persisted by its caller before authorizing any
// future mutation. A reference is evidence, not restore/apply authorization.
type UnitSnapshotReference struct {
	ComponentID uint   `json:"component_id"`
	HostID      uint   `json:"host_id"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
}
type unitSnapshotBinding struct {
	ComponentID, HostID             uint
	Unit, RuntimeSHA256, HostSHA256 string
}
type unitSnapshotState map[string]string
type unitSnapshotFile struct {
	Path, Role             string
	Optional, Present      bool
	Mode, UID, GID         uint32
	Size                   int64
	SHA256, Stamp, Payload string
}
type unitSnapshotManifest struct {
	Schema  int
	Binding unitSnapshotBinding
	State   unitSnapshotState
	Files   []unitSnapshotFile
}

func snapshotReject(reason string) error { return fmt.Errorf("%w: %s", ErrUnitSnapshot, reason) }
func validUnitSnapshotPath(p string) bool {
	if len(p) > 4096 || !unitSnapshotPath.MatchString(p) || p == "/" || filepath.Clean(p) != p {
		return false
	}
	for _, prefix := range []string{"/proc", "/sys", "/dev", "/run"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return false
		}
	}
	return true
}
func parseUnitSnapshotProperties(unit, output string) (unitSnapshotState, []unitSnapshotFile, error) {
	if !unitSnapshotName.MatchString(unit) || len(unit) > 240 || len(output) > 64<<10 {
		return nil, nil, snapshotReject("invalid unit or property size")
	}
	state := unitSnapshotState{}
	allowed := map[string]bool{}
	for _, p := range unitSnapshotProperties {
		allowed[p] = true
	}
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !allowed[k] {
			return nil, nil, snapshotReject("unexpected systemd property")
		}
		if previous, exists := state[k]; exists {
			// systemctl emits one EnvironmentFiles= row per referenced file.
			// Preserve order; all other properties remain singular.
			if k != "EnvironmentFiles" || previous == "" || v == "" {
				return nil, nil, snapshotReject("duplicate systemd property")
			}
			state[k] = previous + " " + v
			continue
		}
		state[k] = v
	}
	if len(state) != len(allowed) {
		return nil, nil, snapshotReject("incomplete systemd properties")
	}
	if state["Id"] != unit || state["LoadState"] != "loaded" || state["NeedDaemonReload"] != "no" || state["Transient"] != "no" {
		return nil, nil, snapshotReject("unit is aliased, unloaded, transient or requires daemon reload")
	}
	switch state["UnitFileState"] {
	case "enabled", "disabled", "static":
	default:
		return nil, nil, snapshotReject("unsupported unit enablement state")
	}
	pid, err := strconv.ParseUint(state["MainPID"], 10, 32)
	if err != nil {
		return nil, nil, snapshotReject("invalid service PID")
	}
	if state["ControlPID"] != "0" {
		return nil, nil, snapshotReject("service control process is active")
	}
	stable := (state["ActiveState"] == "active" && state["SubState"] == "running" && pid > 0) || (state["ActiveState"] == "inactive" && state["SubState"] == "dead" && pid == 0) || (state["ActiveState"] == "failed" && state["SubState"] == "failed" && pid == 0)
	if !stable {
		return nil, nil, snapshotReject("service state is not stable")
	}
	files := []unitSnapshotFile{}
	seen := map[string]int{}
	add := func(path, role string, optional bool) error {
		if !validUnitSnapshotPath(path) {
			return snapshotReject("unsupported source path")
		}
		if n, ok := seen[path]; ok {
			if files[n].Role != role {
				return snapshotReject("source has overlapping roles")
			}
			files[n].Optional = files[n].Optional && optional
			return nil
		}
		if len(files) >= unitSnapshotMaxFiles {
			return snapshotReject("too many unit files")
		}
		seen[path] = len(files)
		files = append(files, unitSnapshotFile{Path: path, Role: role, Optional: optional})
		return nil
	}
	fragment := state["FragmentPath"]
	if filepath.Base(fragment) != unit {
		return nil, nil, snapshotReject("fragment does not match unit")
	}
	if err := add(fragment, "fragment", false); err != nil {
		return nil, nil, err
	}
	for _, p := range strings.Fields(state["DropInPaths"]) {
		if !strings.HasSuffix(p, ".conf") {
			return nil, nil, snapshotReject("invalid drop-in")
		}
		if err := add(p, "dropin", false); err != nil {
			return nil, nil, err
		}
	}
	env := strings.Fields(state["EnvironmentFiles"])
	if len(env)%2 != 0 {
		return nil, nil, snapshotReject("unsupported environment file syntax")
	}
	for i := 0; i < len(env); i += 2 {
		if env[i+1] != "(ignore_errors=yes)" && env[i+1] != "(ignore_errors=no)" {
			return nil, nil, snapshotReject("unsupported environment file syntax")
		}
		if err := add(env[i], "environment", env[i+1] == "(ignore_errors=yes)"); err != nil {
			return nil, nil, err
		}
	}
	return state, files, nil
}
func validUnitSnapshotBinding(b unitSnapshotBinding) bool {
	return b.ComponentID > 0 && b.HostID > 0 && unitSnapshotName.MatchString(b.Unit) && len(b.Unit) <= 240 && unitSnapshotHash.MatchString(b.RuntimeSHA256) && unitSnapshotHash.MatchString(b.HostSHA256)
}
