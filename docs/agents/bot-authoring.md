# Authoring bots — the four rules a catalog bot must clear

A bot shipped in `bots/` is a general-purpose tool. It must be **repo-agnostic**
(no iterion paths baked in), **stack-agnostic** (adding a language is dropping a
skill file), convergent (an asymptote, not an oscillation), and declare the
tools it needs. The mirror rule — **the engine must never know a specific
bot** — is here too, because the two are one boundary seen from both sides.

Read [../workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md)
before writing or amending any `.bot` that can commit code.

## Skills (Claude Code SKILL.md) live with their bundle

Claude Code-style skills ship inside the `.botz` bundle they
support, not at a repo-global location. Iterion's runtime mirrors
`<bundle>/skills/*.md` into `<workspace>/.claude/skills/` at run
start (and on each resume), regardless of backend. Three backends
consume the same directory: `claude_code` through its native lookup,
`claw` through the `skill` tool (registered by
[pkg/backend/tool/claw_builtins.go:RegisterClawSkill](../../pkg/backend/tool/claw_builtins.go)),
and pi through the explicit `--skill <workspace>/.claude/skills`
argument. Each bundle therefore gets exactly the skills it ships,
with no implicit dependency on the host
filesystem. The collision policy (workspace wins, with marker-aware
refresh for upgrade cases) is documented in
[docs/bundles.md](../bundles.md#resource-resolution-at-run-time).

Current bundles and their skills:
- [bots/whats-next/skills/](../../bots/whats-next/skills/) —
  10 skills: `whats-next` (operating playbook), `iterion-bot-catalog`,
  `iterion-dsl-quickref`, `iterion-board` (reference for the
  capability-gated board MCP tools on claude_code, claw, and pi RPC),
  `iterion-label-vocabulary`, `repo-survey`, `roadmap-synthesis`,
  `priority-elicitation`, `session-continuity` (iterion workspace
  memory — `memory_read` / `memory_write` / `memory_list` for the
  cross-run knowledge tree under
  `~/.iterion/projects/<key>/memory/<scope>/`), and `dogfood-cycle`
  (the operator's measured ritual for validating a bot by a real
  run — launch visible, monitor actively, fix both bot and engine,
  land + bilan; from the session-mining work behind
  [docs/references/productive-session-patterns.md](../references/productive-session-patterns.md)).
  Six of the original eight were produced by a dogfood run of claw +
  `openai/gpt-5.5` against this repo; `iterion-board` was added by
  the board-capabilities work and `session-continuity` by the
  workspace-memory work — see
  [scripts/adhoc/whats-next-skills-gen.bot](../../scripts/adhoc/whats-next-skills-gen.bot)
  for the generator (the seed for a future formalised
  `generate-skills.bot`).
- [bots/copilot/skills/](../../bots/copilot/skills/) — 5 skills, **one per
  posture** plus the playbook and a second design skill:
  `iterion-concepts` (info), `iterion-dsl-authoring` +
  `iterion-bot-architecture` (design), `iterion-run-debug` (debug),
  `copi-conversation` (the operating playbook, loaded every turn).
  The two design skills are deliberately separate because they fail
  differently: `iterion-dsl-authoring` is how to SPELL a `.bot` (the
  syntax traps that compile clean and break at runtime),
  `iterion-bot-architecture` is how to DESIGN one (responsibilities,
  closed contracts, the PASS/RETRY/BLOCKED shape, why a retry re-enters
  its producer rather than a correction twin, subbot boundaries,
  idempotency, budgets, proof categories). Folding them together makes
  a reader treat architecture rules as compiler rules — the one thing
  the design posture must never tell an operator. A repo may override
  the second with its own standard (`authoring_standard:` declaration
  or `ITERION_AUTHORING_STANDARD_PATH`), which the skill tells Copi to
  read and prefer.

**A `skills:` entry that resolves to nothing is silent.** It is not a
compile error and not a bundle-lint finding — the runtime mirrors
nothing, the agent's Skill tool finds nothing, and the bot answers from
the model's priors instead of the authored knowledge (it reads as "the
bot got dumber", not as a typo). `bots/catalog_skill_refs_test.go`
closes that gap: every name in a catalog bot's `skills:` list must exist
as `<bundle>/skills/<name>.md` with matching frontmatter. The exception
is a skill a bot deliberately does NOT ship so the operator attaches it
from the skill library (ADR-059) — `deploy-target` for app-dev /
review-env, where shipping one would pin a catalog bot to a platform.
Those live in a commented allowlist in that test.

**Maintain skills inline with the code they describe.** Each time
you touch a skill's subject area and notice the skill is wrong,
incomplete, or out of date, fix it in the same change — the cost
of a small inline correction is much lower than the cost of an
agent later following stale guidance. Concrete examples:
- Changed a bot's purpose/persona/triggers, or renamed/moved it →
  edit that bot's `manifest.yaml` (`display_name` / `description` /
  `when_to_use` / `triggers` / `enabled`), NOT the catalog skill: the
  generated region of `iterion-bot-catalog.md` is rebuilt from
  manifests. Only the hand-authored `iterion-bot-catalog-static.md`
  preamble (decision tree / distinguishers) is edited by hand; run
  `iterion bots regen-catalog` to refresh the committed generated file.
