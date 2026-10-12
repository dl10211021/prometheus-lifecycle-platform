package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gopkg.in/yaml.v3"
)

// ===== Topology =====

// GetTopology builds edges from effective runtime configuration. Type-based
// inference is retained only when no concrete relationship can be observed.
func (h *OpsHandler) GetTopology(c *gin.Context) {
	clusterID, _ := strconv.Atoi(c.Param("id"))
	var components []models.Component
	h.db.Where("cluster_id = ? AND status <> ?", clusterID, "removed").
		Preload("Host").Preload("Configs").Find(&components)

	nodes := make([]gin.H, 0, len(components))
	edges := []gin.H{}
	seen := map[string]bool{}
	addEdge := func(from, to uint, edgeType, sourceKind, evidence string) {
		if from == 0 || to == 0 || from == to {
			return
		}
		key := fmt.Sprintf("%d:%d:%s", from, to, edgeType)
		if seen[key] {
			return
		}
		seen[key] = true
		edges = append(edges, gin.H{
			"source": from, "target": to, "type": edgeType,
			"source_kind": sourceKind, "evidence": evidence,
		})
	}
	for i := range components {
		nodes = append(nodes, gin.H{
			"id": components[i].ID, "name": components[i].Name,
			"type": components[i].Type, "status": components[i].Status,
		})
	}

	for i := range components {
		comp := &components[i]
		if comp.Type == models.ComponentGrafana {
			for _, datasourceURL := range fetchGrafanaDatasourceURLs(comp) {
				if target := componentForEndpoint(datasourceURL, components); target != nil {
					addEdge(comp.ID, target.ID, "datasource", "runtime_api", datasourceURL)
				}
			}
			continue
		}
		managed := latestActiveConfig(comp.Configs)
		if managed == nil {
			managed = &models.ComponentConfig{Name: defaultOperationalConfigName(comp.Type)}
		}
		content, source, err := readActualComponentConfig(comp, managed)
		if err != nil || strings.TrimSpace(content) == "" {
			continue
		}
		var document map[string]interface{}
		if yaml.Unmarshal([]byte(content), &document) != nil {
			continue
		}
		if comp.Type == models.ComponentPrometheus {
			for _, scrape := range objectList(document["scrape_configs"]) {
				job, _ := scrape["job_name"].(string)
				for _, staticConfig := range objectList(scrape["static_configs"]) {
					for _, targetText := range stringList(staticConfig["targets"]) {
						if target := componentForEndpoint(targetText, components); target != nil {
							addEdge(comp.ID, target.ID, "scrape", "runtime_config", source+" job="+job)
						}
					}
				}
			}
			remoteWrites := objectList(document["remote_write"])
			if len(remoteWrites) == 0 {
				remoteWrites = objectList(document["remote_writes"])
			}
			for _, remoteWrite := range remoteWrites {
				if endpoint, _ := remoteWrite["url"].(string); endpoint != "" {
					if target := componentForEndpoint(endpoint, components); target != nil {
						addEdge(comp.ID, target.ID, "remote_write", "runtime_config", endpoint)
					}
				}
			}
			if alerting, ok := document["alerting"].(map[string]interface{}); ok {
				for _, manager := range objectList(alerting["alertmanagers"]) {
					for _, staticConfig := range objectList(manager["static_configs"]) {
						for _, targetText := range stringList(staticConfig["targets"]) {
							if target := componentForEndpoint(targetText, components); target != nil {
								addEdge(comp.ID, target.ID, "notify", "runtime_config", targetText)
							}
						}
					}
				}
			}
		}
		for _, endpoint := range collectConfigURLs(document) {
			if target := componentForEndpoint(endpoint, components); target != nil {
				addEdge(comp.ID, target.ID, "component_url", "runtime_config", endpoint)
			}
		}
	}

	// Explicit fallbacks keep the graph useful for incomplete third-party
	// configs, while source_kind makes inferred relationships unmistakable.
	for i := range components {
		comp := &components[i]
		switch comp.Type {
		case models.ComponentPrometheus:
			inferScrapes := !hasOutgoingEdge(edges, comp.ID, "scrape")
			inferNotify := !hasOutgoingEdge(edges, comp.ID, "notify")
			for j := range components {
				target := &components[j]
				if inferScrapes && (target.Type == models.ComponentNodeExporter || target.Type == models.ComponentBlackbox) {
					addEdge(comp.ID, target.ID, "scrape", "inferred", "component types")
				}
				if inferNotify && target.Type == models.ComponentAlertmanager {
					addEdge(comp.ID, target.ID, "notify", "inferred", "component types")
				}
			}
		case models.ComponentPromtail:
			if !hasOutgoingEdge(edges, comp.ID, "component_url") {
				for j := range components {
					if components[j].Type == models.ComponentLoki {
						addEdge(comp.ID, components[j].ID, "log", "inferred", "component types")
					}
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"nodes": nodes, "edges": edges})
}

func objectList(value interface{}) []map[string]interface{} {
	items, _ := value.([]interface{})
	result := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]interface{}); ok {
			result = append(result, object)
		}
	}
	return result
}

