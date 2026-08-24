---
name: iterion-bot-catalog
description: Catalog of iterion bots — pick a bot name for each roadmap_item.assignee. The stock dispatcher routes by assignee through assignee_workflows.
---

# Iterion Bot Catalog — for whats-next.bot's `propose_roadmap`, `revise_roadmap`, and `emit_action`

<!-- This file is the HAND-AUTHORED TEMPLATE for the bot catalog. The
     persona table + per-bot reference cards between the GENERATED markers
     below are produced from each bot's manifest.yaml by
     botregistry.RegenerateWhatsNextCatalog (run at whats-next start and
     on every studio bot-metadata save). Do NOT hand-edit that region —
     edit the bots' manifest.yaml instead (display_name / description /
     when_to_use / triggers / enabled), or toggle a bot in the studio
     Catalog manager. Everything OUTSIDE the markers is editorial routing
     reasoning you maintain by hand. This template lives at the bundle
     ROOT (not skills/) so it is never mirrored as a skill; the generated
     copy Nexie actually reads is skills/iterion-bot-catalog.md. -->

Consumed by three phases:

1. **`propose_roadmap` / `revise_roadmap`** — pick the right
   bot name for each `roadmap_item.assignee`. Leave it `""`
   when no existing bot fits.
2. **`emit_action`** — validate every assignee against the
   catalog before creating issues. Unrecognised assignees get
   stripped to `""` and the issue is labelled
   `needs-manual-triage`.

**Trust check first**: this catalog enumerates bots discovered
in the workspace. If the workspace ships no bots (none of the
cards below resolve), all assignees should be `""` and all issues
will be `needs-manual-triage`.

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
   workflow. See `docs/dispatcher.md` §Routing by issue assignee.
2. **registry fallback** — when the assignee has no
   `assignee_workflows:` entry, the dispatcher resolves it against
   the discovered bot catalog (any enabled bot is routable by its
   technical name) and runs that bot's workflow.
3. **`workflow:` default** — the precompiled global fallback when
   the assignee is empty or unresolvable.

Native issues also have typed `Bot` / `BotArgs` fields. `BotArgs`
merges over rendered dispatch vars and is usable today.

`assignee_dispatch:` (when present) replaces `dispatch.vars`
wholesale per assignee; per-ticket `BotArgs` then merges on top
key-by-key (see the issue-creation section below).

whats-next records the assignee on every issue so operators can drive
routing by setting `--assignee` and mapping it through
`assignee_workflows:` (or relying on the registry fallback).

## Decision tree — pick `assignee` per roadmap item

Walk top-to-bottom; first match wins.

| If the work sounds like… | → `assignee` |
|---|---|
| "where should this project go next?", "long-term vision", "architectural direction", "strategic axes for the next quarter/year" — STRATEGIC (a quarter+ horizon) AND the project is mature/stable | `evolve` |
| "implement feature X", "add capability", "build the thing" | `feature-dev` |
| "build a new bot for Y" / "create a workflow that does Y" — the catalogue lacks a fit and we need to author one | `feature-dev` (with `feature_prompt` pointing at the new `.bot` file to create) |
| "review the whole codebase", "audit production-readiness", "find bugs anywhere" | `whole-improve-loop` |
| "focus on axis X" (observability / perf / DX / refactoring) ACROSS the codebase — improvement loop, not detection | `whole-improve-loop` (with `--var improvement_prompt=…`) |
| "review this branch", "review the PR", "fix the diff against main" — review AND fix AND commit | `branch-improve-loop` |
| "review this PR / branch and just REPORT the issues" — read-only review, posts findings to the board, does NOT fix or commit | `review-pr` |
| "upgrade dependencies", "patch CVEs", "bump versions", "renovate" — MUTATING (writes package.json / go.mod / lockfiles) | `secured-renovacy` |
| "audit the docs", "find code↔doc drift", "doc/code alignment", "fix outdated README/CLAUDE.md" | `docs-refresh` |
| "audit the source for vulns", "find injection / SSRF / IDOR / secrets", "OWASP source scan" — DETECTION (writes findings, not fixes) | `sec-audit-source` |
| "audit dependencies for malware / typosquats / install hooks", "supply-chain check", "post-`npm install` triage" — DETECTION across installed deps | `sec-audit-deps` |
| architectural choice, hiring, prioritisation meeting, alignment | `""` |
| operator is vague or it's cross-cutting | `""` |
| long-term theme (a quarter+ horizon) on a mature/stable project | `evolve` (it accumulates the vision + proposes evolutions) |
| long-term theme on a greenfield / unstable project | `""` (vision is premature — drive stability first) |

When in doubt, prefer `""` and let the operator triage manually
in the board UI. An empty assignee is honest; a wrong one
wastes a bot run.

## An issue that ALREADY has an open PR → Billy, with a fork guard

Before routing a roadmap item or board card the normal way, check
whether it ALREADY has an open pull request — a linked PR, or a branch
whose diff answers the item. If it does, the work is NOT greenfield: it
lives in a PR that needs finishing, reviewing, hardening, and landing.
Route it to **Billy (`branch-improve-loop`)**, NOT Featurly
(`feature-dev`). Featurly would re-implement from scratch and collide
with the contributor's branch; Billy reads the branch diff
(`base_ref...HEAD`), improves what it finds (bugs, weak tests, unhandled
errors), and commits in stride onto the PR branch. Set the item's
assignee to `branch-improve-loop` and pass the PR's target branch as
`base_ref`.

**The fork guard — a hard budget-safety rule, not a preference.** The
attribution is automatic ONLY when the PR's branch is on the repo
itself. When the PR comes from a **fork**, do NOT auto-assign Billy: a
fork PR is contributor/attacker-controllable, and auto-dispatching a bot
onto it spends LLM budget on code you don't control — a budget-exhaustion
and prompt-injection vector. Surface it and get explicit operator
approval first (an `ask_user` gate) before assigning.

- Same-repo PR (`head.repo == base.repo`, not cross-repository) → Billy,
  automatic.
- Fork PR (cross-repository, `head.repo != base.repo`) → ask the
  operator: "PR #N on issue #M comes from a fork (`<owner>`) — dispatch
  Billy on it? (y/N)". Assign only on an explicit yes; default is NO.

The signal you need is the PR's origin (GitHub's
`pull_request.head.repo.fork` / `isCrossRepository`; a same-repo PR's
head branch lives in the repo). When the board card doesn't carry that
signal, say so and ask the operator — never auto-dispatch a PR whose
origin you cannot confirm.

## Distinguishers — the pairs that ALWAYS need a tie-break

These overlaps come up often; commit each distinguisher to memory
before you walk the table on a new roadmap item.

### `feature-dev` vs `whole-improve-loop`

- `feature-dev` ships a NEW capability. There is a "done" state
  visible from the outside (a new endpoint, a new UI affordance,
  a new CLI flag). Body reads as a feature spec.
- `whole-improve-loop` improves EXISTING code along an axis
  (reliability, perf, observability, DX). There is no new
  capability — just better/cleaner code. Body reads as a quality
  bar to reach.
- Tie-break: "could a user notice the difference without reading
  the diff?" Yes → `feature-dev`. No → `whole-improve-loop`.

### `sec-audit-*` (DETECTION) vs `whole-improve-loop` (FIX-loop on a security axis) vs `secured-renovacy` (MUTATION on deps)

- `sec-audit-source` / `sec-audit-deps` ARE READ-ONLY. They emit
  findings as kanban issues; they don't fix anything. Use when
  the operator wants a security baseline / list of issues / a
  triage pass — NOT when they want fixes applied.
- `whole-improve-loop` with `improvement_prompt: "security focus"`
  is FIX-mode: one adaptive campaign closes issues site by site and commits
  each verified change; its deterministic build/test gate plus the campaign's
  `axis_complete` signal control convergence. Use when the operator wants
  security holes closed in place.
- `secured-renovacy` is MUTATION on dependency manifests
  (package.json / go.mod / Cargo.toml / requirements.txt /
  lockfiles). Use when the operator wants CVE patches landed by
  bumping versions, NOT when they want code rewritten to be
  safer.
- Tie-break ladder: "do they want a list?" → sec-audit-*. "do
  they want code rewritten?" → whole-improve-loop. "do they want
  versions bumped?" → secured-renovacy.

### `whole-improve-loop` vs `branch-improve-loop`

- `whole-improve-loop` scans the entire workspace.
- `branch-improve-loop` scans `git diff base_ref...HEAD` only —
  scoped to what the current PR/branch touched, then commits a
  semantic message covering its fixes.
- Tie-break: "is there an open PR / unmerged branch they want
  reviewed?" → `branch-improve-loop`. "is the work
  workspace-wide / no specific branch?" → `whole-improve-loop`.

### `evolve` (Evoly) vs `whats-next` (Nexie) — altitude

- `whats-next` / Nexie is the **tactical** orchestrator (you). It
  answers "what should we work on this week?" — one next_action,
  ≤2-week-horizon items, kanban dispatch.
- `evolve` / Evoly is the **strategic** partner, one altitude ABOVE
  you. It answers "where should this project go next quarter / year?":
  it accumulates a long-horizon architectural vision in its OWN per-bot
  memory across sessions, interrogates the operator mid-investigation,
  and proposes natural evolutions as dispatch-ready backlog tickets +
  findings — which YOU then pick up on your next survey and triage into
  roadmap items.
- Tie-break — **horizon**: ≤2 weeks → Nexie. ≥ a quarter → Evoly.
  And **altitude**: "what's next?" → Nexie. "where to next?" → Evoly.
- Tie-break — **maturity**: greenfield / unstable / WIP → Nexie (a
  vision is premature; drive stability first). Settled, mature project
  where the question is direction not throughput → Evoly.
- Evoly does NOT implement. Its output is a vision + evolution proposals
  (in `findings/` + `backlog` tickets). You ingest those into roadmap
  items; the dispatcher then routes them to feature-dev /
  whole-improve-loop / etc. When an operator asks you for a long-horizon
  vision on a mature repo, the right move is often to route to `evolve`
  rather than answer at your own altitude.

### `ultra11y` (Ally) vs `rgaa-audit` (Acci) — who FINDS the non-conformity

Both are READ-ONLY accessibility auditors. Neither fixes anything (that
is Willy, `whole-improve-loop`, with the `rgaa` preset). They differ in
where the findings come from, which is what decides between them.

