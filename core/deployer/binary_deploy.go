package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

func componentEndpointHost(host models.Host) string {
	if ssh.IsLocalHost(host.IP, host.ConnectionType) {
		return "127.0.0.1"
	}
	return host.IP
}

// copyFileWithRetry uploads a file over SSH, re-opening the source reader for
// each attempt so transient connection resets (EOF mid-payload) do not fail
// the whole deployment.
func copyFileWithRetry(client *ssh.Client, src func() (io.Reader, int64, error), dst string, mode int64) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		r, size, err := src()
		if err != nil {
			return err
		}
		err = client.CopyFile(r, dst, mode, size)
		if closer, ok := r.(io.Closer); ok {
			closer.Close()
		}
		if err == nil {
			return nil
		}
		lastErr = err
		log.Printf("[Deploy] upload %s failed (attempt %d/3): %v, retrying", dst, attempt, err)
		time.Sleep(time.Duration(attempt) * 3 * time.Second)
	}
	return lastErr
}

// Deploy installs and starts the component on the target host via SSH.
func (d *BinaryDeployer) Deploy(ctx context.Context, component *models.Component) error {
	if component == nil {
		return fmt.Errorf("component is required")
	}
	if err := ValidatePrometheusParameterVersion(component, component.Version); err != nil {
		return err
	}
	reportProgress(d.progress, "prechecking", "检查组件规格、目标主机和部署参数", 5)
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	if component.Port == 0 && spec.DefaultPort > 0 {
		component.Port = spec.DefaultPort
	}
	if err := d.guardBinaryArtifacts(ctx, component, spec); err != nil {
		return err
	}
	// Record the selected identity before any runtime mutation. A failed deploy
	// or server restart must not recompute a different destination on retry.
	component.InstallPath = installDirFor(spec, component)
	component.DataPath = effectiveDataDir(spec, component)
	if spec.HasConfig {
		component.ConfigPath = filepath.Join(spec.ConfigDir, spec.ConfigFile)
	}
	component.ServiceName = serviceUnit(spec, component)
	component.BinaryPath = systemdExecPath(spec, component)
	if d.db != nil && component.ID != 0 {
		if err := d.db.Model(component).Updates(map[string]interface{}{
			"install_path": component.InstallPath, "data_path": component.DataPath,
			"config_path": component.ConfigPath, "service_name": component.ServiceName,
			"binary_path": component.BinaryPath,
		}).Error; err != nil {
			return err
		}
	}

	reportProgress(d.progress, "resolving_package", "解析并校验离线安装包", 15)
	binaryPath, err := d.extractBinary(ctx, spec, component.Version)
	if err != nil {
		return fmt.Errorf("extract %s binary: %w", spec.BinName, err)
	}
	reportProgress(d.progress, "installing", "安装二进制、配置文件和 systemd 服务", 40)

	isLocal := component.Host.IP == "127.0.0.1" || component.Host.IP == "localhost" || component.Host.ConnectionType == "local"
	if isLocal {
		err = d.deployLocal(ctx, component, spec, binaryPath)
	} else {
		err = d.deployRemote(ctx, component, spec, binaryPath)
	}
	if err != nil {
		return err
	}
	reportProgress(d.progress, "verifying", "验证服务状态和健康端点", 85)
	if err := d.HealthCheck(ctx, component); err != nil {
		return err
	}
	reportProgress(d.progress, "integrating", "组件部署验证通过，准备同步平台集成", 95)
	return nil
}

func waitForBinaryHealth(ctx context.Context, component *models.Component, healthPath string) error {
	host := component.Host.IP
	if host == "" || component.Host.ConnectionType == "local" {
		host = "127.0.0.1"
	}
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(component.Port)) + healthPath
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	stable := 0
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				if serviceErr := verifyBinaryServiceActive(ctx, component); serviceErr == nil {
					stable++
					if stable >= 2 {
						return nil
					}
				} else {
					lastErr = serviceErr
					stable = 0
				}
			} else {
				lastErr = fmt.Errorf("HTTP %d", response.StatusCode)
				stable = 0
			}
		} else {
			lastErr = err
			stable = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("health check %s failed: %w", healthPath, lastErr)
		case <-ticker.C:
		}
	}
}

