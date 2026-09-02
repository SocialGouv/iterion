---
name: iterion-bot-catalog
description: Catalog of iterion bots — pick the bot that should execute each card, and stamp it with `set_bot`. The dispatcher routes by that typed bot field.
---

# Iterion Bot Catalog — the bot Nexie stamps on each card

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

Nexie is a **conversation**, not a pipeline: v3 is four nodes
(`seed` → `nexie` → `gate` → `chat`, looping back into `nexie`),
and this catalog is read inside the one `nexie` agent turn that
frames work onto the board.

Its job there is routing: for each card, pick the bot that should
EXECUTE it and stamp that bot with **`set_bot`** — the typed field
the dispatcher's `Claim` reads first. `assign_issue` is reserved
for human ownership and is never a bot selector here. Leave the
bot unstamped when no catalog bot fits, and say so in the reply
rather than guessing.

**Trust check first**: this catalog enumerates bots discovered in
the workspace. If the workspace ships no bots (none of the cards
below resolve), stamp nothing and tell the operator — a card with
a bot that does not exist is worse than a card with none.

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

Nexie stamps the typed `Bot` field (`set_bot`) on every card it frames,
so routing does not depend on the assignee fallback at all. That fallback
remains for cards created outside Nexie — by hand, or synced from an
external tracker.

## Decision tree — pick the bot to stamp on a card

Walk top-to-bottom; first match wins. The right-hand column is the value
for `set_bot` (and, for a hand-created card, the legacy `--assignee`
fallback).

