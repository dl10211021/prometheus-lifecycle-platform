package handlers

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	winrmclient "github.com/promlifecycle/platform/core/winrm"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (h *Handler) ListHosts(c *gin.Context) {
	var hosts []models.Host
	if err := h.db.Preload("Components", "status <> ?", "removed").Find(&hosts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取主机列表失败"})
		return
	}
	for index := range hosts {
		hosts[index] = publicHost(hosts[index])
	}
	c.JSON(http.StatusOK, gin.H{"hosts": hosts})
}

func (h *Handler) GetHost(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var host models.Host
	if err := h.db.Preload("Components", "status <> ?", "removed").First(&host, id).Error; err != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}
	c.JSON(http.StatusOK, publicHost(host))
}

func publicHost(host models.Host) models.Host {
	for index := range host.Components {
		host.Components[index] = publicComponent(host.Components[index])
	}
	return host
}

// connResult carries the outcome of a host connectivity probe.
type connResult struct {
	success       bool
	error         string
	systemInfo    string
	isWindows     bool
	hasDocker     bool
	dockerVersion string
}

// testHostConnectivity is synchronous: one deadline owns both connection setup
// and OS detection. Only the completed probe writes status, never a detached task.
func (h *Handler) testHostConnectivity(parent context.Context, host *models.Host) (result connResult) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	defer func() {
		if recover() != nil {
			result = connResult{error: "连接检查异常"}
		}
		if ctx.Err() != nil {
			result = connResult{error: "连接检查超时或已取消"}
		}
		now := time.Now()
		host.LastCheckedAt = &now
		host.Status = "offline"
		host.LastError = result.error
		updates := map[string]interface{}{
			"status": "offline", "last_error": result.error, "last_checked_at": &now,
		}
		if result.success {
			host.Status, host.LastError, host.LastOnline = "online", "", &now
			host.IsWindows = result.isWindows
			host.OS = "linux"
			if result.isWindows {
				host.OS = "windows"
			}
			host.OSVersion = result.systemInfo
			host.HasDocker, host.DockerVersion = false, ""
			updates["status"], updates["last_error"], updates["last_online"] = "online", "", &now
			updates["os"], updates["os_version"], updates["is_windows"] = host.OS, host.OSVersion, host.IsWindows
			updates["has_docker"], updates["docker_version"] = false, ""
		}
		// New hosts are verified before insertion; failures cannot delete an
		// existing row or make an in-flight, unaccepted host visible.
		if host.ID != 0 {
			if err := h.db.Model(&models.Host{}).Where("id = ?", host.ID).Updates(updates).Error; err != nil {
				result = connResult{error: "无法保存连接检查结果"}
			}
		}
	}()

	password, err := credentials.Decrypt(host.Password)
	if err != nil {
		return connResult{error: "无法解密主机密码"}
	}
	privateKey, err := credentials.Decrypt(host.PrivateKey)
	if err != nil {
		return connResult{error: "无法解密主机私钥"}
	}
	if host.ConnectionType == "local" {
		return connResult{success: true, systemInfo: "local"}
	}
	if host.ConnectionType == "winrm" {
		wr, err := winrmclient.NewClientWithOptions(winrmclient.Options{
			Host: host.IP, Port: host.WinRMPort, Scheme: host.WinRMScheme,
			Username: host.Username, Password: password, SkipTLSVerify: host.WinRMSkipVerify,
			Context: ctx, RequestTimeout: 20 * time.Second,
		})
		if err != nil {
			return connResult{error: "WinRM 客户端初始化失败，请检查连接配置"}
		}
		if _, err := wr.RunPS(ctx, "Write-Output ok"); err != nil {
			return connResult{error: "WinRM 连接失败，请检查网络、证书和账号密码"}
		}
		return connResult{success: true, systemInfo: "windows (winrm)", isWindows: true}
	}

	client, err := ssh.NewClientContext(ctx, host.IP, host.Port, host.Username, password, privateKey)
	if err != nil {
		// Remote banners/command output are untrusted and may echo credentials.
		return connResult{error: "SSH 连接失败，请检查网络、端口、凭据和主机密钥"}
	}
	defer client.Close()
	si, err := client.DetectOS()
	if err != nil {
		return connResult{error: "SSH 系统检测失败，请检查远程命令执行权限"}
	}
	return connResult{success: true, systemInfo: si.OSVersion, isWindows: si.IsWindows}
}

