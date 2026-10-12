package handlers

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/promlifecycle/platform/core/deployer"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

// A single remote command captures listeners and process identity. argv is kept
// internal: it can contain credentials and must never be returned as evidence.
const hostDiscoveryCommand = `set -e
printf 'LISTENERS\n'
ss -H -ltnp
printf 'PROCESSES\n'
for proc in /proc/[0-9]*; do
  [ -r "$proc/cmdline" ] || continue
  exe=$(readlink "$proc/exe") || continue
  case "${exe##*/}" in
    prometheus|alertmanager|node_exporter|blackbox_exporter|pushgateway|grafana|grafana-server|loki|promtail|thanos) ;;
    *) continue ;;
  esac
  printf 'PROC\t%s\t%s\t' "${proc##*/}" "$exe"
  base64 -w0 < "$proc/cmdline" 2>/dev/null || true
  printf '\t'
  base64 -w0 < "$proc/cgroup" 2>/dev/null || true
  printf '\t'
  base64 -w0 < "$proc/stat" 2>/dev/null || true
  printf '\n'
done`

var listenerPIDPattern = regexp.MustCompile(`pid=(\d+)`)

type hostProcessEvidence struct {
	component services.DiscoveredComponent
	args      []string
}

func discoveredBinaryType(path string) string {
	switch filepath.Base(path) {
	case "grafana", "grafana-server":
		return "grafana"
	case "prometheus", "alertmanager", "node_exporter", "blackbox_exporter", "pushgateway", "loki", "promtail", "thanos":
		return filepath.Base(path)
	default:
		return ""
	}
}

func parseHostDiscovery(host models.Host, snapshot string) []hostProcessEvidence {
	ports := make(map[int]map[int]bool)
	lines := strings.Split(snapshot, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "PROC\t") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "LISTEN" {
			continue
		}
		_, rawPort, err := net.SplitHostPort(fields[3])
		if err != nil {
			index := strings.LastIndex(fields[3], ":")
			if index < 0 {
				continue
			}
			rawPort = fields[3][index+1:]
		}
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		for _, match := range listenerPIDPattern.FindAllStringSubmatch(line, -1) {
			pid, _ := strconv.Atoi(match[1])
			if ports[pid] == nil {
				ports[pid] = make(map[int]bool)
			}
			ports[pid][port] = true
		}
	}
	result := make([]hostProcessEvidence, 0)
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 6 || fields[0] != "PROC" {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		componentType := discoveredBinaryType(fields[2])
		if err != nil || pid <= 0 || componentType == "" {
			continue
		}
		argv, err := base64.StdEncoding.DecodeString(fields[3])
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(argv), "\x00"), "\x00")
		if len(args) == 0 || args[0] == "" {
			continue
		}
		cgroup, err := base64.StdEncoding.DecodeString(fields[4])
		if err != nil {
			continue
		}
		unit := ""
		for _, part := range strings.FieldsFunc(string(cgroup), func(r rune) bool { return r == '/' || r == '\n' }) {
			if strings.HasSuffix(part, ".service") && !strings.ContainsAny(part, " \t:") {
				unit = part
			}
		}
		stat, err := base64.StdEncoding.DecodeString(fields[5])
		if err != nil {
			continue
		}
		startTime := ""
		if index := strings.LastIndex(string(stat), ")"); index >= 0 {
			statFields := strings.Fields(string(stat)[index+1:])
			if len(statFields) > 19 {
				startTime = statFields[19]
			}
		}
		if startTime == "" {
			continue
		}
		for port := range ports[pid] {
			identity := fmt.Sprintf("%d:%s:%d:%s:%d:%s", host.ID, componentType, pid, startTime, port, fields[2])
			hash := sha256.Sum256([]byte(identity))
			item := services.DiscoveredComponent{Type: componentType, Name: componentType, Port: port, PID: pid, Binary: fields[2], Unit: unit, Runtime: "process", Health: "unknown", HealthReason: "已确认监听进程；接管时再次核对身份与组件协议", Confidence: 0.9, ConfidenceTag: "process+listener", Fingerprint: hex.EncodeToString(hash[:]), Evidence: []string{"监听端口与 PID 对应", "读取实际 /proc/exe 身份"}}
			if unit != "" {
				item.Runtime = "systemd"
			}
			item.Config = argvValue(args[1:], "--config.file", "-config.file", "--config", "-config")
			item.Data = argvValue(args[1:], "--storage.tsdb.path", "--storage.path", "--data.path", "--data-path")
			result = append(result, hostProcessEvidence{component: item, args: args[1:]})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].component.Port != result[j].component.Port {
			return result[i].component.Port < result[j].component.Port
		}
		return result[i].component.PID < result[j].component.PID
	})
	return result
}