- Added a new DSL primitive or changed edge syntax → update
  `iterion-dsl-quickref`.
- Discovered a better survey heuristic → fold it into `repo-survey`.

When adding a new skill, place it under the bundle's `skills/`
directory with the standard frontmatter (`name`, `description`)
plus an imperative-voice body grounded in real files. Skills must
be self-contained: a reader who lands on one should not have to
chase context across the repo.

If a skill ends up duplicated across multiple bundles, accept the
duplication for now (iterion has no skill-sharing primitive yet)
and add a TODO comment in each copy pointing to its peers.

## Category & tags — the six-verb navigation spine

Every shipped catalog bot declares `category:` and `tags:` in its
manifest. They are **advisory display metadata** (like `when_to_use`):
the runtime never branches on them, discovery just carries them, and no
surface may hide a bot for what they say.

**`category` is a CLOSED, versioned set** — one of six slugs
([pkg/bundle/vocab.go](../../pkg/bundle/vocab.go), mirrored by
`studio/src/lib/botTaxonomy.ts` under a Go parity test):

| slug | reads as | fleet examples |
|---|---|---|
| `build` | ship new capability | feature-dev, app-dev |
| `verify` | judge the code, touch nothing | review-pr, sec-audit-* |
| `harden` | strengthen what exists | branch-improve-loop, secured-renovacy |
| `document` | align words with code | docs-refresh, adr-cartograph |
| `operate` | run the delivery machinery | feed-watch, review-env |
| `steer` | judge the direction, converse | whats-next, arbitrate |

The order is the lifecycle (create → judge → strengthen → explain → run →
decide) and is part of the contract — every grouped surface renders it in
this order. Adding a seventh slug is a **product decision** documented
here and in the vocabulary file, never an opportunistic manifest edit.
An unknown value is never rejected or rewritten: the bot lands in the
visible **Uncategorized** group, last, and bundlelint emits C270 (a
warning) naming the known slugs. Absence is different: a SHIPPED bot
without a category is a catalog regression —
`bots/catalog_taxonomy_test.go` fails the build on it.

**`tags` are an OPEN, governed vocabulary** — orthogonal facets
(domain, safety, modality: `security`, `deps`, `read-only`,
`ships-code`, `conversational`, …). The seed lives next to the
categories; `iterion bots list --tag a --tag b` ANDs them, the studio
chips narrow the gallery, and `?tag=…` on `/bots` is a shareable view.
Governance is the label-vocabulary lesson: **reuse before inventing**.
An unseeded tag stays declared (never a load error) and emits C271
(warning) — but the shipped-catalog consistency gate
(`bots/bundle_consistency_test.go`) holds the fleet to zero warnings, so
a new tag on a catalog bot means seeding `vocab.go` in the same change.
Third-party bundles feel only the soft lint.

