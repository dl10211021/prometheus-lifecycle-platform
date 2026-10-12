package handlers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPersistComponentPowerStateClearsHealthPollError(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "component-power.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	component := models.Component{
		Name: "component-power-state", Type: models.ComponentBlackbox,
		Status: models.StatusError, Error: "not running (detected by health poll)",
	}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}

	if err := persistComponentPowerState(db, &component, models.StatusRunning); err != nil {
		t.Fatal(err)
	}
	var stored models.Component
	if err := db.First(&stored, component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.StatusRunning || stored.Error != "" {
		t.Fatalf("status=%q error=%q, want running with no stale error", stored.Status, stored.Error)
	}
}

func TestRequireManagedAdoptionBlocksObservedComponent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "observed-component.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}, &models.ComponentAdoption{}); err != nil {
		t.Fatal(err)
	}
	component := models.Component{Name: "observed", Type: models.ComponentBlackbox}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ComponentAdoption{ComponentID: component.ID, Ownership: "observe"}).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.POST("/components/:id/rollback", RequireManagedAdoption(db), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/components/"+strconv.FormatUint(uint64(component.ID), 10)+"/rollback", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("observed component mutation status=%d body=%s", response.Code, response.Body.String())
	}
}
