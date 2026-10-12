package deployer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGrafanaDatasourceContentIncludesRunningPrometheusAndLoki(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "datasources.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Component{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "local", IP: "127.0.0.1", OS: "linux", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	components := []models.Component{
		{ClusterID: 1, HostID: host.ID, Type: models.ComponentPrometheus, Name: "prometheus", Port: 9090, Status: models.StatusRunning},
		{ClusterID: 1, HostID: host.ID, Type: models.ComponentLoki, Name: "loki", Port: 3100, Status: models.StatusRunning},
	}
	for index := range components {
		if err := db.Create(&components[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	grafana := &models.Component{ClusterID: 1, HostID: host.ID, Type: models.ComponentGrafana, Name: "grafana", Host: host}
	content := NewBinaryDeployer(db, t.TempDir()).grafanaDatasourceContent(grafana)
	for _, required := range []string{
		"uid: promlifecycle-prometheus", "type: prometheus", "url: http://127.0.0.1:9090", "isDefault: true",
		"uid: promlifecycle-loki", "type: loki", "url: http://127.0.0.1:3100", "isDefault: false",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("Grafana datasource provisioning missing %q:\n%s", required, content)
		}
	}
	if err := db.Delete(&components[1]).Error; err != nil {
		t.Fatal(err)
	}
	content = NewBinaryDeployer(db, t.TempDir()).grafanaDatasourceContent(grafana)
	if !strings.Contains(content, "deleteDatasources:\n  - name: Loki\n    orgId: 1") {
		t.Fatalf("removed Loki datasource was not scheduled for deletion:\n%s", content)
	}
}

func TestFindLocalBinaryPackageAcceptsZipFormat(t *testing.T) {
	basePath := t.TempDir()
	packagePath := writeAcquisitionZIP(t, basePath, "promtail.zip", "promtail-linux-amd64", "offline binary")
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "packages.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Package{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Package{
		Name: "promtail", Type: models.ComponentPromtail, Version: "v3.2.0",
		Format: "zip", StoragePath: packagePath,
	}).Error; err != nil {
		t.Fatal(err)
	}
	spec, err := binarySpecFor(models.ComponentPromtail)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewBinaryDeployer(db, basePath).findLocalBinaryPackage(spec, "3.2.0")
	if err != nil || got != packagePath {
		t.Fatalf("zip package was not selected: path=%q err=%v", got, err)
	}
}

func TestEnvironmentFileJSONRestoresRetainedSecrets(t *testing.T) {
	encoded, err := environmentFileJSON([]byte("GF_SECURITY_ADMIN_PASSWORD=\"secret\\nvalue\"\nGF_SECURITY_SECRET_KEY=\"stable-key\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, `"GF_SECURITY_ADMIN_PASSWORD":"secret\nvalue"`) ||
		!strings.Contains(encoded, `"GF_SECURITY_SECRET_KEY":"stable-key"`) {
		t.Fatalf("retained environment was not decoded: %s", encoded)
	}
}
