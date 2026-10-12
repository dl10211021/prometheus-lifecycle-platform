# Paired restore staging and host writer fencing

Step 10.2.2.4 continues verified commit 84e2a9aa1ec541a0993a3cf46b2bd072d5803944 in the existing feature checkout.

## Goal and boundary
Prepare independently verified current and historical binary/startup/archive copies under a durable transaction-owned staging root while the original Prometheus stays stopped. Establish host writer fencing and an explicit recovery entry before a later step may replace live paths. This is an architectural protocol extension within the user's authorized stepwise development.

Success means restore_staged with both verified sets, the original stopped baseline unchanged, immutable historical/checkpoint/admission evidence, and active durable host/component keys. It does not claim live replacement, compensation of live files, service resumption, health verification or a complete recovery product. Those depend on this prerequisite.

Direct generic restore was rejected because it resumes service and lacks paired phase evidence. A monolithic live-replacement change was rejected as a first step because the existing controller-crash path retains claims and cannot yet invalidate a surviving remote writer. The selected staged protocol exercises real protected host writes and takeover before introducing live mutations.

## Public internal service contracts
StagePrometheusPairedRestore(ctx, transactionID) and VerifyPrometheusPairedRestoreStage(ctx, transactionID) return the persisted PrometheusRestoreTransaction and a redacted error. Stage accepts checkpoint_ready; restore_staged repeats fresh read-only verification. Other phases require explicit recovery, never implicit retry.

RecoverPrometheusPairedRestoreStageForStartup(ctx, transactionID, ownership) requires a storage/sqlite.DatabaseProcessLock capability created by AcquireDatabaseProcessLock. Its private file handle and mutex retain the actual exclusive OS lock for the complete recovery callback; concurrent Close waits, wrong/closed/nil capabilities reject, and the service compares the actual main database filename from SQLite against the canonical locked path. Server startup and recovery share this lock implementation. A dedicated server command --recover-prometheus-restore-stage=<id> acquires that lock before opening the existing DB and invoking it; --restore-backup-root supplies the artifact root, defaulting to the existing /backups. The command does not start HTTP, migrate business metadata or apply a database restore. It rejects an unresolved whole-database restore state and missing/corrupt DB. Unsupported OS lock implementations reject.

## Journal and authority
Add nullable JSON-hidden Stage and StageSHA256 columns. Old rows require them empty. New phases are restore_stage_intent, restore_stage_copying, restore_stage_reclaim and restore_staged. Admission and current checkpoint bytes/digests remain immutable; new phases require the complete checkpoint shape.

Each transition is one exact-row SQLite CAS with COMMIT checked before the next host write. Bind row/token/associations/phase/revision and all evidence digests, trusted history/current identities, old pause/version barriers and own claims. Keep active keys and FinishedAt=nil. Structural decoding has no host reads and retains valid partial rows during authoritative DB rollback.

Stage evidence is bounded to 256 KiB and at most 32 generations. It binds transaction ID, admission/checkpoint digests, deterministic protected staging root, current writer token, generation, start/completion times and host root/lock inode identities once published. Phase-specific shape rejects premature completion, missing initialized identity, malformed token/path and incompatible reclaim evidence. Revisions increment exactly under CAS, are bounded, and cannot wrap.

## Durable initialization and host fencing
Before any initialization write, commit restore_stage_intent with root and random writer token. Use a separate root derived from ID/token under the configured backup root; never add entries to the immutable checkpoint root. Initialize only protected canonical root-owned directories/files. The permanent lock file is root-owned 0600, ordinary, single-link and never unlinked/recreated. Pin its device:inode and its root's identity.

Every stage command acquires the same nonblocking flock, rechecks FD/path identities and the current generation after acquiring the lock, and keeps the lock through all child writes and fsync. Reject stale generations, wrong inode/path/ownership, links, incomplete frames and busy locks. Only bounded foreground copy/sync work is run. No arbitrary caller-supplied host command is exposed publicly.

Commit restore_stage_copying with verified host identities before copying. Copy current and historical startup directory, binary and full archive into distinct generation-owned directories. Reopen copies against the already pinned source hashes/bindings, verify complete expected directory entries, and verify the original stopped baseline and reference inventory again. Do not derive trust by hashing an unreferenced partial copy. A copy failure retains all forensic files and the last durable phase.

## Explicit interrupted-stage recovery
Only these new fenced staging phases can be reclaimed. Old checkpoint commands did not acquire this host fence and must never be reclaimed by this protocol.

