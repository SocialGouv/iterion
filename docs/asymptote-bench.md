# Asymptote benchmark

`iterion bench asymptote` measures the **inter-session quality stabilisation curve** of a workflow: rerun the same task in N independent sessions and watch the per-iteration judge verdict converge. The shape of that convergence is the asymptote — the empirical reliability ceiling of the (model + recipe) on the task.

## The thesis in one paragraph

Run the same task through the same workflow in independent sessions, plot the final judge verdict per session, and the curve climbs across early sessions then *stabilises*. That stabilisation is a positive signal: it proves the recipe is doing reproducible work, and the height of the plateau is the (model + recipe)'s reliability ceiling for the task. A flat tail means a single session is a trustworthy delegate; a noisy tail means run twice and merge. Multi-family alternation (Claude ↔ GPT) can raise the plateau further on critical or complex tasks (security, cryptography), but it is *optional refinement*, not the default thesis. The asymptote is fundamentally about consistency of one recipe across sessions.

This page is the operator's guide.

## What it measures

For each run in the input set, the bench:

1. Loads `events.jsonl` from the store and walks it.
2. Identifies loop iterations via `EventEdgeSelected.Data["iteration"]` (the engine emits this on every loop-back edge).
3. For each iteration, finds the judge node's `EventNodeFinished` and reads the configured verdict field (default: `output.approved`).
4. Maps the verdict to a `[0..1]` score (booleans become `1.0`/`0.0`; numerics pass through clamped).
5. Aggregates across runs in the same group: per-iteration mean, std-error, pass-rate, and a count of how many runs reached that iteration.

The output is markdown (written to `--output <path>` or stdout) with a side-by-side comparison when a `--variant-runs` group is also supplied.

## Quick start

```bash
# Canonical: stabilisation curve over N sessions of the same workflow.
iterion bench asymptote \
    --runs run_a,run_b,run_c,run_d,run_e \
    --judge-node final_judge \
    --output report.md

# Numeric judge field with a non-default approval threshold.
iterion bench asymptote \
    --runs run_a,run_b,run_c \
    --judge-node review_judge --judge-field score --approval-threshold 0.8 \
    --include-per-run --output -

# Compare a baseline against a multi-family alternation variant on a security-
# critical task. Multi-family is *optional refinement*, not the default thesis —
# only reach for it when the cost of a missed defect outweighs the extra spend.
iterion bench asymptote \
    --runs base1,base2,base3 --label single-family \
    --variant-runs alt1,alt2,alt3 --variant-label multi-family \
    --judge-node final_judge \
    --output report.md
```

Required flags:

- `--judge-node <node-id>`: the IR node whose verdict is read each iteration. Must be a real node ID from the workflow (commonly `reviewer_claude`, `judge`, `final_judge`, etc.).

Common optional flags:

- `--judge-field <key>`: the field on the judge's `output` carrying the verdict. Default `approved`.
- `--approval-threshold <0..1>`: score threshold for the boolean "approved" mapping. Default `0.5`.
- `--loop <name>`: pin scoring to one loop name when the workflow has several.
- `--label`, `--variant-label`: human-friendly group names for the report.
- `--include-per-run`: append a per-run iteration list at the end (useful for spotting outliers).
- `--store-dir <dir>`: explicit store path. Defaults to `.iterion/` under cwd.
- `--output -`: write to stdout instead of a file.

## Reading a report

A healthy asymptote on a calibrated workflow looks like this:

```
| Iter | asymptote mean | asymptote pass-rate | asymptote n |
|---:|---:|---:|---:|
| 0 | 0.42 | 40% | 5 |
| 1 | 0.74 | 80% | 5 |
| 2 | 0.92 | 100% | 5 |
| 3 | 0.94 | 100% | 5 |
| 4 | 0.94 | 100% | 5 |
```

```
iter:   0  1  2  3  4
asympt: 4  8  *  *  *
```

Read this as: across 5 independent sessions of the same workflow, iteration 2 is the first time *every* session converges to "approved", and iterations 3-4 confirm the stabilisation. The asymptote sits at iteration 2; the small lift to mean=0.94 in iter 3-4 is residual noise on the verdict prompt, not real quality progress.

A *broken* curve looks like one of these:

| Symptom | Likely cause | What to fix |
|---|---|---|
| Curve flat at low values across all iterations | Judge prompt approves nothing, or looks at the wrong field | Re-prompt the judge; verify `--judge-field` matches the workflow's schema |
| Curve climbs then drops | Implementer regresses on later iterations (often: budget pressure, dropped context) | Check `events.jsonl` for `budget_warning`; raise the iteration cap or compaction |
| Curve climbs then plateaus *below* 1.0 | Judge prompt is over-strict on minor non-blocking issues | The verdict prompt is the load-bearing piece — re-prompt to drop nits / false positives |
| n column drops from 5 → 1 across iterations | Most runs hit the loop cap or terminated early; only outliers reach late iters | Increase the loop budget, or interpret late iters as "the difficult cases only" |

## Single group vs comparison

The bench supports two modes:

- **One group** (`--runs ...`): the canonical asymptote — a single recipe re-run N times. The report shows one column set and the per-run series.
- **Two groups** (`--runs ...` + `--variant-runs ...`): comparison. The report adds a Δ pass-rate column. Use this for A/B-ing recipe variants — most usefully, comparing a single-family baseline to a multi-family alternation variant on security-critical or complex work, to quantify whether alternation lifts the asymptote enough to justify the extra cost.

Multi-family alternation is **not** the default thesis. The asymptote thesis is fundamentally about *consistency of one recipe* across sessions; alternation is an optional refinement when the failure cost is high.

## Pitfalls

1. **Verdict prompt is the load-bearing piece**. If the judge says "approved" at iteration 0 every time, the asymptote is pinned at 0 — meaningless. If it says "needs more" forever, the asymptote sits at the loop cap regardless of real quality. Calibrate the verdict prompt against a corpus where you know the right answer before reading any bench seriously.
2. **Small samples lie**. With n < 5 the per-iteration mean is dominated by which run reached that iter. The bench prints the count column for exactly this reason — read it.
3. **Different workflows aren't comparable**. The asymptote of workflow A isn't the asymptote of workflow B. Variant comparisons must hold the workflow constant on both sides; only the recipe should differ.
4. **Loop name auto-detection**. By default the parser anchors on the *first* loop it observes that touches the judge. If your workflow has multiple loops sharing the judge, pass `--loop <name>` explicitly.
5. **Backend tagging is workflow-side, not event-side**. The bench currently doesn't introspect which backend each iteration used — that lives in the workflow's `.bot` source. To distinguish single-family vs multi-family runs, label them on the CLI (`--label`/`--variant-label`) rather than expecting the bench to infer it.

## What's not here yet

- Cross-session variance metric (the "tail tightness" quoted in the manifesto): the bench computes per-iteration std-error but doesn't surface a single tail-stability number. Coming.
- Test deltas, lint deltas, scanner-issue deltas as additional score axes (the asymptote-instrumentation plan §1 candidates). The current scoring is judge-verdict-only — sufficient for a first-pass curve, insufficient for forensic decomposition. Coming.
- Auto-tagging of multi-family vs single-family from workflow IR introspection. Currently tag manually on the CLI.
- Public benchmark suite (a fixed set of tasks + corpus + expected curves) to compare engine versions over time. Out of scope for V1.

## Related

- [persisted-formats.md](persisted-formats.md) — `events.jsonl` and artifact schema the bench parses.
- [recipes.md](recipes.md) — same-workflow-different-presets, the canonical setup for asymptote sampling.
