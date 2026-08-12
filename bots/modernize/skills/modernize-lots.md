---
name: modernize-lots
description: What a modernisation lot is, why the unit is the lot and not the package, the ordering constraints that make or break a programme, and the four ways a lot goes green while being wrong.
---

# Working a modernisation lot

A **lot** is a step whose entry and exit are both deterministic gates. Not a
ticket, not a sprint: a bounded change with a command that says whether it is
done. "Upgrade the build tool" is a lot. "Modernise the app" is a programme.

## Why the lot, and not the package

Dependency-upgrade pipelines work package by package, and their failure path is
*revert this package and continue*. That is correct for a dependency sweep and
catastrophic here. A runtime upgrade is one indivisible change touching
hundreds of files; there is no "continue without it". Nothing is reverted
piecemeal — either the lot lands or it does not.

Which is why a lot needs a **written intent** stating what may change. Without
it, a green gate is compatible with almost any diff, and the next lot inherits
an unreviewable tree.

## The ordering constraint that costs a day when ignored

Toolchains and runtimes have a compatibility matrix, and it is not symmetric.
**Raise the build tool first, on the runtime you already have; then raise the
runtime.** The reverse order fails in a way that reads like a code problem —
obscure errors from the build tool's own internals, deep in a stack trace with
no application frame — and sends you looking in the wrong place entirely.

The dependency order belongs in the contract's `depends_on`. It is a decision,
not something re-derivable from the tree, and a lot marked `blocked` is stating
that decision rather than recording a failure.

## Where a lot may end: only where the system is still observable

A lot must be verifiable **at its own exit**. That is a stronger constraint than
"bounded", and it is the one most easily got wrong, because a boundary can look
perfectly clean on paper and fall exactly where nothing can be checked.

Observed, on the second lot ever run by this bot. The intent stopped at raising
the framework line and left the configuration changes that raise *requires* to a
following lot. Both exit-gate commands exited 0. Not one reference moved. And
the application did not start — so the behavioural net could say nothing at all,
because it could not even reach it.

That is not a lot that failed. It is a lot that **cannot be judged**, followed by
another that would inherit a state nobody can vouch for. Two unverifiable steps
are worse than one verifiable one, however tidy the split looks.

The rule:

> A change and the changes it **mechanically imposes** are one transition. Split
> them and you put a boundary where the system is dark.

The test to apply before writing a lot: *if this lot succeeds exactly as
described, can the net still observe the application?* If the honest answer is
no, the lot is drawn wrong — widen it to the next point where the system is
alive, or move the boundary earlier.

### The same rule, in its second disguise

A boundary can also fall where the system is perfectly observable and the lot is
*still* unjudgeable — because its exit condition is unreachable by design.

Seen one lot later, and worth as much as the first. A lot was scoped to close
only the divergences that needed no application code, and its intent said, in
writing, that the net would remain red at its exit. The gate requires a green
net. That lot could therefore never be done: it predicted its own failure and
was correct.

> **A lot's exit must be reachable by the lot.** An intent that forecasts a red
> gate has not described a lot, it has described a step — and a step is merged
> with whatever makes it verifiable.

Note the trap for the author. Writing "the net will stay red, that is expected"
feels like honesty, and it reads like rigour. It is neither: it silently
converts the gate from a verdict into a formality, because everyone now knows in
advance that the red means nothing. The moment an exit criterion is knowingly
unsatisfiable, either the criterion or the boundary is wrong — and it is almost
always the boundary.

Splitting work that must land together does not make it more reviewable. Two
lots landing in two commits with two written intents is reviewable; two lots
where the first cannot be judged is one unreviewable change wearing a costume.

Note what this does **not** license. Widening a lot to absorb an unrelated
failure is still overflow; the criterion is mechanical necessity, not
convenience. "The upgrade demands this property or it will not boot" belongs
inside. "While we were there we bumped nine libraries" does not.

## Staying inside the lot

The commonest way a lot goes wrong is not failing. It is **succeeding at more
than it was asked**.

A build tool upgrade that also bumps a dozen libraries "since we were there"
produces a diff nobody can review, and when the oracle goes red there is no way
to tell which change did it. The lot's value comes precisely from its
narrowness: one cause, one verdict.

When a lot genuinely cannot be completed inside its intent, **stop and say so**.
A blocked lot with a written reason is a usable artefact — it feeds the next
planning decision. A green lot that quietly did something else is a debt whose
interest is paid by whoever debugs the next one.

## Never touch the oracle

The behavioural net is off-limits: not its references, not its comparators, not
its fixtures. This is enforced mechanically — the gate diffs the reference
directory against the commit the run started from and fails on any change — but
the reason matters more than the check.

