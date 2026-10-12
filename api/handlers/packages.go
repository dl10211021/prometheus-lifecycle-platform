package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

const MaxUploadSize = 5 * 1024 * 1024 * 1024 // 5GB

type PackageHandler struct {
	db       *gorm.DB
	basePath string
}

func NewPackageHandler(db *gorm.DB, basePath string) *PackageHandler {
	// Ensure packages directory exists
	pkgDir := filepath.Join(basePath, "packages")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		fmt.Printf("warn: failed to create packages dir: %v\n", err)
	}
	pruneStalePackageDownloads(pkgDir, time.Now().Add(-time.Hour))
	return &PackageHandler{
		db:       db,
		basePath: basePath,
	}
}

func pruneStalePackageDownloads(packageDir string, olderThan time.Time) {
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".downloading") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(olderThan) {
			continue
		}
		_ = os.Remove(filepath.Join(packageDir, entry.Name()))
	}
}

// ListPackages returns all packages, optionally filtered by type
func (h *PackageHandler) ListPackages(c *gin.Context) {
	var packages []models.Package
	query := h.db.Model(&models.Package{}).Where("format <> ?", "docker")

	if compType := c.Query("type"); compType != "" {
		query = query.Where("type = ?", compType)
	}
	if name := c.Query("name"); name != "" {
		query = query.Where("name LIKE ?", "%"+name+"%")
	}

	if err := query.Order("created_at DESC").Find(&packages).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取包列表失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"packages": packages})
}

// GetPackage returns a single package by ID
func (h *PackageHandler) GetPackage(c *gin.Context) {
	id := c.Param("id")
	var pkg models.Package
	if err := h.db.Where("format <> ?", "docker").First(&pkg, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "包不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取包信息失败"})
		}
		return
	}
	c.JSON(http.StatusOK, pkg)
}

