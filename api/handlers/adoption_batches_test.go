package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"strings"
	"sync/atomic"
	"testing"
)

func adoptionBatchFixture(t *testing.T) (*Handler, []models.Host, models.User) {
	t.Helper()
	h, hosts := batchFixture(t)
	if err := h.db.AutoMigrate(&models.User{}, &models.Cluster{}, &models.ComponentAdoption{}, &models.HostOperationClaim{}, &models.ComponentConfig{}, &models.OperationLog{}); err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "batch-owner", Role: "change_admin", IsActive: true}
	if err := h.db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Create(&models.Cluster{ID: 1, Name: "single"}).Error; err != nil {
		t.Fatal(err)
	}
	h.basePath = t.TempDir()
	return h, hosts, user
}
func adoptionPayload(hosts []models.Host) map[string]interface{} {
	return map[string]interface{}{"kind": "adopt", "name": "已有服务接管", "request_id": "adopt-request-12345678", "adoptions": []map[string]interface{}{
		{"host_id": hosts[0].ID, "type": "node_exporter", "port": 9100, "fingerprint": strings.Repeat("a", 64), "ownership": "managed"},
		{"host_id": hosts[0].ID, "type": "node_exporter", "port": 19100, "fingerprint": strings.Repeat("b", 64), "ownership": "observe"},
		{"host_id": hosts[1].ID, "type": "node_exporter", "port": 9100, "fingerprint": strings.Repeat("c", 64), "ownership": "managed"},
	}}
}
func adoptionActor(h *Handler, user models.User) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("user_id", user.ID)
		c.Set("username", user.Username)
		c.Set("role", user.Role)
		h.CreateDeploymentBatch(c)
	}
}
func TestAdoptionBatchPersistsInstancesAndReplays(t *testing.T) {
	h, hosts, user := adoptionBatchFixture(t)
	payload := adoptionPayload(hosts)
	w := batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0)
	if w.Code != 202 {
		t.Fatalf("create status=%d: %s", w.Code, w.Body)
	}
	var response struct {
		Batch models.DeploymentBatch `json:"batch"`
	}
	json.Unmarshal(w.Body.Bytes(), &response)
	b, err := h.readDeploymentBatch(response.Batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != "adopt" || len(b.Stacks) != 2 || len(b.Stacks[0].Steps) != 2 || b.Summary.TotalSteps != 3 {
		t.Fatalf("lost host or instance: %+v", b)
	}
	for _, stack := range b.Stacks {
		for _, step := range stack.Steps {
			if step.Action != "adopt" || step.EncryptedRequest == "" || strings.Contains(step.EncryptedRequest, "fingerprint") {
				t.Fatalf("unsafe queued request: %+v", step)
			}
		}
	}
	w = batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0)
	if w.Code != 200 {
		t.Fatalf("replay status=%d: %s", w.Code, w.Body)
	}
	var count int64
	h.db.Model(&models.DeploymentBatch{}).Count(&count)
	if count != 1 {
		t.Fatalf("duplicate batch: %d", count)
	}
	payload["name"] = "changed"
	if w = batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0); w.Code != 409 {
		t.Fatalf("changed replay status=%d", w.Code)
	}
}
func TestAdoptionBatchRejectsInvalidMembersAtomically(t *testing.T) {
	for _, scenario := range []string{"duplicate", "missing_host", "missing_fingerprint", "bad_ownership", "mixed_upgrade", "inactive_actor"} {
		t.Run(scenario, func(t *testing.T) {
			h, hosts, user := adoptionBatchFixture(t)
			payload := adoptionPayload(hosts)
			items := payload["adoptions"].([]map[string]interface{})
			switch scenario {
			case "duplicate":
				items[1] = items[0]
			case "missing_host":
				items[2]["host_id"] = 99999
			case "missing_fingerprint":
				items[1]["fingerprint"] = ""
			case "bad_ownership":
				items[1]["ownership"] = "root"
			case "mixed_upgrade":
				payload["upgrades"] = []map[string]interface{}{{"component_id": 1}}
			case "inactive_actor":
				h.db.Model(&user).Update("is_active", false)
			}
			w := batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0)
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("invalid accepted: %d %s", w.Code, w.Body)
			}
			for _, model := range []interface{}{&models.DeploymentBatch{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}} {
				var n int64
				h.db.Model(model).Count(&n)
				if n != 0 {
					t.Fatalf("partial queue persisted: %d", n)
				}
			}
		})
	}
}

