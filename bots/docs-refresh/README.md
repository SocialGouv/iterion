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
record_run_base ──▶ context_gate   — the run base, read before any pass can write
context_gate ──(context configured)──▶ context_ingest ──▶ scan_hints ──▶ campaign ──▶ scope_check ──▶ gate
context_gate ──(no context)─────────────────────────────────▶ scan_hints
gate ──(converged)────────────────▶ mr_gate
gate ──▶ scan_hints   as continuation_loop(max_passes)  — fresh hints, next pass
mr_gate ──(open_mr)──▶ forge_auth_probe ──(credential)──▶ delivery_gate ──(paths owned)──▶ finalize_mr ──▶ surface_pr_link ──▶ done
delivery_gate ──▶ delivery_paths_refused   (typed fail: DELIVERY_PATHS_REFUSED)
mr_gate ──(not open_mr)──────────────────────────────────────────────▶ done
```

**What the delivery owns, and what it refuses:** the push carries `.md`
files and nothing else — never a symlink (a link wearing the doc suffix
publishes a host path, and the next edit of it writes outside the
repository), never a path under `.github/workflows/`, whatever its suffix
(the one directory the forge credential refuses to push), and never a file
the run committed under `.claude/`, where a `.md` file is executable
instruction rather than documentation (`CLAUDE.md`, `skills/**/SKILL.md`,
`agents/*.md`, `commands/*.md` are auto-loaded by every agent harness that
opens the branch). `scope_check` carries the same rule, measured in the same
window and read in the same frame, so a pass is asked to revert exactly what
the tail would refuse — a path only one of them refuses has no remediation
route, and the two are asserted equal on one tree
(`bots/delivery_gate_test.go`, `TestTheLoopGateAndTheDeliveryGateAgree`). A
run that still touches anything else when the loop is spent ends on
`DELIVERY_PATHS_REFUSED` with every offending path named: no pull request is
opened and nothing is delivered. The series is not lost and not secret
either — the commits stay on the run's own branch, and a repo-targeted
**cloud** run banks them to its storage branch `iterion/run-<run id>`, which
the refusal's message names.

The window those paths are measured in is the one `record_run_base` reads at
the entry, before any pass has written, handed to BOTH nodes on their
in-edges: a base re-derived from the campaign's own commit messages would let
one commit missing the `Bot: docs-refresh` trailer (what `git revert` writes,
what the engine's wip bank writes before a resume) choose the window it is
audited on. Both read the committed series and the worktree with
`--no-renames` (a protected file moved onto a doc path reports its deletion,
not only its destination) and `core.quotePath=false` (an accented doc name
compares equal), and both read the symlink term in both windows, keyed on
the destination mode (a link committed then removed from the worktree stays
refused; a stale link the run removes is a cleanup). The engine's `.claude/`
mirror and the tree noise it declares are skipped only while they stay
UNCOMMITTED, the state in which no pass can revert them (and in which the
most they reach is the run's own bank, never the pull request); anything the
run committed is judged whatever its name. An unborn HEAD — the engine's
in-place degradation for a freshly created repository — records git's empty
tree as the window, so the first commit is judged rather than refused for
want of a base.

What the gate does NOT bound is the same actor on two routes: `finalize_mr`
is an agent holding bash and the mounted `forge_token`, and one that pushes
from inside its own node is bounded by nothing here; and under `repo_devbox`
the workspace's own devbox profile is prepended to the run's PATH
(`pkg/runtime/devbox_host.go`), so the campaign — bash in that very tree —
can rewrite the `git` and `python3` the gate resolves. The gate's scrub
covers git STATE the campaign can plant (replace refs, grafts, an inherited
`GIT_DIR`), not the toolchain; the cure for the toolchain is the engine's (a
gate node resolving its binaries from the engine's PATH, never the run's).


- **`record_run_base`** — the ENTRY: `git rev-parse HEAD` in the
  workspace, before any pass can write. It is the window `delivery_gate`
  measures the delivered paths in, and an empty one refuses the delivery
  rather than falling back to a guess.
- **`context_gate`** — entry gate (mr_gate shape): at least one context
  source configured (`context_git` / `context_jira_base`) → materialize;
  none → the run never executes the ingest node and is unchanged.
- **`context_ingest`** — deterministic materialization of the OPTIONAL
  product context (runs ONCE, before the loop, per-run namespaced under
  `${scratch_dir}/context-<run id>/`): shallow-clones `context_git`
  repos (forge_token via GIT_ASKPASS — never argv; PDFs pre-extracted
  to `.txt` sidecars so claw reads them through bash) and renders the
  Jira board snapshot to markdown. A configured source that fails fails
  the node explicitly. See `skills/context-sources.md`.
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
- **`campaign`** — one adaptive agent, backend per-run selectable via
  `llm_backend` (default `claude_code`). The hints are a
  starting point, never its scope: it explores beyond them (reads the
  code, hunts the semantic drift no regex sees), adjudicates every
  issue to exactly one of **four outcomes** — fix+commit /
  dismiss+ledger / promise+promises-ledger / code-bug→board — and
  commits each aligned doc in stride. git is the durable state.
- **`scope_check`** — deterministic writeable-set containment (`.md`
  only, never a symlink, never under `.github/workflows/`, never a path
  committed under `.claude/`) vs the recorded run base — the same rule,
  window and frame as `delivery_gate`. The bot only touches docs.
- **`gate`** — `converged = scope_ok ∧ campaign.docs_aligned` —
  **nothing else**. There is no build gate: a docs-only change can't
  break the build, so running it would verify an invariant the bot
  can't violate. Hint counts are telemetry, never conditions.
- **Ledgers** — `dismissed.json` (adjudication memory) and
  `promises.json` (unfulfilled ambitions, reported in the PR body).

## Inputs (main vars)

| Var | Default | Description |
|---|---|---|
| `doc_globs` | READMEs + docs/ + AGENTS.md + CLAUDE.md | Doc footprint (universal default). A match under `.github/workflows/` or `.claude/` — reachable only through an explicit glob, `**` never enters a hidden directory — is set aside whatever the globs or `excluded_dirs` say, because the delivery refuses both trees by name, and `scan_hints` says so in its note |
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
| `context_git` | `""` | Comma-separated https URLs of reference repos to shallow-clone into `${scratch_dir}/context-<run id>/<slug>/` before pass 1 (e.g. a repo holding the product's reference PDFs). Auth via the mounted `forge_token` (never argv); PDFs are pre-extracted to `.txt` sidecars (poppler ships in the bundle's devbox) so any backend reads them through bash. Per-run dirs are reclaimed by the engine's scratch sweep (age-based, 168 h retention — `ITERION_SCRATCH_RETENTION` to tune) |
| `context_jira_base` / `_project` / `_board` / `_user` | `""` | Tracker snapshot: fetch the board's issues once and render `context-<run id>/jira/board-<board>.md` (key, type, status, summary, trimmed description — Jira Cloud v3 ADF flattened). Auth = Basic `context_jira_user` + the `tracker_token` file secret (same name review-pr binds). A configured source that fails fails the run explicitly |
| `llm_backend` | `claude_code` | Backend running the campaign node (`--var llm_backend=claw` for a claw run; pass `ITERION_DOC_ALIGN_EFFORT_CLAUDE=high` then — `ultracode` is Claude-family and degrades to `xhigh` elsewhere). The default model id `claude-opus-5-5` is itself valid on claw (the claw client speaks anthropic natively) — the env override is for choosing a different model, not for making it work |

Context is **read-only grounding** for the campaign's enrichment half
(`skills/context-sources.md`): it steers what deserves a page and what
business rule a code path implements; it is never copied into the
target repo (the `.md`-only scope gate refuses it anyway), the code
still wins over a stale spec, and a claim only the context supports is
marked `[à confirmer]` or omitted. |

Retired in v3 (the obligation machinery): `coverage_target_pct`,
`cli_surface_globs`, `diagnostic_surface_globs`,
`max_drift_candidates`, `max_review_chunk_docs`,
`include_unverifiable_symbols`, `enrich`, `enrich_area_depth`,
`code_scope_globs`.

## PR finalization (opt-in)

A scheduled run clones the repo on an ephemeral pod, so this tail is its
only delivery path: the manifest's `schedule` invocation declares
`default_vars: {mode: incremental, open_mr: "true"}`, and a schedule row
built from it (studio enable dialog, forge provisioning) delivers by
construction. A `full` reconciliation is a deliberate manual launch.

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