func argvValue(args []string, flags ...string) string {
	for index, arg := range args {
		for _, flag := range flags {
			if strings.HasPrefix(arg, flag+"=") {
				return strings.TrimPrefix(arg, flag+"=")
			}
			if arg == flag && index+1 < len(args) {
				return args[index+1]
			}
		}
	}
	return ""
}

func (h *Handler) discoverHost(host models.Host) ([]hostProcessEvidence, error) {
	if host.ConnectionType == "winrm" {
		return nil, fmt.Errorf("此发现入口仅支持 Linux；Windows 请使用 Exporter 接入流程")
	}
	output, err := runHostCommand(&host, hostDiscoveryCommand)
	if err != nil {
		return nil, fmt.Errorf("读取主机监听进程失败: %w", err)
	}
	items := parseHostDiscovery(host, output)
	var managed []models.Component
	if err := h.db.Where("host_id = ? AND status <> ?", host.ID, "removed").Find(&managed).Error; err != nil {
		return nil, err
	}
	for i := range items {
		for _, component := range managed {
			if component.Port == items[i].component.Port {
				items[i].component.ComponentID = component.ID
				if !sameDiscoveredIdentity(component, items[i].component) {
					items[i].component.OwnershipConflict = true
					break
				}
				items[i].component.AlreadyOwned = "platform"
				var adoption models.ComponentAdoption
				if h.db.Where("component_id = ?", component.ID).First(&adoption).Error == nil {
					items[i].component.AlreadyOwned = adoption.Ownership
				}
				break
			}
		}
	}
	return items, nil
}

func validateAdoptionProtocol(host *models.Host, componentType string, port int) error {
	path := "/metrics"
	marker := map[string]string{"prometheus": "prometheus_build_info", "alertmanager": "alertmanager_build_info", "node_exporter": "node_exporter_build_info", "blackbox_exporter": "blackbox_exporter_build_info", "pushgateway": "pushgateway_build_info", "loki": "loki_build_info", "promtail": "promtail_build_info", "thanos": "thanos_build_info"}[componentType]
	if componentType == "grafana" {
		path = "/api/health"
		marker = "\"database\""
	}
	if marker == "" {
		return fmt.Errorf("组件类型没有已知的身份验证协议")
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + path
	output, err := runHostCommand(host, "curl -fsS --max-time 5 -- "+utils.QuoteShell(url))
	if err != nil || !strings.Contains(output, marker) {
		return fmt.Errorf("监听进程未通过 %s 身份检查（%s）；请核对类型、端口和访问限制", componentType, path)
	}
	return nil
}

// A port alone cannot establish ownership after a process or service is replaced.
func sameDiscoveredIdentity(component models.Component, item services.DiscoveredComponent) bool {
	binary, unit := component.BinaryPath, component.ServiceName
	if component.DeployType == "binary" {
		var err error
		binary, unit, err = deployer.BinaryRuntimeIdentity(&component)
		if err != nil {
			return false
		}
	}
	return string(component.Type) == item.Type && component.Port == item.Port && binary != "" && binary == item.Binary && deployer.CanonicalServiceName(unit) == deployer.CanonicalServiceName(item.Unit)
}

// Read protocol metadata only. Discovered executables may be owned by another
// OS user and must never be executed with the platform's SSH privileges.
func discoveredProtocolVersion(host *models.Host, kind string, port int) string {
	if port < 1 || port > 65535 {
		return ""
	}
	metric := map[string]string{"prometheus": "prometheus_build_info", "alertmanager": "alertmanager_build_info", "node_exporter": "node_exporter_build_info", "blackbox_exporter": "blackbox_exporter_build_info", "pushgateway": "pushgateway_build_info", "loki": "loki_build_info", "promtail": "promtail_build_info", "thanos": "thanos_build_info"}[kind]
	path := "/metrics"
	if kind == "grafana" {
		path = "/api/health"
	} else if metric == "" {
		return ""
	}
	endpoint := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + path
	output, err := runHostCommand(host, "curl -fsS --max-time 5 --max-filesize 1048576 -- "+utils.QuoteShell(endpoint))
	if err != nil {
		return ""
	}
	if kind == "grafana" {
		var body struct {
			Version string `json:"version"`
		}
		if json.Unmarshal([]byte(output), &body) == nil && utils.ValidVersion(body.Version) {
			return body.Version
		}
		return ""
	}
	versionLabel := regexp.MustCompile(`(?:\{|,)\s*version="([^"]+)"`)
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		match := versionLabel.FindStringSubmatch(line)
		if len(match) == 2 && utils.ValidVersion(match[1]) {
			return match[1]
		}
	}
	return ""
}
