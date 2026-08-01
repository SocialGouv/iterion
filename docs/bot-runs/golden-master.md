# golden-master (Goldy) — dogfood bilan

Index + template: [README.md](README.md). Newest first.

## 2026-07-27 → 08-01 — two lanes added, and each was added because a real defect walked under the others

- Status: **hardening period, not a single run.** Written because the engine changed a lot and the
  record stopped: `bots/golden-master` gained two surfaces and about a dozen fixes while this file
  said nothing. Sustained dogfooding on a legacy Java/Spring application.
- What it is worth: every figure below was measured on that target and is replayable there. None
  of it comes from reasoning about what a net ought to catch.

### The `asset` lane, and what it found the moment it opened

The net watched HTTP responses and two binary formats. It did not watch the files a page loads —
and those are produced by the front chain and git-ignored, so nothing else watched them either.

Measured on opening the lane: **the entire client layer was absent from the baseline environment**
and answered 404 — style sheets, vendor scripts, the view framework itself. **No reference moved.**
A total absence of the client layer was indistinguishable from its presence.

That is our own instrument finding our own defect, and it is the exact family this bot exists to
refuse: a net that establishes something NEAR what it claims. "Behaviour is preserved" silently
meant "server-rendered behaviour is preserved".

The lane inventories what the BUILD packaged — from the artefact, never from the working copy —
then asks the running application for each entry. One line per resource, never a digest of the
set: reading a toolchain upgrade requires knowing *which* ones moved.

### The `a11y` lane, and the third instance of one defect family

It audits the page rendered by a real browser, not the markup. Its first finding was in a
reference of its own: an audit that reported eleven faulty nodes where the CI reported ten,
reproducible, `stable: true` on both sides. The reference had encoded **the fonts of the machine
that recorded it**.

Third time the same family appears in this project:

| where | what the reference encoded |
|---|---|
| spreadsheet | the account name of the recording machine, in a header record |
| PDF | the fonts installed on the recording host |
| accessibility audit | the same fonts, through the rendering engine |

*The reference encodes a property of the producer, not of the product.* Worth naming as a family,
because each instance looked like a different bug and the fix is the same one: declare the
environment the judge observes through, and let nothing ambient reach it.

### Confronting the target's own test suite with the same mutations

New: the same mutants are applied with the target's suite as judge instead of the net. Both claim
to protect against regression; only one of them had ever been asked to prove it.

| | detected |
|---|---|
| the net | **19 / 19** |
| the inherited suite, at discovery | **1 / 19** |
| after security and characterisation tests were added | **9 / 19** |

The measurement taught more than the ratio. **Several of the mutations are a single UPDATE with
not one character of source changed** — a title, a creation date, a counter set to null. No mocked
slice can ever see them: the double returns what the test told it to return. What was missing was
not tests, it was tests that READ THE DATABASE. And five further tests later moved the case count
without moving the ratio at all — volume and efficacy are different quantities.

### The defect in the harness, which is the one worth remembering

`tree_fingerprint` returned `None` on a tree the version-control probe could not read. Ten of
nineteen mutants were invalidated by it — and the report said **100 %**. A check that scored a
counter-test reduced by half and called it complete.

It now walks the tree when the probe fails and REFUSES to run rather than return nothing. The rule
generalises: a fingerprint function may not have a "cannot tell" value that callers read as "no
change".

Same shape, smaller, found the same week: the suite-vs-net case counter watched one of the two
locations the test command writes to. Harmless while the second held two cases; not harmless once
the second was the only one able to see a data mutation. Counts are now kept and compared per
location — a total that holds can hide one task falling to zero while another grows.

### Browser lane, five fixes that only a real runner could produce

A container browser does not render badly when it has no font — it **aborts**. It also stalls
rather than fails, phones home, and dies in ways whose only witness is a log the harness was
discarding. Each fix made the failure NAME its cause; before, a dead renderer read as a slow page
for three rounds.

