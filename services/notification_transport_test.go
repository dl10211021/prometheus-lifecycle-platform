package services

import (
	"encoding/json"
	"github.com/promlifecycle/platform/models"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDirectTransportsUseCanonicalKeysAndProviderAcknowledgement(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_ALLOW_PRIVATE_FETCH", "1")
	for _, test := range []struct{ kind, key, response, want string }{
		{"slack", "api_url", "ok", "sent"},
		{"dingtalk", "webhook_url", `{"errcode":0}`, "sent"},
		{"dingtalk", "webhook_url", `{"errcode":310000,"errmsg":"rejected"}`, "failed"},
		{"dingtalk", "webhook_url", `{}`, "failed"},
	} {
		t.Run(test.kind+test.response, func(t *testing.T) {
			payloads := make(chan map[string]interface{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]interface{}
				json.NewDecoder(r.Body).Decode(&payload)
				payloads <- payload
				w.Write([]byte(test.response))
			}))
			defer server.Close()
			configBytes, _ := json.Marshal(map[string]string{test.key: server.URL})
			channel := models.NotificationChannel{Type: test.kind, Config: string(configBytes)}
			status, message := NewNotificationService(nil).deliver(&channel, "event", "hello", "warning")
			if status != test.want {
				t.Fatalf("delivery %s: %s", status, message)
			}
			var payload map[string]interface{}
			select {
			case payload = <-payloads:
			default:
				t.Fatal("delivery never reached provider")
			}
			if test.kind == "slack" && payload["blocks"] == nil {
				t.Fatalf("Slack payload: %v", payload)
			}
			if test.kind == "dingtalk" && payload["msgtype"] != "text" {
				t.Fatalf("DingTalk payload: %v", payload)
			}
		})
	}
	channel := models.NotificationChannel{Type: "wechat", Config: `{"corp_id":"corp","agent_id":"1001"}`, Secret: "application-secret"}
	if status, _ := NewNotificationService(nil).deliver(&channel, "event", "hello", "warning"); status != "failed" {
		t.Fatal("corporate WeChat was treated as robot webhook")
	}
}
