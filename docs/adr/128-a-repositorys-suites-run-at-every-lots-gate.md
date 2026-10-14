# ADR-128 — A repository's suites run at every lot's gate

- **Status**: Accepted
- **Date**: 2026-10-14
- **Applies to**: `bots/modernize` (Morphy) — `lot_verify`, `lot_gate`,
  `plan_read`, the `campaign_user` prompt; `bots/assessment` (Assessy) —
  `contract_lint`, the contract drafting prompt; `bots/campaign` (Campy) —
  `finalize`; the `plan-contract` skill modernize and assessment share
- **Neighbours**: [ADR-125](125-a-lots-contract-is-a-directory-judged-on-what-lands.md)
  (the contract is read at the base and is read-only inside a lot)

## Context

A lot is done when its `exit_gate` exits 0, the behavioural oracle replays
green, and the references are untouched. The gate is written for the lot, and
the contract asks for it to be **narrow**: a gate that runs the whole test
suite for a build-tool upgrade reports failures the lot did not cause, and
the agent tries to fix them.

A narrow gate lets a lot break what it does not watch. The oracle does not
watch it either: it observes served responses, not the repository's tests.
Nothing in the conjunction ran the repository's own unit and integration
suites. They ran in the target's CI, after the landing, where no gate reads
them.

Measured on a modernization campaign:

| | |
|---|---|
| the lot | moved a route that deletes from GET to POST, as an authorization fix; its gate ran the authorization matrix and its falsifier — no test suite |
| its verdict | converged: gate, oracle and references green; the gate wrote `done` and the lot landed |
| the trunk's CI, after the landing | the integration suite red on a contract test still sending the GET (405 where the contract wants 404), and every later stage skipped — the security scan the next lot was about to be judged on included |

The defect was one line of a test. Nothing in the programme could see it
before the landing.

## Decision

**The repository's own test suites are declared once in the contract, and run
at every lot's gate.**

1. **A top-level key, `suites:`.** The commands the repository's CI runs for
   its unit and integration tests, in `exit_gate`'s shapes: a list of
   single-line commands, a bare string being one. Read **at the run's base**,
   like `gate_timeout_s`: a worker who changes them has rewritten the contract
   (ADR-125), and the verdict reads `contract_rewritten`. Any other shape — a
   mapping, a declaration with no command, a NUL byte, the key declared twice
   (two readers would keep different copies) — is a `CONTRACT_UNREADABLE`
   refusal: by `plan_read` before a lot is picked, and by `lot_verify` before
   any command runs. Absent, the gate is the gate as it was.
2. **`lot_verify` runs them once the lot's own `exit_gate` has passed**,
   every command in order, under the same wall (`gate_timeout_s`). A red gate
   goes back to the worker with its own failure: a suite's log would push it
   out of the log's tail, and a suite that hangs on a broken build would turn
   a repairable red into the wall's terminal verdict. A suite past the wall,
   on a green gate, is the same `GATE_TIMEOUT` verdict.
3. **They judge what lands.** `done` lands HEAD — a cloud run pushes it as it
   is — so the suites never run over a working tree that differs from it: a
   change, staged or not, an untracked file git does not ignore, a change or a
   deletion behind an index flag (`assume-unchanged`, `skip-worktree`, which
   every git view trusts instead of the disk), a submodule with work of its
   own — outside the contract's directory and the net, which have checks of
   their own, with no exception for a sparse checkout: the exemption would
   key on a value the constrained party writes. The tree is measured as the
   engine's bank would stage it, with the stat-cache keys a repository's
   configuration can set (`core.checkStat`, `core.trustctime`,
   `core.ignoreStat`, `core.fsmonitor`, `core.untrackedCache`) overridden for
   the node and every command it runs, **before any gate command**: the gate
   and the oracle would otherwise spend their hours on a lot the suites
   cannot judge. The refusal skips the gate, the suites and the oracle, never
   the references, the extension certificate and what lands, which are judged
   as on any pass. The lot goes back with the paths named and the ways out:
   commit what is its work, git-ignore what a build or a test run writes,
   commit or ignore a submodule's work inside the submodule. A tree git
   refuses to stage — a nested repository with no commit, a file it cannot
   read — goes back to the worker too, at the record check as at the gate,
   not to the unreadable-contract terminal. A gate command that rewrites a
   tracked path or writes a new one — a formatter, an install that rewrites
   a lockfile, generated code — leaves the suites unrun, for the same
   reason; the tree is measured once more on the way out, so what the oracle
   or a late-writing command leaves behind is not what a green suites
   verdict covers. Every command runs in a process group of its own, killed
   whole past its wall, its output written to a file rather than held in
   memory — and the bank's scratch index is built outside the workspace,
   where a TMPDIR resolving into it cannot make the node refuse its own
   scratch.