### Held-out

**Six cycles published** — 7/7, 7/7, 8/8, 3/3, 2/2, 8/8. The sixth was drawn deliberately where
nothing had been tried: twenty corpus entries had never been targeted, and three successive cycles
had reused the same five archetypes on overlapping targets. Freshness is enforced by fingerprint —
the applied script plus its targets — so renaming a spent mutant cannot pass.

Its targets were **measured**, not intended: each candidate applied against the whole corpus and
its blast radius read off the result. That is what makes `collateral: 0` earned rather than
declared.

## 2026-07-27 — FULL GATE GREEN, http + binary, 7/7 sealed held-out (run 019fa4fb)

- Status: **VALIDATED** — every conjunct satisfied, `emit_runner` reached, run `finished`. This
  closes M1 and M2 together.
- Versions: bot v0.1.0 + `065fe48f7` (derived seal) · iterion built **from HEAD** (see the stale
  binary finding below) · `--sandbox none` · `min_corpus=14`, `max_passes=3`, `adversarial=false`,
  `--max-duration 2h`.
- Campaign **57.7 min, 150 001 tokens, $11.24**, one pass. Gate **3.3 min** for 14 mutants. Total
  wall 61 min.

### The verdict

```
mode gate    stable ✅  noop_silent ✅  revert_clean ✅
total 7  valid 7  detected 7  score_pct 100
collateral 0  uncontrolled []  blind_lanes []  missing_archetypes []
holdout_detected 7 / holdout_total 7      log_tail: (empty)
```

Corpus: **18 references, 16 http + 2 binary**. Mutants: 7 visible covering all five http
archetypes plus both binary ones, 7 held-out mirroring them.

### The seal fix, proven in the one place it could be proven

The previous entry's fix was **incomplete, and the incompleteness was fatal** — found by reading
the code before spending the run, not by the run itself. Scoping the seal to the run by forcing
`GM_SEALED_DIR` at the gate ignored that the campaign seals too: the `golden-master` skill has it
run `selfcheck`, in another process, without that environment. It fell back to the shared path and
**moved** the held-out set there; the gate would then have looked in the run-scoped path, found
nothing, and bailed on a seal it had itself broken.

`065fe48f7` removes the coordination instead of repairing it. Both sides **derive** the same path
from the same rule — the workspace basename is the run id inside a worktree, a stable repo name
outside one. This run confirms it end to end:

```
worktrees/gm-holdout-019fa4fb-…/   h1…h7 — sealed by the campaign's selfcheck
worktrees/gm-holdout               — never recreated
```

The general lesson, which cost two runs to learn: **a fix that changes where a value is read must
change where it is written, in the same commit.** Moving one end of a rendezvous is not a fix, it
is a second defect that looks like a fix.

Detail worth keeping: the campaign put `content_empty` on the **xlsx** lane in the held-out set
while the visible set has it on the **PDF**. A mirror with variation, not a copy — which is what a
held-out set is for.

### Three earlier fixes validated in production at the same time

- **`selfcheck` withholds the held-out score.** The campaign's own report carried
  `holdout_detected: -1, holdout_total: 7` — deliberately unequal, so a selfcheck report that ever
  reached the gate must fail it rather than pass by coincidence. The campaign saw `score_pct: 100`
  on the visible set and learned nothing about the sealed one.
- **Record mode reads as a record**, not as a failed gate: `MODE=record — 18 references written.
  No gate was run: the zeroed fields above are defaults, not a verdict.`
- **`emit_runner` passed.** It is the node that died on a `SyntaxError` two runs ago.

### The stale binary — a finding that invalidates a claim in the previous entry

The first launch of this run emitted the six bundle-skill warnings that `34b2f3b4e` was supposed
to have removed. The installed `iterion` on PATH was **ten days old** and predated every engine
commit in this campaign. Consequences, stated plainly:

