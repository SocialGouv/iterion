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
`type:*`, `source:revi`) plus a markdown report. `review_tier` sits above this knob:
`audit` **forces dual** whatever `review_mode` says, and `glance` routes to a
cheaper same-family pair of reviewer nodes.

```
diff_precheck (tool)   empty diff → done (nothing to review)
diff_precheck -> tier_expand (compute, no LLM: resolves severity_threshold,
                              max_findings, post_to_board and the effective
                              review mode from review_tier)
tier_expand -> topology (condition)
  ├─ mono, guard tier (DEFAULT) -> reviewer_claude / reviewer_gpt
  │                                claude-opus-5 / openai/gpt-5.5
  ├─ mono, glance tier          -> reviewer_claude_glance / reviewer_gpt_glance
  │                                claude-sonnet-5 / openai/gpt-5.4-mini
  └─ dual (review_mode=dual, or the audit tier, which forces it)
                                -> fan (fan_out_all) -> both reviewers
reviewer_* -> merge_reviews (best_effort) -> converge (merge + dedupe → board + report)
converge -> pr_gate    deterministic: was a pr_url given?
  ├─ no  -> done
  └─ yes -> publish_review   deterministic forge publish via the iterion
            -> publish_health  server endpoint (inline comments, suggestions,
                               and optional merge-gate status) -> done
```

## Scope

Reviewers audit `git diff $(git merge-base <base> HEAD)` — the
**working-tree** diff against the merge-base, so both committed and
uncommitted branch changes are reviewed.

`<base>` is **refreshed from the remote before the merge-base is taken**:
`diff_precheck` runs `git fetch --quiet origin <base_ref>` and, when that
succeeds, merge-bases against `FETCH_HEAD` rather than the local ref. A
workspace reused across runs otherwise carries whatever `base_ref` meant when
it was made, and a merge-base against a stale base lands *below* the branch
point — every file merged into the base since would enter the diff and be
reviewed as if the branch had touched it. The base resolved this way is
published once as `base_sha` and every consumer reads that sha. If the remote
is unreachable the local ref is used and `base_is_current: false` is reported,
so the reviewers say the scope may be wide instead of failing the run.

To review **only** the uncommitted working tree, run with
`--var base_ref=HEAD` — nothing is fetched in that mode.

## Inputs

All inputs are workflow `vars` (override with `--var name=value`):

> `review_tier` is a preset, not a cage: every knob it resolves stays
> individually overridable with its own `--var`, and the deterministic
> `tier_expand` step records the resolved values in the run.

