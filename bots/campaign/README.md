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
a product against an inventory that does not exist yet — which is why
`oracle_dir` is handed to the docs child. That last half is the **end
state**: the docs child reads no net today, and the var it needs is being
added on its own branch (see *Where this is waiting on a sibling* below).

| child | source | skipped when | landed when |
|---|---|---|---|
| `assessment` | `../assessment/main.bot` | `plan_path` exists **and parses** | `plan_path` committed, HEAD moved |
| `golden_master` | `../golden-master/main.bot` | `<oracle_dir>/verify-oracle.sh` exists | `verify-oracle.sh`, `corpus.json`, `feature-coverage.json` committed, HEAD moved |
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
  that contract**, and refuses on a dirty tree. A contract pointing the
  oracle somewhere other than `oracle_dir` fails there, loudly, rather
  than being silently overridden.

`product_docs` never gets the HEAD-moved test: it runs on every campaign,
and an incremental pass over documentation nothing changed is entitled to
commit nothing. Its artefact — pages committed under `docs_dir` — is still
required. Its catalog is generated **out of tree**, in the run's scratch,
naming this checkout as its one local source: a catalog written into the
workspace would be the uncommitted change preflight then refuses.

`phase_zero: false` cuts the whole phase: the campaign enters at
`preflight`, exactly as it did before. **That is the default today** — see
below.

### Where this is waiting on a sibling

Phase 0 is built, falsified and off by default. Three things have to land
before the default flips to `true`:

| what | why it blocks | measured |
|---|---|---|
| the engine's subbot/worktree condition | `pkg/runtime/engine_run.go` runs a `worktree: auto` child in the parent's tree only when the parent holds a **sandbox**. The docs child declares `worktree: auto`, so a plain local `iterion run` gives it a worktree of its own, it finalises its pages onto a branch, and `docs_landed` refuses — correctly, and every time. | the condition reads `e.sharedSandbox != nil && e.sharedSandbox.Run != nil` while its own comment says "a subbot child in its parent's sandbox" |
| the docs child's local-source key | the generated catalog names its one source with `repos[].path`. Until the child's resolver reads it, the entry is recorded `degraded` and the campaign documents a product from nothing. | `grep -c 'path' …/product-docs` resolver: `url`, `github_repo`, `gitlab_path` only |
| the docs child's `oracle_dir` var | the coverage half of the ordering rationale. An undeclared key is dropped in silence. | `grep -cE 'oracle_dir\|feature-coverage\|corpus\.json' bots/product-docs/main.bot` → `0` |

Until then, a campaign that turns phase 0 on gets a **named refusal**
rather than a silent half-run — which is the point of judging children in
git instead of believing them.

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
| `oracle_dir` | `.golden-master` | Where phase 0 **builds** the net. Once the contract exists, `preflight` reads the net's location from the **contract**. |
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

Phase 0's artefacts — `plan_path`, the net under `oracle_dir`, the pages
under `docs_dir` — are written and committed by the **children**, not by
this bot. It only checks they are there, in git.

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
- **`yq` unavailable while a contract file exists** → refuse. Whether the
  contract *reads* cannot be decided without it, and a guess would either
  skip the child that repairs an unparseable plan or overwrite a good one.
- **a child that committed nothing**, or whose artefact is not in the
  commit, or which left it uncommitted → refuse, naming the child. The
  supervisor reads what a run landed from git; a page or a contract that
  is not committed does not exist. (If a child finalises its series onto a
  *branch* instead of this checkout, that is what this refusal catches.)
