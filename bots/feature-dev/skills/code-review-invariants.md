---
name: code-review-invariants
description: The cross-cutting invariants an adversarial self-review must check on a code diff before it ships — the defect classes that compile, pass tests, and still ship a real bug. Stack- and repo-agnostic.
---

# Code-review invariants — the defects that pass CI and still ship a bug

A green build + passing tests proves the code *runs*. It does NOT prove it
is *correct, safe, or consistent*. The defects that survive CI and reach a
human reviewer are almost always one of a small, recurring set of
cross-cutting invariant violations. Before you declare work complete,
adversarially review your own diff against this checklist — and **fix what
you find in the same pass**. The goal: the independent reviewer downstream
finds nothing.

Review the **working-tree diff** (`git diff <base>`, and `git diff --stat`
to see the whole footprint — including files you may have forgotten). For
each new or changed function, ask the questions below.

## 1. Sibling-consistency (the highest-yield check)

For every new function, handler, method, or manifest you added, **find its
nearest existing sibling** — the function that does the most similar job —
and diff how each handles the cross-cutting concerns:

- **Authorization / scoping**: does the sibling gate access (tenant, owner,
  user, permission) *before* it reads/writes? If it does and yours doesn't,
  that is a leak. (The classic: a new read endpoint that skips the
  ownership check its sibling performs → cross-tenant/cross-user data
  exposure.)
- **Error handling**: does the sibling propagate/log errors where yours
  swallows them?
- **Cleanup / lifecycle**: does the sibling register teardown that yours
  omits?

If your function diverges from its sibling on any cross-cutting concern,
either match the sibling or be able to state precisely why the difference
is correct. Silent divergence is the #1 source of shipped defects.

## 2. Lifecycle parity for new persistent state

If you added a new persistent store — a table, collection, bucket/prefix,
on-disk file, cache, index — you MUST also wire, in the same change:

- **Deletion**: when the owning entity (run, user, record) is deleted, its
  new state is deleted too. Find the existing "delete everything for X"
  path and add your new state to it. A new collection absent from the
  cleanup path leaks that data forever.
- **Retention / TTL**: if the store isn't cleaned by an explicit delete, it
  needs a bounded lifetime (TTL, lifecycle rule) — mirror what sibling
  stores use. "Neither cleaned nor TTL'd" = an unbounded leak.
- **Creation/schema**: index/schema setup wired into the same
  initialization path as siblings.

## 3. Isolation parity for new data access

Every new read/write of shared data must enforce the **same isolation
boundary** as its siblings — tenant, org, user, project. A store keyed only
on an entity id (no tenant prefix) forces the boundary to live at the
caller: load the entity under the caller's scope first, and 404 if it
isn't theirs. Check that your new path does this exactly as the established
one does. A "tenant-aware" comment is not a tenant check — verify the code.

## 4. Authority gates on background mutators

Any code that reaps, garbage-collects, reconciles, or force-mutates state
it doesn't directly own (background loops, orphan cleaners, cross-process
coordination) must gate on **real ownership authority**, never on a probe
that can succeed vacuously. A lock/lease that returns success when no real
locking backend is configured (a no-op) will make every live entity look
"unowned" and get reaped. Confirm the guard checks the *capability*
("do I actually have cross-process authority?"), not just the probe's
return value.

## 5. Claims must be provable against the real path

Any comment, doc, ADR, or commit message that asserts a behavioral or
security guarantee ("this closes the leak", "cascades on delete", "the
reaper covers case X") must be **provable by tracing the actual deployed
code path**. A guarantee that only holds in a topology you don't ship is a
false claim — scope the wording to what the code actually delivers, or make
the code deliver it. Over-claiming safety is worse than admitting a
residual gap.

## 6. Loud failure, never silent masking

An error must surface, not be disguised. A `catch`/`if err != nil` that
returns an empty result makes a genuine failure (store outage, decode
error) indistinguishable from "no data" — the panel renders empty, the
outage is invisible. Log (or propagate) the error before returning the
empty/degraded state. No silent recovery that hides a root cause.

## 7. Fit and rot — did we build the RIGHT thing, and only that

Sections 1–6 catch code that is *wrong*. This one catches code that is
*correct but hollow* — it compiles, passes, reviews clean, and still isn't
what the task needed. Two failure modes, opposite directions:

- **Fit (under-serving):** does the change serve the actual *need*,
  end-to-end, not just the literal acceptance criteria? A change can satisfy
  every stated check and still miss the point — it addresses a symptom, sits
  at the wrong layer, or handles the happy path the ticket named while the
  real workflow the user described stays broken. Re-read the original intent,
  then trace the delivered path against it. Name any gap between what was
  asked for and what the code actually does — a passing test over the wrong
  behavior is a façade, not a fix.
- **Rot (over-serving):** did the change make the codebase *worse to work
  in*? Three tells: **duplication** (a third helper doing what two existing
  ones already do — reuse the sibling instead), **over-engineering** (an
  abstraction, config knob, or generality with exactly one caller and no
  second on the horizon — inline it), and **incoherence** (now there are two
  ways to do the same thing, and the next author won't know which). Leave the
  tree more coherent than you found it, not more tangled.

Conformance to the repo's declared conventions is already covered for
cross-cutting concerns by §1; here, just confirm the change reads like the
code around it. This section is judgment, not a checklist — apply it once,
honestly, and fix what you find in the same pass. It is **advisory**: the
deterministic build+test gate stays the only thing that blocks shipping — this
lens sharpens the diff, it never gates it.## How to use this in a self-review pass

1. `git diff <base>` and `git diff --stat` — see the *whole* change.
2. For each new/changed unit, walk sections 1–7. Most changes only touch a
   few; be honest about which apply.
3. For anything you find, **fix it now** and note it. Do not defer.
4. Only when a section genuinely doesn't apply (no new state, no new
   handler, no claim) skip it — don't invent work. Small changes get a
   small review.

This is not style policing. It is the specific set of correctness,
security, and consistency defects that otherwise ship green and get caught
downstream. Catching them here is the difference between a PR that merges
clean and one that comes back with findings.