func verifyBinaryServiceActive(ctx context.Context, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	unit := serviceUnit(spec, component)
	host := component.Host
	if host.IP == "127.0.0.1" || host.IP == "localhost" || host.ConnectionType == "local" {
		for attempt := 0; attempt < 2; attempt++ {
			if out, err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).CombinedOutput(); err != nil {
				return fmt.Errorf("systemd service %s is not active: %s", unit, strings.TrimSpace(string(out)))
			}
			if attempt == 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
		}
		return nil
	}
	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return fmt.Errorf("verify remote systemd service: %w", err)
	}
	defer client.Close()
	command := "systemctl is-active --quiet " + utils.QuoteShell(unit) +
		" && sleep 1 && systemctl is-active --quiet " + utils.QuoteShell(unit)
	if _, _, err := client.RunCommand(command); err != nil {
		return fmt.Errorf("remote systemd service %s is not stably active: %w", component.ServiceName, err)
	}
	return nil
}

// installBinaryLocal copies the binary to /usr/local/bin and writes the systemd unit.
func (d *BinaryDeployer) deployLocal(ctx context.Context, component *models.Component, spec BinarySpec, binaryPath string) error {
	if spec.FullDir {
		// FullDir components (grafana): install the whole extracted tree
		if err := os.RemoveAll(installDirFor(spec, component)); err != nil {
			return fmt.Errorf("remove old install dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(installDirFor(spec, component)), 0755); err != nil {
			return fmt.Errorf("create parent dir: %w", err)
		}
		if err := copyTree(binaryPath, installDirFor(spec, component)); err != nil {
			return fmt.Errorf("install tree: %w", err)
		}
		if out, err := runChown(ctx, installDirFor(spec, component)); err != nil {
			return fmt.Errorf("chown install dir: %s: %w", out, err)
		}
	} else {
		installDir := installDirFor(spec, component)
		installPath := systemdExecPath(spec, component)
		if err := os.MkdirAll(installDir, 0755); err != nil {
			return fmt.Errorf("create install dir %s: %w", installDir, err)
		}
		tmpPath := installPath + ".promlifecycle-new"
		if err := copyLocalFile(binaryPath, tmpPath, 0755); err != nil {
			return fmt.Errorf("stage binary: %w", err)
		}
		if err := os.Rename(tmpPath, installPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("install binary: %w", err)
		}
	}
	// Write config file(s) if the component needs one
	if spec.HasConfig {
		cfgContent := defaultConfigForComponent(component)
		if _, err := os.Stat(filepath.Join(spec.ConfigDir, spec.ConfigFile)); err == nil {
			cfgContent = ""
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect existing config: %w", err)
		}
		if cfgContent != "" {
			if err := os.MkdirAll(spec.ConfigDir, 0755); err != nil {
				return fmt.Errorf("create config dir %s: %w", spec.ConfigDir, err)
			}
			if err := os.WriteFile(filepath.Join(spec.ConfigDir, spec.ConfigFile), []byte(cfgContent), 0644); err != nil {
				return fmt.Errorf("write config: %w", err)
			}
		}
		if component.Type == models.ComponentPrometheus {
			if err := os.MkdirAll(filepath.Join(spec.ConfigDir, "rules"), 0755); err != nil {
				return fmt.Errorf("create rules dir: %w", err)
			}
		}
	}
	if component.Type == models.ComponentGrafana {
		if err := d.writeGrafanaDatasourceLocal(component); err != nil {
			return err
		}
		if err := writeGrafanaDashboardLocal(component); err != nil {
			return fmt.Errorf("write Grafana heartbeat dashboard: %w", err)
		}
	}

	// Ensure the data directory exists and is writable by the service user
	dataDir := effectiveDataDir(spec, component)
	if dataDir != "" {
		if err := os.MkdirAll(dataDir, 0755); err != nil {
			return fmt.Errorf("create data dir %s: %w", dataDir, err)
		}
		if out, err := runChown(ctx, dataDir); err != nil {
			return fmt.Errorf("chown data dir: %s: %w", out, err)
		}
	}
	if err := validateInstalledConfigLocal(ctx, spec, component); err != nil {
		return err
	}
	if err := writeEnvironmentFileLocal(spec, component); err != nil {
		return fmt.Errorf("write protected environment file: %w", err)
	}

	unit := systemdUnit(spec, component)
	unitPath := "/etc/systemd/system/" + serviceUnit(spec, component) + ".service"
	if err := utils.AtomicWriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := activateLocalSystemdUnit(ctx, serviceUnit(spec, component)); err != nil {
		return fmt.Errorf("activate %s: %w", spec.BinName, err)
	}

	component.ServiceName = serviceUnit(spec, component)
	component.InternalURL = fmt.Sprintf("http://%s:%d", componentEndpointHost(component.Host), component.Port)
	if spec.HasConfig {
		component.ConfigPath = filepath.Join(spec.ConfigDir, spec.ConfigFile)
	}
	component.DataPath = dataDir
	component.InstallPath = installDirFor(spec, component)
	return nil
}

// deployRemote uploads the binary and config over SSH and installs a systemd unit.
func (d *BinaryDeployer) deployRemote(ctx context.Context, component *models.Component, spec BinarySpec, binaryPath string) error {
	host := component.Host
	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return fmt.Errorf("SSH 连接目标主机失败（请检查主机用户名/密码/端口，或先在主机管理里测试连接）: %w", err)
	}
	defer client.Close()

	if spec.FullDir {
		// FullDir components (grafana): pack the extracted tree, upload, extract on the host
		parent := filepath.Dir(binaryPath)
		dirName := filepath.Base(binaryPath)
		archive, err := os.CreateTemp("", "promlifecycle-tree-*.tar.gz")
		if err != nil {
			return err
		}
		tmpTar := archive.Name()
		archive.Close()
		defer os.Remove(tmpTar)
		tarCmd := exec.CommandContext(ctx, "tar", "czf", tmpTar, "-C", parent, dirName)
		if out, err := tarCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("pack install tree: %s: %w", out, err)
		}
		fi, err := os.Stat(tmpTar)
		if err != nil {
			return fmt.Errorf("stat packed tree: %w", err)
		}
		tf, err := os.Open(tmpTar)
		if err != nil {
			return err
		}
		defer tf.Close()
		remoteTar := "/tmp/" + filepath.Base(tmpTar)
		if err := client.CopyFile(tf, remoteTar, 0644, fi.Size()); err != nil {
			return fmt.Errorf("upload install tree: %w", err)
		}
		if _, _, err := client.RunCommand("rm -rf " + utils.QuoteShell(installDirFor(spec, component)) +
			" && " + extractTreeCommand(remoteTar, installDirFor(spec, component)) +
			" && rm -f " + utils.QuoteShell(remoteTar) + " && " + nobodyChownCmd(utils.QuoteShell(installDirFor(spec, component)))); err != nil {
			return fmt.Errorf("extract install tree: %w", err)
		}
	} else {
		// Ensure the install directory exists on the remote host first
		installDir := installDirFor(spec, component)
		if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(installDir)); err != nil {
			return fmt.Errorf("mkdir install dir: %w", err)
		}
		if err := copyFileWithRetry(client, func() (io.Reader, int64, error) {
			f, err := os.Open(binaryPath)
			if err != nil {
				return nil, 0, err
			}
			st, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, 0, err
			}
			return f, st.Size(), nil
		}, systemdExecPath(spec, component), 0755); err != nil {
			return fmt.Errorf("upload binary: %w", err)
		}
	}

	// Config files
	if spec.HasConfig {
		if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(spec.ConfigDir)); err != nil {
			return fmt.Errorf("mkdir config dir: %w", err)
		}
		cfgContent := defaultConfigForComponent(component)
		exists, _, err := client.RunCommand("if test -e " + utils.QuoteShell(filepath.Join(spec.ConfigDir, spec.ConfigFile)) + "; then printf exists; fi")
		if err != nil {
			return fmt.Errorf("inspect existing config: %w", err)
		}
		if strings.TrimSpace(exists) == "exists" {
			cfgContent = ""
		}
		if cfgContent != "" {
			if err := copyFileWithRetry(client, func() (io.Reader, int64, error) {
				return strings.NewReader(cfgContent), int64(len(cfgContent)), nil
			}, filepath.Join(spec.ConfigDir, spec.ConfigFile), 0644); err != nil {
				return fmt.Errorf("upload config: %w", err)
			}
		}
		if component.Type == models.ComponentPrometheus {
			if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(filepath.Join(spec.ConfigDir, "rules"))); err != nil {
				return fmt.Errorf("mkdir rules dir: %w", err)
			}
		}
	}
	if component.Type == models.ComponentGrafana {
		if err := d.writeGrafanaDatasourceRemote(client, component); err != nil {
			return err
		}
		if err := writeGrafanaDashboardRemote(client, component); err != nil {
			return fmt.Errorf("write remote Grafana heartbeat dashboard: %w", err)
		}
	}

	// Ensure the data directory exists and is writable by the service user
	dataDir := effectiveDataDir(spec, component)
	if dataDir != "" {
		if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(dataDir) + " && " + nobodyChownCmd(utils.QuoteShell(dataDir))); err != nil {
			return fmt.Errorf("prepare data dir: %w", err)
		}
	}
	if err := validateInstalledConfigRemote(client, spec, component); err != nil {
		return err
	}
	if err := writeEnvironmentFileRemote(client, spec, component); err != nil {
		return fmt.Errorf("write protected remote environment file: %w", err)
	}

	unit := systemdUnit(spec, component)
	if _, _, err := client.RunCommand("cat > " + utils.QuoteShell("/etc/systemd/system/"+serviceUnit(spec, component)+".service") + " <<'EOF'\n" + unit + "EOF\n" + remoteSystemdActivation(serviceUnit(spec, component))); err != nil {
		return fmt.Errorf("install systemd unit: %w", err)
	}

	component.ServiceName = serviceUnit(spec, component)
	component.InternalURL = fmt.Sprintf("http://%s:%d", componentEndpointHost(host), component.Port)
	if spec.HasConfig {
		component.ConfigPath = filepath.Join(spec.ConfigDir, spec.ConfigFile)
	}
	component.DataPath = dataDir
	component.InstallPath = installDirFor(spec, component)
	return nil
}