// CreatePackage creates a new package record (for remote/download URL packages)
func (h *PackageHandler) CreatePackage(c *gin.Context) {
	var input struct {
		Name        string `json:"name" binding:"required"`
		Type        string `json:"type" binding:"required"`
		Version     string `json:"version"`
		Format      string `json:"format" binding:"required"`
		DownloadURL string `json:"download_url"`
		Description string `json:"description"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "输入无效: " + err.Error()})
		return
	}

	// Only offline binary archives are part of the product.
	if input.Format != "zip" && input.Format != "binary" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "包格式无效，仅支持 zip 或 binary"})
		return
	}
	if input.Version != "" && !utils.ValidVersion(input.Version) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "版本格式无效，仅允许字母数字与 . - _，可选 v 前缀"})
		return
	}
	if !h.componentTypeAllowed(input.Type) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组件类型未知或已停用，请选择可用类型"})
		return
	}

	pkg := models.Package{
		Name:        input.Name,
		Type:        models.ComponentType(input.Type),
		Version:     input.Version,
		Format:      input.Format,
		DownloadURL: input.DownloadURL,
		Description: input.Description,
	}

	if err := h.db.Create(&pkg).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建包记录失败"})
		return
	}

	c.JSON(http.StatusCreated, pkg)
}

// DeletePackage deletes a package record and optionally its stored file
func (h *PackageHandler) DeletePackage(c *gin.Context) {
	id := c.Param("id")
	var pkg models.Package
	if err := h.db.First(&pkg, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "包不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取包信息失败"})
		}
		return
	}
	if err := deployer.RemoveUnusedPackageCache(h.db, h.basePath, &pkg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "清理已提取安装包缓存失败"})
		return
	}

	// Only remove files managed inside the package directory. A corrupted DB
	// record must never turn package deletion into an arbitrary-file delete,
	// and two normalized version records may legitimately share one artifact.
	if pkg.StoragePath != "" && h.validStoredPackagePath(pkg.StoragePath) {
		var sharedReferences int64
		if err := h.db.Model(&models.Package{}).
			Where("id <> ? AND storage_path = ?", pkg.ID, pkg.StoragePath).
			Count(&sharedReferences).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "检查安装包文件引用失败"})
			return
		}
		if sharedReferences == 0 {
			_ = os.Remove(pkg.StoragePath)
		}
	}

	if err := h.db.Delete(&pkg, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除包失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "包已删除"})
}

// UploadImage accepts a binary .tar.gz/.tgz/.zip package or a windows_exporter MSI.
func (h *PackageHandler) UploadImage(c *gin.Context) {
	// Form fields
	name := c.PostForm("name")
	compType := c.PostForm("type")
	version := c.PostForm("version")
	description := c.PostForm("description")

	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 不能为空"})
		return
	}
	if version != "" && !utils.ValidVersion(version) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "版本格式无效，仅允许字母数字与 . - _，可选 v 前缀"})
		return
	}

	// Get uploaded file
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传文件失败，请重新选择文件: " + err.Error()})
		return
	}
	defer file.Close()

	// Check the declared file size and independently bound the stream; clients
	// can lie about multipart sizes.
	if header.Size > MaxUploadSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "文件体积过大，最大允许 5GB"})
		return
	}

	lowerFilename := strings.ToLower(header.Filename)
	ext, format := "", "binary"
	switch {
	case strings.HasSuffix(lowerFilename, ".tar.gz"), strings.HasSuffix(lowerFilename, ".tgz"):
		ext = ".tar.gz"
	case strings.HasSuffix(lowerFilename, ".zip"):
		ext = ".zip"
	case strings.HasSuffix(lowerFilename, ".msi"):
		if compType != string(models.ComponentWindowsExporter) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "MSI 离线包仅用于 windows_exporter"})
			return
		}
		ext = ".msi"
		format = "msi"
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持 .tar.gz、.tgz、.zip 或 windows_exporter .msi 离线包"})
		return
	}
	if format == "binary" && compType == "" {
		compType = string(detectComponentType(header.Filename))
	}
	if !h.componentTypeAllowed(compType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组件类型未知或已停用，请选择可用类型"})
		return
	}

	filename := sanitizeFilename(name) + "_" + sanitizeVersion(version) + ext
	storagePath, err := h.packageStoragePath(filename)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "包存储路径无效，请检查名称和版本"})
		return
	}

	// Calculate checksum while saving
	hasher := sha256.New()

	// Exclusive creation prevents a pre-existing symlink from redirecting the
	// streamed upload outside the package directory.
	outFile, err := os.OpenFile(storagePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "包文件已存在或无法创建，请更换名称或检查存储目录"})
		return
	}
	defer outFile.Close()

	multiWriter := io.MultiWriter(hasher, outFile)
	written, err := io.Copy(multiWriter, io.LimitReader(file, MaxUploadSize+1))
	if err != nil {
		os.Remove(storagePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写入文件失败: " + err.Error()})
		return
	}
	if written > MaxUploadSize {
		os.Remove(storagePath)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "文件体积过大，最大允许 5GB"})
		return
	}
	if written == 0 {
		os.Remove(storagePath)
		c.JSON(http.StatusBadRequest, gin.H{"error": "上传文件不能为空"})
		return
	}
	if err := outFile.Sync(); err != nil {
		os.Remove(storagePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "落盘离线包失败: " + err.Error()})
		return
	}
	if err := outFile.Close(); err != nil {
		os.Remove(storagePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "关闭离线包文件失败: " + err.Error()})
		return
	}
	if format == "msi" && !hasMSIMagic(storagePath) {
		os.Remove(storagePath)
		c.JSON(http.StatusBadRequest, gin.H{"error": "上传文件不是有效的 MSI（缺少 Compound File 签名）"})
		return
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))

	pkg := models.Package{
		Name:        name,
		Type:        models.ComponentType(compType),
		Version:     version,
		Format:      format,
		Size:        written,
		Checksum:    checksum,
		StoragePath: storagePath,
		Description: description,
	}

	if err := h.db.Create(&pkg).Error; err != nil {
		os.Remove(storagePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存包记录失败"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"message": "二进制离线包上传成功", "package": pkg})
}

// DownloadOfficial starts an observable background download for one official
// component package. The client polls /jobs/:id for stages, byte progress and
// logs instead of holding one request open for several minutes.
func (h *PackageHandler) DownloadOfficial(c *gin.Context) {
	var req struct {
		Type    string `json:"type" binding:"required"`
		Version string `json:"version" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if !utils.IsValidComponentType(req.Type) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该组件类型不支持官方包下载"})
		return
	}
	if _, ok := deployer.BuiltinVersions[req.Type]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该组件类型没有官方包源，请手动上传包"})
		return
	}
	if !utils.ValidVersion(req.Version) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "版本格式无效，仅允许字母数字与 . - _，可选 v 前缀"})
		return
	}

	job, err := startJob("package-download", []string{req.Type + " " + req.Version})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	updateJobProgress(job.ID, 0, "等待执行", "下载任务已创建", 0, 0)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
		defer cancel()
		pkg, err := deployer.DownloadOfficialPackage(ctx, h.db, h.basePath, req.Type, req.Version, func(progress deployer.PackageDownloadProgress) {
			updateJobProgress(job.ID, 0, progress.Stage, progress.Detail, progress.Downloaded, progress.Total)
		})
		if err != nil {
			updatePackageJobError(job.ID, 0, err)
			finishJob(job.ID)
			return
		}
		updateJobProgress(job.ID, 0, "完成", "文件已保存并登记", pkg.Size, pkg.Size)
		updateJobItem(job.ID, 0, "done", "下载完成", "")
		finishJob(job.ID)
	}()
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "message": "包下载已开始"})
}