func (h *Handler) CreateHost(c *gin.Context) {
	var req hostCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 不能为空"})
		return
	}
	if req.ConnectionType == "local" {
		req.Username = "root"
	}
	if req.IP == "" || (req.ConnectionType != "local" && req.Username == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "IP 和 username 不能为空，请检查 SSH 连接信息"})
		return
	}
	if !utils.ValidIPv4(req.IP) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 IP 地址（IPv4 每段 0-255）"})
		return
	}
	if req.ConnectionType == "winrm" {
		if strings.TrimSpace(req.Password) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Windows WinRM 主机必须填写管理账号密码"})
			return
		}
		if req.WinRMScheme != "" && req.WinRMScheme != "http" && req.WinRMScheme != "https" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "WinRM 协议仅支持 http 或 https"})
			return
		}
	}
	host, existed, err := h.createHostFromRequest(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if existed {
		c.JSON(http.StatusOK, gin.H{"status": "exists", "id": host.ID, "host": publicHost(host)})
		return
	}
	c.JSON(http.StatusCreated, publicHost(host))
}

// hostCreateRequest is the shared input for single and batch host creation.
type hostCreateRequest struct {
	Name             string `json:"name"`
	VerifyConnection bool   `json:"verify_connection"` // verify before accepting, for single and batch creation
	IP               string `json:"ip"`
	OS               string `json:"os"`
	ConnectionType   string `json:"connection_type"`
	Port             int    `json:"port"`
	WinRMPort        int    `json:"winrm_port"`
	WinRMScheme      string `json:"winrm_scheme"`
	WinRMSkipVerify  bool   `json:"winrm_skip_verify"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	PrivateKey       string `json:"private_key"`
	Description      string `json:"description"`
}

// createHostFromRequest applies defaults, creates the host record and returns
// (host, existed, error). A host with the same IP returns existed=true.
func (h *Handler) createHostFromRequest(ctx context.Context, req hostCreateRequest) (models.Host, bool, error) {
	var existing models.Host
	if err := h.db.Where("ip = ?", req.IP).First(&existing).Error; err == nil {
		return existing, true, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Host{}, false, fmt.Errorf("读取已有主机失败")
	}
	if req.Name == "" {
		req.Name = req.IP
	}
	if req.OS == "" {
		req.OS = "linux"
	}
	if req.ConnectionType == "" {
		req.ConnectionType = "ssh"
	}
	if req.ConnectionType != "local" && req.ConnectionType != "ssh" && req.ConnectionType != "winrm" {
		return models.Host{}, false, fmt.Errorf("connection_type 仅支持 local、ssh 或 winrm")
	}
	if req.ConnectionType == "winrm" {
		req.OS = "windows"
		if strings.TrimSpace(req.Password) == "" {
			return models.Host{}, false, fmt.Errorf("WinRM 主机必须提供密码凭据")
		}
		if req.WinRMScheme != "" && req.WinRMScheme != "http" && req.WinRMScheme != "https" {
			return models.Host{}, false, fmt.Errorf("WinRM 协议仅支持 http 或 https")
		}
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.WinRMScheme == "" {
		req.WinRMScheme = "https"
	}
	if req.WinRMPort == 0 {
		if req.WinRMScheme == "https" {
			req.WinRMPort = 5986
		} else {
			req.WinRMPort = 5985
		}
	}
	isWindows := req.OS == "windows"

	host := models.Host{
		Name:            req.Name,
		IP:              req.IP,
		OS:              req.OS,
		ConnectionType:  req.ConnectionType,
		Port:            req.Port,
		IsWindows:       isWindows,
		WinRMPort:       req.WinRMPort,
		WinRMScheme:     req.WinRMScheme,
		WinRMSkipVerify: req.WinRMSkipVerify,
		Username:        req.Username,
		Password:        req.Password,
		PrivateKey:      req.PrivateKey,
		Status:          "unknown",
		Description:     utils.SanitizeInput(req.Description),
	}
	if req.VerifyConnection {
		if probe := h.testHostConnectivity(ctx, &host); !probe.success {
			return models.Host{}, false, fmt.Errorf("主机连接验证失败：%s", probe.error)
		}
	}

	if err := h.db.Create(&host).Error; err != nil {
		if utils.IsDuplicateEntryError(err) {
			var existing models.Host
			if err2 := h.db.Where("ip = ?", host.IP).First(&existing).Error; err2 == nil {
				return existing, true, nil
			}
		}
		return models.Host{}, false, fmt.Errorf("创建主机失败，请检查名称是否已被使用")
	}
	return host, false, nil
}

// BatchCreateHosts creates multiple hosts in one request and reports a
// per-item result (created / exists / failed).
func (h *Handler) BatchCreateHosts(c *gin.Context) {
	var reqs []hostCreateRequest
	if err := c.ShouldBindJSON(&reqs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if len(reqs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hosts 列表不能为空"})
		return
	}
	if len(reqs) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "主机数量过多，单次最多 200 台"})
		return
	}

	type itemResult struct {
		Index  int    `json:"index"`
		IP     string `json:"ip"`
		Name   string `json:"name"`
		Status string `json:"status"` // created / exists / failed
		ID     uint   `json:"id,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]itemResult, len(reqs))
	// Preserve request order for repeated IPs, while unrelated hosts probe in
	// parallel. No DB transaction or shared lock spans any network operation.
	groups := make([][]int, 0, len(reqs))
	groupByIP := make(map[string]int, len(reqs))
	for i, req := range reqs {
		group, ok := groupByIP[req.IP]
		if !ok {
			group = len(groups)
			groupByIP[req.IP] = group
			groups = append(groups, nil)
		}
		groups[group] = append(groups[group], i)
	}
	jobs := make(chan []int, len(groups))
	for _, group := range groups {
		jobs <- group
	}
	close(jobs)
	var workers sync.WaitGroup
	for worker := 0; worker < 5 && worker < len(groups); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for group := range jobs {
				for _, i := range group {
					req := reqs[i]
					item := itemResult{Index: i, IP: req.IP, Name: req.Name, Status: "failed"}
					if req.ConnectionType == "local" && req.Username == "" {
						req.Username = "root"
					}
					switch {
					case req.IP == "" || (req.ConnectionType != "local" && req.Username == ""):
						item.Error = "ip/username required"
					case !utils.ValidIPv4(req.IP):
						item.Error = "无效的 IP 地址（IPv4 每段 0-255）"
					default:
						host, existed, err := h.createHostFromRequest(c.Request.Context(), req)
						if err != nil {
							item.Error = err.Error()
						} else {
							item.Status, item.ID, item.Name = "created", host.ID, host.Name
							if existed {
								item.Status = "exists"
							}
						}
					}
					results[i] = item
				}
			}
		}()
	}
	workers.Wait()
	failed := 0
	for _, item := range results {
		if item.Status == "failed" {
			failed++
		}
	}

	code := http.StatusOK
	if failed > 0 {
		code = http.StatusMultiStatus
	}
	c.JSON(code, gin.H{
		"results": results,
		"total":   len(results),
		"failed":  failed,
	})
}

