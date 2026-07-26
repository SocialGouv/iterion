# golden-master (Goldy 🪞)

Builds a behavioural non-regression net for an **existing** application — and proves the net is
not blind.

The net is the easy half. Any competent agent can record HTTP responses and diff them later. The
hard half is knowing whether the diff would ever fire, and that is what this bot is actually for.

## The problem it solves

A reference that never fails is silent for one of two reasons, indistinguishable from outside:
the behaviour really did not change, or **the comparison never looked**.

This is not a thought experiment. A recent public-sector modernisation shipped a PDF comparator
that validated, for an entire milestone, pages carrying **not one character** — a word changed in
the database still went green. The same delivery shipped four public-search references that
captured `"content": []` at ~400 bytes each, because the seeded data had expired years earlier;
one of them existed specifically to cover sorting and pagination. A broken `WHERE`, an inverted
sort, a collapsed join: all still return `[]`, all still pass.

Both are the same failure: **a green that was never at risk of being red**.

## The contract

> An oracle is accepted only if it MUST see a known injected divergence, and MUST stay silent on
> a null mutation.

Each half kills one degenerate comparator. One that always reports "different" trivially detects
every mutant — the no-op control stops it. One that always reports "identical" detects nothing —
the mutation score stops it. Neither alone is sufficient; together they are a proof of
non-vacuity.

## Separation of powers

| | writes | |
|---|---|---|
| the **campaign agent** | corpus, canonicalisation rules, mutants | *what is compared* |
| **`oracle_run` + `oracle_gate`** | the harness and the verdict | *how it is decided* |

If one party owned both, a comparator returning "identical" for everything would score a perfect
run. The harness is inlined in `main.bot` precisely so the campaign cannot edit it.
`oracle-harness.py` is a reviewable standalone copy — **keep the two in sync**; the inlined one is
the one that runs.

## Four locks on the mutant set

The agent writes the mutants too, so the harness constrains them mechanically:

1. **Required archetypes are a constant in the harness**, not guidance in a skill. Each archetype
   is blind to a *different* comparator defect; an uncovered one makes the whole figure unearned.
   The run refuses before booting the application.
2. **Mechanical validity.** A mutant whose `apply.sh` changes nothing is `valid: false` — not
   "undetected". It can neither inflate nor dilute the score.
3. **Per-target detection.** `targets` is a contract: every declared reference must move. A mutant
   detected on one target while three stay silent leaves those three in `blind_lanes`.
4. **A sealed held-out set**, never shown to the hardening loop, scored exactly once. Without it
   the loop overfits: the agent hardens the comparator until it catches precisely the mutants it
   can see, and the oracle goes green on its own training set.

Plus two anti-gaming checks: **collateral** (a mutant must not move what it does not declare) and
**`uncontrolled`** (a mutant declaring the whole corpus leaves nothing to control against, so its
clean collateral would be vacuous rather than earned).

## The gate is a conjunction, never a score

```
converged = stable ∧ noop_silent ∧ revert_clean ∧ collateral == 0
          ∧ uncontrolled == [] ∧ blind_lanes == [] ∧ missing_archetypes == []
          ∧ score_pct ≥ floor ∧ holdout_detected == holdout_total
```

An aggregate would let a lane at 100 % average away a lane at 0 %. Measured on the reference
implementation: a correct oracle and a deliberately vacuous one **both scored 100 %**. Only
`blind_lanes` and the held-out result separated them.

## Graph

```
oracle_campaign → adversary_gate → [mutants_adversary] → oracle_run → oracle_gate
                                                                        ├─ converged → emit_runner → done
                                                                        └─ oracle_loop(max_passes) ⟳
```

`mutants_adversary` is a **judge, deliberately isolated**: it sees the corpus and the
canonicalisation rules but never the existing mutants, so it cannot converge on them. A distinct
lens is a property of the check, not a decomposition of the work — the same reasoning that kept
Seki's reviewer isolated under ADR-058.

## Output, in the target repository

```
.golden-master/
  config.json  corpus.json  canon/rules.py  refs/  mutants/  mutants/holdout/
  verify-oracle.sh   one entry point for CI and for humans
  REPORT.md          score, held-out result, what is still blind
```

`verify-oracle.sh` replays; `--self-check` re-runs the counter-test. **A replay alone cannot prove
the references still see** — only the counter-test can, which is why re-baselining requires it to
be re-run in full.

## Repo-agnostic

No language, framework or package manager appears in the DSL — `catalog_universality_test.go`
enforces it. The bundle declares only `python3` and `jq`; the application's toolchain comes from
the target repo's own `devbox.json` / devcontainer. The harness is **stdlib only**: no venv, no
pip, no egress, so it runs in a sandbox with no network.

The runtime forbids mounting a container socket, so the application under test runs **natively**.
`skills/deterministic-fixture.md` covers what that implies.

## Variables

| var | default | |
|---|---|---|
| `surface_scope` | `""` | free-text scope; empty = the campaign chooses |
| `mutation_floor` | `90` | minimum share of valid mutants detected — a floor, never a ceiling |
| `min_corpus` | `25` | width floor; width beats depth |
| `max_passes` | `6` | bounded hardening loop; exhausting it ships what is banked and says so |
| `adversarial` | `true` | the isolated evasive-mutant lens |
| `oracle_dir` | `.golden-master` | where the net lands in the target repo |
