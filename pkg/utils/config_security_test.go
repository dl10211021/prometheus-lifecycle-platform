package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigPreviewRedactsNestedAndUnknownCredentials(t *testing.T) {
	for _, tc := range []struct{ kind, format, input string }{
		{"prometheus", "yaml", "remote_write:\n- url: https://user:CANARY_URL@example.com/write?key=CANARY_QUERY\n  authorization:\n    credentials: CANARY_AUTH\n  vendor_extension:\n    opaque: CANARY_UNKNOWN\nscrape_configs:\n- job_name: safe-job\n  basic_auth:\n    password: CANARY_PASSWORD\n"},
		{"alertmanager", "yaml", "receivers:\n- name: safe-job\n  webhook_configs:\n  - url: https://hooks.example.com/CANARY_WEBHOOK\n  email_configs:\n  - auth_password: CANARY_EMAIL\n"},
		{"grafana", "ini", "[security]\nadmin_password = CANARY_ADMIN\nsecret_key = CANARY_SIGNING\n[extension]\nopaque = CANARY_UNKNOWN\n"},
		{"custom", "yaml", "extension:\n  opaque: CANARY_UNKNOWN\n"},
		{"custom", "unknown", "CANARY_UNPARSEABLE"},
	} {
		got := SanitizeConfig(tc.kind, tc.format, tc.input)
		if strings.Contains(got, "CANARY") {
			t.Fatalf("%s leaked: %s", tc.kind, got)
		}
		if !HasRedactedSecrets(got) {
			t.Fatalf("%s preview not recognized as non-editable: %s", tc.kind, got)
		}
	}
}

func TestSnapshotRedactsEveryConfigCopyWithoutMutatingSource(t *testing.T) {
	input := json.RawMessage(`{"component":{"configs":[{"content_type":"yaml","content":"authorization:\n  credentials: CANARY_HISTORY"}]},"before":{"content":"extension:\n  opaque: CANARY_BEFORE"},"after":{"content":"password: CANARY_AFTER"},"diff":"- password: CANARY_DIFF","evidence":"{\"api_key\":\"CANARY_EVIDENCE\"}"}`)
	got := SanitizeConfigJSON(input)
	if strings.Contains(string(got), "CANARY") {
		t.Fatalf("snapshot leaked: %s", got)
	}
	if !strings.Contains(string(input), "CANARY_HISTORY") {
		t.Fatal("internal snapshot was mutated")
	}
	if !json.Valid(got) {
		t.Fatalf("invalid public JSON: %s", got)
	}
}

func TestBcryptByteBoundary(t *testing.T) {
	valid := strings.Repeat("密", 24)
	hash := HashPassword(valid)
	if hash == "" || !CheckPassword(valid, hash) {
		t.Fatal("72-byte password failed")
	}
	if HashPassword(valid+"a") != "" || CheckPassword(valid+"a", hash) {
		t.Fatal("overlength password accepted")
	}
}
