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
  route-coverage.json    justified holes in the ROUTE perimeter  (see [[surface-discovery]])
  feature-coverage.json  the FEATURE inventory, standard >= 3    (see [[surface-discovery]])
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

## Putting the runner in CI, and the three ways that job goes green without judging

The emitted `verify-oracle.sh` is the entry point a pipeline calls. Wiring it up is where a net
that works gets neutralised, because a CI job can be perfectly green while never judging anything.
Ask, before writing the job: **what would make this job green without it judging anything?** There
are three answers, and all three have been seen.

**The job never runs.** `allow_failure: true`, `when: manual`, or a trigger rule that excludes the
very pipelines that matter. The job exists, appears in the configuration, and the merge stays
green. Refuse trigger conditions on a job of record rather than reason about them: deciding whether
a given rule always fires means simulating the CI platform, and an analysis that gets it wrong
hands out a green on a job that never ran.

**The exit code is swallowed.** `runner.sh | tee gate.log` returns tee's status, so the job is
green while the log it just wrote says `GATE RED` in full. `|| true`, `set +e` and a trailing
`exit 0` do the same thing less subtly. Keep the job body to ONE committed script and `exec` the
runner from it: the exit code then belongs to the runner structurally, with no shell left in
between to lose it.

**The environment is bent.** The runner reads its mode from `GM_MODE`; set that to `record` in the
CI project variables and every pipeline re-records the references and exits 0 — permanently green,
evidence destroyed, and not one line of the repository touched. The emitted runner takes its mode
from its arguments for exactly this reason. The general shape is worth carrying elsewhere:
**cheating through the environment the judge observes through** leaves no trace in the diff.

And the job body must itself be falsifiable in both directions, which means running it: green on
an intact tree, RED on an injected behavioural change, from a checkout of committed content only.
A pipeline nobody has ever seen go red is a pipeline that has never been tested — the same claim
the net refuses to accept about a comparator.

## The app the gate observes is the app THE GATE booted from the judged tree

A live pid dates nothing. An application booted by another era answers every probe, and every
capture taken from it describes a tree that is not the one being judged — in either direction.
Measured twice: a re-record once captured the previous version's output as the new reference, and
a supervisor's gate twice replayed GREEN on a tree whose serve had to diverge, because the capture
reused an app a previous run had left up.

The rule the environment scripts must carry: **reuse a running app iff the content fingerprint of
everything that PRODUCES the artifact matches the fingerprint recorded at its boot** — sources,
build files, the declared toolchain lock, the wrapper. Content, never clocks: rebuild decisions by
mtime lie as soon as files arrive through git, which restores COMMIT timestamps, so a freshly
imported producer can look older than the artifact it must rebuild. Anything else is app-down and
a fresh boot from the tree. A residual state is recovered from; it is never reported as a success.

## Re-baselining, and why it kills nets

A golden master dies by re-baselining. Something breaks three screens, someone regenerates the
references, and the net becomes a rubber stamp.

- References are **never** rewritten as a side effect of some other work.
- Every re-recorded reference carries a **written cause**, one entry per reference, in
  `REBASELINE.md`.
- After any re-baseline, the **entire** mutation counter-test is re-run, held-out set included.
- A re-baseline touching more than a handful of references in one change is not a re-baseline. It
  is a regression wearing its clothes.

### The ledger's machine-readable blocks — this is the canonical format

Prose in `REBASELINE.md` carries the cause; three HTML-comment blocks carry the protocol, so a
supervising process can execute the separation of powers mechanically. Other skills point HERE
rather than restating the format — two declarations drift.

The party that may NOT re-record (a modernisation worker) announces a request:

```
<!-- iterion:rebaseline-request
{"id": "R-<lot>-<n>", "lot": "<lot-id>",
 "cause": "one line: the intended behaviour change that moved these references",
 "expected_paths": ["exact repo-relative reference paths, read from the oracle's own report"],
 "replaces": "R-<lot>-<n> (OPTIONAL: an earlier pending request this one subsumes)"}
-->
```

`replaces` is how supersedence is DECLARED: acting the newer request closes the
chain it replaces. Prose saying "this replaces R-X" counts for the reader and
for nobody else — the harness only follows the field.

**A pending request is a conjunction term.** The gate refuses while any request
is unanswered (`pending_rebaselines` in the verdict): a pending request means
known-diverging entries are quarantined out of the verdict, and a green built
around them narrows the net while reporting progress — the exact failure this
bot exists to catch, one level up. Clearing the ledger is not housekeeping to
do after convergence; it is what convergence MEANS.

The party that owns the net answers with an act — written ONLY after re-recording, and ONLY when
the observed diff equalled `expected_paths` exactly, collateral included — then a verdict once the
full counter-test replayed green on the committed tree:

```
<!-- iterion:rebaseline-act
{"id": "R-<lot>-<n>", "lot": "<lot-id>", "recorded_paths": ["…"], "ts": "…"}
-->
<!-- iterion:rebaseline-verdict
{"id": "R-<lot>-<n>", "gate": "green", "sha": "<commit>", "ts": "…"}
-->
```