- The engine fixes recorded in earlier entries as "fixed" had **never been exercised** — every
  prior run used an engine without them. They are now genuinely validated: the warnings are gone,
  and a failing tool call logs its rejected payload (see below).
- The same stale binary rejects six catalog bots at `iterion validate` with
  `E002: expected variable name, got [` — the `[enum: …]` variable syntax postdates it. Nothing is
  wrong with those bots; **validate against a binary built from HEAD**, or the diagnosis is about
  the tool.

General form, worth generalising beyond this bot: **a dogfood run proves the code that ran, not
the code in the tree.** Record the binary's provenance in the bilan, not just the repo sha.

### Frictions observed (none fatal, none bot defects)

- A shell hook on the operator's machine rewrote the agent's `find … -exec` into a wrapper that
  does not support `-exec`. The agent recovered on the next turn. Visible **only** because
  `ea1e8a0da` now prints the rejected payload — without it the log said `Exit code 1` and nothing
  else.
- The `sleep`-then-check guard fired once. The agent immediately adopted the
  `until <check>; do sleep 10; done` form the error message suggests. A guard whose message
  teaches the correct form costs one turn and is doing its job — reclassified from "friction" to
  "working as intended".
- `finalize` banked uncommitted worktree changes as wip commit `aebb064` on the storage branch,
  unmerged. Expected with `--auto-merge=false`.

### Lessons for next run

- `min_corpus` is **enforced nowhere** — it exists only in the campaign prompt. The same family as
  the seal that was "a sentence in a skill". Counting **distinct** reference hashes against the
  floor closes it, and simultaneously stops byte-identical references on different paths from
  inflating apparent width (a defect observed on a real third-party net: two distinct export
  endpoints, one reference, so the only behavioural difference between them was captured nowhere).
- The adversarial lens is still off. Turn it on only now that a run converges without it.

## 2026-07-26 — binary lane built and proven, and a false overfitting diagnosis (run 019f9ed4)

- Status: **ABORTED on a harness defect** — the campaign delivered, the gate lied. Two fixes
  landed; the run that validates them has not been spent yet.
- Versions: bot v0.1.0 + binary lane (`5886cdc4d`) · iterion `76df5e75d` · `--sandbox none` ·
  `min_corpus=14`, `max_passes=3`, `adversarial=false`, `--max-duration 2h`.
- Campaign: **54 min, 176 000 tokens** for **18 http + 2 binary** references and **7+7** mutants —
  **+42 %** over the http-only campaign, which is the price of the lane.

### The defect that matters: the seal was not scoped to the run

The gate reported a held-out set of **12 where 7 were written**. The sealed directory was a fixed
path under the project scratch, so held-out mutants from earlier runs accumulated into it and the
gate scored a mixed population.

This is the worst failure mode available to this bot, and it is worth naming precisely: it does
not let a bad oracle through, it **makes a sound oracle look overfitted**. A false red is not the
safe direction here — it is the direction that gets the whole net distrusted and re-tuned against
noise. Fixed by scoping the seal with `{{run.id}}` (`6df7fd0c1`).

Same commit: the campaign's output schema required **five fields, not one of which any node
read**. Five obligations on the agent, zero consumers — pure Goodhart surface. Reduced to the one
field the graph actually uses.

### The three residual weaknesses of the previous entry, closed (`babc22ef8`)

- **`selfcheck` mode** — scores the visible set, and withholds the held-out score rather than
  reporting it as zero. It is set to `-1` **deliberately unequal** to the total, so a selfcheck
  report that ever reached the gate must fail it rather than pass on a coincidence.
- **The report carries its mode**, so a record-mode skeleton stops reading as a failed gate.
- `GM_MODE=gate` is **forced** at the gate node, not defaulted — an agent cannot leave a stale
  mode in the environment and have the gate honour it.
- The `golden-master` skill no longer ends its method list with a step the graph performs.

### The binary lane, and a premise of mine that measurement disproved

