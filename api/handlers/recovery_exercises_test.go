package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestInterruptedExerciseCanBeEditedAndDeleted(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "exercises.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.RecoveryExercise{}); err != nil {
		t.Fatal(err)
	}
	interrupted := models.RecoveryExercise{Name: "interrupted", BackupID: 1, ComponentID: 1, Scope: "config", Status: "running", Result: "stale-success"}
	complete := models.RecoveryExercise{Name: "complete", BackupID: 1, ComponentID: 1, Scope: "config", Status: "success", Result: "archive-evidence"}
	if err := db.Create(&interrupted).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&complete).Error; err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedExercises(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&interrupted, interrupted.ID).Error; err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != "interrupted" || interrupted.Result != "" || interrupted.CompletedAt == nil {
		t.Fatalf("abandoned exercise misreported: %#v", interrupted)
	}
	if err := db.First(&complete, complete.ID).Error; err != nil {
		t.Fatal(err)
	}
	if complete.Status != "success" || complete.Result != "archive-evidence" {
		t.Fatalf("completed evidence changed: %#v", complete)
	}
	h := NewRecoveryExercisesHandler(db)
	router := gin.New()
	router.PUT("/exercises/:id", h.Update)
	router.DELETE("/exercises/:id", h.Delete)
	path := "/exercises/" + strconv.FormatUint(uint64(interrupted.ID), 10)
	update := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"name":"retryable"}`))
	update.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, update)
	if response.Code != http.StatusOK {
		t.Fatalf("edit interrupted exercise: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("delete interrupted exercise: %d %s", response.Code, response.Body.String())
	}
}

func TestRecentOperationsPaginationRetainsTotalAcrossPages(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "operations.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Operation{}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := db.Create(&models.Operation{Status: "success"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	router.GET("/operations", NewOperationsHandler(db).GetRecentOperations)
	seen := map[uint]bool{}
	for _, offset := range []int{0, 2, 4} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/operations?offset="+strconv.Itoa(offset)+"&limit=2", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("list operations: %d %s", response.Code, response.Body.String())
		}
		var page struct {
			Operations []operationResponse `json:"operations"`
			Total      int64               `json:"total"`
			Offset     int                 `json:"offset"`
			Limit      int                 `json:"limit"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != 5 || page.Offset != offset || page.Limit != 2 {
			t.Fatalf("pagination evidence: %#v", page)
		}
		for _, operation := range page.Operations {
			if seen[operation.ID] {
				t.Fatalf("operation %d repeated across pages", operation.ID)
			}
			seen[operation.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("pagination lost operations: %v", seen)
	}
}
