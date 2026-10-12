package services

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRefreshAllReturnsImmediatelyWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	result := (&VersionService{}).RefreshAllAndAutoDownload(ctx, t.TempDir(), nil)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled refresh took %v", elapsed)
	}
	if result.Status != "canceled" || result.Canceled != len(componentImages) || !errors.Is(result.Err(), context.Canceled) {
		t.Fatalf("cancellation lost from summary: %#v, error=%v", result, result.Err())
	}
}

func TestRefreshLockWaitIsContextCancelable(t *testing.T) {
	componentType := "context-lock-test"
	lock := versionRefreshLock(componentType)
	lock <- struct{}{}
	defer func() { <-lock }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := (&VersionService{}).RefreshAndAutoDownload(ctx, componentType, t.TempDir())
	if result.Status != "canceled" || !errors.Is(result.Err(), context.DeadlineExceeded) {
		t.Fatalf("result=%#v, error=%v, want context deadline", result, result.Err())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("lock cancellation took %v", elapsed)
	}
}

type versionRoundTripFunc func(*http.Request) (*http.Response, error)

func (f versionRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func versionTestTransport(t *testing.T, transport versionRoundTripFunc) {
	t.Helper()
	client, docker, github := versionHTTPClient, dockerVersionUpstream, githubVersionUpstream
	versionHTTPClient = &http.Client{Transport: transport, Timeout: time.Second}
	dockerVersionUpstream = versionUpstream{gate: make(chan struct{}, 1)}
	githubVersionUpstream = versionUpstream{gate: make(chan struct{}, 1)}
	t.Cleanup(func() {
		versionHTTPClient, dockerVersionUpstream, githubVersionUpstream = client, docker, github
	})
}

func versionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "versions.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.ComponentVersion{}, &models.Package{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

func TestRefreshOfflinePreservesCatalogWithoutRequests(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "1")
	var requests atomic.Int32
	versionTestTransport(t, func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected network request")
	})
	db := versionTestDB(t)
	local := models.ComponentVersion{Type: models.ComponentPrometheus, Version: "v99.1.0", Image: "local/prometheus:v99.1.0", Source: "offline"}
	if err := db.Create(&local).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewVersionService(db)
	started := time.Now()
	summary := svc.RefreshAllAndAutoDownload(context.Background(), t.TempDir(), nil)
	if time.Since(started) > time.Second || requests.Load() != 0 {
		t.Fatalf("offline check was not immediate/network-free: requests=%d", requests.Load())
	}
	if summary.Status != "offline" || summary.Skipped != len(componentImages) || summary.Succeeded != 0 || summary.Added != 0 || summary.Downloaded != 0 {
		t.Fatalf("offline reported as a successful check: %#v", summary)
	}
	versions := svc.GetVersionsByType("prometheus")
	if len(versions) != 1 || versions[0].ID != local.ID || versions[0].Image != local.Image || versions[0].Source != local.Source {
		t.Fatalf("local catalog changed: %#v", versions)
	}
	for _, result := range summary.Results {
		if result.Reason != "offline" || result.Error == "" {
			t.Fatalf("missing offline reason: %#v", result)
		}
	}
}

func TestRefreshUpstreamFailureIsSharedAcrossTypesAndJobs(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "0")
	var requests atomic.Int32
	versionTestTransport(t, func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		// A client timeout is an upstream failure, not caller cancellation.
		return nil, context.DeadlineExceeded
	})
	svc := &VersionService{}
	first := svc.RefreshAllAndAutoDownload(context.Background(), t.TempDir(), nil)
	if first.Status != "failed" || first.Failed != 2 || first.Skipped != len(componentImages)-2 || first.Canceled != 0 || first.Err() == nil {
		t.Fatalf("total failure hidden: %#v", first)
	}
	second := (&VersionService{}).RefreshAllAndAutoDownload(context.Background(), t.TempDir(), nil)
	if second.Status != "skipped" || second.Skipped != len(componentImages) || second.Err() == nil || requests.Load() != 2 {
		t.Fatalf("backoff did not survive across service instances: requests=%d, summary=%#v", requests.Load(), second)
	}
	for _, result := range second.Results {
		if result.Reason != "backoff" || result.RetryAt == nil || result.Error == "" {
			t.Fatalf("missing retry/failure detail: %#v", result)
		}
	}
}

