package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sync"
	"time"
)

type hostClaimContextKey struct{}
type hostClaimContext struct {
	id    uint
	token string
}

// ClaimHostOperation must be acquired before any component claims.
func ClaimHostOperation(ctx context.Context, db *gorm.DB, id uint, kind string) (context.Context, func(), error) {
	if err := rejectPairedRestoreJournalClaim(ctx, db, id, 0); err != nil {
		return ctx, nil, err
	}
	if held, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext); ok && held.id == id {
		var count int64
		if err := db.WithContext(ctx).Model(&models.HostOperationClaim{}).Where("host_id = ? AND token = ?", id, held.token).Count(&count).Error; err != nil {
			return ctx, nil, err
		}
		if count != 1 {
			return ctx, nil, ErrComponentBusy
		}
		return ctx, func() {}, nil
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ctx, nil, err
	}
	token := hex.EncodeToString(random[:])
	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&models.HostOperationClaim{HostID: id, Token: token, Kind: kind, CreatedAt: time.Now()})
	if result.Error != nil {
		return ctx, nil, result.Error
	}
	if result.RowsAffected != 1 {
		return ctx, nil, ErrComponentBusy
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			db.WithContext(cleanup).Where("host_id = ? AND token = ?", id, token).Delete(&models.HostOperationClaim{})
		})
	}
	if err := rejectPairedRestoreJournalClaim(ctx, db, id, 0); err != nil {
		release()
		return ctx, nil, err
	}
	return context.WithValue(ctx, hostClaimContextKey{}, hostClaimContext{id: id, token: token}), release, nil
}
