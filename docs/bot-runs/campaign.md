# Campy 🧭 — `campaign` run bilans

Supervises a whole modernisation programme by running `modernize` as a subbot
in a bounded loop, judging progress in git, and executing the golden-master
ledger's announced re-records between runs. Not one LLM node of its own.
See [bots/campaign/](../../bots/campaign/).

## 2026-08-12 — second dogfood: an UNFINISHED programme, four runs, every core path proven (runs 019ff4af → 019ff4f2)

- Status: **VALIDATED on the paths that matter** — steward act ×2 under the
  observed==announced criterion, contract-extension governance ×2, the first
  lots landed THROUGH the loop, and at the last iteration the full machine
  cycle with no operator gesture: the worker measured red, wrote and
  committed the request block, declared blocked; the steward consumed it in
  the same iteration, recorded, matched the announcement exactly, committed
  act and verdict, replayed the full counter-test green; the lot requalified
  on the final tree.
- Target: the same finished programme, EXTENDED live with three deliberate
  one-key wording lots (`rebaseline_allowed: true`, measured single- and
  double-reference perimeters). ~$8.50 LLM across three child runs.

### What each run taught

**Run A — the producer gap.** The child did the lot impeccably right up to
the protocol: perimeter measured to the file, the future act PROVEN in
advance by replaying the full counter-test on a re-recorded copy (emergent —
nobody asked for that), blocked declared correctly… and the announcement
entirely in prose, because the ledger's existing entries taught it their
shape louder than the skill's template. `pending=[]`; nothing could act. The
skill now says THE BLOCK IS THE REQUEST in so many words, and emitted
ledgers should open with a header naming the blocks.

**Run B — the steward's act, mechanically.** An operator transcription of
the worker's own announcement, then: record, observed diff == announced set
exactly, act + verdict blocks committed, full counter-test green behind,
the lot requalified. Zero LLM.

**Run C — the honest no-move.** The operator's target key turned out to be
rendered nowhere; the child measured that, landed the change, let the net
stay green RIGHTLY, converged done with a committed explanation — and
invented no request. Meanwhile the supervisor flagged the operator's own
plan edit as a contract extension and listed it for the handoff: governance
observed working on the party that configured it.

**Run D — the gate that lied, and the loop that caught it.** Two iterations
returned `oracle green` on a tree whose serve HAD to diverge: the capture
reused an application another era had booted — a live pid dates nothing.
The child, facing a gate that contradicted its own measurement, claimed a
request in its output without committing it; the supervisor believed only
git (`moved=false`), stagnation armed, and the THIRD iteration — fresh
app, honest red — closed the full cycle machine-only. Root cause fixed in
the target's environment scripts (reuse iff build-inputs content
fingerprint matches; content over clock, since git-restored timestamps make
a fresh producer look older than its artifact) and written into the net's
doctrine skill.

### Still unexercised, named

The diff-mismatch refusal (every observed set matched every announcement so
far), `governance: human` and `escalation: interactive` (both dogfoods ran
the defaults), and a campaign long enough to hit `max_lots` or the budget
guard's declined back-edge.

## 2026-08-12 — first real run: a finished programme, walked end to end (run 019ff2f6)

- Status: **VALIDATED on the exhausted-programme path.** preflight →
  subbot no-op → observe → steward → loop_gate → finalize → human review →
  done, every transition for the reason designed.
- Target: a completed 36-lot modernisation programme (5 done / 31 blocked in
  contract terms) with its behavioural net green and its ledger fully acted.
- Cost: **$0.00 LLM** — the child's plan reader emitted a clean no-op and the
  supervisor is deterministic; ~10 min wall clock, almost all of it the final
  gate and the requalification commands.

### The verdict

The child landed nothing (`moved=no`, journal row 1), observe declared the
programme exhausted — no eligible lot remains — and loop_gate left through
the handoff tail on exactly that reason, not on stagnation. finalize replayed
the full counter-test on the committed tree: **green, with figures identical
to the programme's own final verdict** (7/7 mutants detected, no-op silent,
revert clean, collateral 0, stable, corpus 24/23 distinct). The banked run
branch carries two supervisor commits: the journal row and the handoff.

### Requalification: the bot beat the manual pass, on the manual pass's own lesson

31 blocked lots, 16 distinct gate commands, each played once on the final
tree, verdicts projected: **30/31 requalify as written**; one stays blocked
on a script the programme itself retired (exit 127, named in the handoff).

A manual requalification of the same tree had scored only 16/31 as written,
because it ran the gate commands from a host shell where the declared
toolchain was not on PATH. The engine realises the target's devbox
environment before any node runs, so the same commands — build-tool wrapper
included — exited 0 here. *Measure in the declared environment* was a lesson
this programme paid for once; the supervisor embodies it structurally.

One honesty note the handoff carries: two commands qualified for a second
database engine exit 0 **vacuously** — this tree's gate script ignores the
engine selector variables entirely, so they replay the default engine. Green
as written, not evidence about the second engine. A gate command that
ignores its own qualifier is a contract smell worth a lot of its own.

### Two v0 defects, found by the first two launches, fixed the same day

- **The engine's materialised node script is not work in flight** (`15ab7b0`).
  Every `script:` tool sees its own `.iterion-script-*.py` in the workspace,
  so the clean-tree refusal fired on a freshly created worktree — and the
  steward would have counted the file in every observed diff. All three
  status reads now carry that one exclusion.
- **bool/json inputs render as JSON literals** (`1545155`). `{{input.moved}}`
  reached python as the token `false` — NameError. One line names the three
  JSON atoms before interpolation.

Both were caught by the refusal machinery doing its job loudly, which is the
design working: neither failure was silent, both named their cause in one
line of stderr.

### Not yet exercised, named so nobody reads this bilan as more than it is

The steward's act path (no pending ledger requests existed), the
diff-mismatch refusal, contract-extension governance under both policies,
the escalation pause, and the multi-lot loop with real child runs. The next
dogfood that counts is a campaign carried from lot 1 on a programme that is
not finished — where the supervisor has to do the job it was measured
against a human doing.
