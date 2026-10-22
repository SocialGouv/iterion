# Auto-maintenance — wiring a repository so its dependencies update themselves

The goal: a repository receives its dependency updates, aligns its own code
with the breaking changes, proves the result through its own CI, and merges —
with no human in the loop.

That is not one setting. It is a handful of settings in three systems that must
agree: the repository's **ruleset**, the **iterion integration**, and the
**GitHub App installations**. Nothing type-checks the agreement between them,
which is exactly how this project lost a week — a gate context renamed on the
ruleset and left stale on the integration, with no repository able to see the
disagreement.

So the recipe below ends where it must: at a preflight that runs the agreements
and reddens.

```
scripts/auto-maintenance-check.sh <owner>/<repo>
```

---

## The order, and why it is the order

Each step is safe only once the one before it holds. Applying them out of order
produces a repository that looks wired and merges nothing — or worse, one that
merges without proof.

### 1. The repository proves itself first

Before any automation, the repository needs **one command that decides whether
it is healthy**, and a CI job that runs it. The loop's entire premise is that
a machine can tell a good alignment from a bad one; without that command there
is nothing for it to read.

Put every invocation in a `Taskfile.yml` — once. A command that lives in a
script, in the docs and in somebody's memory diverges, and the loop then aligns
against a definition nobody else uses.

```yaml
# Taskfile.yml — the repository's own verdict, one definition
tasks:
  verify:
    desc: Everything that must be true before a change may merge
    cmds:
      - task: lint
      - task: test
      - task: build
```

Wire the CI job to `task verify`, not to its parts: a job that runs two of the
three members of an aggregate proves nothing about the third.

### 2. Renovate opens the PRs

Four gestures, in this order:

**2a. Install the App on the repository.** The org's `socialgouv-renovate` App
is `repository_selection: selected` — a repository not explicitly added never
receives a PR, and every other setting below would still look correct. This is
agreement 6 of the preflight.

**2b. Create the two secrets the workflow consumes.** `RENOVATE_APP_ID` and
`RENOVATE_APP_PRIVATE_KEY`. The workflow mints a token from them; without them
it cannot authenticate, and the run fails at a step that says nothing about
dependencies.

**2c. Copy the workflow.** Self-hosted, on a schedule — not the hosted Mend
service, which stopped producing PRs across the whole org in late 2025 without
anyone noticing.

```yaml
# .github/workflows/renovate.yml (abridged — see buildkit-operator for the full file)
on:
  schedule: [{ cron: "0 3 * * 1" }]
  workflow_dispatch:
permissions:
  contents: write
  pull-requests: write
jobs:
  renovate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/create-github-app-token@<sha> # v3.2.0
        id: app-token
        with:
          client-id: ${{ secrets.RENOVATE_APP_ID }}
          private-key: ${{ secrets.RENOVATE_APP_PRIVATE_KEY }}
      - uses: renovatebot/github-action@<sha> # v46.1.21
        with:
          token: ${{ steps.app-token.outputs.token }}
```

Two traps paid here:

- **Authenticate as the App, never as `GITHUB_TOKEN`.** A PR authored by
  `GITHUB_TOKEN` does **not** trigger other workflows, so its CI never starts —
  and a dependency PR whose CI never starts is the one thing this loop cannot
  work with. Under the App the author becomes `socialgouv-renovate[bot]`, CI
  fires normally, and that identity is what lets everything downstream tell a
  dependency PR from a human one.
- **`client-id`, not `app-id`.** The action reads
  `getInput("client-id") || getInput("app-id")` and passes the value through;
  GitHub accepts either the numeric App ID or the Client ID as the JWT issuer.
  The secret keeps its name; only the input key changes, because `app-id` is
  deprecated and emits two warnings per run.

**2d. Write the config.** Renovate reads the first of `renovate.json`,
`renovate.json5`, `.github/renovate.json`, `.github/renovate.json5`,
`.renovaterc*`. Both pilots use `.github/renovate.json5`.

What the config asks for decides what the App must be allowed to do — that is
agreement 4, and the preflight derives it rather than checking a fixed list:

| config setting | permission it demands |
|---|---|
| `helpers:pinGitHubActionDigests` | `workflows: write` — it rewrites `.github/workflows/**`, and GitHub refuses an App push there without it |
| `dependencyDashboard` | `issues: write` — the dashboard is an issue |
| `vulnerabilityAlerts` | `vulnerability_alerts: read` |