**A golden master dies by re-baselining.** If the party that breaks a reference
can also rewrite it, then green means "someone made it green", which is not
information. When a reference moves, exactly one of two things is true:

- the lot changed observable behaviour it was not supposed to change, or
- the lot changed behaviour it *was* supposed to change, and the reference must
  be re-recorded deliberately, by a different process, with a written cause.

Both are stop conditions for this bot. Neither is yours to resolve.

The second case has a written next step, and it IS yours to write even though
it is not yours to resolve. When the move is the behaviour change the lot
intends — and the lot's contract says `rebaseline_allowed: true` — announce it
in the net's ledger (`REBASELINE.md`, beside the references), in prose for the
cause **plus** a machine-readable request block. The canonical format lives in
the golden-master bot's doctrine skill; the shape is:

    <!-- iterion:rebaseline-request
    {"id": "R-<lot>-<n>", "lot": "<lot-id>",
     "cause": "one line: the intended change that moved these references",
     "expected_paths": ["exact repo-relative reference paths"]}
    -->

**The BLOCK is the request.** The prose around it is for humans; the
consuming process parses only `iterion:rebaseline-request` blocks, so however
complete and well-measured the prose, a request without the block does not
exist and nothing will ever act on it. This is measured, not hypothetical: a
lot once wrote a model announcement — perimeter measured to the file, the
future act proven in advance on a re-recorded copy — entirely in prose,
because the ledger's existing entries taught it their shape. The net stayed
red until an operator transcribed the block by hand. Match the ledger's prose
style if you like; write the block regardless, marker line copied exactly.

Commit the request with the lot's landed work, and let the lot block on its
red gate if it must: a blocked lot carrying a written request is the system
working, not failing. The party that owns the net consumes the request,
re-records, and accepts **iff** the observed diff equals your announced
`expected_paths` exactly — so measure them from the oracle's own red report,
never guess. A request from a lot whose contract says `rebaseline_allowed:
false` is refused by construction: that flag is the assertion your lot was
testing, and asking past it means the lot overflowed.

A red the ledger never heard about is a dead end: the supervising process can
only execute what was announced. Every re-baseline need, however obvious it
feels in the moment, goes to the ledger.

## The third place a fix can cheat: the environment the judge looks through

Two forms of cheating are obvious enough that any net guards against them:
editing the reference, and editing the comparator. There is a third, it is the
hardest to spot, and a lot found it unprompted.

A runtime upgrade changed one recorded field — an error message that the newer
runtime now fills in where the older left it blank. A single flag restores the
old value exactly. It was refused, for two reasons, and the second is the one to
remember.

The flag is **global**: it suppresses that class of message everywhere, logs
included, to bring one recorded field back. Restoring an observation by removing
information the runtime produces everywhere is not restoring behaviour, it is
changing a different and larger behaviour that nobody measured.

And its only possible home was the **launcher the net invokes to start the
application**. Behaviour restored there is restored *for the judge alone*. In
production the flag is absent and the message reappears — so the net would go
green on a system that behaves differently from the one that ships.

That is the shape to watch for, because the diff does not look like cheating: it
touches an environment script, not the oracle. The question that catches it:

> **Would this change be present in production?** If the fix lives only where
> the judge sets up its observation, it has not repaired anything — it has
> arranged for the measurement to come out right.

The same test the intent already carries — *would this change still be right if
the reference did not exist?* — reaches the same verdict from the other side.
Nobody disables useful diagnostics on a fresh deployment.

## Four ways a lot goes green while being wrong

Each of these passes a naive gate. Each is a way of satisfying the measurement
instead of doing the work.

**Skipping the check.** Excluding the failing module, disabling the test task,
adding the failing file to an ignore list. The command exits 0 and proves
nothing.

**Loosening a constraint to dodge a conflict.** Widening a version range until
resolution succeeds does not resolve the conflict, it defers it to whatever
version is published next — and moves the failure to a machine that is not
yours, on a day you are not watching.

**Absorbing a signal into application code.** The upgrade demands a change in
business logic to compile. That is not an upgrade detail, it is the lot
overflowing, and the application code is where it becomes invisible. Report it.

**Pinning to make it reproducible.** Freezing a transitive dependency so the
build stops moving is a legitimate action taken for a *stated* reason, and a
silent way to hide a real incompatibility taken for none. The difference is
entirely in whether the reason is written down.

## What to commit, and when

Commit as you go. An interrupted run should leave landed work, not a worktree
full of uncommitted changes that the next pass cannot tell apart from its own.

Write the reasoning where a human will find it — in the commit message and, for
anything longer, in a file you commit. A status field in a control-plane
message is read by a machine that does not care, and by no one else.
