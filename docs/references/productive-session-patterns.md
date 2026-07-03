# Productive-Session Patterns

What a *productive* human-driven agent session actually looks like — measured,
then distilled into authoring rules for bots. This is the "what works"
companion to [workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md)
("what fails"), and the empirical backing behind ADR-055 (unit-convergent
improve loop) and ADR-057 (axis-driven work-list sweep).

**Provenance.** Mined from the operator's local Claude Code transcript archive
(2026-04 → 2026-07): ~2 900 top-level sessions, of which ~1 200 are manual
(human-piloted, across ~15 repositories in several languages/stacks) and
~1 500 are bot-generated (iterion nodes delegating to claude_code — worktree,
dispatcher, and project-store runs). Manual campaign sessions were also read
qualitatively (14 sessions with ≥24 commits each), as were failing and
succeeding bot transcripts. All identifying material stays out of this doc by
design; only shapes and numbers cross over.

## 1. The measured signature of a productive session

Interactive manual sessions (≥3 human messages, ≥0.5 h active): **~3.3
commits per active hour, sustained**. Key shape numbers:

| metric | manual sessions | bot sessions (pre-ADR-055/057) |
|---|---|---|
| commits / active hour | **3.0–3.8** | 0.08 (worktree loops) — 1.1 (dispatcher) |
| read : edit ratio | **0.8–1.0** | 3.5 |
| todo-list events (share of all actions) | **4.6 %** | 0.2 % |
| commits with a todo-list refresh in their window | **56.7 %** | 13.3 % |
| commits with a test run in their window | 40.7 % | 35.6 % |
| median edits per commit | **3** | 0 |
| time to first commit | median **46 min** (p25 29, p75 84) | n/a (most never commit) |
| inter-commit gap once cruising | median **11.9 min** (p25 5.9, p75 20.7) | n/a |

Robustness facts worth designing around:

- **Flat endurance.** Manual throughput stays ≈3.4 commits/h from 30-minute
  sessions to 48-hour campaigns. The pattern scales; context compactions do
  not dent it (compacted = simply the long sessions, same commits/h).
- **The pattern dominates the model.** Productivity varies little across model
  families in the corpus; the working shape, not the model tier, is the lever.
- **Sub-agents pay.** Sessions spawning ≥3 sub-agents run ~26 % more
  commits/h than sessions spawning none (exploration offloaded, main context
  preserved).
- **Steering is front-loaded.** Human corrections per hour *decrease* with
  session length (~1.0/h in short sessions → ~0.35/h in very long ones), with
  a small end-of-session burst for validation/merge. Once framed, a campaign
  runs itself.

## 2. Campaign invariants (from qualitative reads)

Fourteen high-throughput campaign sessions (≥24 commits each) share eight
structural invariants:

1. **Parallel-explore opening, then a written plan, then approval — never a
   direct fix.** Typically several read-only explore/plan agents in parallel,
   findings verified against the real code ("load-bearing claims" re-checked),
   a plan written to a file, 0–2 scoping questions, explicit go. The first
   commit lands ~45 min in; ~18 edits typically precede it.
2. **Phase 0 is foundation, not feature.** The first commit is structural: a
   documentation schema, a pure-additive interface that de-risks the later
   refactor, a workspace scaffold.
3. **One repeated work unit:** locate the exact insertion points → implement →
   build → test → **semantic commit** (`type(scope): …`). Small atomic units
   (median 3 edits per commit); validation precedes the commit, not the
   reverse.
4. **The unit of work mirrors the brief.** Feature work → one phase;
   validation campaign → one subject; review campaign → one finding/MR;
   migration → one repo/wave. The operator sets the grain implicitly; the
   session sticks to it.
5. **The todo list is the metronome.** It is born from exploration (not
   before it), stays dense and *moving* (re-prioritized, items deferred), and
   is refreshed at unit boundaries — 57 % of commits have a todo refresh in
   their immediate window.
6. **Terse steering, standing autonomy.** Most human turns are one-token
   go/status signals; long messages appear only for the initial brief, a new
   sub-mission, or a factual correction. Recurrent batch grant: "do all the
   items, one by one, until everything passes".
7. **Ritualized closure.** Convergence = proven green (tests/CI/e2e) + merge
   or handoff + **a persistence artifact**: memory update, dated bilan,
   pitfalls-doc entry, or a gitignored handoff file priming the next session.
   Productive sessions never end dry.
8. **Real execution is the value engine.** The deep bugs only surface when the
   thing actually runs (a live run, a deployment, a CI pipeline) — never from
   reading. Every campaign variant embeds a real-run feedback loop, and the
   tightest-loop sessions (CI grinding with pasted logs) are productive with
   almost no plan at all: **feedback-loop latency substitutes for planning**.

