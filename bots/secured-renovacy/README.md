# secured-renovacy (Renovacy) ⬆️

A multi-stack agentic **dependency-update** bot. Renovacy detects every
package ecosystem in the target repo, enumerates what is outdated, and lands
upgrades one verified unit at a time — batching semver-safe patches and
same-scope families, then solving the rest package-by-package with a
security/CVE audit, changelog study, breaking-change code alignment,
build/test validation, and a revert lane on any package that won't validate.
Every landed upgrade carries a per-package audit doc under
`docs/renovacy/`, and the run closes with a software bill of materials. It
runs sandboxed by default (the `iterion-sandbox-full` image) precisely
because it executes untrusted package-manager and post-install code — the
same supply-chain surface `security_audit` exists to catch.

Phase 1 (per-unit upgrades) is the reified pipeline below — its
security/CVE/revert/SBOM gates are deliberately kept in the graph (ADR-055
unit-convergence). Phase 2 is the **ADR-058 v2** minimal-framing shape: ONE
review campaign over the run's cumulative diff, gated by a deterministic
build/test verify, looping until the diff is clean and the tree is green.

## Shape

```
Phase 0   detect_stack ─▶ capture_start_sha ─▶ discover_outdated

Phase 1   discover_outdated ─▶ bucket_patches ─┬─ has_patches ─▶ batch_upgrade_patches ─▶ batch_commit ─▶ write_audit_md
                                               └─ else ────────────────────────────────────────────────┐
          bucket_families ─┬─ has_families ─▶ select_family ─▶ family_upgrade ─▶ family_align_code       │
                           │                       ▲              │ (fail)                                │
                           │                       │              ▼                                       │
                           │                 mark_family_attempted ◀─ family_revert / family_commit ◀─ family_validate
                           └─ else ─▶ select_candidate  ◀───────────────────────────────────────────────┘
                                          │ has_more
                                          ▼
              resolve_pkg_ecosystem ─▶ intel_fanout ─┬─▶ security_audit ─┐
                                                     └─▶ changelog_review ┴─▶ intel_join
                        intel_join ─┬─ not safe ─▶ mark_failed_and_continue ─▶ select_candidate (package_loop 50)
                                    └─ safe ─▶ upgrade ─▶ install ─▶ align_code ─▶ validate_upgrade
                        validate_upgrade ─┬─ stable ─▶ prepare_commit ─▶ join_files ─▶ commit_changes ─▶ write_audit_md ─▶ select_candidate
                                          └─ not stable ─▶ fix_after_upgrade (fix_loop N) ─▶ validate_upgrade / revert_changes
                        select_candidate ─ not has_more ─▶ phase2_decider

Phase 2   phase2_decider ─┬─ go_done (0 attempts or patches-only) ─▶ emit_sbom ─▶ done
                          └─ else ─▶ p2_campaign ─▶ p2_verify_build ─▶ p2_verify_run ─▶ p2_gate
                        p2_gate ─┬─ converged (green ∧ review_clean) ─▶ emit_sbom ─▶ done
                                 └─ else ─▶ p2_campaign  (review_pass_loop max_review_passes)
```

- **Phase 0** — `detect_stack` (agent, read-only) profiles every ecosystem
  and seeds a forkable session; `capture_start_sha` (tool) records the base
  SHA for the Phase-2 cumulative diff.
- **Phase 1** — patch fast-track (`bucket_patches` → `batch_upgrade_patches`
  → `batch_commit`), same-scope family batching (`bucket_families` →
  `select_family` → `family_upgrade`/`family_align_code`/`family_validate` →
  `family_commit`/`family_revert`), then the per-package solo loop
  (`select_candidate` → `resolve_pkg_ecosystem` → parallel
  `security_audit`+`changelog_review` via `intel_fanout`/`intel_join` →
  `upgrade` → `install` → `align_code` → `validate_upgrade` → `fix_after_upgrade`*
  → `prepare_commit`/`commit_changes` → `write_audit_md`, with
  `revert_changes` + `mark_failed_and_continue` as the failure lane). The
  per-package `fix_loop` and the two package loops are bounded.
- **Phase 2 (v2, ADR-058)** — `phase2_decider` short-circuits to `done` when
  there is nothing worth reviewing (zero attempts or patches-only).
  Otherwise ONE `p2_campaign` agent reviews + fixes the run's cumulative diff
  (`git diff` from `capture_start_sha`, committing in stride), then the
  deterministic gate `p2_verify_build` (writes `<scratch>/verify.sh` from the
  repo's own toolchain) → `p2_verify_run` (re-runs it on the real exit code,
  no LLM judgment) → `p2_gate` closes the bounded `review_pass_loop`.
  `p2_gate.converged = p2_verify_run.passed ∧ p2_campaign.review_clean`.
- **`emit_sbom`** writes a per-run SBOM under `docs/renovacy/` before `done`;
  both terminal paths converge on it.

## Inputs

| Var | Default | Description |
|---|---|---|
| `user_prompt` | `""` | Free-form operator context (issue title+body when dispatched); empty renders an empty "Project context" section. |
| `scope` | `"patch,minor,major"` | Which semver tiers to attempt. `major` skips the patch fast-track; a run restricted to lower tiers skips major bumps. |
| `update_scope` | `""` | What *kinds* of deps to touch — free-form, read verbatim by the agents (`libraries`, `languages`, `tooling`, `devops`, `ci_cd`, or a custom sentence). Empty = the whole dep graph. |
| `major_policy` | `attempt` | `skip` \| `gate` \| `attempt` — how to handle major upgrades. **Ask before running `attempt`** — it mutates consuming code on breaking changes. |
| `max_packages_per_run` | `30` | Cap on packages the solo loop selects in one run. |
| `fix_loop_default` / `fix_loop_major` | `3` / `5` | Per-package `fix_after_upgrade` retry budget (major-risk upgrades get the larger budget). |
| `max_review_passes` | `5` | Phase-2 `review_pass_loop` cap (bounds loop-backs → up to N+1 campaign→verify passes). |
| `override_install_cmd` / `override_upgrade_cmd` | `""` | Escape hatches for unusual setups; empty lets `detect_stack` supply the canonical commands. |
| `workspace_dir` | `${PROJECT_DIR}` | Target repo — the run's worktree (`worktree: auto`; do not override). |
| `scratch_dir` | `${PROJECT_SCRATCH_DIR}/secured-renovacy` | Out-of-tree scratch for the Phase-2 `verify.sh` / `verify.log` (never inside the target worktree). |

## Triggered by

- **Command** — `/renovacy` (alias `/secured-renovacy`) on the board;
  `min_replier_role: maintainer`. The issue title+body become `user_prompt`.
- **Schedule** — suggested weekly cron `0 4 * * 1` (board mode).
- **Board** — fires on a matching card transition.
- **Direct** — `iterion run bots/secured-renovacy/main.bot [--var scope=... --var major_policy=skip ...]`.

## Run

```bash
iterion run bots/secured-renovacy/main.bot \
  --var scope="patch,minor" \
  --var update_scope="libraries,tooling"
```

See [main.bot](main.bot) for the full DSL and [NOTES.md](NOTES.md) for the
design journal.
