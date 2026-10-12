package services

import (
	"fmt"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// MaintenanceMatchers uses the labels emitted by the managed scrape configuration.
// Missing or deleted scopes are errors, never an all-alerts silence.
func MaintenanceMatchers(db *gorm.DB, window *models.MaintenanceWindow) ([]map[string]interface{}, error) {
	if window.ComponentID == nil && window.ClusterID == nil {
		return nil, fmt.Errorf("维护窗口必须指定存在的组件或节点")
	}
	matcher := func(name, value string) map[string]interface{} {
		return map[string]interface{}{"name": name, "value": value, "isRegex": false, "isEqual": true}
	}
	if window.ComponentID != nil {
		var component models.Component
		if err := db.First(&component, *window.ComponentID).Error; err != nil {
			return nil, fmt.Errorf("维护组件不存在: %w", err)
		}
		if window.ClusterID != nil && *window.ClusterID != component.ClusterID {
			return nil, fmt.Errorf("维护组件不属于所选节点")
		}
		if component.Name == "" || component.Type == "" {
			return nil, fmt.Errorf("维护组件缺少标签身份")
		}
		return []map[string]interface{}{matcher("component", string(component.Type)), matcher("name", component.Name), matcher("node", "single-node")}, nil
	}
	var cluster models.Cluster
	if err := db.First(&cluster, *window.ClusterID).Error; err != nil {
		return nil, fmt.Errorf("维护节点不存在: %w", err)
	}
	nodeID, err := SingleNodeID(db)
	if err != nil || nodeID != cluster.ID {
		return nil, fmt.Errorf("维护节点不是当前监控节点")
	}
	return []map[string]interface{}{matcher("node", "single-node")}, nil
}
