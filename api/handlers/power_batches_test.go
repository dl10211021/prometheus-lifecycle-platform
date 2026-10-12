package handlers

import (
	"context"
	"encoding/json"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/services"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func powerBatchFixture(t *testing.T) (*Handler, []models.Component, models.User) {
	t.Helper()
	h, hosts, user := adoptionBatchFixture(t)
	cs := []models.Component{
		{HostID: hosts[0].ID, ClusterID: 1, Type: models.ComponentNodeExporter, Name: "node-a", Port: 9100, DeployType: "binary", Status: models.StatusRunning, InstallPath: "/srv/node-a", ServiceName: "node-a"},
		{HostID: hosts[0].ID, ClusterID: 1, Type: models.ComponentNodeExporter, Name: "node-b", Port: 19100, DeployType: "binary", Status: models.StatusStopped, InstallPath: "/srv/node-b", ServiceName: "node-b"},
		{HostID: hosts[1].ID, ClusterID: 1, Type: models.ComponentNodeExporter, Name: "node-c", Port: 9100, DeployType: "binary", Status: models.StatusRunning, InstallPath: "/srv/node-c", ServiceName: "node-c"},
	}
	if err := h.db.Create(&cs).Error; err != nil {
		t.Fatal(err)
	}
	return h, cs, user
}
func powerPayload(cs []models.Component, action string) map[string]interface{} {
	ids := make([]uint, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	return map[string]interface{}{"kind": action, "name": "已有实例批量启停", "reason": "计划维护", "request_id": "power-request-12345678", "component_ids": ids}
}
func TestPowerBatchPersistsInstancesAndReplays(t *testing.T) {
	for _, action := range []string{"start", "stop"} {
		t.Run(action, func(t *testing.T) {
			h, cs, user := powerBatchFixture(t)
			payload := powerPayload(cs, action)
			w := batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0)
			if w.Code != 202 {
				t.Fatalf("create %d: %s", w.Code, w.Body)
			}
			var response struct {
				Batch models.DeploymentBatch `json:"batch"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			b, err := h.readDeploymentBatch(response.Batch.ID)
			if err != nil {
				t.Fatal(err)
			}
			if b.Kind != action || len(b.Stacks) != 2 || len(b.Stacks[0].Steps) != 2 || b.Summary.TotalSteps != 3 {
				t.Fatalf("lost instances: %+v", b)
			}
			for _, stack := range b.Stacks {
				for _, step := range stack.Steps {
					if step.Action != action || step.ComponentID == nil || step.EncryptedRequest == "" || step.Port == 0 {
						t.Fatalf("missing queued identity: %+v", step)
					}
				}
			}
			// Replay remains available after a queued component is removed.
			h.db.Model(&cs[0]).Update("status", "removed")
			w = batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0)
			if w.Code != 200 {
				t.Fatalf("replay %d: %s", w.Code, w.Body)
			}
			var n int64
			h.db.Model(&models.DeploymentBatch{}).Count(&n)
			if n != 1 {
				t.Fatalf("duplicate batches: %d", n)
			}
			payload["kind"] = map[string]string{"start": "stop", "stop": "start"}[action]
			if w = batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0); w.Code != 409 {
				t.Fatalf("changed request accepted: %d", w.Code)
			}
		})
	}
}
func TestPowerBatchRejectsInvalidMembersWithoutPartialQueue(t *testing.T) {
	for _, scenario := range []string{"duplicate", "missing", "removed", "observe", "legacy_docker", "busy", "inactive_actor", "mixed_adopt", "mixed_upgrade", "mixed_deploy", "empty", "too_many", "no_reason"} {
		t.Run(scenario, func(t *testing.T) {
			h, cs, user := powerBatchFixture(t)
			payload := powerPayload(cs, "stop")
			switch scenario {
			case "duplicate":
				payload["component_ids"] = []uint{cs[0].ID, cs[0].ID}
			case "missing":
				payload["component_ids"] = []uint{cs[0].ID, 99999}
			case "removed":
				h.db.Model(&cs[1]).Update("status", "removed")
			case "observe":
				h.db.Create(&models.ComponentAdoption{ComponentID: cs[1].ID, Ownership: "observe"})
			case "legacy_docker":
				h.db.Model(&cs[1]).Update("deploy_type", "docker")
			case "busy":
				h.db.Model(&cs[1]).Update("status", "upgrading")
			case "inactive_actor":
				h.db.Model(&user).Update("is_active", false)
			case "mixed_adopt":
				payload["adoptions"] = []map[string]interface{}{{"host_id": 1}}
			case "mixed_upgrade":
				payload["upgrades"] = []map[string]interface{}{{"component_id": 1}}
			case "mixed_deploy":
				payload["stacks"] = []map[string]interface{}{{"host_id": 1}}
			case "empty":
				payload["component_ids"] = []uint{}
			case "too_many":
				payload["component_ids"] = make([]uint, 201)
			case "no_reason":
				payload["reason"] = " "
			}
			w := batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0)
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("invalid accepted: %d %s", w.Code, w.Body)
			}
			for _, model := range []interface{}{&models.DeploymentBatch{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}} {
				var n int64
				h.db.Model(model).Count(&n)
				if n != 0 {
					t.Fatalf("partial queue: %d", n)
				}
			}
		})
	}
}

// Only the external host is substituted. The handler, claims, durable queue,
// operation receipt, state persistence and HTTP health verification are real.
func TestPowerBatchWorkerPreservesReceiptsAndRechecksTargets(t *testing.T) {
	for _, scenario := range []string{"stop", "start", "remote_failure_retry", "inactive_actor", "observe_after_queue", "changed_unit", "changed_host", "component_busy", "link_transaction_failure", "result_transaction_failure", "foreign_receipt", "unsettled_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			h, cs, user := powerBatchFixture(t)
			if err := h.db.AutoMigrate(&models.Operation{}, &models.ComponentOperationClaim{}, &services.ComponentBackupPause{}); err != nil {
				t.Fatal(err)
			}
			h.deployerFactory = deployer.NewFactory(t.TempDir())
			h.opsHandler = NewOperationsHandler(h.db)
			var controls atomic.Int32
			var remoteFailure atomic.Bool
			if scenario == "remote_failure_retry" {
				remoteFailure.Store(true)
			}
			sshPort := adoptionSSH(t, func(command string) (string, uint32) {
				if strings.HasPrefix(command, "systemctl stop 'node-a'") || strings.HasPrefix(command, "systemctl enable 'node-a'") {
					controls.Add(1)
					if remoteFailure.Load() {
						return "fixture denied", 1
					}
					return "", 0
				}
				if strings.HasPrefix(command, "systemctl is-active --quiet 'node-a'") {
					return "", 0
				}
				t.Errorf("unexpected service command: %s", command)
				return "", 1
			})
			h.db.Model(&models.Host{}).Where("id = ?", cs[0].HostID).Updates(map[string]interface{}{"ip": "127.0.0.2", "port": sshPort, "username": "root", "password": "fixture"})
			action := "stop"
			if scenario == "start" {
				action = "start"
				listener, err := net.Listen("tcp", "127.0.0.2:0")
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
				server.Listener = listener
				server.Start()
				t.Cleanup(server.Close)
				h.db.Model(&cs[0]).Updates(map[string]interface{}{"port": listener.Addr().(*net.TCPAddr).Port, "status": "stopped"})
			}
			w := batchRequest(t, adoptionActor(h, user), "POST", "/batches", powerPayload(cs[:1], action), 0)
			if w.Code != 202 {
				t.Fatalf("create %d %s", w.Code, w.Body)
			}
			var step models.DeploymentStackStep
			h.db.First(&step)
			h.db.Model(&step).Update("status", "running")
			step.Status = "running"
			switch scenario {
			case "inactive_actor":
				h.db.Model(&user).Update("is_active", false)
			case "observe_after_queue":
				h.db.Create(&models.ComponentAdoption{ComponentID: cs[0].ID, Ownership: "observe"})
			case "changed_unit":
				h.db.Model(&cs[0]).Update("service_name", "another")
			case "changed_host":
				h.db.Model(&models.Host{}).Where("id = ?", cs[0].HostID).Update("ip", "192.0.2.200")
			case "component_busy":
				h.db.Create(&models.ComponentOperationClaim{ComponentID: cs[0].ID, Token: "held", Kind: "upgrade"})
			case "link_transaction_failure":
				h.db.Exec("CREATE TRIGGER reject_power_link BEFORE UPDATE OF operation_id ON deployment_stack_steps BEGIN SELECT RAISE(ABORT, 'link failed'); END")
			case "result_transaction_failure":
				h.db.Exec("CREATE TRIGGER reject_power_result BEFORE UPDATE OF result ON deployment_stack_steps WHEN NEW.result <> '' BEGIN SELECT RAISE(ABORT, 'result failed'); END")
			case "foreign_receipt", "unsettled_receipt":
				id := cs[1].ID
				status := "success"
				if scenario == "unsettled_receipt" {
					id = cs[0].ID
					status = "running"
				}
				op := models.Operation{ComponentID: &id, Type: action, Status: status}
				h.db.Create(&op)
				h.db.Model(&step).Updates(map[string]interface{}{"operation_id": op.ID, "result": "stopped"})
				h.db.First(&step, step.ID)
			}
			err := h.runStackStep(context.Background(), &step)
			if scenario != "stop" && scenario != "start" {
				if err == nil {
					t.Fatal("unsafe execution accepted")
				}
				wantControls := int32(0)
				if scenario == "remote_failure_retry" || scenario == "result_transaction_failure" {
					wantControls = 1
				}
				if controls.Load() != wantControls {
					t.Fatalf("unexpected remote actions=%d", controls.Load())
				}
				var saved models.DeploymentStackStep
				h.db.First(&saved, step.ID)
				if scenario == "result_transaction_failure" && saved.Result != "" {
					t.Fatal("failed transaction left a successful receipt")
				}
				if scenario == "link_transaction_failure" {
					var count int64
					h.db.Model(&models.Operation{}).Count(&count)
					if count != 0 || saved.OperationID != nil {
						t.Fatal("failed link left a partial operation")
					}
				}
				if scenario != "remote_failure_retry" && scenario != "result_transaction_failure" && scenario != "link_transaction_failure" {
					return
				}
				h.db.Exec("DROP TRIGGER IF EXISTS reject_power_result")
				h.db.Exec("DROP TRIGGER IF EXISTS reject_power_link")
				remoteFailure.Store(false)
				h.db.First(&step, step.ID)
				if err = h.runStackStep(context.Background(), &step); err != nil {
					t.Fatalf("retry: %v", err)
				}
			} else if err != nil {
				t.Fatalf("worker: %v", err)
			}
			var saved models.DeploymentStackStep
			h.db.First(&saved, step.ID)
			if saved.OperationID == nil {
				t.Fatal("operation not linked")
			}
			var op models.Operation
			h.db.First(&op, *saved.OperationID)
			if op.Status != "success" || op.Type != action || op.ComponentID == nil || *op.ComponentID != cs[0].ID {
				t.Fatalf("invalid operation receipt: %+v", op)
			}
			var component models.Component
			h.db.First(&component, cs[0].ID)
			want := models.StatusStopped
			result := "stopped"
			if action == "start" {
				want = models.StatusRunning
				result = "started"
			}
			if component.Status != want || component.Error != "" || saved.Result != result {
				t.Fatalf("state/receipt missing: %s / %s / %s", component.Status, component.Error, saved.Result)
			}
			var input map[string]interface{}
			json.Unmarshal([]byte(op.Input), &input)
			if input["actor_id"] != float64(user.ID) {
				t.Fatal("durable actor attribution missing")
			}
			before := controls.Load()
			// The worker lost its success acknowledgement; a later manual state change
			// must not cause it to replay this already committed start/stop.
			h.db.Model(&component).Update("status", "error")
			if err := h.runStackStep(context.Background(), &saved); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if controls.Load() != before {
				t.Fatal("completed operation repeated remote control")
			}
		})
	}
}

func TestPowerBatchRejectsChangedWinRMEndpointBeforeOperation(t *testing.T) {
	for _, field := range []string{"winrm_port", "winrm_scheme", "winrm_skip_verify"} {
		t.Run(field, func(t *testing.T) {
			h, cs, user := powerBatchFixture(t)
			if err := h.db.AutoMigrate(&models.Operation{}, &models.ComponentOperationClaim{}, &services.ComponentBackupPause{}); err != nil {
				t.Fatal(err)
			}
			h.db.Model(&cs[0]).Updates(map[string]interface{}{"deploy_type": "windows", "type": "windows_exporter"})
			var host models.Host
			h.db.First(&host, cs[0].HostID)
			host.OS = "windows"
			host.ConnectionType = "winrm"
			host.WinRMPort = 5986
			host.WinRMScheme = "https"
			host.WinRMSkipVerify = false
			if err := h.db.Save(&host).Error; err != nil {
				t.Fatal(err)
			}
			w := batchRequest(t, adoptionActor(h, user), "POST", "/batches", powerPayload(cs[:1], "stop"), 0)
			if w.Code != 202 {
				t.Fatalf("create: %d %s", w.Code, w.Body)
			}
			var step models.DeploymentStackStep
			h.db.First(&step)
			h.db.Model(&step).Update("status", "running")
			plain, err := credentials.Decrypt(step.EncryptedRequest)
			if err != nil {
				t.Fatal(err)
			}
			var queued queuedPower
			if err := json.Unmarshal([]byte(plain), &queued); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "winrm_port":
				host.WinRMPort = 5985
			case "winrm_scheme":
				host.WinRMScheme = "http"
			case "winrm_skip_verify":
				host.WinRMSkipVerify = true
			}
			if err := h.db.Save(&host).Error; err != nil {
				t.Fatal(err)
			}
			ctx, release, err := services.ClaimComponentOperation(context.Background(), h.db, cs[0].ID, "power")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx = context.WithValue(ctx, powerStepContextKey{}, powerStepExecution{StepID: step.ID, Request: queued})
			var component models.Component
			h.db.Preload("Host").First(&component, cs[0].ID)
			if _, err := h.preparePowerOperation(ctx, &component, "stop"); err == nil {
				t.Fatal("changed WinRM target passed execution admission")
			}
			var n int64
			h.db.Model(&models.Operation{}).Count(&n)
			if n != 0 {
				t.Fatal("changed endpoint created an operation")
			}
		})
	}
}
