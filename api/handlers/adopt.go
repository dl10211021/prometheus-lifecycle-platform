package handlers

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// AdoptComponent records an already-running component without changing it.
// Observe is the safe default. Managed ownership requires a successful host
// preflight and a durable takeover snapshot before lifecycle controls unlock.
func (h *Handler) AdoptComponent(c *gin.Context) {
	hostID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var req adoptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "接管请求无效: " + err.Error()})
		return
	}
	if !utils.IsValidComponentType(req.Type) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组件类型无效，请选择受支持的类型"})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "监听端口必须在 1–65535 之间"})
		return
	}
	_, releaseHost, claimErr := services.ClaimHostOperation(c.Request.Context(), h.db, hostID, "adopt")
	if claimErr != nil {
		c.JSON(http.StatusConflict, gin.H{"error": claimErr.Error()})
		return
	}
	defer releaseHost()
	if req.Ownership == "" {
		req.Ownership = "observe"
	}
	if req.Ownership != "observe" && req.Ownership != "managed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ownership 仅支持 observe 或 managed"})
		return
	}
	var host models.Host
	if err := h.db.First(&host, hostID).Error; err != nil {
		utils.HandleNotFoundError(c, "host")
		return
	}
	if req.ClusterID == 0 {
		clusterID, err := services.SingleNodeID(h.db)
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "未找到可接管组件的环境，请先完成单节点初始化"})
			return
		}
		req.ClusterID = clusterID
	}
	var cluster models.Cluster
	if err := h.db.First(&cluster, req.ClusterID).Error; err != nil {
		utils.HandleNotFoundError(c, "cluster")
		return
	}

	var existing models.Component
	existingErr := h.db.Where("host_id = ? AND port = ? AND status <> ?", hostID, req.Port, "removed").First(&existing).Error
	if existingErr == nil {
		if stepID, _ := c.Request.Context().Value(adoptionStepContextKey{}).(uint); stepID != 0 {
			h.skipOwnedAdoption(c, host, existing, req)
			return
		}
		var adoption models.ComponentAdoption
		_ = h.db.Where("component_id = ?", existing.ID).First(&adoption).Error
		c.JSON(http.StatusConflict, gin.H{"error": "该主机端口已被平台记录", "component": existing, "adoption": adoption})
		return
	}
	if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
		c.JSON(500, gin.H{"error": "读取已有组件失败"})
		return
	}

	discovered, err := h.discoverHost(host)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	var selected *hostProcessEvidence
	validFingerprints := make(map[string]bool)
	for index := range discovered {
		candidate := &discovered[index]
		if candidate.component.Type == req.Type && candidate.component.Port == req.Port {
			selected = candidate
			validFingerprints[candidate.component.Fingerprint] = true
			break
		}
	}
	if isLocalAdoptionHost(&host) {
		local, discoveryErr := services.NewOnboardingService(h.db, h.basePath).DiscoverLocal()
		if discoveryErr == nil {
			for _, candidate := range local {
				if candidate.Type != req.Type || candidate.Port != req.Port {
					continue
				}
				if selected == nil && candidate.Runtime == "docker" {
					selected = &hostProcessEvidence{component: candidate}
				}
				if selected != nil && (candidate.Runtime == "docker" || candidate.PID == selected.component.PID) {
					validFingerprints[candidate.Fingerprint] = true
					if selected.component.Config == "" {
						selected.component.Config = candidate.Config
					}
					if selected.component.Data == "" {
						selected.component.Data = candidate.Data
					}
					if candidate.Runtime == "docker" {
						selected.component = candidate
					}
				}
			}
		}
	}
	if selected == nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "该端口没有与所选组件类型匹配的进程，请重新发现并核对组件身份"})
		return
	}
	if req.Fingerprint != "" && !validFingerprints[req.Fingerprint] {
		c.JSON(http.StatusConflict, gin.H{"error": "进程身份已变化，请重新扫描后接管"})
		return
	}
	if err := validateAdoptionProtocol(&host, req.Type, req.Port); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	pid := strconv.Itoa(selected.component.PID)
	binPath := selected.component.Binary
	unit := deployer.CanonicalServiceName(selected.component.Unit)
	configFile, dataDir := selected.component.Config, selected.component.Data
	version := selected.component.Version
	runtimeType := selected.component.Runtime
	if runtimeType != "docker" && version == "" {
		version = discoveredProtocolVersion(&host, req.Type, req.Port)
	}

	discovery := map[string]interface{}{
		"pid": pid, "unit": unit, "runtime": runtimeType, "bin": binPath,
		"config": configFile, "data": dataDir, "version": version, "port": req.Port,
	}
	discoveryJSON, _ := json.Marshal(discovery)
	snapshotPath, snapshotChecksum := "", ""
	var remoteEvidence *remoteTakeoverEvidence
	if req.Ownership == "managed" {
		if runtimeType == "docker" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "检测到现有 Docker 容器；当前可先使用 observe，无 Compose 来源的容器不能安全升级或卸载"})
			return
		}
		if unit == "" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "managed 接管需要可识别的 systemd 单元；请先使用 observe 或将服务交由 systemd 管理"})
			return
		}
		if isLocalAdoptionHost(&host) {
			report, preflightErr := services.NewOnboardingService(h.db, h.basePath).Preflight()
			if preflightErr != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": "接管前预检失败: " + preflightErr.Error()})
				return
			}
			if !report.Ready {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "接管前预检存在阻断项", "preflight": report})
				return
			}
		} else {
			remoteEvidence, err = probeRemoteTakeover(c.Request.Context(), &host, selected)
			if err != nil {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
				return
			}
		}
		snapshotPath, snapshotChecksum, err = h.createTakeoverSnapshot(&host, req.Type, pid, unit, binPath, configFile, dataDir, version, remoteEvidence)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "创建接管快照失败，未执行接管: " + err.Error()})
			return
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.Type
	}
	deployType := "target"
	if req.Ownership == "managed" {
		deployType = "binary"
	}
	installPath := ""
	if runtimeType != "docker" {
		installPath = filepath.Dir(binPath)
		if req.Type == "grafana" {
			if home := argvValue(selected.args, "-homepath", "--homepath"); home != "" {
				installPath = home
			} else if filepath.Base(installPath) == "bin" {
				installPath = filepath.Dir(installPath)
			}
		}
	}
	importedArgs, _ := json.Marshal(selected.args)
	comp := models.Component{
		ClusterID: req.ClusterID, Type: models.ComponentType(req.Type), Name: name,
		HostID: hostID, Version: version, Port: req.Port, DeployType: deployType,
		Status: models.StatusRunning, ServiceName: unit, ConfigPath: configFile,
		DataPath: dataDir, InstallPath: installPath,
		BinaryPath: binPath, ImportedArgs: string(importedArgs),
		InternalURL: "http://" + net.JoinHostPort(host.IP, strconv.Itoa(req.Port)),
	}
	adoption := models.ComponentAdoption{
		HostID: hostID, Ownership: req.Ownership, SnapshotPath: snapshotPath,
		SnapshotChecksum: snapshotChecksum, Discovery: string(discoveryJSON),
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&comp).Error; err != nil {
			return err
		}
		adoption.ComponentID = comp.ID
		// The scheduler must see the original configuration as manually owned
		// as soon as it can see the managed component.
		if remoteEvidence != nil && configFile != "" {
			cfg := models.ComponentConfig{ComponentID: comp.ID, Name: remoteTakeoverConfigName(req.Type),
				ContentType: "yaml", Content: string(remoteEvidence.ConfigContent), Version: 1,
				IsActive: true, FilePath: configFile, Description: "远程接管时保留的原配置"}
			if err := tx.Create(&cfg).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&adoption).Error; err != nil {
			return err
		}
		return linkAdoptionStep(c.Request.Context(), tx, comp.ID, "adopted", adoption.Ownership)
	}); err != nil {
		if snapshotPath != "" {
			_ = os.Remove(snapshotPath)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存接管组件失败: " + err.Error()})
		return
	}
	warning := ""
	if req.Ownership == "observe" {
		warning = "观察模式不会升级、卸载或修改现有进程；可随时无损解除纳管"
	} else if remoteEvidence != nil && configFile != "" {
		warning = "已保留原服务及配置；原配置按手动配置管理，自动生成配置不会覆盖它"
	} else if unit == "" {
		warning = "未检测到 systemd 单元，重启/日志能力受限"
	}
	h.logOp(c, "adopt", "component", comp.ID, "以 "+req.Ownership+" 所有权接管已有组件")
	c.JSON(http.StatusCreated, gin.H{"component": comp, "adoption": adoption, "warning": warning})
}

