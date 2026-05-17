# doc-align (v0.2.0)

A dogfood-friendly iterion bot that detects mismatches between
project documentation and actual code state, then fixes the
**documentation** (never the code) and auto-commits on convergence.

**v0.2.0 changes** (lessons from the v0.1.0 dogfood):
- G4 gate (fixer never touches code) is now actually wired —
  v0.1.0 routed it via `{{outputs.fix_*.code_files_touched}}`
  in the user prompt, which iterion does not substitute.
- Mechanical coverage gate in `streak_check`: convergence
  requires `coverage_target_pct` (default 80%) of `doc_files`
  to be in `cumulative_audited_docs`. Stops the "Claude
  rubber-stamps after partial audit" failure mode.
- `reviewer_gpt` uses `session: inherit` from the prior
  `fix_gpt` so GPT reviews ride on the prompt cache — major
  cost reduction (v0.1.0 spent $7.49 on a single fresh GPT
  review of the workspace).
- `--var bundle_self_path=examples/doc-align` excludes the bot's
  own bundle from the audit footprint when running on its host
  repo.
- `--var diff_since=<ref>` surfaces recently-changed code files
  as a hint to reviewers (incremental mode).
- New `anchor_kind` enum (`symbol | line_range | removed |
  external`) on every blocker — makes the G3 round-trip
  auditable mechanically.

## What it audits

By default the doc footprint covers:

- `README.md` at repo root
- `docs/**/*.md`
- `CLAUDE.md` (any level)
- `examples/*/skills/*.md`
- Go function- and package-level docstrings (when
  `--var go_comment_globs="..."` is set; empty by default for
  faster first runs)

## Inviolable rules

1. **Docs follow code, never the reverse.** If a doc lies about
   what the code does, the doc gets corrected. If a doc reveals
   what looks like a code bug, the bot escalates to the human
   (`ask_user`) — it never silently rewrites correct docs to
   match buggy code.
2. **The fixer must not touch code.** Allowed extensions:
   `.md` only, plus Go code comments inside files matching
   `go_comment_globs`. Any other file appearing in
   `fix_output.code_files_touched` triggers a high-confidence
   blocker on the next iteration.
3. **The doc footprint is determined by a tool, not an agent.**
   `scan_docs` runs once at the start and emits an immutable
   `doc_files[]` that reviewers/fixers cannot reduce. Agents
   that skip a file must raise a coverage blocker, never
   silently elide it.

## Running

```bash
# From the workspace (worktree recommended for dogfooding).
iterion run examples/doc-align/ \
  --var workspace_dir=$(pwd) \
  --var doc_globs="README.md,CLAUDE.md,docs/**/*.md" \
  --var go_comment_globs="" \
  --var max_review_iterations=10 \
  --var coverage_target_pct=80

# Self-host: when running doc-align on the iterion repo itself,
# exclude the bot's own bundle so it doesn't try to "align" its
# own skills/main.bot.
iterion run examples/doc-align/ \
  --var workspace_dir=$(pwd) \
  --var bundle_self_path=examples/doc-align \
  ...

# Incremental (e.g. nightly): focus on docs that reference
# code changed since a ref.
iterion run examples/doc-align/ \
  --var workspace_dir=$(pwd) \
  --var diff_since=main~7 \
  ...
```

Pass `--var scope_notes="..."` to give the reviewers extra context
about what they should pay attention to (e.g. a recent sweeping
refactor).

## Post-run report

The committed change captures what was aligned. For the full
audit trail — every blocker raised, every audited_pair, every
fix iteration's `summary` — use the built-in report command:

```bash
iterion report --run-id <run_id> --output report.md
```

`<run_id>` is printed at the top of every `iterion run`
invocation (`Run ID: run_<timestamp>`). The generated `report.md`
includes per-node costs, the final coverage_pct, and the
chronological event stream.

## Required credentials

- `claude_code` backend reads its own OAuth token (forfait Pro/Max
  or API key — see iterion docs).
- `claw` backend with `openai/gpt-5.5` requires `OPENAI_API_KEY` in
  the environment (or `.env`).

## Convergence

The bot terminates when two consecutive iterations of opposite
families (claude / gpt) both emit `approved=true`, mirroring
`branch_improve_loop`. On convergence, the `prepare_commit` agent
selects the modified files and writes a semantic commit; the
deterministic `commit_changes` tool stages and commits.

Loop bounds: 15 review iterations, 20 recovery (fix) iterations.
