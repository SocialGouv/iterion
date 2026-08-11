# Campy 🧭 — `campaign` run bilans

Supervises a whole modernisation programme by running `modernize` as a subbot
in a bounded loop, judging progress in git, and executing the golden-master
ledger's announced re-records between runs. Not one LLM node of its own.
See [bots/campaign/](../../bots/campaign/).

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
