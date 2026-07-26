---
name: oracle-mutation
description: Write mutants that prove the oracle is not blind — required archetypes, the apply/revert contract, and the sealed held-out set. Read this before writing any mutant.
---

# Proving the oracle sees

A reference that never fails proves nothing. It can be silent for two reasons that are
indistinguishable from outside: *the behaviour really did not change*, or *the comparison never
looked*. Only an injected, known divergence tells them apart.

This is not hypothetical. A recent public-sector modernisation shipped a PDF comparator that
validated, for an entire milestone, pages with **not one character on them** — a word changed in
the database still went green. The team found it by accident and wrote it up honestly. What they
never built was the procedure that would have caught it on day one.

**The contract, both directions:**

> An oracle is accepted only if it MUST see a known injected divergence, and MUST stay silent on
> a null mutation.

Each half kills one degenerate comparator. One that always reports "different" trivially detects
every mutant — the no-op control stops it. One that always reports "identical" detects nothing —
the mutation score stops it. Neither check alone is enough.

## What the harness decides, and what you decide

You write the mutants. The harness decides whether they count. It will:

- **reject a mutant that changes nothing.** After `apply.sh` it compares `git status --porcelain`
  and, when you supply `fingerprint_cmd`, a data digest. No change → `valid: false`. Not
  "undetected" — *invalid*. It can neither inflate nor dilute the score. A no-op mutant is a
  measurement fault, not evidence.
- **check every declared target moved.** `targets` is a contract, not a hint. A mutant that moves
  one target and leaves three untouched is still "detected" in aggregate while three references
  are provably blind. Those three land in `blind_lanes` and the gate goes red.
- **check nothing else moved.** A deterministic sample of non-target entries is replayed. Drift
  there means either your `targets` under-state the blast radius, or the capture is not isolated.
- **refuse a mutant that targets everything.** With no control entries left, clean collateral is
  vacuous rather than earned. Keep mutants narrow: a blast radius covering the whole corpus tests
  nothing precise.

## Required archetypes

Each archetype exists because it is blind to a *different* comparator defect. Skipping one leaves
that defect undetectable, so the set below is enforced as data, not left to judgement: for every
surface class present in the corpus, at least one **valid** mutant of every required archetype
must exist and be detected.

<!-- iterion:mutant-archetypes
[
  {"surface":"http","archetype":"value_change","required":true,
   "catches":"a reference captured against empty or absent data — the page renders a shell, so no value can ever differ",
   "recipe":"change one displayed value (a title, a label, a name) on an entity the reference actually renders"},
  {"surface":"http","archetype":"order_flip","required":true,
   "catches":"a canonicaliser that sorts collections 'to stabilise them' and destroys business ordering",
   "recipe":"reorder a listing without altering any displayed value — shift a sort key, not a label"},
  {"surface":"http","archetype":"subset","required":true,
   "catches":"a reference that checks presence but not completeness — a broken filter or pagination still matches",
   "recipe":"remove or hide exactly one item from a collection the reference lists"},
  {"surface":"http","archetype":"status_change","required":true,
   "catches":"a comparator that only reads the body and never the status line — an authorisation regression reads as identical",
   "recipe":"flip one authorisation outcome (a permitted request becomes refused, or the reverse)"},
  {"surface":"http","archetype":"field_drop","required":true,
   "catches":"a canonicaliser so lossy that whole fields are scrubbed away with the volatile ones",
   "recipe":"null out one non-volatile field the reference is expected to carry"},
  {"surface":"binary","archetype":"content_empty","required":true,
   "catches":"THE blind judge — a structurally valid document with no extractable content compared to another one",
   "recipe":"produce a valid but contentless document (a PDF whose pages carry no glyph, an empty sheet)"},
  {"surface":"binary","archetype":"value_change","required":true,
   "catches":"a comparator reading only document metadata, or a raster diff whose tolerance swallows text",
   "recipe":"change one word that must appear in the rendered document"},
  {"surface":"screen","archetype":"style_shift","required":true,
   "catches":"a pixel tolerance loose enough to swallow a real visual regression",
   "recipe":"shift one element by a few pixels, or change one colour by a small delta"},
  {"surface":"asset","archetype":"content_change","required":true,
   "catches":"a manifest built by scanning the repository instead of the build output",
   "recipe":"change one byte of a served asset without renaming it"}
]
-->

**A note on `style_shift`.** Pixel tolerance is not a comfort setting, it is *the* blinding
vector. Do not choose a threshold and then check the mutant. Tighten the threshold until
`style_shift` is detected: the tolerance is **derived from the gate**, never picked.

**A note on `content_empty`.** A binary document is never compared by rendering alone. Assert
three things: extracted text is non-empty, the canonicalised text hashes as expected, and the
raster hashes as expected. The first assertion alone would have caught the failure described
above.

## The apply / revert contract

One directory per mutant under `.golden-master/mutants/<id>/`:

```
apply.sh     make the change. Exit non-zero on failure — never mask it.
revert.sh    undo it. Must restore the exact prior state; idempotent.
meta.json    what it is and what it must move.
```

```json
{
  "class": "data",
  "archetype": "value_change",
  "surface": "http",
  "targets": ["002", "003", "004"],
  "needs_restart": false,
  "fingerprint_cmd": "…a command whose output digests the mutated state…",
  "rationale": "why this mutation is worth injecting, in one or two sentences"
}
```

- **`targets`** — every reference this mutation MUST move. Establish them **by observation**, not
  by supposition: apply the mutant, look at what actually moved, declare that. A target declared
  on a guess is reported as a blind lane, and the guess is what was wrong.
- **`needs_restart`** — `false` when the application re-reads the mutated state per request (most
  data mutations). `true` for anything requiring a rebuild or a reload. Restarts dominate the
  wall clock; do not ask for one you do not need.
- **`fingerprint_cmd`** — optional but strongly advised for mutations that leave no file trace.
  Without it, a data mutation that silently failed looks like a valid mutation the oracle missed.
- **`revert.sh` must be exact.** After reverting, the harness re-captures the targets and requires
  them back at the reference. A sloppy revert poisons every mutant scored after it.

## The held-out set — the one piece that must survive

`.golden-master/mutants/holdout/` is **sealed**. Never shown to the hardening loop, never quoted
in a failure log, scored exactly once at the final gate.

Without it, the loop overfits. The agent sees which mutants escaped, hardens the comparator until
it catches precisely those, and the oracle goes green **on its own training set**. That is the
Goodhart failure of this bot, and the held-out set is the only defence against it.

Write held-out mutants first, from the same archetypes, then stop looking at them. If the visible
set scores 100 % and the held-out set does not, the comparator was tuned, not fixed — and the
report must say so rather than quietly re-running.

## Writing mutants that are hard to catch

Reach for the ones a lazy comparator survives:

- a value that differs only in **case** or in **accent**
- a value changed to a string of the **same length**
- an ordering change among **equal-ranking** items
- a field set to `null` rather than removed
- a boundary: the **first** and the **last** element of a page, where an off-by-one hides
- a 403 that becomes a 302 — same "not shown", different meaning

If a mutant is caught by every comparator you can imagine, it is measuring your confidence rather
than your oracle.