// The fixture substitutes only the remote command endpoint. The queue,
// discovery parser, protocol admission, host claims, audit and DB are real.
func TestAdoptionBatchWorkerIdentityAndDurableReceipt(t *testing.T) {
	for _, scenario := range []string{"adopt_and_resume", "already_owned", "legacy_owned", "wrong_existing_type", "wrong_existing_binary", "stale_fingerprint", "inactive_actor", "transaction_failure", "busy_host"} {
		t.Run(scenario, func(t *testing.T) {
			h, hosts, user := adoptionBatchFixture(t)
			h.opsHandler = NewOperationsHandler(h.db)
			var commands atomic.Int32
			encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
			snapshot := "LISTENERS\nLISTEN 0 4096 0.0.0.0:19100 0.0.0.0:* users:((\"node_exporter\",pid=531,fd=7))\nPROCESSES\nPROC\t531\t/srv/existing/node_exporter\t" + encode("node_exporter\x00--web.listen-address=:19100\x00") + "\t" + encode("0::/system.slice/existing-node.service\n") + "\t" + encode("531 (node_exporter) S "+strings.Repeat("0 ", 18)+"12345") + "\n"
			sshPort := adoptionSSH(t, func(command string) (string, uint32) {
				commands.Add(1)
				switch {
				case command == hostDiscoveryCommand:
					return snapshot, 0
				case strings.HasPrefix(command, "curl "):
					return "node_exporter_build_info{version=\"1.8.2\"} 1\n", 0
				case strings.Contains(command, " --version"):
					t.Error("adoption executed an unverified binary")
					return "", 1
				default:
					t.Errorf("unexpected mutating command: %s", command)
					return "", 1
				}
			})
			host := hosts[0]
			host.IP = "127.0.0.2"
			host.Port = sshPort
			host.Username = "root"
			host.Password = "fixture"
			if err := h.db.Save(&host).Error; err != nil {
				t.Fatal(err)
			}
			fp := parseHostDiscovery(host, snapshot)[0].component.Fingerprint
			if scenario == "stale_fingerprint" {
				fp = strings.Repeat("f", 64)
			}
			payload := map[string]interface{}{"kind": "adopt", "name": "worker", "request_id": "adopt-worker-12345678", "adoptions": []map[string]interface{}{{"host_id": host.ID, "type": "node_exporter", "port": 19100, "fingerprint": fp, "ownership": "observe"}}}
			w := batchRequest(t, adoptionActor(h, user), "POST", "/batches", payload, 0)
			if w.Code != 202 {
				t.Fatalf("create %d %s", w.Code, w.Body)
			}
			var step models.DeploymentStackStep
			if err := h.db.First(&step).Error; err != nil {
				t.Fatal(err)
			}
			h.db.Model(&step).Update("status", "running")
			step.Status = "running"
			if scenario == "inactive_actor" {
				h.db.Model(&user).Update("is_active", false)
			}
			if scenario == "transaction_failure" {
				h.db.Exec("CREATE TRIGGER reject_step_link BEFORE UPDATE OF component_id ON deployment_stack_steps BEGIN SELECT RAISE(ABORT, 'fixture link failure'); END")
			}
			if scenario == "busy_host" {
				h.db.Create(&models.HostOperationClaim{HostID: host.ID, Token: "other", Kind: "upgrade"})
			}
			existing := models.Component{HostID: host.ID, ClusterID: 1, Type: models.ComponentNodeExporter, Name: "original", Port: 19100, Status: models.StatusRunning, BinaryPath: "/srv/existing/node_exporter", ServiceName: "existing-node"}
			if strings.HasPrefix(scenario, "wrong_existing") || (scenario == "already_owned" || scenario == "legacy_owned") {
				if scenario == "wrong_existing_type" {
					existing.Type = models.ComponentPrometheus
				}
				if scenario == "wrong_existing_binary" {
					existing.BinaryPath = "/srv/different/node_exporter"
				}
				if scenario == "legacy_owned" {
					existing.BinaryPath = ""
					existing.InstallPath = "/srv/existing"
					existing.DeployType = "binary"
				}
				h.db.Create(&existing)
			}
			scan := batchRequest(t, h.DetectComponents, "GET", "/detect", nil, host.ID)
			var evidence struct {
				Components []struct {
					Ownership string `json:"ownership"`
					Conflict  bool   `json:"ownership_conflict"`
				} `json:"components"`
			}
			if err := json.Unmarshal(scan.Body.Bytes(), &evidence); err != nil || len(evidence.Components) != 1 {
				t.Fatalf("discovery: %s", scan.Body)
			}
			discovered := evidence.Components[0]
			if strings.HasPrefix(scenario, "wrong_existing") && (discovered.Ownership != "" || !discovered.Conflict) {
				t.Fatal("port reuse incorrectly advertised as owned")
			}
			if (scenario == "already_owned" || scenario == "legacy_owned") && (discovered.Ownership != "platform" || discovered.Conflict) {
				t.Fatal("matching owned instance lost")
			}
			err := h.runStackStep(context.Background(), &step)
			wantSuccess := scenario == "adopt_and_resume" || scenario == "already_owned" || scenario == "legacy_owned"
			if (err == nil) != wantSuccess {
				t.Fatalf("worker success=%v, err=%v", wantSuccess, err)
			}
			var loaded models.DeploymentStackStep
			h.db.First(&loaded, step.ID)
			if !wantSuccess {
				if loaded.ComponentID != nil {
					t.Fatal("failed step linked a component")
				}
				var n int64
				h.db.Model(&models.ComponentAdoption{}).Count(&n)
				if n != 0 {
					t.Fatal("failed transaction leaked adoption")
				}
				return
			}
			if loaded.ComponentID == nil {
				t.Fatal("missing atomic receipt")
			}
			var component models.Component
			h.db.First(&component, *loaded.ComponentID)
			if component.HostID != host.ID || component.Port != 19100 || (scenario != "legacy_owned" && component.BinaryPath != "/srv/existing/node_exporter") {
				t.Fatalf("wrong component: %+v", component)
			}
			countBefore := commands.Load()
			// Simulates restart after the adoption transaction but before worker marks success.
			if err := h.runStackStep(context.Background(), &loaded); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if commands.Load() != countBefore {
				t.Fatal("committed adoption repeated remote work")
			}
			var n int64
			h.db.Model(&models.Component{}).Count(&n)
			if n != 1 {
				t.Fatalf("duplicate component count=%d", n)
			}
			if (scenario == "already_owned" || scenario == "legacy_owned") && *loaded.ComponentID != existing.ID {
				t.Fatal("already owned instance replaced")
			}
			var logs int64
			h.db.Model(&models.OperationLog{}).Where("user_id = ?", user.ID).Count(&logs)
			if scenario == "adopt_and_resume" && logs != 1 {
				t.Fatalf("audit attribution missing or repeated: %d", logs)
			}
		})
	}
}

