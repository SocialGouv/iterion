# dep-update-guard (Vetty) — security + alignment guard for automated dependency-update PRs

Vetty runs **per PR, on the dependency bot's own branch** (the runner checks it
out — so there is deliberately no `worktree: auto`, which would discard that
branch). It audits the bump for supply-chain risk, aligns the consuming code to
any breaking change, proves the tree with a deterministic build/test gate,
commits the alignment back onto the PR branch, and posts one review comment
carrying the verdict — optionally as a commit status that can be a required
check. It never merges past a required check: unless `arm_automerge` is
enabled, the merge stays a human call, and even then only the audited commit is
merged.

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
continuity     (tool, deterministic)  is this head's change one a green verdict
                                      already covers? carry → post_feedback
                                      (`carried`); already gated →
                                      continuity_standby → arm_automerge; else
                                      (a head moved since prepare included) → audit
security_audit (agent, read-only)     malware, typosquats, compromised-maintainer
                                      signals, CVEs introduced vs resolved
audit_gate     (compute, GATE)        only `safe` proceeds; else → hold_security
align          (agent, mutating)      align consuming code to the breaking change;
                                      flags a structuring decision instead of guessing
align_probe    (tool, deterministic)  git reads what align left: work still pending
                                      in the workspace, and whether the PR's own
                                      commits change every file align claims
align_gate     (compute, GATE)        needs_human → escalate
escalate       (human, llm_or_human)  auto-answers a routine question, pauses for a
                                      real architectural decision → needs_decision
verify_build   (agent, read-only)     capture the repo's real build+tests into an
                                      out-of-tree verify.sh
verify_run     (tool, GATE)           re-runs it; the REAL exit code decides. A
                                      precheck rejection (no script / a script that
                                      runs nothing) loops back as fix_verify(2)
validate_gate  (compute, GATE)        green → delivery_probe ; red → hold_unstable
delivery_probe (tool, GATE)           can the runtime token push what `align` wrote?
                                      HEAD..worktree plus what HEAD adds on top of
                                      origin/<branch> (never the bump, already on the
                                      remote); a workflow file whose delivery is
                                      REFUSED → hold_delivery_permission, no push; a
                                      preflight that did not answer → the stand-down
preflight_standdown (compute)         the one refusal that publishes nothing: no
                                      comment, no gate, arm_automerge still asked to
                                      take a previous pass's arming down, run ends
                                      FORGE_PREFLIGHT_UNAVAILABLE
commit         (agent)                commit + push the alignment onto the PR branch
commit_probe   (tool, deterministic) the workspace HEAD as ONE commit on the head
                                      prepare read, held by the repository pr_url
                                      names (ls-remote), and named by the report
                                      when it names a commit
commit_check   (compute, GATE)        decides from commit_probe and align_probe's
                                      readings — never from the agents' summaries:
                                      committed | clean | hold_lost_alignment
post_feedback  (tool, deterministic)  compose + POST the verdict comment (and the
                                      commit status) over the forge REST API — no LLM;
                                      stamps the change identity, and repins a green
                                      verdict onto a head that moved by a pure rebase
                                      (identity read from the forge, gate history
                                      vetoing)
