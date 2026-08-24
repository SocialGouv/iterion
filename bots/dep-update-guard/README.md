# dep-update-guard (Vetty) — security + alignment guard for automated dependency-update PRs

Vetty runs **per PR, on the dependency bot's own branch** (the runner checks it
out — so there is deliberately no `worktree: auto`, which would discard that
branch). It audits the bump for supply-chain risk, aligns the consuming code to
any breaking change, proves the tree with a deterministic build/test gate,
commits the alignment back onto the PR branch, and posts one review comment
carrying the verdict — optionally as a commit status that can be a required
check. It never merges past a check: unless `arm_automerge` is enabled, the
merge stays a human call, and even then only the audited commit is merged.

Stack-agnostic by construction: no ecosystem or package manager is enumerated in
the DSL. The per-stack knowledge lives in the skills
(`dependency-pr-guard`, `package-managers`, `verify-build`); the adaptive agents
read them and adapt to whatever repo the PR targets.

## When to use it

Guard a repository's automated dependency-update PRs. Enable it on a repo via
the studio Integrations flow with author filtering to the dependency bots — it
then reacts to each Dependabot/Renovate PR, posts a security + alignment
verdict, and commits any code alignment onto the PR branch. **Not** for human
PRs (use Revi / `review-pr`), and **not** for proactively opening update PRs
(that is Renovacy / `secured-renovacy`).

## How it runs

```
prepare        (tool, deterministic)  diff base_ref..HEAD, classify the changed
                                      dependency manifests/lockfiles; nothing
                                      changed → done
security_audit (agent, read-only)     malware, typosquats, compromised-maintainer
                                      signals, CVEs introduced vs resolved
audit_gate     (compute, GATE)        only `safe` proceeds; else → hold_security
align          (agent, mutating)      align consuming code to the breaking change;
                                      flags a structuring decision instead of guessing
align_gate     (compute, GATE)        needs_human → escalate
escalate       (human, llm_or_human)  auto-answers a routine question, pauses for a
                                      real architectural decision → needs_decision
verify_build   (agent, read-only)     capture the repo's real build+tests into an
                                      out-of-tree verify.sh
verify_run     (tool, GATE)           re-runs it; the REAL exit code decides. A
                                      precheck rejection (no script / a script that
                                      runs nothing) loops back as fix_verify(2)
validate_gate  (compute, GATE)        green → commit ; red → hold_unstable
commit         (agent)                commit + push the alignment onto the PR branch
commit_check   (compute, GATE)        reads the shas, not the agent's claim:
                                      committed | clean | hold_lost_alignment
post_feedback  (tool, deterministic)  compose + POST the verdict comment (and the
                                      commit status) over the forge REST API — no LLM
feedback_health(tool, deterministic)  anti-façade check that the comment/gate landed
arm_automerge  (tool, deterministic)  opt-in; arm auto-merge, or merge the audited sha
done
```

Code is committed **only** when the audit verdict is `safe` **and** the
deterministic verify gate is green; a not-safe bump or a red build
short-circuits to a hold comment. The six verdicts are `hold_security`,
`needs_decision`, `hold_unstable`, `hold_lost_alignment`, `committed`, `clean`.

Workflow block: `entry: prepare`; budget `max_parallel_branches: 1`,
`max_duration: "4h"`, `max_cost_usd: 25`; `sandbox.image:
ghcr.io/socialgouv/iterion-sandbox-sec:edge` (the scanners + build toolchain).
The bundle ships its own `devbox.json` pinning `go-containerregistry@0.21.6`
(crane), since the sandbox image has no registry client for container-digest
bumps. Secret `forge_token` is mounted `as: file` and is `optional: true`, so a
local run relying on host git auth still works. Per-node models/effort are
overridable via `VETTY_MODEL_*` / `VETTY_EFFORT_*` env vars.

## Configuration

| Var | Type | Default | Meaning |
|---|---|---|---|
| `workspace_dir` | string | `"${PROJECT_DIR}"` | The PR checkout Vetty operates on |
| `base_ref` | string | `"main"` | PR target branch; the bump is `base_ref..HEAD` (webhook-stamped) |
| `pr_url` | string | `""` | Pull/merge-request URL (webhook-stamped). Empty → the feedback step skips posting; it still audits + aligns |
| `pr_author` | string | `""` | PR author login, informational — the author FILTER lives in the webhook config |
| `scope_notes` | string | `""` | PR title + body, extra context for the audit |
| `max_fix_iterations` | int | `2` | Soft hint: self-fix attempts `align` should spend before flagging unstable (not a hard loop bound) |
| `post_to_board` | bool | `false` | Also mirror the verdict to the native board; the PR comment is the primary sink |
| `gate_enabled` | bool | `true` | Post a commit status on the PR head carrying the verdict — `success` only when audited safe AND verify green |
| `gate_context` | string | `"vetty/deps"` | The status context / name in the repo's required checks; share it per repo with any co-enabled reviewer |
| `arm_automerge` | bool | `false` | Opt-in: on a green verdict with a landed green gate, ask the forge to merge once ITS required checks pass |
| `automerge_method` | string (enum: `squash`, `merge`, `rebase`) | `"squash"` | Merge method asked for when arming; must be one the repo allows |
| `forge_publish_url` | string | `""` | Deterministic forge-publish grant, injected by the iterion server at launch; empty → direct comment, no gate |
| `forge_publish_token` | string | `""` | Token for the publish endpoint above (server-injected) |
| `scratch_dir` | string | `"${PROJECT_SCRATCH_DIR}/dep-update-guard"` | Out-of-tree scratch for the gate's `verify.sh` + log — never pollutes the PR branch |
| `verify_timeout_s` | int | `1200` | Seconds the deterministic verify may run before it is called red (reported as exit 124, never a crashed node) |

The studio launch form renders `pr_url` as the primary input and keeps
`pr_author` hidden (still settable via `--var` / bot_args).

## Invocation

```bash
# Guard an open dependency PR (branch already checked out):
devbox run -- iterion run bots/dep-update-guard/main.bot \
  --var pr_url=https://github.com/<org>/<repo>/pull/123 \
  --var base_ref=main

# Local dry pass with no forge posting (pr_url empty), longer verify budget:
devbox run -- iterion run bots/dep-update-guard/main.bot \
  --var base_ref=origin/main --var verify_timeout_s=2400

# Let Vetty arm auto-merge on a green verdict, on a repo with required checks:
devbox run -- iterion run bots/dep-update-guard/main.bot \
  --var pr_url=<url> --var arm_automerge=true --var automerge_method=squash
```

Declared invocations (`manifest.yaml`):

- **forge**, mode `direct` — `pull_request` on `opened` / `reopened`.
- **command**, mode `direct` — `/vetty` in a PR note (`scope: pr`,
  `disambiguator: when_args_empty`) re-runs the guard.

The manifest's `forge:` block drives the studio Integrations auto-provisioner:
webhook events `pull_request` + `pull_request_comment`; token scopes
`pull_requests: write`, `repository: write`, `statuses: write`; secret
`forge_token`; an author allowlist of `dependabot[bot]` / `renovate[bot]` (plus
suffix wildcards for self-hosted Apps) with `author_scope: exclusive`, so a
general reviewer co-enabled on the repo does not also review the PRs Vetty owns.

Run history and validation status: [`docs/bot-runs/dep-update-guard.md`](../../docs/bot-runs/dep-update-guard.md).