- `ultra11y` / Ally: a static ENGINE finds the non-conformities — 78
  machine-detectable checks tied to the WCAG 2.2 success criteria,
  measured against the W3C ACT corpus. Each finding carries a stable id,
  a criterion, a severity and a `file:line`. The one agent step rules
  only on the criteria a static pass cannot decide, and the engine's own
  fail-closed gate refuses an unjustified verdict or an ungroundable
  non-conformity. It also has a PR mode: `pr_url` + `base_ref` audit
  exactly what the branch introduced.
- `rgaa-audit` / Acci: the AGENT finds the non-conformities, reading the
  UI source theme by theme against the 106 RGAA criteria, with the DSFR
  MCP tools as the reference markup when the target uses the Système de
  Design de l'État. Deterministic gates check the audit happened; they
  do not produce the findings.
- Tie-break — **reproducibility**: "must a finding survive without a
  model in the loop (a conformance deliverable, a per-PR gate, an
  auditor who will re-run it)?" → Ally. "is the value in RGAA
  theme-by-theme reasoning over a DSFR UI?" → Acci.
- Tie-break — **scope shape**: a pull request or a branch → Ally (Acci
  has no diff mode). A whole-repo RGAA campaign on a French public
  service UI → Acci.
- Neither supersedes the other, and running both on the same repo is
  legitimate: they disagree usefully.

## When no row matches confidently — three escape hatches

1. **Propose the closest match in rationale, leave `assignee=""`**
   on the item. The body should explicitly say "closest match:
   `<bot>` — operator should confirm before dispatch." This is
   the most common case for cross-cutting or partially-fitting
   work; the operator decides at human_review.
2. **Surface the ambiguity in `rationale`** as a question the
   operator can answer. Example: "Item #3 ('Refactor auth') sits
   between `feature-dev` (new SAML provider as capability) and
   `whole-improve-loop` (reliability/observability on existing
   auth). Pick by replying with the assignee you want, or accept
   the default `""`." The studio renders the rationale verbatim
   so the operator sees the question.
3. **Propose creating a NEW bot** when the catalogue genuinely
   doesn't have a fit and the work will recur. Emit a
   `feature-dev` item whose `feature_prompt` describes the bot
   you'd build (target `.bot` filename, expected vars, pipeline
   sketch). Example: "Build a new bot `flake-hunter` at
   `examples/flake-hunter/main.bot` that runs the test suite N
   times and groups failures by stack trace — needs `vars: {
   suite: string, repeats: int=20 }`."

Bot creation always routes through `feature-dev`; there's no
"bot_factory" assignee. The new bot ships in the same PR as the
item that called for it.

## What ambiguity looks like in practice — examples

- "Improve our auth reliability" → likely `whole-improve-loop`
  with `improvement_prompt: "auth + session handling
  reliability"`, BUT if the operator's priorities mention
  "add OAuth" the same item is `feature-dev`. Surface the
  question if both fits look plausible.
- "Make the docs match the new dispatcher API" → `docs-refresh`
  (clear). No ambiguity.
- "Fix the failing CI on the rust port" → `branch-improve-loop`
  IF there's an open branch, `feature-dev` IF the CI fix is
  itself a new capability (e.g. a new test runner). Surface
  the question.
- "Reduce vendor dependency footprint" → ambiguous.
  `secured-renovacy` could prune by bumping; `whole-improve-loop`
  could refactor to drop dependencies; `feature-dev` could build
  an in-house replacement. Surface as a three-way question.
- "I want a vision for the next year of this project" → `evolve`
  (clear) when the project is mature/stable. If it's greenfield or
  still churning, surface the question instead: "a vision before the
  project has settled is usually waste — want me to drive a few
  stability iterations first, then hand off to Evoly?"

<!-- ITERION:CATALOG:GENERATED:BEGIN -->

## The team — persona ↔ assignee

When you emit an `assignee`, always use the **technical name** (the
dispatcher routes on it), never the persona.

| Persona | `assignee` (technical name) |
|---|---|
| Adry | `adr-cartograph` |
| ReArchi | `adr-rechallenge` |
| Appy | `app-dev` |
| Themis | `arbitrate` |
| Bmady | `bmady` |
| Billy | `branch-improve-loop` |
| Campy | `campaign` |
| Vetty | `dep-update-guard` |
| Devy | `devbox-setup` |
| Doki | `docs-refresh` |
| Endy | `e2e-coverage` |
| Evoly | `evolve` |
| Featurly | `feature-dev` |
| Fini | `feature-gap-fill` |
| Vigie | `feed-watch` |
| Goldy | `golden-master` |
| Heartbeat (always-on demo) | `heartbeat` |
| Obsy | `instrument` |
| Triagy | `issue-triage` |
| Morphy | `modernize` |
| Nested Subbots Demo | `nested-subbots-demo` |
| Pipeline Board Demo | `pipeline-board-demo` |
| Revi (converse) | `revi-converse` |
| Envy | `review-env` |
| Revi | `review-pr` |
| Acci | `rgaa-audit` |
| Depsy | `sec-audit-deps` |
| Seki | `sec-audit-source` |
| Renovacy | `secured-renovacy` |
| Shieldy | `supply-shield` |
| Vulny | `supply-shield-cve` |
| Testy | `test-coverage` |
| Ally | `ultra11y` |
| Senti | `vuln-watch` |
| Nexie | `whats-next` (this bot) |
| Willy | `whole-improve-loop` |
| Wikky | `wiki-gen` |

## Bot reference

### `adr-cartograph` — Adry

Observes the code-as-implemented and produces committable ADR markdown
(Nygard format) in docs/adr/ — one capable agent over a deterministic
drift manifest, minimal framing. Every ADR is a "constat" recording
the decision the code embodies (decision-vs-mechanic three-check dam
against ADR-spam), authored one `docs(adr):` commit at a time in
stride. Also surfaces feature-completeness gaps.

Idempotent: the manifest re-globs the live ADR directory each pass,
so a converged tree yields no drift, no commits, and a fast exit; the
sha-cache pre-verifies unchanged entries across runs.

