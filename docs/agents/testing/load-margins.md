# Loaded-runner bounds — re-price by ratio, never by a guessed constant

Rows of the [machine-dependent register](flakes.md) whose first failure was a
wall-clock bound blown on a loaded runner, and whose disposition re-prices the
bound against the regression it prices — a ratio, a minimum of runs, or an
explicit tripwire far under the failure it catches. The assertion the bound
protected stays asserted; only the pricing moves.

| Test / check | Machine class | Evidence (log / SHA) | Disposition | Re-check |
|---|---|---|---|---|
| `TestEvalWorkBudgetCountsNodeEvaluations` (pkg/dsl/expr) | Loaded ARC runner under `-race` | [41236299236](https://github.com/SocialGouv/iterion/actions/runs/41236299236): refusal at 20.06 s vs 10 s; 1.45 s locally | T41: tripwire at 2 min — its target is the budget not biting (~1 h). The refusal stays asserted | 2026-11-07 |
| `TestAnInstallTakesTheLockOfEveryFileItWritesOrRemoves` (pkg/botinstall) | Loaded ARC runner under `-race` | [41236299236](https://github.com/SocialGouv/iterion/actions/runs/41236299236): the lock sweep blew its 300 ms on a stalled shard; green locally ×3 | T41: test `installLockWait` at 2 s (production unchanged); the bounded wait and the 10 s watchdog are untouched. | 2026-11-07 |
| `TestSiblingBrokenListsParseInLinearTime` (pkg/dsl/parser) | Loaded ARC runner under `-race` | [41243004752](https://github.com/SocialGouv/iterion/actions/runs/41243004752) (`5058166bc`): 6.43 s vs 5 s; ~150 ms locally | T42: bound at a minute — the quadratic regression it prices takes minutes at this size | 2026-11-07 |
| `TestBackground_ADeliveredResultNoLongerHoldsTheGrace` (pkg/backend/delegate) | Loaded ARC runner under `-race` | [main witness on the T43 tip](https://github.com/SocialGouv/iterion/actions/runs/41243004752) `dfd8c8db4`: 4.15 s against the 4 s ceiling; green locally ×3 | T44: ceiling at 10 s — it prices the grace HELD past a delivered result, which parks the run for the 20 s result wait; the margin stays decisive. | 2026-11-08, and on the background-session grace |
| `TestARefusedPropertyIsNotRecorded` (pkg/dsl/parser) | Host running the full `task check` suite | T48 gate (2026-11-02): the quadrupling ratio measured 8.6x against the 8x bound with the table perfectly correct; green ×3 standalone | T49: min-of-3 on both sides, the repair T41 gave `TestBrokenListSplicesStayLinear` — load only inflates a min; a quadratic Set scores ~16x at best, so the 8x bound stays decisive. | 2026-11-09, and on parser property tables |
