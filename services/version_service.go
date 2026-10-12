package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

type VersionService struct {
	db *gorm.DB
}

var errVersionRefreshOffline = errors.New("离线模式已禁用在线版本检查；保留本地版本目录，可手动上传离线包")

// Each upstream permits one in-flight check across all service instances.
// A failed probe opens a shared circuit for 1–5 minutes, including later jobs.
type versionUpstream struct {
	gate      chan struct{}
	backoff   time.Duration
	retryAt   time.Time
	lastError string
}

var dockerVersionUpstream = versionUpstream{gate: make(chan struct{}, 1)}
var githubVersionUpstream = versionUpstream{gate: make(chan struct{}, 1)}
var versionHTTPClient = &http.Client{Timeout: 8 * time.Second}

type versionBackoffError struct {
	retryAt time.Time
	reason  string
}

func (e *versionBackoffError) Error() string {
	return fmt.Sprintf("上游暂不可用，暂停在线检查至 %s：%s", e.retryAt.Format(time.RFC3339), e.reason)
}

func (u *versionUpstream) fetch(ctx context.Context, fetch func() ([]string, error)) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Do not run the generic TCP availability probe here: it is not
	// context-aware and duplicates the bounded upstream request.
	if os.Getenv("PROMLIFECYCLE_OFFLINE") == "1" {
		return nil, errVersionRefreshOffline
	}
	select {
	case u.gate <- struct{}{}:
		defer func() { <-u.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if os.Getenv("PROMLIFECYCLE_OFFLINE") == "1" {
		return nil, errVersionRefreshOffline
	}
	if time.Now().Before(u.retryAt) {
		return nil, &versionBackoffError{retryAt: u.retryAt, reason: u.lastError}
	}
	versions, err := fetch()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if u.backoff == 0 {
			u.backoff = time.Minute
		} else {
			u.backoff = min(2*u.backoff, 5*time.Minute)
		}
		u.retryAt = time.Now().Add(u.backoff)
		u.lastError = err.Error()
		return nil, err
	}
	u.backoff = 0
	u.retryAt = time.Time{}
	u.lastError = ""
	return versions, nil
}

// Version refreshes are serialized per component type. The channel lock is
// context-aware so scheduler shutdown never waits behind an API refresh.
var versionRefreshLocks sync.Map

func versionRefreshLock(componentType string) chan struct{} {
	lock, _ := versionRefreshLocks.LoadOrStore(componentType, make(chan struct{}, 1))
	return lock.(chan struct{})
}

func NewVersionService(db *gorm.DB) *VersionService {
	return &VersionService{db: db}
}