// detectComponentVersion runs "<binary> --version" on the host and extracts
// the version string.
func detectComponentVersion(host *models.Host, binPath string) string {
	out, err := runHostCommand(host, utils.QuoteShell(binPath)+" --version 2>/dev/null")
	if err != nil && strings.TrimSpace(out) == "" {
		return ""
	}
	re := regexp.MustCompile(`version[= ]+([0-9]+(\.[0-9]+)+)`)
	if m := re.FindStringSubmatch(out); len(m) > 1 {
		return m[1]
	}
	return ""
}

// RequireManagedAdoption prevents mutation routes from treating an observed
// process as platform-owned. Router wiring applies it to upgrade/uninstall.
func RequireManagedAdoption(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		componentID, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil || componentID == 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "组件 ID 无效"})
			return
		}
		var adoption models.ComponentAdoption
		err = db.Where("component_id = ?", uint(componentID)).First(&adoption).Error
		if err == nil && adoption.Ownership == "observe" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":     "观察模式禁止升级或卸载；如需变更，请在完成预检和接管快照后重新以 managed 接管",
				"ownership": adoption.Ownership,
			})
			return
		}
		if err != nil && err != gorm.ErrRecordNotFound {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "读取组件所有权失败"})
			return
		}
		c.Next()
	}
}

