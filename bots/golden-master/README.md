# golden-master (Goldy 🪞)

Builds a behavioural non-regression net for an **existing** application — and proves the net is
not blind.

The net is the easy half. Any competent agent can record observable responses or generated
artefacts and diff them later. The hard half is knowing whether the diff would ever fire, and that
is what this bot is actually for.

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

## Surfaces

Goldy works outward from the application's observable boundary rather than its
source code. The campaign can cover five lanes: HTTP responses; binary exports
(PDF, spreadsheets, CSV); deterministic screenshots; hashes of assets served by
the build; and pinned-ruleset accessibility snapshots. A lane is opened only
when the application exposes it. Binary references require non-empty extracted
text plus canonical-text and raster assertions when rendering is available;
`content_empty` and `value_change` mutants make a broken extractor or renderer
visible.

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

## Locks on the mutant set and the deliverable

The agent writes the mutants too, so the harness constrains them mechanically:

1. **Required archetypes are a constant in the harness**, not guidance in a skill. Each archetype
   is blind to a *different* comparator defect; an uncovered one makes the whole figure unearned.
   The run refuses before booting the application.
2. **Mechanical validity.** A mutant whose `apply.sh` changes nothing is `valid: false` — not
   "undetected". It can neither inflate nor dilute the score.
3. **Per-target detection.** `targets` is a contract: every declared reference must move. A mutant
   detected on one target while three stay silent leaves those three in `blind_lanes`.
4. **A mechanically sealed held-out set** is moved outside the workspace at the first check.
   `--self-check` exercises visible mutants but withholds the held-out score; only the final gate
   scores it. Once the gate converges, `promote_audit` publishes the spent set under
   `mutants/audit/<cycle>/` and commits it as replayable evidence. The next cycle must draw a fresh
   set; fingerprints, not names, prevent laundering a published mutant through a rename.
5. **A width and replayability floor.** `min_corpus` applies to distinct reference hashes, not raw
   entry count, and the gate refuses a runner or harness that is absent or gitignored.

The same gate checks **collateral** (a mutant must not move what it does not declare),
**`uncontrolled`** (a mutant declaring the whole corpus leaves no control), a whole-corpus null
mutation, exact reverts, and a clean stable capture. Gating on an uncommitted tree is reported
loudly because mutant reverts restore `HEAD` and would otherwise judge a tree that never existed.

## The gate is a conjunction, never a score

```
converged = stable ∧ noop_silent ∧ revert_clean ∧ collateral == 0
          ∧ uncontrolled == [] ∧ blind_lanes == [] ∧ missing_archetypes == []
          ∧ corpus_distinct ≥ min_corpus ∧ runner_replayable
          ∧ holdout_reused == [] ∧ score_pct ≥ floor
          ∧ holdout_detected == holdout_total
```

An aggregate would let a lane at 100 % average away a lane at 0 %. Measured on the reference
implementation: a correct oracle and a deliberately vacuous one **both scored 100 %**. Only
`blind_lanes` and the held-out result separated them.

## Graph

```
oracle_campaign → adversary_gate → [mutants_adversary] → oracle_run → oracle_gate
                                        ├─ converged → promote_audit → emit_runner → done
                                        └─ oracle_loop(max_passes) ⟳
```

`mutants_adversary` is a **judge, deliberately isolated**: it sees the corpus and the
canonicalisation rules but never the existing mutants, so it cannot converge on them. A distinct
lens is a property of the check, not a decomposition of the work — the same reasoning that kept
Seki's reviewer isolated under ADR-058.

## Output, in the target repository

```
.golden-master/
  config.json  corpus.json  canon/rules.py  canon/test_rules.py  refs/
  mutants/  mutants/holdout/  mutants/audit/<spent-cycle>/
  harness.py         the exact capture-and-gate implementation
  verify-oracle.sh   one entry point for CI and for humans
  REPORT.md          score, held-out result, what is still blind
```

`verify-oracle.sh` runs the full gate; `--self-check` runs stability, the null control, and visible
mutants while deliberately withholding the held-out result; `--record` re-records references.
**A replay alone cannot prove the references still see** — only the counter-test can, which is why
re-baselining requires it to be re-run in full.

## Repo-agnostic

No language, framework or package manager appears in the DSL — `catalog_universality_test.go`
enforces it. The application's toolchain comes from the target repo's own `devbox.json` /
devcontainer. Goldy's own devbox pins `python3`, `jq`, and Poppler (`pdftotext` / `pdftoppm`) for
the binary lane. The Python harness itself uses only the standard library: no venv or pip.

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
