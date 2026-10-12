# Project lifecycle acceptance on 150/151

The binding product goal is the existing lifecycle roadmap: simple, complete Prometheus management using one platform process, SQLite and Linux binary/systemd. This milestone validates the public workflow on the two authorized test machines, without broad new product features.

Use the committed a92ddcc0aa6833ca890bc173c19fe3a84941a9d0 as baseline. Run a separate local platform database/base/backups, high noncolliding host/component IDs, unique fixture directories and unused ports. Never mutate existing business 150:9090 or 150/151:9100, their configs/data/units. Read-only discovery is allowed; write/adoption/removal applies only to proven owned fixtures. Keep credentials in environment/runtime storage excluded from reports.

Acceptance outcomes:
1. Public preflight plus deployment creates a real2.55.0 Prometheus on150 and1.8.2 node_exporter on151; both exact systemd instances become ready, and Prometheus returns real samples from the selected exporter.
2. Valid configuration publication is reflected remotely; configuration backup, mutation and restore return exact baseline content. Invalid configuration is rejected without replacing a healthy runtime. Viewer writes are denied.
3. Public change-plan creation/execution with after.paired_recovery=true upgrades2.55.0 to3.1.0. Independent runtime version and readiness agree with plan/component status. Both paired association and ordinary backup exist.
4. Capture a fixed historical sample before upgrade and a distinct current sample after upgrade, using an owned mutable exporter textfile metric. Dedicated paired restore returns runtime2.55.0, completed durable transaction, preserved historical sample and absence of the post-capture series at its fixed pre-restore timestamp. Never mistake ordinary binary downgrade for paired data restore.
5. Owned retirement covers keep_service (running service/data retained), observation/adoption of that exact retained fixture without touching business units, keep_data (service removed, recorded data retained), and full_delete on a separate owned fixture (exact resources removed). Verify configuration/target cleanup against the intended Prometheus instance when applicable.
6. Real HTTP/browser state is checked against actual API outcomes. Existing mock browser coverage remains separate evidence. Prior isolated compensation acceptance can be cited as unchanged-source evidence; do not invent a new injected-failure result.

All failures retain their logs. Correct real blockers with focused tests and review; unrelated improvements remain deferred. No credential bypass, disabled production safeguards or fabricated successful state is allowed. Before source edits follow AGENTS.md upstream impact; before commit refresh configured source/vendor/tests/frontend plusCI/group and raw nonpartial/nontruncated detect_changes. Controller owns final review, commit, evidence archive and Windows receipt.