The safety pair carries a WRITTEN boundary — the tag answers "what will
this run leave behind?". For a bot whose run targets a repository,
EXACTLY ONE of the pair applies (carrying both is a contradiction the
fleet gate refuses): `ships-code` = the run COMMITS into the target
repo's history (source, docs, wiki, config, versioned state — anything
`git log` shows: golden-master is ships-code even though its product is
a test net, because it commits; the two supply-shields are read-only
even though they read dependencies deeply, because a finding goes to
the board and no commit exists); `read-only` = the run writes nothing
anywhere. A conversational or board-only bot (Nexie, Copi, Triagy)
targets no repository and legitimately carries neither.

Two things to know:

- **Strict decoding makes the two keys load-breaking for older builds**
  (house policy since `launch:`/`chat:`/`retry:`): a runner older than
  the feature refuses the whole bundle at admission rather than dropping
  the fields. Fleet and binaries upgrade together; update a pinned
  runner before pulling manifests that declare the keys.
- **Marketplace fields are a different vocabulary**: when prefilling a
  marketplace submission from these manifest keys one day, map them to
  NEW marketplace entry fields — the existing `Entry.Categories`
  (plugin contribution kinds) and submit-time `Entry.Tags` mean
  something else, and overwriting them corrupts plugin grouping.

## Authoring `.bot` workflows that touch real code

**Before writing or amending any `.bot` workflow that has the power to
commit code, read [docs/workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md).**
It captures hard-won lessons about Goodhart's law in workflow design,
the façade pattern that LLM agents reach for when goals are
under-specified, and concrete rules for prompts, scanners, and judges
that resist metric-gaming. Skipping it has a real cost — the
goai → claw-code-go migration ran for 3 hours and produced a
96%-parity-reported façade because these lessons weren't yet codified.
Its "what works" companion is
[docs/references/productive-session-patterns.md](../references/productive-session-patterns.md) —
the measured shape of productive operator sessions (commit cadence,
work-list discipline, termination contracts) distilled into authoring
rules; ADR-055/ADR-057 encode its core finding. External cross-check:
[docs/references/external-methodologies.md](../references/external-methodologies.md)
maps two independent 2026 methodology papers (IACDM, AI-DLC) onto
iterion — what they validate, the imported rules (teach-back, cost-tier
switch, scope inventory, …, folded into the pitfalls doc), and what was
deliberately rejected.

### Improvement loops must converge to an asymptote

Every improvement/review loop must **converge to an asymptote** — settle
into a stable approved state and stop — not oscillate. A slight, very
occasional oscillation is acceptable; it must be the rare exception.
**The rule is the asymptote.** (`iterion bench asymptote` measures
exactly this — see [docs/asymptote-bench.md](../asymptote-bench.md).)

**The default mechanism (ADR-058 v2, the whole shipped fleet).** The
flagship loop bots (whole-improve-loop, branch-improve-loop,
feature-dev, feature-gap-fill, test-coverage, e2e-coverage, docs-refresh,
adr-cartograph, secured-renovacy Phase 2) converge through ONE
`campaign` agent + a deterministic gate + a bounded continuation loop:
- the **deterministic verify gate** (`verify_build` writes the repo's
  real build+test into an out-of-tree `verify.sh`; the `verify_run`
  tool re-runs it on the REAL exit code — never an LLM judgment,
  ADR-044) is the truth oracle (docs-refresh is the exception: a
  docs-only campaign can't break the build, so it dropped the verify
  gate and converges on `scope_ok ∧ docs_aligned` alone);
- the **termination contract** (a machine-checkable flag —
  `axis_complete` / `feature_complete` / `docs_aligned` / … — plus
  `commits_this_pass` and a remaining-work note) is the done-oracle,
  with the honesty clause "under-reporting only costs a pass,
  over-reporting lands you right back here";
- **`gate.converged = <flag> ∧ gates green`** closes the single
  declared `continuation_loop(max_passes)`; exhaustion ships what is
  banked (the campaign commits each unit in stride — git is the state);
