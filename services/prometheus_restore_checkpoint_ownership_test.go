package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestPrometheusRestoreCheckpointRejectsMidCaptureOwnershipDrift(t *testing.T) {
	for _, mode := range []string{"host-claim", "component-claim", "sibling-claim", "plan", "host", "component", "token"} {
		t.Run(mode, func(t *testing.T) {
			f, r, row, api := checkpointFixture(t)
			var sibling models.Component
			if mode == "sibling-claim" {
				sibling = models.Component{HostID: row.HostID, ClusterID: f.c.ClusterID, Name: "checkpoint-sibling", Type: models.ComponentThanos, DeployType: "binary", Status: "removed"}
				if err := f.db.Create(&sibling).Error; err != nil {
					t.Fatal(err)
				}
			}
			fired := false
			r.before = func(ctx context.Context, command string) error {
				if fired || !strings.Contains(command, "# promlifecycle:restore-checkpoint-root") {
					return nil
				}
				fired = true
				switch mode {
				case "host-claim":
					return f.db.Model(&models.HostOperationClaim{}).Where("host_id = ?", row.HostID).Update("token", "replacement-owner").Error
				case "component-claim":
					return f.db.Model(&models.ComponentOperationClaim{}).Where("component_id = ?", row.ComponentID).Update("token", "replacement-owner").Error
				case "sibling-claim":
					return f.db.Create(&models.ComponentOperationClaim{ComponentID: sibling.ID, Token: "replacement-owner", Kind: "restart"}).Error
				case "plan":
					return f.db.Model(&models.ChangePlan{}).Where("id = ?", row.PlanID).Update("status", models.ChangeStatusVerified).Error
				case "host":
					return f.db.Model(&models.Host{}).Where("id = ?", row.HostID).Update("ip", "192.0.2.55").Error
				case "component":
					return f.db.Model(&models.Component{}).Where("id = ?", row.ComponentID).Update("version", "3.1.0").Error
				case "token":
					return f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).Update("token", strings.Repeat("d", 64)).Error
				}
				return nil
			}
			got, err := api.CreatePrometheusPairedRestoreCheckpoint(context.Background(), row.ID)
			if !fired || got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("mid-capture owner/identity drift admitted", err)
			}
			retained := checkpointJournal(t, f.db, row.ID)
			if retained.Phase != "checkpointing" || retained.Revision != 2 || retained.ActiveHostID == nil || retained.ActiveComponentID == nil || !f.runner.running {
				t.Fatal("owner drift passed stop boundary")
			}
			for _, command := range r.calls {
				if strings.HasPrefix(command, "systemctl stop ") || strings.Contains(command, "cp -aL ") {
					t.Fatal("owner drift allowed stop/archive")
				}
			}
			if mode == "host-claim" || mode == "component-claim" || mode == "sibling-claim" {
				var count int64
				table := "component_operation_claims"
				if mode == "host-claim" {
					table = "host_operation_claims"
				}
				if err := f.db.Table(table).Where("token = ?", "replacement-owner").Count(&count).Error; err != nil || count != 1 {
					t.Fatal("release stole replacement owner", err)
				}
			} else {
				journalAssertClaimsEmpty(t, f.db)
			}
		})
	}
}
func TestPrometheusRestoreCheckpointVerificationRechecksIdentity(t *testing.T) {
	f, r, row, api := checkpointFixture(t)
	ready, err := api.CreatePrometheusPairedRestoreCheckpoint(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	r.calls = nil
	r.before = func(ctx context.Context, command string) error {
		if fired {
			return nil
		}
		fired = true
		return f.db.Model(&models.ChangePlan{}).Where("id = ?", row.PlanID).Update("status", models.ChangeStatusVerified).Error
	}
	if got, err := api.VerifyPrometheusPairedRestoreCheckpoint(context.Background(), row.ID); !fired || got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("verification published evidence after trusted identity drift", err)
	}
	checkpointAssertReadOnly(t, r.calls)
	retained := checkpointJournal(t, f.db, row.ID)
	if retained.Phase != "checkpoint_ready" || retained.Revision != 4 || !retained.UpdatedAt.Equal(ready.UpdatedAt) || f.runner.running {
		t.Fatal("verification drift changed stopped checkpoint")
	}
	journalAssertClaimsEmpty(t, f.db)
}
