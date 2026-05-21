---
name: iterion-bot-catalog
description: Catalog of iterion example bots — pick a bot name for each roadmap_item.assignee. The stock dispatcher routes by assignee through assignee_workflows.
---

# Iterion Bot Catalog — for whats-next.bot's `propose_roadmap`, `revise_roadmap`, and `emit_action`

Consumed by three phases:

1. **`propose_roadmap` / `revise_roadmap`** — pick the right
   bot name for each `roadmap_item.assignee`. Leave it `""`
   when no existing bot fits.
2. **`emit_action`** — validate every assignee against the
   catalog before creating issues. Unrecognised assignees get
   stripped to `""` and the issue is labelled
   `needs-manual-triage`.

**Trust check first**: this catalog enumerates bots that exist
in the iterion source tree. If the workspace is NOT iterion,
none of these will resolve — all assignees should be `""` and
all issues will be `needs-manual-triage`.

## The pivot: kanban-driven, not shell-driven

whats-next.bot no longer shells out `iterion run <bot>`. Instead
every roadmap item becomes a kanban issue on the native board at
`<workspace>/.iterion/dispatcher/`, and a **dispatcher** dispatches
them. The dispatcher is wired via `iterion dispatch <config.yaml>`.

**How the stock dispatcher picks a workflow per issue today**:
workflow routing is done by the runner built at `iterion dispatch`
startup, not by switching workflows inside a running `EngineRunner`:

1. **`assignee_workflows:` map** — when the issue's `assignee`
   has an entry in the dispatcher YAML's `assignee_workflows:`
   map, `RoutingRunner` selects the precompiled runner for that
   workflow. See
   [docs/dispatcher.md §Routing by issue assignee](../../../docs/dispatcher.md).
2. **`workflow:` default** — the precompiled global fallback when
   the assignee is empty or unmapped.

Native issues also have typed `Bot` / `BotArgs` fields. `BotArgs`
merges over rendered dispatch vars and is usable today. `Bot` is
resolved into `DispatchSpec.WorkflowPath` for custom runners/future
routing, but the stock `EngineRunner` ignores that field and runs
the workflow it was constructed with. Do not rely on per-ticket
`Bot` for stock workflow routing; use `assignee_workflows:`.

`assignee_dispatch:` (when present) replaces `dispatch.vars`
wholesale per assignee; per-ticket `BotArgs` then merges on top
key-by-key (see the issue-creation section below).

whats-next records the assignee on every issue so operators can drive
routing by setting `--assignee` and mapping it through
`assignee_workflows:`.

## Decision tree — pick `assignee` per roadmap item

Walk top-to-bottom; first match wins.

| If the work sounds like… | → `assignee` |
|---|---|
| "implement feature X", "add capability", "build the thing" | `feature_dev` |
| "review the whole codebase", "audit production-readiness", "find bugs anywhere" | `whole_improve_loop` |
| "focus on axis X" (security / observability / perf …) across the codebase | `whole_improve_loop` (with `--var improvement_prompt=…`) |
| "review this branch", "review the PR", "fix the diff against main" | `branch_improve_loop` |
| "upgrade dependencies", "patch CVEs", "bump versions" | `secured-renovacy` |
| "audit the docs", "find code↔doc drift", "doc/code alignment", "fix outdated README/CLAUDE.md" | `doc-align` |
| "audit the source for vulns", "find injection / SSRF / IDOR / secrets", "security scan", "OWASP audit" | `sec-audit-source` |
| "audit dependencies for malware", "supply-chain check", "scan installed packages", "post-`npm install` triage" | `sec-audit-deps` |
| architectural choice, hiring, prioritisation meeting, alignment | `""` |
| operator is vague or it's cross-cutting | `""` |
| long-term theme (a quarter+ horizon) | usually `""` |

When in doubt, prefer `""` and let the operator triage manually
in the board UI. An empty assignee is honest; a wrong one
wastes a bot run.

## Bot reference

### `feature_dev`

- **Path**: `examples/feature_dev/main.bot`
- **Required var**: `feature_prompt` (one feature + acceptance
  criteria).
- **Pipeline**: plan → act → simplify → alternating Claude/GPT
  review/fix → commit.
- **Budget**: 1 branch, 4h, $120.
- **Worktree**: `auto`. **Sandbox**: `auto`.
- **Use when**: an item can be phrased as one feature with a
  clear "done" state.

Example `args` payload for a roadmap_item:
```json
{"feature_prompt": "Add a CSV-export button to the reports page that POSTs to /api/export and saves to ~/Downloads. Include a Playwright test."}
```

### `whole_improve_loop`

- **Path**: `examples/whole_improve_loop/main.bot` (formerly
  `vibe_review_alternating`).