// DownloadAllOfficial downloads every built-in component package that does not
// already have a local binary package, so deployments work fully offline.
// Returns a per-component result summary (downloaded / already-exists / failed).
func (h *PackageHandler) DownloadAllOfficial(c *gin.Context) {
	// Downloading every built-in package can take minutes; run it as a
	// background job with parallel workers and let the frontend poll progress.
	types := make([]string, 0, len(deployer.BuiltinVersions))
	for t := range deployer.BuiltinVersions {
		types = append(types, t)
	}
	sort.Strings(types)
	keys := make([]string, 0, len(types))
	for _, t := range types {
		keys = append(keys, t+" "+deployer.BuiltinVersions[t])
		version := deployer.BuiltinVersions[t]
		if !utils.IsValidComponentType(t) || !utils.ValidVersion(version) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "内置包的类型或版本格式无效"})
			return
		}
	}
	job, err := startJob("package-download", keys)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		sem := make(chan struct{}, 3)
		var wg sync.WaitGroup
		for i, t := range types {
			wg.Add(1)
			go func(idx int, compType string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				version := deployer.BuiltinVersions[compType]
				updateJobProgress(job.ID, idx, "检查本地包", "检查是否已经下载", 0, 0)
				if _, err := deployer.FindAvailablePackage(h.db, h.basePath, compType, version); err == nil {
					updateJobItem(job.ID, idx, "done", "已存在", "")
					return
				}
				if _, err := deployer.DownloadOfficialPackage(ctx, h.db, h.basePath, compType, version, func(progress deployer.PackageDownloadProgress) {
					updateJobProgress(job.ID, idx, progress.Stage, progress.Detail, progress.Downloaded, progress.Total)
				}); err != nil {
					updatePackageJobError(job.ID, idx, err)
					return
				}
				updateJobItem(job.ID, idx, "done", "下载完成", "")
			}(i, t)
		}
		wg.Wait()
		finishJob(job.ID)
	}()
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "message": "正在后台下载包"})
}