- oscillation is structurally absent: one context, fresh each pass,
  re-reads `git log` — there is no reviewer/fixer relay left to
  re-litigate.

**If you author a NEW cross-family reviewer loop** (an optional
amplification per ADR-058 — no catalog bot ships one any more),
preserve the historical convergence mechanisms: a `streak_check` gating
exit on N consecutive cross-family approvals with low-confidence
rejections non-blocking; `prior_pushback` / `previous_scanned_areas`
fed back with "do NOT re-raise without new evidence";
`loop.<name>.previous_output` for monotonic verdicts; bounded
`max_iterations` as the backstop, not the design goal.

**Mono/dual review topology (ADR-052) — MONO IS THE DEFAULT.**
[pkg/reviewtopology](../../pkg/reviewtopology/resolve.go) resolves
`review_mode` (`auto|mono|dual`) + `mono_family` at LAUNCH and injects
them on every surface (CLI `iterion run --review-mode`, studio/API,
dispatcher bot_arg) — but ONLY into bots that declare a `review_mode`
var (`InjectIfDeclared`). **`auto` resolves to `mono`**, even when both
families are available: dual costs a full reviewer pass per family on
EVERY run, and with the merge gate wired every push re-reviews, so
cross-family confirmation is a deliberate spend (`--var review_mode=dual`)
rather than something a host opts into by having two providers configured.
The catalog bots that still run family reviewers — `review-pr` (Revi) and
`evolve` — declare the vars and gate their fan-out behind a `condition`
router (never `round_robin`, and never `when` guards on a `fan_out_all`
router's own edges: both collect every edge without evaluating the
condition). Any new reviewer-loop bot adopts the topology the same way.
The machinery stays guarded non-vacuously by
`e2e/review_topology_test.go` + `e2e/testdata/review_topology_mini.bot`.

**Right-artifact discipline** (now encoded in the campaign contracts,
still binding for anything that diffs code): judge the WORKING TREE
(`git diff HEAD`, or `git diff <base>` for branch/run scopes), never
`HEAD^...HEAD`; and make untracked files visible before diffing (`git
add -N -- ':/' ':(exclude,top).claude'`, or `git add -A -- ':/' ':(exclude,top).claude'` before each in-stride commit — a change that
ADDS files is otherwise invisible to the diff). Both failure modes were
observed live in the v1 reviewer loops (a reviewer concluding "the
feature isn't implemented" and looping forever — see
[docs/bot-runs/feature-dev.md](../bot-runs/feature-dev.md)); the v2
contracts bake the `git add -A -- ':/' ':(exclude,top).claude'`-then-commit unit in (iterion's
skills mirror is never the run's work; a file under `.claude/` that IS the
deliverable is staged by name, `git add -- .claude/<path>`), and any new
reviewer you author must anchor the same way.

## Catalog bots are repo-agnostic

Every bot shipped in `bots/` (the catalog `iterion bots list`
discovers — docs-refresh, feature_dev, whole_improve_loop,
branch_improve_loop, secured-renovacy, whats-next, sec-audit-*, …) is
a **general-purpose tool that must run on ANY target repository**, in
any language, with no knowledge of iterion's own layout baked in.
docs-refresh aligns *a* repo's docs with *its* code; feature_dev ships
*a* feature in *whatever* repo it's pointed at. iterion is just one
possible target, never the assumed one.

**The rule:** a catalog bot's `vars:` defaults, prompts, and scanners
must not hardcode iterion-specific *target-repo* facts. Concretely,
the following are violations when they appear as **defaults**:

- Code/doc globs pinned to iterion's tree — `cmd/iterion/*.go`,
  `pkg/dsl/ir/*.go`, `pkg/**/*.go`, `examples/*/skills/*.md`. Default
  to language/layout-agnostic globs (or empty = "scan the workspace");
  a specific layout is a per-run `--var` override.
