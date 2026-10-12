package services

import (
	"context"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"path/filepath"
	"sort"
	"strings"
)

func (s *BackupService) restoreStageSources(ctx context.Context, r hostCommandRunner, row *models.PrometheusRestoreTransaction, c *models.Component) (*prometheusRecoveryPayload, *prometheusRecoveryPayload, error) {
	admission, err := decodeRestoreJournal(row)
	if err != nil {
		return nil, nil, err
	}
	checkpoint, err := decodeRestoreCheckpoint(row, admission)
	if err != nil {
		return nil, nil, err
	}
	if err := s.verifyRestoreCheckpointHost(ctx, r, c, checkpoint); err != nil {
		return nil, nil, err
	}
	var plan models.ChangePlan
	if err := s.db.WithContext(ctx).First(&plan, row.PlanID).Error; err != nil {
		return nil, nil, err
	}
	_, historical, _, _, err := s.admitPairedRestorePreview(ctx, &plan, c)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyRestoreStageSet(ctx, r, s.backupRoot, historical); err != nil {
		return nil, nil, err
	}
	return checkpoint.Evidence, historical, nil
}
func verifyRestoreStageSet(ctx context.Context, r hostCommandRunner, root string, p *prometheusRecoveryPayload) error {
	if _, err := verifyUnitSnapshot(ctx, r, root, &p.Startup, p.Binding, false); err != nil {
		return err
	}
	if err := verifyRecoveryPointBinary(ctx, r, p.Binary); err != nil {
		return err
	}
	if err := checkRecoveryPointDirectory(ctx, r, p.Binary.Directory); err != nil {
		return err
	}
	command := "set -eu\np=" + utils.QuoteShell(p.Archive.Path) + "\ntest \"$(readlink -m -- \"$p\")\" = \"$p\"\ntest -f \"$p\" && test ! -L \"$p\"\ntest \"$(stat -c '%a:%u:%g:%h' -- \"$p\")\" = '600:0:0:1'\n"
	if _, err := snapshotRun(ctx, r, command); err != nil {
		return err
	}
	sha, size, err := inspectArtifact(ctx, r, p.Archive.Path, "full", "tar.gz", p.Archive.Sources)
	if err != nil {
		return err
	}
	if sha != p.Archive.SHA256 || size != p.Archive.Size {
		return pointReject("stage full archive integrity failed")
	}
	return nil
}
func derivedRestoreStageSet(root string, p *prometheusRecoveryPayload) *prometheusRecoveryPayload {
	copy := *p
	copy.Startup.Path = filepath.Join(root, filepath.Base(p.Startup.Path))
	copy.Binary.Directory = filepath.Join(root, filepath.Base(p.Binary.Directory))
	copy.Archive.Path = filepath.Join(copy.Binary.Directory, filepath.Base(p.Archive.Path))
	return &copy
}
func restoreStageEntries(ctx context.Context, r hostCommandRunner, root string, names ...string) error {
	if err := snapshotPrivateDirectory(ctx, r, root); err != nil {
		return err
	}
	sort.Strings(names)
	out, err := snapshotRun(ctx, r, "find "+utils.QuoteShell(root)+" -mindepth 1 -maxdepth 1 -printf '%f\\n' | LC_ALL=C sort")
	if err != nil {
		return err
	}
	if out != strings.Join(names, "\n")+"\n" {
		return pointReject("unexpected stage artifact entries")
	}
	return nil
}
func copyRestoreStageArtifacts(ctx context.Context, r hostCommandRunner, f restoreStageFence, current, historical *prometheusRecoveryPayload) error {
	gen := filepath.Join(f.Root, "g-"+f.Generation)
	command := "# promlifecycle:restore-stage-copy\nset -eu\numask 077\nmkdir -m 0700 -- " + utils.QuoteShell(gen) + "\n"
	for i, p := range []*prometheusRecoveryPayload{current, historical} {
		name := []string{"current", "historical"}[i]
		root := filepath.Join(gen, name)
		command += "mkdir -m 0700 -- " + utils.QuoteShell(root) + "\n"
		for _, source := range []string{p.Startup.Path, p.Binary.Directory} {
			command += "cp -a -- " + utils.QuoteShell(source) + " " + utils.QuoteShell(filepath.Join(root, filepath.Base(source))) + "\n"
		}
		command += "find " + utils.QuoteShell(root) + " -type f -exec sync -f -- {} +\nfind " + utils.QuoteShell(root) + " -depth -type d -exec sync -f -- {} +\n"
	}
	command += "sync -f -- " + utils.QuoteShell(gen) + "\nsync -f -- " + utils.QuoteShell(f.Root) + "\n"
	return runRestoreStageFenced(ctx, r, f, command)
}
func (s *BackupService) verifyRestoreStageHost(ctx context.Context, r hostCommandRunner, row *models.PrometheusRestoreTransaction, c *models.Component, p *prometheusRestoreStagePayload) error {
	if p.Fence.BackupRoot != s.backupRoot {
		return pointReject("stage artifact root changed")
	}
	if err := verifyRestoreStageFence(ctx, r, p.Fence); err != nil {
		return err
	}
	current, historical, err := s.restoreStageSources(ctx, r, row, c)
	if err != nil {
		return err
	}
	saved, err := verifyUnitSnapshot(ctx, r, filepath.Dir(current.Startup.Path), &current.Startup, current.Binding, false)
	if err != nil {
		return err
	}
	refs, err := s.checkpointReferences(ctx, r, c, saved.Files)
	if err != nil {
		return err
	}
	gen := filepath.Join(p.Fence.Root, "g-"+p.Fence.Generation)
	if err := restoreStageEntries(ctx, r, gen, "current", "historical"); err != nil {
		return err
	}
	for i, source := range []*prometheusRecoveryPayload{current, historical} {
		root := filepath.Join(gen, []string{"current", "historical"}[i])
		copied := derivedRestoreStageSet(root, source)
		if err := restoreStageEntries(ctx, r, root, filepath.Base(source.Startup.Path), filepath.Base(source.Binary.Directory)); err != nil {
			return err
		}
		if err := verifyRestoreStageSet(ctx, r, root, copied); err != nil {
			return err
		}
	}
	if _, _, err := s.restoreStageSources(ctx, r, row, c); err != nil {
		return err
	}
	if err := s.checkCheckpointReferences(ctx, r, c, saved.Files, refs); err != nil {
		return err
	}
	return verifyRestoreStageFence(ctx, r, p.Fence)
}
