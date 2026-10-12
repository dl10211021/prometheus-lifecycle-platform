package deployer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gopkg.in/yaml.v3"
)

type DockerDeployer struct {
	basePath string
}

func NewDockerDeployer(basePath string) *DockerDeployer {
	if basePath == "" {
		basePath = "/opt/promlifecycle"
	}
	return &DockerDeployer{basePath: basePath}
}

func (d *DockerDeployer) Init() error {
	for _, dir := range []string{
		filepath.Join(d.basePath, "compose"),
		filepath.Join(d.basePath, "configs"),
		filepath.Join(d.basePath, "data"),
	} {
		os.MkdirAll(dir, 0755)
	}
	exec.CommandContext(context.Background(), "docker", "network", "create", "promlifecycle").CombinedOutput()
	return nil
}

func dockerHostIsLocal(component *models.Component) bool {
	return ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType)
}

func (d *DockerDeployer) runHostCommand(ctx context.Context, component *models.Component, program string, args ...string) (string, error) {
	if dockerHostIsLocal(component) {
		output, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("%s: %s: %w", program, strings.TrimSpace(string(output)), err)
		}
		return string(output), nil
	}
	if component.Host.IsWindows {
		return "", fmt.Errorf("远程 Windows Docker 主机暂不支持；请使用 Linux SSH 主机")
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username,
		component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("SSH 连接远程 Docker 主机失败: %w", err)
	}
	defer client.Close()
	command := utils.QuoteShell(program)
	for _, arg := range args {
		command += " " + utils.QuoteShell(arg)
	}
	stdout, stderr, err := client.RunCommand(command)
	if err != nil {
		return stdout, fmt.Errorf("%s: %s: %w", program, strings.TrimSpace(stderr), err)
	}
	return stdout, nil
}

func (d *DockerDeployer) runHostShell(ctx context.Context, component *models.Component, command string) (string, error) {
	if dockerHostIsLocal(component) {
		output, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
		}
		return string(output), nil
	}
	if component.Host.IsWindows {
		return "", fmt.Errorf("远程 Windows Docker 主机暂不支持；请使用 Linux SSH 主机")
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username,
		component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("SSH 连接远程 Docker 主机失败: %w", err)
	}
	defer client.Close()
	stdout, stderr, err := client.RunCommand(command)
	if err != nil {
		return stdout, fmt.Errorf("%s: %w", strings.TrimSpace(stderr), err)
	}
	return stdout, nil
}

func (d *DockerDeployer) writeHostFile(ctx context.Context, component *models.Component, path string, content []byte, mode int64) error {
	if dockerHostIsLocal(component) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		return os.WriteFile(path, content, os.FileMode(mode))
	}
	if _, err := d.runHostShell(ctx, component, "mkdir -p "+utils.QuoteShell(filepath.Dir(path))); err != nil {
		return err
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username,
		component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return fmt.Errorf("SSH 连接远程 Docker 主机失败: %w", err)
	}
	defer client.Close()
	return client.CopyFile(bytes.NewReader(content), path, mode, int64(len(content)))
}

func (d *DockerDeployer) hostPathExists(ctx context.Context, component *models.Component, path string) bool {
	_, err := d.runHostShell(ctx, component, "test -e "+utils.QuoteShell(path))
	return err == nil
}

func (d *DockerDeployer) ensureImageOnHost(ctx context.Context, component *models.Component, image string) error {
	if _, err := d.runHostCommand(ctx, component, "docker", "image", "inspect", image); err == nil {
		return nil
	}
	_, err := d.runHostCommand(ctx, component, "docker", "pull", image)
	return err
}

