---
title: Repo Config Reference
description: All fields for .no-mistakes.yaml.
---

Per-repo configuration lives in `.no-mistakes.yaml` at the root of your repository.

:::caution[Security: code-executing fields are read from the default branch]
`commands.*` execute arbitrary shell on the daemon host via `sh -c` / `cmd.exe /c`, `agent` selects which process launches there (including `acp:` targets), `review.reviewers` selects extra reviewer processes, and `review_loop` gates the post-PR review loop (it names the bot login whose comments become fix-prompt content, bounds how many automated fix rounds run, and points at a secret key file), all with the maintainer's credentials. To prevent a supply-chain attack where a contributor lands a hostile value on a gated branch, the daemon always reads **`commands`, `agent`, `review`, and `review_loop` from your default branch** (e.g. `origin/main`), never from the pushed SHA, and reads them at the exact commit a fresh fetch resolved (so a stale `origin/<default>` ref cannot serve a value the live default branch removed). If the fetch fails, those repo-level code-executing fields are forced empty or absent - the run proceeds on built-in/global defaults rather than falling back to a potentially stale or hostile copy. Commit the `commands`, `agent`, `review` panel, and `review_loop` you want the gate to run to your default branch. Non-executing fields (`ignore_patterns`, `auto_fix`, `intent`, `test`, `design_context`) are still read from the pushed branch.

If you genuinely want per-branch `commands`, `agent`, `review`, and `review_loop` (for example, a single-developer repo where you trust your own feature branches), opt in with [`allow_repo_commands: true`](#allow_repo_commands) in this same file on your default branch. This re-enables the previous behavior with eyes open. The switch is read only from the trusted default-branch copy, so a contributor cannot self-enable it from a pushed branch.
:::

```yaml
# .no-mistakes.yaml

agent: codex

commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."

ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

auto_fix:
  rebase: 3
  review: 3
  test: 3
  document: 3
  lint: 5
  ci: 3

review:
  reviewers:
    - agent: codex
    - agent: claude
  max_parallel: 2
  fail_open: false

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: true
    dir: .no-mistakes/evidence

design_context:
  files:
    - docs/design/*.md
    - docs/adr/*.md
```

## Fields

### agent

Override the default agent for this repo and its setup-wizard suggestions.

| | |
|---|---|
| Type | `string` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `grok`, `acp:<target>` |
| Default | Inherits from global config |

`auto` resolves to the first supported native agent found on `PATH` in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, then `grok` (when `grok --version` succeeds).
`acp:<target>` uses the user-installed `acpx` binary configured in global config.
ACP agents are opt-in and are not considered by `agent: auto`.

### allow_repo_commands

Opt in to honoring the code-executing selection fields (`commands.{test,lint,format}`, `agent`, `review`, and `review_loop`) from a contributor's pushed branch instead of the trusted default-branch copy.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |

This field is itself read **only from the trusted default-branch copy** of `.no-mistakes.yaml`, never from the pushed SHA, so a contributor cannot self-enable it by setting it on a feature branch. By default the daemon reads `commands`, `agent`, `review`, and `review_loop` from your default branch (e.g. `origin/main`) so a pushed SHA cannot inject shell or pick launched agents on the daemon host. Leave this `false` for any repo that accepts contributions. Set it to `true` only for a single-developer environment where you trust every branch you push (for example, a personal repo gated by your own daemon).

### commands.test

Explicit test command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent auto-detects tests and evidence checks) |

When set, the test step runs this exact command first as the baseline and checks the exit code.
When empty, the agent detects and runs relevant tests itself.
When user intent is available, the agent may still run after a successful baseline command to gather evidence-oriented validation.

### design_context.files

Repository-relative design-context file selectors to inject into reviewer and fixer prompts for every run on that branch.

| | |
|---|---|
| Type | `string[]` |
| Default | Empty |

Each entry is a repository-relative path or glob.
Matches are sorted and de-duplicated, read once at run start, and stored on the run so later fix rounds use the same design contract even if files change.
Reviewers and fixers are told to check the implementation against this contract and to flag deviations from it, not to treat the files as instructions that override no-mistakes prompt rules.