Read-only on code — a deterministic scope gate fails the run if
anything outside docs/adr/*.md changed. Handoffs: files
type:adr-rechallenge issues (aged ADRs) and type:feature-gap issues
(medium/high gaps) to the board inbox.

- **Use when**:
  Run after a code-mutating session (feature_dev, branch-improve-loop,
  bmady) lands non-trivial decisions, before a release, or on a nightly
  cadence to keep docs/adr/ honest against the code. Use
  --var rechallenge_after_days=90 to invite re-challenge on ADRs older
  than that.
- **Vars**: `adr_dir` (string), `audit_cache_path` (string), `baseline` (string), `bundle_self_path` (string), `code_scope_globs` (string), `coverage_target_pct` (int), `diff_since` (string), `excluded_dirs` (string), `issue_id` (string), `max_passes` (int), `rechallenge_after_days` (int), `scope_notes` (string), `scratch_dir` (string), `workspace_dir` (string)
- **Path**: `bots/adr-cartograph/main.bot`

### `adr-rechallenge` — ReArchi

Human-in-the-loop ADR re-challenge. Loads an ADR + the current code,
presents fresh arguments (changed assumptions, alternatives that
matured, dependency updates, code drift), and asks the human:
keep / change / addendum.
  keep     -> end, no change.
  change   -> file a board ticket describing the proposed change.
  addendum -> write a short dated addendum note appended to the ADR,
              then ask the human commit / skip. commit -> git commit;
              skip -> end (the note is optional).

- **Use when**:
  Run on a type:adr-rechallenge issue created by the adr-cartograph (Adry)
  bot, OR manually via --var adr_path=docs/adr/NNN-<slug>.md when an
  operator wants to revisit a specific decision.
- **Triggers**: adr, architecture-decision, re-challenge, revisit-decision, design-review
- **Vars**: `adr_dir` (string), `adr_path` (string, required), `issue_id` (string), `scope_notes` (string), `workspace_dir` (string)
- **Path**: `bots/adr-rechallenge/main.bot`

### `app-dev` — Appy

Autonomous end-to-end APPLICATION development from a prompt —
greenfield. Creates a NEW app in any stack (Next.js, Django, Rust
CLI, Go service, …): official non-interactive scaffold, walking
skeleton (builds + runs + smoke test), then one verified semantic
commit per slice until the brief is fully shipped. Two modes:
`interview` converges a precise SPEC.md through a conversational
loop BEFORE development starts (recommended for vague briefs);
`autonomous` goes straight to a free first draft. A deterministic
build/test gate + adversarial self-review re-check every pass, and
an opt-in draft-review gate lets the operator ship, request changes
(feedback loops back into the campaign), or hold. Re-running against
the generated app EVOLVES it (brownfield detection) instead of
re-scaffolding. An opt-in tail pushes the series and opens the
pull request (PR; merge request on GitLab).

- **Use when**:
  Use to create a NEW application from a natural-language brief —
  point it at an EMPTY directory (the bot `git init`s and commits its
  own scaffold). Prefer feature-dev when a codebase already exists and
  the ask is one feature inside it; prefer app-dev when the deliverable
  IS the application. Pass mode=interview for a spec-first conversation
  when the brief is under-specified; keep the default mode=autonomous
  for a fast free first draft the operator reframes at the draft-review
  gate. A re-run against the generated app evolves it.
- **Triggers**: new-app, greenfield, scaffold, bootstrap, app-from-prompt
- **Vars**: `app_prompt` (string), `baseline` (string), `deploy_enabled` (bool), `draft_review` (bool), `max_deploy_retries` (int), `max_draft_loops` (int), `max_interview_turns` (int), `max_passes` (int), `mode` (string), `mr_base` (string), `mr_branch` (string), `open_mr` (bool), `scratch_dir` (string), `source_issue_ref` (string), `stack` (string), `workspace_dir` (string)
- **Path**: `bots/app-dev/main.bot`

### `arbitrate` — Themis

Judges the divergence cases a modernisation programme leaves blocked, by
applying the target repository's own written arbitration doctrine — and
nothing else. One adversarial judge, one mechanical consignment: every
decision lands as a machine block in the doctrine's journal, bounded,
evidence-cited and committed; anything the doctrine does not cover exactly
escalates to a human.

The judge DECIDES and never executes: canonicalisation stays the net
owner's act, re-recording goes through the re-baseline ledger's rite, a
defect goes to a lot. Separation of powers is enforced mechanically — the
consignment step refuses a dirty tree (a judge that edited anything is not
a judge) and force-escalates past a per-lot budget.

- **Use when**:
  Use when a modernisation lot is BLOCKED on a measured divergence and the
  target repository carries a written arbitration doctrine (the contract
  names its path; without one this bot refuses to judge). Typical loop:
  modernise → blocked with a report → arbitrate → the consigned decision
  guides the next act (net-owner canonicalisation, ledger re-baseline, or a
  defect lot) → resume.
  
  Do NOT use it to write or extend the doctrine — that is the contract
  owner's pen. Do NOT use it on divergences whose cause is not established:
  an unmeasured case escalates by construction.
- **Vars**: `budget_per_lot` (int), `doctrine_path` (string), `only_lot` (string), `plan_path` (string), `workspace_dir` (string)
- **Path**: `bots/arbitrate/main.bot`

### `bmady` — Bmady

BMAD-METHOD-inspired agile delivery bot. Runs a structured
multi-persona pipeline — Analyst → PM → Architect → Dev → QA —
with a human collaboration gate between every phase. Each gate is
a different kind of decision (free-text elicitation, an
advanced-elicitation option menu, a document approve/reject, a
story multi-select with priority + WIP, a ship/changes/hold
sign-off) so the operator stays in the driver's seat from brief to
commit.

Vehicle for iterion's human-interaction surface: one run exercises
every studio form widget (free-text, radio, select, checkbox
multi-select, numeric, approve/reject).

- **Use when**:
  Use when you want a feature delivered the BMAD way — explicit
  human-approved planning artifacts (analysis, PRD, architecture)
  before any code is written, then an implement → QA → sign-off loop
  you steer at each step. Pick this over feature_dev when you want to
  collaborate on the plan rather than hand off an autonomous run.
- **Triggers**: bmad, agile, plan-then-build, prd, architecture
- **Vars**: `brief` (string, required), `workspace_dir` (string)
- **Path**: `bots/bmady/main.bot`

### `branch-improve-loop` — Billy

Branch-scoped REVIEW-AND-IMPROVE campaign — one capable agent, its natural
flow, minimal framing. Reviews and improves the changes THIS branch
introduces over base_ref (default "main"): reads the diff
`git diff $(git merge-base base_ref HEAD)` (merge-base vs WORKING TREE, so a
prior pass's uncommitted fixes stay visible), finds the REAL issues the
change introduces or leaves (bugs, regressions, missing/weak tests, unhandled
errors, quality problems IN THE DIFF), and improves them — committing each
fix in stride with a semantic message. It does NOT re-litigate code the
branch didn't touch. A deterministic, stack-agnostic build/test gate
re-checks the tree after each pass (the anti-Goodhart truth oracle: the agent
can't self-certify); RED routes back with the failure log, green + branch
clean converges. git IS the durable state — an interrupted / budget-capped
run keeps every committed fix, and a re-dispatch re-runs the campaign. Bounded
by a max_passes cap. An optional PR path (merge request on GitLab) ships the
series of per-pass commits.
Sibling of whole-improve-loop v2 (ADR-058). See
docs/references/productive-session-patterns.md.

- **Use when**:
  Use when an existing branch/PR needs a rigorous review + fix + commit before
  merge. Scopes to the diff base_ref...HEAD (measured against the working tree)
  and commits each fix as a semantic commit in stride; pass base_ref for a
  non-main integration base. One capable agent reviews the branch diff and
  improves what it finds, converging when a fresh re-review is clean and a
  deterministic build/test gate is green. For a whole-codebase (not
  branch-scoped) cross-cutting improvement, use whole-improve-loop instead.
- **Vars**: `base_ref` (string), `baseline` (string), `forge_publish_token` (string), `forge_publish_url` (string), `gate_context` (string), `gate_enabled` (bool), `max_passes` (int), `mr_base` (string), `mr_branch` (string), `open_mr` (bool), `pilot` (string), `pr_url` (string), `prior_review` (string), `push_branch` (string), `scope_notes` (string), `scratch_dir` (string), `source_issue_ref` (string), `workspace_dir` (string)
- **Path**: `bots/branch-improve-loop/main.bot`

### `campaign` — Campy

Supervises a WHOLE modernisation programme, lot after lot, by running the
modernize bot as a subbot in a bounded loop — and holding the separation
of powers a human supervisor held before it existed. Progress is judged by
git, never by what a run says of itself: a child run that did not move
HEAD landed nothing, and two still runs in a row end the campaign.

The supervisor is DETERMINISTIC: not one LLM node of its own.
Intelligence lives in the child bots, judgement lives in gates. Its
steward half executes ledger re-baseline requests if and only if the
observed reference diff equals the announced set exactly, with the full
mutation counter-test replayed on the committed tree behind every act —
a red counter-test unwinds the act. Contract extensions (a lot the worker
added or reshaped in the plan) fall under a configured governance:
accepted in flight and listed at the head of the final handoff (default),
or paused for human approval every time. Other escalations either pause
the run (interactive) or accumulate into the handoff (default) — and the
campaign always ends on a committed handoff plus a human review node,
with blocked lots requalified against the final tree.

- **Use when**:
  Use to carry a WHOLE programme unattended once its two prerequisites
  exist: a `.modernize/plan.yaml` contract and a behavioural net under
  `.golden-master/` (verify-oracle.sh). One `iterion run` then plays lot
  after lot where a human would have relaunched modernize by hand, judged
  progress in git, executed announced re-records between runs, and kept the
  journal.
  
  Do NOT use it to run a single lot (run modernize directly), to build the
  net (golden-master's job), or to decide WHAT to modernise — the programme
  is a human decision recorded in the contract, and this bot's whole
  authority over it is measuring whether it advances.
- **Vars**: `escalation` (string), `governance` (string), `lot_max_passes` (int), `max_lots` (int), `plan_path` (string), `stagnation_stop` (int), `workspace_dir` (string)
- **Path**: `bots/campaign/main.bot`

### `dep-update-guard` — Vetty

Reactive security + alignment guard for automated dependency-update
PRs (Dependabot / Renovate). Triggered per PR, on the bot's own
branch, Vetty: (1) audits the bump for supply-chain risk — known
malware, typosquats, compromised-maintainer signals, and CVEs
introduced vs resolved; (2) aligns the consuming code to any
breaking change (a JS/TS lib API, Helm chart values, a Go module,
…); (3) proves reliability through a DETERMINISTIC build/test gate
(the real exit code of the repo's own commands — the only path to
commit); (4) commits the alignment back onto the PR branch and posts
a complete review comment with the verdict and evidence; and (5)
escalates to a human when a structuring architectural decision is
required. It never merges past a check: unless auto-merge arming is
explicitly enabled, the merge stays a human call.

Stack-agnostic by construction: the per-ecosystem scanner and
build/test knowledge lives in the skills (package-managers,
dependency-pr-guard), and one adaptive agent reads them and adapts
to whatever repo the PR targets; deterministic gates verify the
audit ran and the build is green before anything is committed.

- **Use when**:
  Use to guard a repository's automated dependency-update PRs. Enable
  it on a repo via the studio Integrations flow with author filtering
  to the dependency bots — it then reacts to each Dependabot/Renovate
  PR, posts a security + alignment verdict, and commits any code
  alignment onto the PR branch. Not for human PRs (use Revi /
  review-pr), and not for proactively opening update PRs (that is
  Renovacy / secured-renovacy).
- **Vars**: `arm_automerge` (bool), `automerge_method` (string), `base_ref` (string), `forge_publish_token` (string), `forge_publish_url` (string), `gate_context` (string), `gate_enabled` (bool), `max_fix_iterations` (int), `post_to_board` (bool), `pr_author` (string), `pr_url` (string), `scope_notes` (string), `scratch_dir` (string), `verify_timeout_s` (int), `workspace_dir` (string)
- **Path**: `bots/dep-update-guard/main.bot`

### `devbox-setup` — Devy

Bootstraps a reproducible dev environment for a repository. Detects the
project's languages, runtimes, build + test tools and e2e stack (e.g.
Playwright), then authors a PINNED `devbox.json` (Nix-packaged toolchain)
at the repo root and validates it with `devbox install`. The generated
`devbox.json` is what other iterion bots — and humans — use to run the
project's build / test / e2e in a reproducible toolchain (ADR-017 Tier-2/
Tier-3): once a repo has one, `build_rung` / `regress_rung` / patch_author
run project commands via `devbox run -- …`.

Scope discipline: writes ONLY `devbox.json` (+ `devbox.lock`); never edits
source. Default mode proposes the change in a worktree behind a human gate
(the project's dev environment is consequential — an operator confirms
before it lands).

- **Use when**:
  Run on a repo that has NO `devbox.json` yet (so iterion bots can run its
  build/test/e2e reproducibly), or when its toolchain drifted from what the
  code now needs (new language, runtime bump, added e2e). Produces a pinned
  `devbox.json` + `devbox.lock`; it does not change source.
- **Vars**: `workspace_dir` (string)
- **Path**: `bots/devbox-setup/main.bot`

### `docs-refresh` — Doki

Documentation alignment bot — one capable agent + a mission + truth
gates only. Converges the documentation, exhaustively, to the
actual current state of the repo: docs follow code, exhaustively —
never the reverse. Both halves are doc-side: it REPAIRS stale
documentation (mismatched claims, dead links, drifted invocations,
outdated examples), and it WRITES the missing documentation
(undocumented capabilities, surface, and code areas). Every edit
lands as a `docs(scope):` commit in stride; code is never touched.
When a repo has NO documentation yet, it bootstraps an initial doc
set (configurable docs_dir, default "docs") authored from the code,
then refreshes it through the same campaign. Documented claims that
read as deliberate, unfulfilled AMBITIONS are neither deleted nor
aligned-down: they are recorded and reported in the PR body under
"Unfulfilled documented promises".

The determinism is TRUTH-only: a deterministic scope gate fails the
run if anything outside the doc writeable-set (`.md`) changed, and
convergence is that scope gate ∧ the campaign's honest docs_aligned
contract — nothing else. (A docs-only change cannot affect the
repo's build, so there is no build/test gate.) A deterministic scan
still
runs each pass, but as an ADVISORY hints producer (missing paths,
dead links/anchors, unmentioned code areas, telemetry): help the
agent is free to use, contradict, and explore beyond — never an
obligation. The dismissals ledger and promises ledger persist the
agent's own adjudications across passes (memory, never a cage).

Opt-in delivery: open_mr=true pushes the alignment series and opens
one PR/MR at the end of the run (gated by a deterministic push-
credential probe) — required for repo-targeted cloud runs, whose
clone is ephemeral.

The bot ships 7 skills capturing the discipline: docs-refresh
(playbook), doc-mismatch-taxonomy, doc-enrichment,
doc-scope-enumeration, anti-facade-fix-rules,
doc-verification-checklist, forge-mr-create.

- **Use when**:
  Use when README / CLAUDE.md / docs/**/*.md / bundled skills are
  stale versus the code, before a release, or whenever a survey flags
  code↔doc drift — or when parts of the repo (capabilities, whole
  subsystems) are simply UNDOCUMENTED and the docs should converge to
  the actual state of the repo — or when a repo has NO docs yet and
  needs an initial set authored from the code. Fixes and writes the
  DOCS only (never code logic) and commits.