4. **`suites_passed` joins `lot_gate`'s conjunction**, in `converged` and in
   `stop` alike. A lot whose suites fail goes back to the worker with the
   failure in its `fail_log`, worded for what the gate knows: red on this
   tree, by this lot's change or already at the base. A lot declared
   `blocked` stops as it always did — a suite that runs into the wall on it is
   not a `GATE_TIMEOUT` of its own — and its stop says whether its suites
   were red on its tree or did not run to the end, so the lot after it is not
   told it broke them.
5. **The campaign supervisor runs them once more on the final tree** and
   reports them in its handoff; a blocked lot requalifies only when its own
   gate and the suites are green there. It judges the committed tree the way
   `lot_verify` does — the plan read at HEAD, the working tree measured with
   the index flags, every untracked file whatever the status settings, the
   submodules' work and replace refs turned off — and runs its commands in
   `lot_verify`'s order: the blocked lots' gates, then, on a tree measured
   again, the suites, then the oracle. Every command gets the wall `lot_verify`
   gave it: a blocked lot's gate its lot's `gate_timeout_s` (a command two lots
   share runs once, under the longer wall, and each lot reads the run against
   its own), the oracle and the suites the longest wall of any lot, a
   rebaseline act the lot's. A command past its wall is a red the handoff
   names; a plan the supervisor cannot read, or reads in a shape no reader
   takes — ids that are not non-empty unique strings included, an id being
   the key every verdict reads through — is said, never counted as an empty
   programme; the tree is measured again after the suites and HEAD re-read
   before the oracle, since finalize has no next pass to refuse what moved
   under it; the counter-test's own writes are restored, green or red; and
   the supervisor's commits take its own files only, never what a child run
   left in the index.
6. **The contract lint refuses an unreadable `suites`** at drafting time,
   and the drafting prompt asks for the suites the repository's CI runs. The
   lint does not run them: its gate probe proves a gate is red on the starting
   tree; a suite must be green there.

## Alternatives considered

- **A line in each lot's `exit_gate`.** It contradicts the gate's narrowness
  and is copied from lot to lot, so a lot written without it — or a lot that
  "narrows" it with an exclusion — reopens the hole. A guard copied N times is
  a guard the N+1th copy forgets.
- **A program-level outcome on the trunk's CI status.** It reports the
  breakage after the landing instead of refusing the lot that caused it; the
  next lot then starts from a trunk it did not break and inherits the red.
- **Let the oracle run the suites.** The oracle observes behaviour against a
  booted world and is owned by the net; the suites are the repository's, need
  no world, and are a property of the code, not of the references.
- **Exempt what the suites wrote on the previous pass**, recorded under
  `.git` so that a report a suite leaves in the tree does not refuse the next
  pass. The worker writes under `.git` too, and could exempt its own
  uncommitted work: the record would be a door around the check it serves.
- **Refuse any path a clean filter applies to.** Git LFS stores every large
  file through one: the rule would refuse every lot of every repository that
  uses it.

## Consequences

- A lot cannot converge over a test it broke and committed, whatever its gate
  runs. What the guarantee does not reach: a file git ignores that a suite
  reads — it stays on the working tree, does not land, and the bank cannot
  see it — and, inherited from ADR-125, a refused rewrite of the contract
  that a relaunch from the banked head then reads as its base.
- **A suite red at the base refuses every lot**, whatever the lot did: the
  contract's owner declares only suites green on the trunk the programme
  starts from, and a trunk that turns red is the first thing to fix.
- The gate runs longer by the suites' duration, on every pass of the repair
  loop. Fast suites are part of the contract's obligations, as fast gates are.
- A flaky suite now refuses lots at random. That is the flake becoming
  visible, not a new defect, and it is fixed in the suite.
- A suite that writes a report into the tree, outside what git ignores,
  refuses the next pass until the report is ignored, in a commit — by the
  worker, whom the refusal tells how. The gate cannot tell a test's output
  from the lot's work, and a file the suites leave there never lands.
- A worker who installs a clean filter through `.git` — an attribute in
  `.git/info/attributes`, the filter in the repository's configuration — can
  make git store HEAD's bytes for a path it edited, and no check that reads
  git's view sees the edit: the suites' check and the references' alike. The
  defence is a judge on a fresh clone of what lands, which is beyond this
  decision.
- Contracts without `suites:` behave exactly as before.
