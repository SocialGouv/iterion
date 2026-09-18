# review_pr — Revi

Reviews open with one sentence when no issue is retained, or a compact count
when findings remain. Each inline finding states the trigger, impact and fix
in 2–3 sentences where possible, without truncating evidence or replacement
code. The summary shows only ticket gaps and questions that need a maintainer
decision. Unverifiable ticket context, run telemetry, the reviewed scope, topology
and fixer instructions stay in the collapsed details. If an anchor is missing
or stale, the complete finding and replacement remain readable in the summary.
Editorial brevity does not change severity thresholds, finding caps or gates.

Read-only **code reviewer** with an optional cross-family dual mode. Revi
reviews the changes on the current branch and *publishes* the findings — it
never edits, fixes, or commits code. (Fixing is the improve-loops' job: `branch_improve_loop` =
Billy, `whole_improve_loop` = Willy.)

Revi runs **one reviewer by default** (`review_mode: mono`, Claude unless
`mono_family` selects GPT). Set `review_mode: dual` to run independent Claude
and GPT reviewers in parallel when cross-family confirmation is worth the
extra provider call. A single `converge` step normalises the one or two reviewer
outputs, merges and de-duplicates findings, and raises the confidence of
anything both families flagged ("cross-confirmed"). It then writes one issue
per finding to the iterion native kanban board (labelled `severity:*`,
`type:*`, `source:revi`) plus a markdown report.

```
diff_precheck (tool)   empty diff → done (nothing to review)
diff_precheck -> topology (condition)
  ├─ mono/claude -> reviewer_claude   claude_code, read-only
  ├─ mono/gpt    -> reviewer_gpt      claw + openai/gpt-5.5, read-only tools
  └─ dual        -> fan (fan_out_all) -> both reviewers
reviewer_* -> merge_reviews (best_effort) -> converge (merge + dedupe → board + report)
converge -> pr_gate    deterministic: was a pr_url given?
  ├─ no  -> done
  └─ yes -> publish_review   deterministic forge publish via the iterion
            -> publish_health  server endpoint (inline comments, suggestions,
                               and optional merge-gate status) -> done
```

## Scope

Reviewers audit `git diff $(git merge-base {{base_ref}} HEAD)` — the
**working-tree** diff against the merge-base, so both committed and
uncommitted branch changes are reviewed. To review **only** the
uncommitted working tree, run with `--var base_ref=HEAD`.

## Inputs

All inputs are workflow `vars` (override with `--var name=value`):

