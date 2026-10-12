package deployer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// ErrBinaryRecoveryUnconfirmed forbids restarting an application against data
// whose matching executable could not be restored.
var ErrBinaryRecoveryUnconfirmed = errors.New("binary recovery state is unconfirmed")

// HealthCheck verifies both the systemd service and the component's reviewed
// health endpoint. It is used by explicit operator inspections as well as
// deployment verification.
func (d *BinaryDeployer) HealthCheck(ctx context.Context, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	if err := verifyBinaryServiceActive(ctx, component); err != nil {
		return err
	}
	if spec.HealthPath == "" {
		return nil
	}
	return waitForBinaryHealth(ctx, component, spec.HealthPath)
}

// UploadConfig pushes a config file to the target host (binary deployments) and restarts the service.
func (d *BinaryDeployer) UploadConfig(ctx context.Context, component *models.Component, content string) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	if !spec.HasConfig {
		return fmt.Errorf("%s has no config file to upload", spec.BinName)
	}
	dstPath := filepath.Join(spec.ConfigDir, spec.ConfigFile)

	host := component.Host
	if host.IP == "127.0.0.1" || host.IP == "localhost" || host.ConnectionType == "local" {
		if err := os.MkdirAll(spec.ConfigDir, 0755); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
		if err := os.WriteFile(dstPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		if component.Type == models.ComponentPrometheus {
			if err := os.MkdirAll(filepath.Join(spec.ConfigDir, "rules"), 0755); err != nil {
				return fmt.Errorf("create rules dir: %w", err)
			}
		}
		if out, err := exec.CommandContext(ctx, "systemctl", "restart", serviceUnit(spec, component)).CombinedOutput(); err != nil {
			return fmt.Errorf("restart %s: %s: %w", spec.BinName, out, err)
		}
		return d.HealthCheck(ctx, component)
	}

	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
	}
	defer client.Close()

	if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(spec.ConfigDir)); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	if err := copyFileWithRetry(client, func() (io.Reader, int64, error) {
		return strings.NewReader(content), int64(len(content)), nil
	}, dstPath, 0644); err != nil {
		return fmt.Errorf("upload config: %w", err)
	}
	if component.Type == models.ComponentPrometheus {
		if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(filepath.Join(spec.ConfigDir, "rules"))); err != nil {
			return fmt.Errorf("mkdir rules dir: %w", err)
		}
	}
	if _, _, err := client.RunCommand("systemctl restart " + utils.QuoteShell(serviceUnit(spec, component))); err != nil {
		return fmt.Errorf("restart %s: %w", spec.BinName, err)
	}
	return d.HealthCheck(ctx, component)
}