// sortedComponentTypes returns component types in deterministic order.
func SortedComponentTypes() []string {
	keys := make([]string, 0, len(componentImages))
	for k := range componentImages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Component images mapping for Docker Hub
var componentImages = map[string]string{
	"prometheus":        "prom/prometheus",
	"grafana":           "grafana/grafana",
	"alertmanager":      "prom/alertmanager",
	"windows_exporter":  "prometheuscommunity/windows-exporter",
	"loki":              "grafana/loki",
	"promtail":          "grafana/promtail",
	"node_exporter":     "prom/node-exporter",
	"blackbox_exporter": "prom/blackbox-exporter",
	"pushgateway":       "prom/pushgateway",
	"thanos":            "quay.io/thanos/thanos",
}

// Common stable versions for each component (kept in sync with official release tags)
var defaultVersions = map[string][]string{
	"prometheus":        {"v3.1.0", "v3.0.1", "v2.55.0", "v2.54.0", "v2.53.0", "v2.52.0", "v2.51.2", "v2.50.1", "v2.49.1", "v2.48.1", "v2.47.2", "v2.45.0"},
	"grafana":           {"12.0.0", "11.5.0", "11.4.0", "11.3.2", "11.2.0", "11.1.0", "11.0.0", "10.4.0", "10.3.0", "10.2.0", "10.1.0", "10.0.0", "9.5.0"},
	"alertmanager":      {"v0.28.0", "v0.27.0", "v0.26.0", "v0.25.0", "v0.24.0", "v0.23.0"},
	"loki":              {"3.4.0", "3.3.0", "3.2.0", "3.1.0", "3.0.0", "2.9.8", "2.9.6", "2.9.2"},
	"promtail":          {"3.4.0", "3.3.0", "3.2.0", "3.1.0", "3.0.0", "2.9.8", "2.9.6", "2.9.2"},
	"node_exporter":     {"1.9.0", "1.8.2", "1.8.1", "1.8.0", "1.7.0", "1.6.1", "1.6.0"},
	"blackbox_exporter": {"v0.26.0", "v0.25.0", "v0.24.0", "v0.23.0", "v0.22.0"},
	"pushgateway":       {"v1.9.0", "v1.8.0", "v1.7.0", "v1.6.2", "v1.6.1", "v1.6.0", "v1.5.0"},
	"thanos":            {"v0.39.2", "v0.39.1", "v0.38.0", "v0.37.2", "v0.36.0", "v0.35.0", "v0.34.1"},
}

// InitDefaultVersions populates the database with common stable versions if empty
func (v *VersionService) InitDefaultVersions() error {
	// Idempotent: insert any missing built-in versions (existing records are kept).
	missing := 0
	for compType, vers := range defaultVersions {
		image := componentImages[compType]
		if image == "" {
			continue
		}
		for _, ver := range vers {
			var count int64
			v.db.Model(&models.ComponentVersion{}).Where("type = ? AND version = ?", compType, ver).Count(&count)
			if count > 0 {
				continue
			}
			missing++
			v.db.Create(&models.ComponentVersion{
				Type:    models.ComponentType(compType),
				Version: ver,
				Image:   fmt.Sprintf("%s:%s", image, ver),
				Source:  "official",
			})
		}
	}
	if missing > 0 {
		log.Printf("[Versions] Added %d missing built-in versions", missing)
	}
	return nil
}

// GetVersionsByType returns all versions for a component type
func (v *VersionService) GetVersionsByType(compType string) []models.ComponentVersion {
	var versions []models.ComponentVersion
	v.db.Where("type = ?", compType).Find(&versions)
	sort.SliceStable(versions, func(i, j int) bool {
		return compareVersion(versions[i].Version, versions[j].Version) > 0
	})
	return versions
}

// compareVersion compares two version strings numerically (1 = a>b, -1 = a<b).
func compareVersion(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		va, vb := 0, 0
		if i < len(pa) {
			va = pa[i]
		}
		if i < len(pb) {
			vb = pb[i]
		}
		if va != vb {
			if va > vb {
				return 1
			}
			return -1
		}
	}
	return 0
}

func parseVersion(v string) []int {
	s := strings.TrimPrefix(v, "v")
	nums := []int{}
	for _, p := range strings.Split(s, ".") {
		if n, err := strconv.Atoi(p); err == nil {
			nums = append(nums, n)
		}
	}
	return nums
}

// FetchVersionsFromDockerHub fetches latest versions from Docker Hub API
func (v *VersionService) FetchVersionsFromDockerHub(ctx context.Context, compType string) ([]string, error) {
	if componentImages[compType] == "" {
		return nil, fmt.Errorf("unknown component type: %s", compType)
	}
	upstream := &dockerVersionUpstream
	if compType == "thanos" || compType == "windows_exporter" {
		upstream = &githubVersionUpstream
	}
	return upstream.fetch(ctx, func() ([]string, error) {
		return fetchComponentVersions(ctx, compType)
	})
}

func fetchComponentVersions(ctx context.Context, compType string) ([]string, error) {
	if compType == "thanos" {
		return fetchGitHubReleaseVersions(ctx, "thanos-io/thanos")
	}
	if compType == "windows_exporter" {
		return fetchGitHubReleaseVersions(ctx, "prometheus-community/windows_exporter")
	}
	image := componentImages[compType]
	if image == "" {
		return nil, fmt.Errorf("unknown component type: %s", compType)
	}

	// Docker Hub Tags API
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags?page_size=50&ordering=last_updated", image)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := versionHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Docker Hub tags returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}

	var result struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	versions := []string{}
	for _, r := range result.Results {
		ver := r.Name
		// Filter out non-version tags and Docker image variants. Only plain
		// numeric versions (v3.13.2 / 11.1.0) correspond to official binary
		// release packages, so tags like v3.13.2-distroless or -busybox must
		// not appear as deployable versions.
		if strings.Contains(ver, "latest") || strings.Contains(ver, "beta") ||
			strings.Contains(ver, "alpha") || strings.Contains(ver, "rc") ||
			strings.Contains(ver, "debug") || strings.Contains(ver, "dev") ||
			strings.Contains(ver, "-arm") || strings.Contains(ver, "bookworm") ||
			strings.Contains(ver, "debian") || strings.Contains(ver, "distroless") ||
			strings.Contains(ver, "busybox") || strings.Contains(ver, "windowsservercore") ||
			strings.Contains(ver, "nanoserver") || strings.Contains(ver, "alpine") ||
			strings.Contains(ver, "musl") || strings.Contains(ver, "slim") ||
			strings.Contains(ver, "ubi") {
			continue
		}
		if !isPlainVersion(ver) {
			continue
		}
		versions = append(versions, ver)
		if len(versions) >= 20 {
			break
		}
	}

	return versions, nil
}

