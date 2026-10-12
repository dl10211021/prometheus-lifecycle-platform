package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestPrometheusParameterMigrationPreviewDoesNotAuthorizeUpgrade(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentPrometheus, "2.55.0", "0.39.2")
	runtimeLog := f.binary(t, "3.1.0")
	raw, _ := json.Marshal([]string{"--config.file=" + f.component.ConfigPath, "--storage.tsdb.path=" + f.component.DataPath, "--storage.tsdb.retention=045d"})
	f.component.ImportedArgs = string(raw)
	if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
		t.Fatal(err)
	}
	var before models.Component
	if err := f.db.First(&before, f.component.ID).Error; err != nil {
		t.Fatal(err)
	}
	counts := func() [3]int64 {
		var n [3]int64
		for i, model := range []interface{}{&models.Backup{}, &models.Operation{}, &models.ChangePlan{}} {
			if err := f.db.Model(model).Count(&n[i]).Error; err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	beforeCounts := counts()
	evidence, _, err := f.service.preflightUpgrade(context.Background(), &f.component, upgradeChange{NewVersion: "3.1.0"})
	if !errors.Is(err, ErrPrometheusMigrationAdmission) {
		t.Fatalf("preview bypassed execution admission: %v", err)
	}
	encoded, marshalErr := json.Marshal(evidence["parameter_migration"])
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var preview struct {
		Status      string
		PreviewOnly bool `json:"preview_only"`
		Changes     []struct{ Value string }
	}
	if err := json.Unmarshal(encoded, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Status != "available" || !preview.PreviewOnly || len(preview.Changes) != 1 || preview.Changes[0].Value != "045d" {
		t.Fatalf("rejected upgrade must still expose value-preserving preview: %s", encoded)
	}
	var persisted models.Component
	if err := f.db.First(&persisted, f.component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, persisted) || len(f.commands) != 0 {
		a, b := reflect.ValueOf(before), reflect.ValueOf(persisted)
		for i := 0; i < a.NumField(); i++ {
			if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
				t.Logf("different field: %s", a.Type().Field(i).Name)
			}
		}
		t.Fatalf("component changed; archive command count=%d", len(f.commands))
	}
	if counts() != beforeCounts {
		t.Fatal("preview changed lifecycle record counts")
	}
	if log, _ := os.ReadFile(runtimeLog); len(log) != 0 {
		t.Fatal("preview touched runtime")
	}
}
