package models

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestChangePlanReadsLegacyTextBlobAndNullEvidence(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var plan ChangePlan
	err = db.Raw(`SELECT ? AS "before", ? AS "after", ? AS verification, NULL AS rollback_evidence`,
		[]byte(`{"version":1}`), `{"version":2}`, `{"passed":true}`).Scan(&plan).Error
	if err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		Version int `json:"version"`
	}
	var verification struct {
		Passed bool `json:"passed"`
	}
	for _, item := range []struct {
		data   json.RawMessage
		target interface{}
	}{
		{plan.Before, &before}, {plan.After, &after}, {plan.Verification, &verification},
	} {
		if err := json.Unmarshal(item.data, item.target); err != nil {
			t.Fatal(err)
		}
	}
	if before.Version != 1 || after.Version != 2 || !verification.Passed || plan.RollbackEvidence != nil {
		t.Fatalf("legacy evidence changed: before=%+v after=%+v verification=%+v rollback=%s", before, after, verification, plan.RollbackEvidence)
	}
}

func TestComponentTypeConstants(t *testing.T) {
	types := []ComponentType{
		ComponentPrometheus,
		ComponentGrafana,
		ComponentAlertmanager,
		ComponentLoki,
		ComponentPromtail,
		ComponentThanos,
		ComponentNodeExporter,
		ComponentPushgateway,
		ComponentBlackbox,
	}

	for _, ct := range types {
		if ct == "" {
			t.Errorf("ComponentType should not be empty")
		}
		t.Logf("OK: %s", ct)
	}
}

func TestComponentStatusConstants(t *testing.T) {
	statuses := []ComponentStatus{
		StatusPending,
		StatusDeploying,
		StatusRunning,
		StatusDegraded,
		StatusStopping,
		StatusStopped,
		StatusError,
		StatusUpgrading,
		StatusRollingBack,
		StatusConfiguring,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("ComponentStatus should not be empty")
		}
		t.Logf("OK: %s", s)
	}
}

func TestOperationTypeConstants(t *testing.T) {
	types := []OperationType{
		OpDeploy,
		OpUpgrade,
		OpRollback,
		OpConfigure,
		OpScale,
		OpDelete,
		OpBackup,
		OpRestore,
		OpRestart,
		OpHealthCheck,
	}

	for _, ot := range types {
		if ot == "" {
			t.Errorf("OperationType should not be empty")
		}
		t.Logf("OK: %s", ot)
	}
}

func TestClusterStructure(t *testing.T) {
	now := time.Now()

	cluster := Cluster{
		Name:           "test-cluster",
		Environment:    "production",
		DeploymentType: "docker",
		Host:           "192.168.1.100",
		Description:    "Test cluster",
		Status:         "active",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if cluster.Name != "test-cluster" {
		t.Errorf("Expected name 'test-cluster', got '%s'", cluster.Name)
	}

	if cluster.Environment != "production" {
		t.Errorf("Expected environment 'production', got '%s'", cluster.Environment)
	}

	if cluster.DeploymentType != "docker" {
		t.Errorf("Expected deployment_type 'docker', got '%s'", cluster.DeploymentType)
	}

	t.Logf("OK: Cluster structure valid")
}

func TestComponentStructure(t *testing.T) {
	now := time.Now()

	component := Component{
		ClusterID:   1,
		Type:        ComponentPrometheus,
		Name:        "prometheus-main",
		Status:      StatusRunning,
		Version:     "v2.51.0",
		Image:       "prom/prometheus:v2.51.0",
		Port:        9090,
		ExternalURL: "http://localhost:9090",
		InternalURL: "http://prometheus:9090",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if component.Type != ComponentPrometheus {
		t.Errorf("Expected type 'prometheus', got '%s'", component.Type)
	}

	if component.Status != StatusRunning {
		t.Errorf("Expected status 'running', got '%s'", component.Status)
	}

	if component.Port != 9090 {
		t.Errorf("Expected port 9090, got %d", component.Port)
	}

	t.Logf("OK: Component structure valid")
}

func TestComponentConfigStructure(t *testing.T) {
	now := time.Now()

	config := ComponentConfig{
		ComponentID: 1,
		Version:     1,
		Name:        "prometheus.yml",
		ContentType: "yaml",
		Content: `global:
  scrape_interval: 15s`,
		IsActive:    true,
		Description: "Initial config",
		CreatedAt:   now,
	}

	if config.Version != 1 {
		t.Errorf("Expected version 1, got %d", config.Version)
	}

	if !config.IsActive {
		t.Errorf("Expected IsActive to be true")
	}

	t.Logf("OK: ComponentConfig structure valid")
}

func TestOperationStructure(t *testing.T) {
	now := time.Now()

	compID := uint(1)
	op := Operation{
		ComponentID: &compID,
		Type:        string(OpDeploy),
		Status:      "success",
		Input:       `{"type": "prometheus"}`,
		Output:      "deployed successfully",
		CreatedAt:   now,
	}

	if op.Type != string(OpDeploy) {
		t.Errorf("Expected type 'deploy', got '%s'", op.Type)
	}

	if op.Status != "success" {
		t.Errorf("Expected status 'success', got '%s'", op.Status)
	}

	t.Logf("OK: Operation structure valid")
}

func TestHealthStatusStructure(t *testing.T) {
	now := time.Now()

	health := HealthStatus{
		Status:    "healthy",
		Component: "prometheus",
		Type:      ComponentPrometheus,
		Message:   "Component is healthy",
		Details: map[string]interface{}{
			"up": "1",
		},
		Timestamp: now,
	}

	if health.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", health.Status)
	}

	t.Logf("OK: HealthStatus structure valid")
}

func TestBackupStructure(t *testing.T) {
	now := time.Now()

	compID := uint(1)
	backup := Backup{
		ComponentID: &compID,
		Type:        "full",
		Path:        "/backups/backup-1",
		Size:        1024 * 1024,
		CreatedAt:   now,
	}

	if backup.Type != "full" {
		t.Errorf("Expected type 'full', got '%s'", backup.Type)
	}

	t.Logf("OK: Backup structure valid")
}