Under exclusive DB process ownership, validate all row/trusted identity and exact prior host/component claim tokens before committing a reclaim intent. Reclaim records expected old and desired new writer generation and exact claims to retire. Rotate the host generation while holding the permanent lock: expected-old -> desired-new only; already desired is idempotent verification; every other value rejects. Synchronize generation file and parent before acknowledging. Initial missing-state repair is permitted only for a pre-initialization intent; after initialized identities were durably pinned, missing root/lock/generation rejects.

The old writer has either finished before rotation or will reject its stale token after rotation. A delayed older rotation cannot overwrite a newer generation. Only after confirmed host fencing may one DB transaction retire the exact captured abandoned claims, create BOTH fresh host/component claims, pin their new tokens in stage evidence, and publish the next copying phase. There is no unclaimed handoff gap. Each normal stage intent also pins its actual held claim tokens. Recovery accepts absent claims or a surviving subset of those exact pinned tokens after interrupted release; a replaced token, new same-host sibling claim, zero-row or COMMIT failure rejects/rolls back. Construct the fresh private claim context only after COMMIT, with cleanup bound to the non-transactional DB and exact tokens. Each successful takeover uses a new isolated generation directory and preserves prior partial directories. Finish staging from freshly verified immutable sources.

Crash after host generation switch but before DB publication resumes the same pinned rotation intent. Crash after staged COMMIT but before claim release is recoverable through an explicit new generation. Generation bounds reject further work without modifying evidence.

## Verification and tests
Ready verification is read-only on the host, including no lock file creation, sync, repair, cleanup, stop/start/reload. It reopens both sets, checks generation and inode identity, original stopped baseline and DB ownership before/after. Lost/corrupt artifacts or ordinary drift reject. Public output contains no raw argv, environment contents, command/SQL output, credentials or private tokens.

Use real file-backed SQLite and real filesystem copies. Tests cover distinct historical/current contents, exact commit-before-init/copy/rotation/claim-retirement order; cancellations and command errors; statement/zero-row/deferred COMMIT failures; root/lock/links/generation corruption; concurrent stale commands and reordered rotations; inherited child locks; controller exits at durable boundaries; old/new schema, whole-DB fences, normal lifecycle fences and ready idempotence. All clone copies and original/historical artifacts are checked independently.

Run authorized 150/151 exclusive real Prometheus fixtures plus WSL unsupported reference boundary, retained legacy/checkpoint modes, UBI8/runtime/GLIBC checks, full unit/race/vet/build/46 business closures/frontend/API/official CLI gates, full source+vendor+CI graph coverage and changed-entry review. Freeze source, independently review, commit only verified changes, deliver immutable source/evidence and verify every Windows payload digest. No dependency addition, push or merge.

## Initialization state matrix
Before root/lock identities have been published in the DB, the committed transaction root reservation authorizes only fence bootstrap, never artifact adoption:
- Missing root: create exclusively; a racing creator must converge on the same protected root and one lock inode. No root/lock deletion or replacement.
- Existing protected root, missing lock and generation: create the lock once without truncation, then acquire it. Recheck path/FD identities after acquisition.
- Existing protected root/lock, missing generation: only a pre-pin intent may conditionally initialize while locked; every later initialized phase rejects missing generation.
- Existing generation matching the pinned initial/expected writer: initialize or rotate conditionally under the same lock. Already desired may be durably confirmed. Every unrelated value rejects.
- Generation present with no lock, unsupported entries, links, unsafe metadata or any pinned identity mismatch: reject and preserve evidence.
A delayed initializer checks absence/expected generation under the lock and cannot reset a generation already rotated by recovery. Generation publication uses a new exclusive temporary file, fsync, atomic rename under the permanent lock, and directory fsync. Partial temp files remain forensic evidence, never trusted artifact payloads. Test exits before and after every creation/publication boundary.

## Controller ownership capability
Share canonical path and OS flock acquisition in storage/sqlite; keep the existing server helper as a thin wrapper so startup and recovery cannot use different lock names. DatabaseProcessLock owns a private file, canonical path, closed state and mutex. Its callback method verifies regular unchanged lock inode and actual exclusivity while keeping Close serialized for the whole operation. An existing database is required for recovery. No public raw lock-file handle or boolean permission flag authorizes recovery.

## Reclaim retry and delayed exact-token release
The absent/surviving-subset rule also applies when resuming a committed reclaim intent: a captured old claim may already have disappeared through its old exact-token cleanup. The handoff transaction freshly validates that every surviving host/component claim is one of the captured expected tokens and that no sibling exists, then deletes only those exact present rows and atomically inserts both replacement claims plus the copying CAS. A missing previously captured row is not authority to delete a replacement. A zero-row deletion for a row expected present in that transaction, zero-row journal CAS, or COMMIT failure still rejects/rolls back. Old cleanup remains bound to the old tokens and cannot remove new claims. Tests cover delayed release before and after publication.