- **Vars**: `base_ref` (string), `bundle_self_path` (string), `diff_since` (string), `dismissed_path` (string), `doc_globs` (string), `docs_dir` (string), `excluded_dirs` (string), `max_hints` (int), `max_passes` (int), `mode` (string), `mr_base` (string), `mr_branch` (string), `open_mr` (bool), `pr_url` (string), `scope_notes` (string), `scratch_dir` (string), `source_branch` (string), `source_issue_ref` (string), `workspace_dir` (string)
- **Path**: `bots/docs-refresh/main.bot`

### `e2e-coverage` — Endy

Autonomous end-to-end coverage completion — one capable agent, its
natural flow, minimal framing. Points at an application (or one
feature family of it via `target`), inventories its FEATURES from the
outside (docs, CLI surface, API routes, configuration, grammar),
maintains a committed feature×coverage MATRIX, and closes each gap
with a real, deterministic e2e test written in the repo's OWN harness
and conventions — one `test(e2e):` commit per feature, the matrix row
flipped in the same commit.

Anti-façade by design: the success metric is NOT a green table — it
is features whose regression would fail a test before shipping. The
campaign's contract enforces the feature-level mutation test (break
the feature ⇒ the test must fail) and forbids stub-echo tests,
harness-only tests, no-invariant tests and borrowed claims; a
deterministic gate re-runs the repo's own suite AND parses the
matrix, grep-verifying every covered row's cited test (an orphan
claim is a red gate). Deterministic-first: a CI-runnable test beats
an opt-in/live one; features that genuinely need a live external are
marked covered-live or excluded WITH the reason — never silently
skipped.

Stack-agnostic: how to enumerate features, find the repo's own e2e
harness, and write each test lives in the bot's skills, not in the
workflow — so adding a language or harness style needs no DSL edit.

- **Use when**:
  Use when an application needs its e2e/anti-regression net completed —
  a repo with features that only have unit tests (or none), a grown
  codebase whose e2e suite lags the feature surface, or as a recurring
  audit that keeps a feature×coverage matrix honest. Endy writes and
  commits deterministic e2e tests plus the matrix; it does not change
  product behaviour.
  
  Do NOT use to deepen unit/integration coverage of a code area (that
  is Testy / test-coverage), to review a branch (Billy), or to build a
  behavioural golden-master oracle for an existing app's outputs (that
  is Goldy / golden-master — reference recordings, not feature e2e
  tests). Endy's axis is the FEATURE-level e2e completeness of the
  whole application, made checkable by the matrix.
- **Triggers**: e2e, e2e-coverage, end-to-end, coverage-matrix, e2e-tests, regression-net, feature-coverage
- **Vars**: `baseline` (string), `matrix_path` (string), `max_passes` (int), `scratch_dir` (string), `target` (string), `workspace_dir` (string)
- **Path**: `bots/e2e-coverage/main.bot`

### `evolve` — Evoly

Strategic / architectural / visionary partner. On a mature, stable
repository, Evoly surveys the codebase, accumulates a long-horizon
architectural VISION in PER-BOT persistent memory across sessions,
interrogates the operator MID-INVESTIGATION (ask_user) to collect the
context the code alone cannot give, and proposes natural evolutions as
dispatch-ready backlog tickets.

Evoly sits ABOVE Nexie in the workflow stack: Evoly names where the
project should be in a year; Nexie names what to do this week. Each
proposed evolution lands as a kanban ticket pre-filled with bot +
bot_args (so a human can launch it by dragging it to ready, or Nexie
can action it) plus the full plan / technical decisions in the
project-shared findings/ memory scope.

Evoly PROPOSES and ARCHITECTS — it does not implement features.
Implementation is handed to feature-dev / bmady via Nexie's
roadmap-and-dispatch pipeline.

Showcase of two iterion features:
  - per-bot persistent memory (visibility: "bot"): VISION.md +
    CONTEXT_BRIEF.md + decisions/ accumulate across sessions WITHOUT
    leaking into Nexie or other bots' memory;
  - mid-turn ask_user MCP escalation from the investigation agent so the
    operator is interrogated only when the LLM cannot resolve an
    ambiguity from the code alone — and every answer is persisted to
    per-bot memory so it is never asked twice.

- **Use when**:
  Use ONLY when the project is mature / stable enough that the question
  worth answering is "where should this go next?" (a quarter and beyond),
  not "what should we ship this week?" (that is Nexie / whats-next).
  Engage when:
    - the operator asks for a long-horizon vision, architectural
      direction, or strategic axes;
    - the codebase has settled (low recent breaking-change cadence,
      present ADRs, stable CI) and warrants a vision pass;
    - Nexie has run repeatedly and the operator wants to step UP one
      altitude — from "what's next" to "where to next".
  
  Do NOT use for tactical "what to ship this week" questions (that is
  Nexie), nor on greenfield / unstable projects (premature vision is
  waste). The repo-maturity-assessment skill captures the gating
  heuristic; Nexie can consult it before deciding to route here.
- **Triggers**: evolve, evolution, vision, architecture, long-term, strategy, roadmap-vision
- **Vars**: `mono_family` (string), `review_mode` (string), `scope_notes` (string), `workspace_dir` (string)
- **Path**: `bots/evolve/main.bot`

### `feature-dev` — Featurly

Autonomous end-to-end feature development — one capable agent, its
natural flow, minimal framing. Takes a `feature_prompt` input; the
campaign explores, builds a living todo of slices, and ships the
feature one verified semantic commit at a time (tests included, ADRs
authored for non-trivial decisions). A deterministic build/test gate +
bounded continuation loop re-poke it until the feature is complete and
the tree is green; an opt-in tail pushes the series and opens the
pull request (PR; merge request on GitLab).

- **Use when**:
  Use when an item can be phrased as one feature with a clear,
  externally-visible "done" state (new endpoint, UI affordance, CLI
  flag). Also the route for "build a new bot" work — point
  feature_prompt at the new .bot file to author.
- **Vars**: `baseline` (string), `feature_prompt` (string, required), `max_passes` (int), `mr_base` (string), `mr_branch` (string), `open_mr` (bool), `scratch_dir` (string), `source_issue_ref` (string), `workspace_dir` (string)
- **Path**: `bots/feature-dev/main.bot`

### `feature-gap-fill` — Fini

Gap-driven feature completer — one capable agent, its natural flow,
minimal framing. The input is a STRUCTURED gap spec ("here is what's
implemented, here is what's missing") rather than a feature description
from zero. The campaign surveys the seams, closes the missing items one
verified commit at a time (preserving what already works), and a
deterministic build/test gate + bounded continuation loop re-poke it
until the gap is closed and the tree is green. Use feature_dev for
greenfield work; use Fini to FINISH an existing partial implementation
without re-architecting what already works.

- **Use when**:
  Run on a type:feature-gap issue created by the adr-cartograph (Adry)
  bot, OR manually via --var gap_spec='<spec>' when an operator wants to
  close a specific gap on a feature. Prefer feature_dev when the work is
  greenfield (no existing partial implementation to preserve).
- **Vars**: `baseline` (string), `gap_spec` (string, required), `max_passes` (int), `scope_notes` (string), `scratch_dir` (string), `workspace_dir` (string)
- **Path**: `bots/feature-gap-fill/main.bot`

### `feed-watch` — Vigie

Universal feed-watch + digest bot (Huginn-style veille pipeline as a
single bot). Two run modes over one file-backed state in the target
workspace, selected with --var mode=:

collect (zero-LLM — runs with no LLM credential): polls every
configured RSS/Atom/RDF feed with a stdlib parser, dedups against a
per-category seen-items FIFO (ids + urls, cross-source), and queues
the fresh items in pending.jsonl.

digest (one LLM step): snapshots a category's queue (empty queue →
done at zero cost), then an editorial agent groups same-story items,
web_fetches the top articles to ground the takeaways, semantically
dedups against the previously sent digests, ranks by importance and
writes ONE chat-ready markdown message; a deterministic tool POSTs it
to the configured Mattermost/Slack incoming webhooks and clears
exactly the digested items from the queue.

Everything workspace-specific (categories, feeds, editorial guidance
and language, webhook sinks, cadences) lives in the target repo's
config file (default feed-watch.json) + the `webhooks` secret — no
feed, prompt language or channel is baked into the bot. Requires
python3 (stdlib only) on the execution host.

Hardened against untrusted config: the editorial guidance feeds the LLM
behind a permission gate (WebFetch-only, so an injected prompt can't
reach a shell or the mounted secrets), a deterministic link firewall
rejects any digest URL not drawn from the collected items, and feed
fetching refuses private/loopback/metadata addresses and non-http(s)
schemes by default (opt into internal feeds with
--var allow_private_feeds=true on a trusted deployment).

- **Use when**:
  Use to run a recurring technology/news watch over RSS/Atom feeds with
  LLM-synthesized digests delivered to chat (Mattermost/Slack incoming
  webhooks): schedule `mode=collect` runs to poll feeds cheaply
  (zero-LLM), and per-category `mode=digest` runs (daily/weekly) to
  synthesize and deliver. Replaces a Huginn RSS → dedup → digest → LLM
  → webhook scenario one-for-one. Not for one-shot research questions
  (use a plain research bot) and it never edits code.
- **Vars**: `allow_private_feeds` (bool), `category` (string), `config_path` (string), `dry_run` (bool), `fetch_timeout_secs` (int), `max_digest_items` (int), `max_items_per_feed` (int), `mode` (string), `scratch_dir` (string), `silence_alert_days` (int), `state_commit` (bool), `state_dir` (string), `workspace_dir` (string)
- **Path**: `bots/feed-watch/main.bot`

