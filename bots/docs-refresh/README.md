# docs-refresh (Doki)

Documentation refresh bot — **v2 minimal-framing** (ADR-058). Detects
mismatches between project documentation and the actual code state,
then fixes the **documentation** (never the code), committing each
aligned doc in stride (`docs(scope): …` + `Bot: docs-refresh` trailer).
When a repo has no docs in scope, it bootstraps an initial set first
(DEFAULT-CREATE) and then refreshes it through the same campaign.

## Shape (v2 — deterministic manifest + one campaign agent)

```
scan_docs ──(no docs)──▶ author_docs ──▶ scan_docs   (author_rescan, once)
scan_docs ──(exact HEAD cache hit, clean tree, no issue)──────────▶ done
scan_docs ──▶ scan_code_surface ──▶ build_manifest ──▶ campaign
campaign ──▶ scope_check ──▶ verify_build ──▶ verify_run ──▶ gate
gate ──▶ mark_issue_for_review ──▶ update_audit_cache ──▶ mr_gate   when converged
gate ──▶ build_manifest   as continuation_loop(max_passes)  — re-manifest, next pass
mr_gate ──(open_mr)──▶ forge_auth_probe ──(credential)──▶ finalize_mr ──▶ done
mr_gate ──(not open_mr)──────────────────────────────────────────────▶ done
```

The deterministic audit machinery is the engine's unique value and is
kept in full from v1:

- **`scan_docs`** — immutable doc-footprint enumeration (globs +
  excluded dirs + bundle self-exclusion) + inter-run cache
  pre-verification.
- **`scan_code_surface`** — opt-in CLI/flag/diagnostic inventory
  (Cobra-shaped repos; empty globs = disabled).
- **`build_manifest`** — extracts every code anchor from every doc and
  verifies each mechanically against the live tree; emits the bounded,
  severity-sorted, doc-chunked `drift_candidates` working set, the
  anchor-level `coverage_pct`, and the mechanically `verified_pairs`
  that feed the inter-run cache.
- **`scope_check`** — diffs the run base against the tree: anything
  outside the doc writeable-set (`.md`, the cache file, opted-in Go
  comment globs) fails the gate and bounces back to the campaign.
- **`verify_build` + `verify_run`** — the shared stack-agnostic build
  gate (matters when `go_comment_globs` opts comment edits in).
- **`gate`** — `converged = build green ∧ scope_ok ∧ docs_aligned ∧
  coverage_pct ≥ coverage_target_pct`. The campaign cannot rubber-stamp
  its own alignment: coverage is mechanical.

The **`campaign`** is one adaptive claude_code agent: it adjudicates
the manifest's candidates one doc at a time (verify at the anchor with
read_file/grep — the evidence rule —, fix the doc, negative-space check,
commit in stride), honours the docs-follow-code and is_code_bug rules
from the bundled skills, and files out-of-scope findings to the board
inbox. git is the durable state — a re-dispatch re-manifests and
continues from the commits earlier passes banked.

The v1 alternating cross-family review/fix loop (alt →
reviewer_claude/gpt → streak_check + dismissed-pairs/pushback/chronic
accumulators → fix_claude/gpt → prepare_commit → commit_changes →
detect_doc_changes → enforce_fix_scope) is retired — see the header
comment in `main.bot` and git history for the design and its long
convergence-hardening changelog.

## Inputs (main vars)

| Var | Default | Description |
|---|---|---|
| `doc_globs` | READMEs + docs/ + CLAUDE.md | Doc footprint (universal default) |
| `go_comment_globs` | `""` | Opt-in Go comment auditing + writeable-set widening |
| `code_scope_globs` | `""` | Code the campaign may read to verify claims (empty = all) |
| `scope_notes` | `""` | Operator attention pin |
| `coverage_target_pct` | `80` | Mechanical anchor-coverage the gate requires |
| `diff_since` | `""` | Incremental hint (`git diff <ref>...HEAD`) |
| `cli_surface_globs` / `diagnostic_surface_globs` | `""` | Opt-in surface scan |
| `max_drift_candidates` / `max_review_chunk_docs` | `40` / `30` | Context-bounding caps |
| `audit_cache_path` | `${PROJECT_SCRATCH_DIR}/docs-refresh-cache.json` | Host-persistent, out-of-tree cache; empty disables it |
| `docs_dir` | `docs` | DEFAULT-CREATE target |
| `baseline` | `""` | Known pre-existing failures to SKIP (G5) |
| `max_passes` | `8` | Continuation-loop cap |
| `open_mr` | `false` | Push the alignment series + open ONE PR/MR at the end |
| `mr_branch` / `mr_base` | `""` | PR branch (default `iterion/docs-refresh/<run-id>`) / base (default: repo default branch) |
| `source_issue_ref` | `""` | Issue to back-link the PR URL onto (forge URL or `native:<id>`) |

## PR finalization (opt-in)

`open_mr=true` appends the feature-dev-verbatim PR tail: a deterministic
`forge_auth_probe` checks for a push credential (mounted `forge_token`
secret, `*_TOKEN` env, or host `gh` auth) and only then the
`finalize_mr` agent pushes the doc-alignment series and opens one PR
(GitHub `gh` / GitLab `glab` / Forgejo REST, per the shared
`forge-mr-create` skill), reporting any `drift_remaining` honestly in
the body. Without a credential the tail skips cleanly and the commits
stay on the run's storage branch, exactly as before. This is the
delivery path for repo-targeted **cloud** runs, whose runner clone is
ephemeral — without a push the alignment commits die with the pod.

## Run

```bash
iterion run bots/docs-refresh/main.bot \
  --var scope_notes='Align the CLI docs after the flags rework' \
  --var diff_since=main
```

Campaign skills shipped: `docs-refresh`, `doc-mismatch-taxonomy`,
`doc-scope-enumeration`, `doc-verification-checklist`, and
`anti-facade-fix-rules`. The bundle also carries the shared
`verify-build` skill for its verification agent and the shared
`forge-mr-create` skill for the opt-in PR tail. See [main.bot](main.bot)
for the full DSL.
