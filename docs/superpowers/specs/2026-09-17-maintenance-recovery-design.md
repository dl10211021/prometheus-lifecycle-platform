# Maintenance and paired recovery design

## Goal and accepted scope

Continue original lifecycle roadmap step 6 after onboarding commit bbc915688aee. Deliver a usable paired Prometheus recovery flow through existing maintenance/change records: preview the historical point, persist intent, stop and capture current state, stage, replace matched startup/binary/config/data, start and verify the selected service, publish success. On a confirmed execution failure compensate to the captured current state; uncertain or failed compensation stays fenced and visibly requires recovery. Existing configuration, upgrade, backup, and retirement capabilities remain available from the same maintenance context; do not rebuild them.

User's continued development authorization covers this next milestone already described in the previous delivery. Preserve the current feature branch and existing data. Use Linux binary/systemd capabilities already admitted by paired preview. No new package dependencies, alternate installer, general job queue, or platform architecture.

## Design choice

Extend the existing PrometheusRestoreTransaction with bounded private execution evidence, preserving immutable admission/checkpoint/stage evidence. Reuse the protected host fence and proven artifact validators. A separate replacement subsystem or calling ordinary backup restore across paired locks would duplicate authority and weaken scope; neither is required. Host mutation is foreground and fenced, and an intent must be committed before live mutation. Never treat a GORM SAVEPOINT as that COMMIT.

Execution starts from a freshly verified restore_staged record, acquires its own host/component claims without adopting surviving old owners, and binds immutable stage revision/digest. Preserve existing stage/checkpoint readers by validating their frozen snapshot under the original phase/revision; new phase validation must independently bind all execution evidence, not mask malformed journals. Old prepared/aborted/checkpoint/staging databases and startup fences retain compatibility. Only valid completed/compensated outcomes release durable active keys; malformed terminal rows cannot disappear from safety checks.

## Live switch and success

Use only verified copied artifacts and explicit trusted component paths. Before mutation validate startup path union, optional absent files, no symlinks/hardlinks/unsafe archive members, mount/path ownership and stopped service. Stage extraction separately from live paths; replace config/data/binary/startup with bounded package-authored scripts under the existing host lock. Preserve original files and all source artifacts for compensation; do not delete other services, unknown paths or historical evidence. Restore modes/UID/GID supported by the existing snapshot contract; reject unsupported roots instead of guessing. Persist the phase before switching, starting or compensating.

Reload systemd after matched startup files are restored; start the exact recorded unit. Verify active/running process, exact executable bytes/version, effective configured paths/argv, and Prometheus readiness at its saved route prefix. No success based solely on process exit or an unrelated HTTP port. Historical restoration updates component metadata and change-plan outcome atomically with terminal journal publication only after verified runtime. Preserve Before/After/history contents; use existing rollback_evidence and audit/operation conventions without exposing paths/tokens/file bodies.

## Failure, interruption, retry

A confirmed error during replacement/start/health triggers the current checkpoint restoration under the same authority: stop any attempted historical runtime, replace the current matched set, start and verify it, then record compensated (restore unsuccessful, previous state recovered). Compensation uses a bounded independent context if the request has cancelled, but must not race an unconfirmed remote writer; unknown transport termination keeps a pending fenced journal. Compensation failure keeps locks and a durable actionable phase, never reports restored or compensated.

Repeated terminal requests return their recorded result without replaying host writes. A repeated in-flight request cannot acquire or steal claims. Startup must continue blocking normal schedulers for pending recovery. Extend the existing exclusive database-process-lock recovery CLI to compensate an interrupted execution using pinned evidence and host generation fencing; do not start a second unfenced server or auto retry an unknown HTTP result. Pre-existing unsupported interrupted checkpoint phases remain explicitly blocked rather than being reinterpreted as safe execution evidence.

## Reachable recovery point capture

Normal upgrades currently never call CreatePrometheusRecoveryPoint. Add an explicit `paired_recovery` option to binary Prometheus upgrade requests and the upgrade form. When selected, capture a ready paired point after the plan enters executing and before any version transition/application; capture failure prevents applying the upgrade. Preserve the existing ordinary automatic-rollback backup and behavior. Use an additive nullable plan association to the paired point so the later ordinary backup does not invalidate paired history; older plans without that association retain the exact existing BackupID linkage requirement. Both associations must be checked against trusted point/backup/plan identity and immutable Before/After hashes. Do not allow arbitrary clients to assign the trusted paired association.

This optional extra capture has additional downtime and disk cost, which the upgrade form explains. Existing API clients without the option preserve their current behavior; no silent fallback after requesting paired capture. Normal verified upgrades with a paired point become selectable for the dedicated recovery UI. Generic rollback continues using its existing ordinary backup, without granting generic mutation permission over pinned paired artifacts.

## Product integration

Add read-only preview/current transaction and explicit execution to the existing change-plan maintenance page for eligible Prometheus binary upgrade plans. API read access follows existing read group; writes require change_admin/system_admin. Return the public transaction even on a recoverable conflict, with phase/outcome rendered clearly. Operators must see the selected plan/component/current and historical versions and the service interruption before executing. Client-side confirmation follows existing destructive-change UX, not an extra development approval gate.

Use explicit refresh to find durable results after timeout/disconnect; never retry POST automatically. Existing generic rollback remains for nonpaired cases; paired history must not fall through to standalone backup restore. Link maintenance actions to the current component's config/upgrade/backup/retirement controls, with existing keep_service / keep_data / full_delete distinctions and default data preservation. Do not redesign unrelated navigation.

## Acceptance

Meaningful local filesystem tests must prove historical bytes are restored and distinct current bytes return after an injected start/health failure. Test durable COMMIT failures, duplicate requests, foreign/lost claims, unsafe or corrupt artifacts, restart recovery and compensation failure fencing. Use existing fixtures, with only realistic systemd/HTTP boundary emulation. Cover API role/access/error contracts and real Chromium success/compensation/pending UI transitions. Run focused existing stage/checkpoint/journal regressions, Go race for changed state transitions, vet/build, and local product closure. Validate an actual isolated systemd/Prometheus fixture on the provided test environment if accessible; never use the existing business 9090/9100 instances for switching.

Report exact tested scope, remaining limits, and no invented project percentage. Preserve failed test/review records and deliver a small evidence package; no evidence-framework expansion.
