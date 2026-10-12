package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	winrmclient "github.com/promlifecycle/platform/core/winrm"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

type deploymentPreflightCheck struct {
	Key      string      `json:"key"`
	Name     string      `json:"name"`
	Status   string      `json:"status"` // pass/warn/fail
	Blocking bool        `json:"blocking"`
	Message  string      `json:"message"`
	Evidence interface{} `json:"evidence,omitempty"`
}

type deploymentPreviewAction struct {
	Order  int    `json:"order"`
	Action string `json:"action"`
	Target string `json:"target"`
}

// PreflightComponentDeployment validates and probes a deployment without
// creating Component/Operation rows or writing anything to the target host.
func (h *Handler) PreflightComponentDeployment(c *gin.Context) {
	var request componentCreateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "部署预检输入无效: " + err.Error()})
		return
	}
	checks := make([]deploymentPreflightCheck, 0, 8)
	add := func(key, name, status, message string, blocking bool, evidence interface{}) {
		checks = append(checks, deploymentPreflightCheck{Key: key, Name: name, Status: status, Message: message, Blocking: blocking, Evidence: evidence})
	}

	var customDefinition *models.ComponentDefinition
	if !utils.IsValidComponentType(request.Type) {
		definition, err := FindEnabledComponentDefinition(h.db, request.Type)
		if err != nil {
			add("definition", "部署定义", "fail", "组件类型无效、已停用或没有部署定义", true, nil)
			c.JSON(http.StatusOK, deploymentPreflightResponse(false, request, models.DeploymentDefinition{}, models.Host{}, checks, nil))
			return
		}
		customDefinition = &definition
	}
	definition, err := h.normalizeDeploymentRequest(&request, customDefinition)
	if err != nil {
		add("schema", "参数 Schema", "fail", err.Error(), true, nil)
		c.JSON(http.StatusOK, deploymentPreflightResponse(false, request, definition, models.Host{}, checks, nil))
		return
	}
	add("schema", "参数 Schema", "pass", "必填项、类型、范围和选项均通过", true, safeDeploymentParameters(definition, &request))

	var host models.Host
	if request.HostID == nil || *request.HostID == 0 || h.db.First(&host, *request.HostID).Error != nil {
		add("host", "目标主机", "fail", "目标主机不存在", true, nil)
		c.JSON(http.StatusOK, deploymentPreflightResponse(false, request, definition, host, checks, nil))
		return
	}
	if err := validateDeploymentHost(definition, &request, &host); err != nil {
		add("host", "目标主机", "fail", err.Error(), true, gin.H{"host": host.Name, "ip": host.IP})
	} else {
		add("host", "目标主机", "pass", "主机操作系统与部署定义匹配", true, gin.H{"host": host.Name, "ip": host.IP, "os": host.OS, "connection_type": host.ConnectionType})
	}
	if err := h.preflightPrometheusParameterVersion(&request, &host, &checks); err != nil {
		c.JSON(http.StatusOK, deploymentPreflightResponse(false, request, definition, host, checks, nil))
		return
	}
	h.preflightDependencies(&request, &host, &checks)

	if request.DeployType == "target" {
		address := net.JoinHostPort(host.IP, strconv.Itoa(request.Port))
		connection, dialErr := net.DialTimeout("tcp", address, 5*time.Second)
		if connection != nil {
			_ = connection.Close()
		}
		if dialErr != nil {
			add("target", "外部目标", "warn", "目标端口当前不可连接；允许登记，但 Prometheus 将显示为 down", false, address)
		} else {
			add("target", "外部目标", "pass", "外部监控目标端口可连接", true, address)
		}
	} else {
		h.preflightPackage(&request, &definition, customDefinition, &checks)
		h.preflightHostAccess(&request, &definition, &host, &checks)
		h.preflightPaths(&request, &definition, &checks)
		h.preflightPort(&request, &host, &checks)
		h.preflightPromtailSources(&request, &host, &checks)
		h.preflightGrafanaCredentials(&request, &host, &checks)
	}

	passed := true
	for _, check := range checks {
		if check.Blocking && check.Status == "fail" {
			passed = false
			break
		}
	}
	actions := deploymentPreview(definition, request, host)
	response := deploymentPreflightResponse(passed, request, definition, host, checks, actions)
	if passed {
		credential, err := h.issueDeploymentPreflight(request, definition, customDefinition, checks, actions, inspectionActor(c))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "签发部署预检凭证失败: " + err.Error()})
			return
		}
		response["preflight_token"] = credential.Token
		response["preflight_expires_at"] = credential.ExpiresAt
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) preflightPackage(request *componentCreateRequest, definition *models.DeploymentDefinition, customDefinition *models.ComponentDefinition, checks *[]deploymentPreflightCheck) {
	formats := []string{"binary", "zip"}
	if definition.Platform == "windows" {
		formats = []string{"msi"}
	}
	versions := []string{request.Version}
	if normalized := strings.TrimPrefix(request.Version, "v"); normalized != request.Version {
		versions = append(versions, normalized)
	} else if normalized != "" {
		versions = append(versions, "v"+normalized)
	}
	var pkg models.Package
	err := h.db.Where("type = ? AND version IN ? AND format IN ?", request.Type, versions, formats).Order("created_at DESC").First(&pkg).Error
	if err != nil {
		if customDefinition != nil && customDefinition.PackageURL != "" {
			*checks = append(*checks, deploymentPreflightCheck{
				Key: "package", Name: "在线安装包", Status: "warn", Blocking: false,
				Message:  "离线包未入库；部署时将从组件定义的 HTTPS 地址下载，下载或归档校验失败会终止部署",
				Evidence: gin.H{"version": request.Version, "package_url": customDefinition.PackageURL},
			})
			return
		}
		*checks = append(*checks, deploymentPreflightCheck{Key: "package", Name: "离线安装包", Status: "fail", Blocking: true, Message: "所选版本没有已入库安装包"})
		return
	}
	packageRoot := filepath.Join(h.basePath, "packages")
	info, statErr := os.Lstat(pkg.StoragePath)
	if !utils.PathWithinBase(packageRoot, pkg.StoragePath) || statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		*checks = append(*checks, deploymentPreflightCheck{Key: "package", Name: "离线安装包", Status: "fail", Blocking: true, Message: "安装包文件不存在或不在受管仓库"})
		return
	}
	actualChecksum, checksumErr := calcFileSHA256(pkg.StoragePath)
	if checksumErr != nil || pkg.Checksum == "" || !strings.EqualFold(actualChecksum, pkg.Checksum) {
		*checks = append(*checks, deploymentPreflightCheck{Key: "package", Name: "离线安装包", Status: "fail", Blocking: true, Message: "安装包 SHA256 校验失败"})
		return
	}
	*checks = append(*checks, deploymentPreflightCheck{
		Key: "package", Name: "离线安装包", Status: "pass", Blocking: true,
		Message:  "安装包存在且 SHA256 校验通过",
		Evidence: gin.H{"package_id": pkg.ID, "version": pkg.Version, "format": pkg.Format, "size": info.Size(), "sha256": actualChecksum},
	})
}

