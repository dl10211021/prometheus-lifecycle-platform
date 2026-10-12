package services

import (
	"context"
	"errors"
	coressh "github.com/promlifecycle/platform/core/ssh"
)

type unitSnapshotSSHRunner struct{ client *coressh.Client }

func (r unitSnapshotSSHRunner) Run(ctx context.Context, command string) (string, error) {
	out, _, err := r.client.RunCommandContext(ctx, command)
	return out, err
}

// A snapshot probes several files repeatedly. Reuse one authenticated transport
// per claimed operation instead of dialing for every stat/hash/copy command.
// Custom runners and the local runner retain their existing behavior.
func acquireUnitSnapshotRunner(ctx context.Context, r hostCommandRunner) (hostCommandRunner, func(), error) {
	remote, ok := r.(remoteBackupRunner)
	if !ok {
		return r, func() {}, nil
	}
	if remote.component == nil {
		return nil, nil, snapshotReject("remote component required")
	}
	h := remote.component.Host
	client, err := coressh.NewClientContext(ctx, h.IP, h.Port, h.Username, h.Password, h.PrivateKey)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, errors.Join(ErrUnitSnapshot, ctx.Err())
		}
		return nil, nil, snapshotReject("SSH connection failed")
	}
	return unitSnapshotSSHRunner{client: client}, func() { _ = client.Close() }, nil
}