feedback_health(tool, deterministic)  anti-façade check that the comment/gate landed
arm_automerge  (tool, deterministic)  opt-in; arm auto-merge, or merge the audited sha
done
```

A short or malformed commit report cannot certify an alignment: `commit_probe`
reads Git with repository-redirection variables removed and replace refs and
grafts switched off. It takes the candidate from the workspace HEAD, accepts it
only as ONE commit whose only parent is the audited revision — a commit someone
else pushed meanwhile, rebased or merged under the alignment, would otherwise
ride on a verdict that never saw it — that the branch holds in the repository
`pr_url` names (`git ls-remote` from a scratch repository, system and global
config cut off, the forge token in git's environment: the workspace's remote
and the url rewrites are the agents' to write; only a local run with no
`pr_url`, which publishes nothing, reads the workspace's `origin`); a report
that names a commit id must name that same commit, and one that names none
("pushed", "N/A") is set aside. The published verdict and auto-merge pin use
that full id. The commit agent is told to make exactly one commit, and pushes
with the token in git's environment (the skill's recipe), never in a URL or on
a command line. A failed proof counts as no commit,
preserving the existing lost-alignment decision. A refusal can
disarm or dequeue only while the PR head still matches this run's revision;
a stale run leaves a newer revision's commitment in place. Forge takedowns
have no atomic head precondition, so a head changing after this last read is
still a race the forge API cannot close here.

Code is committed **only** when the audit verdict is `safe` **and** the
deterministic verify gate is green; a not-safe bump or a red build
short-circuits to a hold comment. The eight verdicts are `hold_security`,
`needs_decision`, `hold_unstable`, `hold_lost_alignment`,
`hold_delivery_permission`, `committed`, `clean`, `carried` — the last three
green. The last hold is also typed on
the run (`FORGE_PERMISSION_DENIED`, the named `delivery_permission_denied` fail
node) once the comment, the gate and `arm_automerge` have run. It needs an
ANSWERED refusal — the preflight's verdict, or the forge API's scope list — and
its cure is an operator's, the probe's reason saying which: a permission the App
does not hold — approve the delivery permissions, refresh its token, relaunch; a
classic token short of a scope — reissue it as the reason names, relaunch.

A question **nobody answered** is a different refusal and reaches none of those
eight: any shape of no-response — a refused or dropped connection, a timeout, a
status the endpoint never answers verdicts with, a body short, over-long,
unparseable or missing the verdict fields — is asked once more after a pause and
then ends the run `FORGE_PREFLIGHT_UNAVAILABLE` (the `delivery_preflight_unavailable`
fail node), with **no comment and no verdict published** and `arm_automerge`
ASKED to take down an arming an earlier pass placed (off by default, where the
forge is deliberately not touched and an operator's own arming survives; the
node's own verdict value there is `preflight_unavailable`). A service that did
not answer proves nothing about the credential, so no blocking verdict about one
is published, and the cure is the one the run's reason names: a relaunch once
the service answers, or the configuration gap it names (no delivery-capable
credential mounted, a server with no preflight, a pull request URL that names
another repository or no pull request at all).

What the pull request DOES carry on that path is iterion closing its own
merge-gate claim: the launch posts a pending status in the repo's `gate_context`
([pkg/server/forge_gate_pending.go](../../pkg/server/forge_gate_pending.go)) and
no verdict replaces it, so the reconciler answers it with a synthetic `failure`
quoting this run's reason and its cure
([pkg/server/forge_gate_reconcile.go](../../pkg/server/forge_gate_reconcile.go)).
An operator reads a red `<gate_context>` saying `review died (…)` — iterion's
own lane closing a claim it opened, never a statement about the dependency PR
author's credential.

## Gate continuity — a rebase keeps its gate, with no LLM call

A synchronize launch (the integration's `review_on_sync`) first runs
`continuity`, which reads and never writes. It computes the head's **change
identity** — contract v1: the `git patch-id --verbatim` of
`merge-base(base_ref, head)..head`, every diff knob pinned on the command line
(`--no-renames --full-index --unified=3 --diff-algorithm=myers -O/dev/null`, no
external diff or textconv, system and global config cut off), folded with the
modes, status and path of every changed entry (`git diff --raw`, blob ids left
out) and the `base_ref` name. patch-id alone hashes no path for an entry with
no `---`/`+++` lines — a mode change, an empty file, a binary — that follows a
binary one, so two different changes would share an id without the listing.
Only the commit objects decide: replace refs and grafts are
switched off, and attributes are read from the empty tree, so neither a
`.gitattributes` in the change nor the repository the id is computed in moves
it. A rebase that leaves the hunks and their three context lines alone keeps
it; a conflict always touches one of them, so it changes it. `--stable` is not
used: it strips whitespace and reads a re-indented YAML block as the same
change. A git older than 2.39 (no `--verbatim`), no merge-base, or a degraded
`prepare` has no identity, and the run takes the full audit. On git older than
2.40 the empty-tree attribute source is not honoured, so a repository whose
`.gitattributes` changes diff output gets ids that do not match across
repositories, and carries nothing (the full audit, never a wrong carry).

`continuity` computes the head's identity first, before any agent runs and on
every answer — the `/vetty` opt-out included — and `post_feedback` stamps that
identity on a verdict pinned to the head prepare read. The alignment a run
pushes, and a head a repin moves to, are identified from the **forge** instead
(below): the agents ran in the workspace with a shell and the forge token, and
its refs, replace objects, grafts, config and object store are theirs to
rewrite.

The fact travels **with the gate**, never from run artifacts or "the latest
comment". `post_feedback` ends the status note with ` · pid1:<id>` (within
GitHub's 140 characters on every verdict) and writes the review's **last line**
as `<!-- vetty-gate/v1 sha=… id=… verdict=… origin=… origin_at=… -->`; the
opener is defused wherever an agent-written section carries it. `continuity`
reads the pull request, its reviews with their edit record (GraphQL), the
forge's status for the gate context on every sha a marker names **and on the
head**, and who wrote each, and decides by predicates over sets (one shared
block, the one `post_feedback` decides with):

- a review this App published and somebody edited since — anyone with write
  access can edit it, and its author stays the App — → full audit, whatever it
  says now: its marker deleted, its `origin_at` rejuvenated, a marker
  appended. Edited reviews of other authors veto nothing by themselves;
- a status stamped with this change that another identity wrote (another App,
  a PAT user), or whose author cannot be read → full audit: only this App's
  stamp is a source;
- **any** verdict on the same change that is not green, any status stamped with
  it that is not green, or a red status on the head itself (stamped or not — a
  carry never paints over one) → full audit (unanimity veto — it covers the hold
  whose gate was refused for a moved head, and, on the head itself, the hold
  whose review failed to post while its status landed; such a marker-less hold
  on ANOTHER head of the change is not found — that would take the pull
  request's force-push timeline);
- this head carries an eligible green whose ORIGINAL audit is younger than
  `carry_max_age_h` → `already_gated`: `continuity_standby` hands
  `arm_automerge` the head and the verdict on record for it, the arming is
  renewed pinned to it, and nothing is published;
- another head carries an eligible green whose ORIGINAL audit is younger than
  `carry_max_age_h`, and the base requires a check other than the gate on this
  head (the check rollup's `isRequired`, in any state) → the verdict
  `carried`, published on this head with the origin travelling (a chain of
  carries never extends the age). A base that requires nothing but the gate
  would merge a carried head on the gate alone, with no build: the full audit
  builds it instead;
- anything else — a forge head that is no longer the one `prepare` read
  included: nothing says that head has a delivery of its own — → the full
  audit.

Eligible means an unedited review this App published names the sha and the
id, and the forge's one status for the context on that sha — written by this
App — is green, ends with the same stamp and links that review. This App is
the identity the run token speaks as, told by **user id alone** (REST and
GraphQL spell a bot's login differently, `slug[bot]` / `slug`, and a user
account can register any spelling): a review's author id against the GraphQL
viewer's, a status's creator id — read from the commit's status list, since
the combined status names no creator — against the same. A token whose viewer
has no id owns nothing, and nothing is carried. Authorship decides
eligibility, never a marker's veto: a marker vetoes whoever wrote it, so
Vetty's own holds published by a user (a PAT connection) are read, and an
outsider's marker can only force the full audit. Where the forge token is not
the publishing App (a PAT beside an App connection) no review or stamped
status is this App's, and nothing is carried: the full audit.
GitHub-only; a history not
read whole, or any read that fails, is the full audit, said so in the reason.
Neither the carry nor the standby path holds a node that can call a model, by
the engine's own `ir.NodeUsesLLM`.

`carried` certifies the supply-chain audit and alignment of an **identical
change**, not a build of this tree: its note says *this base is built by the
repo's CI*, and the build on the new base is owed by a check the repository
REQUIRES other than the gate — which is why a carry, a repin and an arming of
a carried head all need one on that head. What it does not cover: a semantic break
between the change and the new base that CI does not exercise, and an advisory
published after the original audit (bounded by `carry_max_age_h`). Patch-ids
ignore line numbers — that is what lets a rebase pass — so a hunk re-applied
elsewhere with the same three context lines is the same change.

`post_feedback` also **repins**: when a green audit finishes after a rebase
moved the head, the new head's identity is computed in a scratch repository the
node creates and fills from the forge alone — an explicit URL, system and
global config cut off, the base branch as the forge holds it now, the token in
git's environment — and, when it equals the audited change's and the new head's
own gate history vetoes nothing (the same predicate, one shared block), the
verdict is published there as `carried`, its origin the commit actually
audited. A hold is never moved, and any other new head is published to as
before — and refused by the server for the moved head; the run's summary says
why it did not repin. A carry `continuity` decided is held to the same rule at
publish time: the gate history is read again right before the publish — the
one `continuity` read can be minutes old, hours on a resumed run — and a hold
published since on any head of the change, an edit, a foreign stamp, an origin
past `carry_max_age_h`, or a base that no longer requires a check other than
the gate **withdraws** the carry: nothing is published, the run's gate claim
stays unanswered, and the reason says why. The server orders decisions by the
time it receives them, so a hold published after that read still supersedes
the carry, and only a hold landing in the second between the read and the
publish is overwritten — a window only a compare-and-set on the publish
endpoint would close. After a
repin, `arm_automerge` vouches the gate against the head the verdict was
carried onto and arms it. The repin obeys the same switches as `continuity`:
`continuity` off, `carry_max_age_h` at 0, or a carried verdict whose original
audit is past `carry_max_age_h` publishes as before, and the summary says which.

The `/vetty` command pins `continuity: "false"`, so a human gesture buys a full
audit — unless an operator pins `continuity` in the integration's
`launch_vars`: launch_vars win over the command's own, on both lanes and on
the repin, so a pin of `true` makes `/vetty` reuse a verdict too. Pin it
`false` to turn every reuse off; leave it unpinned to keep `/vetty`'s full
audit.

**Auto-merge on UNSTABLE.** GitHub refuses to arm a pull request whose required
checks all pass while another check fails or still runs (`UNSTABLE`), as it
refuses `CLEAN`. A refused arming is classified by re-reading the pull request:
`CLEAN` merges pinned as before; `UNSTABLE` reads the check rollup with each
context's `isRequired` — a required context not passing, a base that requires
no check at all (UNSTABLE then says nothing about the build; a required gate
context counts), or a rollup not read whole, merges nothing; otherwise
`automerge_on_unstable` decides (`merge`: pinned now, what an earlier arming
would have done; `wait`: only once the forge reports `CLEAN` within
`automerge_settle_s`). The policy governs that refusal only: an arming GitHub
accepts — a required check still running when the run arms — merges once the
REQUIRED checks pass, whatever a check the repo does not require says. Every
green verdict follows the same rule, so CI timing never decides how a verdict
is merged — and `carried` needs one more thing first: a check the base
requires other than the gate on the vouched head, in any state (an arming and
a queue wait for it to pass; CLEAN and UNSTABLE mean it passed). On a base that
requires only the gate a carried head is neither armed nor merged, at any
door, and an arming an earlier pass of this run's identity left on it is taken
down. `clean` and `committed` need no such check: their gate is Vetty's own
build of the commit it pins. A repository that does not allow auto-merge
gets no merge in place of the refused arming. Every head change re-arms
through the carry and standby paths at no LLM cost.

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
| `respect_human_arming` | bool | `true` | Preserve another actor's arming, on a refusal and on a replacement |
| `automerge_on_unstable` | string (enum: `merge`, `wait`) | `"merge"` | When GitHub refuses to arm an UNSTABLE pull request whose required checks all pass: merge the audited head now, or wait for CLEAN |
| `automerge_settle_s` | int | `600` | The bound on the `wait` policy, in seconds |
| `continuity` | bool | `true` | Reuse a green verdict on a head whose change it already covers (carry, renewal, repin); the `/vetty` command pins it false, and an operator pin in `launch_vars` wins over that on both lanes |
| `carry_max_age_h` | int | `72` | Age past which a green verdict is no longer reused, measured from the original audit; `0` turns every reuse off |
| `forge_publish_url` | string | `""` | Deterministic forge-publish grant, injected by the iterion server at launch; empty → direct comment, no gate |
| `forge_publish_token` | string | `""` | Token for the publish endpoint above (server-injected) |
| `forge_delivery_preflight_url` | string | `""` | Read-only delivery-permission proof endpoint (server-injected on the same launch); empty → a workflow-touching alignment is refused rather than pushed |
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
  `disambiguator: when_args_empty`) re-runs the guard, with
  `context_vars: {continuity: "false"}`: always a full audit.

The forge lane's `[opened, reopened]` is recorded, not enforced
(`pkg/webhooks/types.go`): a push reaches Vetty through the integration's
`review_on_sync`, which is what lets `continuity` carry the gate across a
rebase. Enforcing that list as written would silence the synchronize lane.

The manifest's `forge:` block drives the studio Integrations auto-provisioner:
webhook events `pull_request` + `pull_request_comment`; token scopes
`pull_requests: write`, `repository: write`, `statuses: write`; secret
`forge_token`; an author allowlist of `dependabot[bot]` / `renovate[bot]` (plus
suffix wildcards for self-hosted Apps) with `author_scope: exclusive`, so a
general reviewer co-enabled on the repo does not also review the PRs Vetty owns.

Run history and validation status: [`docs/bot-runs/dep-update-guard.md`](../../docs/bot-runs/dep-update-guard.md).