Repo-config paths must stay inside the run worktree after symlink resolution.
Absolute paths, `~`, `..`, non-regular files, missing explicit files, globs with no matches, and invalid UTF-8 fail the run start with a clear error.
Use [`no-mistakes axi run --design-context`](/no-mistakes/reference/cli/#no-mistakes-axi-run) for explicit local files outside the repository.

### commands.lint

Explicit lint command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
|---|---|
| Type | `string` |
| Default | Empty (agent auto-detects) |

When set, the lint step runs this exact command and checks the exit code.
When empty, the agent detects relevant linters and formatters, applies safe fixes, reruns the relevant checks, commits any agent changes, and reports only unresolved issues.

### commands.format

Formatter command run before the push step commits agent fixes.

| | |
|---|---|
| Type | `string` |
| Default | Empty (no separate push-step formatter) |

This does not prevent empty `commands.lint` from detecting and running formatters during the lint step.

### Command process lifetime

All configured `commands.*` entries are scoped to their step.
After no-mistakes starts one of these commands, it terminates any remaining child processes from that command when the command exits, fails, or the step is cancelled.
Do not rely on a configured command to leave a background server or watcher running after it returns; keep that service inside the command lifetime or start it outside no-mistakes.

### ignore_patterns

Paths to exclude from review and documentation checks.

| | |
|---|---|
| Type | `string[]` |
| Default | Empty (no ignores) |

Pattern matching rules:

| Pattern | Rule |
|---|---|
| `*.generated.go` | No slash - matches by basename |
| `vendor/**` | Ends with `/**` - matches entire subtree |
| `some/path/file.go` | Contains a slash - full path glob |

### auto_fix

Override auto-fix attempt limits for specific steps. Fields not set here inherit from global config.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default |
|---|---|---|
| `auto_fix.rebase` | `int` | Inherits from global (default `3`) |
| `auto_fix.review` | `int` | Inherits from global (default `0`) |
| `auto_fix.test` | `int` | Inherits from global (default `3`) |
| `auto_fix.document` | `int` | Inherits from global (default `3`) |
| `auto_fix.lint` | `int` | Inherits from global (default `3`) |
| `auto_fix.ci` | `int` | Inherits from global (default `3`) |

Set to `0` to disable the follow-up auto-fix loop for a step (findings require manual approval).
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the agent still attempts safe fixes during the initial lint pass; unresolved lint findings then pause for approval instead of starting another automatic fix loop.

`auto_fix.ci` covers the CI step's CI failure and merge-conflict auto-fix attempts.

Legacy alias: `auto_fix.babysit`.

### review

Optional repo-level cross-family review panel.
The schema matches global `review` config: `reviewers`, optional per-reviewer `agent` / `args` / `path`, `max_parallel`, `fail_open`, and `max_fix_rounds`.
An absent `max_fix_rounds` inside an explicit repo `review` block still inherits the global cap; set `max_fix_rounds: 0` to restore unlimited review fix rounds for this repo.

```yaml
review:
  reviewers:
    - agent: codex
    - agent: claude
  max_parallel: 2
  fail_open: false
```

When present, the repo `review` block overrides the global review panel wholesale.
When absent, the repo inherits the global panel.
An explicit empty panel disables an inherited global panel and returns to the single-agent default:

```yaml
review:
  reviewers: []
```

Because reviewers select extra agent processes to launch with maintainer credentials and `max_fix_rounds` controls review fix-round policy, repo-level `review` is treated like `commands` and `agent`: by default it is read only from the trusted default-branch copy of `.no-mistakes.yaml`.
A review panel pushed only on a feature branch is ignored unless `allow_repo_commands: true` is already set on the trusted default branch.

### review_loop

Override the post-PR review loop for this repo.
The schema matches global [`review_loop`](/no-mistakes/reference/global-config/#review_loop): `enabled`, `bot_login`, `max_rounds`, `fail_open`, `reply_on_fix`, `retrigger`, `devin_api_key_file`, `devin_review_api_key_file`, and `devin_org_id`.

| Field | Type | Default |
|---|---|---|
| `review_loop.enabled` | `bool` | Inherits from global (default `false`) |
| `review_loop.bot_login` | `string` | Inherits from global (default `devin-ai-integration[bot]`) |
| `review_loop.max_rounds` | `int` | Inherits from global (default `3`) |
| `review_loop.fail_open` | `bool` | Inherits from global (default `true`) |
| `review_loop.reply_on_fix` | `bool` | Inherits from global (default `true`) |
| `review_loop.retrigger` | `bool` | Inherits from global (default `true`) |
| `review_loop.devin_api_key_file` | `string` | Inherits from global (default `~/.config/devin/api_key`) |
| `review_loop.devin_review_api_key_file` | `string` | Inherits from global (default `~/.config/devin/review_api_key`) |
| `review_loop.devin_org_id` | `string` | Inherits from global (default empty) |

Fields not set here overlay onto the global review loop, and then the built-in defaults.
Because the review loop gates CI, names the bot login whose comments become fix-prompt content, bounds how many fix rounds run, and points at a secret key file, repo-level `review_loop` is treated like `commands`, `agent`, and `review`: by default it is read only from the trusted default-branch copy of `.no-mistakes.yaml`.
A review loop enabled only on a feature branch is ignored unless `allow_repo_commands: true` is already set on the trusted default branch.

### intent

Override transcript-based user-intent extraction settings for this repo.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
|---|---|---|
| `intent.enabled` | `bool` | Inherits from global (default `true`) |
| `intent.threshold` | `float` | Inherits from global (default `0.2`) |
| `intent.slack_days` | `int` | Inherits from global (default `3`) |
| `intent.disabled_readers` | `string[]` | Adds to globally disabled readers |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

### test.evidence

Override where evidence artifacts from the test step are stored.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
|---|---|---|
| `test.evidence.store_in_repo` | `bool` | Inherits from global (default `false`) |
| `test.evidence.dir` | `string` | Inherits from global (default `.no-mistakes/evidence`) |

By default, test evidence stays in a temporary directory keyed by run ID and is referenced by local path.
Set `store_in_repo: true` to write evidence under `<dir>/<branch-slug>` inside the worktree so push can commit and publish it with the branch.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.
