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

## Re-anchoring a mutant is the NET's act, not this bot's — and the signal now exists

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

Only the second is mechanically repairable, and repairing it is still **not this
bot's to do**: a mutant belongs to the net, and the party that changes the code
does not rewrite what judges it. Re-anchoring keeps the archetype, the surface
and the targets identical and only moves the hook — but *keeping the nature
identical* has to be demonstrated, not asserted, and that demonstration belongs
to the authority that owns the net.

What this bot owes is the signal, named and structured, so the work is visible
instead of being discovered three lots later.

## `depends_on`

Lot ids that must be `done` first. A lot whose dependencies are unmet is
skipped with a reason rather than attempted — the compatibility matrix is a hard
edge in the graph, and running a lot out of order produces failures that look
like defects in code that is perfectly fine.
