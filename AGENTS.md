## WSL 重启与停止须经用户确认

任何重启、关闭或终止 WSL 的操作，必须先说明原因和影响，并获得用户对本次操作的明确确认后才能执行。包括 wsl --shutdown、wsl --terminate、发行版内的 reboot/shutdown，以及通过 Windows 服务或进程间接重启、停止 WSL 的操作。

故障恢复、超时处理和环境清理也不例外；已有的开发、测试或部署授权不视为 WSL 重启授权。未获确认时，只能继续不依赖重启 WSL 的工作，不得自行执行或用其他方式绕过确认。

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **prometheus-lifecycle-platform**.

> Index stale? Run `node .gitnexus/run.cjs analyze --index-only` from the project root — it auto-selects an available runner. No `.gitnexus/run.cjs` yet? Bootstrap with `npx`, `bunx`, or `pnpm dlx` — e.g. `bunx gitnexus@latest analyze` (npm 11 npx crash; #1939).

## Always Do

- **MUST run impact before editing.** Use `impact({target: "symbolName", direction: "upstream"})` or `node .gitnexus/run.cjs impact "symbolName" --direction upstream --repo .`; report callers, processes, and risk. Never substitute grep for graph analysis.
- **MUST analyze graph changes before committing.** Use `detect_changes({scope: "all"})` (MCP) or `node .gitnexus/run.cjs detect-changes --scope all --repo .` (CLI fallback). `partial: true` or `truncated: true` is not a clean check — a zero means unseen, not unaffected; re-run it. For regression review: `detect_changes({scope: "compare", base_ref: "main"})` or `node .gitnexus/run.cjs detect-changes --scope compare --base-ref "main" --repo .`.
- MUST warn on HIGH/CRITICAL `risk` pre-edit; never use `riskSharedAxes` to waive a HIGH/CRITICAL `risk` warning. Compare File/symbol: MCP File omits axes; Graph-RAG expands File.
- **MUST treat `risk: UNKNOWN` as unresolved, not as low.** An empty caller set is not evidence the symbol is unused — it can also mean the callers are not resolvable by the index (plain-object property access, dynamic dispatch, cross-language calls). `impact` pairs `UNKNOWN` with a `riskNote` saying so. Confirm with a text search before treating the symbol as safe to change or delete; do not proceed on the strength of a zero.
- **MUST use `query({search_query: "concept"})` for concepts/flows, `context({name: "symbolName"})` for a named symbol, or `impact` for blast radius, on read-only callers, dependencies, imports, or execution flow.** Graph first; text search only for empty/`UNKNOWN`/literals.
- For security review, `explain({target: "fileOrSymbol"})` lists taint findings (source→sink flows; needs `analyze --pdg`).

## Never Do

- NEVER edit a function, class, or method before MCP/CLI impact analysis.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis, and never read `UNKNOWN` as an all-clear — it means the walk could not answer, which is the one verdict that requires confirming by other means.
- NEVER rename symbols with find-and-replace — use `rename` which understands the call graph.
- NEVER commit before MCP/CLI graph change analysis.

## Resources

| Resource | Use for |
| --- | --- |
| `gitnexus://repo/prometheus-lifecycle-platform/context` | Codebase overview, check index freshness |
| `gitnexus://repo/prometheus-lifecycle-platform/clusters` | All functional areas |
| `gitnexus://repo/prometheus-lifecycle-platform/processes` | All execution flows |
| `gitnexus://repo/prometheus-lifecycle-platform/process/{name}` | Step-by-step execution trace |

## Cross-Repo Groups

This repository is listed under GitNexus **group(s): prometheus-lifecycle-platform-full** (see `~/.gitnexus/groups/`). For cross-repo analysis, use MCP tools `impact`, `query`, and `context` with `repo` set to `@<groupName>` or `@<groupName>/<memberPath>` (paths match keys in that group’s `group.yaml`). Use `group_list` / `group_sync` for membership and sync. From the project root: `node .gitnexus/run.cjs group list`, `node .gitnexus/run.cjs group sync <name>`, `node .gitnexus/run.cjs group impact <name> --target <symbol> --repo <group-path>` (the `.gitnexus/run.cjs` path is repo-root-relative).

## CLI

| Task | Read this skill file |
| --- | --- |
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->

## Project indexing scope

The user requires indexing the entire project source, including all vendor dependencies, tests, frontend bundles, and CI configuration. Preserve `.gitnexusignore` inclusions. Refresh with a full-project indexing wrapper that retains the 32 MiB file threshold and resource limits; the original environment-specific wrapper is not included in this source snapshot. Runtime databases, credentials, logs, and SESSION_NOTE.md remain excluded. Check coverage and `incomplete_reasons` before claiming the index is complete; HTML inline-script symbols may not be resolved by GitNexus, so verify those in source.

The hidden `.github` directory is indexed separately as `prometheus-lifecycle-platform-ci` because the GitNexus 1.6.11 root scanner uses dot=false. The group `prometheus-lifecycle-platform-full` combines the source and CI indexes. The full-index script refreshes both. Use explicit repo names for symbol queries.