func (d *DockerDeployer) dockerRunArgs(ctx context.Context, component *models.Component, image string) []string {
	args := []string{"run", "-d", "--name", component.ServiceName, "--restart", "unless-stopped", "--network", "promlifecycle"}
	args = append(args, "-p", fmt.Sprintf("%d:%d", component.Port, containerPort(component.Type)))
	configVolume := component.ConfigPath + ":" + containerConfigPath(component.Type) + ":ro"
	dataVolume := component.DataPath + ":" + containerDataPath(component.Type)
	switch component.Type {
	case models.ComponentGrafana:
		args = append(args, "-v", dataVolume)
		provisioning := filepath.Join(d.basePath, "grafana", safeName(component.Cluster.Name), "provisioning")
		if d.hostPathExists(ctx, component, provisioning) {
			args = append(args, "-v", provisioning+":/etc/grafana/provisioning:ro")
		}
	case models.ComponentNodeExporter:
		args = append(args, "--privileged", "--pid=host",
			"-v", "/:/host:ro,rslave", "-v", "/proc:/host/proc:ro",
			"-v", "/sys:/host/sys:ro", "-v", configVolume)
	default:
		args = append(args, "-v", configVolume, "-v", dataVolume)
	}
	if component.Type == models.ComponentPromtail {
		args = append(args, "-v", "/var/log:/var/log:ro")
	}
	if component.ResourceLimits != "" {
		var limits struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		}
		if json.Unmarshal([]byte(component.ResourceLimits), &limits) == nil {
			if limits.CPU != "" {
				args = append(args, "--cpus", limits.CPU)
			}
			if limits.Memory != "" {
				args = append(args, "--memory", normalizeMemory(limits.Memory))
			}
		}
	}
	if component.CustomEnv != "" {
		var environment map[string]string
		if json.Unmarshal([]byte(component.CustomEnv), &environment) == nil {
			for key, value := range environment {
				args = append(args, "-e", key+"="+value)
			}
		}
	}
	if component.ExtraVolumes != "" {
		var volumes []struct {
			Src string `json:"src"`
			Dst string `json:"dst"`
		}
		if json.Unmarshal([]byte(component.ExtraVolumes), &volumes) == nil {
			for _, volume := range volumes {
				if volume.Src != "" && volume.Dst != "" {
					args = append(args, "-v", volume.Src+":"+volume.Dst)
				}
			}
		}
	}
	args = append(args, image)
	if command := d.getCommand(component.Type); command != "" {
		args = append(args, strings.Fields(command)...)
	}
	return args
}

func (d *DockerDeployer) runContainer(ctx context.Context, component *models.Component, image string) (string, error) {
	args := d.dockerRunArgs(ctx, component, image)
	output, err := d.runHostCommand(ctx, component, "docker", args...)
	if err == nil || component.Type != models.ComponentNodeExporter ||
		!strings.Contains(output+" "+err.Error(), "not a shared or slave mount") {
		return output, err
	}

	// WSL2 and some restricted Linux hosts do not expose / as a shared/slave
	// mount. Preserve the rootfs visibility node_exporter needs, but retry
	// without propagation; the mount remains read-only.
	_, _ = d.runHostCommand(ctx, component, "docker", "rm", "-f", component.ServiceName)
	for index := range args {
		if args[index] == "/:/host:ro,rslave" {
			args[index] = "/:/host:ro"
		}
	}
	log.Printf("[DockerDeployer] Root mount propagation unavailable for %s; retrying with read-only rootfs mount", component.Name)
	return d.runHostCommand(ctx, component, "docker", args...)
}