func fetchGitHubReleaseVersions(ctx context.Context, repository string) ([]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases?per_page=30", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "promlifecycle-version-refresh")
	response, err := versionHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub releases returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	var releases []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(releases))
	for _, release := range releases {
		if release.Draft || release.Prerelease || !isPlainVersion(release.TagName) {
			continue
		}
		versions = append(versions, release.TagName)
		if len(versions) == 20 {
			break
		}
	}
	return versions, nil
}

// isPlainVersion reports whether a tag looks like a plain numeric release
// version (e.g. v3.13.2, 3.13.2, 11.1.0), optionally with an extra dot
// segment (e.g. 1.0.0.1).
func isPlainVersion(v string) bool {
	s := strings.TrimPrefix(v, "v")
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// RefreshVersions fetches latest versions from Docker Hub and updates database
func (v *VersionService) RefreshVersions(ctx context.Context, compType string) ([]models.ComponentVersion, error) {
	newVersions, err := v.FetchVersionsFromDockerHub(ctx, compType)
	if err != nil {
		return nil, err
	}

	image := componentImages[compType]
	if image == "" {
		return nil, fmt.Errorf("unknown component type: %s", compType)
	}

	var added []models.ComponentVersion
	db := v.db.WithContext(ctx)
	for _, ver := range newVersions {
		if err := ctx.Err(); err != nil {
			return added, err
		}
		var existing models.ComponentVersion
		result := db.Where("type = ? AND version = ?", compType, ver).First(&existing)
		imageTag := fmt.Sprintf("%s:%s", image, ver)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			cv := models.ComponentVersion{
				Type:    models.ComponentType(compType),
				Version: ver,
				Image:   imageTag,
				Source:  "official",
			}
			if err := db.Create(&cv).Error; err != nil {
				return added, err
			}
			added = append(added, cv)
		} else if result.Error != nil {
			return added, result.Error
		} else if err := db.Model(&existing).Update("image", imageTag).Error; err != nil {
			return added, err
		}
	}
	return added, ctx.Err()
}

// VersionRefreshResult separates catalog refresh from package acquisition:
// discovering a version does not imply its package was downloaded.
type VersionRefreshResult struct {
	Type           string     `json:"type"`
	Status         string     `json:"status"` // success / failed / skipped / canceled
	Added          int        `json:"added"`
	DownloadStatus string     `json:"download_status"` // not_needed / downloaded / failed / canceled
	DownloadError  string     `json:"download_error,omitempty"`
	Error          string     `json:"error,omitempty"`
	Reason         string     `json:"reason,omitempty"` // offline / backoff
	RetryAt        *time.Time `json:"retry_at,omitempty"`
	cause          error
}

func (r VersionRefreshResult) Err() error {
	return r.cause
}

func (r VersionRefreshResult) Detail() string {
	detail := fmt.Sprintf("新增 %d 个版本", r.Added)
	if r.Status == "success" && r.Added == 0 {
		detail = "版本目录检查成功，无新版本"
	}
	switch r.DownloadStatus {
	case "downloaded":
		detail += "；最新版离线包已入库"
	case "failed":
		detail += "；离线包下载失败：" + r.DownloadError
	case "canceled":
		detail += "；离线包下载已取消：" + r.DownloadError
	}
	if r.Error != "" {
		detail += "；" + r.Error
	}
	return detail
}

func (r *VersionRefreshResult) setError(ctx context.Context, err error) {
	r.cause = err
	r.Error = err.Error()
	r.Status = "failed"
	var backoff *versionBackoffError
	switch {
	case ctx.Err() != nil:
		r.Status = "canceled"
		r.cause = ctx.Err()
	case errors.Is(err, errVersionRefreshOffline):
		r.Status, r.Reason = "skipped", "offline"
	case errors.As(err, &backoff):
		r.Status, r.Reason = "skipped", "backoff"
		r.RetryAt = &backoff.retryAt
	}
}

type VersionRefreshSummary struct {
	Status         string                 `json:"status"` // success / partial / failed / offline / skipped / canceled
	Total          int                    `json:"total"`
	Succeeded      int                    `json:"succeeded"`
	Failed         int                    `json:"failed"`
	Skipped        int                    `json:"skipped"`
	Canceled       int                    `json:"canceled"`
	Added          int                    `json:"added"`
	Downloaded     int                    `json:"downloaded"`
	DownloadFailed int                    `json:"download_failed"`
	Results        []VersionRefreshResult `json:"results"`
}