// ReleaseAdoptedComponent removes only platform metadata. It never stops a
// process, deletes a unit, changes a config, or removes the takeover snapshot.
func (h *Handler) ReleaseAdoptedComponent(c *gin.Context) {
	componentID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	_, release, err := services.ClaimComponentOperation(c.Request.Context(), h.db, componentID, "release_adoption")
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	defer release()
	var backupCount int64
	if err := h.db.Model(&models.Backup{}).Where("component_id = ?", componentID).Count(&backupCount).Error; err != nil || backupCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "组件仍关联恢复点或无法核对恢复点；请先处理备份，现有服务未被修改"})
		return
	}
	var component models.Component
	if err := h.db.First(&component, componentID).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	var adoption models.ComponentAdoption
	if err := h.db.Where("component_id = ?", componentID).First(&adoption).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "该组件不是通过接管创建的，不能使用无损解除纳管"})
		return
	}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		children := []interface{}{
			&models.ComponentConfig{}, &models.ResourceUsage{},
			&models.Operation{}, &models.Backup{}, &models.ScheduledBackup{},
			&models.ComponentVersion{}, &models.MaintenanceWindow{},
			&models.InspectionNotificationState{},
		}
		for _, child := range children {
			if err := tx.Where("component_id = ?", componentID).Delete(child).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("source_id = ? OR target_id = ?", componentID, componentID).Delete(&models.TopologyEdge{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&adoption).Error; err != nil {
			return err
		}
		result := tx.Exec("DELETE FROM components WHERE id = ?", componentID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "解除纳管失败，现有进程未被修改: " + err.Error()})
		return
	}
	h.logOp(c, "release", "component", componentID, "无损解除已有组件纳管，未修改进程、配置或数据")
	c.JSON(http.StatusOK, gin.H{
		"message":       "已无损解除纳管，现有进程、配置和数据保持不变",
		"component_id":  componentID,
		"ownership":     adoption.Ownership,
		"snapshot_path": adoption.SnapshotPath,
	})
}

