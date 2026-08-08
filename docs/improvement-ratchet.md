# The ratchet

[Philosophy](philosophy.md) explains how we decide when a design question is
open. This page names something narrower and older than a design rule: **why an
improvement, once landed, does not come back.**

It introduces no engine mechanism: every part of the ratchet is machinery the
engine and the bot fleet already carry. This page gives the parts one name so
they can be reasoned about together, and so a new bot or a new gate can be
checked against it. The only doctrine it adds is the third question below,
landed in two campaign contracts — `branch-improve-loop` taking the two that
precede it at the same time, for parity with `whole-improve-loop` — and
guarded by [`bots/ratchet_clause_test.go`](../bots/ratchet_clause_test.go).

## Sisyphus

The failure mode first, because it is the one worth recognising on sight.

A loop reviews, fixes, reviews again — and the second review re-raises what the
first one settled. The boulder goes up and rolls back down. Nothing is wrong
with any single pass; the work simply does not accumulate.

Iterion has met this concretely, and its sharpest form is a loop that judges the
wrong artifact. A reviewer anchored on the last *commit* (`HEAD^...HEAD`)
reports "the feature isn't implemented" against work that is plainly present. A
reviewer anchored on `git diff HEAD` — the correct anchor — still cannot see the
new files the implementer created without `git add`ing them, so it rejects,
correctly, every single pass, and the fixer cannot resolve a gap that is about
staging rather than about code. That second run was cancelled at pass 10 of 15,
having burned $4.95 on passes that could never pass
([bilan](bot-runs/feature-dev.md)). The rule it produced — judge the working
tree, make untracked files visible first — is in
[Authoring pitfalls](workflow_authoring_pitfalls.md#improvement-loops-must-converge-to-an-asymptote),
and [ADR-058](adr/058-minimal-framing-lean-on-the-agent.md) collapsed the fixer
half into a single `campaign` agent committing in stride behind a deterministic
gate. Cross-family review left the campaign loop *as its mechanism*: ADR-058
leaves it available as an optional amplification, and the two catalog bots that still ship one
(`review-pr`, `evolve`) gate it behind `--var review_mode=dual`, mono being the
default. What some campaign bots keep in-loop instead (`feature-dev`,
`branch-improve-loop`, `app-dev`) is a single readonly review pass over the
run's — or, for `branch-improve-loop`, the branch's — own diff, run by a fresh
sibling agent whose findings ride the gate's `fail_log` into the campaign's
next pass, rather than by a separate fixer that has to be talked into agreeing.

Sisyphus is the reference point for everything below. A mechanism earns its
place by making the boulder impossible to lose, not by pushing it harder.

## Two scales, two words

**The asymptote** is what happens *inside* one run: the passes settle into a
stable approved state and the run **stops**. It has an empirical counterpart one
scale up — [`iterion bench asymptote`](asymptote-bench.md) aggregates N
already-recorded sessions of the same task and shows where the per-iteration
judge verdict plateaus. That plateau is the *(model + recipe)*'s reliability
ceiling rather than any single run's convergence; the command re-runs nothing,
though it needs a node emitting a per-iteration verdict (`--judge-node`, plus
`--judge-field` when it isn't named `approved`) — which on a campaign bot means
its termination flag rather than a judge.

**The ratchet** is what happens *between* runs, once an operator gives them a
cadence — a weekly improvement axis, a pass on each pull request, a Monday
security audit, a docs alignment after a feature. Each run leaves the
repository a little further along, and no later run has to re-earn what an
earlier one banked.

The two are not competitors. The asymptote is the reason a run terminates; the
ratchet is the reason terminating is not the end of the story. A loop with an
asymptote and no ratchet is efficient amnesia. A ratchet with no asymptote is
Sisyphus with better branding.

## The click

A ratchet is only a ratchet because of the pawl — the small part that drops into
place and makes the last turn irreversible. In iterion that part is never an
agent's promise that the work is done. **It is a deterministic check.**

That is the whole load-bearing idea, and it is the reason for a sentence that
keeps recurring across the codebase: *the half of a gate that can bank a
regression is a `tool` or `compute` node reading a real exit code, never an LLM
judgment.* Other terms sit beside it in the same conjunction — a termination
flag, and where a bot keeps an in-loop reviewer, its verdict — and a
conjunction only ever tightens: none of them can green what the exit code reds.

| Image | What it names | Where it already lives |
|---|---|---|
| **The click** | the deterministic check that locks the turn | `verify_build` writes the repo's real build+test into `verify.sh`; `verify_run` re-runs it and gates on the process exit code |
| **The keyed connector** | a defect class made structurally impossible to repeat — the notch that only lets the plug in one way | the DSL [diagnostics](references/diagnostics.md) (`C001`–`C199` and `C240`–`C242`, plus the bundle's `C200`–`C234`), [`bots/catalog_universality_test.go`](../bots/catalog_universality_test.go) (greps every catalog bot's var defaults for iterion paths, and its whole source for stack-specific logic), `bots/verify_probe_wiring_test.go` |
| **The fuse** | a gate that cuts rather than let damage through | a RED verify gate routes the campaign back with the failure log; the [merge gate](merge-gate.md) posts a status that is a **count**, so a finding cannot be talked past |
| **Placing protection** | commit each unit as it is verified, so a fall costs one metre and not the wall | the campaign contracts' repeated unit (locate → smallest change → build → test → `git add -A` → commit); git *is* the run's durable state, and an interrupted run keeps every committed unit in its preserved worktree |
| **The logbook** | what a run learned, surviving the run that learned it | the `docs/bot-runs/` bilans (e.g. [whole-improve-loop](bot-runs/whole-improve-loop.md)) — committed and PR-reviewable, unlike the gitignored run artifacts — plus skills maintained inline with the code they describe |

Read the table as a checklist rather than a glossary. A new improvement bot that
has a click and no protection loses its work on a budget cap; one with
protection and no click banks unverified work, which is worse.

### Where the click does not click

The mechanisms above are shipped, not perfect. Four gaps are worth carrying in
your head, because each is a place the ratchet slips silently:

- **A missing `verify.sh` is counted as a pass.** `verify_run` reports
  `skipped: true, passed: true` when `verify_build` produced no script, and the
  loop gate consumes only `passed` from it — `skipped` is never read.
  branch-improve-loop's deterministic `publish_verdict` tool *does* count a
  skipped build as blocking when it posts the merge-gate status on a PR, but
  only on that lane; whole-improve-loop has no merge-gate verdict at all.
- **The reuse pre-check validates shape, not content.** After the first pass,
  `verify_probe` reuses the existing `verify.sh` on size plus `sh -n`. A script
  containing `echo ok` passes both and becomes the run's deterministic truth —
  caught only in a repo whose CI already has a drift gate for `verify_run` to
  mirror.
- **The merge gate blocks only when six things hold**: the run's
  forge-publish grant (without it the bot posts no status at all), the bot's
  `gate_enabled`, its pinned `gate_context`, the forge's statuses-write
  permission (a 403 posts nothing), the webhook's `review_on_sync` (off on a
  hand-made webhook, but derived ON at provisioning for any repo whose enabled
  bots declare a `statuses:` token scope), and that context listed in the repo's
  required checks. Miss the last one and the gate is a harmless advisory. Miss
  any of the others *while* the context is required and it is worse than red:
  the check is **absent**, indistinguishable from one still running, and the
  pull request deadlocks. The server's gate reconciler repairs one shape of
  that and only one — a run that held the grant AND the pinned context AND
  named a head that is still the PR's, then died without posting. A missing grant, an unpinned
  context, a 403, or a `review_on_sync` that never launched a run leave it
  nothing to speak for — see [merge gate](merge-gate.md).
- **A `failed_resumable` run banks no branch.** Its commits live only in the
  preserved worktree until the run is resumed to completion, or cancelled
  through the server API — the studio's button, or `iterion remote runs cancel`
  — which finalizes what it banked. A run launched by `iterion run` with no
  server behind it has no such path.

## Where the endlessness lives

This is the family of ideas people already know as *kaizen* — continuous
improvement, and its less-quoted second half, *standardise*: capture the gain so
it cannot slide back. Naming the lineage is honest, and it is cheaper than
having someone point out the resemblance later.

**One divergence matters, and it is not cosmetic.** Continuous improvement is
unending by construction. An iterion **run** must terminate — that is the
asymptote rule, and it is enforced by bounded loops, machine-checkable
termination contracts and budget caps. The endlessness belongs in the
**schedule** ([`iterion schedule`](scheduling.md), `cloudsched`), not in the
graph: a loop that improves forever because nobody bounded it is a defect, and
every shipped improvement bot is bounded.

Which is not the same as forbidding it. Pillar 1 of the
[philosophy](philosophy.md) applies here as everywhere — the limit is
load-bearing, so it carries a greppable hatch: `as name(unbounded [<fuel>])`
opts a graph into Turing-completeness in one keyword, still fuel-bounded so
there is no silent infinity
([totality & TC](dsl-totality-and-tc.md), [ADR-050](adr/050-dsl-turing-completeness-fuel-liveness.md)).
An author who reaches for it has decided something; an author who never bounded
the loop has not.

The rest of the resemblance is not worth borrowing vocabulary for. This is a
property of a machine — a graph that terminates, a check that reads an exit
code, a commit that outlives the run that made it — not a way of running a
team.

## Using it

When you author or review an improvement loop, three questions:

1. **What is the click?** Which deterministic check makes this pass's gain
   irreversible — and is it reading a real exit code, or an agent's opinion?
2. **Where is the protection?** If the budget runs out mid-pass, what is left on
   disk: a landed series of verified commits, or nothing?
3. **What did the run teach?** A defect worth fixing twice is a defect worth a
   keyed connector — a test at the site, an assertion, a diagnostic. Land it
   while you are still on that site — same commit, or its own follow-up commit
   rather than an amend of one already banked — and **only if it cannot fail on
   code the pass did not touch**: a repo-wide rule added mid-campaign turns the
   gate red on someone else's work, and the loop spends its remaining passes
   cleaning up after itself.

Question 3 is written into the campaign contracts of
[whole-improve-loop](../bots/whole-improve-loop/main.bot) and
[branch-improve-loop](../bots/branch-improve-loop/main.bot), as the third of the
advisory questions an agent answers before it reports. It carries one exception,
and only into the former: an axis that IS *about* such checks — landing one is
then the work, but only together with the fixes it demands, and never before the
pass that can green the tree with it. A branch-scoped pass has no axis, so it
has no exception. Deliberately advisory:
the deterministic gate still decides, and a question that could block
convergence would trade the asymptote for the ratchet — the one exchange this
page argues against.

---

Related reading: [Philosophy](philosophy.md) ·
[Asymptote bench](asymptote-bench.md) ·
[Authoring pitfalls](workflow_authoring_pitfalls.md) ·
[Productive session patterns](references/productive-session-patterns.md) ·
[ADR-058](adr/058-minimal-framing-lean-on-the-agent.md)
