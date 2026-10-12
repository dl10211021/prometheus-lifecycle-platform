package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func userHandlerTestSetup(t *testing.T) (*gorm.DB, *Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "users.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatal(err)
	}
	return db, &Handler{db: db}
}

func jsonRequest(t *testing.T, method, path string, body interface{}) *http.Request {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestCreateUserValidatesRoleAndPersistsDisabledState(t *testing.T) {
	db, handler := userHandlerTestSetup(t)
	router := gin.New()
	router.POST("/users", handler.CreateUser)

	invalid := httptest.NewRecorder()
	router.ServeHTTP(invalid, jsonRequest(t, http.MethodPost, "/users", map[string]interface{}{
		"username": "invalid-role", "password": "Acceptance!2026", "role": "administrator",
	}))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid role status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	disabled := httptest.NewRecorder()
	router.ServeHTTP(disabled, jsonRequest(t, http.MethodPost, "/users", map[string]interface{}{
		"username": "disabled-user", "password": "Acceptance!2026", "role": "viewer", "is_active": false,
	}))
	if disabled.Code != http.StatusCreated {
		t.Fatalf("disabled user status=%d body=%s", disabled.Code, disabled.Body.String())
	}
	var created models.User
	if err := json.Unmarshal(disabled.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var stored models.User
	if err := db.First(&stored, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if created.IsActive || stored.IsActive {
		t.Fatalf("explicitly disabled user became active: response=%t stored=%t", created.IsActive, stored.IsActive)
	}
	if stored.Password == "Acceptance!2026" || !utils.CheckPassword("Acceptance!2026", stored.Password) {
		t.Fatal("user password was not stored as a verifiable hash")
	}
}

func TestUserCannotDemoteDisableOrDeleteSelf(t *testing.T) {
	db, handler := userHandlerTestSetup(t)
	admin := models.User{
		Username: "admin", Password: utils.HashPassword("Acceptance!2026"),
		Role: string(models.RoleSystemAdmin), IsActive: true,
	}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	other := models.User{
		Username: "other-admin", Password: utils.HashPassword("Acceptance!2026"),
		Role: string(models.RoleSystemAdmin), IsActive: true,
	}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", admin.ID)
		c.Next()
	})
	router.PUT("/users/:id", handler.UpdateUser)
	router.DELETE("/users/:id", handler.DeleteUser)
	adminPath := "/users/" + strconv.FormatUint(uint64(admin.ID), 10)
	for name, body := range map[string]interface{}{
		"demote":  map[string]interface{}{"role": "viewer"},
		"disable": map[string]interface{}{"is_active": false},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, jsonRequest(t, http.MethodPut, adminPath, body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("self %s status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	selfDelete := httptest.NewRecorder()
	router.ServeHTTP(selfDelete, httptest.NewRequest(http.MethodDelete, adminPath, nil))
	if selfDelete.Code != http.StatusBadRequest {
		t.Fatalf("self delete status=%d body=%s", selfDelete.Code, selfDelete.Body.String())
	}

	otherDelete := httptest.NewRecorder()
	otherPath := "/users/" + strconv.FormatUint(uint64(other.ID), 10)
	router.ServeHTTP(otherDelete, httptest.NewRequest(http.MethodDelete, otherPath, nil))
	if otherDelete.Code != http.StatusOK {
		t.Fatalf("other admin delete status=%d body=%s", otherDelete.Code, otherDelete.Body.String())
	}
	var reloaded models.User
	if err := db.First(&reloaded, admin.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsActive || reloaded.Role != string(models.RoleSystemAdmin) {
		t.Fatalf("self protection changed admin: %#v", reloaded)
	}
}
