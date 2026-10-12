package scheduler

import "github.com/promlifecycle/platform/core/config"

// provisionGrafanaDashboards uses file provisioning only. API fallback with
// guessed credentials caused repeated failed logins and account lockouts.
func (s *Scheduler) provisionGrafanaDashboards(clusterID uint) error {
	base := s.basePath
	if base == "" {
		base = "/opt/promlifecycle"
	}
	return config.NewAutoConfig(s.db, base).ProvisionGrafanaDashboards(clusterID, base)
}