- Output/cache paths under iterion's store — `.iterion/...`,
  `~/.iterion/...` written **into the target repo**. Use a neutral
  repo-root dotfile (e.g. `.docs-refresh-cache.json`) the operator can
  gitignore; never scatter `.iterion/` into someone else's tree.
- Scanners that only produce meaningful output on iterion's shape
  (e.g. gre`p`ing for cobra `Use:` literals, `Cxxx` diagnostic codes,
  or the literal `iterion <subcmd>`). Gate these **off by default**
  (empty scope glob) and document them as an opt-in specialization;
  generalising their patterns to other stacks is the bar for making
  them a default.
- Prose framing the bot AS an iterion tool ("docs-refresh's primary
  target is iterion's own documentation"). The bot's target is
  whatever repo it's run against; iterion is at most the *reference
  self-host case*.

**Not violations** (these are the *runtime*, not the target repo):
references to iterion the engine running the bot — `mcp__iterion_board__*`
capability tools, "iterion's expr / template substitution", `iterion
report` for surfacing output, `.bot` DSL syntax. The bot is
*written for* iterion; it must not be *scoped to* iterion.

**Enforcement:** `bots/catalog_universality_test.go` greps every
catalog bot's var-default block for the violation patterns above and
fails CI on a regression. When a default legitimately needs an
iterion path (rare), add it to the test's allowlist with a comment
explaining why it's universal-safe. When you touch a catalog bot,
re-read this section — the iterion repo is the easiest target to
accidentally overfit to, because it's the one you're staring at.

## Universal code bots — stack knowledge lives in skills

Catalog bots are not only repo-agnostic (layout) — they are
**stack-agnostic** (language/ecosystem). A bot is universal when adding
a new language or package manager requires **zero DSL edits**: the
stack-specific knowledge lives in the bot's **skills**, the (now
adaptive) agent reads the relevant skill and adapts to whatever repo it
is pointed at — exactly how native Claude Code works — and
**deterministic gates verify the right work happened**. This is the
companion dimension to "Catalog bots are repo-agnostic" above; a catalog
bot must clear both bars.

**The rule:** a catalog bot's DSL (`vars:`, `prompt:`, `schema:`,
`tool ... command:`) must not enumerate languages or package managers.
Violations:
- Per-ecosystem shell branches in a tool node — `case "$PKG_MGR" in
  yarn) …; npm) …; go) …`. The skill is the catalogue; the agent
  dispatches.
- Per-language tool nodes wired in fixed position — `tool
  run_go_scanners:` / `run_js_heuristics:` plus a closed router fan-out.
  One adaptive agent step, guided by the skills, replaces them.
- Closed enum booleans in a schema — `has_js: bool`, `has_go: bool`,
  `has_npm: bool`. Emit an open `langs: []` / `ecosystems: []` list.
- Hardcoded language extension globs (`*.go`, `*.py`, `*.rs`) in `vars:`
  defaults or `command:` bodies.

**The canonical pattern (skill-guided + deterministic gate):**
1. A `skills/<topic>.md` (or `skills/lang-<id>.md`) holds the
   stack-specific knowledge — how to detect the stack, which
   scanners/commands to run, how to read the results.
2. An adaptive agent node (claude_code or claw, agentic base restored —
   see "System-prompt composition" in
   [backends-and-execution.md](backends-and-execution.md)) reads the matching skill and
   runs the right commands for the repo in front of it.
3. A **deterministic gate** (a `tool`/`compute` node, no LLM) verifies
   coverage: the always-on floor must have produced output, and every
   detected stack must have produced its expected artifact, else the run
   degrades/fails with a visible banner. The gate is the determinism —
   not an LLM judgment, and not a closed DSL enum. (sec-audit-source's
   `scan_health` is the reference: hard-fail when the generic floor is
   missing, banner partial per-language coverage.)

This keeps the asymptote/quality guarantees intact while removing every
language/ecosystem assumption from the workflow graph. Adding Rust to a
security bot = drop `skills/lang-rust.md`; no `main.bot` or schema edit.