func extractTreeCommand(archive, destination string) string {
	return "mkdir -p " + utils.QuoteShell(destination) + " && tar xzf " +
		utils.QuoteShell(archive) + " --strip-components=1 -C " + utils.QuoteShell(destination)
}

func activateLocalSystemdUnit(ctx context.Context, unit string) error {
	steps := [][]string{
		{"daemon-reload"},
		{"enable", unit},
		{"restart", unit},
		{"is-enabled", "--quiet", unit},
		{"is-active", "--quiet", unit},
	}
	for _, args := range steps {
		if output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
		}
	}
	return nil
}

func remoteSystemdActivation(unit string) string {
	quoted := utils.QuoteShell(unit)
	return "systemctl daemon-reload && systemctl enable " + quoted +
		" && systemctl restart " + quoted +
		" && systemctl is-enabled --quiet " + quoted +
		" && systemctl is-active --quiet " + quoted
}

// unitName returns the systemd unit name for a component, prefixed to avoid
// clashing with pre-existing services of the same name on the target host.
func unitName(spec BinarySpec) string {
	return "promlifecycle-" + spec.BinName
}

// nobodyGroup resolves the primary group of the "nobody" user. It is "nogroup"
// on Debian/Ubuntu and "nobody" on RHEL-family systems, so hardcoding
// "nobody:nobody" breaks chown on Debian/Ubuntu.
func nobodyGroup() string {
	out, err := exec.Command("id", "-gn", "nobody").Output()
	if err == nil {
		if g := strings.TrimSpace(string(out)); g != "" {
			return g
		}
	}
	return "nobody"
}

