package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
)

func (h *Handler) ListVersionsByType(c *gin.Context) {
	compType := c.Query("type")
	if compType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 type 参数"})
		return
	}
	versions := h.versionSvc.GetVersionsByType(compType)
	var packages []models.Package
	if err := h.db.Where("type = ? AND format IN ?", compType, []string{"binary", "zip", "msi"}).Find(&packages).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	available := make([]models.Package, 0, len(packages))
	for _, pkg := range packages {
		if deployer.ValidatePackage(h.basePath, pkg) == nil {
			available = append(available, pkg)
		}
	}
	packages = available
	type availableVersion struct {
		models.ComponentVersion
		PackageAvailable bool   `json:"package_available"`
		PackageID        uint   `json:"package_id,omitempty"`
		PackageFormat    string `json:"package_format,omitempty"`
		PackageSize      int64  `json:"package_size,omitempty"`
	}
	response := make([]availableVersion, 0, len(versions))
	seen := make(map[string]struct{}, len(versions))
	for _, version := range versions {
		want := strings.TrimPrefix(version.Version, "v")
		if _, exists := seen[want]; exists {
			continue
		}
		seen[want] = struct{}{}
		item := availableVersion{ComponentVersion: version}
		for _, pkg := range packages {
			if strings.TrimPrefix(pkg.Version, "v") != want || pkg.StoragePath == "" {
				continue
			}
			item.PackageAvailable = true
			item.PackageID = pkg.ID
			item.PackageFormat = pkg.Format
			item.PackageSize = pkg.Size
			break
		}
		response = append(response, item)
	}
	for _, pkg := range packages {
		want := strings.TrimPrefix(pkg.Version, "v")
		if _, exists := seen[want]; exists || pkg.StoragePath == "" {
			continue
		}
		seen[want] = struct{}{}
		response = append(response, availableVersion{
			ComponentVersion: models.ComponentVersion{
				Type: pkg.Type, Version: pkg.Version, Source: "offline", Checksum: pkg.Checksum,
			},
			PackageAvailable: true,
			PackageID:        pkg.ID,
			PackageFormat:    pkg.Format,
			PackageSize:      pkg.Size,
		})
	}
	c.JSON(http.StatusOK, gin.H{"component_type": compType, "versions": response})
}

// RefreshVersions checks upstream catalogs without replacing local versions.
func (h *Handler) RefreshVersions(c *gin.Context) {
	compType := c.Query("type")
	if compType == "" {
		job, err := startJob("version-refresh", services.SortedComponentTypes())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		go func() {
			// Background jobs outlive the initiating request, but never run
			// indefinitely. The service observes cancellation between items.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			summary := h.versionSvc.RefreshAllAndAutoDownload(ctx, h.basePath, func(index int, result services.VersionRefreshResult) {
				status := result.Status
				if status == "success" {
					status = "done"
				}
				if result.DownloadStatus == "failed" {
					status = "failed"
				} else if result.DownloadStatus == "canceled" {
					status = "canceled"
				}
				updateJobItem(job.ID, index, status, result.Detail(), result.Error)
			})
			finishVersionRefreshJob(job.ID, summary)
		}()
		c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "message": "正在后台刷新版本"})
		return
	}
	result := h.versionSvc.RefreshAndAutoDownload(c.Request.Context(), compType, h.basePath)
	summary := services.SummarizeVersionRefresh([]services.VersionRefreshResult{result})
	status := http.StatusOK
	switch summary.Status {
	case "failed", "skipped":
		status = http.StatusServiceUnavailable
	case "canceled":
		status = http.StatusRequestTimeout
	case "partial":
		status = http.StatusMultiStatus
	}
	c.JSON(status, gin.H{
		"message": summary.Detail(), "summary": summary,
		"versions": h.versionSvc.GetVersionsByType(compType),
	})
}

func finishVersionRefreshJob(jobID string, summary services.VersionRefreshSummary) {
	jobStore.Lock()
	defer jobStore.Unlock()
	if job, ok := jobStore.jobs[jobID]; ok {
		job.VersionRefresh = &summary
		job.Message = summary.Detail()
		job.Status = summary.Status
		if summary.Status == "success" {
			job.Status = "done"
		}
		now := time.Now()
		job.FinishedAt = &now
		saveJobLocked(job)
	}
}

// InitDefaultVersions initializes default versions
func (h *Handler) InitDefaultVersions() error {
	return h.versionSvc.InitDefaultVersions()
}
