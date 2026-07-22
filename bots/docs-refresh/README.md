# docs-refresh (Doki)

Documentation alignment bot — **v3 adaptive** (one capable agent + a
mission + **truth gates only**). Converges the documentation,
exhaustively, to the actual current state of the repo: **docs follow
code, exhaustively — never the reverse**. Both halves are doc-side:

- **Repair**: update every documented claim the code has moved past
  (mismatched claims, dead links, drifted invocations, outdated
  examples) — always the **documentation**, never the code.
- **Write the missing docs**: document capabilities, surface, and code
  areas that exist in the repo but appear in no doc — or dismiss with
  a recorded reason ("internal, not user-facing" is a legitimate,
  persistent outcome).

Each aligned doc lands in stride (`docs(scope): …` + `Bot:
docs-refresh` trailer). When a repo has no docs in scope, it
bootstraps an initial set first (DEFAULT-CREATE) and then refreshes it
through the same campaign. Documented claims that read as deliberate,
unfulfilled **promises** (announced features the code hasn't caught up
with) are neither deleted nor aligned-down: the campaign records them
in a cross-pass ledger (`<scratch>/promises.json`, optionally adding
an honest in-doc status note) and the PR tail reports them under an
"Unfulfilled documented promises" section.

## Why v3 (the paradigm shift)

The v2.x lineage kept growing deterministic machinery in front of the
agent: an anchor scanner promoted to an **obligation generator**
(every regex candidate had to be adjudicated), mechanical convergence
conditions (`coverage_pct >= target`, `undocumented_count == 0`), and
a priority/chunking pipeline that decided the agent's plan for it.
Live runs measured the cost: ~200 false-positive `cli_flag`
candidates (git/docker/gh flags quoted in doc examples) adjudicated
one by one, the real work buried behind scanner noise for 4 passes,
55 min / ~$10 for +61 lines. The scanner metrics had become the goal.

v3 applies the proven Billy/Willy shape: **one capable agent + a
mission + truth-oracle gates**. The scan survives as an **advisory
hints producer**; the gate keeps only what is *true by construction*
(the tree builds; nothing outside the writeable set changed; the
agent's honest termination contract).

## Shape (v3)

```
scan_hints ──(no docs)──▶ author_docs ──▶ scan_hints   (author_rescan, once)
scan_hints ──(exact HEAD cache hit, clean tree, no issue)──────────▶ done
scan_hints ──▶ campaign ──▶ scope_check ──▶ verify_precheck
verify_precheck ──(reuse)─────────────────────────────▶ gate
verify_precheck ──▶ verify_build ──▶ verify_run ──────▶ gate
gate ──(converged)──▶ mark_issue_for_review ──▶ update_audit_cache ──▶ mr_gate
gate ──▶ scan_hints   as continuation_loop(max_passes)  — fresh hints, next pass
mr_gate ──(open_mr)──▶ forge_auth_probe ──(credential)──▶ finalize_mr ──▶ done
mr_gate ──(not open_mr)──────────────────────────────────────────────▶ done
```

- **`scan_hints`** — ONE deterministic producer of **advisory** hints:
  missing repo-rooted paths cited in docs, dead internal links /
  heading anchors, code areas no doc mentions, plus coverage
  telemetry. High precision by construction: a cited path is only
  checkable when its first segment exists as a repo directory —
  foreign-tool example lines (docker/gh flag examples, other repos'
  layouts) are silently skipped, and CLI flags are simply not scanned.
  When nothing is derivable the report degrades **silently** to empty
  hints + an explicit "explore the repo directly" note. Ledger
  entries are excluded, so the campaign's settled adjudications never
  re-surface. Also owns the noop short-circuit and the DEFAULT-CREATE
  route.
- **`campaign`** — one adaptive claude_code agent. The hints are a
  starting point, never its scope: it explores beyond them (reads the
  code, hunts the semantic drift no regex sees), adjudicates every
  issue to exactly one of **four outcomes** — fix+commit /
  dismiss+ledger / promise+promises-ledger / code-bug→board — and
  commits each aligned doc in stride. git is the durable state.
- **`scope_check`** — deterministic writeable-set containment (`.md`,
  the cache file, opted-in `go_comment_globs`) vs the run base.
- **`verify_build` + `verify_run`** — the shared stack-agnostic build
  gate (ADR-044), with `verify_precheck`'s docs-only fast path (a
  green verdict is reused when every change since the green HEAD is
  `.md`).