// Upgrade installs a new version of a binary-deployed component and restarts it.
func (d *BinaryDeployer) Upgrade(ctx context.Context, component *models.Component, newVersion string) (resultErr error) {
	if err := ValidatePrometheusParameterVersion(component, newVersion); err != nil {
		return err
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	if err := d.guardBinaryArtifacts(ctx, component, spec); err != nil {
		return err
	}

	// Extract the new version (offline package preferred, image fallback)
	binaryPath, err := d.extractBinary(ctx, spec, newVersion)
	if err != nil {
		return fmt.Errorf("extract %s %s: %w", spec.BinName, newVersion, err)
	}

	host := component.Host
	isLocal := host.IP == "127.0.0.1" || host.IP == "localhost" || host.ConnectionType == "local"

	if spec.FullDir {
		if isLocal {
			installDir := installDirFor(spec, component)
			stage, err := os.MkdirTemp(filepath.Dir(installDir), ".promlifecycle-upgrade-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(stage)
			if err := copyTree(binaryPath, stage); err != nil {
				return fmt.Errorf("stage install tree: %w", err)
			}
			if out, err := runChown(ctx, stage); err != nil {
				return fmt.Errorf("prepare install tree: %s: %w", out, err)
			}
			backupDir := installDir + ".bak"
			if _, err := os.Stat(backupDir); err == nil {
				return fmt.Errorf("retained backup %s requires recovery before another upgrade", backupDir)
			}
			if err := os.Rename(installDir, backupDir); err != nil {
				return fmt.Errorf("backup install tree: %w", err)
			}
			if err := os.Rename(stage, installDir); err != nil {
				if restoreErr := os.Rename(backupDir, installDir); restoreErr != nil {
					return fmt.Errorf("install tree: %w; restore tree: %v", err, restoreErr)
				}
				return fmt.Errorf("install tree: %w", err)
			}
		} else {
			client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
			if err != nil {
				return fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
			}
			defer client.Close()
			parent := filepath.Dir(binaryPath)
			dirName := filepath.Base(binaryPath)
			archive, err := os.CreateTemp("", "promlifecycle-tree-*.tar.gz")
			if err != nil {
				return err
			}
			tmpTar := archive.Name()
			archive.Close()
			defer os.Remove(tmpTar)
			remoteTar := "/tmp/" + filepath.Base(tmpTar)
			if out, err := exec.CommandContext(ctx, "tar", "czf", tmpTar, "-C", parent, dirName).CombinedOutput(); err != nil {
				return fmt.Errorf("pack install tree: %s: %w", out, err)
			}
			if err := copyFileWithRetry(client, func() (io.Reader, int64, error) {
				f, err := os.Open(tmpTar)
				if err != nil {
					return nil, 0, err
				}
				st, err := f.Stat()
				if err != nil {
					f.Close()
					return nil, 0, err
				}
				return f, st.Size(), nil
			}, remoteTar, 0644); err != nil {
				return fmt.Errorf("upload install tree: %w", err)
			}
			installDir := installDirFor(spec, component)
			stage := installDir + "." + filepath.Base(tmpTar) + ".stage"
			defer func() {
				if errors.Is(resultErr, ssh.ErrRemoteTerminationUnconfirmed) {
					return
				}
				cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				if _, _, err := client.RunCommandContext(cleanup, "rm -rf "+utils.QuoteShell(stage)+" && rm -f "+utils.QuoteShell(remoteTar)); err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("cleanup staged install tree: %w", err))
				}
			}()
			command := "test ! -e " + utils.QuoteShell(installDir+".bak") +
				" && " + extractTreeCommand(remoteTar, stage) +
				" && " + nobodyChownCmd(utils.QuoteShell(stage)) +
				" && mv -T " + utils.QuoteShell(installDir) + " " + utils.QuoteShell(installDir+".bak") +
				" && { mv -T " + utils.QuoteShell(stage) + " " + utils.QuoteShell(installDir) +
				" || { mv -T " + utils.QuoteShell(installDir+".bak") + " " + utils.QuoteShell(installDir) + "; exit 1; }; }"
			if _, _, err := client.RunCommandContext(ctx, command); err != nil {
				return fmt.Errorf("stage or replace install tree (retained .bak requires recovery): %w", err)
			}
		}
	} else {

		if isLocal {
			// Atomic swap: move the running binary aside, put the new one in
			// place. The restart below rolls back on failure.
			installPath := systemdExecPath(spec, component)
			backupPath := installPath + ".bak"
			tmpPath := installPath + ".new"
			if err := copyLocalFile(binaryPath, tmpPath, 0755); err != nil {
				return fmt.Errorf("install binary: %w", err)
			}
			if err := os.Rename(installPath, backupPath); err != nil && !os.IsNotExist(err) {
				os.Remove(tmpPath)
				return fmt.Errorf("backup binary: %w", err)
			}
			if err := os.Rename(tmpPath, installPath); err != nil {
				os.Remove(tmpPath)
				if _, statErr := os.Stat(backupPath); statErr == nil {
					_ = os.Rename(backupPath, installPath)
				}
				return fmt.Errorf("replace binary: %w", err)
			}
		} else {
			client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
			if err != nil {
				return fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
			}
			defer client.Close()
			tmpRemote := systemdExecPath(spec, component) + ".new"
			if err := copyFileWithRetry(client, func() (io.Reader, int64, error) {
				file, err := os.Open(binaryPath)
				if err != nil {
					return nil, 0, err
				}
				info, err := file.Stat()
				if err != nil {
					file.Close()
					return nil, 0, err
				}
				return file, info.Size(), nil
			}, tmpRemote, 0755); err != nil {
				return fmt.Errorf("upload binary: %w", err)
			}
			// Keep a copy of the running binary so a failed restart can be
			// rolled back instead of leaving the host without the service.
			installPath := systemdExecPath(spec, component)
			if _, _, err := client.RunCommandContext(ctx, "cp -f "+utils.QuoteShell(installPath)+" "+utils.QuoteShell(installPath+".bak")+" && mv -f "+utils.QuoteShell(tmpRemote)+" "+utils.QuoteShell(installPath)); err != nil {
				return fmt.Errorf("replace binary: %w", err)
			}
		}
	}

	// Restore executable files on failure, but leave the service stopped.
	// The change service must restore matching data before starting an old release.
	rollbackLocal := func() error {
		recovery, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(recovery, "systemctl", "stop", serviceUnit(spec, component)).CombinedOutput(); err != nil {
			return fmt.Errorf("stop failed version: %s: %w", out, err)
		}
		installDir := installDirFor(spec, component)
		if spec.FullDir {
			if err := os.RemoveAll(installDir); err != nil {
				return err
			}
			if err := os.Rename(installDir+".bak", installDir); err != nil {
				return err
			}
		} else {
			path := systemdExecPath(spec, component)
			if err := os.Rename(path+".bak", path); err != nil {
				return err
			}
		}
		return nil
	}
	rollbackRemote := func(client *ssh.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if _, _, err := client.RunCommandContext(ctx, "systemctl stop "+utils.QuoteShell(serviceUnit(spec, component))); err != nil {
			return err
		}
		installDir := installDirFor(spec, component)
		if spec.FullDir {
			_, _, err := client.RunCommandContext(ctx, "rm -rf "+utils.QuoteShell(installDir)+" && mv "+utils.QuoteShell(installDir+".bak")+" "+utils.QuoteShell(installDir))
			return err
		}
		path := systemdExecPath(spec, component)
		_, _, err := client.RunCommandContext(ctx, "mv -f "+utils.QuoteShell(path+".bak")+" "+utils.QuoteShell(path))
		return err
	}
	if isLocal {
		if out, err := exec.CommandContext(ctx, "systemctl", "restart", serviceUnit(spec, component)).CombinedOutput(); err != nil {
			if rollbackErr := rollbackLocal(); rollbackErr != nil {
				return fmt.Errorf("%w: restart failed: %w; restore files failed: %v", ErrBinaryRecoveryUnconfirmed, err, rollbackErr)
			}
			return fmt.Errorf("restart %s: %s: %w（已恢复上一版本文件，服务保持停止以便恢复数据）", spec.BinName, out, err)
		}
	} else {
		client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
		if err != nil {
			return fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
		}
		defer client.Close()
		if _, _, err := client.RunCommandContext(ctx, "systemctl restart "+utils.QuoteShell(serviceUnit(spec, component))+" && systemctl is-active "+utils.QuoteShell(serviceUnit(spec, component))); err != nil {
			if errors.Is(err, ssh.ErrRemoteTerminationUnconfirmed) {
				return fmt.Errorf("restart %s: %w", spec.BinName, err)
			}
			if rollbackErr := rollbackRemote(client); rollbackErr != nil {
				return fmt.Errorf("%w: restart %s: %w; restore files failed: %w", ErrBinaryRecoveryUnconfirmed, spec.BinName, err, rollbackErr)
			}
			return fmt.Errorf("restart %s: %w（已恢复上一版本文件，服务保持停止以便恢复数据）", spec.BinName, err)
		}
	}

	if spec.HealthPath != "" {
		if healthErr := waitForBinaryHealth(ctx, component, spec.HealthPath); healthErr != nil {
			if isLocal {
				if rollbackErr := rollbackLocal(); rollbackErr != nil {
					return fmt.Errorf("%w: health check failed: %w; restore files failed: %w", ErrBinaryRecoveryUnconfirmed, healthErr, rollbackErr)
				}
			} else {
				client, connectErr := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
				if connectErr != nil {
					return fmt.Errorf("%w: health check failed: %w; recovery connection failed: %v", ErrBinaryRecoveryUnconfirmed, healthErr, connectErr)
				}
				rollbackErr := rollbackRemote(client)
				client.Close()
				if rollbackErr != nil {
					return fmt.Errorf("%w: health check failed: %w; restore files failed: %w", ErrBinaryRecoveryUnconfirmed, healthErr, rollbackErr)
				}
			}
			return fmt.Errorf("health check failed after upgrade: %w（已恢复上一版本文件，服务保持停止以便恢复数据）", healthErr)
		}
	}

	// Success: drop the backup copy.
	if isLocal {
		if spec.FullDir {
			_ = os.RemoveAll(installDirFor(spec, component) + ".bak")
		} else {
			_ = os.Remove(systemdExecPath(spec, component) + ".bak")
			_ = os.Remove(systemdExecPath(spec, component) + ".failed")
		}
	} else {
		client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
		if err == nil {
			defer client.Close()
			installDir := installDirFor(spec, component)
			if spec.FullDir {
				if _, _, err := client.RunCommandContext(ctx, "rm -rf "+utils.QuoteShell(installDir+".bak")); err != nil {
					return fmt.Errorf("cleanup previous install tree: %w", err)
				}
			} else {
				if _, _, err := client.RunCommandContext(ctx, "rm -f "+utils.QuoteShell(systemdExecPath(spec, component)+".bak")+" "+utils.QuoteShell(systemdExecPath(spec, component)+".failed")); err != nil {
					return fmt.Errorf("cleanup previous binary: %w", err)
				}
			}
		}
	}

	component.Version = newVersion
	if component.ServiceName == "" {
		component.ServiceName = serviceUnit(spec, component)
	}
	component.InternalURL = fmt.Sprintf("http://%s:%d", host.IP, component.Port)
	return nil
}

