package services

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

var ErrInspectionBusy = errors.New("巡检正在执行或本分钟已执行")

type inspectionClaimKey struct{}

func ClaimInspection(ctx context.Context, db *gorm.DB) (context.Context, func(), error) {
	if owned, _ := ctx.Value(inspectionClaimKey{}).(bool); owned {
		return ctx, func() {}, nil
	}
	claim := models.InspectionClaim{Resource: "inspection"}
	result := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
	if result.Error != nil {
		return ctx, nil, result.Error
	}
	if result.RowsAffected == 0 {
		return ctx, nil, ErrInspectionBusy
	}
	return context.WithValue(ctx, inspectionClaimKey{}, true), func() { db.Where("resource = ?", "inspection").Delete(&models.InspectionClaim{}) }, nil
}

// RecoverInterruptedInspections runs only at exclusive server startup, before workers.
func RecoverInterruptedInspections(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if err := tx.Model(&models.InspectionRun{}).Where("status = ?", "running").Updates(map[string]interface{}{"status": "interrupted", "result": "unknown", "error": "server stopped before inspection completed", "completed_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ScheduledInspectionStatus{}).Where("last_status = ?", "running").Updates(map[string]interface{}{"last_status": "interrupted", "last_result": "unknown", "last_error": "server stopped before inspection completed"}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.InspectionDelivery{}).Where("status = ?", "sending").Updates(map[string]interface{}{"status": "unknown", "last_error": "server stopped during delivery; receipt cannot be established; automatic resend withheld"}).Error; err != nil {
			return err
		}
		return tx.Where("resource = ?", "inspection").Delete(&models.InspectionClaim{}).Error
	})
}
