package services

import (
	"context"
	"github.com/promlifecycle/platform/models"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPrometheusPairedRestorePreviewOptionalEnvironmentPresence(t *testing.T) {
	for _, historicalPresent := range []bool{false, true} {
		name := "historically_absent"
		if historicalPresent {
			name = "historically_present"
		}
		t.Run(name, func(t *testing.T) {
			f := newPointFixture(t)
			if !historicalPresent {
				if err := os.Remove(f.runner.environment); err != nil {
					t.Fatal(err)
				}
			}
			r := &pairedPreviewRunner{fixture: f.runner}
			r.properties = func() string {
				return strings.Replace(f.runner.properties(), "(ignore_errors=no)", "(ignore_errors=yes)", 1)
			}
			f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return r, nil }
			if _, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); err != nil {
				t.Fatal(err)
			}
			previewUpdate(t, f, "change_plans", "id", f.plan.ID, "status", models.ChangeStatusFailed)
			r.refs = pairedReferenceFrame(f.c.ServiceName, pairedReferenceBlock(f.c.ServiceName, "FragmentPath="+f.runner.fragment+"\nEnvironmentFiles="+f.runner.environment+" (ignore_errors=yes)\nExecStart="+pairedReferenceExec(f.c.BinaryPath)+"\n"))
			for _, currentPresent := range []bool{false, true} {
				action := "replace"
				if !historicalPresent {
					action = "remove"
					if !currentPresent {
						action = "retain_absence"
					}
				}
				t.Run(action, func(t *testing.T) {
					if currentPresent {
						if err := os.WriteFile(f.runner.environment, []byte("OPTIONAL=current-private\n"), 0600); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Remove(f.runner.environment); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					files := pairedPreviewFiles(t, f.root)
					state := pairedPreviewBusinessState(t, f)
					pid := f.runner.pid
					r.calls = nil
					proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, item := range proof.Replacements {
						if item.Path == f.runner.environment {
							found = true
							if item.Action != action || item.HistoricalPresent != historicalPresent || item.CurrentPresent != currentPresent || (historicalPresent != currentPresent && !item.ContentChanged) {
								t.Fatalf("optional action: %+v", item)
							}
						}
					}
					if !found || !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
						t.Fatal("optional preview missing or mutated state")
					}
					assertPairedPreviewNoMutation(t, f, r, files, pid)
				})
			}
		})
	}
}
func TestPrometheusPairedRestorePreviewRejectsEnablementAndTopologyDrift(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	for _, kind := range []string{"enablement", "environment optionality", "fragment identity", "missing environment"} {
		t.Run(kind, func(t *testing.T) {
			r.properties = func() string {
				raw := f.runner.properties()
				switch kind {
				case "enablement":
					return strings.Replace(raw, "UnitFileState=disabled", "UnitFileState=enabled", 1)
				case "environment optionality":
					return strings.Replace(raw, "(ignore_errors=no)", "(ignore_errors=yes)", 1)
				case "fragment identity":
					return strings.Replace(raw, f.runner.fragment, f.runner.fragment+"-other", 1)
				default:
					return strings.Replace(raw, f.runner.environment+" (ignore_errors=no)", "", 1)
				}
			}
			defer func() { r.properties = nil }()
			before := pairedPreviewFiles(t, f.root)
			pid := f.runner.pid
			r.calls = nil
			if proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID); err == nil || proof != nil {
				t.Fatal("startup topology/enablement drift admitted")
			}
			assertPairedPreviewNoMutation(t, f, r, before, pid)
		})
	}
}