// ApplyUnit regenerates the systemd unit from the component's current spec
// (port, data dir, custom args), writes it to the host and restarts the
// service. Used when deploy parameters change after deployment - a plain
// restart would keep running the old ExecStart line.
func (d *BinaryDeployer) ApplyUnit(ctx context.Context, component *models.Component) error {
	if component == nil {
		return fmt.Errorf("component is required")
	}
	if err := ValidatePrometheusParameterVersion(component, component.Version); err != nil {
		return err
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	unit := systemdUnit(spec, component)
	// Operate on the component's real unit: adopted components keep their
	// original unit name (ServiceName), freshly deployed ones use the
	// platform unit name. Writing the platform unit for an adopted component
	// would create a duplicate service that fights the original for the port.
	unitFile := serviceUnit(spec, component)
	isLocal := component.Host.IP == "127.0.0.1" || component.Host.IP == "localhost" || component.Host.ConnectionType == "local"

	if isLocal {
		if err := writeEnvironmentFileLocal(spec, component); err != nil {
			return fmt.Errorf("write protected environment file: %w", err)
		}
		if err := os.WriteFile("/etc/systemd/system/"+unitFile+".service", []byte(unit), 0644); err != nil {
			return fmt.Errorf("write unit: %w", err)
		}
		if err := activateLocalSystemdUnit(ctx, unitFile); err != nil {
			return fmt.Errorf("apply unit %s: %w", spec.BinName, err)
		}
		return d.HealthCheck(ctx, component)
	}

	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username,
		component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
	}
	defer client.Close()
	if err := writeEnvironmentFileRemote(client, spec, component); err != nil {
		return fmt.Errorf("write protected remote environment file: %w", err)
	}
	if _, _, err := client.RunCommand("cat > " + utils.QuoteShell("/etc/systemd/system/"+unitFile+".service") + " <<'EOF'\n" + unit + "EOF\n" + remoteSystemdActivation(unitFile)); err != nil {
		return fmt.Errorf("apply unit: %w", err)
	}
	return d.HealthCheck(ctx, component)
}