**Not violations** (universal infrastructure, not stack-specific tooling):
- The always-on generic floor — `gitleaks` / `trivy` / `semgrep
  --config=p/default` in sec-audit-source's `run_generic_scanners`
  (`p/default` is Semgrep's universal cross-language pack — the metrics-off
  floor, since `--config=auto --metrics=off` is rejected by semgrep; only
  per-**language** packs like `p/golang` / `p/python` are violations, which
  is exactly what `catalog_universality_test.go` matches).
- `npm install -g @anthropic-ai/claude-code` in a sandbox `post_create`
  (bootstrapping the runtime, not the target's stack).
- Prose in a `prompt:` block that *mentions* `go test` / `npm install` as
  an illustrative example — the agent picks its commands from the repo +
  skill; the example is just guidance.

**Enforcement:** `bots/catalog_universality_test.go` greps every catalog
bot's `command:` bodies and `schema:` blocks (not only `vars:` defaults)
for the stack-specific patterns above. When you touch a catalog bot,
re-read this section and "Catalog bots are repo-agnostic" — iterion (Go)
is the easiest stack to overfit to, because it's the one you're staring at.

## The ENGINE stays bot-agnostic — no bot knowledge in `pkg/`/`cmd/`

The mirror of "catalog bots are repo/stack-agnostic": **iterion the engine
must never know about a SPECIFIC catalog bot.** A bot is a catalog artifact
(`bots/<name>/`); the engine wires *any* bot through GENERIC seams and must
carry no `"docs-refresh"` / `"branch-improve-loop"` / `"review-pr"` string,
no `stampDocsRefreshAmendVars`-style helper, no bot-specific prompt, no
`if botID == "<x>"` branch. That coupling is exactly backwards — it makes
the engine un-shippable to anyone whose bots differ, and it means a new bot
needs an engine PR instead of just a bundle.

**When a bot needs special launch/runtime behaviour, the behaviour lives in
the BOT, keyed on generic context the engine already provides:**
- Generic launch vars every bot can read — `pr_url`, `base_ref`,
  `source_branch`, `pr_author`, `scope_notes`, … — set uniformly for ANY bot
  launched on a PR/issue (`reviewPRVars` / `buildPRForgeCommandVars`). Doki's
  amend-on-PR (v3.5.2) is the reference: iterion checks out the PR head +
  sets `pr_url`/`base_ref` for whatever bot the webhook launches; Doki *itself*
  reads a non-empty `pr_url` and switches into amend — zero engine code knows
  it's Doki.
- Manifest `invocations:` (the capability "what can fire me"), `capabilities:`
  (board tools), `contributes:` (plugins), skills. The `Subscription` binds
  (event) → (a bot) generically.
- Manifest **`produces:` / `consumes:`** — the run-to-run hand-off, matched by
  KIND (`review`, `review_ledger`), never by bot id. A bot declares what it
  leaves behind for a later run (naming nodes in its OWN graph) and what it
  wants stamped into a launch var; the engine knows the shape of each role and
  nothing about who fills it. This is how a reviewer seeds a fixer, and how the
  fixer's per-finding answer reaches the next review, with neither manifest
  naming the other bot. Adding a second reviewer or a second fixer is a bundle,
  not an engine PR. See [pkg/server/webhooks_handoff.go](../../pkg/server/webhooks_handoff.go).

**Known debt (extract when touched, don't extend):** the webhook role bot
ids are no longer read as constants — they resolve through
`Server.roleBots()` over the `bot_roles` platform-settings family
([pkg/platformcfg](../../pkg/platformcfg/platformcfg.go), `iterion remote admin
roles set --reviewer …`), the constants remaining only as the DEFAULTS
(enforced by the symbol-sweep test in
[bot_resolver_sweep_test.go](../../pkg/server/bot_resolver_sweep_test.go)). What
remains hardcoded: the Billy merge-queue auto-heal mission prompt
([pkg/server/webhooks_github.go](../../pkg/server/webhooks_github.go)), the
`botRosterOrder` display list ([pkg/server/server_dsl.go](../../pkg/server/server_dsl.go)),
and the dispatcher's `ImplementBotOrDefault → "feature-dev"`
([pkg/dispatcher/config.go](../../pkg/dispatcher/config.go), local-YAML
configurable already). Full role-from-manifest extraction stays future
work. **Do not add to this list** — thread new behaviour through the
generic seams above. If you find a fresh instance, flag it.

## A bot that needs tools declares them in `devbox.json`

**If a bot's steps need a binary the sandbox image does not ship, add a
`devbox.json` next to its `main.bot`.** iterion auto-installs it and puts
the resulting tools on `PATH` for every node of the run. The same applies
to a `devbox.json` at the root of the TARGET repo: iterion loads that one
too, so a bot inherits the toolchain the repo itself declares.

**On every driver, the pod backend included.** A bundle reaches a
container as a host bind mount and the kubernetes driver has none, so
there the config is not read from in-container — it is CARRIED there,
written out by the install prologue before `devbox install` runs. Worth
knowing because it was a decline until 2026-09-10, and that shape is the
one to watch for in its whole class: the feature worked on a laptop and
was inert on the driver bots actually run on, with nothing failing except
the step that needed the tool. Ceiling: the config+lock pair must stay
under 512 KiB, and a pair over it is declined by name, never installed
from a directory it never reached.

This is the supported way, and the alternatives are all worse:

- **Curling a binary in `post_create`** — unpinned, undeclared, and
  invisible to anyone reading the bot.
- **A bespoke sandbox image** (the `-sec` variant) — a CI image chain to
  maintain for every new tool, and a bot pinned to an image instead of to
  the tools it actually needs.
- **Letting the agent improvise** — the failure this rule exists for. In run
  019f8384 the deploy step needed `crane` to publish an image, the sandbox
  had no container tooling at all (no docker/podman/buildah/skopeo, `sudo`
  blocked by `no_new_privs`, no `newuidmap` for rootless BuildKit), and the
  agent spent turns discovering that, fetched a binary itself, then fell back
  to a workaround that produced a live URL and delivered nothing.

**Pin the versions and commit `devbox.lock`.** `some-tool@latest` re-resolves
at install time, so what lands in a run's sandbox can change with no commit
anywhere — a supply-chain surface, and a reproducibility hole for a bot whose
job is to ship code. The lock pins each package to an exact nixpkgs commit;
the explicit version in `devbox.json` makes the intent readable in a diff.
Generate it with `devbox install` in the bot's directory and commit both
files — the engine copies the lock alongside the config, so a locked project
installs exactly what it was authored against.

**A run that does not BUILD the target repo can decline its toolchain.**
`repo_devbox: off` on the `workflow` block skips the *target repo's*
`devbox.json` (never the bot's own) — precedence `--repo-devbox` → workflow
→ `ITERION_REPO_DEVBOX` → **on**, diagnostic C134, and the declined source is
reported on the `sandbox_devbox_provisioned` event rather than dropped in
silence. Reviewers ship with it off (`review-pr`, `revi-converse`): reading a
diff bought nothing from iterion's own 319 Nix paths / 1.8 GiB, and the cold
realise outlasted the sandbox start window often enough to kill runs. Fixers
and updaters (`branch-improve-loop`, `dep-update-guard`, `feature-dev`) keep
it **on** — they build what they change. See
[docs/dsl.md](../dsl.md#the-target-repos-toolchain--repo_devbox).

Two things to know when writing one:

- **Non-interactive PATH is the trap.** `tool` nodes run through a
  non-interactive `bash -c` that never sources a shell profile, so a tool that
  is installed but not on `PATH` is a tool that does not exist. The engine
  prepends the devbox profile's bin dir for this reason — don't hand-roll it
  per bot.
- **Nix installs cost time.** Declare what the bot genuinely needs. A bot
  with no `devbox.json` pays nothing.

The bar for reaching past devbox (a dedicated image) is a tool that Nix does
not package, or a base layer the run needs *before* any step executes.

