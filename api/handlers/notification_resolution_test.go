package handlers

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestUnknownDeliveryResolutionRequiresExplicitAuditedDecision(t *testing.T) {
	for _, resolution := range []string{"confirmed_sent", "retry"} {
		t.Run(resolution, func(t *testing.T) {
			db := notificationAuditDB(t)
			if err := db.AutoMigrate(&models.InspectionDelivery{}, &models.InspectionDeliveryResolution{}); err != nil {
				t.Fatal(err)
			}
			delivery := models.InspectionDelivery{TransitionID: 1, ChannelID: 2, Status: "unknown", Attempts: 1}
			if err := db.Create(&delivery).Error; err != nil {
				t.Fatal(err)
			}
			handler := NewNotificationHandler(db, nil, t.TempDir())
			request := func(role, reason string) *httptest.ResponseRecorder {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				body, _ := json.Marshal(map[string]string{"resolution": resolution, "reason": reason})
				c.Request = httptest.NewRequest(http.MethodPost, "/notifications/deliveries/1/resolve", strings.NewReader(string(body)))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(int(delivery.ID))}}
				c.Set("role", role)
				c.Set("user_id", uint(7))
				c.Set("username", "operator")
				handler.ResolveDelivery(c)
				return recorder
			}
			if response := request("viewer", "checked receipt"); response.Code != http.StatusForbidden {
				t.Fatalf("viewer resolved delivery: %s", response.Body.String())
			}
			if response := request(string(models.RoleSystemAdmin), " "); response.Code != http.StatusBadRequest {
				t.Fatalf("missing reason admitted: %s", response.Body.String())
			}
			response := request(string(models.RoleSystemAdmin), "checked provider receipt")
			if response.Code != http.StatusOK {
				t.Fatalf("resolution: %d %s", response.Code, response.Body.String())
			}
			if err := db.First(&delivery, delivery.ID).Error; err != nil {
				t.Fatal(err)
			}
			want := "sent"
			if resolution == "retry" {
				want = "pending"
			}
			if delivery.Status != want || delivery.Attempts != 1 {
				t.Fatalf("resolution sent automatically or wrong state: %+v", delivery)
			}
			if resolution == "confirmed_sent" && delivery.AcknowledgedAt == nil {
				t.Fatal("confirmed receipt lacks acknowledgement")
			}
			if resolution == "retry" && !strings.Contains(response.Body.String(), "warning") {
				t.Fatal("duplicate risk warning omitted")
			}
			if response := request(string(models.RoleSystemAdmin), "repeat"); response.Code != http.StatusConflict {
				t.Fatalf("stale resolution accepted: %s", response.Body.String())
			}
			var audits []models.InspectionDeliveryResolution
			if err := db.Find(&audits).Error; err != nil {
				t.Fatal(err)
			}
			if len(audits) != 1 || audits[0].Reason != "checked provider receipt" || audits[0].UserID != 7 || audits[0].Resolution != resolution {
				t.Fatalf("resolution audit: %+v", audits)
			}
		})
	}
}
