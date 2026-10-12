package services

import (
	"context"
	"fmt"
	"github.com/promlifecycle/platform/pkg/utils"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

type pairedRestoreUnitReference struct {
	ID              string
	Names, Paths    []string
	NamespaceChecks []string
}

var pairedRestoreReferencePath = regexp.MustCompile(`^/[A-Za-z0-9_./:@-]+$`)

var pairedRestoreUnitName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:@-]*\.service$`)

// Accept standard environment-variable tokens in argv, but no arbitrary
// braces that could impersonate another executable record. Only path is used.
var pairedRestoreExecRecord = regexp.MustCompile(`^\{ path=([^;{}\n]+) ; argv\[\]=(?:[^{}\n]|\$\{[A-Za-z_][A-Za-z0-9_]*\})* ; (?:ignore_errors=(?:yes|no)|flags=[^;{}\n]*) ; start_time=\[[^\]\n]*\] ; stop_time=\[[^\]\n]*\] ; pid=[0-9]+ ; code=[^;{}\n]+ ; status=[^;{}\n]+ \}`)

func pairedRestoreExecutableProperty(key string) bool {
	switch key {
	case "ExecStart", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecCondition", "ExecReload":
		return true
	}
	return false
}
func pairedRestoreExecutablePaths(value string) ([]string, error) {
	var paths []string
	for value != "" {
		match := pairedRestoreExecRecord.FindStringSubmatchIndex(value)
		if match == nil {
			return nil, pointReject("unsupported or ambiguous direct executable record")
		}
		path := value[match[2]:match[3]]
		if !pairedRestoreLiteralReference(path) {
			return nil, pointReject("unsupported direct executable path")
		}
		paths = append(paths, path)
		value = value[match[1]:]
		if value == "" {
			break
		}
		if strings.HasPrefix(value, "\n{ path=") {
			value = value[1:]
		} else if strings.HasPrefix(value, " ; { path=") {
			value = value[3:]
		} else {
			return nil, pointReject("incomplete direct executable records")
		}
	}
	return paths, nil
}

const pairedRestoreNamesStart = "PROMLIFECYCLE_UNITS_V1\n"
const pairedRestorePropertiesStart = "\nPROMLIFECYCLE_PROPERTIES_V1\n"
const pairedRestoreReferencesEnd = "\nPROMLIFECYCLE_END_V1\n"

func pairedRestoreLiteralReference(path string) bool {
	return len(path) <= 4096 && path != "/" && pairedRestoreReferencePath.MatchString(path)
}
func pairedRestoreReferenceNames(raw string) ([]string, error) {
	names := strings.Fields(raw)
	if len(names) == 0 || len(names) > 2048 {
		return nil, pointReject("invalid reference unit count")
	}
	seen := map[string]bool{}
	for _, name := range names {
		if len(name) > 255 || !pairedRestoreUnitName.MatchString(name) || strings.HasSuffix(name, "@.service") || seen[name] {
			return nil, pointReject("invalid or duplicate reference unit name")
		}
		seen[name] = true
	}
	sort.Strings(names)
	return names, nil
}
func uniquePairedRestorePaths(paths []string) []string {
	sort.Strings(paths)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if len(out) == 0 || out[len(out)-1] != p {
			out = append(out, p)
		}
	}
	return out
}

// Only direct references are extracted. Raw argv and environment values never
// leave this parser in errors or the public preview.
func parsePairedRestoreUnitReferences(raw string) ([]pairedRestoreUnitReference, error) {
	reject := func() ([]pairedRestoreUnitReference, error) {
		return nil, pointReject("incomplete or unsupported direct service reference inventory")
	}
	if len(raw) > 3<<20 || !strings.HasPrefix(raw, pairedRestoreNamesStart) || !strings.HasSuffix(raw, pairedRestoreReferencesEnd) || strings.Count(raw, pairedRestoreNamesStart) != 1 || strings.Count(raw, pairedRestorePropertiesStart) != 1 || strings.Count(raw, pairedRestoreReferencesEnd) != 1 {
		return reject()
	}
	body := strings.TrimSuffix(strings.TrimPrefix(raw, pairedRestoreNamesStart), pairedRestoreReferencesEnd)
	namesRaw, properties, ok := strings.Cut(body, pairedRestorePropertiesStart)
	if !ok || len(namesRaw) > 512<<10 || len(properties) > 2<<20 {
		return reject()
	}
	requested, err := pairedRestoreReferenceNames(namesRaw)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	covered := map[string]bool{}
	owners := map[string]string{}
	for _, name := range requested {
		wanted[name] = true
	}
	allowed := map[string]bool{"Id": true, "Names": true, "LoadState": true, "ActiveState": true, "SubState": true, "MainPID": true, "ControlPID": true, "FragmentPath": true, "DropInPaths": true, "EnvironmentFiles": true, "ExecStart": true, "ExecStartPre": true, "ExecStartPost": true, "ExecStop": true, "ExecStopPost": true, "ExecCondition": true, "ExecReload": true}
	for _, key := range []string{"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories"} {
		allowed[key] = true
	}
	refs := map[string]pairedRestoreUnitReference{}
	fields := map[string]string{}
	flush := func() error {
		if len(fields) == 0 {
			return nil
		}
		id := fields["Id"]
		if len(id) > 255 || !pairedRestoreUnitName.MatchString(id) || strings.HasSuffix(id, "@.service") || (fields["LoadState"] != "loaded" && fields["LoadState"] != "masked" && fields["LoadState"] != "not-found") {
			return pointReject("unsupported service identity in reference inventory")
		}
		aliases, err := pairedRestoreReferenceNames(fields["Names"])
		if err != nil {
			return err
		}
		hasID, hasRequested := false, false
		for _, name := range aliases {
			if owner, exists := owners[name]; exists && owner != id {
				return pointReject("conflicting service alias ownership")
			}
			owners[name] = id
			hasID = hasID || name == id
			if wanted[name] {
				covered[name] = true
				hasRequested = true
			}
		}
		if !hasID || !hasRequested {
			return pointReject("unaccounted service identity in reference inventory")
		}
		var namespaceChecks []string
		for _, key := range []string{"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories"} {
			value := fields[key]
			if value == "" {
				continue
			}
			if value == "[unprintable]" && (key == "BindPaths" || key == "BindReadOnlyPaths" || key == "TemporaryFileSystem") {
				namespaceChecks = append(namespaceChecks, key)
				continue
			}
			return pointReject("service filesystem mapping prevents direct host reference admission")
		}
		paths := []string{}
		add := func(path string) error {
			if !pairedRestoreLiteralReference(path) {
				return pointReject("indirect or ambiguous service file reference")
			}
			paths = append(paths, path)
			return nil
		}
		if path := fields["FragmentPath"]; path != "" {
			if err := add(path); err != nil {
				return err
			}
		}
		for _, path := range strings.Fields(fields["DropInPaths"]) {
			if err := add(path); err != nil {
				return err
			}
		}
		env := strings.Fields(fields["EnvironmentFiles"])
		if len(env)%2 != 0 {
			return pointReject("ambiguous service environment reference")
		}
		for i := 0; i < len(env); i += 2 {
			if env[i+1] != "(ignore_errors=yes)" && env[i+1] != "(ignore_errors=no)" {
				return pointReject("ambiguous service environment reference")
			}
			if err := add(env[i]); err != nil {
				return err
			}
		}
		for key, value := range fields {
			if !pairedRestoreExecutableProperty(key) || value == "" {
				continue
			}
			executablePaths, err := pairedRestoreExecutablePaths(value)
			if err != nil {
				return err
			}
			paths = append(paths, executablePaths...)
		}
		if fields["LoadState"] == "not-found" && (fields["ActiveState"] != "inactive" || fields["SubState"] != "dead" || fields["MainPID"] != "0" || fields["ControlPID"] != "0" || len(paths) != 0) {
			return pointReject("absent dependency has unresolved state or references")
		}
		ref := pairedRestoreUnitReference{ID: id, Names: aliases, Paths: uniquePairedRestorePaths(paths), NamespaceChecks: namespaceChecks}
		if previous, ok := refs[id]; ok {
			if !reflect.DeepEqual(previous.Paths, ref.Paths) || !reflect.DeepEqual(previous.NamespaceChecks, ref.NamespaceChecks) {
				return pointReject("service alias references disagree")
			}
			// Inactive aliases may load/unload between systemctl's property
			// calls. Preserve their union only when canonical ID and paths
			// agree; owners above still rejects cross-ID alias conflicts.
			ref.Names = uniquePairedRestorePaths(append(previous.Names, ref.Names...))
		}
		refs[id] = ref
		fields = map[string]string{}
		return nil
	}
	for _, line := range strings.Split(properties, "\n") {
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] || strings.ContainsRune(value, '\r') || strings.ContainsRune(value, 0) {
			return reject()
		}
		if previous, exists := fields[key]; exists {
			if (key != "EnvironmentFiles" && !pairedRestoreExecutableProperty(key)) || previous == "" || value == "" {
				return reject()
			}
			separator := " "
			if pairedRestoreExecutableProperty(key) {
				separator = "\n"
			}
			fields[key] = previous + separator + value
		} else {
			fields[key] = value
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(covered) != len(wanted) || len(refs) == 0 {
		return reject()
	}
	result := make([]pairedRestoreUnitReference, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func readPairedRestoreUnitReferences(ctx context.Context, r hostCommandRunner) ([]pairedRestoreUnitReference, error) {
	// Installed instances and loaded transient services count. Uninstantiated
	// templates cannot execute and do not identify a current service instance.
	script := `# promlifecycle:paired-restore-unit-references