func (d *DockerDeployer) Deploy(ctx context.Context, component *models.Component, cfg interface{}) error {
	if !dockerHostIsLocal(component) && component.Host.IsWindows {
		return fmt.Errorf("远程 Windows Docker 主机暂不支持；请改用 Linux SSH 主机")
	}
	if component.Image == "" {
		component.Image = defaultImage(component)
	}
	if component.Port == 0 {
		component.Port = defaultPort(component.Type)
	}
	clusterName := safeName(component.Cluster.Name)
	componentName := safeName(component.Name)
	composePath, configPath, dataPath := d.resolvePaths(clusterName, componentName, component.Host.IsWindows)
	if component.DataPathOverride != "" {
		dataPath = component.DataPathOverride
	}
	component.ComposePath = composePath
	component.ConfigPath = configPath
	component.DataPath = dataPath
	component.ServiceName = clusterName + "_" + componentName
	component.InternalURL = "http://" + net.JoinHostPort(componentEndpointHost(component.Host), strconv.Itoa(component.Port))

	prepare := "mkdir -p " + utils.QuoteShell(filepath.Dir(composePath)) + " " +
		utils.QuoteShell(configPath) + " " + utils.QuoteShell(dataPath)
	if component.Type == models.ComponentPrometheus || component.Type == models.ComponentGrafana {
		prepare += " && chmod 0777 " + utils.QuoteShell(dataPath)
	}
	if component.Type == models.ComponentGrafana {
		prepare += " && chmod 0777 " + utils.QuoteShell(configPath)
	}
	if _, err := d.runHostShell(ctx, component, prepare); err != nil {
		return fmt.Errorf("准备远程 Docker 目录: %w", err)
	}
	if cfg != nil {
		cfgBytes, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal config: %w", err)
		}
		if err := d.writeHostFile(ctx, component,
			filepath.Join(configPath, defaultConfigFile(component.Type)), cfgBytes, 0644); err != nil {
			return fmt.Errorf("write config on Docker host: %w", err)
		}
	}
	composeBytes, err := yaml.Marshal(map[string]interface{}{
		"version": "3.8",
		"services": map[string]interface{}{component.ServiceName: map[string]interface{}{
			"image": component.Image,
		}},
	})
	if err != nil {
		return fmt.Errorf("marshal compose: %w", err)
	}
	if err := d.writeHostFile(ctx, component, composePath, composeBytes, 0644); err != nil {
		return fmt.Errorf("write compose on Docker host: %w", err)
	}
	if _, err := d.runHostShell(ctx, component,
		"docker network inspect promlifecycle >/dev/null 2>&1 || docker network create promlifecycle"); err != nil {
		return fmt.Errorf("prepare Docker network: %w", err)
	}
	if err := d.ensureImageOnHost(ctx, component, component.Image); err != nil {
		return fmt.Errorf("ensure image: %w", err)
	}
	_, _ = d.runHostCommand(ctx, component, "docker", "stop", component.ServiceName)
	_, _ = d.runHostCommand(ctx, component, "docker", "rm", component.ServiceName)
	output, err := d.runContainer(ctx, component, component.Image)
	if err != nil {
		return fmt.Errorf("docker run: %s: %w", strings.TrimSpace(output), err)
	}
	time.Sleep(3 * time.Second)
	return nil
}

func (d *DockerDeployer) Restart(ctx context.Context, component *models.Component) error {
	output, err := d.runHostCommand(ctx, component, "docker", "restart", component.ServiceName)
	if err != nil {
		return fmt.Errorf("restart: %s: %w", strings.TrimSpace(output), err)
	}
	return nil
}

func (d *DockerDeployer) Upgrade(ctx context.Context, component *models.Component, newVersion string) error {
	oldImage := component.Image
	newImage := dockerImageRepository(component.Image) + ":" + newVersion
	backupPath, err := d.backupComponentConfig(ctx, component)
	if err != nil {
		log.Printf("[DockerDeployer] Warning: failed to backup config before upgrade: %v", err)
	} else {
		log.Printf("[DockerDeployer] Config backed up to %s", backupPath)
	}
	if err := d.ensureImageOnHost(ctx, component, newImage); err != nil {
		return fmt.Errorf("ensure image: %w", err)
	}
	_, _ = d.runHostCommand(ctx, component, "docker", "stop", component.ServiceName)
	_, _ = d.runHostCommand(ctx, component, "docker", "rm", component.ServiceName)
	output, runErr := d.runContainer(ctx, component, newImage)
	if runErr != nil {
		log.Printf("[DockerDeployer] Upgrade failed, attempting rollback from %s to %s", newImage, oldImage)
		if rollbackErr := d.rollbackUpgrade(ctx, component, oldImage, backupPath); rollbackErr != nil {
			return fmt.Errorf("upgrade failed: %s: %w; rollback failed: %v", strings.TrimSpace(output), runErr, rollbackErr)
		}
		return fmt.Errorf("upgrade failed: %s: %w (rolled back to %s)", strings.TrimSpace(output), runErr, oldImage)
	}
	component.Image = newImage
	component.Version = newVersion
	log.Printf("[DockerDeployer] Upgraded %s from %s to %s, config preserved", component.Name, oldImage, newImage)
	return nil
}

