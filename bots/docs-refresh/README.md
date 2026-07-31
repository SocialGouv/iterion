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
docs-refresh` trailer). When a repo has no docs in scope, the campaign
authors an initial set itself (the same adaptive agent, guided by the
doc-enrichment skill) and refreshes it in the same pass. Documented
claims that read as deliberate,
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
hints producer**; the gate keeps only what it can prove or explicitly
contract for: nothing outside the writeable set changed, and the agent
reported the corpus aligned.

## Shape (v3)

```
scan_hints ──▶ campaign ──▶ scope_check ──▶ gate
gate ──(converged)────────────────▶ mr_gate
gate ──▶ scan_hints   as continuation_loop(max_passes)  — fresh hints, next pass
mr_gate ──(open_mr)──▶ forge_auth_probe ──(credential)──▶ finalize_mr ──▶ surface_pr_link ──▶ done
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
  re-surface. A repo with no docs in scope is not special-cased — the
  campaign authors the initial set itself.
- **`campaign`** — one adaptive claude_code agent. The hints are a
  starting point, never its scope: it explores beyond them (reads the
  code, hunts the semantic drift no regex sees), adjudicates every
  issue to exactly one of **four outcomes** — fix+commit /
  dismiss+ledger / promise+promises-ledger / code-bug→board — and
  commits each aligned doc in stride. git is the durable state.
- **`scope_check`** — deterministic writeable-set containment (`.md`
  only) vs the run base. The bot only touches docs.
- **`gate`** — `converged = scope_ok ∧ campaign.docs_aligned` —
  **nothing else**. There is no build gate: a docs-only change can't
  break the build, so running it would verify an invariant the bot
  can't violate. Hint counts are telemetry, never conditions.
- **Ledgers** — `dismissed.json` (adjudication memory) and
  `promises.json` (unfulfilled ambitions, reported in the PR body).

## Inputs (main vars)

| Var | Default | Description |
|---|---|---|
| `doc_globs` | READMEs + docs/ + CLAUDE.md | Doc footprint (universal default) |
| `scope_notes` | `""` | Operator attention pin |
| `mode` | `full` | `full` = whole-corpus semantic sweep (monthly reconciliation); `incremental` = semantic pass scoped to the code changed since the last alignment (auto-detected), for weekly/per-PR runs |
| `diff_since` | `""` | Explicit incremental base (`git diff <ref>...HEAD`). Usually empty — `mode: incremental` auto-detects it from the `Bot: docs-refresh` commit trailer; pin it to force a base (e.g. a PR base) |
| `max_hints` | `120` | Cap on the advisory hints list (context bound) |
| `dismissed_path` | `${PROJECT_SCRATCH_DIR}/docs-refresh/dismissed.json` | Dismissals ledger (cross-pass memory) |
| `docs_dir` | `docs` | Docs dir skipped when scanning for unmentioned code areas |
| `max_passes` | `4` | Continuation-loop cap |
| `open_mr` | `false` | Push the alignment series + open ONE PR/MR at the end |
| `pr_url` / `base_ref` | `""` | GENERIC PR-context vars iterion sets for ANY bot launched on a PR (webhook / `/doki`). Non-empty `pr_url` ⇒ Doki self-switches to AMEND: aligns the PR's own diff (incremental, base = `base_ref`) and pushes onto the PR head + comments, instead of opening a new PR. No docs-refresh-specific engine code |
| `mr_branch` / `mr_base` | `""` | New-PR branch (default `iterion/docs-refresh/<run-id>`) / base; in amend mode `mr_branch` overrides the push target (default: the checked-out PR head) |
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

### Amend an existing PR (`pr_url` set)

Launched ON a pull request — via the generic `pr_url` + `base_ref` vars
iterion sets for ANY bot on a PR (a forge PR webhook pointed at
`docs-refresh`, or a `/doki` comment) — Doki self-switches to AMEND: the
campaign scopes to the PR's own diff (incremental) and `finalize_mr` pushes
the alignment commits onto the PR's head branch (`source_branch` — pushed by
NAME, robust to `worktree: auto`'s detached HEAD) and comments, instead of
opening a separate PR. `mr_gate` opens the tail on any PR launch (amending
the contributor's PR IS the delivery), so `open_mr` need not be set.
**Activate** by pointing a forge PR-open webhook at `docs-refresh` (or
enabling `/doki` on PR comments); until then the feature is dormant.

Limitation: the engine's fork-PR guard blocks auto-launch on PRs from a
FORK (untrusted code + token), so amend-on-PR-open covers same-repo
(internal) branches; an external fork PR is aligned only via a repo
collaborator's deliberate `/doki` comment.

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
also carries the shared `forge-mr-create` skill for the opt-in PR tail —
7 skills total. See [main.bot](main.bot) for the full DSL.
