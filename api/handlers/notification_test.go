package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func serveHandlerRequest(handler gin.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	return recorder
}

func TestNotificationChannelDisabledDraftDoesNotRequireAlertmanager(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:notification-draft?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.Cluster{}, &models.NotificationChannel{}, &models.InspectionDelivery{}, &models.InspectionNotificationPolicy{}); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	handler := NewNotificationHandler(db, nil, t.TempDir())
	response := serveHandlerRequest(handler.CreateChannel, http.MethodPost, "/api/v1/notifications/channels", `{"name":"draft","type":"webhook","config":"{\"webhook\":\"https://example.invalid/hook\"}","enabled":false}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var body notificationChannelResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Enabled || body.ApplyStatus != "draft" || body.ID == 0 {
		t.Fatalf("unexpected draft response: %+v", body)
	}
	var stored models.NotificationChannel
	if err := db.First(&stored, body.ID).Error; err != nil {
		t.Fatalf("load channel: %v", err)
	}
	if stored.Enabled || stored.ApplyStatus != "draft" {
		t.Fatalf("unexpected stored channel: %+v", stored)
	}
	deleted := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(deleted)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/notifications/channels/"+strconv.FormatUint(uint64(body.ID), 10), nil)
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(body.ID), 10)}}
	handler.DeleteChannel(ctx)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete draft status=%d body=%s", deleted.Code, deleted.Body.String())
	}
}
