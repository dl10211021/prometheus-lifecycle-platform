package handlers

import (
	"fmt"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"reflect"
)

// The create transaction overrides GORM's default:true substitution before
// publication can observe the row. Client-supplied publication state is ignored.
func createMonitoring(db *gorm.DB, record interface{}) error {
	return config.WithMonitoringLock(func() error {
		value := reflect.ValueOf(record).Elem()
		for i := range value.NumField() {
			field := value.Field(i)
			if field.Kind() == reflect.String && utils.HasRedactedSecrets(field.String()) {
				return fmt.Errorf("redacted configuration cannot be saved; load authorized raw configuration before editing")
			}
		}
		enabled := value.FieldByName("Enabled").Bool()
		scope := value.FieldByName("ClusterID")
		if scope.Uint() == 0 {
			id, err := services.SingleNodeID(db)
			if err != nil {
				return err
			}
			scope.SetUint(uint64(id))
		}
		value.FieldByName("ID").SetUint(0)
		value.FieldByName("PublicationState").Set(reflect.ValueOf(models.PublicationState{ApplyStatus: "pending"}))
		return db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(record).Error; err != nil {
				return err
			}
			if err := tx.Model(record).UpdateColumn("enabled", enabled).Error; err != nil {
				return err
			}
			value.FieldByName("Enabled").SetBool(enabled)
			return nil
		})
	})
}

func updateMonitoring(db *gorm.DB, record interface{}, updates map[string]interface{}) error {
	return config.WithMonitoringLock(func() error {
		for _, value := range updates {
			if text, ok := value.(string); ok && utils.HasRedactedSecrets(text) {
				return fmt.Errorf("redacted configuration cannot be saved; omit unchanged credentials or use an authorized raw read")
			}
		}
		updates["apply_status"] = "pending"
		updates["last_error"] = ""
		updates["delete_pending"] = false
		result := db.Model(record).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

// A deletion remains visible and retriable until its absence is acknowledged by
// the publisher. Retrying DELETE or the existing sync endpoint is safe.
func deleteMonitoring(db *gorm.DB, record interface{}) error {
	return config.WithMonitoringLock(func() error {
		result := db.Model(record).Updates(map[string]interface{}{
			"enabled": false, "delete_pending": true, "apply_status": "pending", "last_error": "",
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

type remoteWriteResponse struct {
	models.RemoteWriteModel
	PasswordConfigured    bool `json:"password_configured"`
	BearerTokenConfigured bool `json:"bearer_token_configured"`
}

func remoteWriteDTO(item models.RemoteWriteModel) remoteWriteResponse {
	configured, bearer := item.BasicAuthPassword != "", item.BearerToken != ""
	item.BasicAuthPassword, item.BearerToken = "", ""
	item.URL = utils.SanitizeConfigURL(item.URL)
	return remoteWriteResponse{RemoteWriteModel: item, PasswordConfigured: configured, BearerTokenConfigured: bearer}
}

func scrapeConfigDTO(item models.ScrapeConfigModel) models.ScrapeConfigModel {
	item.HTTPSDConfigs = utils.SanitizeConfig("prometheus", "json", item.HTTPSDConfigs)
	return item
}
