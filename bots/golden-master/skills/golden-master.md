---
name: golden-master
description: Build a behavioural non-regression net for an existing application — the five surface lanes, what counts as an oracle, and what does not. Read this first.
---

# The golden master

A golden master records what an application **observably does**, then replays it to surface any
drift. The source code is not read; only external behaviour is evidence. It is the net you put
under a codebase *before* modernising it, when the existing test suite is thin or absent.

You are not writing tests. A test encodes an intention. A golden master encodes the **status
quo**, including its bugs — deliberately. A bug faithfully reproduced after a migration is a
success; a bug silently fixed is an unreviewed behavioural change.

## Deliverable

Everything lands in `.golden-master/` **inside the target repository**, committed. It is both the
net and a deliverable in its own right — a client can read it, run it, and audit it.

```
.golden-master/
  config.json     how to bring the app up and down, base URL, personas
  corpus.json     the request catalogue
  canon/rules.py  canonicalize(entry, status, headers, body) -> str
  refs/<id>.txt   the recorded references — the contractual baseline
  mutants/        proof the references are not blind   (see [[oracle-mutation]])
  mutants/holdout/  the sealed set — write, then stop looking
  REPORT.md       emitted: score per lane, escaped mutants, held-out result
  verify-oracle.sh  emitted: the single entry point for CI and for humans
```

## Five lanes

Work them in this order. Each is only worth opening once the previous one is stable.

| Lane | Captures | Opens on |
|---|---|---|
| `http` | status, meaningful headers, body | always — the widest surface for the least machinery |
| `binary` | PDF, spreadsheets, CSV exports | the app generates documents — see [[binary-lane]] |
| `screen` | deterministic screenshots | the rendering is part of the contract |
| `asset` | SHA-256 of assets **served by the build** | a library bump could change what ships |
| `a11y` | pinned-ruleset accessibility snapshot | accessibility is in scope |

An asset manifest is built from **build output**, never from a repository scan: the point is to
notice that the bytes reaching the browser changed, and a scan of source files cannot see that.

## What is not an oracle

- **A reference recorded against absent data.** If the seeded data does not exercise the surface,
  the reference captures an empty shell and can never fail. This is the single most common way a
  golden master turns out to be worthless — see [[deterministic-fixture]] for how it happens and
  how to prevent it.
- **A comparison that never looked.** A comparator returning "identical" for everything scores a
  perfect run. Only a mutation counter-test tells the two apart — see [[oracle-mutation]].
- **A capture that drifts on its own.** If two captures of an unchanged application differ, every
  later figure is noise. Stabilise before measuring — see [[canonicalization]].
- **A perimeter chosen for convenience.** "All nine screens green" is a true sentence and a
  worthless one when the application has thirty. Completeness that is an artefact of the
  perimeter is not completeness.

## Method

1. **Bring the application up reproducibly** and freeze its world ([[deterministic-fixture]]).
2. **Enumerate the surface** and choose a corpus with real width ([[surface-discovery]]).
3. **Capture, and make the capture deterministic** ([[canonicalization]]). Do not proceed while
   two identical runs disagree.
4. **Record the references — with the harness, not with your own capture.**

   ```
   GM_WORKSPACE=<repo> GM_DIR=.golden-master GM_MODE=record python3 <harness>
   ```

   The harness path is printed in the failure log, and a reviewable copy ships
   as `oracle-harness.py` in this bundle. Use it. Writing your own capture
   script means the references are produced by one code path and judged by
   another: any difference in redirect handling, header selection or decoding
   shows up later as a stability failure you will spend a pass chasing.

   Re-run `GM_MODE=record` freely while the corpus is still moving. Once the
   references are the baseline, re-recording is a re-baseline and carries the
   obligations below.
5. **Prove they see** ([[oracle-mutation]]). This is the step that distinguishes a net from a
   decoration. Check your own work with:

   ```
   GM_MODE=selfcheck python3 <repo>/.golden-master/harness.py
   ```

   Selfcheck runs the stability probe, the negative control and the VISIBLE mutants, and
   **withholds the held-out result on purpose** — seeing it would tell you whether to keep tuning,
   which is the overfitting the sealed set exists to prevent. An empty `log_tail` here means the
   visible set is clean; it is not a green gate.

6. **Stop.** The runner and the report are emitted by the workflow, not by you. Writing a
   `REPORT.md` of your own is welcome when you have something the template cannot say — your
   documented blind spots, the causes behind your canonicalisation rules — and it will not be
   overwritten. Do not write `verify-oracle.sh`.

## Re-baselining, and why it kills nets

A golden master dies by re-baselining. Something breaks three screens, someone regenerates the
references, and the net becomes a rubber stamp.

- References are **never** rewritten as a side effect of some other work.
- Every re-recorded reference carries a **written cause**, one entry per reference, in
  `REBASELINE.md`.
- After any re-baseline, the **entire** mutation counter-test is re-run, held-out set included.
- A re-baseline touching more than a handful of references in one change is not a re-baseline. It
  is a regression wearing its clothes.

## Honesty clause

If the net cannot be made to see something, **write that down** rather than narrowing the corpus
until it goes green. A documented blind spot is a usable engineering artefact. A green run
obtained by removing what failed is a lie with a timestamp on it.