// CheckHostPort inspects a host and reports what is listening on the given
// port plus the next free port, so the UI can resolve conflicts automatically.
func (h *Handler) CheckHostPort(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var host models.Host
	if err := h.db.First(&host, id).Error; err != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}
	port, err := strconv.Atoi(c.Param("port"))
	if err != nil || port < 1 || port > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "端口无效，请输入 1 到 65535"})
		return
	}

	// Collect all listening ports (with process info) from the host.
	out, err := runHostCommand(&host, "ss -ltnp 2>/dev/null || netstat -tlnp 2>/dev/null")
	if err != nil && strings.TrimSpace(out) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法读取主机端口信息: " + err.Error()})
		return
	}

	used := parseListeningPorts(out)
	process := ""
	occupied := false
	if p, ok := used[port]; ok {
		occupied = true
		process = p
	}
	suggested := port + 1
	for limit := 0; limit < 200; limit++ {
		if _, ok := used[suggested]; !ok {
			break
		}
		suggested++
	}
	if suggested > 65535 {
		suggested = 0
	}
	c.JSON(http.StatusOK, gin.H{
		"port":                port,
		"occupied":            occupied,
		"process":             process,
		"suggested_free_port": suggested,
	})
}

// runHostCommand executes a command locally or over SSH.
func runHostCommand(host *models.Host, cmd string) (string, error) {
	if ssh.IsLocalHost(host.IP, host.ConnectionType) {
		out, err := exec.CommandContext(context.Background(), "sh", "-c", cmd).CombinedOutput()
		return string(out), err
	}
	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return "", err
	}
	defer client.Close()
	out, _, err := client.RunCommand(cmd)
	return out, err
}