func dockerImageRepository(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		image = image[:colon]
	}
	return image
}

// backupComponentConfig creates the backup on the same host that owns the
// component, so remote upgrades never copy an unrelated local directory.
func (d *DockerDeployer) backupComponentConfig(ctx context.Context, component *models.Component) (string, error) {
	backupDir := filepath.Join(d.basePath, "backups")
	backupName := fmt.Sprintf("%s-%s-%s", safeName(component.Name), safeName(component.Version), time.Now().Format("20060102-150405"))
	backupPath := filepath.Join(backupDir, backupName)
	command := "mkdir -p " + utils.QuoteShell(backupPath)
	if component.ConfigPath != "" {
		command += " && cp -a " + utils.QuoteShell(component.ConfigPath) + " " +
			utils.QuoteShell(filepath.Join(backupPath, "config"))
	}
	if _, err := d.runHostShell(ctx, component, command); err != nil {
		return "", fmt.Errorf("backup config on Docker host: %w", err)
	}
	metadata := map[string]string{
		"component_name": component.Name, "component_type": string(component.Type),
		"cluster_name": component.Cluster.Name, "previous_image": component.Image,
		"previous_version": component.Version, "backup_time": time.Now().Format(time.RFC3339),
	}
	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", err
	}
	if err := d.writeHostFile(ctx, component, filepath.Join(backupPath, "metadata.json"), metadataJSON, 0644); err != nil {
		return "", err
	}
	return backupPath, nil
}

// rollbackUpgrade rebuilds the old container with the exact same argument
// builder as deploy/upgrade, restoring env, limits, all volumes and Grafana
// provisioning rather than merely restarting the image.
func (d *DockerDeployer) rollbackUpgrade(ctx context.Context, component *models.Component, oldImage, backupPath string) error {
	_, _ = d.runHostCommand(ctx, component, "docker", "stop", component.ServiceName)
	_, _ = d.runHostCommand(ctx, component, "docker", "rm", component.ServiceName)
	output, err := d.runContainer(ctx, component, oldImage)
	if err != nil {
		log.Printf("[DockerDeployer] Rollback failed: %s", strings.TrimSpace(output))
		return err
	}
	log.Printf("[DockerDeployer] Successfully rolled back %s to %s (backup %s)", component.Name, oldImage, backupPath)
	return nil
}

func (d *DockerDeployer) Delete(ctx context.Context, component *models.Component) error {
	_, _ = d.runHostCommand(ctx, component, "docker", "stop", component.ServiceName)
	_, _ = d.runHostCommand(ctx, component, "docker", "rm", component.ServiceName)
	if component.ComposePath != "" {
		if _, err := d.runHostShell(ctx, component, "rm -f "+utils.QuoteShell(component.ComposePath)); err != nil {
			return err
		}
	}
	return nil
}

func (d *DockerDeployer) HealthCheck(ctx context.Context, component *models.Component) (*models.HealthStatus, error) {
	endpoint := healthEndpoint(component.Type)
	if endpoint == "" {
		endpoint = "/-/healthy"
	}
	url := "http://" + net.JoinHostPort(component.Host.IP, strconv.Itoa(component.Port)) + endpoint
	cmd := exec.CommandContext(ctx, "curl", "-sf", "-m", "5", url)
	status := "unhealthy"
	if cmd.Run() == nil {
		status = "healthy"
	}
	return &models.HealthStatus{Status: status, ComponentID: component.ID, Component: component.Name, Type: component.Type, Timestamp: time.Now()}, nil
}