set -euo pipefail
export LC_ALL=C SYSTEMD_COLORS=0 SYSTEMD_PAGER=cat
names() {
 local files loaded all
 files=$(systemctl list-unit-files --type=service --no-legend --no-pager | head -c 524289 && printf .) || return 1
 files=${files%.}
 test "${#files}" -le 524288 || return 1
 loaded=$(systemctl list-units --all --type=service --plain --no-legend --no-pager | head -c 524289 && printf .) || return 1
 loaded=${loaded%.}
 test "${#loaded}" -le 524288 || return 1
 all=$(printf '%s\n%s\n' "$files" "$loaded" | awk 'NF && $1 !~ /@\.service$/ {print $1}' | sort -u) || return 1
 test -n "$all" || return 1
 test "$(printf '%s\n' "$all" | wc -l)" -le 2048 || return 1
 while IFS= read -r name; do
  test "${#name}" -le 255 || return 1
  [[ "$name" =~ ^[A-Za-z0-9_][A-Za-z0-9_.:@-]*\.service$ ]] || return 1
 done <<< "$all"
 printf '%s' "$all"
}
before=$(names)
mapfile -t units <<< "$before"
properties=$(systemctl show --all --no-pager --property=Id,Names,LoadState,ActiveState,SubState,MainPID,ControlPID,FragmentPath,DropInPaths,EnvironmentFiles,ExecStart,ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,ExecCondition,ExecReload,RootDirectory,RootImage,BindPaths,BindReadOnlyPaths,TemporaryFileSystem,MountImages,ExtensionImages,ExtensionDirectories -- "${units[@]}" | head -c 2097153 && printf .)
properties=${properties%.}
test "${#properties}" -le 2097152
after=$(names)
test "$before" = "$after"
printf 'PROMLIFECYCLE_UNITS_V1\n%s\nPROMLIFECYCLE_PROPERTIES_V1\n%s\nPROMLIFECYCLE_END_V1\n' "$before" "$properties"
`
	raw, err := snapshotRun(ctx, r, "bash -c "+utils.QuoteShell(script))
	if err != nil {
		return nil, err
	}
	refs, err := parsePairedRestoreUnitReferences(raw)
	if err != nil {
		return nil, err
	}
	if err := verifyPairedRestoreReferenceNamespaces(ctx, r, refs); err != nil {
		return nil, err
	}
	return resolvePairedRestoreUnitReferences(ctx, r, refs)
}

// systemd 239 renders these arrays as [unprintable], including empty arrays.
// The text is never treated as evidence of emptiness: verify the typed values.
func verifyPairedRestoreReferenceNamespaces(ctx context.Context, r hostCommandRunner, refs []pairedRestoreUnitReference) error {
	const prefix = "# promlifecycle:paired-restore-namespace-references\nset -euo pipefail\nexport LC_ALL=C SYSTEMD_COLORS=0 SYSTEMD_PAGER=cat\n"
	script, expected := prefix, ""
	flush := func() error {
		if expected == "" {
			return nil
		}
		out, err := snapshotRun(ctx, r, "bash -c "+utils.QuoteShell(script))
		if err != nil {
			return err
		}
		if out != expected {
			return pointReject("incomplete typed service namespace verification")
		}
		script, expected = prefix, ""
		return nil
	}
	for _, ref := range refs {
		if len(ref.NamespaceChecks) == 0 {
			continue
		}
		var escaped strings.Builder
		for i, b := range []byte(ref.ID) {
			if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || i > 0 && b >= '0' && b <= '9' {
				escaped.WriteByte(b)
			} else {
				fmt.Fprintf(&escaped, "_%02x", b)
			}
		}
		typed := ""
		for _, key := range ref.NamespaceChecks {
			switch key {
			case "BindPaths", "BindReadOnlyPaths":
				typed += "a(ssbt) 0\n"
			case "TemporaryFileSystem":
				typed += "a(ss) 0\n"
			default:
				return pointReject("unsupported typed namespace property")
			}
		}
		chunk := "systemctl show --no-pager --property=Id -- " + utils.QuoteShell(ref.ID) + " > /dev/null\n"
		chunk += "value=$(busctl get-property org.freedesktop.systemd1 " + utils.QuoteShell("/org/freedesktop/systemd1/unit/"+escaped.String()) + " org.freedesktop.systemd1.Service " + strings.Join(ref.NamespaceChecks, " ") + " | head -c 4097 && printf .)\n"
		chunk += "value=${value%.}\ntest \"${#value}\" -le 4096\ntest \"$value\" = " + utils.QuoteShell(typed) + "\nprintf '%s\\n' " + utils.QuoteShell(ref.ID) + "\n"
		if len(script)+len(chunk) > 48<<10 {
			if err := flush(); err != nil {
				return err
			}
		}
		script += chunk
		expected += ref.ID + "\n"
	}
	return flush()
}
func resolvePairedRestoreUnitReferences(ctx context.Context, r hostCommandRunner, refs []pairedRestoreUnitReference) ([]pairedRestoreUnitReference, error) {
	paths := []string{}
	for _, ref := range refs {
		paths = append(paths, ref.Paths...)
	}
	paths = uniquePairedRestorePaths(paths)
	resolved := map[string]string{}
	for start := 0; start < len(paths); {
		end, size := start, 0
		quoted := []string{}
		for end < len(paths) && size+len(paths[end])+4 < 48<<10 {
			quoted = append(quoted, utils.QuoteShell(paths[end]))
			size += len(paths[end]) + 4
			end++
		}
		out, err := snapshotRun(ctx, r, "# promlifecycle:paired-restore-resolve-references\nreadlink -m -- "+strings.Join(quoted, " "))
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != end-start {
			return nil, pointReject("incomplete canonical service references")
		}
		for i, path := range lines {
			if !pairedRestoreLiteralReference(path) || filepath.Clean(path) != path {
				return nil, pointReject("unsupported canonical service reference")
			}
			resolved[paths[start+i]] = path
		}
		start = end
	}
	for i := range refs {
		for j, path := range refs[i].Paths {
			refs[i].Paths[j] = resolved[path]
		}
		refs[i].Paths = uniquePairedRestorePaths(refs[i].Paths)
	}
	return refs, nil
}
