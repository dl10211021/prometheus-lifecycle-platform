package models

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func init() {
	_ = os.Setenv("PROMLIFECYCLE_JWT_SECRET", "ai-model-test-secret")
}

func TestAIConfigEncryptsAndNeverMarshalsAPIKey(t *testing.T) {
	cfg := AIConfig{APIKey: "sk-sensitive-value"}
	if err := cfg.BeforeSave(nil); err != nil {
		t.Fatalf("BeforeSave: %v", err)
	}
	if cfg.APIKey == "sk-sensitive-value" || !strings.HasPrefix(cfg.APIKey, "enc:v1:") {
		t.Fatalf("APIKey was not encrypted at rest: %q", cfg.APIKey)
	}
	firstCiphertext := cfg.APIKey
	if err := cfg.BeforeSave(nil); err != nil {
		t.Fatalf("second BeforeSave: %v", err)
	}
	if cfg.APIKey != firstCiphertext {
		t.Fatal("idempotent save changed encrypted API key")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "sk-sensitive-value") || strings.Contains(string(encoded), "APIKey") || strings.Contains(string(encoded), "api_key") {
		t.Fatalf("JSON exposed API key: %s", encoded)
	}
	if err := cfg.AfterFind(nil); err != nil {
		t.Fatalf("AfterFind: %v", err)
	}
	if cfg.APIKey != "sk-sensitive-value" {
		t.Fatalf("decrypted APIKey = %q", cfg.APIKey)
	}
}

func TestAIConfigBeforeCreateForcesSingletonID(t *testing.T) {
	cfg := AIConfig{ID: 99}
	if err := cfg.BeforeCreate(nil); err != nil {
		t.Fatalf("BeforeCreate: %v", err)
	}
	if cfg.ID != AIConfigSingletonID {
		t.Fatalf("ID = %d, want singleton %d", cfg.ID, AIConfigSingletonID)
	}
}
