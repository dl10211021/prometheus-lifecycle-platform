package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type bgJobLog struct {
	At      string `json:"at"`
	Message string `json:"message"`
}

// bgJobItem is a single unit of a background job.
type bgJobItem struct {
	Key        string     `json:"key"`
	Status     string     `json:"status"` // pending / running / done / failed / skipped / canceled
	Stage      string     `json:"stage,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	Error      string     `json:"error,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Progress   int        `json:"progress"`
	Downloaded int64      `json:"downloaded_bytes,omitempty"`
	TotalBytes int64      `json:"total_bytes,omitempty"`
	Logs       []bgJobLog `json:"logs,omitempty"`
}

// bgJob tracks the progress of an asynchronous batch operation so long-running
// network tasks (version refresh / package downloads) never block the request.
type bgJob struct {
	ID               string                          `json:"id"`
	Kind             string                          `json:"kind"`
	Status           string                          `json:"status"` // running / done / failed / partial / offline / skipped / canceled
	Total            int                             `json:"total"`
	Done             int                             `json:"done"`
	Items            []bgJobItem                     `json:"items"`
	StartedAt        time.Time                       `json:"started_at"`
	FinishedAt       *time.Time                      `json:"finished_at,omitempty"`
	Message          string                          `json:"message,omitempty"`
	VersionRefresh   *services.VersionRefreshSummary `json:"version_refresh,omitempty"`
	PackageDownload  *services.VersionRefreshSummary `json:"package_download,omitempty"`
	PersistenceError string                          `json:"persistence_error,omitempty"`
}

var jobStore = struct {
	sync.Mutex
	jobs map[string]*bgJob
	db   *gorm.DB
}{jobs: map[string]*bgJob{}}

func newJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func startJob(kind string, keys []string) (*bgJob, error) {
	jobStore.Lock()
	defer jobStore.Unlock()
	// Prune finished jobs so the map does not grow forever.
	if len(jobStore.jobs) > 40 {
		cutoff := time.Now().Add(-2 * time.Hour)
		for id, j := range jobStore.jobs {
			if j.FinishedAt != nil && j.FinishedAt.Before(cutoff) {
				if jobStore.db != nil {
					if err := jobStore.db.Delete(&backgroundJobRecord{}, "id = ?", id).Error; err != nil {
						return nil, err
					}
				}
				delete(jobStore.jobs, id)
			}
		}
	}
	id, err := newJobID()
	if err != nil {
		return nil, err
	}
	j := &bgJob{
		ID:        id,
		Kind:      kind,
		Status:    "running",
		Total:     len(keys),
		StartedAt: time.Now(),
	}
	for _, k := range keys {
		j.Items = append(j.Items, bgJobItem{Key: k, Status: "pending"})
	}
	jobStore.jobs[j.ID] = j
	if err := persistJobLocked(j); err != nil {
		delete(jobStore.jobs, j.ID)
		return nil, err
	}
	snapshot := *j
	return &snapshot, nil
}

func updateJobItem(jobID string, idx int, status, detail, errMsg string) {
	jobStore.Lock()
	defer jobStore.Unlock()
	j, ok := jobStore.jobs[jobID]
	if !ok || idx < 0 || idx >= len(j.Items) {
		return
	}
	item := &j.Items[idx]
	item.Status = status
	item.Detail = detail
	item.Error = errMsg
	if status == "done" {
		item.Progress = 100
	}
	message := detail
	if errMsg != "" {
		message = "失败：" + errMsg
	}
	appendJobLog(item, message)
	recountJobDone(j)
	saveJobLocked(j)
}

func updateJobProgress(jobID string, idx int, stage, detail string, downloaded, total int64) {
	jobStore.Lock()
	defer jobStore.Unlock()
	j, ok := jobStore.jobs[jobID]
	if !ok || idx < 0 || idx >= len(j.Items) {
		return
	}
	item := &j.Items[idx]
	changed := stage != item.Stage || detail != item.Detail
	item.Status = "running"
	item.Stage = stage
	item.Detail = detail
	item.Error = ""
	item.Downloaded = downloaded
	item.TotalBytes = total
	if total > 0 {
		progress := downloaded * 100 / total
		if progress > 100 {
			progress = 100
		}
		item.Progress = int(progress)
	}
	if changed {
		message := stage
		if detail != "" {
			message += "：" + detail
		}
		appendJobLog(item, message)
	}
	if changed {
		saveJobLocked(j)
	}
}

func appendJobLog(item *bgJobItem, message string) {
	if message == "" {
		return
	}
	if n := len(item.Logs); n > 0 && item.Logs[n-1].Message == message {
		return
	}
	item.Logs = append(item.Logs, bgJobLog{
		At:      time.Now().Format("15:04:05"),
		Message: message,
	})
	if len(item.Logs) > 100 {
		item.Logs = append([]bgJobLog(nil), item.Logs[len(item.Logs)-100:]...)
	}
}

func recountJobDone(j *bgJob) {
	j.Done = 0
	for _, item := range j.Items {
		if item.Status == "done" || item.Status == "failed" || item.Status == "skipped" || item.Status == "canceled" || item.Status == "interrupted" {
			j.Done++
		}
	}
}

