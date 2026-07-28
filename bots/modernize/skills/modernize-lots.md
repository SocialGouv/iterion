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
