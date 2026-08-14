---
name: plan-contract
description: The .modernize/plan.yaml contract — its fields, why the programme is a committed file rather than something re-derived from git history, and why an agent-written status is never believed.
---

# The programme contract

`.modernize/plan.yaml` lives in the **target** repository and is the single
source of stack knowledge for this bot. The bot names no build tool and no
runtime; it runs the commands this file declares. That is what lets one bot
serve any repository, and what lets a human audit the programme without reading
the bot.

## Shape

```yaml
version: 1

oracle:
  dir: .golden-master            # the behavioural net
  refs_dir: .golden-master/refs  # references — off-limits to this bot
  verify: .golden-master/verify-oracle.sh

lots:
  - id: L1
    title: "one line, in the imperative"
    status: todo                 # todo | blocked | done
    rebaseline_allowed: false
    crosses_major: false         # true -> the upgrade-archetypes sweep is due
    depends_on: []
    intent: |
      What may change, and what may not. Read by the agent working the lot.
    exit_gate:
      - "the command that decides this lot"
```

## Why a file, and not the git log

Git history is an excellent record of what happened and a poor contract for
what should. Three things this file holds cannot be re-derived from a tree:

- **The lot DAG is a decision.** That the toolchain rises before the runtime is
  a judgement about a compatibility matrix. Nothing in the repository states it,
  and a bot that guesses will guess wrong in the direction that costs a day.
- **Which references are authoritative is state.** "This is what the application
  is supposed to do" is a claim someone made, not a fact recoverable from the
  code.
- **A programme has to be visible while it runs.** A stakeholder asking "where
  are we" deserves an answer that is not an archaeology exercise.

A trace is not a contract.

## `status` is never believed

The bot reads `status` to know which lots to skip, and **ignores it entirely
when deciding whether a lot succeeded**. A lot is done if and only if its
`exit_gate` exits 0 on HEAD, the oracle replays green, and the reference
directory is untouched.

This is not distrust of any particular agent. It is that a self-reported status
and a verified one are different kinds of claim, and a programme that conflates
them will, sooner or later, report a milestone that never happened. The field
is a bookmark, not evidence.

## `exit_gate`

A list of shell commands, run in order, in the repository root. **Every one must
exit 0.** They are the definition of the lot, so write them to be:

- **Deterministic** — the same command on the same tree gives the same verdict.
  A gate that depends on the network or on wall-clock time will eventually fail
  for a reason unrelated to the lot, and teach everyone to re-run it until it
  passes, which destroys its meaning.
- **Fast enough to run every pass** — the repair loop replays it. A twenty-minute
  gate makes a four-pass lot an eighty-minute wait before any feedback.
- **Narrow** — a gate that runs the entire test suite for a build-tool upgrade
  reports failures the lot did not cause, and the agent will try to fix them.

A lot with no `exit_gate` is refused rather than assumed to pass. An
unverifiable lot is indistinguishable from one that was never done.

### Narrowing has a cost, and it is paid later

The third bullet is the dangerous one, so it comes with an obligation. Every
exclusion in a gate — a skipped task, a filtered subset, a disabled check —
silences a verification lane for the whole run of the programme, not just for
the lot that wrote it. And the exclusion propagates: lots are copied from one
another, so the narrowing written for the first toolchain bump is still there
four lots later, under changes it was never reasoned about.

This is not hypothetical. A programme that excluded the test task from every
gate — written down, never hidden — carried an upgrade that removed the old
test API from the compile classpath. Three quarters of the test files stopped
compiling. Four lots reported green, and the behavioural oracle could say
nothing either: it watches served responses, not build tasks. The suite was
found dark only when someone asked what a CI job would actually run.

So, whenever a gate excludes something:

1. **Say why in the lot's comment**, next to the command. An exclusion whose
   reason is not written is indistinguishable from an oversight the moment its
   author stops reading the file.
2. **Name what still watches the excluded thing.** If the answer is nothing,
   the exclusion is a blind spot with a schedule, and it needs a lot of its
   own to close.
3. **Re-widen at the end of the programme, not never.** The last lot's gate
   should be the unexcluded one. If it cannot be, that is a finding.