### `golden-master` — Goldy

Builds a behavioural non-regression net for an EXISTING application, and
PROVES it is not blind. Records what the app observably does (HTTP responses
across a representative catalogue, per persona), canonicalises away what is
volatile, and commits the references into `.golden-master/` in the target
repo. The part that makes it worth having: a deterministic mutation
counter-test. Known divergences are injected one at a time and the oracle
MUST see every one of them, while a no-op mutation MUST leave it silent —
the first kills a blind judge, the second kills a hysterical one. A sealed
held-out set, never shown to the hardening loop, is scored exactly once at
the final gate so the oracle cannot be tuned to pass its own training set.
The gate is a pure conjunction computed in shell and expressions, never by
an LLM: no lane may be blind, no mutant may be invalid, no collateral drift,
and the held-out set must be fully detected. An aggregate score is not
allowed to average a blind lane away. Emits `verify-oracle.sh` (one entry
point for CI and for humans) and `REPORT.md`. Repo-agnostic: it knows
nothing about any language or framework — the app's toolchain comes from the
target repo's own devbox/devcontainer.

- **Use when**:
  Use BEFORE modernising, migrating or refactoring an existing application
  whose test suite is thin, absent, or untrusted — the net you put underneath
  the work so a behavioural change cannot pass unnoticed. Typical trigger: a
  framework/runtime/database migration is planned and nothing today would
  detect an iso-functionality break. Also use to AUDIT an existing golden
  master or approval-test suite: point it at one and the mutation counter-test
  reports which references are provably blind. Do NOT use it to write unit
  tests for new code (that is a test-coverage bot's job), and do NOT use it to
  encode intended behaviour — it records the status quo, bugs included, which
  is exactly what a migration must preserve.
- **Vars**: `adversarial` (bool), `max_passes` (int), `min_corpus` (int), `mutation_floor` (int), `oracle_dir` (string), `scratch_dir` (string), `source_issue_ref` (string), `surface_scope` (string), `workspace_dir` (string)
- **Path**: `bots/golden-master/main.bot`

### `heartbeat` — Heartbeat (always-on demo)

Tool-only demo of an always-on agent. Relaunched continuously by an
`overlap: keepalive` schedule with at-most-one-live semantics and
staleness reaping — the pattern for keeping a watcher/poller/your own
long-lived bot running as a stream of fresh, individually-budgeted runs
rather than one immortal run. No LLM, no API keys.

- **Path**: `examples/keepalive/main.bot`

### `instrument` — Obsy

Observability instrumentation campaign — one capable agent wires a
repo for error tracking and standardized logs, one verified semantic
commit at a time. `scope` is an open family list: *errors* (an SDK
speaking the Sentry DSN protocol — Sentry AND GlitchTip — enabled
only when the DSN env var is set, loud non-fatal init, release/
environment tags, capture at process seams, flush on shutdown,
scrubbing), *logs* (ONE central logging seam — extended, never
replaced — leveled + structured, JSON by default on production
surfaces, error→event / warn→breadcrumb coupling into the tracker),
and opt-in *tracing* (Sentry-first transactions/spans, env-tunable
sampling). A deterministic build/test gate + bounded continuation
loop re-poke the campaign until every requested family is fully
wired, tested and documented; an opt-in tail opens the pull request.

- **Use when**:
  Use to instrument a repository for observability: add Sentry/
  GlitchTip-compatible error tracking, standardize logging onto one
  structured lib with JSON output in production, or both (default
  scope "errors,logs"; add "tracing" explicitly for performance
  traces). Point repo-specific arbitrations (which module to extend,
  which seams to wire) through `mission_notes`. Not the route for
  general feature work (feature-dev) or for building dashboards —
  this bot wires the emission side only.
- **Vars**: `baseline` (string), `dsn_env_var` (string), `max_passes` (int), `mission_notes` (string), `mr_base` (string), `mr_branch` (string), `open_mr` (bool), `scope` (string), `scratch_dir` (string), `source_issue_ref` (string), `workspace_dir` (string)
- **Path**: `bots/instrument/main.bot`

### `issue-triage` — Triagy

Lightweight single-shot card triage. ONE cheap read-classify-stamp
agent: reads a fresh board card, classifies it against the generated
bot catalog's decision tree, stamps the handler bot on the card
(typed Bot field via set_bot) plus vocabulary-consistent labels, and
leaves a one-paragraph routing comment. The card STAYS in inbox —
launching is the operator's drag to Ready (the dispatcher claims the
stamped bot). No confident fit → needs-manual-triage, Bot unset.
Auto-fires via the trigger spine on cards carrying triage:auto
(consumed one-shot); re-add the label to re-triage.

- **Use when**:
  Never dispatch work TO it — Triagy routes work to OTHER bots. It
  fires automatically on trusted-author forge issues synced to the
  board (triage:auto), on an operator's "Approve & triage" of an
  external issue (needs:approval → triage:auto swap), or on any card
  you hand-label triage:auto. Use when you want fresh issues to arrive
  pre-routed so launching is a single drag to Ready.
- **Vars**: `issue_id` (string, required)
- **Path**: `bots/issue-triage/main.bot`

### `modernize` — Morphy

Carries a repository through a programme of modernisation LOTS — steps whose
entry and exit are both deterministic gates — one gate-to-gate step at a
time. The unit of work is the lot, not the package: a dependency pipeline
whose failure path is "revert this package and continue" cannot express "the
runtime moved and nine hundred files went with it".

It holds NO knowledge of any build tool or runtime. Every command it runs is
declared in the target repository's own `.modernize/plan.yaml`, which keeps
the bot universal and lets a human audit the programme without reading it.

A lot is done if and only if three things hold together, as a conjunction and
never as a score: its declared exit_gate exits 0 on HEAD, the behavioural
oracle replays green, and NOT ONE line changed under the oracle's reference
directory. That third check is the separation of powers, verified in git
rather than trusted — the party that changes the code must not be able to
redefine what judges it, because a golden master dies by re-baselining. The
`status` field an agent writes is read as a bookmark and never as evidence.

- **Use when**:
  Use to execute a planned modernisation — toolchain, runtime, framework or
  datastore — on a repository that ALREADY has a behavioural non-regression
  net. Build the net first (see the golden-master bot): this bot refuses to
  call a lot done without one, because a green build proves the code compiles
  and never that it still behaves.
  
  Do NOT use it for routine dependency bumps — that is a dependency-upgrade
  pipeline's job, and its per-package revert semantics are the right ones
  there. Do NOT use it to decide WHAT to modernise: the programme is a human
  decision recorded in the contract, and the lot DAG in particular encodes
  compatibility knowledge that cannot be re-derived from the tree.
- **Vars**: `max_passes` (int), `only_lot` (string), `plan_path` (string), `reanchor` (bool), `source_issue_ref` (string), `workspace_dir` (string)
- **Path**: `bots/modernize/main.bot`

### `nested-subbots-demo` — Nested Subbots Demo

Zero-LLM demo of NESTED subbots (a subbot inside a subbot): main runs
stage.bot as a child, which fans out two ISOLATED step.bot grandchildren
in parallel, each pausing on its own human check. Approving both checks
unparks the whole chain (step → stage → main). Tool / compute / human /
subbot only — no API keys, runs in seconds.

- **Use when**:
  Use to demo or smoke-test the studio's inline subbot display across
  nesting levels (frames within frames, per-frame child tabs) and the
  park-on-child-human-gate chain through two levels. Not a production
  workflow.
- **Path**: `examples/nested-subbots-demo/main.bot`

### `pipeline-board-demo` — Pipeline Board Demo

Zero-LLM demo of the /pipelines board: fans out three ISOLATED sub-bots in
parallel (subbot + fan_out_each), each producing real media (PNG cover,
playable WAV track, mp4 stub) into its own run's artifact area, then
pausing on its own human review. After all three are approved from the
card's sidebar, the PARENT pauses on a final human review; approving it
lands the card in Done with the release notes as output. Tool / compute /
human / subbot only — no API keys, runs in seconds.

- **Use when**:
  Use to demo or smoke-test the pipeline board end to end: parallel sub-bot
  fan-out, child + parent human gates answered from the card sidebar, and
  produced-elements aggregation (image/audio preview) across the whole run
  tree. Not a production workflow.
- **Path**: `examples/pipeline-board-demo/main.bot`

### `revi-converse` — Revi (converse)

Conversational sibling of Revi (review-pr). Triggered when an
authorized forge user asks a focused QUESTION on an open pull
request (PR; merge request on GitLab) — `/revi <question>` (e.g.
`/revi why is the SSRF critical?`). Reads the question + the PR
diff against the branch's merge-base, formulates a CONCISE,
GROUNDED answer (a senior code reviewer's follow-up — not a
fresh full review), and posts the answer as a REPLY in the same
discussion thread via the forge_token. Never edits, fixes, or
commits code. When `/revi` is sent without a question, the
webhook handler routes to review-pr for a fresh re-review
instead.

- **Use when**:
  Use when an operator asks a follow-up question on an open PR
  about Revi's earlier findings or the diff itself — clarification,
  rationale, severity justification, alternative fixes. NOT for
  re-reviewing the PR (that is review-pr / Revi), NOT for editing
  code (that is Billy or Featurly), NOT for triaging issues on the
  board.
- **Triggers**: revi-converse, ask, converse
- **Vars**: `base_ref` (string), `converse_question` (string), `discussion_id` (string), `pr_url` (string), `replier` (string), `thread_context` (string), `trigger_note` (string), `workspace_dir` (string)
- **Path**: `bots/revi-converse/main.bot`

### `review-env` — Envy

Deploys the CURRENT workspace's already-CI-published image to the
operator-attached platform and hands back a LIVE https URL — a real
review environment (real TLS, real ingress, real DNS) for end-to-end
tests, screen and accessibility captures, and human review.

The platform lives ENTIRELY in the attached `deploy-target` skill: this
bot names no cluster, no cloud, no CLI. The operator enables one
deploy-target plugin (the platform playbook) and installs one
`deploy_credential` secret — used strictly by reference, never read —
and swapping infrastructure means swapping that pair, never the bot.
The image is the repo's own CI's: this bot never builds or pushes one,
because a review environment must serve what the forge built from the
pushed commit. The URL verdict is measured, never believed: a
deterministic gate probes the reported URL from outside the agent, with
real certificate verification, and the bot converges only on the
conjunction deployed && healthy && live.

