# Paired restore staging implementation plan

Goal: complete the staged-copy and safe host-writer takeover boundary described by the spec.
Architecture: immutable admission/checkpoint plus a separate strict staging journal, permanent host lock and conditional writer generation; actual live file replacement follows in a later step.
Tech: vendored Go1.26.8, SQLite/GORM, existing local/SSH host runners, Bash/coreutils/flock, systemd239 compatibility.
Spec: docs/superpowers/specs/2026-09-15-paired-restore-staging.md

## Global Constraints
Stay in the existing authorized feature checkout. Do not change live Prometheus files or start/reload its service. Preserve every prior delivery and all original history/checkpoint/admission bytes. Run graph impact before editing existing functions and disclose HIGH/CRITICAL. No new dependency, credentials in files/logs, automatic claim stealing, implicit stage retry, skipped acceptance gate or commit before verification.

## Task 1: Permanent host fence and conditional generation
- [ ] Write real-process/file tests first, observe missing behavior, implement private stage fence types and host operations in services/prometheus_restore_stage_fence.go and its test file.
- [ ] Provide initialization, read-only verification, fenced foreground execution and conditional generation rotation. Validate canonical paths, permissions, single link, stable root/lock inode and exact generation; preserve lock FD across children.
- [ ] Verify stale delayed commands, busy child writers, reordered rotations, wrong path/inode/token, initialization failure windows and redacted errors.

## Task 2: Strict staged journal and service
- [ ] Add private nullable model evidence and strict new-phase decoding while preserving old compatibility and prepared-only authority.
- [ ] Implement StagePrometheusPairedRestore and VerifyPrometheusPairedRestoreStage with exact CAS/COMMIT ordering, owning claims, fresh source checks and generation-isolated copies of both full artifact sets.
- [ ] Cover literal bytes/permissions, ordering, reopen/read-only idempotence, ownership drift, sibling claims and all database/host failure boundaries with behavioral tests.

## Task 3: Explicit safe takeover
- [ ] Share the real database-process-lock capability between startup and recovery; enforce its lifetime and exact DB binding inside the recovery service. Implement RecoverPrometheusPairedRestoreStageForStartup using pinned reclaim intent, conditional host generation rotation, atomic exact old-claim retirement AND fresh host/component claim creation, and a fresh isolated copy generation.
- [ ] Add dedicated server recovery command under acquireDatabaseProcessLock; reject unresolved whole-database restore before any host mutation; never accept old unfenced checkpoint phases.
- [ ] Verify actual child exits and reordered/delayed workers, held controller lock, claim replacement, uncertain rotation/COMMIT, initialized missing evidence and maximum generation bounds.

## Task 4: Independent review and real acceptance
- [ ] Review the complete protocol and fix required findings; keep source identity tied to evidence.
- [ ] Extend opt-in 150/151 fixture with distinct current/historical stage copies, reopen and owned isolated TSDB probes; preserve WSL rejection and existing legacy/checkpoint checks. Verify cleanup independently.
- [ ] Run focused tests first, then freeze source and run required full unit/race/vet/build/product/frontend/API/CLI/UBI8 and source+CI graph gates once on that candidate.

## Task 5: Verified commit and immutable delivery
- [ ] Independent final review of exact candidate and complete gates. Preserve any failed attempts as historical receipts.
- [ ] Commit only verified source, confirm committed tree matches frozen candidate and clean worktree.
- [ ] Create a new immutable Windows delivery, SHA256 manifest and native exact-file/hash verification; report this staging boundary honestly and retain live replacement as next work.