func (h *Handler) preflightHostAccess(request *componentCreateRequest, definition *models.DeploymentDefinition, host *models.Host, checks *[]deploymentPreflightCheck) {
	if definition.Platform == "windows" {
		password, err := credentials.Decrypt(host.Password)
		if err != nil {
			*checks = append(*checks, deploymentPreflightCheck{Key: "access", Name: "远程权限", Status: "fail", Blocking: true, Message: "WinRM 凭据解密失败"})
			return
		}
		client, err := winrmclient.NewClientWithOptions(winrmclient.Options{Host: host.IP, Port: host.WinRMPort, Scheme: host.WinRMScheme, Username: host.Username, Password: password, SkipTLSVerify: host.WinRMSkipVerify})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err = client.RunPS(ctx, `$identity=[Security.Principal.WindowsIdentity]::GetCurrent();$principal=New-Object Security.Principal.WindowsPrincipal($identity);if(-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)){throw 'not administrator'};if(-not [Environment]::Is64BitOperatingSystem){throw 'not 64-bit'};Write-Output $identity.Name`)
		}
		if err != nil {
			*checks = append(*checks, deploymentPreflightCheck{Key: "access", Name: "远程权限", Status: "fail", Blocking: true, Message: "WinRM 连接、管理员权限或系统架构检查失败: " + err.Error()})
			return
		}
		*checks = append(*checks, deploymentPreflightCheck{Key: "access", Name: "远程权限", Status: "pass", Blocking: true, Message: "WinRM 可连接，账号具有管理员权限，系统为 64 位"})
		return
	}
	uid, err := runHostCommand(host, "id -u")
	if err != nil {
		*checks = append(*checks, deploymentPreflightCheck{Key: "access", Name: "远程权限", Status: "fail", Blocking: true, Message: "无法通过本机/SSH 执行权限检查: " + err.Error()})
		return
	}
	if strings.TrimSpace(uid) != "0" {
		*checks = append(*checks, deploymentPreflightCheck{Key: "access", Name: "远程权限", Status: "fail", Blocking: true, Message: "部署账号必须具有 root 权限", Evidence: strings.TrimSpace(uid)})
		return
	}
	*checks = append(*checks, deploymentPreflightCheck{Key: "access", Name: "远程权限", Status: "pass", Blocking: true, Message: "本机/SSH 连接正常，部署账号具有 root 权限"})
}