// serviceUnit returns the systemd unit to operate on: adopted components keep
// their original unit name, freshly deployed ones use the platform unit name.
func serviceUnit(spec BinarySpec, comp *models.Component) string {
	if comp != nil && comp.ServiceName != "" {
		return CanonicalServiceName(comp.ServiceName)
	}
	if comp != nil && comp.ID != 0 && comp.LastDeployedAt == nil {
		return "promlifecycle-" + InstanceKey(comp)
	}
	return unitName(spec)
}

// CanonicalServiceName returns the suffix-free systemd identity.
func CanonicalServiceName(name string) string {
	return strings.TrimSuffix(strings.TrimSpace(name), ".service")
}

func pathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func binaryArtifacts(spec BinarySpec, component *models.Component) []string {
	binary := systemdExecPath(spec, component)
	if spec.FullDir {
		binary = installDirFor(spec, component)
	}
	paths := []string{binary, environmentFilePath(spec, component)}
	if spec.HasConfig {
		path := filepath.Join(spec.ConfigDir, spec.ConfigFile)
		if spec.ConfigDir == filepath.Join(instanceConfigRoot, InstanceKey(component)) {
			path = spec.ConfigDir
		}
		paths = append(paths, path)
	}
	if data := effectiveDataDir(spec, component); data != "" {
		paths = append(paths, data)
	}
	return paths
}