// nobodyChownArgs returns the local chown command for the nobody user with the
// correct primary group.
func nobodyChownArgs(dir string) []string {
	return []string{"chown", "-R", "nobody:" + nobodyGroup(), dir}
}

// nobodyChownCmd returns a shell command that chowns a directory to the nobody
// user with its correct primary group (used in remote shell commands).
func nobodyChownCmd(dir string) string {
	return "chown -R nobody:$(id -gn nobody) " + dir
}

// runChown runs the resolved nobody chown command locally.
func runChown(ctx context.Context, dir string) ([]byte, error) {
	args := nobodyChownArgs(dir)
	return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
}

func installedConfigValidationArgs(spec BinarySpec, component *models.Component) []string {
	if component == nil {
		return nil
	}
	switch component.Type {
	case models.ComponentLoki:
		return []string{"-verify-config=true", "-config.file=" + filepath.Join(spec.ConfigDir, spec.ConfigFile), "-config.expand-env=true"}
	case models.ComponentBlackbox:
		return append(effectiveArgs(spec, component), "--config.check")
	default:
		return nil
	}
}

func validateInstalledConfigLocal(ctx context.Context, spec BinarySpec, component *models.Component) error {
	arguments := installedConfigValidationArgs(spec, component)
	if len(arguments) == 0 {
		return nil
	}
	binary := systemdExecPath(spec, component)
	output, err := exec.CommandContext(ctx, binary, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s 配置原生校验失败: %s: %w", component.Type, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func validateInstalledConfigRemote(client *ssh.Client, spec BinarySpec, component *models.Component) error {
	arguments := installedConfigValidationArgs(spec, component)
	if len(arguments) == 0 {
		return nil
	}
	var command strings.Builder
	command.WriteString(utils.QuoteShell(systemdExecPath(spec, component)))
	for _, argument := range arguments {
		command.WriteByte(' ')
		command.WriteString(utils.QuoteShell(argument))
	}
	stdout, stderr, err := client.RunCommand(command.String())
	if err != nil {
		return fmt.Errorf("%s 配置原生校验失败: %s: %w", component.Type, strings.TrimSpace(stdout+" "+stderr), err)
	}
	return nil
}

// installDirFor returns an isolated platform-owned installation directory.
// Every component gets its own subtree so an upgrade/uninstall cannot touch
// another component's binary.
func installDirFor(spec BinarySpec, comp *models.Component) string {
	if comp != nil && comp.InstallPath != "" {
		return comp.InstallPath
	}
	if comp != nil && comp.ID != 0 && comp.ServiceName == "" && comp.LastDeployedAt == nil {
		return filepath.Join("/opt/promlifecycle/components", InstanceKey(comp))
	}
	if spec.FullDir {
		return spec.InstallDir
	}
	componentName := string(spec.ComponentType)
	if componentName == "" {
		componentName = spec.BinName
	}
	return filepath.Join("/opt/promlifecycle/components", componentName)
}
func (d *BinaryDeployer) grafanaDatasourceContent(component *models.Component) string {
	if component == nil || d.db == nil {
		return ""
	}
	type datasource struct {
		Name, UID, Type, URL string
		Default              bool
	}
	datasources := make([]datasource, 0, 2)
	var prometheus models.Component
	if err := d.db.Preload("Host").
		Where("cluster_id = ? AND type = ? AND status = ?", component.ClusterID, models.ComponentPrometheus, models.StatusRunning).
		Order("id ASC").First(&prometheus).Error; err == nil {
		port := prometheus.Port
		if port == 0 {
			port = 9090
		}
		host := prometheus.Host.IP
		if prometheus.HostID == component.HostID {
			host = "127.0.0.1"
		}
		endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + models.ManagedRoutePrefix(&prometheus)
		datasources = append(datasources, datasource{Name: "Prometheus", UID: "promlifecycle-prometheus", Type: "prometheus", URL: endpoint, Default: true})
	}
	hasLoki := false
	var loki models.Component
	if err := d.db.Preload("Host").
		Where("cluster_id = ? AND type = ? AND status = ?", component.ClusterID, models.ComponentLoki, models.StatusRunning).
		Order("id ASC").First(&loki).Error; err == nil {
		port := loki.Port
		if port == 0 {
			port = 3100
		}
		host := loki.Host.IP
		if loki.HostID == component.HostID {
			host = "127.0.0.1"
		}
		datasources = append(datasources, datasource{Name: "Loki", UID: "promlifecycle-loki", Type: "loki", URL: "http://" + net.JoinHostPort(host, strconv.Itoa(port)), Default: len(datasources) == 0})
		hasLoki = true
	}
	if len(datasources) == 0 {
		return ""
	}
	var content strings.Builder
	content.WriteString("apiVersion: 1\ndatasources:\n")
	for _, item := range datasources {
		content.WriteString("  - name: " + item.Name + "\n")
		content.WriteString("    uid: " + item.UID + "\n")
		content.WriteString("    type: " + item.Type + "\n")
		content.WriteString("    access: proxy\n")
		content.WriteString("    url: " + item.URL + "\n")
		content.WriteString("    isDefault: " + strconv.FormatBool(item.Default) + "\n")
		content.WriteString("    editable: true\n")
		if item.Type == "prometheus" {
			content.WriteString("    jsonData:\n      timeInterval: 15s\n")
		}
	}
	if !hasLoki {
		content.WriteString("deleteDatasources:\n  - name: Loki\n    orgId: 1\n")
	}
	return content.String()
}

func (d *BinaryDeployer) writeGrafanaDatasourceLocal(component *models.Component) error {
	content := d.grafanaDatasourceContent(component)
	if content == "" {
		return nil
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	dir := filepath.Join(spec.ConfigDir, "provisioning", "datasources")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create Grafana datasource directory: %w", err)
	}
	if err := utils.AtomicWriteFile(filepath.Join(dir, "promlifecycle.yml"), []byte(content), 0644); err != nil {
		return fmt.Errorf("write Grafana Prometheus datasource: %w", err)
	}
	return nil
}

func (d *BinaryDeployer) writeGrafanaDatasourceRemote(client *ssh.Client, component *models.Component) error {
	content := d.grafanaDatasourceContent(component)
	if content == "" {
		return nil
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	dir := filepath.Join(spec.ConfigDir, "provisioning", "datasources")
	if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(dir)); err != nil {
		return fmt.Errorf("create remote Grafana datasource directory: %w", err)
	}
	if err := copyFileWithRetry(client, func() (io.Reader, int64, error) {
		return strings.NewReader(content), int64(len(content)), nil
	}, filepath.Join(dir, "promlifecycle.yml"), 0644); err != nil {
		return fmt.Errorf("upload Grafana Prometheus datasource: %w", err)
	}
	return nil
}

func grafanaOverviewDashboard() string {
	return `{"annotations":{"list":[]},"editable":true,"fiscalYearStartMonth":0,"graphTooltip":1,"id":null,"links":[],"liveNow":false,"panels":[{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"color":{"mode":"thresholds"},"thresholds":{"mode":"absolute","steps":[{"color":"red","value":null},{"color":"green","value":1}]}}},"gridPos":{"h":5,"w":6,"x":0,"y":0},"id":1,"options":{"reduceOptions":{"calcs":["lastNotNull"],"values":false}},"targets":[{"expr":"count(up)","refId":"A"}],"title":"监控目标总数","type":"stat"},{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"color":{"mode":"thresholds"},"thresholds":{"mode":"absolute","steps":[{"color":"red","value":null},{"color":"green","value":1}]}}},"gridPos":{"h":5,"w":6,"x":6,"y":0},"id":2,"options":{"reduceOptions":{"calcs":["lastNotNull"],"values":false}},"targets":[{"expr":"sum(up)","refId":"A"}],"title":"正常目标","type":"stat"},{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"color":{"mode":"thresholds"},"thresholds":{"mode":"absolute","steps":[{"color":"green","value":null},{"color":"red","value":1}]}}},"gridPos":{"h":5,"w":6,"x":12,"y":0},"id":3,"options":{"reduceOptions":{"calcs":["lastNotNull"],"values":false}},"targets":[{"expr":"sum(up == 0)","refId":"A"}],"title":"异常目标","type":"stat"},{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"unit":"percentunit"}},"gridPos":{"h":5,"w":6,"x":18,"y":0},"id":4,"options":{"reduceOptions":{"calcs":["lastNotNull"],"values":false}},"targets":[{"expr":"avg(up)","refId":"A"}],"title":"整体可用率","type":"stat"},{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"custom":{"drawStyle":"line","lineInterpolation":"smooth","lineWidth":2},"min":0,"max":1}},"gridPos":{"h":9,"w":12,"x":0,"y":5},"id":5,"targets":[{"expr":"up","legendFormat":"{{instance}} · {{job}}","refId":"A"}],"title":"目标在线状态","type":"timeseries"},{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"unit":"percent"}},"gridPos":{"h":9,"w":12,"x":12,"y":5},"id":6,"targets":[{"expr":"(1 - avg by(instance) (rate(node_cpu_seconds_total{mode=\"idle\"}[5m]))) * 100","legendFormat":"{{instance}}","refId":"A"}],"title":"主机 CPU 使用率","type":"timeseries"},{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"unit":"percent"}},"gridPos":{"h":9,"w":12,"x":0,"y":14},"id":7,"targets":[{"expr":"(1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) * 100","legendFormat":"{{instance}}","refId":"A"}],"title":"主机内存使用率","type":"timeseries"},{"datasource":{"type":"prometheus","uid":"promlifecycle-prometheus"},"fieldConfig":{"defaults":{"min":0,"max":1}},"gridPos":{"h":9,"w":12,"x":12,"y":14},"id":8,"targets":[{"expr":"probe_success","legendFormat":"{{instance}}","refId":"A"}],"title":"Blackbox 探测状态","type":"timeseries"}],"refresh":"15s","schemaVersion":39,"tags":["promlifecycle","heartbeat"],"templating":{"list":[]},"time":{"from":"now-1h","to":"now"},"timepicker":{},"timezone":"browser","title":"PromLifecycle Heartbeat Overview","uid":"promlifecycle-heartbeat","version":1,"weekStart":""}`
}

func writeGrafanaDashboardLocal(component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	dashboardDir := filepath.Join(spec.ConfigDir, "provisioning", "dashboards", "promlifecycle")
	dir := filepath.Dir(dashboardDir)
	if err := os.MkdirAll(dashboardDir, 0755); err != nil {
		return err
	}
	if err := utils.AtomicWriteFile(filepath.Join(dir, "promlifecycle.yml"), []byte(strings.ReplaceAll(config.GrafanaDashboardProvider(), config.GrafanaDashboardDir, dashboardDir)), 0644); err != nil {
		return err
	}
	return utils.AtomicWriteFile(filepath.Join(dashboardDir, "heartbeat.json"), []byte(grafanaOverviewDashboard()), 0644)
}

func writeGrafanaDashboardRemote(client *ssh.Client, component *models.Component) error {
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return err
	}
	dashboardDir := filepath.Join(spec.ConfigDir, "provisioning", "dashboards", "promlifecycle")
	dir := filepath.Dir(dashboardDir)
	if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(dashboardDir)); err != nil {
		return err
	}
	for path, content := range map[string]string{filepath.Join(dir, "promlifecycle.yml"): strings.ReplaceAll(config.GrafanaDashboardProvider(), config.GrafanaDashboardDir, dashboardDir), filepath.Join(dashboardDir, "heartbeat.json"): grafanaOverviewDashboard()} {
		if err := copyFileWithRetry(client, func() (io.Reader, int64, error) {
			return strings.NewReader(content), int64(len(content)), nil
		}, path, 0644); err != nil {
			return err
		}
	}
	return nil
}