Built and proven by hand before handing it to the campaign: **green gate at 7 mutants** (5 http +
2 binary) on the target's PDF exports, with `content_empty` detected.

The skill originally asserted that *rendering alone is never enough*. That is **false**, and the
measurement says so: a **raster-only** comparator, with no text assertion at all, detects
`content_empty` and passes a full gate on this application. Rasterising is not a weak method.

What made the reference failure possible — a public-sector modernisation whose PDF comparator
validated blank pages for an entire milestone — is that *their* renderer had **no font data**
(pdf.js in a hermetic context, `disableFontFace`, no standard-font URL). It drew nothing, and
blank matched blank. The same comparator with fontconfig access catches everything.

The rule that replaced mine, now in [binary-lane.md](../../bots/golden-master/skills/binary-lane.md):

> A rendering comparator is exactly as good as its renderer's font access — and that property is
> **invisible in the diff**. Nothing in a green result tells you which of the two situations you
> are in.

Which reframes the `content_empty` mutant: it is not only a trap detector, it is a **positive
diagnostic that tells you which renderer you have**. Archetypes for the lane are enforced as
harness data (`content_empty`, `value_change`), not left to the agent's judgement.

### Target-side defects found (baseline harness, the target repository)

- **The jar was rebuilt only when missing** (`bc0eedd`). Every mutant touching code or a template
  — the only way to build some mutations, including a structurally valid document with no text —
  had **no observable effect**: the net replayed the old jar and declared the mutant invalid. A
  mutation harness that silently tests a stale artifact is indistinguishable from a blind oracle.
- A cold `baseline-up` **exceeds an agent's 2-minute tool timeout** (`99ea343`) — documented
  rather than papered over, since the fix is to run it in the background, not to make it faster.

### Engine improvements this run produced

- `6d8401cc4` — the bundle-skill warning from the previous entry is gone: a skill already
  satisfied by the bundle/plugin mirror no longer reports as absent from the library. It was true,
  useless, and read as a broken run at every start.
- `76df5e75d` — **a failing tool call now logs the payload that was rejected** (bounded to 600
  characters, passed through the secret guard so a failure cannot turn a log line into a leak
  channel). Six schema failures had been undiagnosable from the log. Worth recording *why*: I
  claimed they were a truncation, and they were not — the model had emitted XML parameter tags
  inside a JSON string value. I also claimed the payload was not recoverable; it was in
  `events.jsonl` all along (`tool_started`, field `input`). There was no engine bug on that path.
  The real gap was the log line, and that is what the commit fixes.

### Lessons for next run

- The run repo must be rebuilt with **fresh history** before each run, not merely cleaned:
  `worktree: auto` branches from repository HEAD and prior oracles stay reachable. Verify with
  `git rev-list --all --objects | grep -c golden-master` → 0.
- An aborted run still leaks `mysqld` and the jar; `baseline-down` never runs on interruption.
  Kill them before relaunching or the next run captures the previous application.
- One forfait probe, **5 minutes minimum**. Two concurrent probers earn an HTTP 429 and disarm the
  guard exactly when it is needed.

## 2026-07-26 — GATE CONVERGED on a legacy Java/Spring application: 5/5 visible, 5/5 sealed held-out, no blind lane (run 019f9e47)

- Status: **VALIDATED** for the `http` lane — the graph-triggered `oracle_run` converged. One
  defect remained, on the last node, after convergence.
- Versions: bot v0.1.0 + the four fixes from the previous entry · iterion `dev+75eb03daaedc` ·
  `--sandbox none` · `min_corpus=12`, `max_passes=3`, `adversarial=false`, `--max-duration 2h`.
- Campaign: **37.1 min, 123 388 tokens**, one pass. Gate: **46.5 s**.

### The verdict (authoritative, from the graph — not the agent's self-check)