- **Vars**: `workspace_dir` (default), `scope_notes: string=""`
  — free-text steering ("focus on auth and persistence",
  "ignore the studio"); `improvement_prompt: string=""` —
  optional axis override. Empty (default) uses the full
  production-ready grid; non-empty REPLACES the grid as the
  reviewer/fixer focus (e.g. `"Focus exclusively on
  observability: structured logs, traces, metrics, lost error
  signals."`).
- **Pipeline**: alternating Claude/GPT review → fix loop until
  two consecutive cross-family approvals (max 15 iterations).
  No auto-commit — edits land in the working tree, operator
  commits.
- **Budget**: 1 branch, 2h, $60.
- **Use when**: existing code, operator wants rigorous
  production-readiness audit across the whole codebase, or
  wants to drive iterative improvement on a specific axis.

### `branch_improve_loop`

- **Path**: `examples/branch_improve_loop/main.bot`.
- **Vars**: `workspace_dir` (default), `scope_notes: string=""`,
  `base_ref: string="main"` — branch comparison base. The
  bot reviews the diff `git diff base_ref...HEAD` only.
- **Pipeline**: same alternating Claude/GPT review/fix loop as
  `whole_improve_loop`, but scoped to the branch footprint;
  on cross-family convergence, `prepare_commit` + the
  `commit_changes` tool write a semantic commit covering the
  improvements applied. Runs in a `worktree: auto` so the
  operator's main checkout is shielded; the worktree
  finalizer creates `iterion/run/<name>` and fast-forwards
  the current branch.
- **Budget**: 1 branch, 2h, $60.
- **Use when**: an existing branch/PR needs a rigorous
  production-readiness review + fix + commit before merge.
  Pass `--var base_ref=develop` (or another integration
  branch) when reviewing against a non-main base.

### `secured-renovacy`

- **Path**: `examples/secured-renovacy/main.bot` (or packed
  `examples/secured-renovacy.botz`).
- **Vars**: `scope: "patch"|"minor"|"patch,minor,major"`,
  `max_packages_per_run`, `major_policy:
  "skip"|"gate"|"attempt"`, `update_scope`. **Ask before
  running with `major_policy: "attempt"`**.
- **Budget**: 4 branches, 12h, $100, 500 iter, 5M tokens.
- **Use when**: dependency risk is the priority; CVE alerts;
  stale lockfiles.

### `sec-audit-source`

- **Path**: `examples/sec-audit-source/main.bot` (or packed
  `examples/sec-audit-source.botz`).
- **Vars**: `workspace_dir` (default `${PROJECT_DIR}`),
  `severity_threshold: "low"|"medium"|"high"|"critical"` (skip
  findings below this on the board), `scope_notes` (optional
  free-text steering hint).
- **Pipeline**: `detect_tech` (claw, readonly) → fan_out_all
  scanners (gitleaks + trivy + semgrep auto always; semgrep+gosec
  if Go; semgrep+bandit if Python; semgrep JS/TS profile if JS) →
  `triage` agent normalises raw output against
  `[[finding-taxonomy]]` and consults
  `.iterion/security/fp-known.yaml` for curated FP suppression →
  two-phase `revalidate` judge (anti-façade) → `report_card`
  (claude_code, board.create + board.label) writes one kanban
  issue per surviving finding plus a markdown summary at
  `.iterion/security/findings.md`. Cross-run FP memory committed
  in repo.
- **Budget**: 4 branches, 2h, $25 (typical).
- **Use when**: security audit of the source itself (SQL/cmd
  injection, SSRF, IDOR, broken auth, hardcoded secrets, crypto
  misuse, deserialisation, path traversal, misconfig);
  pre-release hardening; PR-scope security review.

### `sec-audit-deps`

- **Path**: `examples/sec-audit-deps/main.bot` (or packed
  `examples/sec-audit-deps.botz`).
- **Vars**: `workspace_dir`, `severity_threshold` (default
  `medium`), `cache_ttl_days` (default `30`).
- **Pipeline**: `enumerate_deps` (claw, readonly) walks
  `node_modules` / `.venv` / `vendor/` + lockfiles → fan_out_all
  per-ecosystem heuristic tool nodes (`run_js_heuristics`,
  `run_py_heuristics`, `run_go_heuristics`,
  `run_generic_heuristics`) emit structured signals (install
  hooks, eval-on-import, obfuscation, typosquat, vuln-db hits) →
  `load_cache` + `filter_cached` skip packages already analysed
  at acceptable scanner version within TTL — host-wide cache at
  `~/.iterion/security-cache/packages.jsonl` shared across all
  repos → `llm_review` (claude_code, board.create + board.label)
  validates signals against package source, computes
  `max(heuristic_score, llm_score)`, buckets LOW/MEDIUM/HIGH,
  creates one kanban issue per MEDIUM+ finding and writes
  `.iterion/security/deps-findings.md` → `update_cache` appends
  fresh JSONL line per analysed package.
