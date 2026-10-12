package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestRecoveryScopeForTargetRemovalUsesMetadataSnapshot(t *testing.T) {
	after, _ := json.Marshal(removeChange{Mode: "keep_data"})
	plan := &models.ChangePlan{Kind: models.ChangeKindRemove, After: after}
	component := &models.Component{DeployType: "target"}
	scope, required, err := recoveryScopeForPlan(plan, component)
	if err != nil {
		t.Fatalf("target removal recovery scope: %v", err)
	}
	if required || scope != "" {
		t.Fatalf("target removal unexpectedly requires artifact: scope=%q required=%v", scope, required)
	}
	if !rollbackWithoutArtifactAllowed(plan, component) {
		t.Fatal("target removal metadata rollback was not allowed")
	}
}

func TestRecoveryScopeForManagedRemovalMatchesImpact(t *testing.T) {
	component := &models.Component{DeployType: "binary", ConfigPath: "/etc/example/config.yml", DataPath: "/var/lib/example"}
	cases := []struct {
		mode     string
		scope    string
		required bool
	}{
		{mode: "keep_service", scope: "", required: false},
		{mode: "keep_data", scope: "config", required: true},
		{mode: "full_delete", scope: "full", required: true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			after, _ := json.Marshal(removeChange{Mode: tc.mode})
			plan := &models.ChangePlan{Kind: models.ChangeKindRemove, After: after}
			scope, required, err := recoveryScopeForPlan(plan, component)
			if err != nil {
				t.Fatalf("scope: %v", err)
			}
			if scope != tc.scope || required != tc.required {
				t.Fatalf("scope=%q required=%v, want %q/%v", scope, required, tc.scope, tc.required)
			}
		})
	}
}

func TestBinaryUpgradeWithoutConfigUsesVersionRollback(t *testing.T) {
	plan := &models.ChangePlan{Kind: models.ChangeKindUpgrade}
	component := &models.Component{DeployType: "binary"}
	scope, required, err := recoveryScopeForPlan(plan, component)
	if err != nil {
		t.Fatalf("binary upgrade recovery: %v", err)
	}
	if scope != "" || required {
		t.Fatalf("binary upgrade unexpectedly required file backup: scope=%q required=%v", scope, required)
	}
	if !rollbackWithoutArtifactAllowed(plan, component) {
		t.Fatal("binary version rollback was not accepted as the recovery mechanism")
	}
}

func TestVersionCandidatesAcceptVPrefixEitherWay(t *testing.T) {
	withPrefix := versionCandidates("v0.26.0")
	withoutPrefix := versionCandidates("0.26.0")
	if len(withPrefix) != 2 || withPrefix[0] != "v0.26.0" || withPrefix[1] != "0.26.0" {
		t.Fatalf("unexpected v-prefixed candidates: %#v", withPrefix)
	}
	if len(withoutPrefix) != 2 || withoutPrefix[0] != "0.26.0" || withoutPrefix[1] != "v0.26.0" {
		t.Fatalf("unexpected plain candidates: %#v", withoutPrefix)
	}
}

func TestStatelessBinaryRemovalUsesPackageRecovery(t *testing.T) {
	after, _ := json.Marshal(removeChange{Mode: "keep_data"})
	plan := &models.ChangePlan{Kind: models.ChangeKindRemove, After: after}
	component := &models.Component{DeployType: "binary"}
	scope, required, err := recoveryScopeForPlan(plan, component)
	if err != nil {
		t.Fatalf("stateless binary removal recovery: %v", err)
	}
	if scope != "" || required {
		t.Fatalf("stateless binary removal unexpectedly required file backup: scope=%q required=%v", scope, required)
	}
	if !rollbackWithoutArtifactAllowed(plan, component) {
		t.Fatal("registered package recovery was not accepted for a stateless binary")
	}
}
func TestWriteLiveConfigForStoppedLocalComponent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prometheus.yml")
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	service := &ChangeService{}
	component := &models.Component{
		DeployType: "binary",
		Status:     models.StatusStopped,
		ConfigPath: path,
		Host: models.Host{
			IP:             "127.0.0.1",
			ConnectionType: "local",
		},
	}
	writtenPath, err := service.writeLiveConfig(context.Background(), component, "prometheus.yml", "new\n")
	if err != nil {
		t.Fatalf("write live config: %v", err)
	}
	if writtenPath != path {
		t.Fatalf("written path=%q, want %q", writtenPath, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live config: %v", err)
	}
	if string(content) != "new\n" {
		t.Fatalf("live content=%q", content)
	}
}

func TestPrometheusVerificationWaitsForOperationalData(t *testing.T) {
	cases := []struct {
		name     string
		response string
		ready    bool
	}{
		{"critical_query", `{"status":"success","data":{"result":[]}}`, false},
		{"critical_query", `{"status":"success","data":{"result":[{}]}}`, true},
		{"targets", `{"status":"success","data":{"activeTargets":[]}}`, false},
		{"targets", `{"status":"success","data":{"activeTargets":[{"health":"unknown"}]}}`, false},
		{"targets", `{"status":"success","data":{"activeTargets":[{"health":"up"},{"health":"down"}]}}`, true},
		{"rules", `{"status":"success","data":{"groups":[{"rules":[{"health":"unknown"}]}]}}`, false},
		{"rules", `{"status":"success","data":{"groups":[{"rules":[{"health":"ok"}]}]}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.response, func(t *testing.T) {
			ready, err := prometheusVerificationReady(tc.name, tc.response)
			if err != nil {
				t.Fatal(err)
			}
			if ready != tc.ready {
				t.Fatalf("ready=%v, want %v", ready, tc.ready)
			}
		})
	}
}
