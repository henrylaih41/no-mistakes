---
title: Configuration
description: Global and per-repo configuration options.
---

Configuration is optional. Without config files, `no-mistakes` defaults to `agent: auto`, which picks the first supported native agent available on your system, with sensible defaults for everything else.

Config exists for the parts that genuinely vary by machine or repo:

- which agent or ordered fallback list you prefer
- whether review uses a cross-family reviewer panel or post-PR review loop
- which test or lint commands are canonical for the repo
- where test evidence and design context come from
- how aggressive the auto-fix loop should be
- how soon AXI should call an active step quiet
- whether the review loop reuses supported native agent sessions
- whether no-mistakes should infer intent from recent local agent transcripts

Config is split across two files:

| File | Scope | Full field reference |
|---|---|---|
| `~/.no-mistakes/config.yaml` | Global defaults for all repos | [Global Config Reference](/no-mistakes/reference/global-config/) |
| `<repo>/.no-mistakes.yaml` | Per-repo overrides | [Repo Config Reference](/no-mistakes/reference/repo-config/) |

Set `NM_HOME` to relocate the global config directory. Provider credentials and review-loop tokens come from environment variables where documented; see [Environment Variables](/no-mistakes/reference/environment/).

## How to think about config

- **Global config** is for machine-level defaults.
- **Repo config** is for codebase-specific behavior that should travel with the repo.

Most teams should keep personal preferences global and repo policy local.

## What to configure first

1. Set `commands.test` and `commands.lint` in repo config so the gate runs the exact commands the repo expects.
2. Override `agent` per repo only when one codebase works better with a different tool or fallback order.
3. Tune `auto_fix` after seeing how much automation you want.

The reference pages own each field's syntax, defaults, and exact semantics. This page covers only cross-cutting rules involving both files.

## Precedence

- Repo `agent` replaces global `agent`, including an ordered fallback list.
- `auto_fix`, `intent`, `review_loop`, and `test.evidence` overlay individual fields; `intent.disabled_readers` adds to globally disabled readers. A present repo `review` block replaces the global reviewer panel wholesale, while an absent block inherits it.
- `agent_path_override`, `agent_args_override`, `acpx_path`, `acp_registry_overrides`, `ci_timeout`, `daemon_connect_timeout`, `step_quiet_warning`, `log_level`, and `session_reuse` are global-only.
- `commands`, `ignore_patterns`, `design_context`, `document.instructions`, and `allow_repo_commands` are repo-only.
- By default, `commands`, `agent`, `review`, and `review_loop` are read from the trusted default branch. A trusted `allow_repo_commands: true` opt-in honors their pushed-branch values. `document.instructions` and `allow_repo_commands` themselves always come from the trusted default branch; non-executing `design_context` remains branch-scoped. See the [Repo Config Reference](/no-mistakes/reference/repo-config/) security note.
- no-mistakes reloads global config while setting up each run. For repeatable profiles, use separately initialized `NM_HOME` roots; each root has its own config and state.

## Explicit commands versus agent detection

Explicit `commands.test` and `commands.lint` provide deterministic baseline behavior. Leaving either empty asks the configured agent to fill the gap: tests are detected and run by the agent, while lint folds into the document step's combined housekeeping pass.

An empty `commands.format` runs no separate formatter. Available user intent can still trigger evidence-oriented agent validation after a successful test baseline, and evidence remains temporary unless the repo opts into `test.evidence.store_in_repo`.

The [Repo Config Reference](/no-mistakes/reference/repo-config/) owns exact command semantics, process lifetime, and `ignore_patterns` matching.

Before a new gate starts, its effective agent configuration must resolve to a runnable native agent or ACP bridge, even when explicit commands are configured. Run `no-mistakes doctor` to check the global runner, and see [Choosing an Agent](/no-mistakes/guides/agents/) for selection and fallback behavior.
