package models

import (
	"encoding/json"
	"fmt"
)

const DeploymentParametersSnapshotKey = "_deployment_parameters"

// EmbedDeploymentParameters stores normalized, non-secret deployment values
// alongside existing component-specific configuration.
func EmbedDeploymentParameters(config string, parameters map[string]interface{}) (string, error) {
	values := make(map[string]json.RawMessage)
	if config != "" {
		if err := json.Unmarshal([]byte(config), &values); err != nil {
			return "", fmt.Errorf("component config must be a JSON object: %w", err)
		}
		if values == nil {
			return "", fmt.Errorf("component config must be a JSON object")
		}
	}
	encodedParameters, err := json.Marshal(parameters)
	if err != nil {
		return "", err
	}
	values[DeploymentParametersSnapshotKey] = encodedParameters
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func DeploymentParametersFromConfig(config string) (map[string]interface{}, error) {
	if config == "" {
		return map[string]interface{}{}, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &values); err != nil {
		return nil, fmt.Errorf("invalid component config JSON: %w", err)
	}
	raw, exists := values[DeploymentParametersSnapshotKey]
	if !exists {
		return map[string]interface{}{}, nil
	}
	var parameters map[string]interface{}
	if err := json.Unmarshal(raw, &parameters); err != nil {
		return nil, fmt.Errorf("invalid deployment parameter snapshot: %w", err)
	}
	if parameters == nil {
		parameters = map[string]interface{}{}
	}
	return parameters, nil
}