func (h *Handler) preflightPaths(request *componentCreateRequest, definition *models.DeploymentDefinition, checks *[]deploymentPreflightCheck) {
	if definition.Platform == "windows" {
		*checks = append(*checks, deploymentPreflightCheck{Key: "paths", Name: "部署路径", Status: "pass", Blocking: true, Message: `MSI 将安装到 C:\Program Files\windows_exporter`})
		return
	}
	if !utils.SafeInstallPath(request.InstallPath) {
		*checks = append(*checks, deploymentPreflightCheck{Key: "paths", Name: "部署路径", Status: "fail", Blocking: true, Message: "安装路径不符合安全规则"})
		return
	}
	if !utils.SafeDataPath(request.DataPathOverride) {
		*checks = append(*checks, deploymentPreflightCheck{Key: "paths", Name: "部署路径", Status: "fail", Blocking: true, Message: "数据路径不符合安全规则"})
		return
	}
	*checks = append(*checks, deploymentPreflightCheck{Key: "paths", Name: "部署路径", Status: "pass", Blocking: true, Message: "安装路径和数据路径符合受管目录规则", Evidence: gin.H{"install_path": request.InstallPath, "data_path": request.DataPathOverride}})
}

func (h *Handler) preflightDependencies(request *componentCreateRequest, host *models.Host, checks *[]deploymentPreflightCheck) {
	if request.Type == string(models.ComponentPrometheus) {
		evidence, err := services.CheckPrometheusDependents(h.db, host.ID, request.Version)
		if err != nil {
			*checks = append(*checks, deploymentPreflightCheck{Key: "dependent_thanos", Name: "已有组件兼容性", Status: "fail", Blocking: true, Message: err.Error(), Evidence: evidence})
		} else if len(evidence) > 0 {
			*checks = append(*checks, deploymentPreflightCheck{Key: "dependent_thanos", Name: "已有组件兼容性", Status: "pass", Blocking: true, Message: "同主机已有 Thanos 满足目标 Prometheus 版本的 Sidecar 兼容规则", Evidence: evidence})
		}
	}

	required := make(map[string]bool)
	for _, componentType := range request.RequiredComponents {
		required[componentType] = true
	}
	switch request.Type {
	case string(models.ComponentThanos):
		required[string(models.ComponentPrometheus)] = true
	case string(models.ComponentPromtail):
		required[string(models.ComponentLoki)] = true
	}
	plannedOrder := make(map[string]int, len(request.PlannedComponents))
	for index, componentType := range request.PlannedComponents {
		if _, duplicate := plannedOrder[componentType]; duplicate {
			*checks = append(*checks, deploymentPreflightCheck{Key: "dependency_plan", Name: "组件依赖", Status: "fail", Blocking: true, Message: "部署计划包含重复组件类型，无法确定依赖实例: " + componentType})
			return
		}
		plannedOrder[componentType] = index
	}
	if len(required) == 0 {
		return
	}
	currentIndex, currentPlanned := plannedOrder[request.Type]
	for componentType := range required {
		if componentType == request.Type || !utils.IsValidComponentType(componentType) {
			*checks = append(*checks, deploymentPreflightCheck{Key: "dependency_" + componentType, Name: "组件依赖", Status: "fail", Blocking: true, Message: "组件依赖定义无效: " + componentType})
			continue
		}
		var dependency models.Component
		dependencyIndex, dependencyPlanned := plannedOrder[componentType]
		found := false
		if dependencyPlanned {
			if !currentPlanned || dependencyIndex >= currentIndex {
				*checks = append(*checks, deploymentPreflightCheck{Key: "dependency_" + componentType, Name: "组件依赖", Status: "fail", Blocking: true, Message: componentType + " 必须在当前组件前部署，不能用现有实例替代计划中的未来版本检查"})
				continue
			}
			dependency.Version = strings.TrimSpace(request.PlannedVersions[componentType])
			if dependency.Version == "" {
				*checks = append(*checks, deploymentPreflightCheck{Key: "dependency_" + componentType, Name: "组件依赖", Status: "fail", Blocking: true, Message: "部署计划缺少依赖 " + componentType + " 的目标版本，请重新执行整栈预检"})
				continue
			}
			found = true
		} else {
			found = h.db.
				Where("host_id = ? AND type = ? AND status = ? AND deploy_type <> ?", host.ID, componentType, models.StatusRunning, "target").
				First(&dependency).Error == nil
		}
		if found {
			evidence := gin.H{"source": "running", "dependency_id": dependency.ID}
			message := componentType + " 已在目标主机运行"
			if dependencyPlanned {
				evidence = gin.H{"source": "planned", "version": dependency.Version}
				message = componentType + " 将在当前组件前部署；执行时会复核实际运行状态"
			}
			if request.Type == string(models.ComponentThanos) && componentType == string(models.ComponentPrometheus) {
				compatibility := thanosPrometheusCompatibility(request.Version, dependency.Version)
				evidence["compatibility"] = compatibility
				evidence["thanos_version"] = request.Version
				evidence["prometheus_version"] = dependency.Version
				if compatibility != "compatible" {
					message := "无法确认 Thanos 与 Prometheus 的版本兼容性；请核对完整的正式版本号，未知、预发布或未覆盖的主版本不能自动通过"
					if compatibility == "incompatible" {
						message = fmt.Sprintf("Thanos %s 不满足 Prometheus %s 的 Sidecar 兼容规则；Prometheus 3 需要 Thanos 0.37.0 或更高的正式 0.x 版本", request.Version, dependency.Version)
					}
					*checks = append(*checks, deploymentPreflightCheck{Key: "dependency_" + componentType, Name: "组件依赖", Status: "fail", Blocking: true, Message: message, Evidence: evidence})
					continue
				}
			}
			*checks = append(*checks, deploymentPreflightCheck{Key: "dependency_" + componentType, Name: "组件依赖", Status: "pass", Blocking: true, Message: message, Evidence: evidence})
			continue
		}
		*checks = append(*checks, deploymentPreflightCheck{Key: "dependency_" + componentType, Name: "组件依赖", Status: "fail", Blocking: true, Message: "目标主机缺少运行中的前置组件 " + componentType + "，且部署计划未在当前组件前包含它"})
	}
}