// parseListeningPorts extracts "port -> process" from ss -ltnp output.
func parseListeningPorts(out string) map[int]string {
	result := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// formats: LISTEN 0 128 *:9090 *:* users:(("prometheus",pid=1234,fd=3))
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		addr := fields[3]
		idx := strings.LastIndex(addr, ":")
		if idx < 0 {
			continue
		}
		p, err := strconv.Atoi(addr[idx+1:])
		if err != nil || p < 1 || p > 65535 {
			continue
		}
		proc := ""
		if u := strings.Index(line, "users:(("); u >= 0 {
			if q := strings.Index(line[u:], "\""); q >= 0 {
				start := u + q + 1
				if end := strings.Index(line[start:], "\""); end >= 0 {
					proc = line[start : start+end]
				}
			}
		}
		result[p] = proc
	}
	return result
}

func (h *Handler) UpdateHost(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var req struct {
		Name            string `json:"name"`
		IP              string `json:"ip"`
		OS              string `json:"os"`
		ConnectionType  string `json:"connection_type"`
		Port            int    `json:"port"`
		WinRMPort       int    `json:"winrm_port"`
		WinRMScheme     string `json:"winrm_scheme"`
		WinRMSkipVerify *bool  `json:"winrm_skip_verify"`
		Username        string `json:"username"`
		Password        string `json:"password"`
		PrivateKey      string `json:"private_key"`
		Description     string `json:"description"`
		HasDocker       *bool  `json:"has_docker"`
		Role            string `json:"role"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	var host models.Host
	if err := h.db.First(&host, id).Error; err != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}

	if req.Name != "" {
		host.Name = req.Name
	}
	if req.IP != "" {
		host.IP = req.IP
	}
	if req.OS != "" {
		host.OS = req.OS
		host.IsWindows = req.OS == "windows"
	}
	if req.ConnectionType != "" {
		host.ConnectionType = req.ConnectionType
	}
	if req.Port != 0 {
		host.Port = req.Port
	}
	if req.WinRMPort != 0 {
		host.WinRMPort = req.WinRMPort
	}
	if req.WinRMScheme != "" {
		host.WinRMScheme = req.WinRMScheme
	}
	if req.WinRMSkipVerify != nil {
		host.WinRMSkipVerify = *req.WinRMSkipVerify
	}
	if req.Username != "" {
		host.Username = req.Username
	}
	if req.Password != "" {
		host.Password = req.Password
	}
	if req.PrivateKey != "" {
		host.PrivateKey = req.PrivateKey
	}
	if req.Description != "" {
		host.Description = utils.SanitizeInput(req.Description)
	}
	if req.HasDocker != nil {
		host.HasDocker = *req.HasDocker
	}
	if req.Role != "" {
		host.Role = req.Role
	}

	if err := h.db.Save(&host).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新主机失败"})
		return
	}

	c.JSON(http.StatusOK, host)
}

func (h *Handler) DeleteHost(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ctx, releaseHost, err := services.ClaimHostOperation(ctx, h.db, id, "delete_host")
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	defer releaseHost()
	var host models.Host
	if err := h.db.First(&host, id).Error; err != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}
	if ssh.IsLocalHost(host.IP, host.ConnectionType) {
		c.JSON(http.StatusConflict, gin.H{"error": "平台本机不能删除"})
		return
	}
	var components []models.Component
	if err := h.db.Preload("Host").Preload("Cluster").Where("host_id = ?", id).Order("id").Find(&components).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取主机组件失败"})
		return
	}
	force := c.Query("force") == "1" || c.Query("force") == "true"
	if len(components) > 0 && !force {
		c.JSON(http.StatusConflict, gin.H{"error": "主机仍有关联组件，请先安全移除组件；force=1 会逐项验证卸载，失败时保留记录", "count": len(components)})
		return
	}
	releases := make([]func(), 0, len(components))
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	for _, component := range components {
		var release func()
		ctx, release, err = services.ClaimComponentOperation(ctx, h.db, component.ID, "delete_host")
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "component_id": component.ID})
			return
		}
		releases = append(releases, release)
		if componentBusy(&component) {
			c.JSON(http.StatusConflict, gin.H{"error": "组件有未完成操作，主机未被删除", "component_id": component.ID})
			return
		}
		var count int64
		if err := h.db.Model(&models.Backup{}).Where("component_id = ?", component.ID).Count(&count).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法核对恢复点，已拒绝删除"})
			return
		}
		if count > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "该主机仍承载组件恢复点。请先迁移或明确删除备份；保留主机和组件记录以便恢复", "component_id": component.ID, "backup_count": count})
			return
		}
	}
	results := make([]gin.H, 0, len(components))
	failed := false
	for i := range components {
		component := &components[i]
		var removeErr error
		if component.Status != "removed" {
			switch component.DeployType {
			case "binary":
				removeErr = deployer.NewBinaryDeployer(h.db, h.basePath).Remove(ctx, component)
			case "target":
			default:
				removeErr = fmt.Errorf("不支持通过主机删除入口卸载 %s；请先使用对应生命周期流程", component.DeployType)
			}
		}
		if removeErr == nil {
			removeErr = h.deleteHostComponentMetadata(component.ID)
		}
		if removeErr != nil {
			failed = true
			results = append(results, gin.H{"component_id": component.ID, "status": "failed", "error": removeErr.Error()})
			continue
		}
		results = append(results, gin.H{"component_id": component.ID, "status": "deleted"})
	}
	if failed {
		c.JSON(http.StatusMultiStatus, gin.H{"error": "部分组件未能完成移除，主机及失败项记录已保留，可修复后重试", "results": results})
		return
	}
	if result := h.db.Delete(&host); result.Error != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "主机仍有关联记录，已保留主机以便处理", "results": results})
		return
	}
	h.logOp(c, "delete", "host", id, "主机组件逐项移除成功后删除主机")
	c.JSON(http.StatusOK, gin.H{"message": "主机已删除", "results": results})
}

func (h *Handler) deleteHostComponentMetadata(componentID uint) error {
	return h.db.Transaction(func(tx *gorm.DB) error {
		for _, model := range []interface{}{&models.ComponentConfig{}, &models.ResourceUsage{}, &models.InspectionNotificationState{}, &models.ComponentAdoption{}, &models.MaintenanceWindow{}} {
			if tx.Migrator().HasTable(model) {
				if err := tx.Where("component_id = ?", componentID).Delete(model).Error; err != nil {
					return err
				}
			}
		}
		var schedules []models.ScheduledBackup
		if err := tx.Where("component_id = ?", componentID).Find(&schedules).Error; err != nil {
			return err
		}
		for _, schedule := range schedules {
			if err := tx.Where("scheduled_backup_id = ?", schedule.ID).Delete(&models.ScheduledBackupStatus{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&schedule).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&models.Operation{}).Where("component_id = ?", componentID).Update("component_id", nil).Error; err != nil {
			return err
		}
		if err := tx.Where("source_id = ? OR target_id = ?", componentID, componentID).Delete(&models.TopologyEdge{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Component{}, componentID).Error
	})
}

// BrowseDirectory lists subdirectories of a path on the target host (FTP-style dir picker).
func (h *Handler) BrowseDirectory(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var host models.Host
	if err := h.db.First(&host, id).Error; err != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}

	p := c.Query("path")
	if p == "" {
		p = "/"
	}
	p = filepath.Clean(p)
	if p == "." {
		p = "/"
	}
	// Reject shell metacharacters to prevent command injection in the remote ls command
	if strings.ContainsAny(p, "'\"$`;|&<>()\\") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "路径包含非法字符"})
		return
	}

	isLocal := host.IP == "127.0.0.1" || host.IP == "localhost" || host.ConnectionType == "local"
	var dirNames []string

	if isLocal {
		entries, err := os.ReadDir(p)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无法读取目录: " + err.Error()})
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				dirNames = append(dirNames, e.Name())
			}
		}
	} else {
		client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "SSH 连接目标主机失败（请检查凭据或先在主机管理测试连接）: " + err.Error()})
			return
		}
		defer client.Close()
		quoted := "'" + strings.ReplaceAll(p, "'", "'\\''") + "'"
		out, _, err := client.RunCommand("ls -1dp " + quoted + "/*/ 2>/dev/null; ls -1dp " + quoted + "/* 2>/dev/null | head -200")
		if err != nil && strings.TrimSpace(out) == "" {
			c.JSON(http.StatusBadGateway, gin.H{"error": "列出目录失败: " + err.Error()})
			return
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			base := filepath.Base(line)
			if strings.HasSuffix(line, "/") {
				base = filepath.Base(strings.TrimSuffix(line, "/"))
			}
			if base == "" || base == "." || base == "/" || base == ".." {
				continue
			}
			if !seen[base] {
				seen[base] = true
				dirNames = append(dirNames, base)
			}
		}
	}

	sort.Strings(dirNames)
	dirs := make([]gin.H, 0, len(dirNames))
	for _, name := range dirNames {
		dirs = append(dirs, gin.H{"name": name, "full": filepath.Join(p, name)})
	}

	parent := p
	if p != "/" {
		parent = filepath.Dir(p)
		if parent == "" {
			parent = "/"
		}
	}

	c.JSON(http.StatusOK, gin.H{"path": p, "parent": parent, "dirs": dirs})
}

