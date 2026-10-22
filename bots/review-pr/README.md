# review_pr — Revi

Reviews open with one sentence when no issue is retained, or a compact count
when findings remain. Each inline finding states the trigger, impact and fix
in 2–3 sentences where possible, without truncating evidence or replacement
code. The summary shows only ticket gaps and questions that need a maintainer
decision. Unverifiable ticket context, run telemetry, the reviewed scope, topology
and fixer instructions stay in the collapsed details. If an anchor is missing
or stale and the summary note lands, the complete finding and replacement remain
readable in the summary; a missing summary is reported as degraded instead.
Editorial brevity does not change severity thresholds, finding caps or gates.

Read-only **code reviewer** with an optional cross-family dual mode. Revi
reviews the changes on the current branch and *publishes* the findings — it
never edits, fixes, or commits code. (Fixing is the improve-loops' job: `branch_improve_loop` =
Billy, `whole_improve_loop` = Willy.)

Revi runs **one reviewer by default** (`review_mode: mono`, Claude unless
`mono_family` selects GPT). Set `review_mode: dual` to run independent Claude
and GPT reviewers in parallel when cross-family confirmation is worth the
extra provider call. One `converge` step normalises the one or two reviewer
outputs, merges and de-duplicates findings, and raises the confidence of
anything both families flagged ("cross-confirmed"). It then writes one issue
per finding to the iterion native kanban board (labelled `severity:*`,
`type:*`, `source:revi`) plus a markdown report. `mono_family` picks the
family of the WHOLE review: under `mono_family=gpt` the merge runs on
`converge_gpt`, the same step on the GPT route, and the `pacer` supervisor is
the GPT one — a mono-GPT review touches no Anthropic credential with no
configuration at all.

**Every reviewer route is a `${ENV_VAR:-default}` dial**, so an instance
points a slot at whatever its credentials serve without re-releasing the
bot: the claude slot defaults to `claude-opus-5-5` on `claude_code` (both
tiers — the catalog ships no other Anthropic default; glance's frugality is
its reasoning effort and tier caps), the gpt slot to `openai/gpt-6-astra` (glance twin:
`openai/gpt-6-luna`) on `claw`, the gpt merge twin to `openai/gpt-6-sol` on
`claw`, and every claude node — both reviewers and
the `converge` merge step — carries a GLM fallback route (`provider: "zai"`,
dial `ITERION_VIBE_MODEL_CLAUDE_FALLBACK`, default `glm-5.3`) for the day the
Anthropic credential cannot serve. The published run table labels each row
with the family that actually served — read from the session's routing label
first (a z.ai facade is GLM, whatever id it was sent), then from the model —
never from the slot's name: Revue Claude / Revue GPT / Revue GLM, and
Synthèse · <family> for the merge step. Only the family is published, never
the routing label. The routing label exists on claude_code sessions; a claw
node on a `session: fresh` carries none, so its row falls back to the model
id. Known limit: the `pacer` supervisor resolves in-process on claw, where a
skipped forfait leaves it no Anthropic credential of its own — at the cap its
evaluations fail; it publishes nothing. (`pacer_gpt`, which watches the gpt
reviewer, has its own route for the same reason the merge twin does.)