func thanosSupportsPrometheus(thanosVersion, prometheusVersion string) bool {
	return thanosPrometheusCompatibility(thanosVersion, prometheusVersion) == "compatible"
}

func thanosPrometheusCompatibility(thanosVersion, prometheusVersion string) string {
	return services.ThanosPrometheusCompatibility(thanosVersion, prometheusVersion)
}

func (h *Handler) preflightPromtailSources(request *componentCreateRequest, host *models.Host, checks *[]deploymentPreflightCheck) {
	if request.Type != string(models.ComponentPromtail) || request.StructuredParameters["file_log_enabled"] != true {
		return
	}
	pattern, _ := request.StructuredParameters["file_glob"].(string)
	matched := false
	var checkErr error
	if ssh.IsLocalHost(host.IP, host.ConnectionType) {
		matches, err := filepath.Glob(pattern)
		checkErr = err
		matched = len(matches) > 0
	} else {
		client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
		if err != nil {
			checkErr = err
		} else {
			defer client.Close()
			_, _, checkErr = client.RunCommand("compgen -G " + utils.QuoteShell(pattern) + " >/dev/null")
			matched = checkErr == nil
		}
	}
	if matched {
		*checks = append(*checks, deploymentPreflightCheck{Key: "log_source", Name: "日志采集源", Status: "pass", Blocking: true, Message: "文件日志路径至少匹配一个现有文件", Evidence: pattern})
		return
	}
	blocking := request.StructuredParameters["journal_enabled"] != true
	status := "warn"
	if blocking {
		status = "fail"
	}
	message := "文件日志路径当前未匹配任何文件"
	if checkErr != nil {
		message += ": " + checkErr.Error()
	}
	if blocking {
		message += "；未启用 Journal 时必须先创建匹配文件"
	}
	*checks = append(*checks, deploymentPreflightCheck{Key: "log_source", Name: "日志采集源", Status: status, Blocking: blocking, Message: message, Evidence: pattern})
}