// DownloadImage streams a stored package file to the client
func (h *PackageHandler) DownloadImage(c *gin.Context) {
	id := c.Param("id")
	var pkg models.Package
	if err := h.db.First(&pkg, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "包不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取包信息失败"})
		}
		return
	}

	if pkg.StoragePath == "" || !h.validStoredPackagePath(pkg.StoragePath) {
		c.JSON(http.StatusNotFound, gin.H{"error": "包文件不存在，请重新下载或上传"})
		return
	}

	file, err := os.Open(pkg.StoragePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "打开包文件失败"})
		return
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取包文件信息失败"})
		return
	}

	// Determine content type
	contentType := "application/octet-stream"
	if strings.HasSuffix(pkg.StoragePath, ".tar") {
		contentType = "application/x-tar"
	} else if strings.HasSuffix(pkg.StoragePath, ".tar.gz") || strings.HasSuffix(pkg.StoragePath, ".tgz") {
		contentType = "application/gzip"
	}

	// Set headers for file download
	filename := sanitizeFilename(pkg.Name) + "_" + sanitizeVersion(pkg.Version)
	if pkg.Format == "docker" {
		filename += ".tar"
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", fmt.Sprintf("%d", fi.Size()))
	c.Header("X-Checksum-SHA256", pkg.Checksum)

	c.File(pkg.StoragePath)
}

// PushImage loads a stored Docker tar image into the local Docker daemon
func (h *PackageHandler) PushImage(c *gin.Context) {
	id := c.Param("id")
	var pkg models.Package
	if err := h.db.First(&pkg, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "包不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取包信息失败"})
		}
		return
	}

	if pkg.StoragePath == "" || !h.validStoredPackagePath(pkg.StoragePath) {
		c.JSON(http.StatusNotFound, gin.H{"error": "包文件不存在，请重新下载或上传"})
		return
	}

	// Check if docker is available
	if _, err := exec.LookPath("docker"); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "docker 不可用，请先安装并启动 docker"})
		return
	}

	// Run docker load
	cmd := exec.Command("docker", "load", "-i", pkg.StoragePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "加载镜像失败，请检查包格式和 docker 状态: " + err.Error(),
			"output": string(output),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "镜像加载成功", "output": strings.TrimSpace(string(output))})
}