```
stable ✅  noop_silent ✅  revert_clean ✅
total 5  valid 5  detected 5  score_pct 100
collateral 0  uncontrolled []  blind_lanes []  missing_archetypes []
holdout_detected 5 / holdout_total 5     log_tail: (empty)
→ oracle_gate: converged
```

### What the four fixes bought, measured

- **`GM_MODE=record` used 3 times, zero shadow harness written** (the previous run built its own
  capture script *and* its own mutation scorer with divergent semantics). The campaign now
  self-checks with the code path that judges it.
- **`harness.py` materialised** (29 504 bytes) — the emitted runner has something to call, and the
  campaign can record with it.
- **The seal held**: `mutants/holdout/` left the worktree at the first gate; the five held-out
  mutants were scored from outside the workspace.
- **Shebang honoured** — no repeat of the `exit 127` that invalidated every mutant last time.

### Deliverable quality — above what was asked

The campaign's own `REPORT.md` is better than the emitted template, which is why `emit_runner`
now refuses to overwrite one that already exists. Three things it did that the reference
implementation (hand-written, by a human) did **not**:

- **`order_flip` via a non-serialised column.** It reorders the page through `date`, which carries
  `@JsonIgnore` — the order changes with *no displayed value changing at all*, not even at day
  granularity. Placed in the held-out set. A canonicaliser that sorts arrays is totally blind to
  it.
- **Volatility neutralised by JSON key name, not by regex.** `timestamp` → `<TS>`, with the
  explicit note that `publicationStartDate` shares the ISO format but is business data and stays.
  The hand-written oracle used an ISO regex — coarser, and exposed to the over-scrubbing the
  `canonicalization` skill warns about.
- **A second cause for scrubbing stack traces that was not previously known**: beyond the
  per-boot CGLIB proxy hash, JVM reflection inflation renames `NativeMethodAccessorImpl` to
  `GeneratedMethodAccessorNNN` after ~15 reflective calls. A trace in a reference therefore drifts
  with *request volume*, not just with restarts.

It also documented its blind spots without being asked to justify them away: no true 404 (the
`/{slug}` catch-all returns a 200 shell), binary exports out of scope, mail flows not exercised,
four roles with no seeded account and how they are covered by substitution.

Non-vacuity, the consultancy's failure, avoided: public search renders **349 records**, not `content: []`.
Credit where due — the fixture in the target's baseline harness already widened the publication
window; the campaign inherited that rather than diagnosing it.

### The seventh defect: `emit_runner` (fixed in `322230023`)

The run failed on its **last node, after the gate had converged**:

```
"GM_DIR=" + json.dumps({{vars.oracle_dir}) + "}"
                                        ^ SyntaxError
```

The DSL expands environment expressions — **including the `${VAR:-default}` form** — before the
script runs. The default clause ran to the first closing brace, which was the closing brace of
the next template substitution. **Shell brace syntax is unusable inside a `.bot` script body.**
The emitted runner now uses no shell braces at all (`[ -n "$VAR" ] || VAR=…`, and `set -e`
without `-u`, since the usual guard for `-u` would itself need `${VAR-}`).

### Corrections to the previous entry

- "The campaign does not commit as it goes" was true of run 019f9e18, **not** a general property:
  this run banked two commits (`golden-master: behavioural net`, then `add REPORT.md`).
- The 60% held-out figure reported by the previous run's *shadow* scorer conflated three failure
  modes. The real harness separates them: one invalid mutant, one under-declared blast radius,
  zero actual blindness.

### Residual weaknesses, not yet fixed

- **Gate mode reveals the held-out score to whoever runs it.** The seal stops the campaign
  *re-reading* the mutants, but an agent running gate mode still sees `holdout_detected`. Seeing
  3/5 would tell it to keep tuning even without file access. A `selfcheck` mode that scores the
  visible set and stays silent on the held-out one would close this.