func (h *Handler) preflightGrafanaCredentials(request *componentCreateRequest, host *models.Host, checks *[]deploymentPreflightCheck) {
	if request.Type != string(models.ComponentGrafana) {
		return
	}
	if request.InitialPassword != "" {
		*checks = append(*checks, deploymentPreflightCheck{Key: "grafana_credentials", Name: "Grafana 初始化凭据", Status: "pass", Blocking: true, Message: "已提供首次安装管理员密码"})
		return
	}
	retainedComponent := &models.Component{Type: models.ComponentGrafana, Name: request.Name, Host: *host}
	var raw string
	var err error
	if request.ReplaceExisting {
		var existing models.Component
		result := h.db.Where("host_id = ? AND type = ? AND deploy_type <> ? AND status <> ?", host.ID, request.Type, "target", "removed").
			Order("id").Limit(1).Find(&existing)
		err = result.Error
		if err == nil && result.RowsAffected != 0 {
			existing.Host = *host
			retainedComponent = &existing
			raw = existing.CustomEnv
		}
	}
	if err == nil && raw == "" {
		raw, err = deployer.NewBinaryDeployer(h.db, h.basePath).ReadRetainedEnvironment(context.Background(), retainedComponent)
	}
	if err == nil && raw != "" {
		var environment map[string]string
		if json.Unmarshal([]byte(raw), &environment) == nil &&
			environment["GF_SECURITY_ADMIN_PASSWORD"] != "" && environment["GF_SECURITY_SECRET_KEY"] != "" {
			*checks = append(*checks, deploymentPreflightCheck{Key: "grafana_credentials", Name: "Grafana 初始化凭据", Status: "pass", Blocking: true, Message: "检测到保留数据恢复所需的受保护管理员凭据和 Secret Key"})
			return
		}
	}
	message := "首次安装必须提供管理员密码；保留数据恢复必须存在受保护的历史凭据"
	if err != nil {
		message += ": " + err.Error()
	}
	*checks = append(*checks, deploymentPreflightCheck{Key: "grafana_credentials", Name: "Grafana 初始化凭据", Status: "fail", Blocking: true, Message: message})
}

