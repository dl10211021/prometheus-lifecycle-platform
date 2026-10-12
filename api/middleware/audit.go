package middleware

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// AuditLog records authenticated change attempts, including failed results.
// It persists outcome metadata only and never reads or stores request bodies.
// Mount it before role enforcement so denied high-risk attempts are visible.
func AuditLog(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()

		userIDVal, _ := c.Get("user_id")
		usernameVal, _ := c.Get("username")
		userID, _ := userIDVal.(uint)
		username, _ := usernameVal.(string)
		if userID == 0 || username == "" {
			return
		}
		action, resource, resourceID := auditInfo(c)
		if action == "" {
			return
		}
		statusCode := c.Writer.Status()
		result := "success"
		if statusCode >= http.StatusBadRequest {
			result = "failure"
		}
		db.Create(&models.OperationLog{
			UserID:        userID,
			Username:      username,
			Role:          c.GetString("role"),
			Action:        action,
			Resource:      resource,
			ResourceID:    resourceID,
			Description:   auditDescription(c, action, resource),
			Result:        result,
			StatusCode:    statusCode,
			DurationMS:    time.Since(started).Milliseconds(),
			CorrelationID: c.GetString("correlation_id"),
			IPAddress:     c.ClientIP(),
			UserAgent:     c.Request.UserAgent(),
		})
	}
}

func auditInfo(c *gin.Context) (action, resource string, resourceID uint) {
	path := c.Request.URL.Path
	trimmed := strings.TrimPrefix(path, "/api/v1/")
	parts := strings.Split(trimmed, "/")
	method := c.Request.Method
	if len(parts) == 0 || parts[0] == "" {
		return method + " " + path, "api", 0
	}
	resource = parts[0]

	// Pull a numeric :id from the route when present.
	if idStr := c.Param("id"); idStr != "" {
		var v uint
		if _, err := fmt.Sscanf(idStr, "%d", &v); err == nil {
			resourceID = v
		}
	} else if len(parts) >= 2 {
		var v uint
		if _, err := fmt.Sscanf(parts[1], "%d", &v); err == nil {
			resourceID = v
		}
	}

	normParts := make([]string, len(parts))
	for i, p := range parts {
		if _, err := fmt.Sscanf(p, "%d", new(uint)); err == nil {
			normParts[i] = ":id"
		} else {
			normParts[i] = p
		}
	}
	key := method + " " + strings.Join(normParts, "/")
	named := map[string]string{
		"POST clusters":                        "create_cluster",
		"PUT clusters/:id":                     "update_cluster",
		"DELETE clusters/:id":                  "delete_cluster",
		"POST clusters/:id/components":         "create_component",
		"PUT components/:id":                   "update_component",
		"DELETE components/:id":                "delete_component",
		"POST components/:id/restart":          "restart_component",
		"POST components/:id/upgrade":          "upgrade_component",
		"POST components/:id/rollback":         "rollback_component",
		"POST components/:id/uninstall":        "uninstall_component",
		"POST components/:id/configs":          "update_component_config",
		"POST configs/:id/rollback":            "rollback_config",
		"POST hosts":                           "create_host",
		"POST hosts/batch":                     "batch_create_hosts",
		"PUT hosts/:id":                        "update_host",
		"DELETE hosts/:id":                     "delete_host",
		"POST hosts/:id/test":                  "test_host",
		"POST hosts/:id/adopt":                 "adopt_component",
		"POST users":                           "create_user",
		"PUT users/:id":                        "update_user",
		"DELETE users/:id":                     "delete_user",
		"POST packages":                        "create_package",
		"POST packages/upload":                 "upload_package",
		"DELETE packages/:id":                  "delete_package",
		"POST packages/official-download":      "download_package",
		"POST packages/download-all":           "download_all_packages",
		"POST versions":                        "create_version",
		"PUT versions/:id":                     "update_version",
		"DELETE versions/:id":                  "delete_version",
		"POST versions/refresh":                "refresh_versions",
		"POST backups":                         "create_backup",
		"POST backups/:id/restore":             "restore_backup",
		"DELETE backups/:id":                   "delete_backup",
		"POST backups/platform/restore":        "restore_platform_backup",
		"GET backups/platform/download":        "download_platform_backup",
		"POST backups/scheduled":               "create_scheduled_backup",
		"PUT backups/scheduled/:id":            "update_scheduled_backup",
		"DELETE backups/scheduled/:id":         "delete_scheduled_backup",
		"POST alerts/rules":                    "create_alert_rule",
		"PUT alerts/rules/:id":                 "update_alert_rule",
		"DELETE alerts/rules/:id":              "delete_alert_rule",
		"POST scrape-configs":                  "create_scrape_config",
		"PUT scrape-configs/:id":               "update_scrape_config",
		"DELETE scrape-configs/:id":            "delete_scrape_config",
		"POST recording-rules":                 "create_recording_rule",
		"PUT recording-rules/:id":              "update_recording_rule",
		"DELETE recording-rules/:id":           "delete_recording_rule",
		"POST remote-writes":                   "create_remote_write",
		"PUT remote-writes/:id":                "update_remote_write",
		"DELETE remote-writes/:id":             "delete_remote_write",
		"POST notifications/channels":          "create_notification_channel",
		"PUT notifications/channels/:id":       "update_notification_channel",
		"DELETE notifications/channels/:id":    "delete_notification_channel",
		"POST notifications/channels/:id/test": "test_notification_channel",
		"POST dashboards":                      "create_dashboard",
		"PUT dashboards/:id":                   "update_dashboard",
		"DELETE dashboards/:id":                "delete_dashboard",
		"POST dashboards/import":               "import_dashboard",
		"POST dashboards/upload":               "upload_dashboard",
		"POST maintenance":                     "create_maintenance_window",
		"DELETE maintenance/:id":               "delete_maintenance_window",
		"POST certificates":                    "generate_certificate",
		"DELETE certificates/:id":              "delete_certificate",
		"POST templates":                       "create_template",
		"PUT templates/:id":                    "update_template",
		"DELETE templates/:id":                 "delete_template",
		"POST templates/upload":                "upload_template",
		"POST templates/:id/render":            "render_template",
		"POST alerts/silences/:id":             "create_silence",
		"DELETE alerts/silences/:id/:id":       "delete_silence",
		"POST prometheus/validate":             "validate_prometheus_config",
		"POST alertmanager/validate":           "validate_alertmanager_config",
		"POST scrape-configs/sync/:cluster_id": "sync_prometheus_config",
	}
	if a, ok := named[key]; ok {
		action = a
	} else {
		action = strings.ToLower(method) + "_" + strings.ReplaceAll(strings.Join(parts, "_"), ":", "")
	}
	return action, resource, resourceID
}

func auditDescription(c *gin.Context, action, resource string) string {
	if action == "" {
		action = "api"
	}
	return fmt.Sprintf("%s %s (%s)", action, c.Request.URL.Path, c.Request.Method)
}