| Var | Default | Description |
|---|---|---|
| `workspace_dir` | `${PROJECT_DIR}` | Repo to review (the run's workspace). |
| `base_ref` | `main` | Ref to diff against (`merge-base(base_ref, HEAD)` vs working tree). `HEAD` = uncommitted only. |
| `review_tier` | `guard` | ONE preset for a repo's criticality / budget policy: `glance` \| `guard` \| `audit`. The deterministic `tier_expand` node resolves the defaults of `severity_threshold`, `max_findings`, `post_to_board` and `review_mode` from it, and it picks the reviewer models — `glance` is frugal (claude-sonnet-5 / gpt-5.4-mini), `guard` is the default and byte-identical to the pre-tier posture, `audit` is exhaustive and forces dual. Pinnable per repo through the integration's `launch_vars`. See [Review tiers](../../docs/merge-gate.md#review-tiers). |
| `scope_notes` | `""` | Free-text steering passed to the selected reviewer(s). |
| `prior_pushback` | `""` | What a fixer already did with an EARLIER review of this same PR, per finding id: fixed (with the commit), contested (with its argument), or deferred. Stamped at launch by the engine when such a run exists; empty is the normal case. This is what keeps a review↔fix pair converging instead of oscillating — a contested finding returns only against NEW evidence. It is **not** an instruction to drop it: a wrong argument must be answered, and a finding that is still real still counts against the gate. |
| `severity_threshold` | `auto` | Findings below this severity are never written (low < medium < high < critical). `auto` (the default) lets the tier pick the floor: **medium** on `guard`, `high` on `glance`, `low` on `audit`. Any concrete value is an explicit override that wins over the tier. Keep the EFFECTIVE value at or below `gate_severity`, or the required check stops seeing what it gates on. |
| `max_findings` | `0` | Cap on issues/rows (highest severity first); a capped run says so. `0` (the default) is a sentinel: the tier picks the cap — **15** on `guard`, 5 on `glance`, 40 on `audit`. Any positive value is an explicit override. |
| `post_to_board` | `auto` | File findings on the native board — a string enum (`auto` \| `true` \| `false`), not a bool. `auto` (the default) resolves from the tier: `false` on `glance` (a quick signal has no business filing board issues), `true` on `guard`/`audit`. `false` = report only. |
| `report_path` | `.review-pr/findings.md` | Markdown report destination (gitignorable; not under `.iterion/`). |
| `pr_url` | `""` | When set, ALSO publish the review onto this PR (see below). Empty = board + report only. |
| `pr_review_mode` | `inline` | How the PR review is posted: `inline` (per-line comments) or `summary` (one comment). |
| `review_mode` | `mono` | Reviewer topology: `mono` (default), `dual`, or `auto` (launch-time resolver also resolves to mono). |
| `mono_family` | `claude` | Family used in mono mode: `claude` or `gpt`. |
| `gate_enabled` | `true` | Ask the server to post a deterministic commit-status gate with the review. |
| `gate_severity` | `high` | Lowest finding severity that makes the gate fail. |
| `gate_context` | `revi/review` | Commit-status context; use a shared context when another bot gates different PRs in the same repo. |
| `ticket_context` | `auto` | Ticket-conformance source: `auto` picks the EXTERNAL tracker when `tracker_api_base` is set, else the FORGE-NATIVE one from `pr_url` (the forge is asked which issues the PR closes — GitLab `closes_issues`, GitHub `closingIssuesReferences` — falling back to a text scan). `off` disables the check; a PR with no ticket is never a finding either way. |
| `tracker_api_base` | `""` | Base URL of an EXTERNAL tracker API. Setting it selects external mode for `ticket_context`. |
| `tracker_user` | `""` | Identity used against `tracker_api_base`. |
| `ticket_refs` | `""` | Explicit ticket references for this review, when neither the forge nor the PR text carries them. |
| `source_branch` | `""` | The PR's head branch name, injected by the webhook lanes on every PR launch (empty on a bare CLI run); used to recover ticket references embedded in the branch name (`feature/PROJ-123-…`). |
| `forge_publish_url` / `forge_publish_token` | `""` | Stamped by the SERVER at launch — the endpoint and the ephemeral per-run token `publish_review` posts through. Never set these by hand; see "Publish onto a forge PR" below. |

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
  The LLM never chooses the gate result. The gate **fails closed** on the two
  shapes where that count means nothing: a scope that never resolved
  (`diff_precheck` could not read a diff — zero findings out of zero files
  read is not an approval) and reviewer output that was not machine-readable.
  Both post `failure` carrying a `note` that replaces the status description,
  so the check names the real reason instead of pointing at a blocking finding
  that was never published. The status is advisory until the repo
  requires that context in branch protection; see
  [Merge gate](../../docs/merge-gate.md).

## Read-only by construction

No node mutates source: `reviewer_claude` and `reviewer_claude_glance` are
`readonly: true` (Write/Edit removed, Read/Grep/Bash kept for `git diff`), and
`reviewer_gpt` / `reviewer_gpt_glance` are given only read tools (`bash`,
`read_file`, `glob`, `grep` — no `write_file`/`file_edit`). The single downstream `converge` step writes
only the report file and creates board issues over MCP.

See [main.bot](main.bot) for the full DSL.

## Layout — a bot in several files

`main.bot` holds the header, the vars, the secrets, the supervisor and the workflow; the
rest lives beside it under `lib/` and is reached through the `import` lines at the head of
the main — `lib/schemas.bot` (the schemas), `lib/prompts.bot` (the prompts), `lib/nodes.bot`
(the nodes). The four files are ONE program: `iterion validate`, `run`, the studio and a
remote launch read the unit; the manifest's `requires.iterion` names the release that reads
`import`. See docs/dsl.md, "import — a bot in several files".