// SyncGrafanaDatasource rewrites the managed Prometheus datasource and
// restarts Grafana through its existing systemd unit.
func (d *BinaryDeployer) SyncGrafanaDatasource(ctx context.Context, component *models.Component) error {
	if component == nil || component.Type != models.ComponentGrafana {
		return fmt.Errorf("Grafana component is required")
	}
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		if err := d.writeGrafanaDatasourceLocal(component); err != nil {
			return err
		}
		if err := writeGrafanaDashboardLocal(component); err != nil {
			return err
		}
	} else {
		client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
		if err != nil {
			return err
		}
		defer client.Close()
		if err := d.writeGrafanaDatasourceRemote(client, component); err != nil {
			return err
		}
		if err := writeGrafanaDashboardRemote(client, component); err != nil {
			return err
		}
	}
	return d.ApplyUnit(ctx, component)
}

func (d *BinaryDeployer) ReadRetainedEnvironment(ctx context.Context, component *models.Component) (string, error) {
	if component == nil {
		return "", nil
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return "", err
	}
	path := environmentFilePath(spec, component)
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		content, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return environmentFileJSON(content)
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return "", err
	}
	defer client.Close()
	stdout, _, err := client.RunCommand("if test -f " + utils.QuoteShell(path) + "; then cat " + utils.QuoteShell(path) + "; fi")
	if err != nil {
		return "", err
	}
	return environmentFileJSON([]byte(stdout))
}

