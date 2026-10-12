package services

import (
	"github.com/promlifecycle/platform/core/config"
	"gorm.io/gorm"
)

type AlertSyncService struct {
	db       *gorm.DB
	basePath string
}

func NewAlertSyncService(db *gorm.DB, basePath ...string) *AlertSyncService {
	base := "/opt/promlifecycle"
	if len(basePath) > 0 && basePath[0] != "" {
		base = basePath[0]
	}
	return &AlertSyncService{db: db, basePath: base}
}

func (s *AlertSyncService) GetPrometheusEndpoint(clusterID uint) (string, error) {
	return config.NewAutoConfig(s.db, s.basePath).GetPrometheusEndpoint(clusterID)
}

// SyncAlertRules publishes rules and prometheus.yml as one validated snapshot.
func (s *AlertSyncService) SyncAlertRules(clusterID uint) error {
	return config.NewAutoConfig(s.db, s.basePath).SyncPrometheusConfig(clusterID, s.basePath)
}
