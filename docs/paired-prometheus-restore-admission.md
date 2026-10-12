# Paired historical restore admission preview

Step 10.2.2.1 adds the internal service method `BackupService.PreviewPrometheusRecoveryPointRestore(ctx, planID)`. It assesses whether the retained paired history and the currently running binary Prometheus instance satisfy the admission contract for a future restoration transaction.

This method has no HTTP route and performs no restore, stop, restart, daemon reload, file replacement, version update or automatic upgrade action. It temporarily claims the host and component in that order. The returned observation is not an execution token and always has `restore_authorized=false`, `requires_downtime=true`, `requires_current_checkpoint=true`, and scope `paired_restore_admission_preview_only`.

## Required history and current state

The plan must be failed or verified, with a ready paired recovery point and matching pinned backup/manifest/plan associations. A verified plan must currently run its target version; a failed plan may still run the before or target version. Both current and historical versions must be supported stable Prometheus versions. Unknown or unrelated versions reject admission.

Current host identity, deployment type, service, executable/config/data paths and retained imported arguments must match the historical binding. Actual executable/version, process identity, literal loaded launch and argv are checked through the existing runtime admission probes. An unresolved backup pause or version transition rejects admission even when the caller already holds a reentrant component claim. Retained same-host Thanos components, including stopped instances, must be compatible with the historical target.

Startup path roles, optional environment-file declarations and enablement must match history. Startup contents and supported metadata may differ and are reported as replacements. Optional files are reported as `replace`, `remove`, or `retain_absence`, depending on their historical/current presence. Config and TSDB roots are listed as `replace_from_stopped_archive`; their current contents are not compared with the stopped archive.

## Ownership and complete reference inventory

The preview checks retained same-host registered component paths and direct systemd references: fragment, drop-ins, environment files, and all start/stop/condition/reload executables. Absolute literal references, including template-file @ characters and parent-directory segments, are canonicalized on the owning host and checked for overlap with every proposed replacement and the protected backup root. A symlink in another service cannot hide a shared target.

Installed service instances and loaded transient/dependency names are enumerated before and after property collection. Uninstantiated templates are excluded. Every requested name must be covered by a canonical ID and every alias must have one owner. Repeated execution rows used by systemd 239 are supported. Same-ID aliases with identical paths are merged; paths or cross-ID ownership disagreement rejects admission. Names existing solely as missing dependencies are accepted only with `not-found/inactive/dead`, zero main/control PIDs, and no file/executable reference.

Collection limits are 2048 service names, 512 KiB per input listing and 2 MiB property output. Exact byte counts preserve trailing newlines. Failed commands, incomplete frames, unknown fields, truncated command records, conflicting aliases and oversized output reject admission.

The supported reference representation requires literal absolute paths. Bare executable names, glob/dynamic/escaped paths and ambiguous braces in human-rendered argv reject the whole preview. Standard environment-variable tokens in argv are allowed; the executable path must remain literal. In particular, newer WSL vendor units use bare executable names; the local acceptance fixture verifies this explicit rejection boundary. Full positive acceptance runs use the authorized systemd 239 test hosts. The existing systemctl interface is documented in the [systemd v239 source](https://github.com/systemd/systemd/blob/v239/man/systemctl.xml).

Nonempty service root/image/bind/tmpfs/extension mappings reject admission because a service-local path cannot then be equated with a host path. For the three systemd 239 array fields that print `[unprintable]`, the collector separately verifies exact typed empty values with bounded D-Bus reads. A placeholder alone is never accepted as emptiness.

## Evidence and stability

The method freshly verifies the binary artifact, startup snapshot and stopped archive, then repeats the current startup/binary/process/reference observations and trusted database admission checks. Changes detected during the observation reject the preview. Rejection does not compensate another actor's changes, resume a paused service, update history or retain operation claims.

The returned object includes IDs, source/target versions, replacement roles and actions, dependency results, completed checks and an observation timestamp. It does not return raw argv, environment contents, manifest payloads, SSH credentials or command output.

## Remaining transaction work

A successful preview is only a current observation. It does not certify application semantics, ACL/xattr preservation, external config dependencies, indirect helper accesses, enablement reconstruction or changes after observation. The actual restore must repeat admission, capture a complete current checkpoint, maintain a durable recovery journal, replace the paired artifacts, verify readiness and historical samples, and implement compensation and crash recovery. Those operations remain the next Step 10.2.2 transaction substep.
