package models

import (
	"net/url"
	"path"
	"strings"
)

func ManagedHealthPath(component *Component) string {
	if component == nil {
		return ""
	}
	defaultPath := "/metrics"
	switch component.Type {
	case ComponentPrometheus, ComponentAlertmanager:
		defaultPath = "/-/ready"
	case ComponentGrafana:
		defaultPath = "/api/health"
	case ComponentLoki, ComponentPromtail:
		defaultPath = "/ready"
	case ComponentPushgateway, ComponentBlackbox:
		defaultPath = "/-/healthy"
	case ComponentThanos:
		defaultPath = "/-/ready"
	}
	if snapshot, found, err := ComponentDefinitionSnapshotFromConfig(component.Config); err == nil && found && snapshot.HealthPath != "" {
		defaultPath = snapshot.HealthPath
	}
	prefix := ManagedRoutePrefix(component)
	if prefix == "" {
		return defaultPath
	}
	return "/" + strings.TrimPrefix(path.Join(prefix, defaultPath), "/")
}

func ManagedRoutePrefix(component *Component) string {
	if component == nil {
		return ""
	}
	parameters, err := DeploymentParametersFromConfig(component.Config)
	if err != nil {
		return ""
	}
	prefix := ""
	switch component.Type {
	case ComponentPrometheus:
		prefix = urlPathParameter(parameters, "external_url")
	case ComponentAlertmanager:
		prefix, _ = parameters["route_prefix"].(string)
	case ComponentGrafana:
		if enabled, _ := parameters["serve_from_sub_path"].(bool); enabled {
			prefix = urlPathParameter(parameters, "root_url")
		}
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || prefix == "/" {
		return ""
	}
	return "/" + strings.Trim(prefix, "/")
}

func urlPathParameter(parameters map[string]interface{}, field string) string {
	raw, _ := parameters[field].(string)
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Path
}