func (d *BinaryDeployer) guardBinaryArtifacts(ctx context.Context, component *models.Component, spec BinarySpec) error {
	paths := binaryArtifacts(spec, component)
	if !utils.SafePathSegment(serviceUnit(spec, component)) {
		return fmt.Errorf("invalid systemd identity %q", component.ServiceName)
	}
	if spec.FullDir && !utils.SafeInstallPath(installDirFor(spec, component)) {
		return fmt.Errorf("full-tree installation %s is not a dedicated managed directory; migrate it before replacing or removing the tree", installDirFor(spec, component))
	}
	if d.db != nil {
		var siblings []models.Component
		if err := d.db.Where("host_id = ? AND id <> ?", component.HostID, component.ID).Find(&siblings).Error; err != nil {
			return err
		}
		for _, sibling := range siblings {
			// Removal retains runtime identity for history, but releases ownership.
			// Every other state, including stopped/error or unknown, still owns it.
			if sibling.Status == "removed" {
				continue
			}
			other, err := binarySpecForComponent(&sibling)
			if err != nil {
				continue
			}
			if serviceUnit(spec, component) == serviceUnit(other, &sibling) {
				return fmt.Errorf("unit %s is also owned by component %d; resolve shared ownership before continuing", serviceUnit(spec, component), sibling.ID)
			}
			for _, a := range paths {
				for _, b := range binaryArtifacts(other, &sibling) {
					if pathsOverlap(a, b) {
						return fmt.Errorf("artifact %s overlaps component %d (%s); select isolated paths or release shared ownership first", a, sibling.ID, sibling.Name)
					}
				}
			}
		}
	}
	return GuardHostArtifactReferences(ctx, component.Host, serviceUnit(spec, component), 0, paths)
}