- **Use when**:
  Use when a flow needs a live deployed environment of the current
  workspace: realistic end-to-end testing, behavioural-net captures
  against a real URL (point the net's base URL at the returned
  deployed_url), or a reviewable environment per branch. Runs standalone
  or as a subbot of a larger campaign.
  
  Do NOT use it to build or publish images (the repo's CI owns that), to
  develop features (app-dev's job — whose opt-in deploy phase shares this
  bot's skill and credential), or on a platform with no deploy-target
  plugin attached: it will refuse loudly rather than improvise one.
- **Vars**: `expected_status` (int), `image_ref` (string), `max_deploy_retries` (int), `slug` (string), `workspace_dir` (string)
- **Path**: `bots/review-env/main.bot`

### `review-pr` — Revi

Read-only cross-family code reviewer. Reviews the working-tree diff
of the current branch against its base with two independent reviewers
(Claude + GPT), merges and de-duplicates their findings (cross-family
agreement raises confidence), then publishes one issue per finding to
the iterion native kanban board (labelled severity + type +
source:revi) and writes a markdown report. Given a pull-request URL
(PR; merge request on GitLab; --var pr_url), it ALSO posts the findings onto that PR as a real
forge review — inline comments anchored to file:line with one-click
```suggestion blocks (GitHub / GitLab / Forgejo). Never edits, fixes,
or commits code — that is the improve-loops' job (Billy / Willy).

- **Use when**:
  Use when you want a PR/branch REVIEWED and its issues surfaced — to
  the board for triage and/or posted directly onto the PR (pass
  --var pr_url) as inline comments + ```suggestion fixes — but NOT
  auto-fixed. Read-only: Revi reports; Billy (branch-improve-loop)
  reviews AND fixes AND commits.
- **Triggers**: review-pr, pr-review, review
- **Vars**: `base_ref` (string), `forge_publish_token` (string), `forge_publish_url` (string), `gate_context` (string), `gate_enabled` (bool), `gate_severity` (string), `max_findings` (int), `mono_family` (string), `post_to_board` (bool), `pr_review_mode` (string), `pr_url` (string), `prior_pushback` (string), `report_path` (string), `review_mode` (string), `scope_notes` (string), `severity_threshold` (string), `workspace_dir` (string)
- **Path**: `bots/review-pr/main.bot`

### `rgaa-audit` — Acci

Universal RGAA 4.1.2 accessibility auditor (read-only) — one audit
agent over deterministic gates. Statically reviews a project's UI
source (HTML, JSX/TSX, Vue, Twig, CSS) against the 106 RGAA criteria
across 13 themes (WCAG 2.1 AA basis), guided by the bundled
rgaa-criteria-* skills and — when the target uses the Système de
Design de l'État — the DSFR MCP tools. Scores each applicable
criterion C / NC / NA, classifies non-conformities by priority
(🔴 Bloquant / 🟠 Majeur / 🟡 Mineur), exports a dated Markdown
conformance report under `audits/` and (optionally) posts one board
issue per non-conformity, labelled by severity + theme + criterion.

Static analysis only: it reads source code, it does not launch a
browser or run a DOM scanner. A deterministic scan_health gate
hard-fails the run if the RGAA criteria skills are not available or the
review examined no files while a UI surface exists — so a broken setup
cannot masquerade as a clean bill of health.

- **Use when**:
  Use for a READ-ONLY accessibility audit of a web UI codebase: produce
  an RGAA conformance report and surface non-conformities (missing alt
  text, unlabelled fields, low contrast, keyboard traps, broken heading
  hierarchy, missing ARIA status messages). Emits a report + board
  findings; does not fix. Pre-release accessibility review or recurring
  conformance tracking. For FIXING accessibility issues, use Willy
  (whole-improve-loop) with the rgaa preset.
- **Vars**: `findings_cap` (int), `post_to_board` (bool), `report_dir` (string), `scope_globs` (string), `scope_notes` (string), `workspace_dir` (string)
- **Capabilities**: board.create, board.label, board.read
- **Path**: `bots/rgaa-audit/main.bot`

### `sec-audit-deps` — Depsy

Universal supply-chain malware auditor. Enumerates installed
dependencies per ecosystem (npm/yarn/pnpm, pip/poetry/uv,
go.mod/vendor, …), looks each `(ecosystem, name, version,
checksum)` triple up against a package cache (per-run scratch by
default; point `cache_path` at
`~/.iterion/security-cache/packages.jsonl` for host-wide reuse)
to skip packages that
were already analysed at an acceptable scanner version, runs
language-specific static heuristics on the rest (install-time
hooks, eval, obfuscation, fetch+exec, base64 blobs, init()
side-effects), passes the structured signals to an LLM reviewer
with strict JSON output schema (no-package-malware style),
combines heuristic + LLM scores by `max()`, buckets into
LOW/MEDIUM/HIGH, emits findings to the iterion kanban board and
appends a fresh line to the package cache.

Cross-run memory: cache is host-wide and shared across repos
because a published package version is universal. The
`scanner_version` field lets the bot opportunistically rescan
packages analysed by older versions.

Per-language extensibility: ships JS/TS (npm), Python (pip/poetry)
and Go (modules + vendor/), plus a language-agnostic pass on
embedded binaries and locale anomalies. Add a language by dropping
a `skills/lang-<id>.md` and an entry in the `heuristic_scan`
router.

- **Use when**:
  Use for a READ-ONLY supply-chain audit of installed dependencies:
  post-install triage, malware / typosquat / install-hook detection,
  CVE baseline. Emits findings to the board; does not fix.
- **Vars**: `cache_dir` (string), `cache_path` (string), `cache_ttl_days` (int), `scan_dir` (string), `scanner_version` (string), `scope_notes` (string), `severity_threshold` (string), `workspace_dir` (string)
- **Path**: `bots/sec-audit-deps/main.bot`

### `sec-audit-source` — Seki

Universal source-code security auditor. Detects languages and
frameworks, runs language-specific SAST (semgrep + gosec / bandit /
npm audit) plus language-agnostic scanners (gitleaks for secrets,
trivy fs for filesystem misconfig, semgrep --config=auto), triages
the raw output with an LLM against a finding taxonomy, confronts
candidates against `.sec-audit/fp-known.yaml` to suppress
curated false positives, revalidates surviving candidates with a
two-phase judge (anti-façade), then writes findings to the iterion
native kanban board (one issue per finding, labelled by severity +
type) and exports a markdown summary.

Cross-run memory: false positives confirmed by the operator (or by
the revalidate judge after explicit human reasoning) are written
back to `.sec-audit/fp-known.yaml` in the repo so the next
run does not re-surface them. Entries are reviewable + editable by
humans.

Per-language extensibility: ships JS/TS, Go, Python and a
language-agnostic baseline. Add a language by dropping a
`skills/lang-<id>.md` and an entry in the `lang_scan` router.

- **Use when**:
  Use for a READ-ONLY security audit of the source itself (injection,
  SSRF, IDOR, broken auth, hardcoded secrets, crypto misuse,
  deserialisation, path traversal, misconfig). Emits findings to the
  board; does not fix. Pre-release hardening / PR-scope review.
- **Vars**: `confirm_threshold` (int), `context_path` (string), `context_ttl_days` (int), `deepsec_concurrency` (int), `deepsec_out` (string), `deepsec_process_limit` (int), `deepsec_root` (string), `diff_base` (string), `enable_deepsec` (bool), `enable_project_context` (bool), `file_filter` (string), `findings_cap_per_file` (int), `force_context_refresh` (bool), `fp_append_policy` (string), `fp_path` (string), `hard_stop_categories` (string), `matchers_dir` (string), `max_fix_per_run` (int), `min_generic_scanners` (int), `patch_attempts` (int), `patch_dir` (string), `records_dir` (string), `records_ttl_days` (int), `remediate` (bool), `remediation_mode` (string), `scan_dir` (string), `scanner_version` (string), `scope_notes` (string), `severity_threshold` (string), `shard_concurrency` (int), `shard_size` (int), `workflow_path` (string), `workspace_dir` (string)
- **Path**: `bots/sec-audit-source/main.bot`

### `secured-renovacy` — Renovacy

Multi-stack agentic dependency upgrade pipeline. Updates every kind of
dependency (libs, languages, frameworks, devops, ci_cd) across every
recognised package ecosystem, aligns consuming code on breaking
changes, cross-references CVE feeds, and runs heuristic malware
detection on the new versions + transitively-introduced libs. Phase 2
closes with ONE review campaign over the run's cumulative diff,
gated by a deterministic build/test verify (ADR-058) — fixes land as
in-stride commits until the diff is clean and the tree is green.

- **Use when**:
  Use when dependency risk is the priority: CVE alerts, stale
  lockfiles, version bumps. MUTATES dependency manifests/lockfiles and
  aligns consuming code on breaking changes. Ask before running with
  major_policy: attempt.
- **Vars**: `fix_loop_default` (int), `fix_loop_major` (int), `major_policy` (string), `max_packages_per_run` (int), `max_review_passes` (int), `override_install_cmd` (string), `override_upgrade_cmd` (string), `scope` (string), `scratch_dir` (string), `update_scope` (string), `user_prompt` (string), `workspace_dir` (string)
- **Path**: `bots/secured-renovacy/main.bot`

### `supply-shield` — Shieldy

Global supply-chain MALWARE shield. PR / push-driven, diff-scoped
sibling of sec-audit-deps (Depsy): it inspects only the dependency
versions a change ADDS or UPGRADES (it diffs the changed lockfiles),
looks each `(ecosystem, name, version, checksum)` triple up against
the host-wide package cache so a version is analysed once and reused
across runs / PRs / repos, and runs language-specific malware
analysis on the rest — js-x-ray AST analysis for npm (the
@nodesecure analyzer the no-package-malware project relied on),
install-hook + SHA-512 checksum-integrity checks, osv-scanner /
trivy CVE baseline, and an LLM deep-read of install scripts /
entry points when heuristics are inconclusive. A deterministic
coverage gate hard-fails when the scanner floor did not run so a
missing analyzer never reads as "0 malware found". Confirmed findings
are reported back onto the PR (merge request on GitLab) via the
native forge API (GitHub / GitLab / Forgejo) — a sticky summary comment, inline review comments,
and a SARIF / code-scanning upload — and emitted to the kanban board.

Cross-run memory: the package cache is shared (a published package
version is the same artifact everywhere), so reports are accessible
across runs. Point `cache_path` at `$HOME/.iterion/security-cache/
packages.jsonl` for host-wide cross-repo dedup.

- **Use when**:
  Use to gate dependency changes on a PR / push for MALWARE
  (install-hook backdoors, obfuscated/encoded payloads, network-exfil,
  typosquat / homoglyph names, supply-chain re-publish). Diff-scoped by
  default; pass scope_mode=full for a whole-tree audit. Reports back on
  the forge and the board; does not fix. For a CVE-focused gate use the
  companion bot supply-shield-cve (Vulny).
- **Vars**: `base_ref` (string), `cache_dir` (string), `cache_path` (string), `cache_ttl_days` (int), `forge_marker` (string), `head_ref` (string), `pr_ref` (string), `report_path` (string), `sarif_dir` (string), `sarif_path` (string), `scan_dir` (string), `scanner_version` (string), `scope_mode` (string), `scope_notes` (string), `severity_threshold` (string), `workspace_dir` (string)
- **Path**: `bots/supply-shield/main.bot`

### `supply-shield-cve` — Vulny

Global supply-chain CVE shield. PR / push-driven, diff-scoped CVE
sibling of supply-shield (Shieldy): same pipeline, but the analysis
axis is KNOWN VULNERABILITIES, not malware. It inspects only the
dependency versions a change adds or upgrades (it diffs the changed
lockfiles), matches each `(ecosystem, name, version)` against the
advisory databases via a universal lockfile CVE floor (trivy fs +
osv-scanner, no install needed) plus the per-ecosystem SCA scanners
(npm audit / pip-audit / govulncheck), validates each advisory
against the resolved version (affected range / fixed version / Go
reachability) with an LLM reviewer, and reports confirmed CVEs back
onto the PR (merge request on GitLab) via the native forge API
(sticky comment, inline
review, SARIF / code-scanning) and the kanban board. A deterministic
coverage gate hard-fails when the CVE floor did not run so a missing
scanner never reads as "0 CVEs found".

Cross-run memory: verdicts are cached as `kind: cve` lines with a
short TTL + `advisory_db_date`, because a clean-today version can gain
a CVE tomorrow as advisories land. Point `cache_path` at
`$HOME/.iterion/security-cache/packages.jsonl` for host-wide dedup.

- **Use when**:
  Use to gate dependency changes on a PR / push for KNOWN CVEs
  (vulnerable transitive/direct pins, advisories with an available fix).
  Diff-scoped by default; pass scope_mode=full for a whole-tree CVE
  baseline. Reports back on the forge and the board; does not fix. For a
  MALWARE-focused gate use the companion bot supply-shield (Shieldy).
- **Vars**: `base_ref` (string), `cache_dir` (string), `cache_path` (string), `cache_ttl_days` (int), `forge_marker` (string), `head_ref` (string), `pr_ref` (string), `report_path` (string), `sarif_dir` (string), `sarif_path` (string), `scan_dir` (string), `scanner_version` (string), `scope_mode` (string), `scope_notes` (string), `severity_threshold` (string), `workspace_dir` (string)
- **Path**: `bots/supply-shield-cve/main.bot`

### `test-coverage` — Testy

Autonomous test-coverage augmentation — one capable agent, its natural
flow, minimal framing. Points at a target area (a path, package, or
free description — or nothing, in which case Testy picks the
lowest-coverage / most-critical / most-recently-changed code itself),
builds a living todo of coverage gaps, and closes them one verified
`test:` commit at a time with the repo's OWN test framework.

The operator chooses which test types to add via checkboxes (unit /
integration / e2e) plus a free-text field for any other kind
(property-based, contract, snapshot, smoke, performance, …). When
nothing is checked, Testy chooses the types that fit the code and the
repo's conventions.

Anti-façade by design: the success metric is NOT coverage percentage
— it is meaningful tests that would CATCH A REAL REGRESSION. The
campaign's contract enforces the mutation test (a test must fail if
the code under test were stubbed broken) and forbids zero-assertion
tests, tautologies, unverified snapshots and over-mocking; a
deterministic gate proves the repo's own suite still passes AND that
genuinely-new test code actually landed in the diff.

Stack-agnostic: how to detect the test runner, where tests live, and
how to write each test type idiomatically lives in the bot's skills,
not in the workflow — so adding a language needs no DSL edit.

- **Use when**:
  Use when a repo (or a specific area of it) is under-tested and needs
  REAL tests added — a thin-coverage package, a critical path with no
  tests, freshly-landed code that shipped without them. Testy writes
  and commits the tests (semantic `test:` commit on cross-family
  approval); it does not change product behaviour.
  
  Do NOT use to review an existing branch/PR (that is Billy /
  branch-improve-loop) or to build a new feature (that is Featurly /
  feature-dev — though feature-dev already writes tests for the feature
  it ships). Testy's job is coverage of code that already exists.
- **Triggers**: test, tests, testing, coverage, test-coverage, unit-test, add-tests, augment-tests
- **Vars**: `baseline` (string), `extra_test_kinds` (string), `max_passes` (int), `scratch_dir` (string), `target` (string), `test_e2e` (bool), `test_integration` (bool), `test_unit` (bool), `workspace_dir` (string)
- **Path**: `bots/test-coverage/main.bot`

### `ultra11y` — Ally

Engine-backed WCAG 2.2 AA / RGAA accessibility auditor (read-only), with a
pull-request mode. The ultra11y static engine produces the findings — one
per criterion, anchored file:line, with a stable id — and ONE adjudication
agent rules on the criteria a static pass cannot decide (alt relevance,
link purpose, reading order), each verdict justified or grounded. The
engine's own gates then refuse the run: `verify --apply` fail-closes on an
unjustified verdict or an ungroundable non-conformity, and `check` rejects
a report citing a criterion that does not exist.

Given a pull request (`pr_url` / `base_ref`, set by iterion for any bot
launched on a PR) it audits exactly what the branch introduced; otherwise
it audits the whole UI surface and writes a dated conformance report under
`audits/`, optionally filing one board issue per criterion.

Criteria that need a rendered page (contrast, visible focus, zoom, reflow)
are reported as RESIDUAL RISKS, never as conforming. No browser is
launched; nothing is fixed or committed.

- **Use when**:
  Use when accessibility findings must be DEFENSIBLE — a dated WCAG 2.2 AA or
  RGAA conformance deliverable, or a per-PR accessibility check whose findings
  carry file:line anchors and a criterion each. The detection is deterministic,
  so a finding is reproducible without a model in the loop.
  
  Read-only: Ally reports. For FIXING accessibility issues, use Willy
  (whole-improve-loop) with the rgaa preset. Acci (rgaa-audit) is the sibling
  to prefer for RGAA theme-by-theme reasoning over a Système de Design de
  l'État UI, where the DSFR MCP tools carry the reference markup.
- **Triggers**: ultra11y, a11y, accessibility, wcag
- **Vars**: `base_ref` (string), `engine_version` (string), `findings_cap` (int), `force_jsx` (bool), `post_to_board` (bool), `pr_url` (string), `prior_pushback` (string), `report_dir` (string), `report_lang` (string), `run_dir` (string), `scope_globs` (string), `scope_notes` (string), `standard` (string), `workspace_dir` (string)
- **Capabilities**: board.create, board.label, board.read
- **Path**: `bots/ultra11y/main.bot`

### `vuln-watch` — Senti

Inventory-scoped vulnerability sentinel (hourly watch, zero LLM). One
deterministic run mode over a git-versioned state in the target
workspace: poll the security sources, match them against the
workspace's technology inventory, and post an actionable alert to
chat (Mattermost/Slack incoming webhooks) within the hour — for
exactly the vulnerabilities that are EXPLOITED and that touch a
technology the inventory says you run.

Three detection lanes, all structured (no LLM anywhere — the
compiled workflow contains no agent/judge node, so a run can neither
spend a token nor show a project name to a model):
- GitHub org Dependabot alerts (library-level, per repo): new alerts
  grouped by advisory, repos joined to inventory projects.
- Advisory feeds (CERT-FR-style): new publications matched
  word-boundary against the inventory technologies; CERT-FR's
  structured per-publication JSON (cves + affected systems) is used
  when available.
- Exploitation signals: CISA KEV (diff + join) and EPSS scores — the
  anti-noise core. The default policy alerts ONLY on an exploitation
  signal (KEV entry, alert-class advisory, EPSS ≥ threshold); an
  ordinary new critical stays silent, recorded in an observation
  window, and RE-FIRES the day its exploitation signal lights up.

Dedup is deterministic (CVE/GHSA alias sets in a seen state), alerts
carry the affected projects/repos joined from the inventory, message
wording is label-templated (any language via config), and source
failures are explicit: a configured org without a usable token fails
the run, a source silent for too long is announced on the sinks.

- **Use when**:
  Use to watch published vulnerabilities (CVE/GHSA/CERT-style
  advisories) against a maintained inventory of the technologies and
  projects an organisation actually runs, with hourly deterministic
  alerting to chat and exploitation-driven noise control. Requires the
  target workspace to carry a vuln-watch.json config + an
  inventory.json (see skills/senti-config.md). Not an editorial news
  digest (use feed-watch), not a PR dependency gate (use
  supply-shield-cve), not a code audit (use sec-audit-*); it never
  edits code.
- **Vars**: `allow_private_sources` (bool), `config_path` (string), `dry_run` (bool), `fetch_timeout_secs` (int), `inventory_path` (string), `max_alerts_per_run` (int), `mode` (string), `observe_window_days` (int), `scratch_dir` (string), `source_stale_hours` (int), `state_commit` (bool), `state_dir` (string), `workspace_dir` (string)
- **Path**: `bots/vuln-watch/main.bot`

### `whats-next` — Nexie

Conversational co-CTO. ONE adaptive agent (claude_code + opus, full
board capabilities, bundled skills) in a standing chat loop: the
operator talks, Nexie analyses the board and the repo, recommends
(recommendation-first, never raw dumps), creates/curates/dispatches
tickets, verifies whether issues are still relevant against the
code and git history, and keeps a cross-session CONTEXT_BRIEF.
On roadmap-scale asks she leads the full study cycle end-to-end:
parallel audit fan-out → chantiers with now/next/later tiering +
quick-wins + blind spots → grouped operator arbitrage → framed
tickets (context / done-criteria / verify) with a limited ready
lot → factory observation + evidence-based bilan.
Every turn ends at a budget-free chat pause — the session stays
reachable for days; only an explicit "close" ends it. Direct action
on targeted instructions; dry-run + confirmation before bulk or
destructive board changes.

- **Use when**:
  Use to decide and drive what happens next on a repo: discuss the
  board, get a recommendation (quick wins, priorities), create or
  clean up tickets, and dispatch work to the right bot — all in one
  ongoing conversation. Also the entry point for a full roadmap
  study: ask "quels sont les prochains chantiers ?" (or dispatch a
  study-titled card) and Nexie runs the audit→synthesis→arbitrage→
  board→bilan cycle. The orchestrator / entry point, not a worker
  bot.
- **Vars**: `initial_message` (string), `scope_notes` (string), `workspace_dir` (string)
- **Path**: `bots/whats-next/main.bot`

### `whole-improve-loop` — Willy

Whole-codebase improvement CAMPAIGN on one axis — one capable agent, its
natural flow, minimal framing. `improvement_prompt` is THE AXIS: one
determined improvement applied everywhere it applies (e.g. "split every file
over 600 lines into cohesive units", "converge duplicated X onto a shared
helper", "make error handling use pattern Y"). It is a refactoring-campaign
engine, not a scanner. A single adaptive agent (claude_code, full tools,
whole-repo context) works exactly as in a productive human session: a LIVING
todo list born from a brief exploration (never frozen upfront phases), and
for each site the repeated unit locate → smallest change → build → test →
COMMIT, ~a few edits per commit, validation BEFORE the commit, committing
each site AS it finishes (never batch). A deterministic, stack-agnostic
build/test gate re-checks the tree after each pass (the anti-Goodhart truth
oracle: the agent can't self-certify); RED routes back to the agent with the
failure log to fix what it broke, green + axis-complete converges. git IS the
durable state — an interrupted / budget-capped run keeps every committed
site, and a re-dispatch re-runs the campaign, which reads `git log` and
continues (no worklist/cursor scratch). Bounded by a max_passes cap so a
pathological axis terminates. An optional PR path (merge request on
GitLab) ships the series of per-pass commits. Supersedes the v1 axis-sweep (ADR-057). See
docs/references/productive-session-patterns.md.

- **Use when**:
  Use on EXISTING code to apply one determined, cross-cutting improvement AXIS
  across the whole codebase, site by site — the campaigns a human runs as a
  todo-list + frequent incremental commits (split the largest files, converge
  N call sites onto a shared primitive, make a pattern consistent everywhere).
  Pass the axis as improvement_prompt (empty = it picks the single
  highest-value cross-cutting improvement it can name). One capable agent
  commits each site in stride and the run converges when the agent reports the
  axis fully applied and a deterministic build/test gate is green, so long runs
  always leave landed, reviewable commits. For an open-ended "find whatever is
  wrong" production-readiness audit (no single axis), point a review-loop bot
  at the tree instead — this bot needs an axis to sweep.
- **Vars**: `baseline` (string), `improvement_prompt` (string), `max_passes` (int), `mr_base` (string), `mr_branch` (string), `open_mr` (bool), `scope_globs` (string), `scope_notes` (string), `scratch_dir` (string), `source_issue_ref` (string), `workspace_dir` (string)
- **Path**: `bots/whole-improve-loop/main.bot`

### `wiki-gen` — Wikky

Wiki generator — one capable agent builds and incrementally maintains
a navigable, Open-Knowledge-Format wiki for whatever repository it is
pointed at, in any language. It surveys the code, plans the concept
pages and their relationships, and writes a structured wiki tree
(architecture/, workflows/, domain/, …) under wiki/ with a quickstart
entrypoint — every claim grounded in the source, never invented.

Deterministic by construction: after each authoring pass a tool
regenerates every directory index from the pages' frontmatter, and a
validator gate fails the run on invalid OKF frontmatter, a dead
intra-wiki link, or any write outside the wiki tree — so the agent
cannot rubber-stamp a broken or hallucinated wiki. A persistent,
out-of-tree git_head cache lets a scheduled run skip entirely when the
wiki is already current for the exact commit.

The OKF output (type-only-required YAML frontmatter + Markdown links as
concept-relationship edges) is a standard, tool-agnostic interchange
format, directly ingestible by a knowledge-graph explorer.

Ships 2 skills: wiki-authoring (the operating playbook) and okf-format
(the frontmatter + link-graph contract the validator enforces).

- **Use when**:
  Use to bootstrap a navigable wiki for a repository that has none, or to
  keep an existing wiki/ tree current as the code evolves (nightly, or on
  demand). Wikky OWNS the wiki/ tree and writes only there — it never
  edits source. Reach for Doki (docs-refresh) instead when the goal is to
  fix a repo's EXISTING hand-authored docs (README/docs/**) against the
  code, editing them in place.
- **Vars**: `bundle_self_path` (string), `code_scope_globs` (string), `excluded_dirs` (string), `issue_id` (string), `max_passes` (string), `okf_version` (string), `scope_notes` (string), `wiki_cache_path` (string), `wiki_dir` (string), `workspace_dir` (string)
- **Path**: `bots/wiki-gen/main.bot`

<!-- ITERION:CATALOG:GENERATED:END -->

## Issue-creation mapping (consumed by `emit_action`)

Each `roadmap_item` lands on the native kanban board as one
issue. The data model on the wire is:

| `roadmap_item` field | Native tracker field | CLI flag (today) |
|---|---|---|
| `title`              | `title`              | `--title`        |
| `body`               | `body`               | `--body`         |
| `assignee`           | `assignee`           | `--assignee`     |
| _(bot name, e.g. `feature-dev`)_ | `bot` (string)       | `--bot` (on `create`) |
| `args` (object)      | `bot_args` (`map[string]string`) | `--bot-arg key=value` (on `create`) |

`bot` and `bot_args` are dedicated typed fields on
`native.Issue` (`pkg/dispatcher/native/issue.go`; JSON
keys `bot`, `bot_args`); they are NOT stored under the freeform
`Fields` map. Set them via `iterion issue create --bot <name>
--bot-arg key=value` (repeatable; values are kept verbatim, so
comma-containing glob lists survive intact), the REST API (POST/PATCH
`/api/v1/native/issues` with `{ "bot": "...", "bot_args": { ... } }`),
or direct `store.Create/Update` calls. `bot_args` is usable today: the
dispatcher merges it on top of the rendered `dispatch.vars`
key-by-key, with `bot_args` winning on shared keys (see `pkg/dispatcher/loop.go`, `buildSpec`).

Concrete `bot_args` example — for an issue assigned to
`feature-dev` with `args = {"feature_prompt": "Add CSV export"}`:

```json
{
  "title": "Add CSV export",
  "assignee": "feature-dev",
  "bot": "feature-dev",
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
`assignee_dispatch:` in the dispatcher YAML (or the registry
fallback) to map that assignee to a workflow + var template — see
`docs/dispatcher.md` §Routing by issue assignee.

## Verification ritual (emit_action)

Before creating each issue:

1. If `assignee != ""`, look it up in the persona table above. If
   it is not one of the listed bots, AND it does not correspond to
   a `.bot` file the explorer surfaced — strip to `""` and add
   label `needs-manual-triage`. NEVER invent an assignee.
2. Empty assignee is FINE. The issue lands without an assignee
   and the operator triages.

## What you do NOT do

- You do NOT shell out `iterion run …` directly. The bot used
  to do that; it doesn't anymore.
- You do NOT enumerate bots from the user's free-text alone.
  Walk the decision tree against the explore summary.
- You do NOT recommend an `assignee` whose card is not in the
  catalog above (and whose `.bot` file the explorer did not
  surface).
- You do NOT recommend more than one `next_action`.

## Backend selection

When authoring a `.bot` (e.g. via `feature-dev`), each agent/judge
node picks where its LLM call runs:

- `backend: "claude_code"` — the official Claude Code CLI. Use for
  nodes that need real tool/shell access (implementers, fixers) or
  the native Skill tool / Claude Code MCP servers.
- `backend: "claw"` — in-process, multi-provider. Use for read-only
  nodes (judges, reviewers, planners) and for any non-Anthropic model
  (`openai/*` models MUST use `backend: "claw"`).
- Omit `backend:` to let the runtime auto-detect from host credentials
  (see `docs/backends.md`).

### Per-node `provider:` and the fallback chain

`provider:` is a credential-routing hint, resolved per node after
`${VAR}` expansion. A **single value** routes one credential lane; a
**comma-separated, ordered chain** declares fallbacks that the runtime
walks transparently when a provider fails *beyond its retry budget*:

```yaml
agent reviewer:
  backend: "claude_code"
  provider: "zai,anthropic"        # try z.ai; on hard failure, fall through to Anthropic
  model: "claude-opus-4-8"
```

- Known hints: `anthropic`, `zai`, `openai`, `auto` (≡ default
  precedence). Unknown tokens are warned at compile time (**C087**)
  and ignored at run time.
- On a hard provider failure beyond retries, the executor re-issues the
  same call against the next hint and logs **one** fall-through note —
  the operator sees a route change, not a failure. The run only fails
  if every provider in the chain is exhausted.
- This **generalises `RESCUE_PROVIDER`**: `provider: "${RESCUE_PROVIDER:-zai},anthropic"`
  starts on z.ai (or whatever `RESCUE_PROVIDER` overrides to) and falls
  back to Anthropic automatically — no env flip + manual resume needed.
- The chain is honoured by **`claude_code`** today (same-API family:
  `anthropic`↔`zai`↔Anthropic-compatible facades, identical model id).
  `claw` derives its provider from the `model:` prefix and `codex`
  ignores the hint, so a multi-element chain on those backends is a
  no-op — the runtime uses only the first provider and the compiler
  warns (**C088**). For cross-provider failover on `claw`, vary the
  `model:` instead.
- Single-value `provider:` (and unset) behaves exactly as before —
  the chain form is purely additive.
- For what `provider:` cannot do — continuing on **another backend**
  when a CLI forfait's window shuts — use **`fallbacks:`** (ADR-087):
  named routes carrying their own backend + model, tried in declaration
  order, filtered by `on:` (default `[usage_window, unavailable]`). A
  fall-through emits a `model_fallback` event and stamps
  `_fallback_used` / `_served_by` on the node output, so a deterministic
  gate can fail closed on a degraded input. See
  [docs/backends.md](../../docs/backends.md) §Cross-backend fallback routes.
