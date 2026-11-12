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

1. **lot_room** — a lot starts only where one can finish: while the
   elapsed time plus `lot_room_s` stays under 90% of the campaign's wall.
   The child budgets itself, runs in this run's sandbox and is never
   interrupted, so a lot started later would end where the engine starts no
   tool node — observe refused, no handoff — and on Kubernetes the
   shared sandbox pod dies at the wall plus 30 min. Otherwise the campaign
   hands off (finalize), and the handoff opens on why: no lot
   started (or none after the N this campaign ran), the elapsed time,
   `lot_room_s` and the wall it was judged on, and the two levers — a
   smaller `lot_room_s`, a longer `--max-duration`. The first lot starts
   through it too, so a wall of 16h40m or less (a short validation run, a
   deployment ceiling under the 48h) starts none at the default
   `lot_room_s`.
2. **run_lot** — one `modernize` child run (one lot, or a clean no-op).
3. **observe** — everything re-read from git and files, never from the
   child's self-report: did HEAD move, how many consecutive still runs,
   is any lot still eligible, did the plan gain/lose/reshape lots
   (**contract extensions**), which ledger requests are pending.
4. **steward** — extensions handled under the configured governance;
   each pending request refused (wrong contract, malformed, dirty tree, a
   wall it cannot read) or executed under the observed-equals-announced
   criterion, the record and the counter-test under the lot's own
   `gate_timeout_s`, the counter-test's writes restored after it; journal
   row and escalation log appended **and committed**. An act that would not
   end, at those walls, 30 min before 90% of the campaign's wall — past
   which no tool node starts, the handoff's included — is deferred: logged,
   left pending in the ledger for the next campaign, never refused.
5. **escalate** *(when configuration asks)* — a human node. The answer
   edits nothing: an operator who wants to change the programme changes
   the **repository** and commits, exactly as a human supervisor did. The
   pause is the window; git carries the intervention.
6. **loop_gate** — continue while something can still land: not
   exhausted, fewer than `stagnation_stop` consecutive still runs, bounded
   by `max_lots` and the workflow budget (whose declined back-edge also
   exits through the handoff).
7. **finalize** — on the committed tree only (the plan read at HEAD; a
   working tree that differs from HEAD, index flags included, is reported
   as not judged): **requalification** of blocked lots (each distinct gate
   command played once on the final tree, each lot reading the run against
   its own wall, to the millisecond — a lot blocked mid-programme often
   blocked on a cause the finished tree has closed; it requalifies only when
   its gate and the suites are green on a tree measured again after the
   suites, with HEAD the handoff names), then the repository's `suites` when
   the contract declares them, then the final counter-test — the suites and
   the
   oracle under the longest wall any lot's gate gave them. A command past
   its wall is a red the handoff names; a plan finalize cannot read is
   said, never counted as an empty programme. Every command runs under its
   own wall or what the campaign's wall leaves before the handoff must be
   written, whichever is shorter: one the campaign's wall stops is named
   as cut, never read as a verdict, and the handoff is written either way.
   Handoff written and committed, its own files only, why the campaign
   ended under its title, **extensions first**.
8. **handoff_review** — a human closes every campaign, whatever the mode.

## Configuration (launch-time `--var`)

| var | default | meaning |
|---|---|---|
| `governance` | `bot` | Who approves **contract extensions**. `bot`: accepted in flight, listed at the head of the handoff for a human to re-take. `human`: every extension pauses the campaign, whatever the escalation mode — an unapproved extension shapes every lot after it. |
| `escalation` | `handoff` | Other escalation items (refused/mismatched/red-gate requests, dirty tree). `handoff`: accumulate and present once at the end — a blocked lot no longer blocks its dependants, so the campaign keeps landing what it can. `interactive`: pause there and then. |
| `max_lots` | `40` | Upper bound on child runs (the loop's fuel). |
| `stagnation_stop` | `2` | Consecutive child runs without a new commit that end the campaign. |
| `lot_max_passes` | `4` | Forwarded to the child: repair passes per lot. |
| `lot_room_s` | `54000` | The campaign time one lot may take: lot_room starts a lot only while the elapsed time plus this stays under 90% of the wall. 15h = modernize's 12h wall, its 10% exit grace and one golden-master child (1.1h), then 0.7h for the campaign to reach its handoff: observe (four commands under 60 s walls) and the steward's 30-min reserve. Lower it for a contract whose lots are known to be short — and for any wall of 16h40m or less, which otherwise starts no lot. |
| `workspace_dir`, `plan_path` | `${PROJECT_DIR}`, `.modernize/plan.yaml` | Where the programme lives. |

```sh
cd <target-repo>
iterion run <path>/bots/campaign --sandbox none \
  --var governance=bot --var escalation=handoff
```

A short wall — a validation run, a deployment whose ceiling is under
campaign's 48h — sizes `lot_room_s` to its lot, or starts none: a 30-minute
run whose one lot takes about 10 minutes, for instance,

```sh
iterion run <path>/bots/campaign --sandbox none --max-duration 30m \
  --var max_lots=1 --var lot_room_s=900
```

`lot_room_s` is the operator's estimate of one lot, and nothing interrupts a
child once started: a lot that outlives it runs on under its own 12h wall,
and a campaign it carries past 90% of its wall ends with no handoff. Whenever
lot_room refuses a lot, the handoff names the figures it judged.

A contract whose `exit_gate` judges a deployed application through
`$DEPLOY_CREDENTIAL` holds under a direct `modernize` launch, not under
campaign: campaign does not declare `deploy_credential`, so its lots' gates
find no credential and refuse, and finalize's requalification of those lots
runs without it too. Declaring it would hand every campaign run the team's
platform credential — the operator's decision, not taken here.

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