// DetectComponents returns one row per actual listener/process from one host snapshot.
func (h *Handler) DetectComponents(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var host models.Host
	if h.db.First(&host, id).Error != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}
	items, err := h.discoverHost(host)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	results := make([]services.DiscoveredComponent, 0, len(items))
	for _, item := range items {
		if c.Query("versions") == "1" {
			if c.Request.Context().Err() != nil {
				return
			}
			item.component.Version = discoveredProtocolVersion(&host, item.component.Type, item.component.Port)
		}
		results = append(results, item.component)
	}
	c.JSON(http.StatusOK, gin.H{"host_id": host.ID, "components": results})
}

func (h *Handler) TestHostConnection(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var host models.Host
	if err := h.db.First(&host, id).Error; err != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}

	result := h.testHostConnectivity(c.Request.Context(), &host)
	if !result.success {
		c.JSON(http.StatusBadGateway, gin.H{
			"success": false, "error": result.error, "host": host.Name,
			"status": host.Status, "last_error": host.LastError, "last_checked_at": host.LastCheckedAt,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true, "system_info": result.systemInfo, "is_windows": result.isWindows,
		"has_docker": result.hasDocker, "docker_version": result.dockerVersion,
		"status": host.Status, "last_error": host.LastError, "last_checked_at": host.LastCheckedAt,
	})
}