func stringList(value interface{}) []string {
	items, _ := value.([]interface{})
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func collectConfigURLs(value interface{}) []string {
	result := []string{}
	var walk func(interface{})
	walk = func(current interface{}) {
		switch typed := current.(type) {
		case map[string]interface{}:
			for _, child := range typed {
				walk(child)
			}
		case []interface{}:
			for _, child := range typed {
				walk(child)
			}
		case string:
			if strings.HasPrefix(typed, "http://") || strings.HasPrefix(typed, "https://") {
				result = append(result, typed)
			}
		}
	}
	walk(value)
	return result
}

func componentForEndpoint(endpoint string, components []models.Component) *models.Component {
	key := endpointKey(endpoint)
	if key == "" {
		return nil
	}
	for i := range components {
		comp := &components[i]
		port := comp.Port
		if port == 0 {
			port = defaultOperationalPort(comp.Type)
		}
		candidates := []string{
			endpointKey(comp.InternalURL), endpointKey(comp.ExternalURL),
			strings.ToLower(net.JoinHostPort(comp.Host.IP, strconv.Itoa(port))),
			strings.ToLower(net.JoinHostPort(comp.Name, strconv.Itoa(port))),
			strings.ToLower(net.JoinHostPort(comp.ServiceName, strconv.Itoa(port))),
		}
		for _, candidate := range candidates {
			if candidate != "" && candidate == key {
				return comp
			}
		}
	}
	return nil
}

func endpointKey(endpoint string) string {
	value := strings.TrimSpace(strings.ToLower(endpoint))
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func defaultOperationalPort(componentType models.ComponentType) int {
	switch componentType {
	case models.ComponentPrometheus:
		return 9090
	case models.ComponentAlertmanager:
		return 9093
	case models.ComponentGrafana:
		return 3000
	case models.ComponentLoki:
		return 3100
	case models.ComponentPromtail:
		return 9080
	case models.ComponentNodeExporter:
		return 9100
	case models.ComponentPushgateway:
		return 9091
	case models.ComponentBlackbox:
		return 9115
	default:
		return 0
	}
}

func fetchGrafanaDatasourceURLs(comp *models.Component) []string {
	endpoint := comp.ExternalURL
	if endpoint == "" {
		endpoint = comp.InternalURL
	}
	if endpoint == "" {
		endpoint = "http://" + net.JoinHostPort(comp.Host.IP, strconv.Itoa(defaultOperationalPort(comp.Type)))
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(endpoint, "/")+"/api/datasources", nil)
	if err != nil {
		return nil
	}
	user := os.Getenv("GRAFANA_ADMIN_USER")
	if user == "" {
		user = "admin"
	}
	password := os.Getenv("GRAFANA_ADMIN_PASSWORD")
	if password == "" {
		password = "admin"
	}
	req.SetBasicAuth(user, password)
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var datasources []struct {
		URL string `json:"url"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&datasources) != nil {
		return nil
	}
	result := make([]string, 0, len(datasources))
	for i := range datasources {
		if datasources[i].URL != "" {
			result = append(result, datasources[i].URL)
		}
	}
	return result
}

func hasOutgoingEdge(edges []gin.H, source uint, edgeType string) bool {
	for _, edge := range edges {
		if edge["source"] == source && edge["type"] == edgeType {
			return true
		}
	}
	return false
}
