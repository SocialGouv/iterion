# doc-align (v0.13.1)

A dogfood-friendly iterion bot that detects mismatches between
project documentation and actual code state, then fixes the
**documentation** (never the code) and auto-commits on convergence.

**v0.13.1 hotfix** — enforce_fix_scope argv handling:
The v0.13.0 rewrite passed `declared_paths` and `cumulative_so_far`
to enforce_fix_scope via shell env vars
(`DECLARED={{input.declared_paths}}`), expecting the substituted
value to be a single token. But iterion shell-tokenises string[]
inputs into space-separated quoted entries (`'a.md' 'b.md'`), so
only the first token landed in `DECLARED` and the rest became
orphan argv. The script then tried to `json.loads('')` and the
node failed on iter 1 of the v0.13.0 retest. v0.13.1 switches to
positional argv with a `--` sentinel between the two arrays —
the established pattern that already works in update_audit_cache.

**v0.13.0 changes** (Goodhart-proof scope-honesty gate — threshold
moved from prompt rule into the deterministic tool):
- v0.12.0 exposed the raw revert history (`cumulative_reverted_paths`,
  duplicates preserved) to the reviewer and asked it to apply a
  `≥3 reverts → blocker, <3 → silence` rule. The v0.12 dogfood
  showed the reviewer being **adaptive** — at threshold=1-2 it
  embedded scope hints inline in unrelated blockers' `suggested_fix`
  ("**Declare docs/cli-reference.md in modified_doc_files this
  iteration** — it has been silently reverted twice"). The
  silence-below-threshold rule was respected in the LETTER but
  not the SPIRIT, and the inline hints didn't change fixer
  behaviour anyway (iter 3 fix_claude over-reached MORE on the
  hinted paths). Net effect: Goodhart-prone reviewer side-talk
  with zero behavioural change.
- v0.13 fixes this by construction. The threshold now lives in
  `enforce_fix_scope`'s Python script: it receives the cumulative
  revert history from `streak_check` each iteration and emits a
  `chronic_paths[]` output containing only paths that have crossed
  ≥3 reverts. `streak_check` accumulates this into
  `cumulative_chronic_paths` (deduplicated). The `alt → reviewer`
  edges pass ONLY this thresholded list — `cumulative_reverted_paths`
  and `previous_reverted_paths` are no longer in `review_input`.
  The reviewer cannot raise scope-honesty blockers on a
  sub-threshold path because the data for that decision is no
  longer in its inputs.
- The system-prompt rule 6 is also simpler: "if
  cumulative_chronic_paths is non-empty, raise one blocker per
  element; if empty, say absolutely nothing about scope". No more
  per-path counting in the prompt.
- The raw `cumulative_reverted_paths` accumulator stays in
  `streak_state` (kept for telemetry / events and re-fed to
  enforce_fix_scope each iter for the threshold computation),
  but it is **internal**: not exposed to the reviewer.
- Schema changes: `enforce_fix_scope_input.cumulative_so_far`
  (new), `enforce_fix_scope_output.chronic_paths` (new),
  `streak_state.cumulative_chronic_paths` (new),
  `review_input` loses `previous_reverted_paths` and
  `cumulative_reverted_paths`, gains `cumulative_chronic_paths`.
- Bot graph unchanged (14 nodes, 21 edges) — pure
  schema + prompt + edge-mapping + Python-tool change.

**v0.12.0 changes** (scope-honesty gate — reviewer-mediated
feedback on repeated over-reach):
- The v0.11 dogfood showed `enforce_fix_scope` correctly
  reverting out-of-declaration edits — BUT `fix_claude` then
  repeated the EXACT SAME 2-file scope violation
  (`docs/cli-reference.md` + `docs/development.md`) on iters
  2, 3, AND 4. The filet works mechanically; the fixer never
  learned. v0.10 over-reach (fix_gpt editing the bot's own
  skill) was a one-off; the v0.11 dogfood revealed a chronic
  pattern instead.
- v0.12 wires the revert history into the REVIEWER, not into
  the fixer. The reviewer now reads
  `input.cumulative_reverted_paths` (every revert, duplicates
  preserved, fed forward via streak_check) and raises ONE
  blocker per path appearing ≥ 3 times. The fixer learns from
  the next reviewer's CRITIQUE channel, not from a direct
  "you were reverted" signal — which would risk Goodhart's
  law (pre-declare-everything games the contract).
- New schema fields: `streak_state.cumulative_reverted_paths`
  + `review_input.{previous,cumulative}_reverted_paths`. New
  edge mappings: reviewer→streak_check now carries
  `enforce_reverted_paths`; alt→reviewer now carries the
  history. streak_check accumulates WITHOUT `unique()` —
  duplicates are the signal.
- Rule lives in `review_system` point 6 (SCOPE-HONESTY GATE)
  with mechanical thresholds (≥ 3 = blocker, < 3 = silence).
  The blocker reuses `mismatch_kind: stale_behavior_description`
  to match the existing G4 contract-violation pattern; no
  taxonomy growth required.
- Bot graph unchanged (still 14 nodes, 21 edges) — pure
  schema + prompt + edge-mapping change.

**v0.11.0 changes** (mechanical fix-scope enforcement):
- New tool node `enforce_fix_scope` runs between every
  `fix_claude`/`fix_gpt` and `alt`. It walks
  `git diff --name-only` against the fixer's declared
  `modified_doc_files[]` and runs `git checkout --` on every
  edit that's NOT in the declaration. The fixer's own
  output is the contract — out-of-declaration writes are
  reverted before the next reviewer sees them.
- Catches the v0.10 dogfood gap: fix_gpt edited
  `examples/doc-align/skills/doc-verification-checklist.md`
  (the bot's own skill) despite `bundle_self_path` excluding
  it from `doc_files`. The fixer's tool access (write_file,
  file_edit) is filesystem-wide; previous releases relied on
  skill-level discipline ("you may not write outside scope")
  which the agent occasionally over-reached.
- `reverted_paths[]` is emitted in events for visibility — a
  non-empty list signals the prompt needs reinforcement or the
  agent is consistently over-stepping.
- Workflow grows by 1 node / 3 edges (14 nodes, 21 edges
  pre-rest). Loop count unchanged (still 20 recovery iters).

**v0.10.0 changes** (telemetry trailer for post-run analysis):
- `prepare_commit` now requires the commit message to end with a
  `Bot: doc-align` trailer line. Auto-commits become findable
  via `git log --grep "^Bot: doc-align"`. Combined with
  `iterion report --run-id <id>` for per-run cost data, this
  makes both cost telemetry and revert tracking trivial:
  ```bash
  # All doc-align auto-commits across history
  git log --grep "^Bot: doc-align" --oneline

  # Reverts of doc-align commits (false-positive signal)
  git log --grep "^Revert" --grep "Bot: doc-align" --all-match --oneline
  ```
- This closes the v0.3.0 telemetry gap noted in the
  "limitations" section: we couldn't tell which fixes survived
  vs got reverted. Now you can `git log --grep` to find every
  doc-align run's commit, then check if any of them were
  subsequently reverted.

**v0.9.0 changes** (reviewer discipline tightening):
- New `STEP 0b — First-iteration triage` in the verification
  checklist instructs the iter-0 reviewer to do a fast
  inventory pass over `doc_files[]` (add every touched file
  to `audited_docs` for coverage) before deep-diving. Targets
  the v0.3.0 dogfood pattern where iter 0 covered ~25% of
  files and the cross-family reviewers spent iters 1-7
  chasing the long tail.
- New `STEP 1b — Adversarial spot-check` requires every
  reviewer to re-grep 3 random entries from
  `input.previous_audited_pairs` before voting `approved=true`.
  Makes cross-family alternation a MECHANICAL honesty check
  rather than a statistical one: padding `audited_docs` to
  fake coverage now requires the next reviewer's randomly
  sampled spot-check to miss the padding, which is unlikely
  over a 5-iteration loop.
- No bot.bot or schema changes — pure skill discipline.

**v0.8.0 changes** (configurable exclusion → multi-repo audit):
- `excluded_dirs` is now a `--var`-overridable comma-separated
  list (was hardcoded in scan_docs's python). Default still
  covers `.iterion`, `.works`, `.claude`, `vendor`,
  `node_modules`, `.git`, `dist`, `build`, `out`.
- Drop `.works` from the list to audit sibling repos checked
  out under `.works/<name>/` in the same run:
  ```bash
  iterion run examples/doc-align/ \
    --var excluded_dirs=".iterion,.claude,vendor,node_modules,.git,dist,build,out" \
    --var doc_globs="README.md,CLAUDE.md,docs/**/*.md,.works/*/README.md,.works/*/docs/**/*.md"
  ```
- Cross-repo references (an iterion doc citing
  `.works/claw-code-go/...`) were already supported at the
  reviewer level (read_file is unrestricted); v0.8.0 makes the
  scanner symmetric.

**v0.7.0 changes** (GPT session inherit, finally):
- `reviewer_gpt` now declares `session: inherit_if_available` —
  a new iterion runtime mode that behaves as `inherit` when
  `_session_id` resolves to a non-empty value, and silently
  falls back to `fresh` otherwise. Logs which path fired.
- The `alt -> reviewer_gpt` edge wires
  `_session_id: "{{outputs.fix_gpt._session_id}}"`. On iter 1
  (cold) fix_gpt hasn't run and the substitution is empty → run
  fresh. On iter 2+ the reviewer rides fix_gpt's prompt cache,
  cutting per-iter cost roughly 30-50% (v0.2.0 cold reviewer_gpt
  cost up to $7.49; expected $2-4 with cache).
- This is the realisation of the v0.2.0-attempted-and-reverted
  Fix 3. The original revert was correct under the diagnosis-of-
  the-moment (we thought empty session_id broke claw) but the
  real cause was an OpenAI quota issue. With v0.5.0's SSE-error
  surfacing in claw + the new tolerant mode, the optimisation
  is safe to land.

**v0.6.0 changes** (counter-omission audit):
- New deterministic tool node `scan_code_surface` extracts
  "publicly-exposed identifiers" from the workspace via grep:
  CLI commands (cobra `Use:` literals), CLI flags
  (`StringVar`/`BoolVar`/… registrations), and diagnostic codes
  (`Cxxx` constants). Run on this repo it surfaced 26 commands,
  70 flags, 54 diagnostic codes in 50ms.
- New `mismatch_kind` value `undocumented_capability` captures
  the counter-omission case: a code-exposed identifier exists
  but no doc in scope lists it. Distinct from
  `obsolete_capability` (the doc→code direction).
- Reviewers receive the surface lists in `input.cli_commands` /
  `cli_flags` / `diagnostic_codes` and audit code→doc presence
  alongside the existing doc→code audit.
- `--var cli_surface_globs=""` and `--var diagnostic_surface_globs=""`
  both empty disables the surface scan — for library-only repos
  with no CLI or diagnostic surface to document.

**v0.5.0 changes** (inter-run audit cache):
- `scan_docs` now reads `.iterion/doc-align/audit-cache.json`
  (path configurable via `--var audit_cache_path`). For each
  doc whose content sha1 AND every previously-cited code-file
  sha1 are unchanged since the last successful run, the doc is
  emitted in `pre_verified_docs` and seeded directly into the
  coverage gate's `cumulative_audited_docs`. Repeat runs on an
  already-aligned workspace can hit the streak gate on the
  first iteration without re-reading the unchanged majority —
  5-10× speedup on incremental usage (nightly / per-PR).
- New terminal tool node `update_audit_cache` runs after
  `commit_changes` and rewrites the cache from
  `streak_check.cumulative_audited_pairs`. A failed commit
  short-circuits before this node, so the cache only records
  audit state that was actually shipped.
- Cache invalidation is conservative: ANY change to ANY
  referenced code file invalidates the doc's pre-verification.
  No anchor-level precision (we'd need a language-aware
  parser); the trade-off is a slightly higher miss rate for a
  much simpler implementation.
- Empty `audit_cache_path` disables both the read and the
  write — for one-shot CI runs that want a guaranteed fresh
  audit.

**v0.4.0 changes** (anchor_kind tightening):
- `doc-mismatch-taxonomy.md` now carries a STRICT consistency table
  between `anchor_kind` and `code_anchor` shape. A blocker with
  `anchor_kind: symbol` but `code_anchor: "<no longer exists>"`
  is now defined as an inconsistency the fixer pushes back on.
- New `STEP 4b — Anchor consistency self-check` in the reviewer
  checklist; new `Rule 6` in the fixer's anti-façade rules.
- iterion's expr can't validate the json-typed `blockers` field
  mechanically (no JSON parsing in expressions), so the gate is
  prompt-and-skill discipline, ratified through cross-family
  alternation. A future iterion compute primitive could move
  this check into a deterministic node.

**v0.3.0 changes** (lessons from the v0.2.0 dogfood deadlock):
- Streak gate now treats `blocker_count == 0` as effective
  approval. The v0.2.0 streak required `approved=true` from both
  reviewers consecutively; GPT settled into a stable "0 blockers
  + confidence: low" state after substantive findings were
  exhausted, which the v0.2.0 gate didn't accept. The bot
  alternated forever without converging. New verdict field
  `blocker_count: int` (explicit count emitted by the agent —
  needed because iterion's `length()` on a json-typed field
  returns the JSON-encoded char count, not the array size).
- `coverage_pct` is clamped to ≤ 100 in `streak_check`.
  Reviewers sometimes put paths into `audited_docs` outside the
  original footprint (e.g. a `.md` outside `doc_globs` that
  legitimately covers a code surface they were verifying); the
  raw formula pushed the reported coverage to 101+%. Harmless
  for the gate (`>= 80%` still fires) but confusing in reports.
- Fix routing now guards on `blocker_count > 0` instead of
  `length(blockers) > 0` — the latter was effectively a no-op
  because the JSON string is always non-empty even for `[]`.

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
