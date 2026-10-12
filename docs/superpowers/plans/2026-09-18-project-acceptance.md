# Project lifecycle acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Prove the complete public Prometheus lifecycle on isolated150/151 resources and correct only demonstrated blockers.
**Architecture:** An API-driven Python acceptance runner talks to a separate platform process and independently checks systemd/files/Prometheus through SSH. The controller owns platform bootstrap, noncolliding identity allocation and initial/final business baselines.
**Tech Stack:** Existing Go1.26.8 vendor build, Python/requests/Paramiko, installed Chromium/Playwright, RHEL8.5/systemd239, pinned offline packages.
**Spec:** docs/superpowers/specs/2026-09-18-project-acceptance-design.md

## Global Constraints

- Use a92ddcc0aa6833ca890bc173c19fe3a84941a9d0 as baseline; preserve existing feature branch and reviewed history.
- Never mutate existing business150:9090 or150/151:9100, their configs/data/units. Write/adoption/removal applies only to proven owned fixtures.
- Credentials are environment/runtime inputs; never include them or auth headers in source, reports or command logs.
- No installs, dependency changes, broad new product features, weakened checks, repeated automatic mutation after uncertain responses, or fabricated PASS/SKIP.
- AGENTS.md graph/impact/detect and complete configured indexing requirements remain mandatory for code changes.

### Task 1: One complete real lifecycle runner and acceptance

**Files:** Create test-scripts/project-lifecycle-acceptance.py. Controller creates runtime bootstrap/evidence under `<external-acceptance-evidence-directory>`. Product source changes are scoped separately only if a real blocker is demonstrated.
**Interfaces:** Controller provides an environment file outside the repo with BASE, PROMLIFECYCLE_ADMIN_PASSWORD, REMOTE_PASS and FIXTURE_MANIFEST. The manifest is nonsecret JSON containing run_id, fixture_root, host/component ID range, selected ports, authorized host addresses and package versions. Runner authenticates afresh; no token is written to evidence. It writes result.json and stage logs under its designated output directory.

- [ ] Read spec and current graph/source contracts for preflight, change plans, configuration, backup, paired recovery and retirement; record pre-edit impact for touched existing symbols.
- [ ] Implement one fail-closed runner using public API for lifecycle writes. Central request logging omits auth/host credentials, records method/path/status and safe result identifiers. Before any remote mutation assert selected IDs/units/paths/ports belong to the fixture and were absent in the baseline.
- [ ] Use the real upgrade contract (maintenance UI path):
```python
payload = {"kind": "upgrade", "resource_type": "component", "resource_id": prometheus_id,
           "reason": "isolated project acceptance", "after": {"new_version": "3.1.0", "paired_recovery": True}}
# POST /api/v1/change-plans, then POST /api/v1/change-plans/{id}/execute.
# Read durable state after an uncertain response; do not repeat the POST.
```
- [ ] Run setup/configuration/backup/rejection/permission stages and independently assert healthy selected runtime, exact config bytes and real selected target samples.
- [ ] Run real2.55.0→3.1.0 optional capture upgrade, record both backup associations and independently verified versions. Fixed timestamp queries distinguish pre-capture and post-capture metrics.
- [ ] POST /api/v1/change-plans/{id}/paired-recovery exactly once; GET durable result as necessary. Assert completed, historical binary/runtime/config/data and fixed-sample discrimination.
- [ ] Exercise retained owned fixture observation/adoption and all three retirement intents, with independent service/data/target evidence. Retained resources are intentional until controller cleanup.
- [ ] Verify real browser/public display against actual state using the existing Playwright runtime. Run focused syntax checks and any meaningful regression required by actual fixes. No mocked response is represented as remote acceptance.
- [ ] Write report with stage commands/outcomes, raw log paths, source hashes and any actual blocker. Freeze source and hand off for independent task review; do not stage or commit.

### Task 2: Controller final acceptance and delivery

**Files:** Evidence/verification under `<external-acceptance-evidence-directory>`; update `<external-project-roadmap>`. Source fixes and operator notes only as proven necessary.
**Interfaces:** Consumes the reviewed runner, result.json and exact owned-resource manifest; produces final verification, local commit if source changed and Windows hash receipt.

- [ ] Check task report against public API results and independent SSH samples; correct blockers through a bounded fix/review round.
- [ ] Verify exact owned resources cleaned or intentionally retained, and original business endpoints/configs/units match baseline.
- [ ] Run required final configured index/detect and exact-source checks before local commit. Preserve earlier failed and passing records.
- [ ] Produce a concise acceptance archive and update the roadmap using business milestones and real limits. The installable package remains the following milestone.

## Self-review

Task1 produces one complete business result rather than independent framework features. Task2 consumes only its manifest/results and has no parallel source owner. Both agree on private runtime inputs, isolated resource identity, source review, exact-version evidence and preserved failure logs. All six spec outcomes map to Task1; cleanup/index/commit/delivery map to Task2.