```
diff_precheck (tool)   empty diff → done (nothing to review)
diff_precheck -> topology (condition)
  ├─ mono/claude -> reviewer_claude   claude_code + opus 5.5, read-only
  ├─ mono/gpt    -> reviewer_gpt      claw + openai/gpt-6-astra, read-only tools
  └─ dual        -> fan (fan_out_all) -> both reviewers
reviewer_* -> merge_reviews (best_effort)
merge_reviews -> converge_gpt   when mono_family == gpt   (claw + openai/gpt-6-sol)
merge_reviews -> converge       otherwise                 (claude_code + opus 5.5)
                                merge + dedupe → board + report
converge* -> pr_gate   deterministic: was a pr_url given?  (the one node both
                       merge twins feed: it carries the merged payload AND the
                       merge's run telemetry forward)
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
- **Workspace anomaly (#1666).** The workspace must equal the tree under
  review. `diff_precheck` reports any TRACKED uncommitted entry
  (`git status --porcelain -z`, untracked scratch and engine/tooling tree
  noise excluded — neither enters the review's diff scope) as
  `workspace_anomaly`; the reviewers scope those files out WHOLE
  (committed hunks included), and `publish_review` strips any finding
  still anchored on a drifted file from the PR findings and reports it
  under a "workspace anomaly" section naming the file and the difference
  — an edit the PR does not contain is workspace state, never a finding
  against the change under review. A finding is routed on its ANCHOR,
  compared WHOLE with an entry (its rendering normalized first, to a
  fixpoint: `./path`, `/path` and this workspace's own absolute path are the
  same file), and a KEPT finding is published under that same normalization
  — the forge anchors an inline comment on a repo-relative path, and one
  rooted rendering 422s a create-review call into its
  fold-everything-into-the-body fallback, costing the review every inline
  comment. (`finding_id` stays on the raw anchor: the engine re-derives it
  for the fixer's digest from the path as written, and has no workspace path
  to strip.) A diff file header renders the same path as `a/path` / `b/path`
  and the prompt orders the reviewer to read that diff, so those two are
  normalized as well — but against the WORKSPACE, not blindly: the prefix is
  dropped only where the workspace does not hold the prefixed path and does
  hold (or reports uncommitted) the stripped one, so a repo owning a
  top-level `a/` or `b/` keeps its own anchor on the channel that decides a
  merge. An anchor the workspace resolves neither way stays as written and
  422s as it did before: a path the change deletes, a rendering climbing out
  through `..`, an absolute path outside the workspace (an uncommitted
  deletion still resolves — its porcelain entry names it). The name-shaped
  match of the free-text channels below is deliberately not
  accepted on this channel: it cannot tell a drifted file from a namesake,
  and here that ambiguity would decide a merge — so a finding anchored on
  a bare basename stays the pull request's, loud, beside the anomaly
  section naming the drift. Reclassified findings stay visible:
  the workspace-anomaly section and the gate note always carry their
  count (a green gate never reads as approval by omission), and when the
  strip empties the finding list the review headline itself names the
  reclassification instead of claiming a clean review. A failed status
  read publishes the "?" marker: nothing is stripped and the review says
  the workspace state could not be verified.
  The three reviewer-authored free texts are routed the same way (#1674):
  a `questions` line, or a `ticket_conformance` verdict that would be
  charged to the PR as a gap, prints under the anomaly section as
  reclassified instead of under "À confirmer" / "Écarts au ticket" — a
  question about an edit the PR does not contain costs the author a
  written answer for someone else's workspace; a `scanned_areas` entry
  naming one prints under "Périmètre déclaré nommant un fichier non commité
  listé ci-dessus, donc non publié comme revu" instead of inside "Périmètre
  revu", which would claim an audit of a file rule 2d puts out of scope
  whole — the line says what the name match establishes and never that the
  file went unreviewed, which it cannot know (see the basename collisions
  below). A verdict claiming no gap is left alone: `covered` prints
  nowhere and `unverifiable` keeps its own list, so routing either would
  publish a ticket gap the reviewer never wrote. The verdict is read off
  the line's first field — the text up to its first colon, or the whole
  URL when the ref is rendered as one, which is how a PR body writes its
  `Closes` line; a reason mentioning `covered` after a later colon still
  reads as the gap it is.
  None of the three carries a file anchor, so the match is on the NAME:
  the entry's path — bare, or rooted as `./path`, `/path` or this
  workspace's own absolute path, the form the prompt hands the reviewer —
  or any of its segment suffixes that carries an extension or a directory
  (a reviewer names the file as prose does, by its basename; a bare
  extension-less segment like `changes` is never matched as a suffix).
  A rooted rendering has to OPEN its word — line start, or after a space,
  a bracket or a backtick — because `~/catalog.md` and `file:/catalog.md`
  name files outside this workspace (a quoted rendering is that set's
  residual). A whole path that is ONE extension-less segment is both a path
  and an English word (a root-level `build`, `test` or `run` script):
  carrying no uppercase letter it is matched only in a rooted rendering,
  since a bare one would hand every question using that word, and the axis
  token of every `scanned_areas` entry, to the anomaly; carrying a capital
  (`LICENSE`, `Makefile`, `Dockerfile`) it names the file in prose and is
  matched bare too.
  The boundaries are deliberately asymmetric: a longer or prefixed name
  (`catalog.mdx`, `my-catalog.md`, `catalog.md-old`) and a path merely
  ENDING with the entry's path (`vendor/<path>`, reachable 548 times in
  this tree) stay ordinary questions, while a neighbour named by its BARE
  basename is reclassified — 5056 of 13993 tracked files share a basename,
  so the published line claims only what the match establishes (the line
  names an uncommitted entry), never that the question is outside the PR's
  diff. A forge blob URL, whose path sits under an arbitrary ref, is
  indistinguishable from those sibling paths and stays a question. The match
  is case-sensitive and literal, so a name capitalised to open a sentence
  (`Iterion-bot-catalog.md`) and a file described without being named stay
  questions as well: rule 2d is the guard, this routing only its backstop,
  and both prompt sites say so rather than promising the routing is total.
  Scope: the sanitisation covers the FORGE publication (summary, inline
  comments, gate); the board/markdown report written upstream by
  `emit`/`converge` may still carry a drift-drawn finding or question a
  reviewer filed against rule 2d — and so may the prior-review digest a
  later fixer run starts from, which reads that same upstream merged set
  (through `pr_gate`'s artifact, whichever twin merged). Empty in
  `base_ref: HEAD` mode, where uncommitted work IS the review's object.
- **Anti-façade.** The endpoint re-fetches the posted review to count the
  comments the forge actually stored (falling back to a summary-only
  review when inline anchors are rejected — findings are folded into the
  body, never dropped), and a deterministic `publish_health` gate raises
  a loud banner if findings existed but zero inline comments landed. An
  HTTP 200 can also carry `body_missing=true` when inline comments landed
  but the summary note did not: this is `degraded=true`, with an explicit
  "review body did not land" banner and no claim that rejected anchors were
  folded. The board + report still succeed, so fix the forge connection and
  re-run with the same `pr_url`.
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

No node mutates source. `reviewer_gpt` runs on `claw` and is given only
read tools (`bash`, `read_file`, `glob`, `grep` — no
`write_file`/`file_edit`), so it can run git and read the workspace but has
no structured path to edit it.
The claude reviewers declare `readonly: true`, which
is a **scheduling** declaration — "this node mutates no workspace file", the
promise that lets dual fan both reviewers onto one worktree — and a tool
boundary, with the lever each backend has: `codex` an OS sandbox, `pi` a
readers-only tool list, `claude_code` the withholding of the file-writing
tools whatever the node's `tools:` list says — on the claude slots that
list is `bash`, `read_file`, `glob`, `grep` (since 0.9.17) — plus the
worktree-moving pair; `claw`, `opencode` and `cliagent` do not read
the flag at all, so there the `tools:` list is the only bound. It is not a
sandbox on `claude_code` either — the native shell stays — so their prompt
and their output schema still carry the rest. The declared list does bound
the roster on `claude_code` too: a non-empty list installs
`--disallowedTools`, taking `Task`/`WebFetch`/`Skill` off the node on every
run — and it is required anyway, because on `claw` an empty list is no
tools at all (SocialGouv/iterion#2705: a listless reviewer reviewed
nothing and greened the gate). The one downstream merge
step — `converge`, or `converge_gpt` on the GPT family — writes only the
report file and creates board issues over MCP. `converge_gpt` runs on `claw`,
where a `tools:` list is the node's WHOLE surface and an empty one is no tools
at all, so it declares the three the merge needs (`read_file` for the
iterion-board skill, `write_file` for the report, `glob`); the board
operations then ride that list as the `mcp__iterion_board__` names the prompt
calls, and the step is the same step on either route.

## The Anthropic pin, and the z.ai route below it

Every claude_code node on a claude id — both claude reviewers **and**
`converge` — pins its provider to `anthropic`, through a dial
(`ITERION_VIBE_PROVIDER_CLAUDE` for the claude slot, `ITERION_VIBE_PROVIDER_EMIT`
for the merge step; the `converge_gpt` twin carries no hint, because on claw
the model's `openai/` prefix is the route). The pin is **load-bearing**: the wire's credential order
(secrets.AnthropicWireSlotOrder) puts facade keys (zai, moonshot) FIRST, so a
z.ai key the run holds as a DEFAULT credential serves every unpinned node of
the wire, the forfait beside it or not. The team's own z.ai key is always one
(a team's keys fill before its forfait). A shared tier's is one only where the
forfait is off the wire — skipped because its window is closed (a provider
refusal, or an operator usage cap, soft or hard: a launch is a new run), or
never connected — and the tier's facade policy lets a facade key take a free
family: under `facade_default: auto`, the default, only in a tier that holds
no Claude credential of its own; under `always`, anywhere; under `never`,
nowhere ([cloud-llm-credentials.md](../../docs/cloud-llm-credentials.md)). An
unpinned node then sends its claude id to the z.ai facade, which **accepts it
and serves GLM in silence**, under the claude label — PR #1924's gpt slot did
exactly that. Pinned, the primary is forced Anthropic-direct, facade branches
skipped: the run's default `anthropic` key if it holds one, else the
`claude_code` forfait, else an `anthropic` key a shared tier sealed for the
pin alone. A default key wins over the forfait — the team's own, or a shared
tier's that took the family (the forfait closed at launch, or `keys_first` in
a tier holding both); a key an org or platform tier funds only for the pin,
beside the team's forfait, waits behind it, so the subscription's work stays
on the subscription.
When none is sealed it falls back to the runner's AMBIENT Anthropic auth
(`ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN` in the pod env) — with none,
as on iterion.cloud, it fails "Not logged in" (classified auth, no spend) and
the route below takes it; with one, the pod's account serves the node.

The fallback route to **GLM** is the second server of the same review family.
It is a `provider: "zai"` hint with no `backend:` of its own, so it stays on
`claude_code`, the one backend that honours a provider hint: the hint forces
the z.ai facade (`ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` from the run's
`zai` credential, or `ZAI_API_KEY`) **even when Anthropic credentials are
present**. The rescue rides `converge`'s own primary route, which a CLI
backend serves by construction: claw reads no hint and a bare GLM id is not a
claw spec. The merge step's `backend:` note bounds that dial to the CLI
backends for a second reason — pointed at claw, the node holds no tool at all
(below). It is armed for three failure categories — each a failure OF THE
PINNED ANTHROPIC ROUTE, never of a facade:

| category | the shape that produces it |
|---|---|
| `usage_window` | the forfait's usage window is spent mid-run — the weekly cap diverts the node to GLM instead of parking it. A spent z.ai key plays no part in the primary's health; it only makes this ROUTE fail, and when both are spent the run parks on its durable usage-window retry — there is no third server to fall to |
| `auth` | the Anthropic credential present but rejected or expired. Not in the default trigger set; named here deliberately. A run whose forfait the publisher skipped at the cap lands here too when the runner carries no ambient Anthropic auth: the forced-Anthropic primary has no channel, fails "Not logged in" (no spend), and this route takes it |
| `unavailable` | the resolved credential cannot reach the model it was given |

The rescue model is a dial like every other on those nodes:
`${ITERION_VIBE_MODEL_CLAUDE_FALLBACK:-glm-5.3}`, one for both tiers since
the rescue is deliberately the same on each. An account entitled to a
different GLM id — or one z.ai renames — repoints it without re-releasing
the bundle, live through `bot-vars` like the other `ITERION_*` dials.

With no credential at all the route has none either, and the run fails loudly.

The route is `metered: true`. That is a declaration of intent, not a governor:
nothing in the executor reads it (ADR-087's `ITERION_FORBID_METERED_FALLBACK`
is prose, not code). The route spends a billed key where the primary normally
spends a subscription.

With a `zai` hint and **no** z.ai key reachable, the route is refused before
the CLI spawns — `ErrNoFacadeCredential`, which names `zai` and `ZAI_API_KEY` —
with no spend. The engine strips every ambient Anthropic channel for a
`zai`-pinned node (env tokens, the forfait file channel, the alt-provider
switches), so it can never silently route to another provider: the fix is the
z.ai key.

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

The GPT nodes — both reviewer tiers and the `converge_gpt` merge twin — have
no fallback route of their own: their backend, model and provider are dials
(`ITERION_VIBE_BACKEND_GPT*`, `ITERION_VIBE_MODEL_GPT*`,
`ITERION_VIBE_PROVIDER_GPT`; `ITERION_VIBE_{MODEL,BACKEND,PROVIDER}_EMIT_GPT`
for the merge twin, which keeps its own trio because the merge model is not
the reviewer's), so an instance without a usable OpenAI route
points them at a family it can serve — all three together, or the slot lands
on a route nobody chose:

- the claude forfait: `claude_code` + `claude-opus-5-5` + provider
  `anthropic` (without the hint, a z.ai key holding the wire answers the
  claude id with GLM — the PR #1924 label);
- GLM: `claude_code` + `glm-5.3` + provider `zai`. Not claw +
  `anthropic/glm-5.3`: claw reaches z.ai only through a z.ai key sealed as
  the run's DEFAULT, which a sealed Claude forfait prevents.

See [main.bot](main.bot) for the full DSL.

## Running mono-GPT: the family of the whole review

`mono_family` is the family of the WHOLE review, not only of the reviewer
that judges. A launch carrying `mono_family=gpt` takes `topology ->
reviewer_gpt`, `merge_reviews -> converge_gpt`, and the `pacer_gpt`
supervisor; the claude reviewers, the claude merge step and the claude
`pacer` are on no route such a run takes. **No deployment profile is needed
for that** — not for the review, not for the merge, not for the pacer.

That is not a convenience: `mono_family` is resolved from the run's own
credentials and claude is the PREFERRED family
(`reviewtopology.monoPreference`), so a run carrying `gpt` holds no Claude
credential at all. A merge step pinned to the Anthropic wire there died
AFTER the reviewer had already spent, and the run never reached `pr_gate` —
the `revi/review` gate stayed dark on a pull request whose review had in
fact run. A webhook launch needs no `--var` for any of it: `SubmitLaunch`
resolves the family through `reviewtopology.InjectAll` into the run's launch
vars before publishing it (`pkg/server/cloudpublisher`), which is the
reading the usage-cap pre-flight folds
([usage-caps.md](../../docs/usage-caps.md#what-a-cap-does-not-stop)).

| route | no `mono_family=gpt` | `mono_family=gpt` | its own dials |
|---|---|---|---|
| reviewer | `reviewer_claude` — `claude_code` + opus 5.5 + `anthropic` | `reviewer_gpt` — `claw` + `openai/gpt-6-astra` | `ITERION_VIBE_{MODEL,BACKEND}_{CLAUDE,GPT}`, `ITERION_VIBE_PROVIDER_{CLAUDE,GPT}` |
| merge | `converge` — `claude_code` + opus 5.5 + `anthropic`, with the GLM rescue route below it | `converge_gpt` — `claw` + `openai/gpt-6-sol`, no hint, no rescue route (claw reads none) | `ITERION_VIBE_{MODEL,BACKEND,PROVIDER}_EMIT` / `…_EMIT_GPT` |
| pacer supervisor | `pacer` — `anthropic/claude-opus-5-5`, watching `reviewer_claude` | `pacer_gpt` — `openai/gpt-6-sol`, watching `reviewer_gpt` | `ITERION_VIBE_MODEL_SUPERVISOR` / `…_SUPERVISOR_GPT`, or `ITERION_SUPERVISORS=off` |

The column heading is the LAUNCH VAR, not the topology. A webhook launch the
publisher resolved dual carries no `mono_family` at all
(`reviewtopology.InjectIfDeclaredFamilies` deletes it there), so the var falls
back to its declared `claude` default and the left column holds. A launch that
keeps the pin while fanning both reviewers — `review_tier: audit`, or an
explicit `--var review_mode=dual --var mono_family=gpt` — reviews
cross-family and **merges on `converge_gpt`**: the pin is the operator's
explicit choice, and on a GPT-only tenant (where `InjectAll` injects it)
sending the merge back to the Anthropic wire is the failure this release
closes. The published run table names the merge model that actually ran
(`ai_converge_model`, carried through `pr_gate`), so a cross-family review
merged on gpt-6-sol says so.

**The trade, named: Opus 5.5 is no longer the default merger of a mono-GPT
review.** A deployment that wants it back dials the twin
(`ITERION_VIBE_BACKEND_EMIT_GPT=claude_code` +
`ITERION_VIBE_MODEL_EMIT_GPT=claude-opus-5-5` +
`ITERION_VIBE_PROVIDER_EMIT_GPT=anthropic` — all three, or the slot lands on
a route nobody chose), pins an integration override on the `converge_gpt`
selector, or pins `--var mono_family=claude`. The `_EMIT` trio keeps dialling
the claude twin, which the twin's defaults now make redundant: a deployment
carrying the pre-0.9.16 mono-GPT profile — the `_EMIT` backend dial pointed at
claw, with an `openai/…` model and an `auto` hint — **must drop that profile**,
not merely may. `converge` declares no `tools:` list, and on claw a node's
list is its whole surface, so that profile leaves the claude-family merge with
no tool at all: no report under `report_path`, no board issue, and its
`capabilities:` grant inert (`TestReviewPrTheEmitBackendDialStopsAtTheCLIBackends`,
`bots`). `converge_gpt` was split off to serve exactly that route, and it
serves it with the list declared. A deployment that pointed the merge
somewhere ELSE repeats that choice on the `_EMIT_GPT` trio instead.

Each `ITERION_VIBE_*` dial repoints live through `bot-vars`, without
re-releasing the bundle; `ITERION_SUPERVISORS` is the machine-wide kill
switch instead, the heavier lever of the two. A supervisor is metered as a
route on every execution only while one of the nodes it WATCHES is on the
run's topology — a coordinator evaluates nothing until a watched node is
active — which is what lets each family keep its own pacer without the other
one parking the run (`runner.preflightSupervisors`). Pinned with no dial set,
a mono-GPT review starts under a spent Anthropic window, soft cap or hard
(`TestUsageCapPreflight_MonoGPTOnTheReviewPrBot`, `pkg/runner`), and the
merge runs on claw (`TestReviewPRTier_GlanceRoutesToCheapReviewerGPT`,
`e2e`).

## Layout — a bot in several files

`main.bot` holds the header, the vars, the secrets, the supervisor and the workflow; the
rest lives beside it under `lib/` and is reached through the `import` lines at the head of
the main — `lib/schemas.bot` (the schemas), `lib/prompts.bot` (the prompts), `lib/nodes.bot`
(the nodes). The four files are ONE program: `iterion validate`, `run`, the studio and a
remote launch read the unit; the manifest's `requires.iterion` names the release that reads
`import`. See docs/dsl.md, "import — a bot in several files".