// GuardHostArtifactReferences checks stopped systemd units and live executable
// references before mutation. It is shared with discovery deletion; conflicts
// are returned before stopping the selected instance.
func GuardHostArtifactReferences(ctx context.Context, host models.Host, unit string, pid int, paths []string) error {
	var script strings.Builder
	script.WriteString("set -eu\nunit=" + utils.QuoteShell(CanonicalServiceName(unit)+".service") + "\nskip=" + fmt.Sprint(pid) + "\n")
	script.WriteString(`for artifact in`)
	for _, path := range paths {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return fmt.Errorf("unsafe artifact path %q", path)
		}
		script.WriteString(" " + utils.QuoteShell(path))
	}
	script.WriteString(`; do
resolved=$(readlink -f -- "$artifact") || resolved="$artifact"
for file in /etc/systemd/system/*.service /usr/lib/systemd/system/*.service /lib/systemd/system/*.service; do
 [ -f "$file" ] || continue
 [ "${file##*/}" = "$unit" ] && continue
 content=$(cat "$file") || exit 1
 case "$content" in *"$artifact"*|*"$resolved"*) printf 'artifact %s referenced by unit %s\n' "$artifact" "$file"; exit 23;; esac
done
for proc in /proc/[0-9]*; do
 [ "${proc##*/}" = "$skip" ] && continue
 exe=$(readlink -f "$proc/exe") || continue
 case "$exe" in "$resolved"|"$resolved"/*)
  cgroup=$(cat "$proc/cgroup") || exit 1
  case "$cgroup" in *"/$unit") continue;; esac
  printf 'artifact %s referenced by PID %s\n' "$artifact" "${proc##*/}"; exit 23;;
 esac
done
done
`)
	var output string
	var err error
	if ssh.IsLocalHost(host.IP, host.ConnectionType) {
		var data []byte
		data, err = exec.CommandContext(ctx, "sh", "-c", script.String()).CombinedOutput()
		output = string(data)
	} else {
		var client *ssh.Client
		client, err = ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
		if err != nil {
			return err
		}
		defer client.Close()
		var stderr string
		output, stderr, err = client.RunCommand(script.String())
		output += stderr
	}
	if err != nil {
		return fmt.Errorf("shared artifact safety check failed: %s; preserve the artifact and migrate dependent instances to separate paths before retrying: %w", strings.TrimSpace(output), err)
	}
	return nil
}

// DetectComponent checks whether the requested port is already occupied.
func (d *BinaryDeployer) DetectComponent(ctx context.Context, component *models.Component) (bool, string) {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return false, err.Error()
	}
	port := component.Port
	if port == 0 {
		port = defaultPort(component.Type)
	}

	host := component.Host
	var out string
	if host.IP == "127.0.0.1" || host.IP == "localhost" || host.ConnectionType == "local" {
		b, err := exec.CommandContext(ctx, "ss", "-tln").Output()
		if err != nil {
			b, err = exec.CommandContext(ctx, "netstat", "-tln").Output()
		}
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, fmt.Sprintf(":%d ", port)) || strings.Contains(line, fmt.Sprintf(":%d\t", port)) {
					proc := ""
					if pb2, err2 := exec.CommandContext(ctx, "sh", "-c",
						fmt.Sprintf("ss -ltnp 2>/dev/null | grep ':%d '", port)).Output(); err2 == nil {
						proc = ssProcessName(string(pb2))
					}
					if proc == "" {
						proc = "未知进程"
					}
					return true, fmt.Sprintf("%s 端口 %d 已被占用（%s）", spec.BinName, port, proc)
				}
			}
		}
		return false, ""
	}

	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return false, ""
	}
	defer client.Close()
	if out, _, err = client.RunCommand(fmt.Sprintf("ss -ltnp 2>/dev/null | grep ':%d ' || netstat -tlnp 2>/dev/null | grep ':%d '", port, port)); err == nil && strings.TrimSpace(out) != "" {
		proc := ssProcessName(out)
		if proc == "" {
			proc = "未知进程"
		}
		return true, fmt.Sprintf("%s 端口 %d 已被占用（%s）", spec.BinName, port, proc)
	}
	return false, ""
}

// ssProcessName extracts the process name from an ss -ltnp line.
func ssProcessName(out string) string {
	if u := strings.Index(out, "users:(("); u >= 0 {
		if q := strings.Index(out[u:], "\""); q >= 0 {
			start := u + q + 1
			if end := strings.Index(out[start:], "\""); end >= 0 {
				return out[start : start+end]
			}
		}
	}
	return ""
}

// Remove stops the component and removes its files on the target host.
func (d *BinaryDeployer) Remove(ctx context.Context, component *models.Component) error {
	return d.remove(ctx, component)
}