| If the work sounds like… | → bot |
|---|---|
| "where should this project go next?", "long-term vision", "architectural direction", "strategic axes for the next quarter/year" — STRATEGIC (a quarter+ horizon) AND the project is mature/stable | `evolve` |
| "implement feature X", "add capability", "build the thing" | `feature-dev` |
| "build a new bot for Y" / "create a workflow that does Y" — the catalogue lacks a fit and we need to author one | `feature-dev` (with `feature_prompt` pointing at the new `.bot` file to create) |
| "build a new app from scratch", "greenfield from a prompt" — no existing codebase to extend | `app-dev` |
| "finish the half-built thing", "close the gaps against the spec" — gap-driven completion, not a new feature | `feature-gap-fill` |
| "add tests", "raise unit-test coverage", "the suite misses X" | `test-coverage` |
| "we have no end-to-end tests for X", "keep the feature×coverage matrix honest" | `e2e-coverage` |
| "wire error tracking / structured logs / tracing", "make it observable" — INSTRUMENTATION, a specific artefact, not a general improvement axis | `instrument` |
| "review the whole codebase", "audit production-readiness", "find bugs anywhere" | `whole-improve-loop` |
| "focus on axis X" (perf / DX / refactoring) ACROSS the codebase — improvement loop, not detection. For observability specifically, prefer `instrument` above | `whole-improve-loop` (with `--var improvement_prompt=…`) |
| "review this branch", "review the PR", "fix the diff against main" — review AND fix AND commit | `branch-improve-loop` |
| "review this PR / branch and just REPORT the issues" — read-only review, posts findings to the board, does NOT fix or commit | `review-pr` |
| "upgrade dependencies", "patch CVEs", "bump versions", "renovate" — MUTATING (writes package.json / go.mod / lockfiles) | `secured-renovacy` |
| "audit the docs", "find code↔doc drift", "doc/code alignment", "fix outdated README/CLAUDE.md" | `docs-refresh` |
| "document what the product does for its users", "functional / business documentation", "doc produit", "the user guide is out of date" — for a NON-TECHNICAL audience, in a DEDICATED docs repo, from one or more source repos named by a product catalog | `product-docs` |
| "audit the source for vulns", "find injection / SSRF / IDOR / secrets", "OWASP source scan" — DETECTION (writes findings, not fixes) | `sec-audit-source` |
| "audit dependencies for malware / typosquats / install hooks", "supply-chain check", "post-`npm install` triage" — DETECTION across installed deps | `sec-audit-deps` |
| "migrate the framework major", "modernise the legacy app", "the runtime moved and everything with it" — behaviour-preserving, gate to gate | `modernize` for ONE lot; `campaign` to supervise the whole programme |
| "pin down what this app actually does before we touch it", "golden master", "non-regression net" — record behaviour and prove the net is not blind | `golden-master` |
| "the modernisation is blocked on a judgement call" — a divergence the written doctrine must settle | `arbitrate` (needs the target repo's own arbitration doctrine) |
| "accessibility audit", "WCAG / RGAA conformance" | `ultra11y` — but read the Ally vs Acci distinguisher below first |
| "generate / refresh the project wiki", "a navigable knowledge base" | `wiki-gen` |
| "map our ADRs", "which decisions were never written down?" | `adr-cartograph` |
| "is ADR-NNN still right?", "re-challenge that decision" — human-gated, ends in keep/change/addendum | `adr-rechallenge` |
| "set up a reproducible toolchain", "we need a devbox.json" | `devbox-setup` |
| "watch these feeds / releases and digest them for us" — recurring veille | `feed-watch` |
| "give me a live URL for this branch", "a real review environment" | `review-env` |
| architectural choice, hiring, prioritisation meeting, alignment | `""` |
| operator is vague or it's cross-cutting | `""` |
| long-term theme (a quarter+ horizon) on a mature/stable project | `evolve` (it accumulates the vision + proposes evolutions) |
| long-term theme on a greenfield / unstable project | `""` (vision is premature — drive stability first) |

When in doubt, prefer `""` and let the operator triage manually
in the board UI. An unstamped card is honest; a wrong bot wastes a
run — and a bot name that does not exist fails at claim time, far from
the operator who could have fixed it.

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
  answers "what should we work on this week?" — one clear next move,
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

### `docs-refresh` (Doki) vs `wiki-gen` (Wikky) vs `product-docs` (Prody) — the three documentation bots

All three write `.md` and commit. They differ on **who reads the
result**, and secondarily on **where the pages live relative to the
code**. Settle the audience first; the topology follows.

- `docs-refresh` / Doki: a **developer** audience. It aligns the repo's
  OWN existing technical docs (README, CLAUDE.md, `docs/**`) against
  the code, IN PLACE, in the same repo. Repairs stale prose and writes
  what is missing.
- `wiki-gen` / Wikky: a **developer** audience too, but it OWNS a
  parallel artifact — a navigable OKF `wiki/` tree it generates and
  maintains in the code repo. Reach for it when the repo needs a
  navigable knowledge base it does not have, not when existing docs
  need fixing.
- `product-docs` / Prody: a **business / end-user** audience. It writes
  what the product does for the people who USE it — role by role,
  journey by journey, in French — in a **dedicated documentation
  repository**, grounded in the source code of the N **other**
  repositories a product catalog names. It never touches those source
  repos, and it publishes no technical content at all.
- Tie-break — **audience**: "could a non-developer act on this page?"
  Yes → Prody. No → Doki or Wikky.
- Tie-break — **topology**: docs and code in the SAME repo → Doki or
  Wikky. Docs in their own repo, code in one or more others, with a
  catalog naming them → Prody (it is the only one that clones sources).
- Prody needs two inputs with no default: `catalog_path` and
  `product_id`. If the operator cannot name a product catalog, the work
  is probably not Prody's — ask before routing.

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
<!-- ITERION:CATALOG:GENERATED:END -->

## Issue-creation mapping

Each framed unit of work lands on the native kanban board as one
issue. The data model on the wire is:

| What you frame | Native tracker field | CLI flag |
|---|---|---|
| the card's title     | `title`              | `--title`        |
| the card's body      | `body`               | `--body`         |
| the HUMAN owner      | `assignee`           | `--assignee`     |
| the EXECUTING bot (e.g. `feature-dev`) | `bot` (string) | `--bot` (on `create`) |
| per-card var overrides | `bot_args` (`map[string]string`) | `--bot-arg key=value` (on `create`) |

The two identity fields are not interchangeable: `bot` is what the
dispatcher routes on, `assignee` is human ownership. Through the board
tools that is `set_bot` vs `assign_issue`.

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

Concrete `bot_args` example — a card `feature-dev` should execute,
carrying its own `feature_prompt`:

```json
{
  "title": "Add CSV export",
  "bot": "feature-dev",
  "bot_args": { "feature_prompt": "Add CSV export" },
  "labels": ["horizon:next-action", "source:whats-next"]
}
```

(No `assignee`: nobody human owns it yet. Setting `assignee:
"feature-dev"` would be the legacy selector shape — harmless but
misleading, since `bot` already decides.)

Horizon labels — the current vocabulary is `now` / `next` / `later`
(plus `theme` for a strategic item never dispatched directly):

```
horizon:now    + source:whats-next   → start this week
horizon:next   + source:whats-next   → this month
horizon:later  + source:whats-next   → this quarter
horizon:theme  + source:whats-next   → strategic, not dispatched
```

The `next-action` / `short-term` / `long-term` spellings are **legacy**.
Treat them as equivalent when filtering old cards, emit the current ones
on new cards, and do not mass-relabel without an explicit operator ask —
see `iterion-label-vocabulary`.

`--assignee <bot_name>` plus `assignee_workflows:` /
`assignee_dispatch:` in the dispatcher YAML remains supported for
back-compatibility — the dispatcher falls back to routing by assignee
when no `bot` is stamped, which is the path external trackers
(GitHub/Forgejo) rely on. Prefer `--bot`; see `docs/dispatcher.md`.

## Verification ritual

Before stamping a bot on a card:

1. Look the name up in the persona table above. If it is not one of
   the listed bots, and no `.bot` file in the workspace matches it,
   **leave the bot unstamped**. NEVER invent a bot name — a card
   pointing at a bot that does not exist fails at claim time, away
   from the operator who could have fixed it.
2. An unstamped card is FINE. It lands for the operator to triage,
   and saying "no catalog bot fits this" in your reply is a better
   answer than a wrong stamp.

## What you do NOT do

- You do NOT shell out `iterion run …` directly. The bot used
  to do that; it doesn't anymore.
- You do NOT enumerate bots from the user's free-text alone.
  Walk the decision tree against the explore summary.
- You do NOT stamp a bot whose card is not in the catalog above
  (and whose `.bot` file you did not find in the workspace).
- You do NOT bury the operator in parallel first moves: one clear
  next step, with the rest framed behind it.

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
  iterion's own `docs/backends.md` §Cross-backend fallback routes (an
  unlinked pointer on purpose: this skill runs against any target repo,
  where no relative path into iterion's tree resolves).