func finishJob(jobID string) {
	jobStore.Lock()
	defer jobStore.Unlock()
	if j, ok := jobStore.jobs[jobID]; ok {
		results := make([]services.VersionRefreshResult, 0, len(j.Items))
		interrupted := false
		for i := range j.Items {
			item := &j.Items[i]
			status := item.Status
			switch status {
			case "done":
				status = "success"
			case "pending", "running":
				item.Status, item.Error = "interrupted", "任务未完成，需重新发起"
				fallthrough
			case "interrupted":
				interrupted, status = true, "canceled"
			}
			result := services.VersionRefreshResult{Type: item.Key, Status: status, Reason: item.Reason, Error: item.Error}
			if status == "success" && item.Detail != "已存在" {
				result.DownloadStatus = "downloaded"
			}
			results = append(results, result)
		}
		summary := services.SummarizeVersionRefresh(results)
		j.PackageDownload = &summary
		j.Status = summary.Status
		if j.Status == "success" {
			j.Status = "done"
		}
		if interrupted {
			j.Status = "interrupted"
		}
		j.Message = fmt.Sprintf("包任务 %s：成功 %d/%d，失败 %d，跳过 %d，取消 %d", j.Status, summary.Succeeded, summary.Total, summary.Failed, summary.Skipped, summary.Canceled)
		recountJobDone(j)
		now := time.Now()
		j.FinishedAt = &now
		saveJobLocked(j)
	}
}

func getJob(jobID string) (*bgJob, bool) {
	jobStore.Lock()
	defer jobStore.Unlock()
	j, ok := jobStore.jobs[jobID]
	if !ok {
		return nil, false
	}
	snapshot := *j
	snapshot.Items = append([]bgJobItem(nil), j.Items...)
	for i := range snapshot.Items {
		snapshot.Items[i].Logs = append([]bgJobLog(nil), j.Items[i].Logs...)
	}
	return &snapshot, true
}

// GetJob returns the current state of a background batch job.
func GetJob(c *gin.Context) {
	id := c.Param("id")
	j, ok := getJob(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	status := http.StatusOK
	if j.PersistenceError != "" {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, j)
}

// backgroundJobRecord stores snapshots; no initiating browser is needed to
// recover a running job after navigation or an explicitly interrupted restart.
type backgroundJobRecord struct {
	ID        string `gorm:"primaryKey"`
	Snapshot  []byte `gorm:"not null"`
	UpdatedAt time.Time
}

func InitBackgroundJobs(db *gorm.DB) error {
	jobStore.Lock()
	defer jobStore.Unlock()
	if err := db.AutoMigrate(&backgroundJobRecord{}); err != nil {
		return err
	}
	var records []backgroundJobRecord
	if err := db.Find(&records).Error; err != nil {
		return err
	}
	jobs := make(map[string]*bgJob, len(records))
	for _, record := range records {
		var job bgJob
		if err := json.Unmarshal(record.Snapshot, &job); err != nil {
			return fmt.Errorf("restore job %s: %w", record.ID, err)
		}
		if job.FinishedAt == nil {
			job.Status, job.Message = "interrupted", "服务重启中断任务；请重新发起，已验证的包会被复用"
			now := time.Now()
			job.FinishedAt = &now
			for i := range job.Items {
				if job.Items[i].Status == "pending" || job.Items[i].Status == "running" {
					job.Items[i].Status, job.Items[i].Error = "interrupted", job.Message
				}
			}
			recountJobDone(&job)
			data, err := json.Marshal(&job)
			if err != nil {
				return err
			}
			if err := db.Model(&record).Update("snapshot", data).Error; err != nil {
				return err
			}
		}
		jobs[job.ID] = &job
	}
	jobStore.db, jobStore.jobs = db, jobs
	return nil
}

func persistJobLocked(job *bgJob) error {
	if jobStore.db == nil {
		return nil
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return jobStore.db.Save(&backgroundJobRecord{ID: job.ID, Snapshot: data}).Error
}

func saveJobLocked(job *bgJob) {
	job.PersistenceError = ""
	if err := persistJobLocked(job); err != nil {
		job.PersistenceError = "任务状态持久化失败: " + err.Error()
		log.Printf("[Jobs] %s: %s", job.ID, job.PersistenceError)
	}
}

func updatePackageJobError(jobID string, index int, err error) {
	status, reason := "failed", ""
	switch {
	case errors.Is(err, deployer.ErrPackageOffline):
		status, reason = "skipped", "offline"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = "canceled"
	}
	updateJobItem(jobID, index, status, "", err.Error())
	jobStore.Lock()
	defer jobStore.Unlock()
	if job, ok := jobStore.jobs[jobID]; ok && index >= 0 && index < len(job.Items) {
		job.Items[index].Reason = reason
		saveJobLocked(job)
	}
}

func ListJobs(c *gin.Context) {
	jobStore.Lock()
	defer jobStore.Unlock()
	jobs := make([]*bgJob, 0, len(jobStore.jobs))
	for _, job := range jobStore.jobs {
		if kind := c.Query("kind"); kind == "" || kind == job.Kind {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].StartedAt.After(jobs[j].StartedAt) })
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}
