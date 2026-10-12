package deployer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// ErrKeepDataUnsafe is an admission failure, before any service or file mutation.
// Callers must not compensate by redeploying over the data being protected.
var ErrKeepDataUnsafe = errors.New("keep_data removal cannot safely preserve data")

// ValidateRemoveKeepData checks the same deletion targets used by remove. Both
// recorded and effective data roots are retained: an override does not prove
// that a previously recorded root has been emptied or migrated.
func (d *BinaryDeployer) ValidateRemoveKeepData(ctx context.Context, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrKeepDataUnsafe, err)
	}
	var dataPaths []string
	for _, path := range []string{component.DataPath, effectiveDataDir(spec, component)} {
		if path != "" && (len(dataPaths) == 0 || dataPaths[0] != path) {
			dataPaths = append(dataPaths, path)
		}
	}
	if len(dataPaths) == 0 {
		return nil
	}
	artifact := systemdExecPath(spec, component)
	if spec.FullDir {
		artifact = installDirFor(spec, component)
	}
	targets := []string{artifact, "/etc/systemd/system/" + serviceUnit(spec, component) + ".service", environmentFilePath(spec, component)}
	if spec.HasConfig && spec.ConfigDir == filepath.Join(instanceConfigRoot, InstanceKey(component)) {
		targets = append(targets, spec.ConfigDir)
	}
	if !spec.FullDir && installDirFor(spec, component) == filepath.Join("/opt/promlifecycle/components", InstanceKey(component)) {
		targets = append(targets, installDirFor(spec, component))
	}
	paths := append(append([]string(nil), dataPaths...), targets...)
	for _, path := range paths {
		if !filepath.IsAbs(path) || path == "/" || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("%w: invalid path %q", ErrKeepDataUnsafe, path)
		}
	}
	rejectOverlap := func(data, deleted []string) error {
		for i, root := range data {
			for j, target := range deleted {
				if pathsOverlap(root, target) {
					return fmt.Errorf("%w: data directory %s overlaps removal target %s; migrate data to a separate directory before retrying", ErrKeepDataUnsafe, dataPaths[i], targets[j])
				}
			}
		}
		return nil
	}
	if err := rejectOverlap(dataPaths, targets); err != nil {
		return err
	}
	resolved, err := resolveKeepDataPaths(ctx, component.Host, paths)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve removal and retained data paths: %w", ErrKeepDataUnsafe, err)
	}
	return rejectOverlap(resolved[:len(dataPaths)], resolved[len(dataPaths):])
}

// Resolve existing ancestors strictly, allowing only absent trailing entries.
// readlink -m alone accepts broken/looping links and inaccessible paths. Never
// fall back to a lexical path when an existing ancestor cannot be inspected.
// The same read-only script runs locally and over the existing SSH transport.
func resolveKeepDataPaths(ctx context.Context, host models.Host, paths []string) ([]string, error) {
	var script strings.Builder
	script.WriteString("set -eu\nfor original in")
	for _, path := range paths {
		script.WriteString(" " + utils.QuoteShell(path))
	}
	script.WriteString(`; do
 path="$original"
 suffix=
 while [ ! -e "$path" ] && [ ! -L "$path" ]; do
  [ "$path" != / ] || exit 1
  suffix="/${path##*/}$suffix"
  path="${path%/*}"
  [ -n "$path" ] || path=/
 done
 if [ -n "$suffix" ] && [ ! -d "$path" ]; then exit 1; fi
 if [ -d "$path" ]; then [ -r "$path" ] && [ -x "$path" ] || exit 1; fi
 resolved=$(readlink -e -- "$path") || exit 1
 if [ "$resolved" = / ]; then resolved=; fi
 printf '%s%s\n' "$resolved" "$suffix"
 done
`)
	var output string
	if ssh.IsLocalHost(host.IP, host.ConnectionType) {
		raw, err := exec.CommandContext(ctx, "sh", "-c", script.String()).Output()
		if err != nil {
			return nil, err
		}
		output = string(raw)
	} else {
		client, err := ssh.NewClientContext(ctx, host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		raw, _, err := client.RunCommandContext(ctx, script.String())
		if err != nil {
			return nil, err
		}
		output = raw
	}
	resolved := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(resolved) != len(paths) {
		return nil, errors.New("incomplete canonical path response")
	}
	for _, path := range resolved {
		if !filepath.IsAbs(path) || path == "/" || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return nil, fmt.Errorf("invalid canonical path %q", path)
		}
	}
	return resolved, nil
}
