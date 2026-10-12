package handlers

import (
	"encoding/json"
	"testing"
)

func TestScrapePayloadAcceptsFormStringsAndStructuredJSON(t *testing.T) {
	fields, err := normalizeScrapeJSONFields(map[string]json.RawMessage{
		"targets":         json.RawMessage(`"[\"localhost:9100\"]"`),
		"labels":          json.RawMessage(`{"team":"ops"}`),
		"file_sd_configs": nil,
		"http_sd_configs": json.RawMessage(`""`),
		"relabel_configs": json.RawMessage(`null`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	if err := json.Unmarshal([]byte(fields["targets"]), &targets); err != nil || len(targets) != 1 || targets[0] != "localhost:9100" {
		t.Fatalf("form string was double-encoded: %q, %v", fields["targets"], err)
	}
	if _, exists := fields["file_sd_configs"]; exists {
		t.Fatal("omitted discovery config became an explicit update")
	}
	if fields["http_sd_configs"] != "" || fields["relabel_configs"] != "" {
		t.Fatal("explicit clear was not preserved")
	}
	structured, err := normalizeScrapeJSONFields(map[string]json.RawMessage{"targets": json.RawMessage(`["localhost:9100"]`), "labels": json.RawMessage(`"{\"team\":\"ops\"}"`)})
	if err != nil {
		t.Fatal(err)
	}
	if structured["targets"] != fields["targets"] || structured["labels"] != fields["labels"] {
		t.Fatal("structured and string JSON have different desired configuration")
	}
}