| Var | Default | Description |
|---|---|---|
| `workspace_dir` | `${PROJECT_DIR}` | Repo to review (the run's workspace). |
| `base_ref` | `main` | Ref to diff against (`merge-base(base_ref, HEAD)` vs working tree). `HEAD` = uncommitted only. |
| `scope_notes` | `""` | Free-text steering passed to the selected reviewer(s). |
| `prior_pushback` | `""` | What a fixer already did with an EARLIER review of this same PR, per finding id: fixed (with the commit), contested (with its argument), or deferred. Stamped at launch by the engine when such a run exists; empty is the normal case. This is what keeps a review↔fix pair converging instead of oscillating — a contested finding returns only against NEW evidence. It is **not** an instruction to drop it: a wrong argument must be answered, and a finding that is still real still counts against the gate. |
| `severity_threshold` | `low` | Drop findings below this (low < medium < high < critical). |
| `max_findings` | `40` | Cap on issues/rows (highest severity first); a capped run says so. |
| `post_to_board` | `true` | File findings on the native board; `false` = report only. |
| `report_path` | `.review-pr/findings.md` | Markdown report destination (gitignorable; not under `.iterion/`). |
| `pr_url` | `""` | When set, ALSO publish the review onto this PR (see below). Empty = board + report only. |
| `pr_review_mode` | `inline` | How the PR review is posted: `inline` (per-line comments) or `summary` (one comment). |
| `review_mode` | `mono` | Reviewer topology: `mono` (default), `dual`, or `auto` (launch-time resolver also resolves to mono). |
| `mono_family` | `claude` | Family used in mono mode: `claude` or `gpt`. |
| `gate_enabled` | `true` | Ask the server to post a deterministic commit-status gate with the review. |
| `gate_severity` | `high` | Lowest finding severity that makes the gate fail. |
| `gate_context` | `revi/review` | Commit-status context; use a shared context when another bot gates different PRs in the same repo. |
| `fixer_hint` | *(empty)* | Sentence published with the findings when this repo has a fixer to escalate to. Empty omits the line: the reviewer does not advertise a fixer on its own initiative. `{finding}` is replaced by the first finding's id. |

## Run

```bash
iterion run bots/review-pr/main.bot \
  --var workspace_dir=/path/to/repo \
  --var base_ref=main

# Explicitly pay for both reviewer families:
iterion run bots/review-pr/main.bot \
  --var base_ref=main \
  --var review_mode=dual
```

Findings land on the native board under the label `source:revi`; the
markdown report is written to `report_path`. Surface a run's output with
`iterion report --run-id <id>`.

## Publish onto a forge PR (`--var pr_url=…`)

Give Revi a pull-request URL (merge request on GitLab) and it ALSO posts its findings onto
that PR as a real forge review — one inline comment per finding anchored
to `file:line`, with a one-click ` ```suggestion ` block when the
finding carries a concrete replacement, plus a summary comment. The
board + report still run; the PR review is additive.

The review ends with a collapsed **Détails du run IA · Iterion** block. It
shows the run id, linked to the instance's normal run page (login and run access
rights still apply), and cumulative token total at publication, plus each executed
review/synthesis node's served model, harness, requested reasoning effort and
tokens. Values come from engine metadata; absent telemetry is marked unavailable.
Effort follows the node's `ITERION_VIBE_EFFORT_*` setting/default and describes
the requested level, not a provider measurement. Token counts accumulate calls,
including repeated context; they are not the session's context-window size.

```bash
# Check out the PR's branch locally, then:
iterion run bots/review-pr/main.bot \
  --var workspace_dir=/path/to/repo \
  --var base_ref=main \
  --var pr_url=https://github.com/owner/repo/pull/42
```

- **Deterministic + tokenless-in-workspace.** `publish_review` is a tool
  node (no LLM): it POSTs the findings to the iterion server's
  `POST /api/v1/forge/publish-review` endpoint, authenticated by an
  ephemeral per-run token the server injects at launch as the
  `forge_publish_url` / `forge_publish_token` vars. The SERVER posts the
  review through the team forge connection's live client (a GitHub App
  connection mints a fresh installation token per call), so no forge
  credential ever sits in the run's workspace and a token can't expire
  mid-run. Forge-agnostic: the endpoint dispatches by the connection's
  provider (GitHub / GitLab / Forgejo-Gitea); no forge names in the
  workflow.
- **Diff model (v1).** Revi reviews the LOCAL checkout (`base_ref..HEAD`)
  and publishes to `pr_url`; check out the PR branch and pass its base
  as `base_ref`. Auto-resolving base/head from the URL is a planned
  enhancement.
- **Anti-façade.** The endpoint re-fetches the posted review to count the
  comments the forge actually stored (falling back to a summary-only
  review when inline anchors are rejected — findings are folded into the
  body, never dropped), and a deterministic `publish_health` gate raises
  a loud banner if findings existed but zero inline comments landed — the
  board + report still succeed, so fix the forge connection and re-run
  with the same `pr_url`.
- **Stale-review guard.** The inline findings anchor to `file:line`
  positions computed against the tree the reviewers judged — the worktree
  HEAD `diff_precheck` captured as `reviewed_sha`. `publish_review`
  re-reads HEAD before posting and, if it has moved, **refuses to
  publish** rather than landing comments on lines that have since shifted;
  it emits a `skipped` reason telling you to re-run with the same `pr_url`
  for a fresh review. Within a single run Revi is read-only, so HEAD never
  drifts between the two nodes — the guard bites on a **resume** after the
  branch advanced (and any future mid-run mutation). Fail-open: it fires
  only when both SHAs read cleanly and differ, so a normal publish is
  never blocked, and the board + report already ran, so no findings are
  lost.
- **Deterministic merge gate.** With `gate_enabled: true`, the publish node
  counts findings at or above `gate_severity` and asks the server to post
  `gate_context` on the PR head (`success` for zero, `failure` otherwise).
  The LLM never chooses the gate result. The status is advisory until the repo
  requires that context in branch protection; see
  [Merge gate](../../docs/merge-gate.md).

## Read-only by construction

No node mutates source. `reviewer_gpt` is given only read tools (`bash`,
`read_file`, `glob`, `grep` — no `write_file`/`file_edit`), which on `claw` is
the whole surface it has. The claude reviewers declare `readonly: true`, which
is a **scheduling** declaration — "this node mutates no workspace file", the
promise that lets dual fan both reviewers onto one worktree. Only `pi` and
`codex` read it as a tool boundary; on `claude_code` nothing does, so what
bounds those nodes is their prompt and their output schema. Declaring `tools:`
on them would bound them for real and is not free: a non-empty list installs
`--disallowedTools`, taking `Task`/`WebFetch`/`Skill` off the node on every
run. The single downstream `converge` step writes only the report file and
creates board issues over MCP.

## When the Anthropic credential cannot serve

The claude slot is not a dead end. Both its nodes declare a fallback route to
**GLM** — the claude family's other server — as a `provider: "zai"` hint with
no `backend:` of its own. The route therefore stays on `claude_code`, the one
backend that honours a provider hint: the hint forces the z.ai facade
(`ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` from the run's `zai` credential,
or `ZAI_API_KEY`) **even when Anthropic credentials are present**, which is
exactly the tenant whose forfait window is spent. It is armed for three failure
categories:

| category | the instance shape that produces it |
|---|---|
| `usage_window` | an Anthropic credential that **outranks** z.ai in the CLI's precedence, whose window is spent. This route answers the *provider's* refusal, mid-run — the operator's own cap, when it is already shut at launch, refuses the whole run before any node dispatches and the route is never consulted. A cap that shuts *during* the run does divert here, metered key and all |
| `auth` | an Anthropic credential present but rejected or expired. It still takes precedence, so the CLI uses it and 401s. Not in the default trigger set; named here deliberately |
| `unavailable` | the resolved credential cannot reach the model it was given |

The rescue model is a dial like every other on those nodes:
`${ITERION_VIBE_MODEL_CLAUDE_FALLBACK:-glm-5.2}`, one for both tiers since the
rescue is deliberately the same on each. An account entitled to a different GLM
id — or one z.ai renames — repoints it without re-releasing the bundle, live
through `bot-vars` like the other `ITERION_*` dials.

**An instance with only a z.ai key is not in that table:** `claude_code`
resolves z.ai by itself — the BYOK z.ai pair is the first case of its credential
precedence — so the primary is already credentialed and the route is not what
makes that host work. Give it the dial instead: set `ITERION_VIBE_MODEL_CLAUDE`
(and `_GLANCE`) to the GLM id the account serves. Left at `claude-opus-5` the
request still reaches z.ai's gateway, which aliases the model internally — the
review runs, but the run table then names a model that did not serve it, and a
gateway that refuses the id instead sends the node down the `unavailable`
trigger once per run. The route is the net, not the configuration.

With no credential at all the route has none either, and the run fails loudly.

The route is `metered: true`. That is a declaration of intent, not a governor:
nothing in the executor reads it (ADR-087's `ITERION_FORBID_METERED_FALLBACK`
is prose, not code). On a forfait instance it does spend a billed key where the
primary spent a subscription; on a BYOK z.ai instance both are the same billed
key.

One residual, engine-side: with the `zai` hint and **no** z.ai key reachable,
the CLI's ambient `ANTHROPIC_API_KEY` is not cleared, so the route asks
Anthropic for a GLM id and gets a 404 instead of a clean "no credential". It
fails rather than mis-spending, so it bounds what the route buys rather than
undoing it.

Nothing about the degradation is silent. `_backend` and `_model` name what
actually served; the published review's run table carries them
(`ai_reviewer_claude_*`, or `ai_reviewer_claude_glance_*` on the glance tier);
and the **merge-gate status itself says so** — the gate reads the engine's
`_fallback_used` stamp and writes a note naming the model that ran. Advisory,
not fail-closed, and deliberately: the gate's three fail-closed branches are
*non-reviews* (no diff read, output unreadable, merge step lost) where green
would approve by omission, whereas a fallback-served review read the same diff
and emitted the same schema. Blocking it would turn the rescue into a merge
block on exactly the runs it exists to keep reviewing.

The GPT nodes have no equivalent route; their model and backend stay steerable
by their own `ITERION_VIBE_*_GPT` / `ITERION_VIBE_*_GPT_GLANCE` dials.

See [main.bot](main.bot) for the full DSL.

## Layout — a bot in several files

`main.bot` holds the header, the vars, the secrets, the supervisor and the workflow; the
rest lives beside it under `lib/` and is reached through the `import` lines at the head of
the main — `lib/schemas.bot` (the schemas), `lib/prompts.bot` (the prompts), `lib/nodes.bot`
(the nodes). The four files are ONE program: `iterion validate`, `run`, the studio and a
remote launch read the unit; the manifest's `requires.iterion` names the release that reads
`import`. See docs/dsl.md, "import — a bot in several files".
