# campaign (Campy) 🧭

Supervises a **whole modernisation programme**: runs the [modernize
bot](../modernize/) as a subbot in a bounded loop, judges progress **in
git**, executes the golden-master ledger's re-baseline requests between
runs under a mechanical acceptance criterion, escalates to a human in two
configurable modes, and closes on a committed handoff.

It exists because `modernize` carries **one lot per run** by design — the
programme is a suite of runs, and someone has to be the suite. That someone
was a human first: a full programme was replayed end to end under manual
supervision, and the interventions were counted. Nearly all were mechanical
— a written procedure with a verifiable criterion — and the criterion held
on **every** re-record act: the observed reference diff equalled the
announced one, every time. This bot mechanises exactly what was mechanical,
and routes the rest to a human.

## The supervisor is deterministic

There is **no LLM node** in this graph. Intelligence lives in the child
bots; judgement lives in deterministic gates; the supervisor measures,
acts on written announcements, and keeps a journal. A supervisor with
opinions would be a third author in a system whose safety comes from
having exactly two, separated:

- **the worker** (`modernize`, as a subbot) changes the code and never
  writes the oracle's references — its own gate enforces that in git;
- **the steward** (a tool node here) re-records references **only** on a
  request written to the ledger, **only** when the observed diff equals
  the announced set exactly, with the full mutation counter-test replayed
  on the committed tree behind every act. A red counter-test unwinds the
  act and escalates.

The window discipline this separation demands — never act while a worker
is writing — is structural: a subbot node returns only when the child run
is over.

## Phase 0 — the campaign produces its own prerequisites

`preflight` refuses a campaign with no contract and no net. It still does,
and it is **unchanged**. What used to satisfy it was two manual gestures
before anyone could launch — somebody wrote the plan, somebody built the
net. Phase 0 is those gestures, run as child bots:

```
phase_zero ─▶ assessment ─▶ golden_master ─▶ product_docs ─▶ preflight ─▶ run_lot ⟲ …
     │            │               │               │              ▲
     │       plan_landed     net_landed      docs_landed         │
     └──────────────────── phase_zero: false ────────────────────┘
```

The order is not a preference: neither child has anything to work from
until the contract is written, and documentation cannot be shown to cover
a product against an inventory that does not exist yet — which is why the
net's location is handed to the docs child, whose exhaustiveness gate reads
the net's feature inventory and corpus there (see *What phase 0 needs from
its children* below).

**Where the net lives is the contract's to say.** `net_gate` reads
`oracle.dir` and `oracle.verify` from the contract exactly as `preflight`
does — `.golden-master` and its `verify-oracle.sh` when the contract names
none — and hands that one derivation to the golden-master child, to
`net_landed` and to the docs child. It is deliberately not a var: preflight
is where the campaign looks for the net once phase 0 is over, and a second
source for the same path would let phase 0 build a net nobody reads. Two
contracts are refused before any child starts: one that puts the net
outside the workspace, and one naming an entry point (`oracle.verify`) that
is absent — the golden-master child writes its entry point at
`<dir>/verify-oracle.sh` and nowhere else, so launching it could not
satisfy preflight.

| child | source | skipped when | landed when |
|---|---|---|---|
| `assessment` | `../assessment/main.bot` | `plan_path` exists **and parses** | `plan_path` committed, HEAD moved |
| `golden_master` | `../golden-master/main.bot` | the contract's entry point, `corpus.json` **and** `feature-coverage.json` are all where the contract puts the net | the same three committed, HEAD moved |
| `product_docs` | `../product-docs/main.bot` | never — always launched, `full` or `incremental` | at least one `*.md` committed under `docs_dir` |

Three properties hold the phase to the rest of the bot's doctrine:

- **every decision is a tool node reading a FILE.** There is still not one
  LLM node in this graph. A skip names the artefact that caused it; a
  launch names what was missing. Both go in the node's notice.
- **a child is judged in git, never on its word.** After each one, a
  deterministic node re-reads the tree: the artefact must be *committed*
  and free of uncommitted changes, and — for the two children that run
  only when their artefact is absent — HEAD must have moved. A run that
  did not move HEAD landed nothing, which is the rule this bot already
  applies to its lots. Anything else REFUSES, naming the child.
- **`preflight` still has the last word.** Phase 0 produces; preflight
  judges. It re-reads the contract, re-derives the net's location **from
  that contract** — the derivation `net_gate` copies term for term, and a
  test runs both nodes on the same trees to keep the copy honest — and
  refuses on a dirty tree.

`product_docs` never gets the HEAD-moved test: it runs on every campaign,
and an incremental pass over documentation nothing changed is entitled to
commit nothing. Its artefact — pages committed under `docs_dir` — is still
required, and `docs_gate`'s page count from **before** the run travels into
`docs_landed` so its notice says which of the two it is looking at: pages
landed where the product had none, pages added to what was there, or the
same set it started from. On an already-documented product that last case
is the honest limit of what this gate proves — it cannot distinguish a pass
that changed nothing from a child that committed elsewhere, and it says so
rather than implying the stronger claim.

