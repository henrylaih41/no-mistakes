---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: auto

acpx_path: acpx

acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs

agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
  pi: /usr/local/bin/pi
  copilot: /usr/local/bin/copilot

agent_args_override:
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"

ci_timeout: "168h"

step_quiet_warning: "10m"

daemon_connect_timeout: "3s"

log_level: info

session_reuse: true

auto_fix:
  rebase: 3
  review: 0
  test: 3
  document: 3
  lint: 3
  ci: 3

review:
  reviewers:
    - agent: codex
    - agent: claude
      args:
        - --model
        - sonnet
      path: /Users/you/bin/claude-review
  max_parallel: 2
  fail_open: false
  max_fix_rounds: 0

review_loop:
  enabled: false
  bot_login: "devin-ai-integration[bot]"
  max_rounds: 3
  fail_open: true
  reply_on_fix: true
  retrigger: true
  devin_api_key_file: "~/.config/devin/api_key"
  devin_review_api_key_file: "~/.config/devin/review_api_key"
  devin_org_id: ""

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: false
    dir: .no-mistakes/evidence
```

## Fields

### agent

Default agent for all repos and setup-wizard suggestions. Can be overridden per-repo.

|         |                                                                                   |
| ------- | --------------------------------------------------------------------------------- |
| Type    | `string` or `string[]`                                                            |
| Values  | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `acp:<target>` |
| Default | `auto`                                                                            |

`auto` resolves to the first supported native agent found on `PATH` in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, then `copilot`.
`acp:<target>` uses the user-installed `acpx` binary to run an ACP target, for example `acp:gemini`.
ACP agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If an explicit agent is unavailable, `auto` finds no native agent, or no fallback-list entry is available, the gate fails before its first pipeline step rather than reporting a partial command-only validation as passed.
`no-mistakes doctor` checks the global configuration, while every run repeats resolution after applying any trusted repository-level `agent` override.

You can also set an ordered fallback list:

```yaml
agent: [codex, claude]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.

### acpx_path

Path to the user-installed `acpx` binary used for `agent: acp:<target>`.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | `acpx`   |

### acp_registry_overrides

Map an ACP target name to a raw ACP agent command.
When `agent: acp:<target>` matches an override key, no-mistakes runs `acpx --agent <command>` instead of `acpx <target>`.

|         |                     |
| ------- | ------------------- |
| Type    | `map[string]string` |
| Default | Empty               |

Example:

```yaml
agent: acp:local-gemini
acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs
```

### agent_path_override

Custom binary paths for native agents.
When set, `no-mistakes` uses this path instead of looking up the binary on `PATH`.
ACP agents use `acpx_path` instead.

|         |                                   |
| ------- | --------------------------------- |
| Type    | `map[string]string`               |
| Default | Empty (uses default binary names) |

Default native binary names when no override is set:

| Agent      | Binary     |
| ---------- | ---------- |
| `claude`   | `claude`   |
| `codex`    | `codex`    |
| `rovodev`  | `acli`     |
| `opencode` | `opencode` |
| `pi`       | `pi`       |
| `copilot`  | `copilot`  |

### agent_args_override

Extra CLI flags to pass to each native agent.
Use this to set model selection, service tier, reasoning effort, permission mode, or any other flag the underlying agent supports.

|         |                                                           |
| ------- | --------------------------------------------------------- |
| Type    | `map[string][]string`                                     |
| Keys    | `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot` |
| Default | Empty (no extra flags)                                    |

User-supplied flags are inserted ahead of no-mistakes' managed flags, so your choices usually take precedence. A few flags are reserved because no-mistakes depends on them to communicate with the agent - setting any of these returns a config error on load:

| Agent      | Reserved flags                                                                                              |
| ---------- | ----------------------------------------------------------------------------------------------------------- |
| `claude`   | `-p`, `--print`, `--verbose`, `--output-format`, `--json-schema`, `-r`, `--resume`, `--session-id`, `-c`, `--continue`, `--fork-session` |
| `codex`    | `exec`, `resume`, `--resume`, `--session`, `--session-id`, `--thread`, `--thread-id`, `--last`, `--json`, `--color` |
| `rovodev`  | `rovodev`, `serve`, `--disable-session-token`                                                               |
| `opencode` | `serve`, `--hostname`, `--port`, `--print-logs`                                                             |
| `pi`       | `--mode`, `--no-session`                                                                                    |
| `copilot`  | `-p`, `--prompt`, `--output-format`, `--no-color`                                                          |

For structured `codex` runs, no-mistakes also appends its own `--output-schema <tempfile>` after your overrides. Treat that flag as managed even though config validation does not currently reject it.
The Claude and Codex session-control forms are reserved so no-mistakes can keep reviewer and fixer conversations role-isolated.

Smart defaults:

- For `claude`, supplying `--permission-mode` (or `--dangerously-skip-permissions`) suppresses the default `--dangerously-skip-permissions`.
- For `codex`, supplying `--ask-for-approval`, `--sandbox`, or `--dangerously-bypass-approvals-and-sandbox` suppresses the default `--dangerously-bypass-approvals-and-sandbox`.

Reviewers declared in `review.reviewers` inherit `agent_args_override` by agent name unless the reviewer sets its own `args`.

Permission and sandbox flags affect the underlying agent, but they do not disable no-mistakes' pipeline prompt steering.
Pipeline agents are still told to keep intentional writes inside the worktree and avoid mutating system state outside it.

Example:

```yaml
agent_args_override:
  claude:
    - --model
    - sonnet
    - --permission-mode
    - acceptEdits
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"
  rovodev:
    - --profile
    - work
  opencode:
    - --model
    - gpt-5
  pi:
    - --provider
    - google
```

For Codex, `service_tier` and `model_reasoning_effort` tune different things: `service_tier` selects the speed or priority lane, while `model_reasoning_effort` selects reasoning depth. no-mistakes reloads global config while setting up each run, so edits made before `no-mistakes axi run` apply to that run. For repeatable profiles, use separately initialized `NM_HOME` directories; each has its own `config.yaml` and no-mistakes state.

### ci_timeout

How long the CI step monitors an open PR, including provider CI status and on GitHub, GitLab, or Azure DevOps PR mergeability, before giving up.

|         |                                                 |
| ------- | ----------------------------------------------- |
| Type    | `string` (Go duration, or an unlimited keyword) |
| Default | `168h` (7 days)                                 |

Accepts any Go `time.ParseDuration` string: `30m`, `2h`, `4h30m`, etc.

This is an idle timeout, not an absolute deadline: every time the base branch advances, the monitor re-arms it.
So an actively-updated green PR keeps its monitor no matter how long it stays open.
If it later develops an actual GitHub, GitLab, or Azure DevOps merge conflict, the CI auto-fix path rebases and re-pushes it, while a clean behind PR needs no command.
A genuinely idle/abandoned PR still parks at an approval gate after the timeout elapses.
While that CI gate is parked, the daemon continues bounded read-only PR-state checks.
If the PR is merged or closed externally, the stale gate completes automatically; an open, unknown, or temporarily unreachable PR remains parked for a user decision.

Set it to `unlimited` (`none`, `off`, and `never` are accepted aliases), `0`, or any non-positive duration to monitor until the PR is merged, closed, or the run is aborted with `no-mistakes axi abort --run <id>`.

Legacy alias: `babysit_timeout`.

### step_quiet_warning

How long a running or fixing step can go without recorded step-log or native-agent lifecycle activity before AXI status marks the step as quiet.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `10m`                  |

Accepts any positive Go `time.ParseDuration` string: `30s`, `5m`, `1h`, etc.
Non-positive values are ignored and keep the default.

This is observability only.
It does not cancel the step, change auto-fix behavior, or mark the run failed.
AXI renders the quiet signal in the `active_steps` table as part of `last_activity`, for example `quiet 12m3s ago: codex started pid=4242`.
For older active runs that do not yet have activity rows, AXI falls back to the step log file's modification time.

