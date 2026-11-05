# ADR-162 — The fork review lane: a maintainer's gesture admits one commit, on untrusted-workspace floors that land first

- **Status**: Proposed — design only, nothing below is built. The operator chose to re-implement from main with the dormant branch as the spec (debt-leap #113 = B, 2026-11-05); Q1–Q4 are his to decide.
- **Date**: 2026-11-05
- **Deciders**: jo (#113), Claude (analysis, design)
- **Serves**: [#874](https://github.com/SocialGouv/iterion/issues/874) (the admission half), [#1730](https://github.com/SocialGouv/iterion/issues/1730) (the guarantee's remainder). Spec, read-only: `fleet/c3-fork-lane-2@201d6ed4e` (62 commits on `849d78703`, v3.190, 24/09) and `leap/kimi82@31d80e119` (2 commits on it: #1730 R2's reads, R4's unit row, R6–R8)
- **Relates to**: [ADR-033](033-launch-gate-fails-open-on-metering-error.md) · [ADR-059](059-skill-library.md) (tier precedence) · [ADR-129](129-per-integration-typed-routing-overrides.md) §4 · [ADR-132](132-a-run-reads-no-environment.md) · [ADR-154](154-the-sovereign-pool-boundary-is-a-namespace-and-an-enforced-proxy.md) · [ADR-161](161-one-resolver-decides-the-credential-a-call-spends.md) (admission's credential reach)

## Context

PR #1670 (squash `6898e6c78`) landed the enforcement half of the fork review lane. `store.RunTrust` rides the run document (filesystem and Mongo), the queue message, resume, retry and fork; an untrusted run gets no publish grant (`injectForgePublishVars`, `runOwnsGrant`), no `forge_token` by either carrier, no commit status; the runner refuses a fetch that misses `RepoSHAExpected`; `launchWebhookTarget` keeps lane kinds disjoint and refuses an untrusted target without a pin; a board-mode route refuses provenance. `webhooks.Config.ForkLane` exists and nothing can set it; `orgusage.ForkAuthorSubject` has no caller. Nothing sets `RunTrustFork`.

**Main has grown trust readers since.** `admitRun` refuses an untrusted run without a per-run sandbox (#1773); `admitSandboxCredentials` refuses one no credential reaches (#1768, moving into ADR-161's resolver); the skill channel serves only owned skills — claw through `ExecuteSkillPinned`, claude_code through `--add-dir` (#1719); the connector resolution answers reads only and skips the workspace catalog tier for a run the tenant does not control (`pkg/connection/trust.go`, #1802). Forge clients stay in `pkg/forge`: a PR's client resolves through the team forge connection (`forgeConnectionForPR`) before the `forge_token` binding. The untrusted-input class freezes 203 acting prompts, `review-pr` among them (#1639's posture). And today no deployment can run a fork run: `runview.Launch` refuses an untrusted launch on the in-process path (no publisher persists the marker), and the chart ships `ITERION_SANDBOX_OVERRIDE=none` to every runner unless `runner.sandbox.enabled` (off by default), which #1773 refuses.

**The branch is the floors, not the gesture.** It holds 25 untrusted-workspace floors behind a bench (`e2e/untrusted_*_test.go`: one hostile checkout through a real `Engine.Run` and real `claude`/`pi`/`opencode` spawns) and #1730's lesson: a guarantee established once and re-derived downstream from a mutable location fails, so it becomes a handle (`os.Root`) and an in-memory value whose zero value refuses. The gesture itself was never written. Every file the branch modifies still exists on main, but main rewrote them (`claw_backend.go` +2104 lines, `runtime/sandbox.go` +1402, `claude_code.go` +1085, `runner/loop.go` +973): 124 of 245 files conflict, so its diffs are a spec, not patches. One record is wrong: #1639 was closed on 28/10 as fixed by #1670, but the trust refusal in `stampHandoffs` exists only on the branch.

## Decision

1. **The opt-in is a gesture on a fork-lane config, never a record.** `ForkLane` is settable at creation (`POST /api/teams/{id}/webhooks`) and immutable: PATCH refuses a change, which would re-read parked debounce targets as the other kind. The gesture is a `/command` on a fork pull request, delivered to that config and routed by its own table (default `/revi` → `review-pr`). A GitLab fork-lane config is refused at creation, typed, naming `refs/merge-requests/<iid>/head` as the unwired seam. The ordinary GitLab MR lane stops stamping "trusted" on a head neither project names: it fails closed like `forkGuardRefusal`.
2. **Who opts in.** A commenter whose live forge permission on the base repository reaches the route's `min_replier_role`, never below `developer` (write). The self-comment loop guard stays; the `AuthorizedRepliers` login short-circuit is not consulted on this lane, because a login is renameable.
3. **How the commit is pinned.** The handler resolves the pull request through the forge API and admits only a PROVEN fork: open, head repository named and not the base. It launches on the base's `CloneURL` and `refs/pull/<n>/head` with `launchProvenance{Trust: RunTrustFork, ExpectedSHA: HeadSHA}`. An optional SHA argument narrows the gesture to that commit, or the launch is refused. A push between the gesture and the fetch is refused by the runner, naming both commits; a new head needs a new gesture.
4. **The base repository's configuration may stop a fork run, never steer or grant it.** `applyWebhookVarLayers` applies neither `LaunchVars` nor `OperatorLaunchVars` to an untrusted target; `trustedVarNames` stays empty; the hold label applies per Q2. Model routing resolves from the database cascade, because it spends the tenant's money, not the repository's. Gate relaunch and auto-fix stay refused on a fork-lane config.
5. **The bound.** `gateMonthlyCaps` meters a fork target twice: on the org's budget (ADR-033, fail-open) and on `ForkAuthorSubject(org, provider, the PR author's numeric forge id)`, fail-CLOSED, because it is an abuse bound like the issue lane's author gate. `forge.PullRef` gains that numeric id; today it carries only the login. Its values follow Q4. The org purge matches the escaped key spelling.
6. **What the run may read and write.** It reads the base clone at the pinned commit (the base is its only remote), its sealed LLM credential (never a host login), connector read operations and the bot's own skills through the pinned channel. It writes its sandboxed workspace and its own events and artifacts. Nothing reaches the forge, the board, the host capabilities, the tenant's memory (either way) or a hand-off seed (either way). Nothing the checkout declares is executed or loaded: its hooks, `.mcp.json`, `devbox.json`, `devcontainer.json`, `.env`, slash commands and context files. Plugin MCP servers are not booted either, because they would start inside the checkout.
7. **Where the guarantee is enforced.** It is enforced in three layers, and every predicate is `Trusted()`, so an unknown trust loses. The server enforces disjointness, the pin and the bound at `launchWebhookTarget`, plus #1670's grant and secret refusals. Runtime admission enforces the sandbox and the credential's reach at `admitRun`. The floors read one value resolved per run from `runtime.WithTrust` and handed down: `delegate.Task`/`IOTask` carry it across the sandbox wire, and a child run (store stamp and wire) and a node's processes (`ITERION_RUN_TRUST`, cooperative) inherit it. It is never re-derived from the workspace. Every read and write under `<workDir>/.claude` goes through the workspace handle.
8. **Backend parity: every floor on both, or a typed refusal.**
   - Project settings: claude_code drops `project` and `local` from `--setting-sources` at the one builder every spawn takes. claw registers only iterion's own hook groups, from memory, and claude_code gets the same groups through `--settings`.
   - Skills: #1719, on both.
   - Slash commands: claw stops resolving the checkout's. claude_code gets them only through the floored project source.
   - MCP: neither backend autoloads `.mcp.json` or boots a plugin server. The bot's own stdio servers start from `/`, and strict mode is forced.
   - Memory: claw's `memory:` block and auto-memory are both withdrawn.
   - Host capabilities: one resolver, checked when the tool is called.
   - Other backends: pi gets its three floors. codex, opencode, grok and kimi refuse with `ErrUntrustedWorkspaceRefused`, and the protocol's zero value refuses.
9. **Where it runs.** Only where the run's sandbox resolves active, so #1773 stays the refusal. The in-process launch persists `Trust` and `RepoSHAExpected` instead of refusing (one code path, single-process and multi-replica). In the cloud, Q3 decides. The output follows Q1.

## Branch → main

| Branch element | Main today | Unit |
|---|---|---|
| Trust on `delegate.Task`/`IOTask`, router task (`stampWorkspace`), sub-bot compile (`compileUnit(…, trust)`) | absent; the cloud child inherits through the wire copy, `runview` fork copies it | U1 |
| Child stamp in both stores, `ITERION_RUN_TRUST`, `.env` skipped by helpers and carriers | absent; ADR-132's `envtrust` keeps a planted value off the sandbox for trusted runs, which narrows this but does not close it | U1 |
| `stampHandoffs` keyed on trust, both directions (#1639) | absent | U1 |
| Workspace handle, zero value refuses; R1–R8 (`08c2379d2` + kimi82) | absent; main added two writers the handle must take (`owned_skills.go`, `skill_channel.go`) | U2 |
| claude_code setting floor + inline `--settings`; claw hooks from memory; `mergePluginHooks` writes nothing | absent; the empty-list `--setting-sources ""` has landed | U3 |
| pi context files, `--approve`, checkout skills, codex-bridge seed; codex/opencode/grok/kimi refusal | absent | U3 |
| Slash commands not resolved from the checkout | absent; the size ceiling has landed | U3 |
| claw `skill` tool refused; shadowed skill overwritten | **obsolete**: #1719's channel serves owned skills only, and #1730's "pin a skill-less reviewer" lifts | — |
| MCP: `.mcp.json` names-only, plugin servers withheld (`MCPPrepared`, `admitMCPCatalog`), stdio servers from `/`, strict forced | absent; #1945's launcher placement is the sandbox seam it plugs into | U4 |
| `bot://` sources refused; memory and auto-memory withdrawn; `workspace_trust_withdrawal` event | absent | U4 |
| Host capabilities: one resolver, call-time check, no board/runs listener; board items | absent; `hostmcp` records trust for the connector facade only | U4 |
| Connector project catalog floor | **obsolete**: #1802 at the resolution | — |
| devcontainer not read, `host_state: none`, no `.git` mount or git identity, `repo_devbox` off, network floor `allowlist` + `iterion-default` | absent (`resolveRepoDevbox` and `ResolveNetworkPolicy` take no trust) | U5 |
| `sandbox_skipped` marker, `sandbox_no_credential_warning` | **obsolete**: #1773 and #1768 refuse instead | — |
| The gesture, settable `ForkLane`, var layers, hold label, author bound, GitLab | never written | U6 |

## Migration

Each unit lands alone and ports its rows of the branch's bench, each row seen red on main with the floor removed and green with it. U1–U5 cannot change a live run's behaviour, because nothing sets `RunTrustFork` until U6. Order: U1, then U2–U5 in any order, then U6, then U7.

- **U1, the carrier.** Covers the trust on `Task`/`IOTask` and every task assembler, the compile seam, the child stamp in both stores (conformance row), the process environment and `.env`, the handoffs, and the in-process launch persisting the marker. Witnesses: the carrier, child-run and dotenv benches, `handoff_untrusted`.
- **U2, the handle (#1730).** Every reader and writer under `<workDir>/.claude` (mirror family, plugin hooks, owned skills, skill channel) holds it; `CopyFrom` is atomic and keeps the source mode. Witnesses: the `.iterion-managed` escape row, `CopyFrom` hardlink and concurrency, the pruner sidecar; the raw-writer text guard as a comfort.
- **U3, agent configuration.** Covers the claude_code and claw settings and hooks, slash commands, pi, and the typed refusals. Witnesses: the real-CLI setting-source probe, every spawn path, and a hook veto through a dangling link.
- **U4, MCP, sources, memory, host capabilities.** Witnesses: the MCP-channel bench, memory in both directions, and a host tool named in `command:` refused.
- **U5, the sandbox.** The host-state floor resolves before the credential admission, which then judges the floored mount (ADR-161's `SandboxHomeState`). Otherwise #1768 would admit a run whose only credential is a login that is no longer mounted. Witnesses: a devcontainer `postCreateCommand` canary, the host-state mounts, a file-login-only run refused, devbox, and egress to a host outside the preset.
- **U6, admission.** Covers decisions 1–5 and Q1/Q2/Q4: create-time `ForkLane` with PATCH and GitLab refusals, the GitLab MR fail-closed, the gesture, the var layers, the bound in both counter twins, and the delivery-row reasons. Witnesses: a real GitHub delivery replayed through the handler on a fake forge (fork admitted, same-repo refused, withheld head refused, login-allowlisted non-writer refused, stale SHA refused), a PATCH flip refused, the bound denying the N+1th run, and the metering error refusing.
- **U7, deployment and proof.** Covers Q3 in the chart and the publisher, plus `docs/webhooks.md`'s table rewritten to what ships. Proof: one live `review-pr` run on a lab repository's fork PR, through a real delivery and sandboxed; its cost goes in the commit.

## Open operator decisions

- **Q1 — what the maintainer sees.** (A, recommended) A server-written note on the PR naming the run link and the pinned commit, and nothing the run wrote. (B) Only the console and the delivery row. (C) The review posted by a separate comment-only credential, which puts prose an attacker can steer under iterion's identity and needs its own security review.
- **Q2 — the hold label.** (A, recommended) It vetoes the gesture, because it stops a run and never steers one. (B) It is ignored, following #874's letter and the `/command` rule that labels pause only automation.
- **Q3 — where fork runs execute in the cloud.** (A, recommended) A dedicated runner pool that resolves a kubernetes sandbox for every run, named by the fork-lane config and stamped as `RunnerPool` by the publisher. This needs chart work, because `runner.sandbox.enabled` is deployment-wide today. (B) `runner.sandbox.enabled` for the whole deployment, which moves every trusted run whose bot declares a `sandbox:` into a sibling pod. (C) Self-hosted only, with the cloud refusing as it does today.
- **Q4 — the per-contributor bound.** (A, recommended) Runs and dollars per month, held on the org beside its quota, with a platform default in the settings ladder (proposed: 20 runs, $20) and an explicit unbounded value as the escape hatch. (B) On the fork-lane config; the key has no config segment, so two configs of one org would share one counter under two caps. (C) No bound, with the gesture as the only one.

## Consequences

- A fork contributor can get a review, and only after a write-level maintainer says so, for one commit, inside a sandbox, with nothing published by the run.
- The floors protect any future untrusted class (a mirror, an imported tree), because they read `Trusted()` and not `IsFork()`.
- Behaviour changes for trusted runs at two points: an ordinary GitLab MR whose payload names neither project is refused, and the in-process launch carries the marker.
- Each unit reviews against the branch's rows rather than re-deriving the threat model, which already cost six rounds.
- The per-author row stores an outside contributor's numeric forge id under the 400-day TTL, purged with the org.
- Out of scope: the GitLab fork lane, ADR-161's resolver (U5 only changes the mount its admission reads), and the forge-integration toggle that would provision a fork-lane hook.