func (d *BinaryDeployer) RemoveKeepData(ctx context.Context, component *models.Component) error {
	if err := d.ValidateRemoveKeepData(ctx, component); err != nil {
		return err
	}
	return d.remove(ctx, component)
}

func (d *BinaryDeployer) remove(ctx context.Context, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	if err := d.guardBinaryArtifacts(ctx, component, spec); err != nil {
		return err
	}
	unit := serviceUnit(spec, component)
	artifact := systemdExecPath(spec, component)
	recursive := spec.FullDir && utils.SafeInstallPath(installDirFor(spec, component))
	if spec.FullDir && !recursive {
		return fmt.Errorf("installation %s is not an exclusively owned tree; migrate to a dedicated directory before uninstall", installDirFor(spec, component))
	}
	if recursive {
		artifact = installDirFor(spec, component)
	}
	installDir := installDirFor(spec, component)
	ownedInstall := !recursive && installDir == filepath.Join("/opt/promlifecycle/components", InstanceKey(component))
	paths := []string{"/etc/systemd/system/" + unit + ".service", environmentFilePath(spec, component)}
	// Legacy/adopted config directories may contain other services' files.
	// Only the ID-owned config subtree can be recursively removed.
	ownedConfig := spec.HasConfig && spec.ConfigDir == filepath.Join(instanceConfigRoot, InstanceKey(component))
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		for _, verb := range []string{"stop", "disable"} {
			if out, err := exec.CommandContext(ctx, "systemctl", verb, unit).CombinedOutput(); err != nil {
				return fmt.Errorf("%s %s: %s: %w", verb, unit, out, err)
			}
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if recursive {
			if err := os.RemoveAll(artifact); err != nil {
				return err
			}
		} else if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
			return err
		}
		if ownedInstall {
			if err := os.Remove(installDir); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("remove empty installation directory: %w", err)
			}
		}
		if ownedConfig {
			if err := os.RemoveAll(spec.ConfigDir); err != nil {
				return err
			}
		}
		if output, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("daemon-reload after removal: %s: %w", output, err)
		}
		return verifyLocalBinaryRemoval(unit, artifact, component.Port)
	}
	host := component.Host
	client, err := ssh.NewClientContext(ctx, host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return err
	}
	defer client.Close()
	command := "systemctl stop " + utils.QuoteShell(unit) + " && systemctl disable " + utils.QuoteShell(unit)
	for _, path := range paths {
		command += " && rm -f " + utils.QuoteShell(path)
	}
	if recursive {
		command += " && rm -rf " + utils.QuoteShell(artifact)
	} else {
		command += " && rm -f " + utils.QuoteShell(artifact)
	}
	if ownedInstall {
		command += " && { test ! -d " + utils.QuoteShell(installDir) +
			" || rmdir --ignore-fail-on-non-empty -- " + utils.QuoteShell(installDir) + "; }"
	}
	if ownedConfig {
		command += " && rm -rf " + utils.QuoteShell(spec.ConfigDir)
	}
	command += " && systemctl daemon-reload && ! systemctl is-active --quiet " + utils.QuoteShell(unit) +
		" && ! systemctl is-enabled --quiet " + utils.QuoteShell(unit) + " && test ! -e " + utils.QuoteShell(artifact)
	if component.Port > 0 {
		command += " && test -z \"$(ss -ltnH '( sport = :" + fmt.Sprint(component.Port) + " )')\""
	}
	if _, _, err := client.RunCommandContext(ctx, command); err != nil {
		return fmt.Errorf("remove or verify component: %w", err)
	}
	return nil
}

func verifyLocalBinaryRemoval(unit, artifactPath string, port int) error {
	if exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil {
		return fmt.Errorf("remove verification failed: unit %s is still active", unit)
	}
	if exec.Command("systemctl", "is-enabled", "--quiet", unit).Run() == nil {
		return fmt.Errorf("remove verification failed: unit %s is still enabled", unit)
	}
	if artifactPath != "" {
		if _, err := os.Stat(artifactPath); !os.IsNotExist(err) {
			return fmt.Errorf("remove verification failed: artifact still exists %s", artifactPath)
		}
	}
	if port > 0 {
		output, err := exec.Command("ss", "-ltnH", fmt.Sprintf("sport = :%d", port)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("remove verification failed: inspect port %d: %s: %w", port, strings.TrimSpace(string(output)), err)
		}
		if strings.TrimSpace(string(output)) != "" {
			return fmt.Errorf("remove verification failed: port %d is still occupied", port)
		}
	}
	return nil
}

