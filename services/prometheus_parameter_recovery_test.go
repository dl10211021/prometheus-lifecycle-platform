package services

import (
	"encoding/json"
	"errors"
	"github.com/promlifecycle/platform/models"
	"reflect"
	"testing"
)

// Recovery and automatic compensation must reject incompatible historical argv
// at snapshot admission, before archive verification, metadata or runtime work.
func TestPrometheusParameterVersionHistoricalSnapshotAdmission(t *testing.T) {
	for _, tc := range []struct {
		previous, current string
		rejected          bool
	}{{"2.55.0", "3.1.0", false}, {"3.1.0", "3.2.0", true}} {
		t.Run(tc.previous, func(t *testing.T) {
			current := models.Component{ID: 41, HostID: 7, Type: models.ComponentPrometheus, Name: "prom", DeployType: "binary", Version: tc.current, ImportedArgs: `["--storage.tsdb.retention=45d"]`}
			previous := current
			previous.Version = tc.previous
			raw, err := json.Marshal(prometheusBefore{Component: &previous, Version: previous.Version, Image: previous.Image, ImportedArgsSHA256: importedArgsHash(previous.ImportedArgs)})
			if err != nil {
				t.Fatal(err)
			}
			plan := models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: current.ID, Before: raw}
			beforeCurrent, beforePlan := current, plan
			_, err = prometheusUpgradeSnapshot(&plan, &current)
			if tc.rejected {
				if !errors.Is(err, models.ErrPrometheusParameterVersion) || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
					t.Fatalf("historical parameter rejection missing: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current, beforeCurrent) || !reflect.DeepEqual(plan, beforePlan) {
				t.Fatal("snapshot validation changed recovery input")
			}
		})
	}
}