- **`gate`** — `converged = verify.passed ∧ scope_ok ∧
  campaign.docs_aligned` — **nothing else**. Hint counts are
  telemetry, never conditions.
- **Ledgers** — `dismissed.json` (adjudication memory) and
  `promises.json` (unfulfilled ambitions, reported in the PR body).

## Inputs (main vars)

| Var | Default | Description |
|---|---|---|
| `doc_globs` | READMEs + docs/ + CLAUDE.md | Doc footprint (universal default) |
| `go_comment_globs` | `""` | Opt-in Go comment auditing + writeable-set widening |
| `scope_notes` | `""` | Operator attention pin |
| `diff_since` | `""` | Incremental hint (`git diff <ref>...HEAD`) |
| `max_hints` | `120` | Cap on the advisory hints list (context bound) |
| `audit_cache_path` | `${PROJECT_SCRATCH_DIR}/docs-refresh-cache.json` | Noop cache (aligned git HEAD); empty disables it |
| `dismissed_path` | `${PROJECT_SCRATCH_DIR}/docs-refresh/dismissed.json` | Dismissals ledger (cross-pass memory) |
| `docs_dir` | `docs` | DEFAULT-CREATE target |
| `baseline` | `""` | Known pre-existing failures to SKIP (G5) |
| `max_passes` | `8` | Continuation-loop cap |
| `open_mr` | `false` | Push the alignment series + open ONE PR/MR at the end |
| `mr_branch` / `mr_base` | `""` | PR branch (default `iterion/docs-refresh/<run-id>`) / base (default: repo default branch) |
| `source_issue_ref` | `""` | Issue to back-link the PR URL onto (forge URL or `native:<id>`) |

Retired in v3 (the obligation machinery): `coverage_target_pct`,
`cli_surface_globs`, `diagnostic_surface_globs`,
`max_drift_candidates`, `max_review_chunk_docs`,
`include_unverifiable_symbols`, `enrich`, `enrich_area_depth`,
`code_scope_globs`.

## PR finalization (opt-in)

`open_mr=true` appends the feature-dev-verbatim PR tail: a deterministic
`forge_auth_probe` checks for a push credential (mounted `forge_token`
secret, `*_TOKEN` env, or host `gh` auth) and only then the
`finalize_mr` agent pushes the doc-alignment series and opens one PR
(GitHub `gh` / GitLab `glab` / Forgejo REST, per the shared
`forge-mr-create` skill), reporting any `drift_remaining` honestly in
the body — plus, when the campaign recorded unfulfilled promises
(`<scratch>/promises.json`), an "Unfulfilled documented promises"
section listing each doc claim and its big-picture code gap. Without a
credential the tail skips cleanly and the commits stay on the run's
storage branch. This is the delivery path for repo-targeted **cloud**
runs, whose runner clone is ephemeral — without a push the alignment
commits die with the pod.

## Run

```bash
iterion run bots/docs-refresh/main.bot \
  --var scope_notes='Align the CLI docs after the flags rework' \
  --var diff_since=main
```

Campaign skills shipped: `docs-refresh`, `doc-mismatch-taxonomy`,
`doc-enrichment` (what deserves documentation, placement, style,
dismissal discipline, obsolete-vs-promise), `doc-scope-enumeration`,
`doc-verification-checklist`, and `anti-facade-fix-rules`. The bundle
also carries the shared `verify-build` skill for its verification
agent and the shared `forge-mr-create` skill for the opt-in PR tail —
8 skills total. See [main.bot](main.bot) for the full DSL.