func TestRefreshPartialSuccessKeepsFailureDetails(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "0")
	versionTestTransport(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "hub.docker.com" {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("[]")), Header: make(http.Header)}, nil
	})
	summary := NewVersionService(versionTestDB(t)).RefreshAllAndAutoDownload(context.Background(), t.TempDir(), nil)
	if summary.Status != "partial" || summary.Succeeded != 2 || summary.Failed != 1 || summary.Skipped != len(componentImages)-3 || summary.Err() == nil {
		t.Fatalf("partial result lost: %#v", summary)
	}
	for _, result := range summary.Results {
		if result.Status != "success" && !strings.Contains(result.Error, "503") {
			t.Fatalf("upstream error detail lost: %#v", result)
		}
	}
}

func TestRefreshDownloadFailureRetainsNewVersions(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "0")
	versionTestTransport(t, func(*http.Request) (*http.Response, error) {
		// Connectivity disappears after discovery and before package acquisition.
		t.Setenv("PROMLIFECYCLE_OFFLINE", "1")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"results":[{"name":"v99.1.0"}]}`)), Header: make(http.Header)}, nil
	})
	svc := NewVersionService(versionTestDB(t))
	result := svc.RefreshAndAutoDownload(context.Background(), "prometheus", t.TempDir())
	summary := SummarizeVersionRefresh([]VersionRefreshResult{result})
	if summary.Status != "partial" || summary.Added != 1 || summary.Downloaded != 0 || summary.DownloadFailed != 1 || summary.Err() == nil || result.DownloadError == "" {
		t.Fatalf("failed download was counted as success: %#v", summary)
	}
	versions := svc.GetVersionsByType("prometheus")
	if len(versions) != 1 || versions[0].Version != "v99.1.0" {
		t.Fatalf("discovered version lost after failed download: %#v", versions)
	}
	// The catalog row now exists. A second successful discovery must retry
	// acquisition rather than turn the earlier failure into not_needed.
	t.Setenv("PROMLIFECYCLE_OFFLINE", "0")
	retry := svc.RefreshAndAutoDownload(context.Background(), "prometheus", t.TempDir())
	if retry.Added != 0 || retry.DownloadStatus != "failed" || retry.DownloadError == "" {
		t.Fatalf("missing package was not retried after discovery inserted the version: %#v", retry)
	}
}

func TestVersionUpstreamBackoffRecoversAndHonorsCancellation(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "0")
	upstream := versionUpstream{gate: make(chan struct{}, 1)}
	calls := 0
	fail := func() ([]string, error) { calls++; return nil, errors.New("upstream unavailable") }
	for attempt := range 5 {
		upstream.retryAt = time.Time{} // advance beyond the circuit's cooldown
		if _, err := upstream.fetch(context.Background(), fail); err == nil {
			t.Fatal("failure disappeared")
		}
		if delay := time.Until(upstream.retryAt); delay <= 0 || delay > 5*time.Minute {
			t.Fatalf("unbounded retry delay: %v", delay)
		}
		if _, err := upstream.fetch(context.Background(), fail); err == nil || calls != attempt+1 {
			t.Fatalf("open circuit sent another request: calls=%d, error=%v", calls, err)
		}
	}
	upstream.retryAt = time.Time{}
	versions, err := upstream.fetch(context.Background(), func() ([]string, error) { return []string{"1.0.0"}, nil })
	if err != nil || len(versions) != 1 {
		t.Fatalf("half-open circuit failed to recover: %v, %v", versions, err)
	}
	if _, err := upstream.fetch(context.Background(), func() ([]string, error) { return []string{"2.0.0"}, nil }); err != nil {
		t.Fatalf("successful probe did not close circuit: %v", err)
	}
	upstream.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = upstream.fetch(ctx, fail)
	<-upstream.gate
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("upstream lock ignored cancellation: %v", err)
	}
	ctx, cancelInFlight := context.WithCancel(context.Background())
	_, err = upstream.fetch(ctx, func() ([]string, error) { cancelInFlight(); return nil, ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation lost: %v", err)
	}
	if _, err := upstream.fetch(context.Background(), func() ([]string, error) { return []string{"3.0.0"}, nil }); err != nil {
		t.Fatalf("caller cancellation poisoned upstream circuit: %v", err)
	}
}
