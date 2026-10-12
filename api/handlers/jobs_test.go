package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestJobProgressReturnsStableSnapshot(t *testing.T) {
	job, err := startJob("package-download", []string{"prometheus v2.53.0"})
	if err != nil {
		t.Fatal(err)
	}
	updateJobProgress(job.ID, 0, "下载文件", "github.com / prometheus.tar.gz", 25, 100)

	snapshot, ok := getJob(job.ID)
	if !ok {
		t.Fatal("job snapshot not found")
	}
	item := snapshot.Items[0]
	if item.Status != "running" || item.Progress != 25 || item.Downloaded != 25 || item.TotalBytes != 100 {
		t.Fatalf("unexpected progress snapshot: %+v", item)
	}

	updateJobProgress(job.ID, 0, "下载文件", "github.com / prometheus.tar.gz", 75, 100)
	if snapshot.Items[0].Progress != 25 || snapshot.Items[0].Downloaded != 25 {
		t.Fatalf("returned snapshot mutated after store update: %+v", snapshot.Items[0])
	}

	updateJobItem(job.ID, 0, "done", "下载完成", "")
	finishJob(job.ID)
	finished, _ := getJob(job.ID)
	if finished.Status != "done" || finished.Done != 1 || finished.Items[0].Progress != 100 {
		t.Fatalf("unexpected finished job: %+v", finished)
	}
}

func TestPackageJobAggregationReflectsOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		want     string
	}{
		{"all failed", []string{"failed", "failed"}, "failed"},
		{"partial", []string{"done", "failed"}, "partial"},
		{"offline", []string{"offline", "offline"}, "offline"},
		{"skipped", []string{"skipped", "skipped"}, "skipped"},
		{"canceled", []string{"done", "canceled"}, "canceled"},
		{"unfinished", []string{"done", "pending"}, "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := startJob("package-download", []string{"first", "second"})
			if err != nil {
				t.Fatal(err)
			}
			for i, status := range tc.statuses {
				if status == "offline" {
					updatePackageJobError(job.ID, i, deployer.ErrPackageOffline)
				} else {
					updateJobItem(job.ID, i, status, "", "")
				}
			}
			finishJob(job.ID)
			result, _ := getJob(job.ID)
			if result.Status != tc.want || result.Done != 2 || result.FinishedAt == nil {
				t.Fatalf("outcome hidden: %+v", result)
			}
		})
	}
}

func TestJobsSurviveRestartAndAreDiscoverable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "jobs.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	jobStore.Lock()
	previousDB, previousJobs := jobStore.db, jobStore.jobs
	jobStore.Unlock()
	t.Cleanup(func() {
		jobStore.Lock()
		jobStore.db, jobStore.jobs = previousDB, previousJobs
		jobStore.Unlock()
		sqlDB, _ := db.DB()
		sqlDB.Close()
	})
	if err := InitBackgroundJobs(db); err != nil {
		t.Fatal(err)
	}
	running, err := startJob("package-download", []string{"loki 3.2.0", "promtail 3.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	updateJobItem(running.ID, 0, "done", "已存在", "")
	updateJobProgress(running.ID, 1, "下载文件", "", 5, 100)
	finished, err := startJob("package-download", []string{"prometheus 2.53.0"})
	if err != nil {
		t.Fatal(err)
	}
	updateJobItem(finished.ID, 0, "failed", "", "upstream unavailable")
	finishJob(finished.ID)
	if err := InitBackgroundJobs(db); err != nil {
		t.Fatal(err)
	}
	restored, ok := getJob(running.ID)
	if !ok || restored.Status != "interrupted" || restored.Items[0].Status != "done" || restored.Items[1].Status != "interrupted" || restored.Done != 2 {
		t.Fatalf("restart lost actual progress: %+v", restored)
	}
	completed, ok := getJob(finished.ID)
	if !ok || completed.Status != "failed" || completed.Items[0].Error != "upstream unavailable" {
		t.Fatalf("restart rewrote terminal outcome: %+v", completed)
	}
	router := gin.New()
	router.GET("/jobs", ListJobs)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/jobs?kind=package-download", nil))
	var response struct {
		Jobs []bgJob `json:"jobs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(response.Jobs) != 2 {
		t.Fatalf("browser cannot recover jobs: %s", recorder.Body.String())
	}
}
