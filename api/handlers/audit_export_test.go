package handlers

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAuditCSVIncludesUTF8BOMAndEscapesFormulaCells(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "audit-export.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.OperationLog{}); err != nil {
		t.Fatal(err)
	}
	logEntry := models.OperationLog{
		UserID: 7, Username: "=cmd", Role: string(models.RoleSystemAdmin),
		Action: "+formula", Resource: "users", ResourceID: 1,
		Result: "success", StatusCode: http.StatusOK, IPAddress: "127.0.0.1",
		CorrelationID: "@correlation", CreatedAt: time.Now(),
	}
	if err := db.Create(&logEntry).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/export", NewAuditExportHandler(db).ExportCSV)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/export", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.Bytes()
	if len(body) < 3 || body[0] != 0xEF || body[1] != 0xBB || body[2] != 0xBF {
		t.Fatalf("audit CSV does not start with UTF-8 BOM: %x", body[:min(len(body), 8)])
	}
	rows, err := csv.NewReader(strings.NewReader(string(body[3:]))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1][1] != "'=cmd" || rows[1][3] != "'+formula" || rows[1][10] != "'@correlation" {
		t.Fatalf("unexpected escaped CSV rows: %#v", rows)
	}
}