A request whose `id` has no act is PENDING. `expected_paths` is the announcement the act is judged
against: observed == announced, exactly, or nothing is committed. An unparseable request block is
an escalation, never a silence — and a refusal, once recorded by the consuming process, is
committed state that is not retried.

Two cautions. Never paste an example block into the ledger itself: a consumer reads every block as
real, fences included. And `expected_paths` are measured, not guessed — the oracle's red report
names the references that diverged; announce exactly those.

A ledger this campaign emits should OPEN with a short header naming these three blocks. A worker
under pressure learns a file's convention from the file, and a ledger whose visible entries are
all prose teaches prose — measured: a model announcement, complete in every way except the block,
that nothing could act on until an operator transcribed it by hand.

### The extension ledger — additions, the one change a bot may grant

`EXTENSIONS.md`, beside `REBASELINE.md`, carries the ADDITIVE counterpart with the opposite
authority. A re-baseline MOVES a reference, so only a human act closes it. An extension ADDS an
observation point — a new route, a state only modernised code reaches — and the net's own subbot
(`extend.bot`) may act it, because an addition is checkable: it cannot mask an existing
divergence, it can only add a constraint. Same block idiom:

```
<!-- iterion:extension-request
{"id": "E-<lot>-<n>", "lot": "<lot-id>", "type": "add-file|add-entry",
 "paths": ["new reference paths under refs/, if add-file"],
 "corpus_entries": [{"id": "<new-entry-id>", "...": "the full entry, tuple included"}],
 "justification": "one line: the observation the intent requires and the net lacks"}
-->
<!-- iterion:extension-act
{"id": "E-<lot>-<n>", "lot": "<lot-id>", "recorded_paths": ["…"], "ts": "…"}
-->
```

An `add-entry` implies its reference: the gate demands `refs/<id>.txt` for
every corpus entry, so the acting bot captures it (`GM_MODE=record` scoped
with `GM_RECORD_IDS=<id,…>` — never a full re-record) and records it in the
act; a claimed entry claims its derived reference, no `paths` line needed.
Entry ids are file-name-safe by construction — an id carrying a path
separator would derive a reference OUTSIDE refs/ and is refused.

**A pending extension request is a conjunction term too** (`pending_extensions` in the verdict),
for the mirrored reason: it names coverage the intent already knows is missing, and a green built
while it waits reports that coverage anyway. No `replaces` chain here — a request that no longer
applies is acted or withdrawn by its requester, never superseded.

What the acting side is held to, mechanically (`GM_MODE=extend-verify`, judged in git against the
run's base): every recorded path is a PURE addition — a refs/ file absent at base, or corpus
entries where every base entry survives equal. A rewrite, a delete, or a rename (its delete side
loses) is the masking vector wearing an addition's name → refused, re-baseline ledger, human. An
added entry no acted request claims is smuggling → refused. An added entry whose observation
tuple equals an existing one is a COLLISION — the tuple is the `OBSERVATION_FIELDS` allowlist
(`method`, `path`, `persona`, `surface`, `fields`, `steps`, `params`, `query`, `body`,
`readback`, `no_redirect`, `csrf_field`), NOT "the entry minus its id": a distinguishing field
outside the allowlist does not disambiguate, and absent and empty compare equal. Two references
for one observation resolve later by a cleanup that picks the masking direction → refused. The
ledger is append-only: an edited trail audits nothing.

## The `write` surface — the only one a read-only capture cannot reach

Every other surface watches a response **served**. A corruption that happens
when content is **stored** — a tag lost, an attribute normalised, an identifier
drawn afresh on every save — moves no reference and passes the gate green.
Measuring it takes a script outside the net, which is exactly where such proofs
end up when the net cannot write.

A `write` entry declares:

```json
{
  "id": "065", "persona": "…", "surface": "write",
  "method": "POST", "path": "/…/edit/6",
  "fields": { "…": "…" },
  "readback": "/…/edit/6",
  "csrf_field": "_csrf"
}
```

and the capture holds **two** things: what the write answered, and what the
readback rendered. The second is the one that carries the verdict.

Three rules, and each of them was learned by paying for it:

1. **`restore` is not optional.** The configuration must declare a command that
   puts the seed back, and the harness refuses to capture without it. A write
   lane without a restore is not a lane, it is a contamination: the first entry
   that posts leaves the world in another state, and every entry captured after
   it describes something nobody seeded — silently, since each stays stable from
   one pass to the next. Writes are captured LAST for the same reason.

2. **Write to something the DOMAIN accepts.** A row invented for the lane, that
   no other reference observes, is the tempting design and it can be refused by
   the model itself — an enumerated key is not a free string, and an invented one
   makes the edit screen answer 500 and the save 400. Worse, such a row left
   behind breaks an unrelated page later, on a request that asked for nothing.
   Prefer a real record, and let the fixture declare its content so the restore
   is exact rather than approximate.

3. **The payload is not decorative.** Put in it the shapes a migration or an
   upgrade is known to lose: semantic tags that render like presentational ones,
   attributes a renderer ignores, ordering. Those are what come back deformed.

## Honesty clause

If the net cannot be made to see something, **write that down** rather than narrowing the corpus
until it goes green. A documented blind spot is a usable engineering artefact. A green run
obtained by removing what failed is a lie with a timestamp on it.