### daemon_connect_timeout

Maximum time a CLI client waits for an existing daemon socket to accept a connection before failing instead of hanging. Guards against a daemon process that is alive but stuck or unresponsive.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `3s`                   |

Accepts any positive Go `time.ParseDuration` string. Overridable per-invocation with the `NM_DAEMON_CONNECT_TIMEOUT` environment variable; see [Environment Variables](/no-mistakes/reference/environment/#nm_daemon_connect_timeout).

### log_level

Daemon log verbosity.

|         |                                  |
| ------- | -------------------------------- |
| Type    | `string`                         |
| Values  | `debug`, `info`, `warn`, `error` |
| Default | `info`                           |

### session_reuse

Per-run, per-role agent session reuse for the review loop.

|         |        |
| ------- | ------ |
| Type    | `bool` |
| Default | `true` |

When enabled and the pipeline agent supports native session resume (claude via `--resume`, codex via `exec resume`), each run keeps one durable reviewer session across the initial full review and every full rereview, and a separate durable fixer session across review-fix turns.
The roles never share a session, other pipeline steps stay session-isolated in their own cold invocations, and different runs never reuse identities.
Every review turn still performs a full review of the complete branch diff; only the reviewer's own prior context is carried.
When resume is unavailable or fails, the invocation falls back to a cold run or a fresh same-role session and the fallback is recorded in the local `agent_invocations` performance record.
Session identities are persisted only as minimum local resume metadata, never as prompts or transcripts.
The [daemon crash-recovery reference](/no-mistakes/concepts/daemon/#crash-recovery) owns which parked gates can resume or reconcile after a restart.
Set `false` to force every agent invocation cold.

### auto_fix

Maximum follow-up auto-fix attempts per step. Set a step to `0` to disable the follow-up auto-fix loop, so findings require manual approval.
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the document step's combined housekeeping pass also attempts safe lint fixes, and the lint step consumes its result; unresolved blocking lint findings then pause for approval instead of starting another automatic fix loop.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field               | Type  | Default | Description                                                                                 |
| ------------------- | ----- | ------- | ------------------------------------------------------------------------------------------- |
| `auto_fix.rebase`   | `int` | `3`     | Rebase conflict auto-fix attempts                                                           |
| `auto_fix.review`   | `int` | `0`     | Review finding auto-fix attempts                                                            |
| `auto_fix.test`     | `int` | `3`     | Test failure auto-fix attempts                                                              |
| `auto_fix.document` | `int` | `3`     | Not used by the automatic document pass                                                     |
| `auto_fix.lint`     | `int` | `3`     | Lint issue auto-fix attempts                                                                |
| `auto_fix.ci`       | `int` | `3`     | CI auto-fix attempts for CI failures, plus GitHub, GitLab, and Azure DevOps merge conflicts |

Legacy alias: `auto_fix.babysit`.

These are global defaults. Per-repo config can override individual steps.

### review

Optional cross-family review panel for the review step.
When no reviewers are configured, review runs once with the resolved `agent`, preserving the default single-agent behavior.
When reviewers are configured, no-mistakes sends the same review prompt to each reviewer independently, then merges the reports into one findings payload for the fix agent and approval UI.

| | |
|---|---|
| Type | `object` |
| Default | Empty (single configured agent) |

| Field | Type | Default | Description |
|---|---|---|---|
| `review.reviewers` | `Reviewer[]` | Empty | Reviewers to run for the panel |
| `review.reviewers[].agent` | `string` | Required | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, or `acp:<target>` |
| `review.reviewers[].args` | `string[]` | Inherits `agent_args_override.<agent>` | Extra native-agent CLI flags for this reviewer |
| `review.reviewers[].path` | `string` | Inherits `agent_path_override.<agent>` or default binary | Binary path for this reviewer |
| `review.max_parallel` | `int` | `0` | Maximum reviewers to run at once; `0` means all reviewers at once |
| `review.fail_open` | `bool` | `false` | When `false`, any reviewer error fails the review step; when `true`, failed reviewers are dropped if at least one reviewer succeeds |
| `review.max_fix_rounds` | `int` | `0` | Maximum review fix/re-review rounds before parking at `awaiting_triage`; `0` means unlimited |

Each reviewer returns its own findings. In the merged gate, finding IDs are namespaced by reviewer and each finding's `source` is set to the reviewer name, so the TUI, AXI output, and fix prompt can show who reported it.
The merged `risk_level` is the highest risk any reviewer reported; summaries and rationales are labeled by reviewer.
The configured pipeline `agent` still applies fixes.

For reviewer specs, `agent: auto` expands to the already resolved pipeline `agent`.
Use an explicit reviewer agent name when you want a different family.
Reviewer `args` use the same reserved-flag checks as `agent_args_override`.
For `acp:<target>` reviewers, use `acpx_path` and `acp_registry_overrides`; native `args` are ignored by ACP agents.
Identical resolved reviewers are de-duplicated.

Per-repo config can override the global review block wholesale.
An absent repo `review` block inherits the global panel; an explicit repo block with `reviewers: []` disables the inherited panel and reverts to the single-agent default.
For `review.max_fix_rounds`, an absent field inside an explicit repo `review` block still inherits the global cap; set `max_fix_rounds: 0` explicitly to restore unlimited review fix rounds for that repo.
`review.max_fix_rounds` counts all persisted review fix rounds from `step_rounds`, both automatic review fixes and user-approved `axi respond --action fix` rounds.
When the cap is reached, the review step parks at `awaiting_triage` with residual findings intact.
A normal fix response is refused; another fix round requires `axi respond --action fix --fix-override --override-reason "<master triage reason>" --findings <ids>`, and the reason is persisted on the round.

Repo-level `review` is code-executing config because it selects extra agent processes and controls review fix-round policy.
Like repo `commands`, `agent`, and `review_loop`, it is read from the trusted default-branch `.no-mistakes.yaml` unless `allow_repo_commands: true` is already set there.

### review_loop

Optional post-PR review loop (read layer). When enabled, the CI step reads an external review bot's PR verdict and findings, feeds them back to no-mistakes' own fix agent, optionally re-triggers the bot, and converges on a bot-green verdict before the PR is allowed through.
The bot is review-only; no-mistakes is always the fixer, so no fixer agent is configured here.
The loop is off by default, so leaving it unset keeps pipeline behavior byte-identical.

| | |
|---|---|
| Type | `object` |
| Default | Disabled |

| Field | Type | Default | Description |
|---|---|---|---|
| `review_loop.enabled` | `bool` | `false` | Turn the post-PR review loop on |
| `review_loop.bot_login` | `string` | `devin-ai-integration[bot]` | GitHub account whose PR reviews are read |
| `review_loop.max_rounds` | `int` | `3` | Maximum review/fix rounds before escalating (must be `>= 0`) |
| `review_loop.fail_open` | `bool` | `true` | When `true`, a silent reviewer does not block the PR; when `false`, the loop waits for the bot's verdict |
| `review_loop.reply_on_fix` | `bool` | `true` | After pushing a fix, post a threaded reply on each addressed review comment |
| `review_loop.retrigger` | `bool` | `true` | Explicitly re-trigger a Devin review via the Devin HTTP API instead of relying solely on Devin's auto-review |
| `review_loop.devin_api_key_file` | `string` | `~/.config/devin/api_key` | Path read for the legacy `/v1/sessions` Devin API key when `DEVIN_API_KEY` is unset (a leading `~` expands to the home directory) |
| `review_loop.devin_review_api_key_file` | `string` | `~/.config/devin/review_api_key` | Path read for the dedicated Devin Review API token (a `cog_`-prefixed service-user token, distinct from `devin_api_key_file`) when `DEVIN_REVIEW_API_KEY` is unset |
| `review_loop.devin_org_id` | `string` | _(empty)_ | Devin organization id used in the Review API path (`/v3/organizations/{org}/pr-reviews`) |

`retrigger` is best-effort: a missing key or any Devin API error is logged and the loop continues. Each trigger creates a paid Devin review, so the loop fires it at most once per head SHA. When a Devin Review token **and** `devin_org_id` both resolve, the loop prefers the dedicated Devin Review API (`POST /v3/organizations/{org}/pr-reviews`), which is not per-organization ACU-limited and so keeps working when `/v1/sessions` is exhausted (`out_of_quota`); the review token is read from `DEVIN_REVIEW_API_KEY` first and from `devin_review_api_key_file` otherwise (see [`DEVIN_REVIEW_API_KEY`](/no-mistakes/reference/environment/#devin_review_api_key)). Otherwise it falls back to the legacy `/v1/sessions` trigger, whose key is read from `DEVIN_API_KEY` first and from `devin_api_key_file` otherwise (see [`DEVIN_API_KEY`](/no-mistakes/reference/environment/#devin_api_key)).

With `fail_open: false`, the loop waits for the bot and leans on the CI step's idle timeout to escalate to the human gate. That idle timer re-arms whenever the base branch advances (see [`ci_timeout`](#ci_timeout)), so on a PR whose base branch is actively moving while the bot stays silent the timeout may never elapse - abort the run explicitly with `no-mistakes axi abort --run <id>` if that happens.

Like `commands`, `agent`, and `review`, the repo-level `review_loop` is a code-executing selection field: it gates CI, names the bot login whose comments become fix-prompt content, bounds how many fix rounds run, and points at a secret file, so repo-level `review_loop` is read only from the trusted default-branch copy of `.no-mistakes.yaml`.
Per-repo config overlays the global review loop field by field; fields not set in the repo fall through to the global value and then the built-in default.

### intent

Transcript-based user-intent extraction settings.
When enabled and no intent was supplied directly for the run, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, pass that summary to rebase, review, test, document, lint, CI auto-fix, and PR prompts, and include it in generated PR descriptions.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                     | Type       | Default | Description                                                |
| ------------------------- | ---------- | ------- | ---------------------------------------------------------- |
| `intent.enabled`          | `bool`     | `true`  | Enable transcript-based intent extraction                  |
| `intent.threshold`        | `float`    | `0.2`   | Minimum raw match score for selecting a transcript session |
| `intent.slack_days`       | `int`      | `3`     | Extra days to look back before the change window           |
| `intent.disabled_readers` | `string[]` | Empty   | Transcript readers to disable                              |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

The match score is the share of matching files mentioned in a transcript session; deleted files are ignored when the diff also contains non-deleted changes.
All-deletion diffs still match against the deleted changed files.
Mentioning extra files does not reduce the score.
For multi-file diffs, no-mistakes still requires at least two overlapping files and an effective minimum score of `0.5`.
Partial matches older than 24 hours are rejected unless their raw score is at least `0.8`.
If exactly one accepted candidate has a raw score of at least `0.85`, that decisive candidate wins before recency ranking.
Otherwise, accepted candidates are ranked by confidence, which combines the raw score with a small recency boost, with ties going to the most recent matching session, and ambiguous accepted candidates may be disambiguated by the configured pipeline agent.

### test.evidence

Test-step evidence storage settings.
By default, evidence artifacts stay in a temporary directory keyed by run ID and are referenced by local path.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                         | Type     | Default                 | Description                                                           |
| ----------------------------- | -------- | ----------------------- | --------------------------------------------------------------------- |
| `test.evidence.store_in_repo` | `bool`   | `false`                 | Commit and push test evidence artifacts from inside the repo worktree |
| `test.evidence.dir`           | `string` | `.no-mistakes/evidence` | Repo-relative parent directory used when `store_in_repo` is true      |

When `store_in_repo` is true, the test step writes evidence under `<dir>/<branch-slug>` and the push step stages files from that directory before committing agent changes.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.

These are global defaults. Per-repo config can override either field.

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, `NM_DAEMON_CONNECT_TIMEOUT`, provider credentials, the `DEVIN_API_KEY` and `DEVIN_REVIEW_API_KEY` review-loop tokens, and update-check suppression.
