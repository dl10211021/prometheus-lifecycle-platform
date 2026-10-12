package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeploymentTemplateReuseOmitsCredentialsNotTunables(t *testing.T) {
	content := `{"component_order":["prometheus"],"settings":{"prometheus":{"version":"3.1.0","port":19090,"custom_args":"--query.max-concurrency=20\n--password=SECRET_ARG\n--remote-url=https://user:SECRET_URL@example.com/write","custom_env":"{\"GOGC\":\"75\",\"API_TOKEN\":\"SECRET_ENV\"}","initial_password":"SECRET_INITIAL"}},"version_profile":{"validated":true}}`
	safe, err := sanitizeDeploymentTemplate(content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(safe, "SECRET_") || strings.Contains(safe, "[REDACTED]") {
		t.Fatalf("secret or undeployable placeholder retained: %s", safe)
	}
	var result struct {
		Settings map[string]map[string]interface{} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(safe), &result); err != nil {
		t.Fatal(err)
	}
	settings := result.Settings["prometheus"]
	if settings["port"] != float64(19090) || settings["custom_args"] != "--query.max-concurrency=20" {
		t.Fatalf("safe tunables lost: %+v", settings)
	}
	var environment map[string]string
	if err := json.Unmarshal([]byte(settings["custom_env"].(string)), &environment); err != nil {
		t.Fatal(err)
	}
	if environment["GOGC"] != "75" || len(environment) != 1 {
		t.Fatalf("safe environment not reusable: %+v", environment)
	}
	if strings.Contains(safe, "version_profile") {
		t.Fatal("custom template inherited validated-profile claim")
	}
}