The general shape, and it is the same defect the oracle exists to catch one
level down: *a check that establishes something NEAR what it claims and
reports success on the resemblance.* `build -x test` really does establish
"the build is green" — on what it was told to look at. The line in the report
says "build green" without the qualifier, and everyone downstream reads the
line.

## `rebaseline_allowed`

Set `false` for any lot that must not change observable behaviour — a toolchain
upgrade, a build reorganisation, a dependency bump with no API change. It is not
a precaution, it is **the assertion being tested**: if the oracle moves during
such a lot, the lot overflowed, and finding that out is the point.

This bot never re-baselines regardless. The flag documents intent for the
humans and for the process that owns re-recording.

## `crosses_major`

Set `true` on any lot that crosses a MAJOR of anything with semantics — web
framework, ORM, template engine, database engine, language runtime. It puts the
[[upgrade-archetypes]] sweep in scope: derive the drift probes from that
major's own migration notes, run them, and leave the record in the tree at
`.modernize/sweeps/<lot-id>.md`.

The contract makes the record inspectable rather than optional — such a lot's
`exit_gate` includes:

```yaml
    exit_gate:
      - "test -s .modernize/sweeps/L22.md"
      - "…the commands that decide the lot itself"
```

The gate checks the record EXISTS; it does not read its content. That is
deliberate and it is the same division as everywhere else in this file: the
mechanical layer proves an artefact was produced and committed, the reviewer
judges what it says, and the behavioural net remains the only party that can
prove the sweep missed nothing it watches. A sweep record that says "class not
instantiated in this stack, because X" is a legitimate record; an absent one is
a lot that skipped a due diligence its own contract named.

## Re-anchoring a mutant is the NET's act, and this bot delegates it

A lot may legitimately remove the thing a mutant hooks into. A security major
withdraws the matcher idiom a mutant named; a front-end major replaces the
configuration block a mutant edited. The patch stops applying, the harness marks
the mutant **INVALID**, and an invalid mutant neither scores nor dilutes — it
simply stops proving anything, quietly, on whichever lane it covered.

That is a real cost and it is easy to miss: the gate can stay green while the
counter-test that made one lane worth trusting has silently gone dark.

`lot_verify` therefore surfaces `oracle_invalid` — the ids and reasons, read from
the oracle's own report rather than matched out of a log. Two very different
things wear that label:

- **the mutant never mutated anything** — it proved nothing to begin with;
- **its anchor vanished under a change the lot was entitled to make** — it DID
  prove something and has stopped.

Only the second is mechanically repairable, and repairing it is **not this bot's
to do**: a mutant belongs to the net, and the party that changes the code does
not rewrite what judges it. So the repair is DELEGATED — `reanchor` runs the
net's own bot as a subbot, on this lot's checkout, and the verdict is then
re-taken on the repaired net rather than patched.

Where the line actually sits is worth stating, because it is not where one first
looks for it. It is not between this bot and a human: the child runs inside this
lot's workspace, so anything the child may write, this lot may write through it.
The line is inside the net:

- **re-anchor** a mutant → mechanical, and its result is verifiable. The repaired
  mutant must mutate something again, judged by the harness's own code
  (`GM_MODE=validate`), and it must still prove what it proved: same class, same
  archetype, same surface, and no target dropped that the corpus still carries.
- **re-record** a reference → never. The child's own deterministic check reads
  the diff in git and refuses any path outside the mutants directory, naming it.

The routing has one non-obvious property: the repair is attempted on EVERY
outcome, including a converged lot. That is deliberate — an invalidated mutant
does not fail the gate, its verdict is excluded from the score, so the figure
looks no worse. A lot can go green while the net gets narrower, which is the one
failure a green cannot report.

The signal itself was written before it was readable: `lot_verify` printed
`oracle_invalid` into its report while the `lot_report` schema did not declare
the field, so no expression could reach it — and the validator refuses the
reference outright (C031) rather than reading an empty default. A signal that
exists in a log and not in the contract is not a signal.

## `depends_on`

Lot ids that must be `done` first. A lot whose dependencies are unmet is
skipped with a reason rather than attempted — the compatibility matrix is a hard
edge in the graph, and running a lot out of order produces failures that look
like defects in code that is perfectly fine.
