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
   "recipe":"change one byte of a served asset without renaming it"},
  {"surface":"asset","archetype":"asset_missing","required":true,
   "catches":"a manifest that fingerprints the files it finds without ever stating how many it expected — every file present still matches, and the one that stopped being produced is simply absent from the comparison",
   "recipe":"stop the build from producing one asset, by editing the build's own configuration rather than deleting the output"},
  {"surface":"a11y","archetype":"violation_added","required":true,
   "catches":"an audit that ran against something other than the page under test — a browser error page, a login redirect, or a half-built DOM — which produces a well-formed, stable, page-independent result",
   "recipe":"remove one accessible name: detach a label from its control, or drop an image's alternative text"}
]
-->

**A note on the `asset` surface.** Inventory from the BUILD OUTPUT — the archive or directory
the build produced — and never from the worktree. Assets are commonly gitignored, so a repository
scan reads a different set: it misses what only exists after a build, and it counts leftovers from
an earlier one. Then ask the running application for each entry: what is packaged and what is
served are two claims, and a lane that establishes only the first has not shown that any of those
files is reachable.

Three traps, each of which turns a defect of the net into a reported defect of the product, or
the reverse. Do not follow redirects — an authorisation refusal answers 200 with the body of the
login page, which reads as *served, with different bytes*. Percent-encode the request path but
record the raw one — a filename with a space makes a malformed request, whose transport error
reads as *absent*. And strip commented-out markup before extracting referenced URLs — a link
nobody serves is not a reference.

Emit ONE LINE PER ASSET, never a digest of the set. A digest says *something moved* and nothing
else, and the reason this lane exists — reading a build-chain upgrade — is to know which files
moved and how many.

**A note on the `a11y` surface.** Audit the RENDERED page, in a real browser, with the persona's
session cookies. An audit of raw markup measures the markup; an audit run without cookies measures
the login page under every dashboard entry's name. Ship the audit engine vendored, pinned by
version and hash, with its provenance written down: publishing results asks a reader to believe
whoever produced them, publishing the engine lets them recompute. And keep the engine version
inside the reference — bumping it can move the count without a line of the product changing, and
without that line the move reads as a regression of the application.

`a11y/run-axe.mjs` in this bundle is the runner to write into the target repo. It uses node's
built-in `fetch` and `WebSocket` and nothing else, so the lane inherits the same property as the
rest of the net: it runs in a sandbox with no egress. Three guards in it are not optional, and each
one was written after the failure it prevents. Refuse a navigation the browser reports as failed —
otherwise a dead port yields an audit of the browser's own error page, identical for every entry,
well-formed, and recorded as the reference. Check the loaded origin separately — a redirect to a
login page is not a navigation error and passes the first guard. And let the process flush its
output before exiting — a large audit truncates at the pipe buffer, and the canonicaliser then
blames the audit for what the runner did.

Emit ONE LINE PER FAULTY ELEMENT. A count of violations is not a measurement while nobody can say
which ones: the number gets re-read, the list gets checked, and a fix reads as three lines
disappearing rather than a counter dropping for a reason someone has to go and find.

**Putting the existing test suite through the same trials.** `suite-vs-net.py` in this bundle
applies every mutant in turn and runs the target's own test suite, so the two instruments that both
claim to protect against regression are measured against one set of trials instead of being
compared by assertion. It reads `test_cmd` and `test_results_dir` from `config.json` and refuses to
run without them: guessing a build tool would make it a program that sometimes measures something
other than what it reports.

The command must FORCE re-execution. A build tool that considers its test task up to date when its
inputs have not moved will skip it — and a data mutation touches none of them. The measurement then
records "the suite did not see it" where the suite did not run, exits 0, and reads exactly like a
real result. The guard against that is not the flag but the COUNT: how many cases each run actually
executed, read out of the produced reports rather than inferred from an exit code, because a
skipped task exits 0 exactly like a green one. A run with fewer cases than the baseline is reported
as an empty measurement, never as a miss.

The figure is not a quality score for those tests. A unit suite localises a fault, runs in seconds
and documents an intention; none of that is measured. What is measured is behavioural coverage of
what the application actually serves — which is the one thing a migration puts at risk.

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

**Both scripts are run honouring their shebang, and must be executable**
(`chmod +x`). A non-executable script falls back to `sh`, which on most systems
is dash — and dash has no `source`. A helper file pulled in with `source` then
never loads, every function it defined is "not found", and the mutant dies with
a bare `exit 127` that says nothing about the shell having been substituted.
Either write POSIX `sh` and use `.` instead of `source`, or declare
`#!/usr/bin/env bash` and make the file executable.

**Never derive a path from `$0`.** The harness runs both scripts with the
working directory already on the workspace, so every path you write is relative
to the repository root — and that is the only anchor a mutant may use.

A line like `cd "$(dirname "$0")/../../../.."` looks harmless and is a trap with
a delay fuse, because **sealing MOVES the held-out set out of the repository**.
In place the arithmetic is right and everything passes; sealed, the same
expression lands somewhere else entirely and the mutant dies on a file it can
see perfectly well. Worse, the radius tool runs mutants IN PLACE, so every
rehearsal is green: the defect can surface only in the one-shot scoring pass —
the single run that has no second chance.

Measured: eight held-out mutants, all eight `INVALID` at the gate, all eight for
this and nothing else. An invalid mutant neither scores nor dilutes, so the
report read `holdout 0/0` — a vacuous truth wearing a green coat, which the
conjunction passes without proving anything at all.

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

### Gate on a committed tree, always

`revert.sh` for a file mutation is almost always `git checkout -- <file>`, which restores **HEAD**.
That has a consequence nobody expects the first time: **any uncommitted change to a file a mutant
touches is destroyed by the gate**, silently, mid-run.

It gets worse than losing work. The gate captures references from the working tree it starts with,
then the first mutant revert snaps those files back to HEAD, and every later capture describes a
DIFFERENT tree. The verdict then belongs to no tree that ever existed: green or red, it does not
describe what you were testing.

Seen for real. An application change sat uncommitted, the gate ran, mutant 06 reverted the file,
and the four mutants after it reported collateral drift on two entries — a coherent-looking,
entirely fictitious finding, produced by the gate mutating the thing it was judging.

So: **commit before gating.** The harness reports a dirty workspace in its notice for exactly this
reason; treat that notice as a stop, not a remark.

## The held-out set — the one piece that must survive

`.golden-master/mutants/holdout/` is **sealed**, and the seal is mechanical, not a promise:

- the first check **relocates the set out of the workspace** — you lose file access after it;
- `GM_MODE=selfcheck` runs stability, the negative control and the visible mutants, and
  **withholds the held-out score**;
- only the final gate, which the workflow triggers and you do not, ever scores it.

The withholding is the point. A number you can watch rise is a number you can tune against: seeing
"3/5" tells you to keep hardening just as surely as reading the mutants would. Never shown to the
hardening loop, never quoted in a failure log, scored exactly once.

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
