package services

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// The caller commits this exact root before any host-side write. Exclusive
// mkdir never adopts or removes a prior attempt's forensic artifacts.
func createRestoreCheckpointRoot(ctx context.Context, r hostCommandRunner, root string) error {
	parent := filepath.Dir(root)
	command := "# promlifecycle:restore-checkpoint-root\nset -eu\numask 077\nroot=" + utils.QuoteShell(root) + "\nparent=" + utils.QuoteShell(parent) + `
test "$(id -u)" = 0
test "$(readlink -m -- "$parent")" = "$parent"
test -d "$parent" && test ! -L "$parent"
test "$(stat -c %u -- "$parent")" = 0
test -z "$(find "$parent" -maxdepth 0 -perm /022 -print)"
test "$(readlink -m -- "$root")" = "$root"
mkdir -m 0700 -- "$root"
chown 0:0 -- "$root"
sync -f "$root"
sync -f "$parent"
`
	if _, err := snapshotRun(ctx, r, command); err != nil {
		return err
	}
	return snapshotPrivateDirectory(ctx, r, root)
}

type restoreCheckpointReferences struct {
	Registered, Units []pairedRestoreUnitReference
}

func (s *BackupService) checkpointReferences(ctx context.Context, r hostCommandRunner, c *models.Component, files []unitSnapshotFile) (*restoreCheckpointReferences, error) {
	registered, err := s.pairedRestoreRegisteredReferences(ctx, c)
	if err != nil {
		return nil, err
	}
	registered, err = resolvePairedRestoreUnitReferences(ctx, r, registered)
	if err != nil {
		return nil, err
	}
	refs, err := readPairedRestoreUnitReferences(ctx, r)
	if err != nil {
		return nil, err
	}
	paths := []string{s.backupRoot, c.BinaryPath, c.ConfigPath, c.DataPath}
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	for i, path := range paths {
		if !validUnitSnapshotPath(path) {
			return nil, pointReject("invalid checkpoint protected path")
		}
		for _, previous := range paths[:i] {
			if recoveryPathsOverlap(path, previous) {
				return nil, pointReject("overlapping checkpoint protected paths")
			}
		}
	}
	if err := rejectPairedRestoreSharedIdentity(makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)).Unit, refs, registered); err != nil {
		return nil, err
	}
	if err := checkPairedRestoreOwnReferences(c, files, refs); err != nil {
		return nil, err
	}
	if err := rejectPairedRestoreSharedPaths(paths, registered, ""); err != nil {
		return nil, err
	}
	if err := rejectPairedRestoreSharedPaths(paths, refs, makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)).Unit); err != nil {
		return nil, err
	}
	return &restoreCheckpointReferences{Registered: registered, Units: refs}, nil
}
func (s *BackupService) checkCheckpointReferences(ctx context.Context, r hostCommandRunner, c *models.Component, files []unitSnapshotFile, expected *restoreCheckpointReferences) error {
	current, err := s.checkpointReferences(ctx, r, c, files)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return pointReject("checkpoint service references changed")
	}
	return nil
}
func checkpointRunningBaseline(ctx context.Context, r hostCommandRunner, c *models.Component, p *prometheusRecoveryPayload) error {
	state, err := compareRecoveryPointBaseline(ctx, r, c, p, false)
	if err != nil {
		return err
	}
	runtime, err := readRecoveryPointRuntime(ctx, r, c, state, p.Binary.File)
	if err != nil {
		return err
	}
	if runtime != p.Runtime {
		return pointReject("original process identity changed before checkpoint stop")
	}
	return nil
}
func (s *BackupService) capturePairedRestoreCheckpointHost(ctx context.Context, r hostCommandRunner, row *models.PrometheusRestoreTransaction, admission *prometheusRestoreJournalPayload, c *models.Component) (*models.PrometheusRestoreTransaction, error) {
	// A prepared record is not permission to reuse an old host observation.
	if _, err := s.PreviewPrometheusRecoveryPointRestore(ctx, row.PlanID); err != nil {
		return nil, err
	}
	if !validUnitSnapshotPath(s.backupRoot) {
		return nil, pointReject("invalid checkpoint artifact root")
	}
	state, files, err := snapshotInventory(ctx, r, admission.Current.Unit)
	if err != nil {
		return nil, err
	}
	binary, err := inspectBoundedSnapshotFile(ctx, r, unitSnapshotFile{Path: c.BinaryPath, Role: "binary"}, recoveryPointMaxBinary)
	if err != nil {
		return nil, err
	}
	runtime, err := readRecoveryPointRuntime(ctx, r, c, state, binary)
	if err != nil {
		return nil, err
	}
	refs, err := s.checkpointReferences(ctx, r, c, files)
	if err != nil {
		return nil, err
	}
	proof := &prometheusRestoreCheckpointPayload{Schema: 1, TransactionID: row.ID, AdmissionSHA256: row.PayloadSHA256, Root: restoreCheckpointRoot(s.backupRoot, row), StartedAt: s.now().UTC()}
	row, err = s.advanceRestoreCheckpoint(ctx, row, "checkpointing", proof)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := s.checkpointOwner(ctx, row); err != nil {
		return nil, err
	}
	if err := createRestoreCheckpointRoot(ctx, r, proof.Root); err != nil {
		return nil, err
	}
	startup, err := captureUnitSnapshot(ctx, r, proof.Root, admission.Current)
	if err != nil {
		return nil, err
	}
	dir, err := createRecoveryPointDirectory(ctx, r, proof.Root, c.ID)
	if err != nil {
		return nil, err
	}
	if err := captureRecoveryPointBinary(ctx, r, dir, binary); err != nil {
		return nil, err
	}
	p := &prometheusRecoveryPayload{Schema: 1, BeforeSHA256: admission.BeforeSHA256, AfterSHA256: admission.AfterSHA256, Binding: admission.Current, Startup: *startup, Binary: recoveryPointBinary{Directory: dir, File: binary}, Runtime: runtime}
	saved, err := verifyUnitSnapshot(ctx, r, proof.Root, startup, admission.Current, false)
	if err != nil {
		return nil, err
	}
	copied := append([]unitSnapshotFile(nil), saved.Files...)
	for i := range copied {
		copied[i].Payload = ""
	}
	if !reflect.DeepEqual(state, saved.State) || !reflect.DeepEqual(files, copied) {
		return nil, pointReject("startup observation changed while checkpointing")
	}
	if err := checkpointRunningBaseline(ctx, r, c, p); err != nil {
		return nil, err
	}
	if err := s.checkCheckpointReferences(ctx, r, c, files, refs); err != nil {
		return nil, err
	}
	proof.Evidence = p
	row, err = s.advanceRestoreCheckpoint(ctx, row, "checkpoint_stopping", proof)
	if err != nil {
		return nil, err
	}
	if err := checkpointRunningBaseline(ctx, r, c, p); err != nil {
		return nil, err
	}
	if err := s.checkCheckpointReferences(ctx, r, c, files, refs); err != nil {
		return nil, err
	}
	if _, _, _, err := s.checkpointOwner(ctx, row); err != nil {
		return nil, err
	}
	if err := s.stopComponent(ctx, c); err != nil {
		return nil, err
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, p, true); err != nil {
		return nil, err
	}
	if err := s.verifyPrometheusResolvedRoots(ctx, c); err != nil {
		return nil, err
	}
	if err := s.checkCheckpointReferences(ctx, r, c, files, refs); err != nil {
		return nil, err
	}
	if _, _, _, err := s.checkpointOwner(ctx, row); err != nil {
		return nil, err
	}
	sources, err := backupSourcePaths(c, "full")
	if err != nil {
		return nil, err
	}
	archive := filepath.Join(dir, "payload.tar.gz")
	if _, err := s.createArchiveArtifact(ctx, r, archive, sources, "full"); err != nil {
		return nil, err
	}
	if _, err := snapshotRun(ctx, r, "sync -f "+utils.QuoteShell(archive)+" && sync -f "+utils.QuoteShell(dir)+" && sync -f "+utils.QuoteShell(proof.Root)); err != nil {
		return nil, err
	}
	sha, size, err := inspectArtifact(ctx, r, archive, "full", "tar.gz", sources)
	if err != nil {
		return nil, err
	}
	p.Archive = recoveryPointArchive{Path: archive, SHA256: sha, Version: c.Version, Size: size, Sources: sources}
	proof.StoppedAt = s.now().UTC()
	if err := s.verifyRestoreCheckpointHost(ctx, r, c, proof); err != nil {
		return nil, err
	}
	if err := s.checkCheckpointReferences(ctx, r, c, files, refs); err != nil {
		return nil, err
	}
	return s.advanceRestoreCheckpoint(ctx, row, "checkpoint_ready", proof)
}

