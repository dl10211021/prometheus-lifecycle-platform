package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Accept JSON values and JSON-encoded form strings identically on create and
// update. Omitted fields remain absent; null/empty strings explicitly clear.
func normalizeScrapeJSONFields(fields map[string]json.RawMessage) (map[string]string, error) {
	normalized := make(map[string]string, len(fields))
	for name, raw := range fields {
		if len(raw) == 0 {
			continue
		}
		var value interface{}
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", name, err)
		}
		if encoded, ok := value.(string); ok {
			if strings.TrimSpace(encoded) == "" {
				normalized[name] = ""
				continue
			}
			if err := json.Unmarshal([]byte(encoded), &value); err != nil {
				return nil, fmt.Errorf("invalid %s: %w", name, err)
			}
		}
		if value == nil {
			normalized[name] = ""
			continue
		}
		if name == "labels" {
			if _, ok := value.(map[string]interface{}); !ok {
				return nil, fmt.Errorf("%s must be a JSON object", name)
			}
		} else if _, ok := value.([]interface{}); !ok {
			return nil, fmt.Errorf("%s must be a JSON array", name)
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		normalized[name] = string(canonical)
	}
	return normalized, nil
}
