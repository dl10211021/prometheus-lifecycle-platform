package services

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"io"
	"path/filepath"
	"regexp"
	"time"
)

const restoreStageMaxPayload = 256 << 10
const restoreStageMaxGenerations = 32

var restoreStageClaimToken = regexp.MustCompile("^[0-9a-f]{48}$")

type restoreStageClaims struct{ Host, Component string }
type restoreStageReclaim struct {
	FromPhase             string
	Desired               string
	Captured, Replacement restoreStageClaims
	StartedAt             time.Time
}
type prometheusRestoreStagePayload struct {
	Schema                            int
	TransactionID                     uint
	AdmissionSHA256, CheckpointSHA256 string
	Fence                             restoreStageFence
	Generations                       []string
	Claims                            restoreStageClaims
	StartedAt, CompletedAt            time.Time
	Reclaim                           *restoreStageReclaim
}

func stagePhase(phase string) bool {
	return phase == "restore_stage_intent" || phase == "restore_stage_copying" || phase == "restore_stage_reclaim" || phase == "restore_staged"
}
func restoreStageRoot(root string, row *models.PrometheusRestoreTransaction) string {
	return filepath.Join(root, fmt.Sprintf("restore-stage-%d-%s", row.ID, row.Token[:32]))
}
func randomRestoreStageToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", restoreJournalReject("cannot allocate stage identity")
	}
	return hex.EncodeToString(raw), nil
}
func validRestoreStageClaims(c restoreStageClaims) bool {
	return restoreStageClaimToken.MatchString(c.Host) && restoreStageClaimToken.MatchString(c.Component)
}
func decodeRestoreStage(row *models.PrometheusRestoreTransaction) (*prometheusRestoreStagePayload, error) {
	reject := func() (*prometheusRestoreStagePayload, error) {
		return nil, restoreJournalReject("invalid stage evidence")
	}
	if !stagePhase(row.Phase) {
		if len(row.Stage) != 0 || row.StageSHA256 != "" {
			return reject()
		}
		return nil, nil
	}
	if len(row.Stage) == 0 || len(row.Stage) > restoreStageMaxPayload || !unitSnapshotHash.MatchString(row.StageSHA256) || pointDigest(row.Stage) != row.StageSHA256 {
		return reject()
	}
	var p prometheusRestoreStagePayload
	dec := json.NewDecoder(bytes.NewReader(row.Stage))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return reject()
	}
	f := p.Fence
	if p.Schema != 1 || p.TransactionID != row.ID || p.AdmissionSHA256 != row.PayloadSHA256 || p.CheckpointSHA256 != row.CheckpointSHA256 ||
		!validUnitSnapshotPath(f.BackupRoot) || !validUnitSnapshotPath(f.Root) || f.Root != restoreStageRoot(f.BackupRoot, row) ||
		p.StartedAt.IsZero() || !validRestoreStageClaims(p.Claims) || len(p.Generations) == 0 || len(p.Generations) > restoreStageMaxGenerations {
		return reject()
	}
	seen := map[string]bool{}
	for _, g := range p.Generations {
		if !unitSnapshotHash.MatchString(g) || seen[g] {
			return reject()
		}
		seen[g] = true
	}
	if f.Generation != p.Generations[len(p.Generations)-1] {
		return reject()
	}
	pinned := f.RootIdentity != "" || f.LockIdentity != ""
	if pinned && (!restoreStageFenceIdentity.MatchString(f.RootIdentity) || !restoreStageFenceIdentity.MatchString(f.LockIdentity)) {
		return reject()
	}
	if row.Phase == "restore_stage_intent" && (pinned || len(p.Generations) != 1 || row.Revision != 5) {
		return reject()
	}
	if (row.Phase == "restore_stage_copying" || row.Phase == "restore_staged") && !pinned {
		return reject()
	}
	if row.Phase == "restore_staged" {
		if p.CompletedAt.IsZero() || p.CompletedAt.Before(p.StartedAt) {
			return reject()
		}
	} else if !p.CompletedAt.IsZero() {
		return reject()
	}
	if row.Phase == "restore_stage_reclaim" {
		r := p.Reclaim
		if r == nil || (r.FromPhase != "restore_stage_intent" && r.FromPhase != "restore_stage_copying" && r.FromPhase != "restore_staged") || (r.FromPhase != "restore_stage_intent" && !pinned) || (r.FromPhase == "restore_stage_intent" && (pinned || len(p.Generations) != 1)) || len(p.Generations) >= restoreStageMaxGenerations || !unitSnapshotHash.MatchString(r.Desired) || seen[r.Desired] || !validRestoreStageClaims(r.Replacement) || r.StartedAt.IsZero() || r.StartedAt.Before(p.StartedAt) ||
			r.Captured.Host != "" && r.Captured.Host != p.Claims.Host || r.Captured.Component != "" && r.Captured.Component != p.Claims.Component ||
			r.Replacement.Host == p.Claims.Host || r.Replacement.Component == p.Claims.Component || !pinned && len(p.Generations) != 1 {
			return reject()
		}
	} else if p.Reclaim != nil {
		return reject()
	}
	if !validRestoreStageRevision(row, &p) {
		return reject()
	}
	return &p, nil
}

// A generation is published by exactly one copying transition. A takeover adds
// reclaim+copying, and can additionally follow a completed prior generation.
// The first pre-initialization takeover saves the initial copying transition.
func validRestoreStageRevision(row *models.PrometheusRestoreTransaction, p *prometheusRestoreStagePayload) bool {
	n := uint64(len(p.Generations))
	if row.Phase == "restore_stage_intent" {
		return n == 1 && row.Revision == 5
	}
	low, high := 2*n+3, 3*n+3
	if n == 1 {
		low, high = 6, 6
	}
	switch row.Phase {
	case "restore_stage_copying":
	case "restore_staged":
		low++
		high++
	case "restore_stage_reclaim":
		if p.Reclaim == nil {
			return false
		}
		switch p.Reclaim.FromPhase {
		case "restore_stage_intent":
			return n == 1 && row.Revision == 6
		case "restore_stage_copying":
			low++
			high++
		case "restore_staged":
			low += 2
			high += 2
		default:
			return false
		}
	default:
		return false
	}
	return row.Revision >= low && row.Revision <= high
}