func TestBatchDiscoveryCanIncludeVersionsWithoutChangingLegacyScan(t *testing.T) {
	h, hosts, _ := adoptionBatchFixture(t)
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	snapshot := "LISTENERS\nLISTEN 0 4096 0.0.0.0:19100 0.0.0.0:* users:((\"node_exporter\",pid=531,fd=7))\nPROCESSES\nPROC\t531\t/srv/existing/node_exporter\t" + encode("node_exporter\x00") + "\t" + encode("0::/system.slice/existing-node.service\n") + "\t" + encode("531 (node_exporter) S "+strings.Repeat("0 ", 18)+"12345") + "\n"
	var versionCalls atomic.Int32
	port := adoptionSSH(t, func(command string) (string, uint32) {
		if command == hostDiscoveryCommand {
			return snapshot, 0
		}
		if strings.Contains(command, " --version") {
			t.Error("scan executed an unverified binary")
			return "", 1
		}
		if strings.HasPrefix(command, "curl ") {
			versionCalls.Add(1)
			return "node_exporter_build_info{version=\"1.8.2\"} 1\n", 0
		}
		return "", 1
	})
	host := hosts[0]
	host.IP = "127.0.0.2"
	host.Port = port
	host.Username = "root"
	host.Password = "fixture"
	h.db.Save(&host)
	for _, path := range []string{"/detect", "/detect?versions=1"} {
		w := batchRequest(t, h.DetectComponents, "GET", path, nil, host.ID)
		if w.Code != 200 {
			t.Fatalf("scan: %d %s", w.Code, w.Body)
		}
		var body struct {
			Components []struct {
				Version string `json:"version"`
			} `json:"components"`
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		if len(body.Components) != 1 {
			t.Fatal("missing instance")
		}
		if path == "/detect" && versionCalls.Load() != 0 {
			t.Fatal("legacy scan unexpectedly executes version probes")
		}
		if path != "/detect" && body.Components[0].Version != "1.8.2" {
			t.Fatalf("batch scan omitted version: %s", w.Body)
		}
	}
}

func TestDiscoveryMatchesLegacyLayoutWithoutHidingReplacedInstances(t *testing.T) {
	for _, tc := range []struct {
		name         string
		component    models.Component
		binary, unit string
		want         bool
	}{
		{"legacy_node", models.Component{Type: models.ComponentNodeExporter, DeployType: "binary", InstallPath: "/opt/old/node", ServiceName: "existing-node"}, "/opt/old/node/node_exporter", "existing-node.service", true},
		{"legacy_grafana", models.Component{Type: models.ComponentGrafana, DeployType: "binary", InstallPath: "/opt/old/grafana", ServiceName: "existing-grafana"}, "/opt/old/grafana/bin/grafana", "existing-grafana.service", true},
		{"wrong_path", models.Component{Type: models.ComponentNodeExporter, DeployType: "binary", InstallPath: "/opt/old/node", ServiceName: "existing-node"}, "/opt/replaced/node_exporter", "existing-node.service", false},
		{"wrong_unit", models.Component{Type: models.ComponentNodeExporter, DeployType: "binary", InstallPath: "/opt/old/node", ServiceName: "existing-node"}, "/opt/old/node/node_exporter", "another-node.service", false},
		{"explicit_path_wins", models.Component{Type: models.ComponentNodeExporter, DeployType: "binary", InstallPath: "/opt/old/node", BinaryPath: "/opt/imported/custom-node", ServiceName: "existing-node"}, "/opt/old/node/node_exporter", "existing-node.service", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := services.DiscoveredComponent{Type: string(tc.component.Type), Binary: tc.binary, Unit: tc.unit}
			if got := sameDiscoveredIdentity(tc.component, item); got != tc.want {
				t.Fatalf("identity match=%v, want %v", got, tc.want)
			}
		})
	}
}