func SummarizeVersionRefresh(results []VersionRefreshResult) VersionRefreshSummary {
	s := VersionRefreshSummary{Status: "success", Total: len(results), Results: results}
	offline := 0
	for _, r := range results {
		s.Added += r.Added
		switch r.Status {
		case "success":
			s.Succeeded++
		case "failed":
			s.Failed++
		case "skipped":
			s.Skipped++
			if r.Reason == "offline" {
				offline++
			}
		case "canceled":
			s.Canceled++
		}
		switch r.DownloadStatus {
		case "downloaded":
			s.Downloaded++
		case "failed":
			s.DownloadFailed++
		case "canceled":
			s.Canceled++
		}
	}
	switch {
	case s.Canceled > 0:
		s.Status = "canceled"
	case s.Total > 0 && offline == s.Total:
		s.Status = "offline"
	case s.Succeeded == 0 && s.Failed > 0:
		s.Status = "failed"
	case s.Total == 0 || s.Skipped == s.Total:
		s.Status = "skipped"
	case s.Failed > 0 || s.Skipped > 0 || s.DownloadFailed > 0:
		s.Status = "partial"
	}
	return s
}

func (s VersionRefreshSummary) Err() error {
	var errs []error
	for _, r := range s.Results {
		// Offline is an intentional operational mode, not a scheduler error.
		if r.Err() != nil && r.Reason != "offline" {
			errs = append(errs, fmt.Errorf("%s: %w", r.Type, r.Err()))
		}
	}
	return errors.Join(errs...)
}

func (s VersionRefreshSummary) Detail() string {
	return fmt.Sprintf("版本刷新 %s：检查成功 %d/%d，新增 %d，失败 %d，跳过 %d，取消 %d；离线包入库 %d，下载失败 %d",
		s.Status, s.Succeeded, s.Total, s.Added, s.Failed, s.Skipped, s.Canceled, s.Downloaded, s.DownloadFailed)
}

// RefreshAndAutoDownload preserves partial catalog writes and download failures.
func (v *VersionService) RefreshAndAutoDownload(ctx context.Context, compType, basePath string) VersionRefreshResult {
	result := VersionRefreshResult{Type: compType, Status: "success", DownloadStatus: "not_needed"}
	if err := ctx.Err(); err != nil {
		result.setError(ctx, err)
		return result
	}
	lock := versionRefreshLock(compType)
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-ctx.Done():
		result.setError(ctx, ctx.Err())
		return result
	}
	added, err := v.RefreshVersions(ctx, compType)
	result.Added = len(added)
	if err != nil {
		result.setError(ctx, err)
		return result
	}
	// Acquisition is desired-state reconciliation, not a side effect of INSERT:
	// retry the latest known release after a transient download failure.
	var versions []models.ComponentVersion
	if err := v.db.WithContext(ctx).Where("type = ?", compType).Find(&versions).Error; err != nil {
		result.setError(ctx, err)
		return result
	}
	if len(versions) == 0 {
		return result
	}
	latest := versions[0]
	for _, version := range versions[1:] {
		if compareVersion(version.Version, latest.Version) > 0 {
			latest = version
		}
	}
	if _, err := deployer.FindAvailablePackage(v.db, basePath, compType, latest.Version); err == nil {
		return result
	}
	if _, err := deployer.DownloadOfficialPackage(ctx, v.db, basePath, compType, latest.Version, nil); err != nil {
		result.DownloadStatus = "failed"
		result.DownloadError = err.Error()
		result.cause = err
		if ctx.Err() != nil {
			result.DownloadStatus = "canceled"
			result.cause = ctx.Err()
		}
	} else {
		result.DownloadStatus = "downloaded"
	}
	return result
}

// RefreshAllAndAutoDownload uses the same bounded worker path for API jobs and
// scheduler runs. The optional observer receives each completed result.
func (v *VersionService) RefreshAllAndAutoDownload(ctx context.Context, basePath string, observe func(int, VersionRefreshResult)) VersionRefreshSummary {
	types := SortedComponentTypes()
	results := make([]VersionRefreshResult, len(types))
	work := make(chan int)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				result := v.RefreshAndAutoDownload(ctx, types[index], basePath)
				results[index] = result
				if observe != nil {
					observe(index, result)
				}
			}
		}()
	}
	for index := range types {
		work <- index
	}
	close(work)
	wg.Wait()
	return SummarizeVersionRefresh(results)
}
