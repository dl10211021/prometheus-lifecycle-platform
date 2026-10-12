package handlers

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParameterMigrationPreviewAPIReadsFreshStateWithoutWrites(t *testing.T) {
	db := newAIHandlerTestDB(t, &models.Component{}, &models.ChangePlan{}, &models.Backup{}, &models.Operation{})
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", ImportedArgs: `["--storage.tsdb.retention=30d","--opaque=private-secret"]`, CustomEnv: "PRIVATE=env-secret"}
	if err := db.Omit("Host").Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	request := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Params = gin.Params{{Key: "id", Value: "1"}}
		ctx.Set("single_node_id", uint(7))
		ctx.Request = httptest.NewRequest("GET", "/api/v1/components/1/parameter-migration-preview?"+query, nil)
		h.PreviewPrometheusParameterMigration(ctx)
		return w
	}
	for _, value := range []string{"30d", "045d"} {
		if err := db.Model(&c).Update("imported_args", `["--storage.tsdb.retention=`+value+`","--opaque=private-secret"]`).Error; err != nil {
			t.Fatal(err)
		}
		var before models.Component
		db.First(&before, c.ID)
		w := request("target_version=3.1.0&cluster_id=7")
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var response struct {
			ComponentID uint                                       `json:"component_id"`
			Preview     models.PrometheusParameterMigrationPreview `json:"preview"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.ComponentID != c.ID || response.Preview.Status != "available" || response.Preview.Changes[0].Value != value {
			t.Fatalf("%+v", response)
		}
		if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "custom_env") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("preview exposed raw state or can be cached")
		}
		var after models.Component
		db.First(&after, c.ID)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("preview wrote component")
		}
		for _, model := range []interface{}{&models.ChangePlan{}, &models.Backup{}, &models.Operation{}} {
			var n int64
			if err := db.Model(model).Count(&n).Error; err != nil || n != 0 {
				t.Fatal("preview wrote lifecycle records")
			}
		}
	}
	for _, q := range []string{"", "target_version=", "target_version=3.1.0&target_version=2.55.0", "target_version=3.1.0&arguments=x", "target_version=3.1.0&cluster_id=8", "target_version=3.1.0&source_version=2.55.0", "target_version=%ZZ", "target_version=" + strings.Repeat("a", 129)} {
		if w := request(q); w.Code != 400 {
			t.Errorf("query %q status=%d", q, w.Code)
		}
	}
	if w := request("target_version=3.1.0-rc.0"); w.Code != 200 || !strings.Contains(w.Body.String(), "unsupported_version") {
		t.Fatal("unknown version did not return blocked preview")
	}
	if err := db.Delete(&c).Error; err != nil {
		t.Fatal(err)
	}
	if w := request("target_version=3.1.0"); w.Code != 404 {
		t.Fatalf("missing=%d", w.Code)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if w := request("target_version=3.1.0"); w.Code != 500 || strings.Contains(w.Body.String(), "closed") {
		t.Fatalf("db failure=%d %s", w.Code, w.Body.String())
	}
}
func TestParameterMigrationPreviewAPIRejectsInvalidIDBeforeDB(t *testing.T) {
	for _, id := range []string{"0", "-1", "1 OR 1=1", "18446744073709551616", ""} {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Params = gin.Params{{Key: "id", Value: id}}
		ctx.Request = httptest.NewRequest("GET", "/preview?target_version=3.1.0", nil)
		(&Handler{}).PreviewPrometheusParameterMigration(ctx)
		if w.Code != 400 {
			t.Fatalf("id=%q code=%d", id, w.Code)
		}
	}
}