### 3. The ruleset requires the CI checks — and only those, for now

Require what already runs. **Do not** require the review context yet: a
required check that nothing produces blocks every PR on the repository,
forever. That is agreement 7, and it is the shape of the friction that started
this work.

Inventory the **bypass actors** while you are there. They are not a defect —
they are the admin escape hatch, kept on purpose. What is a defect is not
knowing them, because a merge by a bypass actor did **not** pass the gate, and
nothing downstream should read it as a conforming merge.

### 4. The iterion integration, auto-merge OFF

```
iterion remote api PATCH /api/teams/<team-id>/forge/repo-bots/<integration-id> \
  --input - <<'JSON'
{ "bot_ids": ["dep-update-guard", "review-pr"],
  "launch_vars": { "gate_context": "revi/review", "arm_automerge": "false" } }
JSON
```

Three rules, each paid for:

- **Send the WHOLE `launch_vars` map.** A non-empty partial one is REFUSED
  (400, naming the keys to echo — see [merge-gate.md](merge-gate.md#switch-gate));
  `bot_ids` may be omitted when the bot set does not change.
- **Pin `gate_context` to one shared name.** A required check applies to
  *every* PR, so on a repository where a reviewer takes the human PRs and Vetty
  takes the bot's, both must post the **same** context — otherwise whichever
  bot did not run leaves the check permanently absent. Vetty's own default is
  `vetty/deps` and Revi's is `revi/review`; the override is what makes them
  agree. Never *remove* it.
- **`arm_automerge: "false"` until the end.** Steps 5 to 7 exist to earn it.

The same PATCH carries the repo's routing block when the pinning recipe
below it needs one (`"routing": {...}` — ADR-129):
[cloud-llm-credentials.md § The binding level](cloud-llm-credentials.md#the-binding-level--a-repo-pins-its-own-slots)
has the vocabulary, the write-role table and the two rules that bite
(the `*`-cell warning at save time; absent keeps / null clears / an
object replaces). On an org that requires provisioning approval, a
spoken routing block parks the PATCH like any other expansion.

Of the block's fields, `providers:` can stop this loop. It binds the webhook
deliveries the integration makes, and every scheduled fire whose row carries
no routing block of its own — the integration's block is PROJECTED onto the
scheduled rows (`RoutingFromIntegration`), and a row an operator tuned
through the schedule PATCH keeps its own and is not narrowed by this write.
Where it binds, a primary route resolving outside the list with no allowed
step left to serve the node REFUSES the launch, naming the node, the provider
and the level that set the list, instead of re-routing. So a repository
narrowed to one gateway loses the deliveries of any co-enabled bot whose
route resolves ONLY to providers the list omits, and whatever gate those runs
produced goes with them. A non-empty list also refuses a launch whose routing
fields it cannot READ (ADR-129 §5), and this block's own `overrides` cells do
not repair every such slot: not a node the walk records as fieldless
(`fieldlessSite` — a `human` node's llm half, a subbot, an agent recovery),
whose refusal says to replace the node or launch without the list, and not
`review-pr`'s two `${ITERION_VIBE_PROVIDER_GPT:-}` slots (ADR-129 §7). The
recipe above carries both — `dep-update-guard`'s `escalate` is
`interaction: llm_or_human`. So screen EVERY bot the integration enables, not
the one you are wiring: a refused launch names only ONE unreadable route, the
head of a list sorted by site then field (`FirstUnreadable`) — a SAMPLE, not
the inventory: on `dep-update-guard` under `providers: [anthropic]`, with none
of its dials answered, it names `align`, while `escalate` sits further down
the same list.

Then **re-read** the integration and the webhook config. A write you did not
read back is a write you are hoping about.

### 5. Watch one verdict land green

On a real PR. Not a test repository, not a replay — the point is to see the
context appear on a head SHA and turn green.

### 6. Now make the review context required

Add `revi/review` (or whatever `gate_context` names) to the ruleset. It is safe
now, and only now: step 5 proved something produces it.

Pin the **producing App** on the required check while you are there. A required
check pinned by name alone is satisfied by *any* credential able to post a
commit status on the repository — including one that is neither Revi nor Vetty.
The loop's whole premise is that the gate proves an adversarial reviewer saw
the change, and a name does not prove that. This is agreement 2.

### 7. Watch it BLOCK a fresh revision

Push a new commit to an open PR and confirm the gate goes back to pending and
holds the PR. A gate that only ever goes green has not been observed to gate.

### 8. Arm auto-merge — last

```
"launch_vars": { "gate_context": "revi/review", "arm_automerge": "true" }
```

Two settled arbitrations govern this step:

- **A PR that merges unattended must have had its gates run on ITS head.** A
  human-reviewed PR can lean on continuous proof; an unwatched merge cannot.
- **A configuration that cannot converge refuses AT LAUNCH**, and the status
  carries the reason. Failing at 3 a.m. with a green-looking board is the
  outcome this rule exists to prevent.

Which is why the preflight's agreement 7 sharpens its wording once
`arm_automerge` is on: a required context that nothing produces stops being
"this PR blocks" and becomes "this configuration cannot converge".

---

## When the alignment touches a workflow file

Agreement 5 covers a real and narrow hole: Renovate **must** hold
`workflows:write` (`helpers:pinGitHubActionDigests` rewrites
`.github/workflows/**`), while the App that delivers Vetty's alignment need
not — and GitHub refuses outright an App push touching that path without it.
So Renovate can open an Actions bump whose alignment Vetty cannot push.

Vetty asks before pushing. `delivery_probe` sends the digest of the token
actually mounted in the run to the server's read-only
`POST /api/v1/forge/delivery-preflight`, which answers against the permission
proof recorded when that token was minted — never the installation's advertised
grant. Only when the staged alignment touches `.github/workflows/**`: the
probe measures what this run will push — `HEAD..worktree`, the index included,
plus what `HEAD` adds on top of `origin/<branch>` — so the ordinary digest-pin
bump (already committed and pushed by Renovate's own App, needing no alignment)
is never held.

Two credential kinds can be PROVED, and only those two: an App installation
token (`ghs_`), against the permission proof recorded when it was minted, and a
classic PAT or OAuth token (`ghp_`/`gho_`), against the scope list the forge API
stamps on its own answer. A fine-grained PAT, an external App token or a
user-to-server token is DELIVERED unverified — the App workflows restriction
this preflight proves does not apply to it — so for those a refusal still
arrives at push, where the unmoved head reads as `hold_lost_alignment`.

An ANSWERED refusal — the preflight's verdict, or the forge API's scope list
for a classic token — is the verdict `hold_delivery_permission`: a red comment
naming the permission, a red gate, no push attempted, and `arm_automerge` asked
to take down a commitment an earlier pass placed (off by default, where the
forge is deliberately not touched and an operator's own arming survives). The
run then ends `FORGE_PERMISSION_DENIED`, quoting what `arm_automerge` actually
reported. The cure is an operator's, and the reason the comment carries says
which: a permission the App does not hold — approve the delivery permissions,
refresh its managed token and re-run Vetty on the PR; a classic token short of
a scope — reissue it as the reason names and re-run Vetty. Either way the
alignment can be applied by hand instead.

A question **nobody answered** never becomes a verdict. Every shape of
no-response — a refused or dropped connection, a timeout, a status the endpoint
does not answer verdicts with, a body short, over-long, unparseable or missing
the verdict fields — is one outcome: asked once more after a pause, then the run
ends `FORGE_PREFLIGHT_UNAVAILABLE` with no comment and no verdict published,
`arm_automerge` ASKED to take a previous pass's arming down (off by default,
where the forge is deliberately not touched and an operator's own arming
survives), and the cure in the reason: a relaunch where the service went
silent, the configuration gap where it names one — no delivery-capable
credential mounted, a server with no preflight service, a pull request URL that
names another repository or no pull request at all. That asymmetry is the
point: a blocking check on a
third party's dependency PR is a statement about THEIR credential, and an
outage of ours is no evidence for it.

What the pull request does carry on that path is iterion closing its OWN
merge-gate claim, not a verdict: the launch posts a pending status in the
repo's `gate_context`, nothing replaces it, and the reconciler answers with a
synthetic `failure` quoting the run's reason and its cure
([pkg/server/forge_gate_reconcile.go](../pkg/server/forge_gate_reconcile.go)).
An operator sees a red `<gate_context>` reading `review died (…)`; the lane
that opened the claim is the lane that closed it.
The probe is built as the complement of a blacklist for the same reason — three
rounds of hardening each found one more failure shape of the HTTP library that
an enumeration had missed. Granting `workflows:write` is **connection-wide** — every
bot minting a runtime token on that connection carries it, and an actor that
can rewrite CI runs arbitrary code in it — so it stays a deliberate,
per-connection decision (#1595).

---

## The preflight

```
scripts/auto-maintenance-check.sh <owner>/<repo> [--json]
```

It reads; it never writes. Seven agreements:

| # | agreement | broken means |
|---|---|---|
| 1 | every context a bot posts is a context the ruleset requires | the verdict is advisory — nobody waits for it |
| 2 | the required check pins its producing App | any credential that can post a status satisfies the gate |
| 3 | the installed dependency App's login matches Vetty's `author_allowlist` | Vetty never sees the PRs it exists to guard |
| 4 | the Renovate App is live and holds what its config demands | Renovate's push is refused for a reason unrelated to the bump |
| 5 | the delivery App is live, reaches this repository, can touch what Renovate may bump, and the workflow's secrets exist | the alignment cannot be delivered — a workflow-touching one is held by name (see below), the rest is refused at push |
| 6 | the repository is inside the Renovate installation | no dependency PR is ever opened here |
| 7 | every required context has actually been produced | the PR blocks forever — or, armed, cannot converge |

Four things it does **not** do the obvious way, each because the obvious way
was measured wrong:

- **Only rulesets that gate the default branch count.** Dependency PRs target
  the default branch, so a check required by a `refs/heads/release/*` ruleset is
  a verdict nobody waits for where it matters — the same defect as a renamed
  context, reached through the condition instead of the name.
- **Presets are expanded, and the expansion is bounded by a blind spot.**
  `config:recommended` extends `:dependencyDashboard`, `config:best-practices`
  extends `helpers:pinGitHubActionDigests`; a config whose whole body is one
  preset demands two permissions while containing neither literal. Because
  enumerating spellings never converges, an `extends` the script cannot expand
  becomes a blind spot — but only when the App is *missing* one of the tracked
  permissions. Hold all three and no unknown preset can create a disagreement,
  so there is nothing to be blind about.
- **A suspended installation is a disagreement, not a permission question.** It
  holds every permission it ever held and uses none of them.
- **Bypass actors are printed as inventory, not scored as an agreement.** They
  are the admin escape hatch, kept on purpose, so neither outcome can redden —
  and a line that cannot fail should not be counted among lines that can. What
  the reader needs is the list, so that a merge by one of them is never
  mistaken for a conforming merge.

**Exit codes.** `0` all agree · `1` at least one measured disagreement · `2` a
**blind spot** — a tool missing, an API that did not answer, a truncated page,
an unparseable payload, or a read this credential cannot make.

Blind outranks broken, deliberately. With a blind spot open the run cannot
claim its list of disagreements is the whole list, and a comparison that fails
open is worse than no comparison: it restores confidence over real work.

The blind spot you will meet in normal use is the installation coverage, and
it applies to **both** Apps — SocialGouv installs `socialgouv-renovate` *and*
`iterion-forge-core` with `repository_selection: selected`. Listing a selected
installation's repositories needs a classic PAT (`read:user`); pass it as
`AUTO_MAINT_PAT` and both become decidable. Without it the script refuses to
assume either App reaches the repository.

`--json` writes the document to stdout **alone** — the human report moves to
stderr — so `… --json | jq` works.

**Acceptance.** The witness lives in
[internal/automaintguard](../internal/automaintguard/preflight_test.go): every
scenario is a configuration defect the script must redden on — including the
two this project actually paid, the gate-context rename applied to one of two
sites and the delivery App that cannot touch the files Renovate bumps — plus
one conforming configuration it must pass. A bench made only of defects cannot
be told apart from a script that always fails.

It guards the *inputs* as well as the predicates, which is the harder half: a
ruleset scoped to a release branch, an `evaluate`-mode ruleset, a preset-only
config, a config GitHub withheld for size, two installations of the same scope,
a suspended App, `arm_automerge: "yes"`, the inline spelling of the manifest's
allowlist, and a working directory holding files that look like glob matches
for it. Every one of those was a full green before its fix.