- **Record mode returns the full gate skeleton** (`total: 0`, `noop_silent: false`,
  `score_pct: 0`) with only `log_tail: "recorded N references"` to distinguish it. It reads as a
  failed gate. The report should carry its mode.
- The `golden-master` skill's method list ends with "emit the runner and the report", a step the
  **graph** performs — which is why the campaign wrote its own `REPORT.md`.

### Budget, measured

The forfait usage endpoint (`api/oauth/usage`) is **itself rate-limited**: a 90 s watchdog plus a
120 s sampler earned an HTTP 429 and disarmed the guard at the moment it was needed. One prober,
5 min minimum. Over 37 minutes of active campaign the 7-day window did not move a measurable
point (integer resolution).

## 2026-07-26 — first real runs on a legacy Java/Spring app: 6 defects found, 4 in the bot (runs 019f9e0b → 019f9e18)

- Status: **PARTIAL** — the bot-owned half is validated end to end against a real legacy
  application; no run has yet reached a converged gate. Four bot defects and two harness defects
  found, all fixed and regression-tested.
- Versions: bot v0.1.0 · iterion `dev+75eb03daaedc` · `--sandbox none` · backend `claude_code`
  (opus-4-8).
- Target: a legacy Java/Spring application — Spring Boot 2.0.3 / Java 8 / MySQL 5.7, server-rendered Thymeleaf with
  sprinkled Vue 2 and a JSON API. Legacy: 747 files, **28 test files, no CI at all**. Baseline
  brought up natively (no container: the sandbox forbids mounting a container socket), MySQL 5.7
  from nixpkgs, cold nix realisation 95s, full teardown+rebuild 11s.
- Method: `min_corpus=12`, `max_passes=3`, `adversarial=false`, `--max-cost-usd 15`,
  `--max-duration 45m`. Four runs: three aborted on environment isolation, one complete.

### What the campaign produced (run 019f9e18)

20 references, 4 personas (`anon`, `admin`, `manager`, `consultor`), 10 mutants — 5 visible
covering all five required HTTP archetypes plus 5 held-out mirroring them. Quality above
expectation for a first run:

- The **refusal lane captured the 302 itself** (`STATUS 302 / Location: /login`, 29 bytes), not
  the login page it redirects to — the trap `surface-discovery` warns about.
- The **`order_flip` mutant** moved one row to the top of a creationDate-sorted list by pushing
  its time to `23:59:59`, leaving the *displayed* date (day granularity) unchanged. That is the
  hard one to write, and it found the only lever the schema offered after discarding two easier
  candidates in its reasoning.
- It **anticipated collateral before running it**: "unpublishing ID 1 would also affect the
  back-office list, where it would show as DRAFT instead of PUBLISHED" → changed mutant.
- Targets were declared **by observation**, naming the actual value and row, not guessed ids.

### What the gate did (the point of the bot)

```
stable: true   noop_silent: true   missing_archetypes: []
total: 5   valid: 0   detected: 0   score_pct: 0
log_tail: mutant m01_value_change is INVALID: apply.sh exited 127: backup_field: not found
```

Correct on every axis: it refused to score, counted the mutants **invalid rather than
undetected**, and fed the exact error back to the campaign, which repaired in pass 2.

One unplanned result worth keeping: **`stable: true` means the references the campaign recorded
with its OWN capture script matched the harness's capture byte for byte.** The
`canonicalize(entry, status, headers, body)` contract is robust enough that two independent
capture implementations converge — a risk that did not materialise.

### Bot defects found (all fixed in `6a2e6c19e`)

1. **The harness forced `sh`, ignoring the shebang.** /bin/sh is dash on most systems, which has
   no `source`; the mutants' helper file never loaded and every function it defined was "not
   found". The author sees `exit 127` and no hint the interpreter was swapped. → run scripts
   honouring their shebang, `sh` fallback when not executable.