func (d *DockerDeployer) GetLogs(ctx context.Context, component *models.Component, tailLines int) ([]string, error) {
	output, err := d.runHostCommand(ctx, component, "docker", "logs", "--tail", strconv.Itoa(tailLines), component.ServiceName)
	if err != nil {
		return nil, err
	}
	return strings.Split(output, "\n"), nil
}

func (d *DockerDeployer) Update(ctx context.Context, component *models.Component, cfg interface{}) error {
	return d.Restart(ctx, component)
}

func (d *DockerDeployer) Rollback(ctx context.Context, component *models.Component) error {
	if component.Image == "" {
		return fmt.Errorf("rollback image is empty")
	}
	_, _ = d.runHostCommand(ctx, component, "docker", "stop", component.ServiceName)
	_, _ = d.runHostCommand(ctx, component, "docker", "rm", component.ServiceName)
	output, err := d.runContainer(ctx, component, component.Image)
	if err != nil {
		return fmt.Errorf("rollback docker run: %s: %w", strings.TrimSpace(output), err)
	}
	return nil
}

func (d *DockerDeployer) Scale(ctx context.Context, component *models.Component, replicas int32) error {
	if replicas < 0 {
		return fmt.Errorf("replicas must be >= 0")
	}
	serviceName := component.ServiceName
	if serviceName == "" {
		serviceName = safeName(component.Name)
	}

	if component.ComposePath == "" {
		return fmt.Errorf("scale requires docker compose (component must have ComposePath set)")
	}

	output, err := d.runHostCommand(ctx, component, "docker", "compose",
		"-f", component.ComposePath, "up", "-d",
		"--scale", fmt.Sprintf("%s=%d", serviceName, replicas), "--no-recreate")
	if err != nil {
		return fmt.Errorf("scale failed: %w\noutput: %s", err, output)
	}
	return nil
}

type DeploymentStatus struct{}

func (d *DockerDeployer) GetStatus(ctx context.Context, component *models.Component) (*DeploymentStatus, error) {
	return nil, nil
}

func (d *DockerDeployer) getCommand(t models.ComponentType) string {
	cp := containerConfigPath(t)
	dp := containerDataPath(t)
	switch t {
	case models.ComponentPrometheus:
		return "--config.file=" + cp + "/prometheus.yml --storage.tsdb.path=" + dp + " --web.enable-lifecycle"
	case models.ComponentAlertmanager:
		return "--config.file=" + cp + "/alertmanager.yml --storage.path=" + dp
	case models.ComponentLoki:
		return "-config.file=" + cp + "/loki.yml"
	case models.ComponentPromtail:
		return "-config.file=" + cp + "/promtail.yml"
	case models.ComponentNodeExporter:
		return "--path.rootfs=/host"
	case models.ComponentBlackbox:
		return "--config.file=" + cp + "/blackbox.yml"
	}
	return ""
}

// SafeName normalizes a cluster/component name into a safe directory/file
// segment: lowercase, separators collapsed to '-', traversal segments rejected.
func SafeName(name string) string {
	name = strings.ToLower(name)
	name = strings.NewReplacer(" ", "-", "_", "-", "/", "-", "\\", "-").Replace(name)
	name = strings.Trim(name, "-")
	if name == "" || name == "." || name == ".." {
		return "component"
	}
	return name
}

func safeName(name string) string { return SafeName(name) }

func containerPort(t models.ComponentType) int {
	ports := map[models.ComponentType]int{
		models.ComponentPrometheus: 9090, models.ComponentGrafana: 3000,
		models.ComponentAlertmanager: 9093, models.ComponentLoki: 3100,
		models.ComponentPromtail: 9080, models.ComponentNodeExporter: 9100,
		models.ComponentPushgateway: 9091, models.ComponentBlackbox: 9115,
	}
	if p, ok := ports[t]; ok {
		return p
	}
	return 8080
}