- **Budget**: 4 branches, 2h, $25 (typical).
- **Use when**: post-`npm install` / `pip install` / `go mod
  download` triage; CVE supply-chain audit; suspicion of
  install-time malware (preinstall hooks, eval on import,
  typosquats); periodic baseline scan of vendored deps.

### `doc-align`

- **Path**: `examples/doc-align/main.bot` (or packed
  `examples/doc-align.botz`).
- **Vars**: none required. Optional: `doc_globs` (default
  `README.md,docs/**/*.md,CLAUDE.md`), `go_comment_globs`
  (default empty — opt in to comment audits),
  `code_scope_globs`, `coverage_target_pct` (default `80`),
  `diff_since` (hint only — recently-changed code files to
  prioritise), `bundle_self_path` (auto-exclude when the bot
  is auditing its own source tree).
- **Pipeline**: deterministic `scan_docs` enumerates the doc
  footprint once → alternating Claude/GPT review/fix on docs
  only (fixer is forbidden to touch code logic — `.md` files
  + Go comments only) → `prepare_commit` + `commit_changes`.
  A mechanical coverage gate (`cumulative_audited_docs /
  doc_count ≥ coverage_target_pct`) is baked into
  `streak_check` to prevent early "approved" on partial
  audits.
- **Budget**: 1 branch, 2h, $60 (typical).
- **Use when**: README / CLAUDE.md / `docs/**/*.md` / bundled
  skills are stale vs the code; before a release when the
  docs must reflect what shipped; whenever `repo-survey`
  flags drift between what the code does and what the docs
  claim.

## Issue-creation mapping (consumed by `emit_action`)

Each `roadmap_item` lands on the native kanban board as one
issue. The data model on the wire is:

| `roadmap_item` field | Native tracker field | CLI flag (today) |
|---|---|---|
| `title`              | `title`              | `--title`        |
| `body`               | `body`               | `--body`         |
| `assignee`           | `assignee`           | `--assignee`     |
| _(bot name, e.g. `feature_dev`)_ | `bot` (string)       | _no CLI flag — REST only_ |
| `args` (object)      | `bot_args` (`map[string]string`) | _no CLI flag — REST only_ |

`bot` and `bot_args` are dedicated typed fields on
[`native.Issue`](../../../pkg/dispatcher/native/issue.go) (JSON
keys `bot`, `bot_args`); they are NOT stored under the freeform
`Fields` map. The `iterion issue create/update` CLI does not yet
expose `--bot` / `--bot-arg` flags — until they ship, set those
fields through the REST API (POST/PATCH `/api/v1/native/issues`
with `{ "bot": "...", "bot_args": { ... } }`) or via direct
`store.Create/Update` calls. `bot_args` is usable today: the
dispatcher merges it on top of the rendered `dispatch.vars`
key-by-key, with `bot_args` winning on shared keys (see
[pkg/dispatcher/loop.go](../../../pkg/dispatcher/loop.go) `buildSpec`,
lines 276-296). `bot` is persisted and resolved into the dispatch
request, but the stock `EngineRunner` does not consume it for
workflow switching; use `assignee_workflows:` for real stock bot
routing.

Concrete `bot_args` example — for an issue assigned to
`feature_dev` with `args = {"feature_prompt": "Add CSV export"}`:

```json
{
  "title": "Add CSV export",
  "assignee": "feature_dev",
  "bot": "feature_dev",
  "bot_args": { "feature_prompt": "Add CSV export" },
  "labels": ["horizon:next-action", "source:whats-next"]
}
```

Horizon labels:

```
horizon=next_action  → --label horizon:next-action --label source:whats-next
horizon=short_term   → --label horizon:short-term --label source:whats-next
horizon=long_term    → --label horizon:long-term --label source:whats-next
```

Operators driving routing only through the CLI today should set
`--assignee <bot_name>` and rely on `assignee_workflows:` /
`assignee_dispatch:` in the dispatcher YAML to map that assignee
to a workflow + var template — see
[docs/dispatcher.md §Routing by issue assignee](../../../docs/dispatcher.md).

## Verification ritual (emit_action)

Before creating each issue:

1. If `assignee != ""`, look it up in the table above. If it's
   not one of the seven known bots (`feature_dev`,
   `whole_improve_loop`, `branch_improve_loop`,
   `secured-renovacy`, `sec-audit-source`, `sec-audit-deps`,
   `doc-align`), AND it doesn't correspond to a `.bot` file the
   explorer surfaced — strip to `""` and add label
   `needs-manual-triage`. NEVER invent.
2. Empty assignee is FINE. The issue lands without an assignee
   and the operator triages.

## What you do NOT do

- You do NOT shell out `iterion run …` directly. The bot used
  to do that; it doesn't anymore.
- You do NOT enumerate bots from the user's free-text alone.
  Walk the decision tree against the explore summary.
- You do NOT recommend an `assignee` whose `.bot` file the
  explorer did not surface.
- You do NOT recommend more than one `next_action`.