`docs_landed` also carries phase 0's **last word on the tree as a whole**.
The three landed gates scope their dirt check to their own artefact — that
is what lets their refusals name a child — so a child writing outside its
own paths would pass all three and have preflight refuse the campaign once
the whole bootstrap was paid. One unscoped read at the end, with the same
two exclusions, is the chokepoint for that.

The docs child's catalog is generated **out of tree**, in the run's scratch,
naming this checkout as its one local source: a catalog written into the
workspace would be the uncommitted change preflight then refuses.

`phase_zero: false` cuts the whole phase: the campaign enters at
`preflight`, exactly as it did before. **That is the default** — see below.

### What phase 0 needs from its children

Phase 0 is off by default. Two things outside this bundle decide when it
can default on:

| what | why | what holds it |
|---|---|---|
| the engine's subbot/worktree condition | `pkg/runtime/engine_run.go` runs a `worktree: auto` child in the parent's tree only when the parent holds a **sandbox**. The docs child declares `worktree: auto`, so a plain local `iterion run` gives it a worktree of its own, it finalises its pages onto a branch, and `docs_landed` refuses — on a product with no pages yet. On one that arrived documented, that gate sees the pages that were already there and can only SAY so. A sandboxed run does not hit this. | the default: the condition reads `e.sharedSandbox != nil && e.sharedSandbox.Run != nil` while its own comment says "a subbot child in its parent's sandbox" |
| the docs child's interface: its local-source catalog form (`repos[].path`) and its `oracle_dir` var | a docs child without the first records the generated catalog's one source `degraded` and documents the product **from nothing**; without the second it drops the net's location without a word, and its exhaustiveness gate never arms. Phase 0 cannot see either at run time — `docs_landed` accepts committed pages, whatever they were written from. | `TestCampaignPhaseZeroChildrenReadWhatTheyAreHanded` feeds the catalog phase 0 generates to the docs child's own `catalog_ingest`. While the docs child declares no `oracle_dir`, it asserts phase 0 defaults **off**; once it does, it asserts the source is read locally and the net found where the contract put it. |

So `--var phase_zero=true` is safe where the catalogue's docs child reads
that interface; on a local, non-sandboxed run, the first row still makes a
product with no pages yet a **named refusal** rather than a silent
half-run, and one that already has pages a notice that says the gate could
not tell. Judging children in git instead of believing them is the point;
saying exactly what that proves is the other half of it.

## What a campaign iteration does

0. **phase 0** *(once, before the loop)* — the three prerequisite children,
   each skipped on its artefact and verified in git. See above.
1. **run_lot** — one `modernize` child run (one lot, or a clean no-op).
2. **observe** — everything re-read from git and files, never from the
   child's self-report: did HEAD move, how many consecutive still runs,
   is any lot still eligible, did the plan gain/lose/reshape lots
   (**contract extensions**), which ledger requests are pending.
3. **steward** — extensions handled under the configured governance;
   each pending request refused (wrong contract, malformed, dirty tree)
   or executed under the observed-equals-announced criterion; journal row
   and escalation log appended **and committed**.
4. **escalate** *(when configuration asks)* — a human node. The answer
   edits nothing: an operator who wants to change the programme changes
   the **repository** and commits, exactly as a human supervisor did. The
   pause is the window; git carries the intervention.
5. **loop_gate** — continue while something can still land: not
   exhausted, fewer than `stagnation_stop` consecutive still runs, bounded
   by `max_lots` and the workflow budget (whose declined back-edge also
   exits through the handoff).
6. **finalize** — final counter-test on the committed tree;
   **requalification** of blocked lots (each distinct gate command played
   once on the final tree, verdicts projected per lot — a lot blocked
   mid-programme often blocked on a cause the finished tree has closed);
   handoff written and committed, **extensions first**.
7. **handoff_review** — a human closes every campaign, whatever the mode.

## Configuration (launch-time `--var`)

