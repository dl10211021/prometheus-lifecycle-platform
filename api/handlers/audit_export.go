package handlers

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AuditExportHandler streams filtered audit outcomes without request bodies,
// descriptions, user agents, or any other field that could contain secrets.
type AuditExportHandler struct {
	db *gorm.DB
}

func NewAuditExportHandler(db *gorm.DB) *AuditExportHandler {
	return &AuditExportHandler{db: db}
}

func (h *AuditExportHandler) ExportCSV(c *gin.Context) {
	query := h.db.Table("operation_logs").
		Select(`operation_logs.created_at,
			operation_logs.username,
			COALESCE(NULLIF(operation_logs.role, ''), users.role, '') AS role,
			operation_logs.action,
			operation_logs.resource,
			operation_logs.resource_id,
			COALESCE(NULLIF(operation_logs.result, ''), 'success') AS result,
			operation_logs.status_code,
			operation_logs.duration_ms,
			operation_logs.ip_address,
			operation_logs.correlation_id`).
		Joins("LEFT JOIN users ON users.id = operation_logs.user_id").
		Order("operation_logs.created_at DESC")

	if raw := c.Query("user_id"); raw != "" {
		userID, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || userID == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_id 必须是正整数"})
			return
		}
		query = query.Where("operation_logs.user_id = ?", userID)
	}
	if username := strings.TrimSpace(c.Query("user")); username != "" {
		query = query.Where("operation_logs.username = ?", username)
	}
	if action := strings.TrimSpace(c.Query("action")); action != "" {
		query = query.Where("operation_logs.action LIKE ?", "%"+action+"%")
	}
	if resource := strings.TrimSpace(c.Query("resource")); resource != "" {
		query = query.Where("operation_logs.resource = ?", resource)
	}
	if startDate := strings.TrimSpace(c.Query("start_date")); startDate != "" {
		query = query.Where("operation_logs.created_at >= ?", startDate)
	}
	if endDate := strings.TrimSpace(c.Query("end_date")); endDate != "" {
		query = query.Where("operation_logs.created_at <= ?", endDate)
	}

	rows, err := query.Rows()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出审计日志失败"})
		return
	}
	defer rows.Close()

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="audit-log.csv"`)
	if _, err := c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return
	}
	writer := csv.NewWriter(c.Writer)
	defer writer.Flush()
	if err := writer.Write([]string{"time", "user", "role", "action", "resource", "resource_id", "result", "status_code", "duration_ms", "ip", "correlation_id"}); err != nil {
		return
	}

	for rows.Next() {
		var (
			createdAt     time.Time
			username      string
			role          string
			action        string
			resource      string
			resourceID    uint
			result        string
			statusCode    int
			durationMS    int64
			ipAddress     string
			correlationID string
		)
		if err := rows.Scan(&createdAt, &username, &role, &action, &resource, &resourceID, &result, &statusCode, &durationMS, &ipAddress, &correlationID); err != nil {
			return
		}
		if err := writer.Write([]string{
			createdAt.UTC().Format(time.RFC3339Nano),
			safeCSVCell(username),
			safeCSVCell(role),
			safeCSVCell(action),
			safeCSVCell(resource),
			strconv.FormatUint(uint64(resourceID), 10),
			safeCSVCell(result),
			strconv.Itoa(statusCode),
			strconv.FormatInt(durationMS, 10),
			safeCSVCell(ipAddress),
			safeCSVCell(correlationID),
		}); err != nil {
			return
		}
	}
}

func safeCSVCell(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@':
		return "'" + value
	default:
		return value
	}
}