func isLocalAdoptionHost(host *models.Host) bool {
	return host != nil && (host.ConnectionType == "local" || host.IP == "127.0.0.1" || host.IP == "localhost")
}

func (h *Handler) createTakeoverSnapshot(host *models.Host, componentType, pid, unit, binPath, configPath, dataPath, version string, remote *remoteTakeoverEvidence) (string, string, error) {
	directory := filepath.Join(h.basePath, "backups", "adoption")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("%s-%d-%d.tar.gz", componentType, time.Now().UnixNano(), os.Getpid())
	finalPath := filepath.Join(directory, name)
	tempFile, err := os.CreateTemp(directory, ".takeover-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	tempPath := tempFile.Name()
	cleanup := func() {
		tempFile.Close()
		_ = os.Remove(tempPath)
	}
	if err := tempFile.Chmod(0600); err != nil {
		cleanup()
		return "", "", err
	}
	gzipWriter := gzip.NewWriter(tempFile)
	tarWriter := tar.NewWriter(gzipWriter)
	manifest, _ := json.MarshalIndent(map[string]interface{}{
		"created_at": time.Now().UTC(), "host_id": host.ID, "host_ip": host.IP,
		"type": componentType, "pid": pid, "unit": unit, "bin": binPath,
		"config": configPath, "data": dataPath, "version": version,
	}, "", "  ")
	if err := writeTakeoverEntry(tarWriter, "manifest.json", manifest, 0600); err != nil {
		cleanup()
		return "", "", err
	}
	if configPath != "" {
		var config []byte
		var readErr error
		if remote != nil {
			config = remote.ConfigContent
		} else {
			config, readErr = os.ReadFile(configPath)
		}
		if readErr == nil {
			if len(config) > 16<<20 {
				cleanup()
				return "", "", fmt.Errorf("配置文件超过 16 MiB 安全上限")
			}
			if err := writeTakeoverEntry(tarWriter, "config/"+filepath.Base(configPath), config, 0600); err != nil {
				cleanup()
				return "", "", err
			}
		} else if !os.IsNotExist(readErr) {
			cleanup()
			return "", "", fmt.Errorf("读取配置失败: %w", readErr)
		}
	}
	unitFile := deployer.CanonicalServiceName(unit) + ".service"
	if unit != "" && regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`).MatchString(unitFile) {
		var unitText string
		var unitErr error
		if remote != nil {
			unitText = string(remote.UnitContent)
		} else {
			unitText, unitErr = runHostCommand(host, "systemctl cat -- "+utils.QuoteShell(unitFile))
		}
		if unitErr == nil && strings.TrimSpace(unitText) != "" {
			if err := writeTakeoverEntry(tarWriter, "systemd/"+unitFile, []byte(unitText), 0600); err != nil {
				cleanup()
				return "", "", err
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		cleanup()
		return "", "", err
	}
	if err := gzipWriter.Close(); err != nil {
		cleanup()
		return "", "", err
	}
	if err := tempFile.Sync(); err != nil {
		cleanup()
		return "", "", err
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", "", err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		_ = os.Remove(tempPath)
		return "", "", err
	}
	checksum, err := checksumFile(finalPath)
	if err != nil {
		_ = os.Remove(finalPath)
		return "", "", err
	}
	return finalPath, checksum, nil
}

func writeTakeoverEntry(writer *tar.Writer, name string, content []byte, mode int64) error {
	header := &tar.Header{Name: name, Mode: mode, Size: int64(len(content)), ModTime: time.Now()}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(content)
	return err
}

func checksumFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
