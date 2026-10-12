package services

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestPrometheusArtifactVersionPlanExecuteRollback(t *testing.T) {
	for _, tc := range []struct{ name, source, target, want string }{
		{"metadata", "3.13.3", "3.13.4+promlifecycle.1", "3.13.4+promlifecycle.1"},
		{"metadata_with_legacy_prefix", "3.13.3+promlifecycle.1", " v3.13.4+promlifecycle.1 ", "3.13.4+promlifecycle.1"},
		{"ordinary_release_with_legacy_prefix", "v3.13.3", " v3.13.4 ", "3.13.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrometheusArtifactFixture(t, tc.source)
			sourceArtifact := strings.TrimPrefix(tc.source, "v")
			// The source package is needed for historical recovery. A different build
			// of the same numeric release must never replace it.
			addPrometheusArtifactPackage(t, f, 60, sourceArtifact, []byte("current-binary"))
			if strings.Contains(sourceArtifact, "+") {
				addPrometheusArtifactPackage(t, f, 61, strings.Split(sourceArtifact, "+")[0], []byte("WRONG historical official binary"))
			}
			wanted := []byte("exact target executable " + tc.want)
			addPrometheusArtifactPackage(t, f, 59, tc.want, wanted)
			if strings.Contains(tc.want, "+") {
				addPrometheusArtifactPackage(t, f, 58, strings.Split(tc.want, "+")[0], []byte("WRONG target official binary"))
			}
			actor := ChangeActor{ID: 1, Username: "artifact-fixture", Role: "system_admin"}
			after, _ := json.Marshal(upgradeChange{NewVersion: tc.target})
			plan, err := f.service.Create(context.Background(), ChangePlanRequest{Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: f.component.ID, Reason: "exact artifact upgrade", After: after}, actor)
			if err != nil {
				t.Fatal(err)
			}
			var evidence struct {
				Preflight struct {
					PackageID uint `json:"package_id"`
				} `json:"preflight"`
			}
			if err := json.Unmarshal(plan.Verification, &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Preflight.PackageID != 59 {
				t.Fatalf("preflight selected package%d instead of exact artifact59: %s", evidence.Preflight.PackageID, plan.Verification)
			}
			var target upgradeChange
			if err := json.Unmarshal(plan.After, &target); err != nil {
				t.Fatal(err)
			}
			if target.NewVersion != tc.want {
				t.Fatalf("plan lost artifact identity: %q want %q", target.NewVersion, tc.want)
			}
			result, err := f.service.Execute(context.Background(), plan.ID, actor)
			if err != nil {
				t.Fatalf("execute result=%s err=%v", artifactPlanStatus(result), err)
			}
			assertPrometheusArtifactRuntime(t, f, tc.want, wanted)
			result, err = f.service.Rollback(context.Background(), plan.ID, actor)
			if err != nil {
				t.Fatalf("rollback result=%s err=%v", artifactPlanStatus(result), err)
			}
			assertPrometheusArtifactRuntime(t, f, tc.source, []byte("current-binary"))
		})
	}
}

func TestPrometheusArtifactVersionMissingBuildNeverUsesBase(t *testing.T) {
	for _, source := range []string{"package", "version_catalog"} {
		t.Run(source, func(t *testing.T) {
			f := newPrometheusArtifactFixture(t, "3.13.3")
			if source == "package" {
				addPrometheusArtifactPackage(t, f, 58, "3.13.4", []byte("official release is not requested build"))
			} else if err := f.db.Create(&models.ComponentVersion{Type: models.ComponentPrometheus, Version: "3.13.4", Image: "prom/prometheus:v3.13.4", Source: "official"}).Error; err != nil {
				t.Fatal(err)
			}
			plan, err := f.service.Create(context.Background(), ChangePlanRequest{Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: f.component.ID, Reason: "missing exact artifact", After: json.RawMessage(`{"new_version":"3.13.4+promlifecycle.1"}`)}, ChangeActor{ID: 1, Username: "artifact-fixture", Role: "system_admin"})
			if err == nil || plan != nil {
				t.Fatalf("missing exact build admitted via %s: status=%s err=%v", source, artifactPlanStatus(plan), err)
			}
			var count int64
			if err := f.db.Model(&models.ChangePlan{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("rejected artifact persisted a plan")
			}
			assertPrometheusArtifactRuntime(t, f, "3.13.3", []byte("current-binary"))
		})
	}
}

func newPrometheusArtifactFixture(t *testing.T, version string) *compatibilityFixture {
	t.Helper()
	f := newCompatibilityFixture(t, models.ComponentPrometheus, version, "0.39.2")
	artifact := strings.TrimPrefix(strings.TrimSpace(version), "v")
	f.binary(t, artifact)
	if err := os.Remove(filepath.Join(f.service.factory.BasePath(), "bin", "prometheus-"+artifact)); err != nil {
		t.Fatal(err)
	}
	f.service.client = &http.Client{Transport: migrationReadinessTransport{}}
	if err := f.db.AutoMigrate(&models.Package{}); err != nil {
		t.Fatal(err)
	}
	return f
}

func addPrometheusArtifactPackage(t *testing.T, f *compatibilityFixture, id uint, version string, binary []byte) {
	t.Helper()
	root := filepath.Join(f.service.factory.BasePath(), "packages")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "prometheus/prometheus", Mode: 0755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "prometheus-"+version+".tar.gz")
	if err := os.WriteFile(path, archive.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	pkg := models.Package{ID: id, Name: "prometheus-" + version, Type: models.ComponentPrometheus, Version: version, Format: "binary", StoragePath: path, Size: int64(archive.Len()), Checksum: hex.EncodeToString(sum[:])}
	if err := f.db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
}

func assertPrometheusArtifactRuntime(t *testing.T, f *compatibilityFixture, version string, want []byte) {
	t.Helper()
	var current models.Component
	if err := f.db.First(&current, f.component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Version != version || current.Status != models.StatusRunning {
		t.Fatalf("runtime metadata=%s/%s want %s/running", current.Version, current.Status, version)
	}
	got, err := os.ReadFile(current.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("wrong artifact executable installed: %q", got)
	}
}

func artifactPlanStatus(plan *models.ChangePlan) string {
	if plan == nil {
		return "<nil>"
	}
	return string(plan.Status)
}