2. **The held-out set was sealed by a sentence in a skill and nothing else.** The campaign simply
   executed it, learned which mutants escaped, and could then harden against them — the exact
   overfitting the set exists to prevent. → relocated out of the workspace at the first gate, so
   the seal holds from pass 2 on, which is where hardening compounds. A broken seal is now
   reported explicitly instead of surfacing as a phantom missing archetype.
3. **`verify-oracle.sh` pointed at `.golden-master/harness.py`, a file nothing ever wrote.** The
   emitted runner was unusable. → the harness materialises itself there from `__file__`, which
   also gives the campaign and CI the code path that judges them.
4. **No documented way to record references**, so the campaign wrote a shadow harness with
   different semantics (it conflated invalid / blind / collateral into one "FAIL", reporting
   60% where the real gate would have reported one invalid mutant and one under-declared radius).
   → `GM_MODE=record` documented in the `golden-master` skill, next to why a shadow harness is a
   trap.

### Environment defects found (in the target's baseline harness, not the bot)

5. **MySQL socket truncated at 107 characters.** `struct sockaddr_un.sun_path` is capped;
   `~/.iterion/projects/<bot>/worktrees/<uuid>/.state/poss/mysql/mysqld.sock` is 109. The kernel
   truncates silently, mysqld listens somewhere the client never looks, the baseline never boots.
   Invisible on a dev checkout (71 characters). → socket moved to a short `/tmp` path keyed by a
   hash of the repo path, plus an explicit length guard.
6. **Fixed ports.** Two working copies fought over 18080/13306. At best the second refuses to
   start; at worst it captures the first one's application and produces a net that looks valid
   and describes a different tree. This happened — a dev instance was left running during a run.
   → ports derived from the repo path like the socket, effective URL published to
   `../.state/poss/baseline-url.txt`.

None of these six was findable by review. All required a real run.

### Iterion frictions observed (not bot defects)

- **`worktree: auto` branches from the repository HEAD, not from the branch checked out in the
  cwd.** Isolating a run by checking out a branch does not work; the run tree must be a
  repository whose HEAD is already what you want. Removing files from the tree is not enough
  either — history stays reachable.
- **The cost budget is not enforced in flight on the `claude_code` path.** After 43 minutes and
  174 events, `events.jsonl` carried no cost, token or usage key; telemetry arrives only at
  delegate completion (110 582 + 30 785 tokens, known afterwards). `--max-cost-usd` is recorded in
  `run.json` but nothing measures against it. **`--max-duration` is the only working control** —
  it fired correctly: `budget hard limit reached: duration at 96%`.
- **An interrupted run leaks the processes its agent started.** The campaign's `baseline-up` is
  never torn down when the budget cap fires; a `mysqld` from an aborted run was still holding its
  port minutes later.
- **Bundle skills emit a misleading warning.** All five are reported "not in the skill library —
  not mirrored" while being correctly mirrored into the run worktree's `.claude/skills/`. The
  check tests the library path without accounting for bundle skills.
- **Claude Code auto-backgrounds long Bash commands**, so a following `EXIT=$?` captures the
  backgrounding rather than the script. An agent-authored `verify.sh` that trusts that exit code
  reads an imaginary success.

### Timings, for calibrating the next run

| | |
|---|---|
| campaign pass 1 | 33.8 min, 110 582 tokens |
| campaign pass 2 | 8.7 min, 30 785 tokens |
| gate (`oracle_run`, bailed early on invalid mutants) | 28 s |
| **`--max-duration 45m` was too tight** | the cap fired entering the second gate |

### Lessons for next run

- Budget by **duration**, not cost, until the telemetry gap is closed. 2h is a realistic first
  cap for a 20-entry corpus with 10 mutants.
- The campaign does **not** commit as it goes despite the instruction; an interrupted run leaves
  the work in the worktree but unbanked. Worth strengthening in the prompt, or gating on it.
- The adversarial lens was off for these runs. Turning it on should only happen once a run
  converges without it, otherwise two variables move at once.