func environmentFileJSON(content []byte) (string, error) {
	environment := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || !utils.ValidEnvironmentName(parts[0]) {
			return "", fmt.Errorf("invalid retained environment entry")
		}
		value, err := strconv.Unquote(parts[1])
		if err != nil {
			return "", fmt.Errorf("invalid retained environment value: %w", err)
		}
		environment[parts[0]] = value
	}
	if len(environment) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(environment)
	return string(encoded), err
}

// systemdExecPath returns the ExecStart binary path for a component.
func systemdExecPath(spec BinarySpec, comp *models.Component) string {
	if comp != nil && comp.BinaryPath != "" {
		return comp.BinaryPath
	}
	installDir := installDirFor(spec, comp)
	if spec.FullDir {
		return filepath.Join(installDir, "bin", spec.BinName)
	}
	return filepath.Join(installDir, spec.BinName)
}

func environmentFilePath(spec BinarySpec, component *models.Component) string {
	return filepath.Join("/etc/promlifecycle/environment", serviceUnit(spec, component)+".env")
}

func environmentFileContent(component *models.Component) string {
	if component == nil || component.CustomEnv == "" {
		return ""
	}
	var environment map[string]string
	if json.Unmarshal([]byte(component.CustomEnv), &environment) != nil {
		return ""
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		if utils.ValidEnvironmentName(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var content strings.Builder
	for _, key := range keys {
		content.WriteString(key)
		content.WriteByte('=')
		content.WriteString(strconv.Quote(environment[key]))
		content.WriteByte('\n')
	}
	return content.String()
}

func writeEnvironmentFileLocal(spec BinarySpec, component *models.Component) error {
	path := environmentFilePath(spec, component)
	content := environmentFileContent(component)
	if content == "" {
		_ = os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return utils.AtomicWriteFile(path, []byte(content), 0600)
}

func writeEnvironmentFileRemote(client *ssh.Client, spec BinarySpec, component *models.Component) error {
	path := environmentFilePath(spec, component)
	content := environmentFileContent(component)
	if content == "" {
		_, _, _ = client.RunCommand("rm -f " + utils.QuoteShell(path))
		return nil
	}
	if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(filepath.Dir(path)) + " && chmod 0700 " + utils.QuoteShell(filepath.Dir(path))); err != nil {
		return err
	}
	return copyFileWithRetry(client, func() (io.Reader, int64, error) {
		return strings.NewReader(content), int64(len(content)), nil
	}, path, 0600)
}

func systemdUnit(spec BinarySpec, comp *models.Component) string {
	environmentDirective := ""
	if environmentFileContent(comp) != "" {
		environmentDirective = "EnvironmentFile=-" + environmentFilePath(spec, comp) + "\n"
	}
	var serviceLines strings.Builder
	serviceLines.WriteString("NoNewPrivileges=true\nPrivateTmp=true\n")
	if len(spec.Capabilities) > 0 {
		capabilities := strings.Join(spec.Capabilities, " ")
		serviceLines.WriteString("AmbientCapabilities=" + capabilities + "\n")
		serviceLines.WriteString("CapabilityBoundingSet=" + capabilities + "\n")
	}
	if comp != nil && comp.ResourceLimits != "" {
		var limits struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		}
		if json.Unmarshal([]byte(comp.ResourceLimits), &limits) == nil {
			if cores, err := strconv.ParseFloat(limits.CPU, 64); err == nil && cores > 0 && cores <= 64 {
				serviceLines.WriteString(fmt.Sprintf("CPUQuota=%.0f%%\n", cores*100))
			}
			if validSystemdMemoryLimit(limits.Memory) {
				serviceLines.WriteString("MemoryMax=" + limits.Memory + "\n")
			}
		}
	}
	return fmt.Sprintf(`[Unit]
Description=Prometheus %s (managed by Prometheus Lifecycle Platform)
After=network.target

[Service]
Type=simple
User=%s
%s%sExecStart=%s %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, spec.BinName, spec.User, environmentDirective, serviceLines.String(), systemdExecPath(spec, comp), systemdExecArguments(effectiveArgs(spec, comp)))
}

func systemdExecArguments(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		argument = strings.ReplaceAll(argument, `\`, `\\`)
		argument = strings.ReplaceAll(argument, `"`, `\"`)
		argument = strings.ReplaceAll(argument, `%`, `%%`)
		quoted[index] = `"` + argument + `"`
	}
	return strings.Join(quoted, " ")
}

func validSystemdMemoryLimit(value string) bool {
	if value == "" || len(value) > 16 {
		return false
	}
	for i, r := range value {
		if r >= '0' && r <= '9' {
			continue
		}
		if i > 0 && strings.ContainsRune("KMGTPE", r) && i == len(value)-1 {
			continue
		}
		return false
	}
	return true
}

// copyLocalFile streams one regular file without buffering it in memory.
func copyLocalFile(sourcePath, targetPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// copyTree recursively copies a directory tree while rejecting links.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(sourcePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic link blocked while copying tree: %s", sourcePath)
		}
		rel, err := filepath.Rel(src, sourcePath)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if rel != "." && !utils.PathWithinBase(dst, target) {
			return fmt.Errorf("copy target escapes destination: %s", rel)
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type while copying tree: %s", sourcePath)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		source, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		defer source.Close()
		mode := info.Mode().Perm()
		if mode&0111 != 0 {
			mode = 0755
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, source)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