// PullImage pulls a Docker image from a registry and saves it as a package
func (h *PackageHandler) PullImage(c *gin.Context) {
	var input struct {
		Image       string `json:"image" binding:"required"` // e.g. prom/prometheus:v2.40.0
		CompType    string `json:"type"`
		Description string `json:"description"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "输入无效: " + err.Error()})
		return
	}

	// Check if docker is available
	if _, err := exec.LookPath("docker"); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "docker 不可用，请先安装并启动 docker"})
		return
	}

	// Parse image name and tag
	parts := strings.Split(input.Image, ":")
	name := parts[0]
	version := "latest"
	if len(parts) > 1 {
		version = parts[1]
	}
	// Strip registry path from name for storage
	nameOnly := filepath.Base(name)
	compType := models.ComponentType(input.CompType)
	if compType == "" {
		compType = detectComponentType(nameOnly)
	}
	if !h.componentTypeAllowed(string(compType)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组件类型未知或已停用，请选择可用类型"})
		return
	}

	filename := sanitizeFilename(nameOnly) + "_" + sanitizeVersion(version) + ".tar"
	storagePath, err := h.packageStoragePath(filename)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "包存储路径无效，请检查名称和版本"})
		return
	}
	if _, err := os.Lstat(storagePath); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "包文件已存在，请更换版本或先删除现有包"})
		return
	}
	// Pull the image
	pullCmd := exec.Command("docker", "pull", input.Image)
	if pullOut, err := pullCmd.CombinedOutput(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "拉取镜像失败，请检查镜像地址和网络: " + err.Error(),
			"output": string(pullOut),
		})
		return
	}

	// Save to a unique regular temp file and atomically rename it into place;
	// docker must never write directly through a predictable symlink.
	tempFile, err := os.CreateTemp(filepath.Dir(storagePath), ".docker-save-*.tar")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建包临时文件失败，请检查存储空间和权限"})
		return
	}
	tempPath := tempFile.Name()
	tempFile.Close()
	defer os.Remove(tempPath)
	saveCmd := exec.Command("docker", "save", "-o", tempPath, input.Image)
	if saveOut, err := saveCmd.CombinedOutput(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "保存镜像失败，请检查 docker 状态和存储空间: " + err.Error(),
			"output": string(saveOut),
		})
		return
	}
	if err := os.Rename(tempPath, storagePath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "存储镜像包失败，请检查存储空间和权限"})
		return
	}

	// Calculate checksum
	checksum, err := calcFileSHA256(storagePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "计算包校验和失败"})
		return
	}

	// Get file size
	fi, err := os.Stat(storagePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取包文件信息失败"})
		return
	}

	pkg := models.Package{
		Name:        nameOnly,
		Type:        compType,
		Version:     version,
		Format:      "docker",
		Size:        fi.Size(),
		Checksum:    checksum,
		StoragePath: storagePath,
		Description: input.Description,
	}

	if err := h.db.Create(&pkg).Error; err != nil {
		os.Remove(storagePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存包记录失败"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"message": "镜像已拉取并保存", "package": pkg})
}

// Helper functions

func (h *PackageHandler) packageStoragePath(filename string) (string, error) {
	if !utils.SafePathSegment(filename) {
		return "", fmt.Errorf("unsafe package filename")
	}
	pkgDir := filepath.Join(h.basePath, "packages")
	path := filepath.Join(pkgDir, filename)
	if !utils.PathWithinBase(pkgDir, path) {
		return "", fmt.Errorf("package path escapes package directory")
	}
	return path, nil
}

func (h *PackageHandler) validStoredPackagePath(path string) bool {
	pkgDir := filepath.Join(h.basePath, "packages")
	if !utils.PathWithinBase(pkgDir, path) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func (h *PackageHandler) componentTypeAllowed(componentType string) bool {
	if utils.IsValidComponentType(componentType) {
		return true
	}
	if h.db == nil {
		return false
	}
	var count int64
	if h.db.Model(&models.ComponentDefinition{}).
		Where("type = ? AND enabled = ?", componentType, true).Count(&count).Error == nil && count > 0 {
		return true
	}
	// A deleted definition must not strand components that already carry an
	// immutable snapshot; they still need offline upgrade package uploads.
	count = 0
	return h.db.Model(&models.Component{}).
		Where("type = ? AND config LIKE ?", componentType, "%\""+models.ComponentDefinitionSnapshotKey+"\"%").
		Count(&count).Error == nil && count > 0
}

func sanitizeFilename(name string) string {
	// Replace problematic characters with underscore
	result := strings.ReplaceAll(name, "/", "_")
	result = strings.ReplaceAll(result, "\\", "_")
	result = strings.ReplaceAll(result, " ", "_")
	result = strings.ReplaceAll(result, ":", "_")
	return strings.ToLower(result)
}

func sanitizeVersion(version string) string {
	if version == "" {
		return "latest"
	}
	result := strings.ReplaceAll(version, ".", "_")
	result = strings.ReplaceAll(result, "-", "_")
	return result
}

func calcFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func hasMSIMagic(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var header [8]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return false
	}
	return header == [8]byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}
}

func detectComponentType(name string) models.ComponentType {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "prometheus"):
		return models.ComponentPrometheus
	case strings.Contains(lower, "grafana"):
		return models.ComponentGrafana
	case strings.Contains(lower, "alertmanager"):
		return models.ComponentAlertmanager
	case strings.Contains(lower, "loki"):
		return models.ComponentLoki
	case strings.Contains(lower, "promtail"):
		return models.ComponentPromtail
	case strings.Contains(lower, "windows_exporter"), strings.Contains(lower, "windows-exporter"):
		return models.ComponentWindowsExporter
	case strings.Contains(lower, "node_exporter"):
		return models.ComponentNodeExporter
	case strings.Contains(lower, "pushgateway"):
		return models.ComponentPushgateway
	case strings.Contains(lower, "blackbox"):
		return models.ComponentBlackbox
	default:
		return models.ComponentType("unknown")
	}
}
