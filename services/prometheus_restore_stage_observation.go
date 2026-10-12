package services

import (
	"context"

	"github.com/promlifecycle/platform/models"
)

// A ready observation has no writer authority and never acquires or releases
// operation claims. The active journal keys continue excluding business writers.
// Recovery changes the journal monotonically before rotating the host fence.
func (s *BackupService) observeRestoreStage(ctx context.Context, expected *models.PrometheusRestoreTransaction) (*models.PrometheusRestoreTransaction, error) {
	p, c, claims, err := s.checkRestoreStageObservation(ctx, expected)
	if err != nil {
		return nil, err
	}
	r, release, err := s.pointRunner(ctx, c)
	if err != nil {
		return nil, err
	}
	defer release()
	if err = s.onPointRunner(r).verifyRestoreStageHost(ctx, r, expected, c, p); err != nil {
		return nil, err
	}
	_, _, after, err := s.checkRestoreStageObservation(ctx, expected)
	if err != nil {
		return nil, err
	}
	if claims != after {
		return nil, restoreJournalReject("stage claim state changed during observation")
	}
	return expected, nil
}

// Each query uses the root, nontransactional connection required by the public
// entry. No SQLite snapshot spans host observation. Surviving pinned tokens are
// evidence only; this never installs them in a claim context or adopts authority.
func (s *BackupService) checkRestoreStageObservation(ctx context.Context, expected *models.PrometheusRestoreTransaction) (*prometheusRestoreStagePayload, *models.Component, restoreStageClaims, error) {
	if expected.Phase != "restore_staged" {
		return nil, nil, restoreStageClaims{}, restoreJournalReject("observation requires ready stage")
	}
	p, c, claims, err := s.reclaimStageOwner(ctx, s.db, expected)
	if err != nil {
		return nil, nil, claims, err
	}
	// Recheck after the identity/claim queries, so a takeover beginning during
	// those reads cannot be accepted on the earlier row observation.
	var current models.PrometheusRestoreTransaction
	if err = s.db.WithContext(ctx).First(&current, expected.ID).Error; err != nil {
		return nil, nil, claims, err
	}
	if _, err := decodeRestoreJournal(&current); err != nil {
		return nil, nil, claims, err
	}
	if !sameRestoreStageRow(&current, expected) || !current.CreatedAt.Equal(expected.CreatedAt) || !current.UpdatedAt.Equal(expected.UpdatedAt) {
		return nil, nil, claims, restoreJournalReject("stage observation journal changed")
	}
	return p, c, claims, nil
}