func (h *Handler) preflightPort(request *componentCreateRequest, host *models.Host, checks *[]deploymentPreflightCheck) {
	occupied := false
	process := ""
	var probeErr error
	if host.ConnectionType == "winrm" {
		password, decryptErr := credentials.Decrypt(host.Password)
		if decryptErr != nil {
			probeErr = decryptErr
		} else {
			client, clientErr := winrmclient.NewClientWithOptions(winrmclient.Options{Host: host.IP, Port: host.WinRMPort, Scheme: host.WinRMScheme, Username: host.Username, Password: password, SkipTLSVerify: host.WinRMSkipVerify})
			if clientErr != nil {
				probeErr = clientErr
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				output, runErr := client.RunPS(ctx, fmt.Sprintf(`$item=Get-NetTCPConnection -State Listen -LocalPort %d -ErrorAction SilentlyContinue|Select-Object -First 1;if($item){Write-Output ('occupied:'+ $item.OwningProcess)}else{Write-Output 'free'}`, request.Port))
				probeErr = runErr
				if runErr == nil && strings.HasPrefix(strings.TrimSpace(output), "occupied:") {
					occupied = true
					process = strings.TrimSpace(output)
				}
			}
		}
	} else {
		output, commandErr := runHostCommand(host, "ss -ltnp 2>/dev/null || netstat -tlnp 2>/dev/null")
		probeErr = commandErr
		if commandErr == nil || strings.TrimSpace(output) != "" {
			probeErr = nil
			if value, exists := parseListeningPorts(output)[request.Port]; exists {
				occupied, process = true, value
			}
		}
	}
	if probeErr != nil {
		*checks = append(*checks, deploymentPreflightCheck{Key: "port", Name: "监听端口", Status: "fail", Blocking: true, Message: "无法读取目标主机端口状态: " + probeErr.Error()})
		return
	}
	if !occupied {
		*checks = append(*checks, deploymentPreflightCheck{Key: "port", Name: "监听端口", Status: "pass", Blocking: true, Message: fmt.Sprintf("端口 %d 当前可用", request.Port)})
		return
	}
	var existing models.Component
	managed := h.db.Where("host_id = ? AND type = ? AND port = ? AND deploy_type <> ? AND status <> ?", host.ID, request.Type, request.Port, "target", "removed").First(&existing).Error == nil
	if request.ReplaceExisting && managed {
		*checks = append(*checks, deploymentPreflightCheck{Key: "port", Name: "监听端口", Status: "warn", Blocking: false, Message: "端口由当前受管同类型组件占用，重新部署时将替换原服务", Evidence: gin.H{"component_id": existing.ID, "process": process}})
		return
	}
	*checks = append(*checks, deploymentPreflightCheck{Key: "port", Name: "监听端口", Status: "fail", Blocking: true, Message: fmt.Sprintf("端口 %d 已被占用", request.Port), Evidence: process})
}

func safeDeploymentParameters(definition models.DeploymentDefinition, request *componentCreateRequest) map[string]interface{} {
	result := make(map[string]interface{})
	for _, parameter := range definition.Parameters {
		value, exists := deploymentRequestValue(request, parameter.Field)
		if !exists {
			continue
		}
		if parameter.Type == "password" {
			result[parameter.Field] = "configured"
		} else if parameter.Field == "custom_env" && strings.TrimSpace(fmt.Sprint(value)) != "" {
			result[parameter.Field] = "configured"
		} else {
			result[parameter.Field] = value
		}
	}
	return result
}

func deploymentPreview(definition models.DeploymentDefinition, request componentCreateRequest, host models.Host) []deploymentPreviewAction {
	if request.DeployType == "target" {
		return []deploymentPreviewAction{{Order: 1, Action: "注册 Prometheus 采集目标", Target: net.JoinHostPort(host.IP, strconv.Itoa(request.Port))}, {Order: 2, Action: "校验并重载 Prometheus 配置", Target: "prometheus.yml"}}
	}
	installTarget := request.InstallPath
	if definition.Platform == "windows" {
		installTarget = `C:\Program Files\windows_exporter`
	} else if installTarget == "" {
		installTarget = "按实例 ID 自动分配；重新部署保留已有安装路径"
	}
	manager := definition.ServiceManager
	healthPath := definition.HealthPath
	if config, err := models.EmbedDeploymentParameters("{}", request.StructuredParameters); err == nil {
		healthPath = models.ManagedHealthPath(&models.Component{Type: models.ComponentType(request.Type), Config: config})
	}
	return []deploymentPreviewAction{
		{Order: 1, Action: "校验并准备安装包", Target: request.Type + " " + request.Version},
		{Order: 2, Action: "连接目标主机", Target: host.Name + " · " + host.IP},
		{Order: 3, Action: "安装或替换组件", Target: installTarget},
		{Order: 4, Action: "生成并启用服务", Target: manager},
		{Order: 5, Action: "启动并验证健康端点", Target: healthPath},
		{Order: 6, Action: "同步 Prometheus、告警与可视化集成", Target: "平台自动配置"},
	}
}

func deploymentPreflightResponse(passed bool, request componentCreateRequest, definition models.DeploymentDefinition, host models.Host, checks []deploymentPreflightCheck, actions []deploymentPreviewAction) gin.H {
	return gin.H{
		"passed": passed, "component_type": request.Type, "definition": definition,
		"host":                  gin.H{"id": host.ID, "name": host.Name, "ip": host.IP, "os": host.OS, "connection_type": host.ConnectionType},
		"normalized_parameters": safeDeploymentParameters(definition, &request),
		"checks":                checks, "actions": actions, "checked_at": time.Now(),
	}
}
