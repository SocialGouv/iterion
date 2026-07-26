# golden-master (Goldy) — dogfood bilan

Index + template: [README.md](README.md). Newest first.

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