func containerConfigPath(t models.ComponentType) string { return "/etc/" + string(t) }
func containerDataPath(t models.ComponentType) string   { return "/var/lib/" + string(t) }

func defaultConfigFile(t models.ComponentType) string {
	switch t {
	case models.ComponentPrometheus:
		return "prometheus.yml"
	case models.ComponentAlertmanager:
		return "alertmanager.yml"
	case models.ComponentLoki:
		return "loki.yml"
	case models.ComponentPromtail:
		return "promtail.yml"
	case models.ComponentBlackbox:
		return "blackbox.yml"
	default:
		return string(t) + ".yml"
	}
}

func defaultImage(component *models.Component) string {
	images := map[models.ComponentType]string{
		models.ComponentPrometheus: "prom/prometheus", models.ComponentGrafana: "grafana/grafana",
		models.ComponentAlertmanager: "prom/alertmanager", models.ComponentLoki: "grafana/loki",
		models.ComponentPromtail: "grafana/promtail", models.ComponentNodeExporter: "prom/node-exporter",
		models.ComponentPushgateway: "prom/pushgateway", models.ComponentBlackbox: "prom/blackbox-exporter",
	}
	base, ok := images[component.Type]
	if !ok {
		return string(component.Type)
	}
	if component.Version != "" {
		return base + ":" + component.Version
	}
	return base + ":latest"
}

func defaultPort(t models.ComponentType) int { return containerPort(t) }

func healthEndpoint(t models.ComponentType) string {
	endpoints := map[models.ComponentType]string{
		models.ComponentPrometheus: "/-/healthy", models.ComponentGrafana: "/api/health",
		models.ComponentAlertmanager: "/-/healthy", models.ComponentLoki: "/ready",
		models.ComponentNodeExporter: "/-/healthy", models.ComponentBlackbox: "/-/healthy",
	}
	return endpoints[t]
}

// resolvePaths returns the compose, config, and data paths based on host OS
// For Windows hosts, it uses WSL-compatible paths via /mnt/c/ or /wsl$/
func (d *DockerDeployer) resolvePaths(clusterName, componentName string, isWindows bool) (composePath, configPath, dataPath string) {
	if isWindows {
		// Windows paths mapped through WSL (assumes WSL2 with Docker Desktop)
		// Using /mnt/c/PromLifecycle as the base directory on Windows
		base := "/mnt/c/PromLifecycle"
		return filepath.Join(base, "compose", clusterName, componentName+".yaml"),
			filepath.Join(base, "configs", clusterName, componentName),
			filepath.Join(base, "data", clusterName, componentName)
	}
	return filepath.Join(d.basePath, "compose", clusterName, componentName+".yaml"),
		filepath.Join(d.basePath, "configs", clusterName, componentName),
		filepath.Join(d.basePath, "data", clusterName, componentName)
}

// normalizeMemory converts memory values to Docker-compatible format
// e.g., "128Mi" -> "128m", "1Gi" -> "1g", "512M" -> "512m"
func normalizeMemory(mem string) string {
	mem = strings.TrimSpace(mem)
	if len(mem) < 2 {
		return mem
	}
	// Normalize Ki/Mi/Gi/Ti -> k/m/g/t
	switch strings.ToLower(mem[len(mem)-2:]) {
	case "ki":
		return mem[:len(mem)-2] + "k"
	case "mi":
		return mem[:len(mem)-2] + "m"
	case "gi":
		return mem[:len(mem)-2] + "g"
	case "ti":
		return mem[:len(mem)-2] + "t"
	}
	// Normalize K/M/G/T -> k/m/g/t
	switch strings.ToUpper(mem[len(mem)-1:]) {
	case "K":
		return mem[:len(mem)-1] + "k"
	case "M":
		return mem[:len(mem)-1] + "m"
	case "G":
		return mem[:len(mem)-1] + "g"
	case "T":
		return mem[:len(mem)-1] + "t"
	}
	return mem
}
