# Paired Prometheus restore staging

This step prepares verified current and historical copies while the original Prometheus remains stopped. It does not replace live files, compensate live changes, start/reload the service, or verify application health.

The internal staging service accepts a transaction at `checkpoint_ready`. It persists `restore_stage_intent` before host initialization, pins the permanent host lock/root identities in `restore_stage_copying`, and publishes `restore_staged` only after both complete startup, executable, and full archive copies pass verification. Repeating stage/verify on `restore_staged` observes the journal, claims, stopped baseline and artifacts without acquiring or releasing operation claims or changing journal evidence/timestamps. The active journal keys continue excluding business writers. A changed row, claim state or host generation rejects the observation; only explicit recovery can create a new staging generation. Surviving original pinned claims are checked as evidence and are never adopted as writer authority.

## Explicit interrupted-stage recovery

Stop the platform server first, then run its server executable against the existing data directory:

```sh
./server --data-dir=/var/lib/promlifecycle \
  --recover-prometheus-restore-stage=123 \
  --restore-backup-root=/backups
```

Replace `123` with the persisted transaction ID and use the original configured artifact root. `--restore-backup-root` defaults to `/backups`. The database is `<data-dir>/promlifecycle.db`.

The command acquires the same exclusive canonical database process lock used by normal startup. It opens an existing database without business migrations, database-restore application, HTTP, or schedulers. Linux flock is required; unsupported platforms reject ownership.

Recovery accepts only `restore_stage_intent`, `restore_stage_copying`, `restore_stage_reclaim`, and `restore_staged`. Old checkpoint phases cannot be reclaimed because their writers did not use this host fence. Each successful takeover creates a new generation under the same protected staging root, preserving previous partial generations. A pinned reclaim intent resumes its exact conditional old-to-new host generation rotation. Both durable host/component operation claims are replaced atomically after confirmed host fencing.

The command refuses a held database lock, missing/corrupt/wrong database, hard-link aliases, unresolved or malformed whole-database restore state, replaced/sibling claims, changed trusted history or stopped baseline, missing initialized fence evidence, busy/stale host generations, corrupt artifacts, and the 32-generation limit. Failures retain the last committed journal and forensic files; they do not grant permission to replace live paths.

Success prints the public transaction with phase `restore_staged`; private paths, tokens, payloads and command output are omitted. Admission/checkpoint/history and original live files remain unchanged. Durable active host/component journal keys remain held until a later protocol step resolves the restore. The subsequent [paired recovery execution workflow](paired-prometheus-recovery-execution.md) now performs live replacement, verified service resumption and failure compensation; staging alone still authorizes none of those outcomes.