Campaign variants: *epic* (phased plan, heavy todo), *review* (N parallel
reviewers + explicit false-positive verification, then fan-out into isolated
fixes/MRs), *migration* (distrust-and-verify knowledge base first, then
repo-by-repo waves with "trap" notes committed as docs), *validation* (dated
bilans as the primary deliverable), *CI-grind* (no plan, human pastes failure
logs, minutes-long iteration).

## 3. Where bot sessions lose (gap analysis)

Reading failing bot transcripts against the manual baseline shows the deficit
is **framing, not capability** — the failed sessions contain correct, tested
code that simply never lands. The recurring gaps:

- **G1 — No materialization stage.** Code-writing nodes without a commit step
  leave finished work stranded in the worktree. (Fixed for the improve loops
  by ADR-055/057's per-item verified commit; any new code-writing bot must
  include an equivalent stage.)
- **G2 — No termination contract.** Nodes without a required structured
  output leave the engine unable to detect completion: the same instruction
  gets re-injected, the agent re-does or re-verifies work, and the session is
  eventually killed mid-flight with nothing banked. Every node must end on a
  machine-checkable output.
- **G3 — Redundant re-reading.** Failing sessions re-read the same large
  files 15+ times (read redundancy ×2) — a symptom of over-broad scope and
  lost anchors after failed edits. Narrow the per-item scope instead of
  prompting "read less".
- **G4 — Fighting the environment.** Large time sinks on unprovisioned
  sandboxes (read-only caches, missing toolchains, purged temp dirs). This is
  an engine/infra fix, never a prompt fix.
- **G5 — Self-exoneration spirals.** Without a baseline of pre-existing test
  failures, a diligent agent burns its budget proving failures aren't its own.
  Give nodes the baseline as input and explicit permission to skip what they
  didn't break.
- **G6 — Frozen upfront plan vs living list.** Failing bots create 5–6 coarse
  phases upfront and tick them; the productive shape is a dense, re-prioritized
  work list. In the corpus the correlation is stark: dense todo ↔ frequent
  commits.
- **G7 — Contradictory injected instructions.** Missions assembled from
  successive injections ("produce the plan, do not modify files" … "now
  implement it") confuse the turn loop; a node's mission must be one coherent
  contract.
- **G8 — Commit cadence absent from the contract.** The one bot session in
  the corpus that matched manual throughput (7 real commits/h-equivalent) was
  the one whose brief imposed operator-style waves: test → commit → todo
  refresh, repeated. Cadence must be in the contract (agent-side) or enforced
  deterministically (engine-side).

## 4. Authoring rules derived (the "do" checklist)

For any bot whose nodes write code, alongside the anti-Goodhart checklist in
[workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md):

1. **Encode the work-list sweep, not open review** for whole-codebase change:
   determined axis → enumerate concrete sites → transform/verify/commit each →
   re-enumerate as the done-oracle (ADR-057).
2. **One verified commit per work item** — match the measured cadence (small
   atomic units, ~3 edits/commit, minutes not hours apart). An interrupted run
   must have banked every completed item.
3. **Termination contract on every node**: required structured output; the
   engine detects completion rather than re-poking.
4. **Give the fixer the baseline** (pre-existing failures, known flakes) plus
   explicit skip permission — and bound the "is it mine?" investigation.
5. **Budget a real opening phase.** Manual sessions spend ~45 min
   understanding before the first commit; an enumerate/plan node needs an
   equivalent whole-repo, adaptive exploration (and it should emit the
   work-list from it).
6. **Prefer a living work list** over frozen upfront phases: items get
   appended, re-ordered, deferred as the sweep learns.
7. **Close with a persistence artifact**: report, bilan, board update, or
   handoff file. A run whose learnings evaporate with the worktree did not
   finish.
8. **Shorten the real-feedback loop before enriching prompts.** A tight
   run-observe-fix loop outperforms a richer plan (the CI-grind result); in
   bot terms: deterministic verify gates close to the change, real runs over
   static analysis, fast re-entry after failure.
9. **Offload exploration to sub-agents** where the backend allows it — keep
   the main context on the transform, not the search.
10. **Fix the environment in the engine, not the prompt** (G4): provision
    toolchains/caches before delegating; an agent fighting its sandbox is an
    infra bug report, not a prompt-tuning opportunity.

## 5. Reproducing the measurements

The mining pipeline (transcript census → per-session metrics → event
timelines → qualitative reads) is deliberately simple: parse the local
`~/.claude/projects/**/*.jsonl` transcripts, count tool uses by kind, detect
`git commit`/test invocations in Bash inputs, compute active time by clamping
inter-event gaps, then read the outlier sessions. Anyone can re-run the idea
on their own archive; the scripts are session-scratch, not part of this repo
(and transcript archives must never be committed — they contain private
project material).
