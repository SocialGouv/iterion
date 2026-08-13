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

## What a campaign iteration does

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

## What it refuses at preflight

No plan → refuse (a campaign supervises a *written* programme). No
behavioural net → refuse (lots would be "done" against nothing). Dirty
tree → refuse (a supervisor acts between runs on committed trees, and
must not adopt work in flight). A supervisor that finishes green having
supervised nothing is the blind judge this family of bots exists to
refuse.