| var | default | meaning |
|---|---|---|
| `governance` | `bot` | Who approves **contract extensions**. `bot`: accepted in flight, listed at the head of the handoff for a human to re-take. `human`: every extension pauses the campaign, whatever the escalation mode — an unapproved extension shapes every lot after it. |
| `escalation` | `handoff` | Other escalation items (refused/mismatched/red-gate requests, dirty tree). `handoff`: accumulate and present once at the end — a blocked lot no longer blocks its dependants, so the campaign keeps landing what it can. `interactive`: pause there and then. |
| `max_lots` | `40` | Upper bound on child runs (the loop's fuel). |
| `stagnation_stop` | `2` | Consecutive child runs without a new commit that end the campaign. |
| `lot_max_passes` | `4` | Forwarded to the child: repair passes per lot. |
| `workspace_dir`, `plan_path` | `${PROJECT_DIR}`, `.modernize/plan.yaml` | Where the programme lives. |
| `phase_zero` | `false` | **The phase-0 switch.** `false` enters at `preflight`, which refuses whichever prerequisite is missing — the campaign exactly as it ran before. Off by default until the three siblings below land; `--var phase_zero=true` turns it on. |
| `brief_path` | `.modernize/brief.yaml` | The written brief the assessment child turns into the contract. Missing it, when the contract has to be written, is a **refusal**: a plan is never guessed. |
| `docs_dir` | `docs/client` | Where the docs child writes the product documentation, in the target repo. |
| `docs_product_id` | `product` | The id the generated catalog gives the product and its single repo. Any stable slug does — the catalog is this bot's own. |
| `scratch_dir` | `${PROJECT_SCRATCH_DIR}/campaign` | Out-of-tree home of that generated catalog. |

```sh
cd <target-repo>
iterion run <path>/bots/campaign --sandbox none \
  --var governance=bot --var escalation=handoff
```

Headless runs park durably on the two human nodes
(`paused_waiting_human`); everything is already committed when they do —
answer with `iterion resume --run-id <id> --answer action=continue` (or
from the board card).

## The ledger protocol the steward consumes

Requests and acts live in the oracle's `REBASELINE.md` as machine-readable
blocks (the canonical format is documented in the golden-master bot's
[doctrine skill](../golden-master/skills/golden-master.md)). A pending
request is a `iterion:rebaseline-request` block whose `id` has no matching
`iterion:rebaseline-act` block and no committed refusal. The steward:

- refuses a request whose lot declares `rebaseline_allowed: false` — that
  flag is the assertion being tested, and a request against it means the
  lot overflowed;
- refuses a malformed request loudly (an unreadable request is an
  escalation, not a silence) and never retries a refusal (the refusal is
  committed state);
- executes the rest with `verify-oracle.sh --record`, accepts **iff**
  `observed changed paths == expected_paths` exactly (any collateral file
  fails the whole act), commits the act, replays the **full**
  counter-test on the committed tree, and unwinds the act if it goes red.

## What it writes in the target repo

- `.modernize/campaign/journal.tsv` — one committed row per child run:
  the auditable trace a third party recounts (`before`, `moved`, `acts`,
  `after`).
- `.modernize/campaign/escalations.jsonl` — every item and every operator
  answer, committed before anyone is asked anything.
- `.modernize/campaign/handoff.md` + `requalification.json` — the closing
  deliverable, extensions first.
- Ledger act/verdict blocks and re-recorded references — steward commits,
  clearly labelled `gm(rebaseline): …`.

Phase 0's artefacts — `plan_path`, the net where the contract puts it, the
pages under `docs_dir` — are written and committed by the **children**, not
by this bot. It only checks they are there, in git.

## What it refuses at preflight

No plan → refuse (a campaign supervises a *written* programme). No
behavioural net → refuse (lots would be "done" against nothing). Dirty
tree → refuse (a supervisor acts between runs on committed trees, and
must not adopt work in flight). A supervisor that finishes green having
supervised nothing is the blind judge this family of bots exists to
refuse.

With phase 0 on, those three are normally *produced* rather than
demanded — but preflight is the node that still decides, on the tree
phase 0 committed.

## What phase 0 refuses, before anything is launched

- **a tree carrying work in flight** → refuse, before a single child
  starts. Every phase-0 decision reads the **checkout** on purpose, so an
  operator's uncommitted draft contract skips the child instead of being
  overwritten by it — which is only sound while the checkout *is* the
  commit. `preflight` makes the same refusal, but hours of child runs
  later. Same two exclusions as preflight: iterion's `.claude/` scaffold,
  and the engine's own materialised node script.
- **no contract and no brief** → refuse. The assessment child derives the
  contract from a brief; with neither, there is nothing to derive it
  from, and inventing lots is the one thing no bot here may do. Write the
  brief, write the contract, or launch with `phase_zero: false`.
- **`yq` unavailable** → refuse, contract or not. Whether a contract
  *reads* cannot be decided without it — a guess would either skip the
  child that repairs an unparseable plan or overwrite a good one — and
  every later reader needs it: `net_gate` for where the net lives,
  `preflight` after the whole phase. Without it the children would run only
  to be refused.
- **a net outside the workspace, or a contract-chosen entry point that is
  absent** → refuse, at `net_gate`, before the golden-master child starts.
  The children resolve the net inside the workspace, and that child writes
  its entry point at `<dir>/verify-oracle.sh` only.
- **a child that committed nothing**, or whose artefact is not in the
  commit, or which left it uncommitted → refuse, naming the child. The
  supervisor reads what a run landed from git; a page or a contract that
  is not committed does not exist. (If a child finalises its series onto a
  *branch* instead of this checkout, that is what this refusal catches.)