func (s *BackupService) verifyRestoreCheckpointHost(ctx context.Context, r hostCommandRunner, c *models.Component, proof *prometheusRestoreCheckpointPayload) error {
	p := proof.Evidence
	sources, err := backupSourcePaths(c, "full")
	if err != nil {
		return err
	}
	if filepath.Dir(proof.Root) != s.backupRoot || p == nil || p.Binding != makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)) || p.Binary.File.Path != c.BinaryPath || p.Archive.Version != c.Version || !equalStringSlices(p.Archive.Sources, sources) {
		return pointReject("checkpoint no longer binds current component or artifact root")
	}
	if err := snapshotPrivateDirectory(ctx, r, proof.Root); err != nil {
		return err
	}
	names := []string{filepath.Base(p.Startup.Path), filepath.Base(p.Binary.Directory)}
	sort.Strings(names)
	entries, err := snapshotRun(ctx, r, "find "+utils.QuoteShell(proof.Root)+" -mindepth 1 -maxdepth 1 -printf '%f\\n' | LC_ALL=C sort")
	if err != nil {
		return err
	}
	if entries != strings.Join(names, "\n")+"\n" {
		return pointReject("unexpected checkpoint root entries")
	}
	saved, err := verifyUnitSnapshot(ctx, r, proof.Root, &p.Startup, p.Binding, false)
	if err != nil {
		return err
	}
	refs, err := s.checkpointReferences(ctx, r, c, saved.Files)
	if err != nil {
		return err
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, p, true); err != nil {
		return err
	}
	if err := s.verifyPrometheusResolvedRoots(ctx, c); err != nil {
		return err
	}
	if err := verifyRecoveryPointBinary(ctx, r, p.Binary); err != nil {
		return err
	}
	if err := checkRecoveryPointDirectory(ctx, r, p.Binary.Directory); err != nil {
		return err
	}
	command := "set -eu\np=" + utils.QuoteShell(p.Archive.Path) + `
test "$(readlink -m -- "$p")" = "$p"
test -f "$p" && test ! -L "$p"
test "$(stat -c '%a:%u:%g:%h' -- "$p")" = '600:0:0:1'
`
	if _, err := snapshotRun(ctx, r, command); err != nil {
		return err
	}
	sha, size, err := inspectArtifact(ctx, r, p.Archive.Path, "full", "tar.gz", p.Archive.Sources)
	if err != nil {
		return err
	}
	if sha != p.Archive.SHA256 || size != p.Archive.Size {
		return pointReject("checkpoint archive failed integrity")
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, p, true); err != nil {
		return err
	}
	if err := s.verifyPrometheusResolvedRoots(ctx, c); err != nil {
		return err
	}
	return s.checkCheckpointReferences(ctx, r, c, saved.Files, refs)
}
