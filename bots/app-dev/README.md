# app_dev (Appy 🏗️)

A complete application from a prompt — **greenfield**, on the v2
minimal-framing shape (ADR-058). ONE adaptive `campaign` agent takes
the brief (raw prompt, or a SPEC.md converged by the interview mode),
scaffolds the requested stack with its official non-interactive
scaffolder, wires the walking skeleton (builds + runs + smoke test),
then ships the app one verified semantic commit at a time.
feature-dev's deterministic build/test gate + adversarial in-loop
review re-check every pass; a draft-review human gate is the designed
"reframe after the first draft" point. Re-running against the generated
app EVOLVES it (brownfield detection) instead of re-scaffolding.

## Inputs

| Var | Required | Description |
|---|---|---|
| `app_prompt` | autonomous: yes | Natural-language brief of the app to build (interview mode may start empty) |
| `mode` | no | `autonomous` (default — free first draft) or `interview` (spec-first conversation → committed SPEC.md → campaign) |
| `draft_review` | no | Pause after convergence for ship / request_changes / hold (default true; headless paths set false) |
| `stack` | no | Open stack hint (`nextjs-dsfr`, `django`, …); empty = infer from the brief |
| `workspace_dir` | no | Defaults to `${PROJECT_DIR}` — do not override |
| `baseline` | no | Pre-existing failures to SKIP (meaningful on brownfield re-runs) |
| `max_passes` / `max_interview_turns` / `max_draft_loops` | no | Loop caps (10 / 30 / 5) |
| `deploy_enabled` | no | Opt-in deploy phase after convergence (default false) — publishes via the attached `deploy-target` skill + `deploy_credential` secret |
| `max_deploy_retries` | no | Redeploy attempts if the first deploy isn't healthy (default 2) |
| `open_mr` + `mr_branch` / `mr_base` / `source_issue_ref` | no | Opt-in PR tail — needs a forge remote |

## Shape

```
route ─ interview ─▶ interviewer ⇄ interview_chat   (Nexie loop, session
  │                       │ spec_ready               continuity; SPEC.md
  │                       ▼                          committed on disk)
  └─ autonomous ────▶ campaign → verify_probe → (verify_build) → verify_run → review → gate
                          ▲  ▲                                                     │
                          │  └── continuation_loop(max_passes), fail_log ──────────┘
                          │                                                        │ converged
                          └── draft_loop(max_draft_loops), operator feedback       ▼
                                        ▲                                     draft_gate
                                        │ request_changes                          │
                              derive_draft ◀── draft_review (human) ◀──────────────┘ (skipped when
                                 │ ship            │ hold_for_later                   draft_review=false)
                                 ▼                 ▼
                              deploy_gate ─ deploy_enabled=false ─▶ mr_gate
                                 │ enabled                            │
                                 ▼                                    │  (opt-in deploy
                              deploy → deploy_trace → deploy_verify   │   phase, AFTER
                                 ▲                          │ pass    │   the app converges)
                                 └─ deploy_retry(max) ◀─────┴─────────┤
                                                            not pass  ▼
                              mr_gate → forge_auth_probe → finalize_mr → done
```

## Greenfield mechanics worth knowing

- **Empty non-git dir**: `worktree: auto` degrades to running in place
  (engine warning, by design) — the bot `git init -b main`s and commits
  from slice 0. There is no operator branch to protect yet, so
  `--merge-into` is a no-op on that first run. Launch from a **fresh,
  dedicated directory** (its pre-existing files would be swept into the
  scaffold commit by `git add -A`).
- **Re-runs (brownfield)**: the workspace is now a git repo → normal
  worktree isolation, storage branch + best-effort FF; the contract's
  brownfield check makes the campaign evolve instead of re-scaffold.
- **Spec handoff is a file**: interview mode commits `SPEC.md` at the
  workspace root; the campaign (every pass, every resume) re-reads it
  as the authoritative spec. `app_prompt` is then just the tagline.
- **First pass verify on a bare tree**: verify_build writes an
  echo-and-exit-0 script (its skill §5) until the scaffold lands;
  verify_probe decides regeneration from a build-manifest fingerprint
  (a sha256 over the workspace's root manifests/lockfiles/build files),
  so the gate re-authors the script exactly when the toolchain changes —
  the bare-tree pass 1 (no manifests) and the scaffold landing both
  trigger it, while a reframe that only edits app code reuses the script.

## Run recipes

Free first draft (from an EMPTY directory — the app repo to be):

```sh
mkdir ~/apps/my-app && cd ~/apps/my-app
iterion run <iterion>/bots/app-dev/main.bot \
  --var app_prompt='Une app Next.js + DSFR : annuaire des administrations, recherche + page détail'
```

Spec-first interview (answer in the studio chat, or
`iterion resume --run-id <id> --answer message="…"`):

```sh
iterion run <iterion>/bots/app-dev/main.bot --var mode=interview
```

Preset: `--preset nextjs-dsfr` biases stack + skeleton and loads the
`rgaa-dsfr` skill (RGAA conformance as part of the definition of done).

## Skills shipped

`interview-playbook` (the adaptive questioning method + SPEC.md format),
`greenfield-bootstrap` (scaffold/skeleton/DoD discipline — the
stack-specific knowledge lives HERE, never in the DSL), plus byte-shared
copies of feature-dev's `verify-build`, `code-review-invariants`,
`forge-mr-create`, and `rgaa-dsfr` (for the nextjs-dsfr preset).

## Plan phase (cross-model pair review, ADR-091)

`plan_review: auto` resolves at launch from the run's credentials: when a
SECOND model family is available, the build plan is authored after the
spec hand-off (claude, read-only), critiqued by a cross-family peer
(`claw` + `openai/gpt-5.6-sol` by default), and revised by the SAME
author session before the campaign builds; otherwise the spec hands off
straight to the campaign (the v2 shape, unchanged). `plan_review_policy`
picks the mid-run peer-unavailability behaviour: `wait` (default — the
run parks failed_resumable, the usage-window retry resumes it) or `skip`
(the reviewer's `action: skip` route — continue unreviewed, loudly
stamped).