// Restart restarts the component via systemd.
func (d *BinaryDeployer) Restart(ctx context.Context, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	binName := serviceUnit(spec, component)
	host := component.Host
	if host.IP == "127.0.0.1" || host.IP == "localhost" || host.ConnectionType == "local" {
		out, err := exec.CommandContext(ctx, "systemctl", "restart", binName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("restart %s: %s: %w", binName, out, err)
		}
		return d.HealthCheck(ctx, component)
	}

	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
	}
	defer client.Close()

	if _, _, err := client.RunCommand("systemctl restart " + utils.QuoteShell(binName) + " && systemctl is-active " + utils.QuoteShell(binName)); err != nil {
		return fmt.Errorf("restart %s: %w", binName, err)
	}
	return d.HealthCheck(ctx, component)
}

func (d *BinaryDeployer) Start(ctx context.Context, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	unit := serviceUnit(spec, component)
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		for _, args := range [][]string{{"enable", unit}, {"start", unit}, {"is-enabled", "--quiet", unit}, {"is-active", "--quiet", unit}} {
			if output, commandErr := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); commandErr != nil {
				return fmt.Errorf("systemctl %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(output)), commandErr)
			}
		}
		return d.HealthCheck(ctx, component)
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return err
	}
	defer client.Close()
	quoted := utils.QuoteShell(unit)
	if _, _, err := client.RunCommand("systemctl enable " + quoted + " && systemctl start " + quoted + " && systemctl is-enabled --quiet " + quoted + " && systemctl is-active --quiet " + quoted); err != nil {
		return fmt.Errorf("start and verify unit: %w", err)
	}
	return d.HealthCheck(ctx, component)
}

func (d *BinaryDeployer) Stop(ctx context.Context, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	unit := serviceUnit(spec, component)
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		if output, commandErr := exec.CommandContext(ctx, "systemctl", "stop", unit).CombinedOutput(); commandErr != nil {
			return fmt.Errorf("stop unit: %s: %w", strings.TrimSpace(string(output)), commandErr)
		}
		if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() == nil {
			return fmt.Errorf("unit %s is still active after stop", unit)
		}
		return nil
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return err
	}
	defer client.Close()
	quoted := utils.QuoteShell(unit)
	if _, _, err := client.RunCommand("systemctl stop " + quoted + " && ! systemctl is-active --quiet " + quoted); err != nil {
		return fmt.Errorf("stop and verify unit: %w", err)
	}
	return nil
}

// GetLogs returns recent component logs via journalctl.
func (d *BinaryDeployer) GetLogs(ctx context.Context, component *models.Component, tailLines int) ([]string, error) {
	if tailLines <= 0 {
		tailLines = 100
	}
	binName := component.ServiceName
	if binName == "" {
		if spec, err := binarySpecForComponent(component); err == nil {
			binName = unitName(spec)
		} else {
			binName = component.Name
		}
	}

	host := component.Host
	if host.IP == "127.0.0.1" || host.IP == "localhost" || host.ConnectionType == "local" {
		out, err := exec.CommandContext(ctx, "journalctl", "-u", binName, "-n", fmt.Sprintf("%d", tailLines), "--no-pager").Output()
		if err != nil {
			return nil, fmt.Errorf("journalctl: %w", err)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
	}

	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
	}
	defer client.Close()

	out, _, err := client.RunCommand(fmt.Sprintf("journalctl -u %s -n %d --no-pager", utils.QuoteShell(binName), tailLines))
	if err != nil {
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	return strings.Split(strings.TrimSpace(out), "\n"), nil
}
