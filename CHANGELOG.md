# Changelog

Generated from Conventional Commits at each release. Older majors are archived
under [docs/changelog/](https://github.com/SocialGouv/iterion/tree/main/docs/changelog).

## [3.112.16](https://github.com/SocialGouv/iterion/compare/v3.112.15...v3.112.16) (2026-09-06)

### Bug Fixes

* **delegate:** a terminal verdict on the formatting pass is returned, not retried; a structured answer is an answer; a 403 is a refusal ([#845](https://github.com/SocialGouv/iterion/issues/845)) ([8202bcf](https://github.com/SocialGouv/iterion/commit/8202bcffbc4706fcde475d52827ef4a95a836d4a))

    <details><summary>why</summary>

    Third review round of the facade-render fix. The formatting loop assigned every rendered verdict to fmtErr and retried the attempt once, so a dead credential, an unavailable model or an exhausted window bought a second CLI spawn against a provider that had just refused, and re-filed the same usage evidence. Only the transient class is retried now (renderRetryable); the rest returns typed at once, as the recovery pass already did.

    </details>

## [3.112.15](https://github.com/SocialGouv/iterion/compare/v3.112.14...v3.112.15) (2026-09-06)

### Bug Fixes

* **server:** a refused launch gives the board card back and retries with a backoff; the attempt cap files it blocked with the reason ([#840](https://github.com/SocialGouv/iterion/issues/840)) ([558325b](https://github.com/SocialGouv/iterion/commit/558325ba7a00c61d6062416f63a898a30af02a5c)), closes [#814](https://github.com/SocialGouv/iterion/issues/814), references [#839](https://github.com/SocialGouv/iterion/issues/839)

    <details><summary>why</summary>

    Issue.LaunchRefusal (attempts, last reason, last instant, not_before) is the cloud dispatcher's retry bound for a claimed card whose launch the run service refused before any run started. Written fenced (SetLaunchRefusalOwned, a BoardStore contract method on both twins), kept across the give-back transition, cleared by a stamped run (SetLastRun / SetLastRunOwned - a launch happened) and by an operator Reopen. Coordinator.ListDispatchable skips a card until its not_before, in the query, for the…

    </details>

## [3.112.14](https://github.com/SocialGouv/iterion/compare/v3.112.13...v3.112.14) (2026-09-06)

### Bug Fixes

* **billy:** a delivery reserve out of the duration cap, a typed decline the platform honours, a structural drift gate, and no masked exit status in verify.sh ([#830](https://github.com/SocialGouv/iterion/issues/830)) ([5166823](https://github.com/SocialGouv/iterion/commit/51668231d8704c8af6d5466a1a3970ff176227f3)), closes [#789](https://github.com/SocialGouv/iterion/issues/789) [#779](https://github.com/SocialGouv/iterion/issues/779) [#705](https://github.com/SocialGouv/iterion/issues/705) [#706](https://github.com/SocialGouv/iterion/issues/706), references [#770](https://github.com/SocialGouv/iterion/issues/770) [#705](https://github.com/SocialGouv/iterion/issues/705) [#706](https://github.com/SocialGouv/iterion/issues/706) [#779](https://github.com/SocialGouv/iterion/issues/779) [#789](https://github.com/SocialGouv/iterion/issues/789)

    <details><summary>why</summary>

    has_drift_gate read the verify script one line at a time and counted a quiet diff only when the failing exit appeared on the SAME line, so the commonest real shape -- a multiline "if ! git diff --quiet ...; then ... exit 1; fi", which is what this repo's own openapi:check target writes -- was rejected. Run 01a072b5 delivered eight commits, logged VERIFY OK, and still returned exit 3 / DRIFT GATE MISSING, leaving the merge gate red on a green tree.

    </details>
* **forge,webhooks:** one App client per connection with its real slug, tokens minted from the recorded grant, a 403 that arms the preflight, a connection-first reply gate, and a GitLab fork MR that names its source ([#846](https://github.com/SocialGouv/iterion/issues/846)) ([e2b5fe7](https://github.com/SocialGouv/iterion/commit/e2b5fe70a695ac2e78dfd22992f08ba612878496)), closes [#711](https://github.com/SocialGouv/iterion/issues/711) [#710](https://github.com/SocialGouv/iterion/issues/710) [#717](https://github.com/SocialGouv/iterion/issues/717) [#708](https://github.com/SocialGouv/iterion/issues/708) [#728](https://github.com/SocialGouv/iterion/issues/728), references [#711](https://github.com/SocialGouv/iterion/issues/711)

    <details><summary>why</summary>

    Two AppClient gaps that share a fix site.

    </details>
* **golden-master:** the harness reverts a mutant an interrupted gate left applied, at the start of the next ([#807](https://github.com/SocialGouv/iterion/issues/807)) ([7a70bbc](https://github.com/SocialGouv/iterion/commit/7a70bbc53d082e4356c9441f866ced1b6a940d61)), references [#799](https://github.com/SocialGouv/iterion/issues/799)

    <details><summary>why</summary>

    A stream cut, a SIGTERM or a pod kill between apply.sh and revert.sh leaves a mutant in the tree. The next gate on that tree judged a mutated program and called it the lot's: measured on a verify node retried on the same tree after its exec stream broke at 7 676 s — the oracle reported the mutant's file as "not committed", the build gate went red on a package that exists on the bank, and the run finished not converged with hours of budget left.

    </details>
* **server:** a publish grant must belong to the run that carries it ([#849](https://github.com/SocialGouv/iterion/issues/849)) ([0aab3d2](https://github.com/SocialGouv/iterion/commit/0aab3d2b583017a70e9ac103a0555e8c998bc434)), closes [#825](https://github.com/SocialGouv/iterion/issues/825)

    <details><summary>why</summary>

    The publish token is a launch VAR, and injectForgePublishVars honours a caller-pinned one. Every reader then proved the grant SELF-consistent — its connection belongs to its team, its repo matches the pull request, its host matches the connection — and a grant minted for another tenant passed all of them. None asked whether that tenant was the run's. So an authenticated operator of team B holding a team-A token could have iterion comment on team A's pull request, write team A's REQUIRED commit…

    </details>

## [3.112.13](https://github.com/SocialGouv/iterion/compare/v3.112.12...v3.112.13) (2026-09-06)

### Bug Fixes

* **server,store:** the pr_url launches pass the fork guard, a typed PR-closed end reason, a claimed launch_error retry, and gate notices that name a remedy that can reach the run ([#824](https://github.com/SocialGouv/iterion/issues/824)) ([463394b](https://github.com/SocialGouv/iterion/commit/463394bac887d7d9215e051503ff6e027556d166)), closes [#683](https://github.com/SocialGouv/iterion/issues/683) [#702](https://github.com/SocialGouv/iterion/issues/702) [#722](https://github.com/SocialGouv/iterion/issues/722) [#721](https://github.com/SocialGouv/iterion/issues/721) [#713](https://github.com/SocialGouv/iterion/issues/713) [#715](https://github.com/SocialGouv/iterion/issues/715) [#712](https://github.com/SocialGouv/iterion/issues/712), references [#713](https://github.com/SocialGouv/iterion/issues/713)

    <details><summary>why</summary>

    applyPRLaunchContext — the door the studio launch, the remote CLI/MCP launch and the cloud board coordinator use to stamp a PR onto a run — carried no fork guard, while the five webhook lanes were made fail-closed by #683. Same launch pair (<base>.CloneURL + the PR's head branch), so a head branch that does not live in the base repo could be checked out against a same-named branch of the base repo and a code-pushing bot would commit onto it.

    </details>

## [3.112.12](https://github.com/SocialGouv/iterion/compare/v3.112.11...v3.112.12) (2026-09-06)

### Bug Fixes

* **runner:** the per-run clone's git commands leave no maintenance process behind ([#829](https://github.com/SocialGouv/iterion/issues/829)) ([da8a2f0](https://github.com/SocialGouv/iterion/commit/da8a2f0bc8278e4ca206ffcf4a56e735415cbb6b)), closes [#821](https://github.com/SocialGouv/iterion/issues/821), references [#813](https://github.com/SocialGouv/iterion/issues/813) [#821](https://github.com/SocialGouv/iterion/issues/821) [#828](https://github.com/SocialGouv/iterion/issues/828)

    <details><summary>why</summary>

    The merge-queue build of #813 ejected the PR on a cleanup, not an assertion: `TempDir RemoveAll cleanup: unlinkat …/deploy-onyxia-…/.git/ objects: directory not empty` — something was still writing under the plugin-source cache checkout after the test had passed.

    </details>
* **runtime,bots:** an oracle that never ran is not a RED — the run-files env var goes with its bind, the report goes to a temp file of the run ([#815](https://github.com/SocialGouv/iterion/issues/815)) ([f517637](https://github.com/SocialGouv/iterion/commit/f5176376a1f74cd1888feeec5aa79d0bce624807)), references [#795](https://github.com/SocialGouv/iterion/issues/795) [pre-#795](https://github.com/pre-/issues/795)

    <details><summary>why</summary>

    Measured on the pod backend, four modernize lots in one night: gate green, references intact, and "oracle RED" on one line — the gate wrapper's `cannot create /iterion/artifact-files/gm-last-report.json: Directory nonexistent`. The harness never ran: the redirect died first. One lot diagnosed it and blocked, two were stopped by the loop budget guard, one spent 173 minutes repairing an environment. Some 20 run-hours, no verdict.

    </details>
* **sandbox:** a sandboxed claw node relays its tools, retries and turns ([#835](https://github.com/SocialGouv/iterion/issues/835)) ([e6c4b40](https://github.com/SocialGouv/iterion/commit/e6c4b4075c1c85ad3fe6ae5d636e690afaf15bba)), closes [#811](https://github.com/SocialGouv/iterion/issues/811), references [#805](https://github.com/SocialGouv/iterion/issues/805)

    <details><summary>why</summary>

    The `__claw-runner` relay carried the LLM steps (#805) and nothing else, so everything the in-container loop observed died at the container boundary: the studio timeline of a sandboxed claw node showed LLM steps and no tools, an in-container permission denial left no audit, a supervisor's `tool_*` monitor never armed, the plan checklist stayed empty, a retry or a compaction round read as a silent gap, and `iterion fork --turn` had no anchor at all.

    </details>
* **server:** a live 401 marks the connection revoked; only a forge that answered "no" earns the vouch ([#833](https://github.com/SocialGouv/iterion/issues/833)) ([5cd4877](https://github.com/SocialGouv/iterion/commit/5cd48770251e44828d0cd538fb172f847453334d))

    <details><summary>why</summary>

    A failed WhoAmI now takes three shapes instead of one. A credential the forge rejects outright is the reconnect problem the revoked-status rung already names — a 422, forced or not, because no force can carry an upload the forge would refuse the same way. A forge that answered but would not describe the account (insufficient scope) is the one case the operator can vouch for: a 409 unforced, tolerated under force. Anything else — a spent budget, an unreachable forge, a 5xx — is not an answer: a…

    </details>

## [3.112.11](https://github.com/SocialGouv/iterion/compare/v3.112.10...v3.112.11) (2026-09-06)

### Bug Fixes

* **git,pluginsource,runview:** a git command iterion runs into a directory it disposes of leaves nothing running ([#827](https://github.com/SocialGouv/iterion/issues/827)) ([cdfdc65](https://github.com/SocialGouv/iterion/commit/cdfdc6523b241ca608109398e8d0dbcd4f3b983a)), closes [#821](https://github.com/SocialGouv/iterion/issues/821), references [#813](https://github.com/SocialGouv/iterion/issues/813)

    <details><summary>why</summary>

    The merge-queue build of #813 ejected the PR on a cleanup, not an assertion: `TempDir RemoveAll cleanup: unlinkat …/deploy-onyxia-…/.git/ objects: directory not empty` — something was still writing under the plugin-source cache checkout after the test had passed.

    </details>

## [3.112.10](https://github.com/SocialGouv/iterion/compare/v3.112.9...v3.112.10) (2026-09-06)

### Bug Fixes

* **forge:** the GitHub-App client serves the pull/CI API on scoped tokens — the card PR panel works on App connections ([#809](https://github.com/SocialGouv/iterion/issues/809)) ([89fafb5](https://github.com/SocialGouv/iterion/commit/89fafb512c774aa41ffb5027712af0da7524c80d)), closes [#777](https://github.com/SocialGouv/iterion/issues/777)

    <details><summary>why</summary>

    forgeAdminFor yields a forgegithub.AppClient for a github_app connection, and that type did not implement forge.PullClient, so pullClientForConn failed its assertion: GET|POST /api/v1/native/issues/{id}/pulls, …/pulls/{n}/ci and …/pulls/{n}/merge answered 501 on the connection kind the connect wizard creates by default, while the same card worked on a PAT connection.

    </details>

## [3.112.9](https://github.com/SocialGouv/iterion/compare/v3.112.8...v3.112.9) (2026-09-06)

### Bug Fixes

* **delegate:** a facade's bracketed API-error render is an API error, not a node's answer ([#818](https://github.com/SocialGouv/iterion/issues/818)) ([6930fd8](https://github.com/SocialGouv/iterion/commit/6930fd82ac1664d0471500e4914467585976b84b))

    <details><summary>why</summary>

    Measured on a golden-master run behind an Anthropic-shaped facade: the campaign node finished in 2.5 minutes with its structured output built from "API Error: [500][Operation failed][<request id>]" — the CLI's render of an upstream 500 — and the graph continued: 283 minutes and the next node's budget spent on a request that was never acted, the run ending unconverged.

    </details>

## [3.112.8](https://github.com/SocialGouv/iterion/compare/v3.112.7...v3.112.8) (2026-09-06)

### Bug Fixes

* **secrets,server,cli:** the OAuth token endpoints are env-overridable, a refused personal OAuth credential is audited, and `iterion secret set` shape-checks what it stores ([#819](https://github.com/SocialGouv/iterion/issues/819)) ([ca46d33](https://github.com/SocialGouv/iterion/commit/ca46d3331c7f7f69ba5d044240e21b0aea477243)), closes [#725](https://github.com/SocialGouv/iterion/issues/725) [#726](https://github.com/SocialGouv/iterion/issues/726) [#727](https://github.com/SocialGouv/iterion/issues/727)

    <details><summary>why</summary>

    The Anthropic forfait's authorize URL, redirect URI and scopes each read envOr("ITERION_OAUTH_FORFAIT_ANTHROPIC_...", default). The token endpoint the auth-code exchange and the refresh worker both POST to was a bare const, under a comment promising the same per-deployment override — so an OEM-repackaged CLI or a proxying deployment could move three endpoints of four and keep refreshing against the vendor's host, silently.

    </details>

## [3.112.7](https://github.com/SocialGouv/iterion/compare/v3.112.6...v3.112.7) (2026-09-06)

### Bug Fixes

* **server:** the board dispatcher never claims a card it cannot launch, and a column iterion wrote on its own authority is not reflected onto the bound board ([#813](https://github.com/SocialGouv/iterion/issues/813)) ([9a4a6a0](https://github.com/SocialGouv/iterion/commit/9a4a6a07609b43ee237adb99a550578beec6b794)), references [#798](https://github.com/SocialGouv/iterion/issues/798) [#798](https://github.com/SocialGouv/iterion/issues/798) [#798](https://github.com/SocialGouv/iterion/issues/798) [#798](https://github.com/SocialGouv/iterion/issues/798)

    <details><summary>why</summary>

    Two store-level seams the #798 fix rests on. Coordinator.ListDispatchable is the cloud dispatch tick's candidate query: unclaimed cards in a launch column THAT CARRY A BOT, filtered in the query because the batch is capped and bot-less cards (never written, so the oldest) would fill every batch. ListEligible stays the sweeps' listing. Issue.StateReason persists the provenance of the card's last transition - the same value its state event carries, derived once by native.StateProvenance - so a…

    </details>

## [3.112.6](https://github.com/SocialGouv/iterion/compare/v3.112.5...v3.112.6) (2026-09-06)

### Bug Fixes

* **dsl,runtime:** three silent constants — run.* in every data mapping, a typed compute int, outputs.* in tool commands ([#816](https://github.com/SocialGouv/iterion/issues/816)) ([0b2138c](https://github.com/SocialGouv/iterion/commit/0b2138ce5c7817229a5c7ffb5a375600eac3efd9)), closes [#791](https://github.com/SocialGouv/iterion/issues/791) [#792](https://github.com/SocialGouv/iterion/issues/792) [#797](https://github.com/SocialGouv/iterion/issues/797)

    <details><summary>why</summary>

    resolveRef's RefRun arm served `id` and nothing else, so a fail node's `message:` (and an edge `with`, an `emit` payload, a subbot `with:`) rendered `{{run.max_cost_usd}}` / `{{run.elapsed_seconds}}` as an empty string with no diagnostic — C029/C036 accept the reference, the resolver dropped it. The very message a budget guard wants ("planning used X of Y, raise --max-duration and resume") could not be written from `run.*`.

    </details>
* **server:** a forge that will not describe the account asks for the operator's word, not a 502 dead end ([#808](https://github.com/SocialGouv/iterion/issues/808)) ([bd94b5a](https://github.com/SocialGouv/iterion/commit/bd94b5a1781d2d73776f920cfc8c10c95da422af))

    <details><summary>why</summary>

    An avatar apply on a connection of unknown kind asks the forge who the token is. When the forge refuses to say (a token without the scope, a forge without the field), the unforced apply answered a bare 502 that recorded nothing — so the studio card, which now only offers the vouch on a 409, repeated the identical 502 on every click and the forced apply the server still supports was unreachable from it.

    </details>

## [3.112.5](https://github.com/SocialGouv/iterion/compare/v3.112.4...v3.112.5) (2026-09-06)

### Bug Fixes

* **runner:** a sandboxed claw node meters from its relayed steps, on the credential that served it ([#810](https://github.com/SocialGouv/iterion/issues/810)) ([dc76093](https://github.com/SocialGouv/iterion/commit/dc760935d1c283aac79edf10a00b666699074d1b)), closes [#805](https://github.com/SocialGouv/iterion/issues/805), references [#805](https://github.com/SocialGouv/iterion/issues/805)

    <details><summary>why</summary>

    A sandboxed claw node prices its call inside the container, from a cold process where neither the live registry nor the aggregator has a cache yet, so the committed table is what it falls back to — and gpt-5.6-sol, the model the campaign bots' cross-family plan review runs on, had no entry: 24 of the 33 plan reviews served on ovh-prod in the last week carried no cost_usd at all (#805).

    </details>

## [3.112.4](https://github.com/SocialGouv/iterion/compare/v3.112.3...v3.112.4) (2026-09-06)

### Bug Fixes

* **runner:** an IR this runner cannot load is a verdict on the run, acked — not eight silent redeliveries to the DLQ ([#806](https://github.com/SocialGouv/iterion/issues/806)) ([b693fd0](https://github.com/SocialGouv/iterion/commit/b693fd0e550d250f1e05c6799b6f8e80fb9a231e))

    <details><summary>why</summary>

    Measured on a five-release server/runner skew: six runs (five of one team, one of another) went failed_resumable within 75 s with zero events and an empty final_error; the runner logs read "compile IR: N diagnostic(s)" on every delivery, 1 … 8, then "parking on DLQ". The compiled IR of a server ahead of the fleet does not load on the runner, the error came back generic, and the delivery loop did what it does with generic errors — while the run said nothing.

    </details>

## [3.112.3](https://github.com/SocialGouv/iterion/compare/v3.112.2...v3.112.3) (2026-09-06)

### Bug Fixes

* **server:** a forced apply bails when /user never answers; the card vouches only on a 409 ([#804](https://github.com/SocialGouv/iterion/issues/804)) ([66e63b5](https://github.com/SocialGouv/iterion/commit/66e63b5674318528632b4d63e47a83bc83c4eda5)), references [#803](https://github.com/SocialGouv/iterion/issues/803)

    <details><summary>why</summary>

    Revi's gate on #803 (R77fadf, R4a0ce8) and its assumptions: - Under force, a WhoAmI failure was swallowed even when it was the apply's own deadline expiring — the upload then ran on a dead context and stamped "context deadline exceeded" on the connection, blaming the avatar for a forge that does not answer. A genuine refusal is still tolerated; a spent budget bails with the accurate reason and records nothing. - The card sent force:true for every account not flagged as a bot, so the 409 branch…

    </details>

## [3.112.2](https://github.com/SocialGouv/iterion/compare/v3.112.1...v3.112.2) (2026-09-06)

### Bug Fixes

* **server:** the avatar record rides its own budget, not the round-trips' deadline ([#803](https://github.com/SocialGouv/iterion/issues/803)) ([75e8d0e](https://github.com/SocialGouv/iterion/commit/75e8d0e400bf2bb832f2b48ae09e50b2a6b576c2)), references [#800](https://github.com/SocialGouv/iterion/issues/800)

    <details><summary>why</summary>

    Revi's gate on #800 (R8b0bd7): giving the apply one bounded context put the outcome record under the same 20 s deadline as the upload — so the slow forge, the failure worth recording, expired the context first and the record never ran (and an upload landing near the deadline answered 502 for an avatar that was live). The record now takes a 10 s budget of its own, detached from the round-trips; a forge that hangs is proved to leave its reason on the connection.

    </details>

## [3.112.1](https://github.com/SocialGouv/iterion/compare/v3.112.0...v3.112.1) (2026-09-06)

### Bug Fixes

* **runtime:** resolve the template snapshot on every dispatch path — a fan-out branch, the llm router and llm_or_human rendered {{run.*}} and {{outputs.*}} as literals ([#796](https://github.com/SocialGouv/iterion/issues/796)) ([72275c0](https://github.com/SocialGouv/iterion/commit/72275c0ff80a7af8cd398067f5060608239a4ba0)), closes [#763](https://github.com/SocialGouv/iterion/issues/763), references [#763](https://github.com/SocialGouv/iterion/issues/763) [#763](https://github.com/SocialGouv/iterion/issues/763)

    <details><summary>why</summary>

    `model.WithRunID` / `WithTemplateData` were attached on the trunk dispatch path only, so the SAME node rendered two ways depending on how it was reached. Inside a `fan_out_all` / `fan_out_each` body a prompt kept its literal `{{outputs.x.y}}` braces and a tool `command:` substituted an EMPTY string for `{{run.id}}` — a silent constant, not a visible failure. The llm router and the `llm_or_human` LLM half never had the snapshot either.

    </details>

## [3.112.0](https://github.com/SocialGouv/iterion/compare/v3.111.1...v3.112.0) (2026-09-06)

### Features

* **chart:** a priorityClassName for the server and runner Deployments ([#802](https://github.com/SocialGouv/iterion/issues/802)) ([8a22cc7](https://github.com/SocialGouv/iterion/commit/8a22cc71ca8d3bfbdf578d434a21022bab315cbf))

    <details><summary>why</summary>

    The platform pods (server, runner) and the sandboxed run pods carried no PriorityClass, so a run pod bursting on a node committed at 99 % of its requests was a peer of the runner that owns its run for the scheduler and for eviction. `server.priorityClassName` / `runner.priorityClassName` (empty = the cluster default) let a deployment rank the platform above the run pods it starts (SocialGouv/iterion#732: prod sets both to the cluster's `resource-burstable`; the run pods stay unclassed).

    </details>

### Bug Fixes

* **board:** derive the degraded readout from the binding, not from one pass ([#787](https://github.com/SocialGouv/iterion/issues/787)) ([cfaaa62](https://github.com/SocialGouv/iterion/commit/cfaaa629957b6b8d0ed89090d4857d59578e4cc4)), closes [#775](https://github.com/SocialGouv/iterion/issues/775), references [#793](https://github.com/SocialGouv/iterion/issues/793)

    <details><summary>why</summary>

    ReconcileStatusOptions reported a LostColumn only on the pass that observed the loss: both append sites were guarded by `id != ""` and that same pass dropped the cached id. From the next pass on the state had no id, so `rep.Lost` was empty and `rep.Reason()` was "" — and the caller, reading "nothing lost", cleared `degraded` on the first unrelated adoption. The lost column then read healthy forever while every reflect onto it still refused, counted in `reflect_no_column` and invisible…

    </details>
* **runtime:** the loop budget guard's warning says its rule ([#801](https://github.com/SocialGouv/iterion/issues/801)) ([1dfe7ec](https://github.com/SocialGouv/iterion/commit/1dfe7ecd0a676bbeeb2f9725d2b0f91d8b08a76f))

    <details><summary>why</summary>

    A declined back-edge emitted budget_warning {remaining, needed, used, limit}; read on a run with more remaining than needed, it did not say why the loop stopped — the rule is that the next iteration would land at or past the 90 % threshold where the engine refuses to start any node, and remaining > needed does not contradict it. Measured on a run whose repair loop was declined with remaining 15 644 s and needed 13 154 s: used 13 156 + needed 13 154 = 26 310 ≥ 25 920 (90 % of 28 800). The event…

    </details>

## [3.111.1](https://github.com/SocialGouv/iterion/compare/v3.111.0...v3.111.1) (2026-09-06)

### Bug Fixes

* **server:** a connection older than account_kind learns it before the avatar gate ([#800](https://github.com/SocialGouv/iterion/issues/800)) ([d7e4138](https://github.com/SocialGouv/iterion/commit/d7e41384ad7862a65badf81e6c5859f08c359e9f))

    <details><summary>why</summary>

    Revi's gate on #794: the runbook handed the PIC operator the non-forced avatar command for connections that predate AccountKind — every one of them answered 409 needs_force, because the field is written at connect time only. The apply now asks the forge who the token is when the field is empty and records what it learns, so the bot gate judges the account and a group-token bot user needs no --force; a person's PAT still does.

    </details>

## [3.111.0](https://github.com/SocialGouv/iterion/compare/v3.110.0...v3.111.0) (2026-09-05)

### Features

* **brand:** the iterion-bot mascot everywhere — forge bot avatars, favicons, app icon, docs logo ([#794](https://github.com/SocialGouv/iterion/issues/794)) ([1d0661d](https://github.com/SocialGouv/iterion/commit/1d0661d74da8953ac2ec4f0ce78988c62c515772))

    <details><summary>why</summary>

    The official iterion-bot GitHub account wears the mascot; the product still wore the hexagon, and every favicon/app-icon/logo copy was a hand-dropped file with no source of truth.

    </details>

## [3.110.0](https://github.com/SocialGouv/iterion/compare/v3.109.0...v3.110.0) (2026-09-05)

### Features

* **trigger:** an effect KIND on the outbox — a native card move reaches the bound GitHub board through the durable outbox, the pass stays the net ([#793](https://github.com/SocialGouv/iterion/issues/793)) ([7d01723](https://github.com/SocialGouv/iterion/commit/7d01723333c26ae64c4d66821c3de21424977c05))

    <details><summary>why</summary>

    The outbox row gains a discriminator so an effect that is NOT owed to a subscription can ride it: `launch` (ADR-094's original) and `projection` (ADR-097 §10's named follow-up).

    </details>

### Bug Fixes

* **runtime:** a loop is priced on any entry into its body; a stale run-start mark is re-based on resume ([#783](https://github.com/SocialGouv/iterion/issues/783)) ([d69189a](https://github.com/SocialGouv/iterion/commit/d69189ae8bd4d647c2fd076556ef73d26c62b4f3))

    <details><summary>why</summary>

    The loop budget guard prices the next iteration by the last one, from a mark set when the loop is entered from outside. That mark was only set when the body was entered at the loop's HEAD (loop.Entries). A campaign bot's extension loop is entered elsewhere: its body shares the verify and gate nodes with the lot's own repair loop, so the run reaches it at verify, off its head, and the loop kept the mark the session baseline had set at run start. Its first crossing was then priced at everything…

    </details>

## [3.109.0](https://github.com/SocialGouv/iterion/compare/v3.108.3...v3.109.0) (2026-09-05)

### Features

* **bots:** the plan-budget guard reads the run, and every coded refusal reaches the run as a typed fail ([#790](https://github.com/SocialGouv/iterion/issues/790)) ([044904c](https://github.com/SocialGouv/iterion/commit/044904c33422f37ac1f401a200d6fa7bca5237b4))

    <details><summary>why</summary>

    branch-improve-loop's plan-phase guard measured its own wall clock in `plan_scope_probe` (a `started_epoch` stamp) and compared spend against two vars mirroring the `budget:` block by hand. Both were workarounds for primitives that did not exist; both are now defects. A mirror var is not reached by `iterion run --max-cost-usd 200`, so a re-budgeted run kept refusing against the literal 75 nobody had updated — the guard's arithmetic silently disagreeing with the budget the run was actually under.

    </details>

### Bug Fixes

* **engine,bots:** the engine's script and the judge's report stay out of the judged tree ([#795](https://github.com/SocialGouv/iterion/issues/795)) ([7a792ac](https://github.com/SocialGouv/iterion/commit/7a792ac4d8ac4da173d10eaa640e17c8a693cec4))

    <details><summary>why</summary>

    An extension act by pure addition, correct in content, was refused by the net's verify for three untracked paths the acting agent never wrote: `.iterion-script-*.py` — the tool node's own script, created by the executor at the workspace root so a bind-mount sandbox can see it — and `.golden-master/.last-report.json` — the gate wrapper's report, defaulting inside the net — beside the skills the engine mirrors. The gate that judges the tree's cleanliness read the engine's instrument and the…

    </details>

## [3.108.3](https://github.com/SocialGouv/iterion/compare/v3.108.2...v3.108.3) (2026-09-05)

### Bug Fixes

* **forge:** the GitHub-App client serves the issue API on scoped tokens — the forge→board issue sync works on App connections ([#776](https://github.com/SocialGouv/iterion/issues/776)) ([0025229](https://github.com/SocialGouv/iterion/commit/0025229fd36219a92b095ff0fa6680718969b109)), closes [#781](https://github.com/SocialGouv/iterion/issues/781)

    <details><summary>why</summary>

    forgeAdminFor returns a *github.AppClient for a github_app connection and that type carried only CommentIssue, so `admin.(forge.IssueClient)` failed on the connection shape the studio's connect wizard creates by default. The forge->board sync answered 502 "provider github has no issue client" on demand and warned it every 5 minutes in the worker, so a bound team's cards were never hydrated and the ADR-097 project pass read skipped_no_card for every item, pass after pass. The autofix lane's…

    </details>

## [3.108.2](https://github.com/SocialGouv/iterion/compare/v3.108.1...v3.108.2) (2026-09-05)

### Bug Fixes

* **runner:** retain exhausted lock deliveries in the DLQ ([#770](https://github.com/SocialGouv/iterion/issues/770)) ([c804064](https://github.com/SocialGouv/iterion/commit/c80406445b17f4d4ce42ed55be6f58795db01593)), closes [#703](https://github.com/SocialGouv/iterion/issues/703)

    <details><summary>why</summary>

    Delay lock contention by the configured lease interval and retain the final delivery in the DLQ with a durable event. Closes #703.

    </details>

## [3.108.1](https://github.com/SocialGouv/iterion/compare/v3.108.0...v3.108.1) (2026-09-05)

### Bug Fixes

* **cli:** a subbot child on the CLI host executes in the parent's sandbox and in the parent's workdir ([#778](https://github.com/SocialGouv/iterion/issues/778)) ([1a781bb](https://github.com/SocialGouv/iterion/commit/1a781bbb05035a9a4be88f27184ebc3c1db0585e)), references [#766](https://github.com/SocialGouv/iterion/issues/766)

    <details><summary>why</summary>

    #766 made a child execute in its parent's sandbox on the cloud runner and in the studio's in-process service — and left the third host out: the CLI's subbot runner built the child engine without the parent's sandbox facts and without a workdir, so the child defaulted to the process cwd (engine.go: workDir defaults to os.Getwd() at Run time). A parent that swapped to a per-run worktree therefore handed its child the BASE tree: whatever the child committed landed in a tree the parent's gate never…

    </details>

## [3.108.0](https://github.com/SocialGouv/iterion/compare/v3.107.0...v3.108.0) (2026-09-05)

### Features

* **dsl:** a node reads the run's budget through run.*, and a fail node carries a typed code, message and resumability ([#764](https://github.com/SocialGouv/iterion/issues/764)) ([34a6850](https://github.com/SocialGouv/iterion/commit/34a6850df9ef5007281f9bd287a89ba3f7b25dde)), closes [#738](https://github.com/SocialGouv/iterion/issues/738) [#739](https://github.com/SocialGouv/iterion/issues/739), references [#737](https://github.com/SocialGouv/iterion/issues/737) [#695](https://github.com/SocialGouv/iterion/issues/695) [#670](https://github.com/SocialGouv/iterion/issues/670) [#760](https://github.com/SocialGouv/iterion/issues/760)

    <details><summary>why</summary>

    A node that wants to reason about the run's budget -- "planning has used a third of max_duration, stop planning" -- had nothing to read: the `run` namespace resolved only `run.id`, and the `budget:` block's caps were compile-time literals. PR #737 had to self-measure wall-clock in a tool node and mirror the budget through two hand-maintained vars that drift from the block in silence; every phase-budget guard would have repeated it.

    </details>

## [3.107.0](https://github.com/SocialGouv/iterion/compare/v3.106.1...v3.107.0) (2026-09-05)

### Features

* **golden-master:** sync-harness.bot — the judge's code reaches a target tree without a rite ([#750](https://github.com/SocialGouv/iterion/issues/750)) ([12e35c6](https://github.com/SocialGouv/iterion/commit/12e35c6731d26a2b0802d643027e392d7f311ce1)), references [#765](https://github.com/SocialGouv/iterion/issues/765) [#765](https://github.com/SocialGouv/iterion/issues/765)

    <details><summary>why</summary>

    The harness is the net's decision procedure. Its content belongs to the net's owner and changes through a rite; its code is this bundle's and is repaired upstream — and until now the only path for a repaired judge into a tree that judges with it was a full rite: hours of agent work and a fresh held-out cycle for a one-line fix in a file no agent may edit. Measured on a live campaign: a rite re-materialised the harness with the bug still in it, and every lot on that tree was refused on the same…

    </details>

## [3.106.1](https://github.com/SocialGouv/iterion/compare/v3.106.0...v3.106.1) (2026-09-05)

### Bug Fixes

* **board:** re-land [#745](https://github.com/SocialGouv/iterion/issues/745)'s round-4 fixes the merge queue dropped, plus the round-5 findings and [#759](https://github.com/SocialGouv/iterion/issues/759) ([#772](https://github.com/SocialGouv/iterion/issues/772)) ([ef7b8c1](https://github.com/SocialGouv/iterion/commit/ef7b8c1758a3e4e0a03ef169d71aaf1cec3ad1da))

    <details><summary>why</summary>

    SetStateFrom answers a card that drifted between the read and the write with (issue, changed=false, nil) — the operator got there first, which is not an error. The project import discarded that flag and read the nil error as success: it counted a transition the store never made, skipped the reflect, and recorded the board's status as synchronized, which makes decideProjectStatus a no-op from then on. The periodic worker repairs that on a later pass; the one-shot `iterion issue import --project`…

    </details>
* **runtime,runner:** a subbot child executes in its parent's sandbox — a pod of its own lost its commits ([#766](https://github.com/SocialGouv/iterion/issues/766)) ([b272045](https://github.com/SocialGouv/iterion/commit/b272045cfb5a13730098360c3c81dbf8003cf4e8))

    <details><summary>why</summary>

    Measured on the first subbot to run on a cloud pod: under the kubernetes driver the child engine started a sandbox of its own, the driver copies the workspace into a pod, so the child's commits lived in its copy and died with it (final_branch null, the commit unreachable from anywhere) while the parent, parked on the subbot node, resumed and re-judged an unchanged tree. A net's extension loop cannot converge that way. On the docker driver the same code happened to work: a second container…

    </details>
* **store:** protect run saves with document version checks ([#771](https://github.com/SocialGouv/iterion/issues/771)) ([09fe07f](https://github.com/SocialGouv/iterion/commit/09fe07f354d10763720f4d142338230dbb772ad6)), closes [#701](https://github.com/SocialGouv/iterion/issues/701)

    <details><summary>why</summary>

    Advance versions on partial writes in both stores; protect rename and rewind, and reload queued transitions before saving metadata. Preserve legacy documents and destination versions during migration. Closes #701.

    </details>
* **webhooks:** acknowledge authorization outages without launching work ([#768](https://github.com/SocialGouv/iterion/issues/768)) ([d2d0def](https://github.com/SocialGouv/iterion/commit/d2d0defdf8f074e6f43f1cd57e99d6bc4c61a486)), closes [#704](https://github.com/SocialGouv/iterion/issues/704)

    <details><summary>why</summary>

    Retain the forge failure in the delivery audit and return HTTP 200 across the command, conversation and review request lanes. Closes #704.

    </details>

## [3.106.0](https://github.com/SocialGouv/iterion/compare/v3.105.0...v3.106.0) (2026-09-05)

### Features

* **bots:** the campaign fleet plans by default, refuses a missing repo typed, and carries Persy ([#761](https://github.com/SocialGouv/iterion/issues/761)) ([7aa77ff](https://github.com/SocialGouv/iterion/commit/7aa77ffeba2e01c18c0f4c529ac978bb566d42a5)), references [#751](https://github.com/SocialGouv/iterion/issues/751) [#752](https://github.com/SocialGouv/iterion/issues/752) [#619](https://github.com/SocialGouv/iterion/issues/619) [#751](https://github.com/SocialGouv/iterion/issues/751) [#752](https://github.com/SocialGouv/iterion/issues/752) [#619](https://github.com/SocialGouv/iterion/issues/619)

    <details><summary>why</summary>

    The seven campaign bots (feature-dev, feature-gap-fill, branch-improve-loop, whole-improve-loop, test-coverage, e2e-coverage, app-dev) keyed their plan phase on plan_review, which ResolvePlanReview answers off on every single-provider deployment - so the commonest setup never planned, silently, under a var named after a review (#751).

    </details>

### Bug Fixes

* **golden-master:** the certifier reads a request in the re-baseline ledger's spelling too; an act already at the base is not an extension ([#765](https://github.com/SocialGouv/iterion/issues/765)) ([21ee24f](https://github.com/SocialGouv/iterion/commit/21ee24f124b3b90519a2f34d391f04857c0e886f))

    <details><summary>why</summary>

    Measured on the first extension request to reach the additions-only verdict in cloud: the request had been written as the ledger's own header taught — `expected_paths` and `entries` (the re-baseline idiom a worker had copied into the header) — while `extension_verdict` read only `paths` and `corpus_entries`. Every conforming request was therefore "smuggled": "1 added entry no acted request claims", "refs/<id>.txt is neither declared in the request's paths nor derived from a claimed corpus…

    </details>

## [3.105.0](https://github.com/SocialGouv/iterion/compare/v3.104.2...v3.105.0) (2026-09-05)

### Features

* **board:** sync a team's GitHub Projects v2 board with the native board — ADR-097 ([#745](https://github.com/SocialGouv/iterion/issues/745)) ([3e991ce](https://github.com/SocialGouv/iterion/commit/3e991ce0ae90714ed0da033bf0847892d45d3017)), references [#2](https://github.com/SocialGouv/iterion/issues/2) [#1](https://github.com/SocialGouv/iterion/issues/1) [SocialGouv/iterion#613](https://github.com/SocialGouv/iterion/issues/613)

    <details><summary>why</summary>

    AGENTS.md makes the Projects v2 board the roadmap truth and the native board the bots' operational surface, but nothing joins them: the board's Status/Area/Mode/Priority live in the GraphQL API, and no seam in iterion speaks GraphQL to a forge. So a human's "In progress" never reaches the dispatcher and a bot's "done" never reaches the roadmap.

    </details>

## [3.104.2](https://github.com/SocialGouv/iterion/compare/v3.104.1...v3.104.2) (2026-09-05)

### Bug Fixes

* **modernize:** every refusal is a verdict the graph fails on; the gate's commit is HEAD plus one line; a timeout fails before any subbot ([#757](https://github.com/SocialGouv/iterion/issues/757)) ([7503e2f](https://github.com/SocialGouv/iterion/commit/7503e2fe5332f645178c0073c5df3368eef56978))

    <details><summary>why</summary>

    Third review round (two medium, three open questions), each reproduced and pinned.

    </details>
* **runtime:** elect a fan-out collector from predecessors inside the fan-out only ([#758](https://github.com/SocialGouv/iterion/issues/758)) ([2f69832](https://github.com/SocialGouv/iterion/commit/2f69832b05a0c5f107e747761d19adc2944e810e)), closes [#741](https://github.com/SocialGouv/iterion/issues/741), references [SocialGouv/iterion#741](https://github.com/SocialGouv/iterion/issues/741) [#741](https://github.com/SocialGouv/iterion/issues/741) [#741](https://github.com/SocialGouv/iterion/issues/741) [#741](https://github.com/SocialGouv/iterion/issues/741) [#741](https://github.com/SocialGouv/iterion/issues/741)

    <details><summary>why</summary>

    A fan-out target that is ALSO reachable from outside the fan-out — the mono/dual topology review-pr and evolve ship, where a `condition` router reaches the same reviewer directly or through the fan_out_all — has two distinct predecessors, and the collector election counted both. The target itself was elected: its branch stopped before executing anything, the sibling ran the whole post-fan-out chain inside its branch, and the trunk then ran the target plus the same chain a second time. Observed…

    </details>
* **sandbox:** the claw bind-mount follows the effective backend; a pod that never got placed parks the run failed_resumable ([#760](https://github.com/SocialGouv/iterion/issues/760)) ([cdcd63b](https://github.com/SocialGouv/iterion/commit/cdcd63b8fdb39187876df034b9112b2fc7d650f2)), closes [#724](https://github.com/SocialGouv/iterion/issues/724) [#699](https://github.com/SocialGouv/iterion/issues/699)

    <details><summary>why</summary>

    `containsClawNode` read each node's declared `backend:` and `fallbacks:` only. Launch-time model/backend overrides (`--backend '*=claw'`, `--model`, the studio override object, `RunMessage.model_overrides`) are applied at dispatch and never folded into the IR, so a workflow of `claude_code` nodes run with `--backend '*=claw'` got no in-container iterion binary: every node died on `exec: /usr/local/bin/iterion: no such file or directory`, and `sandbox_claw_routed_via_runner` stayed silent.

    </details>
* **store/mongo:** New retries a late server selection; the CI replica-set init fails without a PRIMARY ([#754](https://github.com/SocialGouv/iterion/issues/754)) ([d6e7985](https://github.com/SocialGouv/iterion/commit/d6e798548fb2c19268a9a11868e227e434801ea7)), closes [#729](https://github.com/SocialGouv/iterion/issues/729), references [#698](https://github.com/SocialGouv/iterion/issues/698)

    <details><summary>why</summary>

    A fresh client's first server selection can outlast a single 5 s ping on a loaded host while the replica set is healthy — the handshake is late, not the primary absent. New pinged once and turned that lag into a boot failure, and into a conformance harness that ejected a green PR from the merge queue (#729: "rs0 primary elected" printed, then the 5th fresh client saw ReplicaSetNoPrimary and PR #698 was thrown out).

    </details>

## [3.104.1](https://github.com/SocialGouv/iterion/compare/v3.104.0...v3.104.1) (2026-09-05)

### Bug Fixes

* **webhooks:** the GitLab note handler routes every slash command generically — /revi approve and the fork guard reach GitLab ([#753](https://github.com/SocialGouv/iterion/issues/753)) ([6f336f4](https://github.com/SocialGouv/iterion/commit/6f336f4a8f8b5b95bb8d29e557991f303607000d)), closes [#683](https://github.com/SocialGouv/iterion/issues/683)

    <details><summary>why</summary>

    GitLab addresses merge requests as a REST resource separate from issues (an MR and an issue can share the same iid in one project), so CommentIssue's /issues/:iid/notes endpoint would land on, or 404 against, the wrong resource for a caller replying on an MR. Adds the MR-scoped counterpart posting to /merge_requests/:iid/notes, needed by the upcoming GitLab /revi approve reply lane.

    </details>

## [3.104.0](https://github.com/SocialGouv/iterion/compare/v3.103.0...v3.104.0) (2026-09-05)

### Features

* **credentials:** a refresh writes only the tokens, refusals earn a rest and stay visible, and every credential's spend is metered by nature ([#748](https://github.com/SocialGouv/iterion/issues/748)) ([1906ab1](https://github.com/SocialGouv/iterion/commit/1906ab101b2076be8ef66ab03eac5c4a8d72c7fa)), references [#656](https://github.com/SocialGouv/iterion/issues/656) [#629](https://github.com/SocialGouv/iterion/issues/629) [#610](https://github.com/SocialGouv/iterion/issues/610) [#624](https://github.com/SocialGouv/iterion/issues/624) [#629](https://github.com/SocialGouv/iterion/issues/629) [#690](https://github.com/SocialGouv/iterion/issues/690) [#629](https://github.com/SocialGouv/iterion/issues/629) [#629](https://github.com/SocialGouv/iterion/issues/629) [#624](https://github.com/SocialGouv/iterion/issues/624) [#629](https://github.com/SocialGouv/iterion/issues/629) [#641](https://github.com/SocialGouv/iterion/issues/641)

    <details><summary>why</summary>

    The three refresh paths did Get -> RefreshRecord -> whole-record Upsert, so a rename committed between the read and the persist was reverted to the label the refresh had read a provider round trip earlier. The rename side was already a store-level $set (SetAccountLabel); this is its mirror.

    </details>

### Bug Fixes

* **modernize:** the contract is not the worker's to rewrite — typed only_lot refusal, done written by the gate, rewrites refused before the gate ([#734](https://github.com/SocialGouv/iterion/issues/734)) ([a430ddc](https://github.com/SocialGouv/iterion/commit/a430ddcb3c8f948e11149ecd1fdc098d0a3b843c)), references [#737](https://github.com/SocialGouv/iterion/issues/737)

    <details><summary>why</summary>

    Three defects of one class, measured on a live campaign: a run's terminal state lied about what had been proven, and an operator relaunched on the lie.

    </details>

## [3.103.0](https://github.com/SocialGouv/iterion/compare/v3.102.6...v3.103.0) (2026-09-05)

### Features

* **review-pr:** per-repo review tiers — glance / guard / audit; the pure re-request click is pinned; Revi/Billy/Vetty's shared gate documented ([#742](https://github.com/SocialGouv/iterion/issues/742)) ([b2a31a1](https://github.com/SocialGouv/iterion/commit/b2a31a15657d0f6c426fd02a0eefe530b58499cf)), references [#685](https://github.com/SocialGouv/iterion/issues/685) [pre-#685](https://github.com/pre-/issues/685) [#621](https://github.com/SocialGouv/iterion/issues/621) [#650](https://github.com/SocialGouv/iterion/issues/650) [#650](https://github.com/SocialGouv/iterion/issues/650) [#646](https://github.com/SocialGouv/iterion/issues/646) [#683](https://github.com/SocialGouv/iterion/issues/683) [#683](https://github.com/SocialGouv/iterion/issues/683)

    <details><summary>why</summary>

    Lets a repo's criticality or budget policy pick ONE preset instead of tuning severity_threshold/max_findings/post_to_board/review_mode separately. guard (the default) reproduces the bot's pre-#685 posture byte-for-byte; glance trades depth for a lower floor (cheaper same-family reviewer model via two new topology-routed judge nodes, since model:/ reasoning_effort: only ever resolve ${ENV_VAR}, never a runtime var, on any backend); audit forces the cross-family dual fan-out regardless of…

    </details>
* **runner:** wire a SubbotRunner into the cloud runner — subbot nodes ran locally only ([#743](https://github.com/SocialGouv/iterion/issues/743)) ([1716b9e](https://github.com/SocialGouv/iterion/commit/1716b9e266328332bdc5757012da92f544fb61d1))

    <details><summary>why</summary>

    The pod's engine was built without WithSubbotRunner (pkg/runner/loop.go), while the CLI and the studio paths carried one. Every `subbot` node on a cloud run therefore died at dispatch with `subbot "x": no SubbotRunner is wired` — a net's extension subbot, its re-anchor subbot, and a programme supervisor's per-lot child all existed locally only. "No feature ships local-only", violated by the one surface that runs unattended.

    </details>

### Bug Fixes

* **claw:** carry the run's forfait across the sandbox boundary (it currently cannot authenticate there at all) ([#744](https://github.com/SocialGouv/iterion/issues/744)) ([edd5b9d](https://github.com/SocialGouv/iterion/commit/edd5b9dcfee2a2ad3a14501a95831d99a9f51520)), closes [#736](https://github.com/SocialGouv/iterion/issues/736) [#698](https://github.com/SocialGouv/iterion/issues/698), references [#698](https://github.com/SocialGouv/iterion/issues/698) [#687](https://github.com/SocialGouv/iterion/issues/687) [#698](https://github.com/SocialGouv/iterion/issues/698) [#736](https://github.com/SocialGouv/iterion/issues/736) [#687](https://github.com/SocialGouv/iterion/issues/687)

    <details><summary>why</summary>

    in-process generation — supervisor evals, GenerateObjectDirect — which is what fixed #687's pacer. It did not hold for `backend: claw` AGENT nodes on a cloud pod, which are the DEFAULT shape: production runs ITERION_SANDBOX_DEFAULT=auto with an empty override, so those execute sandboxed and the in-container __claw-runner rebuilds its registry from env alone.

    </details>

## [3.102.6](https://github.com/SocialGouv/iterion/compare/v3.102.5...v3.102.6) (2026-09-05)

### Bug Fixes

* **bots:** the plan phase can no longer eat the delivery budget; only_lot on a non-actionable lot fails typed ([#737](https://github.com/SocialGouv/iterion/issues/737)) ([b2b3616](https://github.com/SocialGouv/iterion/commit/b2b3616b03b67c6d9e30f47646cce35b5dcfb882)), references [iterion#683](https://github.com/iterion/issues/683)

    <details><summary>why</summary>

    Two production runs on iterion#683 spent the whole planning chain (plan -> plan_review -> plan_revise) for 150 min / $8.59 combined and never reached campaign, the node that writes code (native:695). The chain had no ceiling of its own, so it could freely spend the entire run's max_duration / max_cost_usd budget on an optional enrichment (ADR-091) before the actual delivery work ever started.

    </details>
* **golden-master:** an extension act already present at base is not re-judged ([#735](https://github.com/SocialGouv/iterion/issues/735)) ([292e3f6](https://github.com/SocialGouv/iterion/commit/292e3f65a577a46b05096ebc7f14aa02e1950726))

    <details><summary>why</summary>

    extension_verdict judged EVERY act in the ledger at HEAD against the run's base. An act introduced before that base has, by construction, its added references in the base tree — so every one of them read as "existed at base — a rewrite wearing an addition's name", and the certifier refused a net that had done nothing wrong.

    </details>
* **quota:** a reading is trusted for a bounded time, the retry waits for the nearest key, the ceiling counts spenders, the stamp is visible ([#730](https://github.com/SocialGouv/iterion/issues/730)) ([f25d224](https://github.com/SocialGouv/iterion/commit/f25d2240408b0f96a41988adf8032977aa89ec81)), references [#690](https://github.com/SocialGouv/iterion/issues/690) [#684](https://github.com/SocialGouv/iterion/issues/684) [#661](https://github.com/SocialGouv/iterion/issues/661) [#659](https://github.com/SocialGouv/iterion/issues/659) [#659](https://github.com/SocialGouv/iterion/issues/659) [#690](https://github.com/SocialGouv/iterion/issues/690) [#690](https://github.com/SocialGouv/iterion/issues/690) [#684](https://github.com/SocialGouv/iterion/issues/684) [#661](https://github.com/SocialGouv/iterion/issues/661) [#659](https://github.com/SocialGouv/iterion/issues/659) [#690](https://github.com/SocialGouv/iterion/issues/690)

    <details><summary>why</summary>

    A reading carrying a reset instant was trusted until that instant, however old. The provider resets windows early: on 2026-09-04 the ledger held 93-99% seven_day readings taken before such a reset, every credential walk skipped both forfaits on them and every claude_code run was refused at admission — the revi/review gate of two PRs with them — for a reset four days out. The lock was self-sustaining: the only writer of a fresh reading is a live session's rate_limit_event, and the refusal is…

    </details>

## [3.102.5](https://github.com/SocialGouv/iterion/compare/v3.102.4...v3.102.5) (2026-09-05)

### Bug Fixes

* **webhooks:** the gate lanes answer, refuse and stay same-repo ([#683](https://github.com/SocialGouv/iterion/issues/683)) ([c0a55fc](https://github.com/SocialGouv/iterion/commit/c0a55fc5c4216a56083555f2e6b2c7f312fb81bf)), closes [#663](https://github.com/SocialGouv/iterion/issues/663) [#639](https://github.com/SocialGouv/iterion/issues/639) [#662](https://github.com/SocialGouv/iterion/issues/662), references [#642](https://github.com/SocialGouv/iterion/issues/642) [#642](https://github.com/SocialGouv/iterion/issues/642) [#646](https://github.com/SocialGouv/iterion/issues/646) [#663](https://github.com/SocialGouv/iterion/issues/663) [#650](https://github.com/SocialGouv/iterion/issues/650) [#646](https://github.com/SocialGouv/iterion/issues/646) [#650](https://github.com/SocialGouv/iterion/issues/650) [#650](https://github.com/SocialGouv/iterion/issues/650) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662) [#663](https://github.com/SocialGouv/iterion/issues/663) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662) [#650](https://github.com/SocialGouv/iterion/issues/650) [#652](https://github.com/SocialGouv/iterion/issues/652) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662) [#663](https://github.com/SocialGouv/iterion/issues/663) [#650](https://github.com/SocialGouv/iterion/issues/650) [#650](https://github.com/SocialGouv/iterion/issues/650) [#662](https://github.com/SocialGouv/iterion/issues/662) [#663](https://github.com/SocialGouv/iterion/issues/663) [#663](https://github.com/SocialGouv/iterion/issues/663) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662) [#662](https://github.com/SocialGouv/iterion/issues/662)

    <details><summary>why</summary>

    Class surfaced by Revi on #626, fixed there for the review-thread reply lane only. handlePRForgeComment resolves the PR through GetPullRequest → forge.PullRef, then launches with the base repo's CloneURL + the PR head ref — a ref that lives in the head repo on a fork. The checkout misses, or hits a same-named branch on the base and the bot answers grounded in the wrong code, under the bot identity.

    </details>

## [3.102.4](https://github.com/SocialGouv/iterion/compare/v3.102.3...v3.102.4) (2026-09-05)

### Bug Fixes

* **claw:** an Anthropic forfait authenticates claw, on the pod and on disk ([#698](https://github.com/SocialGouv/iterion/issues/698)) ([7d8d81c](https://github.com/SocialGouv/iterion/commit/7d8d81cd502b583c2237ebe041c649dbd983294b)), references [#687](https://github.com/SocialGouv/iterion/issues/687) [#687](https://github.com/SocialGouv/iterion/issues/687) [#687](https://github.com/SocialGouv/iterion/issues/687) [#687](https://github.com/SocialGouv/iterion/issues/687) [#687](https://github.com/SocialGouv/iterion/issues/687)

    <details><summary>why</summary>

    A run whose only anthropic credential is a Claude Code OAuth forfait resolved an UNAUTHENTICATED claw client: non-nil, so callers proceeded, and every call answered 401 "x-api-key header is required". Revi's pacer supervisor died this way in prod for a full day (#687) while its unit tests stayed green — a clean failure is a feature you believe you shipped.

    </details>

## [3.102.3](https://github.com/SocialGouv/iterion/compare/v3.102.2...v3.102.3) (2026-09-05)

### Bug Fixes

* **board:** the cloud twin claims launches atomically, the reaper gives up on a pruned run, labels adjust relatively, GitHub claims bootstrap their label ([#731](https://github.com/SocialGouv/iterion/issues/731)) ([53f5635](https://github.com/SocialGouv/iterion/commit/53f5635167d0593dae2ec7f12d33dc5f535ac689)), references [#665](https://github.com/SocialGouv/iterion/issues/665) [#660](https://github.com/SocialGouv/iterion/issues/660) [#660](https://github.com/SocialGouv/iterion/issues/660) [#667](https://github.com/SocialGouv/iterion/issues/667) [#666](https://github.com/SocialGouv/iterion/issues/666)

    <details><summary>why</summary>

    The studio's pipeline admission loop launches a Ready card through native.LaunchClaimer — a CAS Ready → in_progress that also reads the claim family, because the dispatcher wins a card with the CLAIM and moves it to in_progress afterwards, off the actor. The Mongo twin never implemented it, so on a cloud board the admission loop degraded to a best-effort SetState: replica A's board dispatcher claims a Ready card (lease live, in_progress move in flight) while replica B's admission tick launches…

    </details>
* **engine:** a recovery pause retries its node, an orchestration stall recovers in place, and a run read never writes run.json back ([#716](https://github.com/SocialGouv/iterion/issues/716)) ([9c3ab3d](https://github.com/SocialGouv/iterion/commit/9c3ab3d51b25469098c3ba007faf11faee75cb75)), references [#688](https://github.com/SocialGouv/iterion/issues/688) [#692](https://github.com/SocialGouv/iterion/issues/692) [#692](https://github.com/SocialGouv/iterion/issues/692) [#691](https://github.com/SocialGouv/iterion/issues/691) [#691](https://github.com/SocialGouv/iterion/issues/691)

    <details><summary>why</summary>

    The recovery dispatcher parks a FAILED node for a human (AUTH_FAILED, budget, any RecoveryPauseForHuman policy) through the plain human-pause path, with an empty pauseInfo. The checkpoint therefore carried no BackendName, and resumeFromPause has exactly one branch that re-executes the paused node — the delegate pause, keyed on BackendName. A recovery pause fell through to the human path: the acknowledgement became the node's OUTPUT, node_finished was emitted, and the DAG moved on to the gate,…

    </details>
* **sandbox/kubernetes:** the pod-ready wait is configurable and defaults to 10 min — a 180 s cap killed runs the autoscaler had just made room for ([#707](https://github.com/SocialGouv/iterion/issues/707)) ([70da3d9](https://github.com/SocialGouv/iterion/commit/70da3d9c249000d420ae573526d502ed23a8530f)), closes [#696](https://github.com/SocialGouv/iterion/issues/696), references [#694](https://github.com/SocialGouv/iterion/issues/694)

    <details><summary>why</summary>

    Measured on a 12-node cluster once run pods carry requests (#694): with ten run pods the scheduler answered "0/12 nodes are available: 11 Insufficient cpu" for two minutes, the cluster autoscaler added a worker, the fresh node's CNI took a few seconds, the 736 MB sandbox image pulled in 1m37 — and the driver, which had given up at 180 s, killed the container one second after it started. Cold pulls alone measured 2m25 to 3m05 on other nodes; one run died on that too. Closes #696.

    </details>

## [3.102.2](https://github.com/SocialGouv/iterion/compare/v3.102.1...v3.102.2) (2026-09-05)

### Bug Fixes

* **launch:** the launch path survives one team's broken plugin, strands no row, retries with a budget, commits any message ([#709](https://github.com/SocialGouv/iterion/issues/709)) ([ad5405e](https://github.com/SocialGouv/iterion/commit/ad5405ee5f9025a04e1fb9214b8cb06560df0077)), references [#631](https://github.com/SocialGouv/iterion/issues/631) [#537](https://github.com/SocialGouv/iterion/issues/537) [#536](https://github.com/SocialGouv/iterion/issues/536) [#538](https://github.com/SocialGouv/iterion/issues/538)

    <details><summary>why</summary>

    A converged run's squash merge failed with "fork/exec /usr/bin/git: argument list too long": the assembled message was passed as one `-m` argument, and Linux caps a single argv element at 128 KiB (MAX_ARG_STRLEN), so exactly the runs that produced the most could not be merged (#631).

    </details>

## [3.102.1](https://github.com/SocialGouv/iterion/compare/v3.102.0...v3.102.1) (2026-09-05)

### Bug Fixes

* **webhooks:** a merge-queue heal stands down when the queue takes the PR back ([#693](https://github.com/SocialGouv/iterion/issues/693)) ([1b1009f](https://github.com/SocialGouv/iterion/commit/1b1009f0a6319ffd8ab491e6fd6b7ee10f3f5653)), references [iterion#682](https://github.com/iterion/issues/682) [#692](https://github.com/SocialGouv/iterion/issues/692)

    <details><summary>why</summary>

    The auto-heal lane launches the brancher bot when the queue ejects a PR, to rebase and push so the PR re-enters the queue. Nothing closed that loop: once the PR WAS back in the queue, the heal kept running, and its delivery tail force-pushes the branch — which cancels the queue build in flight and ejects the PR a second time. The repair becomes the next breakage.

    </details>

## [3.102.0](https://github.com/SocialGouv/iterion/compare/v3.101.3...v3.102.0) (2026-09-04)

### Features

* **sandbox/kubernetes:** run pods carry resource requests and a soft node spread ([#694](https://github.com/SocialGouv/iterion/issues/694)) ([8243001](https://github.com/SocialGouv/iterion/commit/8243001d56d18f00bc85ef8569f765e54c73384c))

    <details><summary>why</summary>

    A sibling pod that requests nothing scores every node the same, so the scheduler packs a campaign's runs onto whichever node already holds the sandbox image. Measured on a three-worker pool (8 cores each): five of six run pods on one node at 89% CPU while two workers idled, and a behavioural oracle's 300 s application boot budget blown at 459 s — red verdicts on untouched trees, one run burning its 8 h budget on that red.

    </details>

## [3.101.3](https://github.com/SocialGouv/iterion/compare/v3.101.2...v3.101.3) (2026-09-04)

### Bug Fixes

* **runner:** a stalled resume is adopted, re-budgeted and told apart from a quota pause ([#689](https://github.com/SocialGouv/iterion/issues/689)) ([9f7bc53](https://github.com/SocialGouv/iterion/commit/9f7bc5391a6e157b3d16fe49a3f3e958c0de1838)), references [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#652](https://github.com/SocialGouv/iterion/issues/652) [#669](https://github.com/SocialGouv/iterion/issues/669) [652/#669](https://github.com/SocialGouv/iterion/issues/669) [#652](https://github.com/SocialGouv/iterion/issues/652) [#669](https://github.com/SocialGouv/iterion/issues/669) [#652](https://github.com/SocialGouv/iterion/issues/652) [#652](https://github.com/SocialGouv/iterion/issues/652) [#652](https://github.com/SocialGouv/iterion/issues/652) [#652](https://github.com/SocialGouv/iterion/issues/652) [#652](https://github.com/SocialGouv/iterion/issues/652) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [#669](https://github.com/SocialGouv/iterion/issues/669) [resume#1](https://github.com/resume/issues/1) [resume#2](https://github.com/resume/issues/2)

    <details><summary>why</summary>

    A DLQ-parked run reached noticeGatePausedForRetry with the same RetryState the pre-park usage-window carried, so the PR got a second "the LLM provider's quota is exhausted … resume automatically at HH:MM" — but nothing wakes a DLQ-parked run automatically; only `iterion remote admin dlq` replays it. Observed live 2026-09-03: the comment sent the operator down the wrong path.

    </details>

## [3.101.2](https://github.com/SocialGouv/iterion/compare/v3.101.1...v3.101.2) (2026-09-04)

### Bug Fixes

* **credentials:** the wants a run actually needs, and a refusal that says why ([#682](https://github.com/SocialGouv/iterion/issues/682)) ([d2f7cee](https://github.com/SocialGouv/iterion/commit/d2f7cee5c54d1f02f7b1721899f5dda238ca3557)), references [#627](https://github.com/SocialGouv/iterion/issues/627) [#668](https://github.com/SocialGouv/iterion/issues/668) [#654](https://github.com/SocialGouv/iterion/issues/654) [#668](https://github.com/SocialGouv/iterion/issues/668) [#668](https://github.com/SocialGouv/iterion/issues/668) [#659](https://github.com/SocialGouv/iterion/issues/659) [#659](https://github.com/SocialGouv/iterion/issues/659) [#668](https://github.com/SocialGouv/iterion/issues/668) [#668](https://github.com/SocialGouv/iterion/issues/668) [#659](https://github.com/SocialGouv/iterion/issues/659) [#627](https://github.com/SocialGouv/iterion/issues/627) [#654](https://github.com/SocialGouv/iterion/issues/654) [#659](https://github.com/SocialGouv/iterion/issues/659) [#668](https://github.com/SocialGouv/iterion/issues/668) [#654](https://github.com/SocialGouv/iterion/issues/654) [#627](https://github.com/SocialGouv/iterion/issues/627)

    <details><summary>why</summary>

    Two paid failures in prod, hours apart, were accepted silently by the server: a claude_code accessToken pasted as a terminal transcript (embedded newlines/ANSI escapes) and a claude_code record missing expiresAt/scopes — the shape the CLI reads as "Not logged in". Both lived on disk after ingestion, so every downstream call from a full fleet of runs would die on 401 for hours before the cause was found.

    </details>

## [3.101.1](https://github.com/SocialGouv/iterion/compare/v3.101.0...v3.101.1) (2026-09-04)

### Bug Fixes

* **docs-refresh:** 3.5.7 — scan_hints stops manufacturing the same false positives every pass ([#680](https://github.com/SocialGouv/iterion/issues/680)) ([e1786cc](https://github.com/SocialGouv/iterion/commit/e1786cc3bc48bce89ac3ea957f463543bbce5da2))

    <details><summary>why</summary>

    A link written /dsl is SITE-absolute: every static-site generator routes it from the site root. os.path.join drops everything left of an absolute path, so the scanner asked the filesystem for /dsl and reported it dead. On prod run 01a055f9 that was 20 of the 22 hints, every one false, and the remaining 2 were explicit HTML anchors the heading-only collector could not see.

    </details>

## [3.101.0](https://github.com/SocialGouv/iterion/compare/v3.100.3...v3.101.0) (2026-09-04)

### Features

* **product-docs:** 1.1.0 — platform-agnostic publish tail (image + deploy-target skill) ([#675](https://github.com/SocialGouv/iterion/issues/675)) ([ad5d29b](https://github.com/SocialGouv/iterion/commit/ad5d29bd909f37131817227ff10543a57be0e622))

    <details><summary>why</summary>

    The publish tail spoke one platform: the SSP Cloud datalab (three S3 STS secrets with a 7-day life and a standing serve service), which let the demo URL die by itself and put platform literals in the DSL. It now mirrors app-dev's deploy phase: the bundle's own publish-static-site skill builds the site and packages it with crane as one layer on a non-root nginx base pushed to publish_image:<docs commit> (registry_token on stdin, never argv), and the operator-attached deploy-target skill puts…

    </details>

## [3.100.3](https://github.com/SocialGouv/iterion/compare/v3.100.2...v3.100.3) (2026-09-04)

### Bug Fixes

* **cloudpublisher:** the walk consults the operator's hard caps — a capped forfait is passed over like a refused one ([#678](https://github.com/SocialGouv/iterion/issues/678)) ([aa91cb8](https://github.com/SocialGouv/iterion/commit/aa91cb8e705131e590345ce759e4db0b6b482ad1)), closes [#677](https://github.com/SocialGouv/iterion/issues/677)

    <details><summary>why</summary>

    Measured 2026-09-04 on a production tenant: a weekly forfait at 97% utilization (provider still ALLOWING, hard cap 95, reset four days out) was granted by the resolution on four consecutive retry attempts; the runner's pre-flight parked each one in ~1s, and because a park writes no refusal reading, no signal ever engaged the credential-tier fallback — a fresh org-tier forfait at ~0% sat unreached the whole time. Two closures:

    </details>

## [3.100.2](https://github.com/SocialGouv/iterion/compare/v3.100.1...v3.100.2) (2026-09-04)

### Bug Fixes

* **runner:** a re-execution restores the chain its earlier attempt banked ([#674](https://github.com/SocialGouv/iterion/issues/674)) ([ea2b274](https://github.com/SocialGouv/iterion/commit/ea2b274811a6f820200109bb8782838e162106a8)), closes [#652](https://github.com/SocialGouv/iterion/issues/652), references [#652](https://github.com/SocialGouv/iterion/issues/652)

    <details><summary>why</summary>

    A resume or redelivery of a repo-targeted run re-cloned the target branch and started the remaining nodes on a bare tree: the commits the earlier attempt had banked (final_branch/final_commit) or parked (an attempt ref) stayed reachable on the forge but never reached the workspace, so a campaign that had committed three passes planned its fourth on nothing and the PR tail had nothing to deliver — docs-refresh 01a055f9 (33 commits) and Billy 01a06728 (#652).

    </details>

## [3.100.1](https://github.com/SocialGouv/iterion/compare/v3.100.0...v3.100.1) (2026-09-04)

### Bug Fixes

* **forge:** a re-provision keeps a schedule row's id, vars and last fire; Doki 3.5.6 declares its schedule vars ([#673](https://github.com/SocialGouv/iterion/issues/673)) ([7cbf526](https://github.com/SocialGouv/iterion/commit/7cbf5262bc346ff579ee8c4afb4078f8dd7aaadd))

    <details><summary>why</summary>

    syncSchedules rebuilds an integration's schedule rows from the manifests on every re-provision and carried cron, pause and guard tuning over — but not the vars the operator set, nor last_fire_at, and it minted a new id each time. The docs-refresh weekly lost its open_mr/mode twice that way (07-27, 09-03: the second time from a re-provision that enabled another bot), ran a multi-hour full sweep and shipped nothing, while runs and audit entries kept pointing at a schedule id that no longer…

    </details>

## [3.100.0](https://github.com/SocialGouv/iterion/compare/v3.99.1...v3.100.0) (2026-09-04)

### Features

* **review:** Revi cost-reduction pass — severity floor at the source, pacer supervisor, live cost signal, push debounce ([#651](https://github.com/SocialGouv/iterion/issues/651)) ([2bd3bbc](https://github.com/SocialGouv/iterion/commit/2bd3bbca3ed9a66fd8b3f1c5cba2e7c9e3066e4b)), references [acme/b#7](https://github.com/acme/b/issues/7) [acme/a#7](https://github.com/acme/a/issues/7) [acme/b#7](https://github.com/acme/b/issues/7) [acme/a#7](https://github.com/acme/a/issues/7)

    <details><summary>why</summary>

    Measured in prod (2026-09-03, 9-run sample): reviewer_claude (opus/high) is 85-93% of a $1.4-$10.5 run (median $3.6), ~28 runs in 3h. The reviewer's judging quality is untouched (stays opus/high); the waste is cut around it:

    </details>

## [3.99.1](https://github.com/SocialGouv/iterion/compare/v3.99.0...v3.99.1) (2026-09-04)

### Bug Fixes

* **oauth:** pre-fill the account name on reconnect, cap the label, and publish the team OAuth routes in the spec ([#657](https://github.com/SocialGouv/iterion/issues/657)) ([ed85bf6](https://github.com/SocialGouv/iterion/commit/ed85bf6c8379de308cc00f5a184b5f6b9b51d44d)), references [#656](https://github.com/SocialGouv/iterion/issues/656)

    <details><summary>why</summary>

    Follow-ups from the second review round of #653:

    </details>

## [3.99.0](https://github.com/SocialGouv/iterion/compare/v3.98.0...v3.99.0) (2026-09-03)

### Features

* **board:** claim lease + fenced watchdog for the dispatcher board (ADR-096) ([#646](https://github.com/SocialGouv/iterion/issues/646)) ([e194aeb](https://github.com/SocialGouv/iterion/commit/e194aebe09f91063799bc2d3477b50e397d7d513)), references [#597](https://github.com/SocialGouv/iterion/issues/597)

    <details><summary>why</summary>

    Commit 0 of the dispatcher-watchdog chantier (C1 slice 2/3) — the prerequisite the plan review made non-negotiable (F3/F25):

    </details>

## [3.98.0](https://github.com/SocialGouv/iterion/compare/v3.97.0...v3.98.0) (2026-09-03)

### Features

* **oauth:** name the account behind a credential, and expose its fingerprint ([#653](https://github.com/SocialGouv/iterion/issues/653)) ([5e27183](https://github.com/SocialGouv/iterion/commit/5e2718395fbe11b756cabcd46c9546893506a362))

    <details><summary>why</summary>

    Nothing on an OAuth record said WHOSE account it was. The payload is sealed, the API view exposed neither a label nor the fingerprint, and the only place the credential is ever identified is a server log line the publisher writes when it picks one (fp=700acc7b…). Answering "whose subscription paid for that run?" therefore meant grepping logs and correlating hex by hand — measured today, with three fingerprints across three owners in one window.

    </details>

### Bug Fixes

* **mcp:** make the explicitly-named wildcard's fatal boot a decision, not an accident ([#645](https://github.com/SocialGouv/iterion/issues/645)) ([fd3f89b](https://github.com/SocialGouv/iterion/commit/fd3f89b5019894297cdc1711c5ed47d1a309a3d2)), closes [#638](https://github.com/SocialGouv/iterion/issues/638), references [#633](https://github.com/SocialGouv/iterion/issues/633) [#633](https://github.com/SocialGouv/iterion/issues/633)

    <details><summary>why</summary>

    Two lines apart, expandWildcards hard-fails a wildcard whose MCP server cannot boot and merely warns when that server booted with no tools. The asymmetry is correct — #633 established that ambient servers (target repo .mcp.json, plugin catalog) degrade per-server upstream in buildTask, so a wildcard reaching this function is a DECLARED dependency and must never be dropped in silence — but nothing at the code site said so, and the error named neither the declaration nor a way out. A reader had…

    </details>

## [3.97.0](https://github.com/SocialGouv/iterion/compare/v3.96.2...v3.97.0) (2026-09-03)

### Features

* **ir:** a codex fallback stage is refused at launch where the node will run sandboxed ([#648](https://github.com/SocialGouv/iterion/issues/648)) ([c820b54](https://github.com/SocialGouv/iterion/commit/c820b5445e60b75456eb962f4a04f33fda7d7ce1))

    <details><summary>why</summary>

    The codex CLI hard-errors on any non-noop sandbox driver at dispatch — so a codex stage on a sandboxed node fails EXACTLY when the fallback chain is needed, which is worse than not having it. ApplyRunFallback now resolves each node's effective sandbox mode (node override → workflow spec → the deployment's ITERION_SANDBOX_DEFAULT snapshot, threaded through ExecutorSpec from both the cloud runner and the local service) and refuses the stage with the same launch-time voice as the other C176…

    </details>

## [3.96.2](https://github.com/SocialGouv/iterion/compare/v3.96.1...v3.96.2) (2026-09-03)

### Bug Fixes

* **forge:** refresh lead must exceed the tick period, or a token phase-locks onto its own expiry ([#649](https://github.com/SocialGouv/iterion/issues/649)) ([1417654](https://github.com/SocialGouv/iterion/commit/1417654fbb946051c352377dc3aa95cc6eab5d8b))

    <details><summary>why</summary>

    The refresh worker only renews connections expiring within Lead (was 5m), swept every 10m: a 1h installation token could go from "not yet due" to "expired" between two ticks, the refresh then landing AT expiry — and since each mint inherits that phase, the connection locks onto an always-refreshed-at-death cycle. Any run whose launch minute sits just before the lock point is sealed a token with seconds of life: its clone barely makes it, its state push and NATS redeliveries die on it.

    </details>

## [3.96.1](https://github.com/SocialGouv/iterion/compare/v3.96.0...v3.96.1) (2026-09-03)

### Bug Fixes

* **quota:** classify an account spend ceiling, tell the PR when a review parks, stop runs on a closed PR ([#639](https://github.com/SocialGouv/iterion/issues/639)) ([cca3d20](https://github.com/SocialGouv/iterion/commit/cca3d20d524d73de9b44e069a9a2333ddb0bfce8)), references [other/#7](https://github.com/SocialGouv/iterion/issues/7) [widgets/#7](https://github.com/SocialGouv/iterion/issues/7)

    <details><summary>why</summary>

    "You've hit your org's monthly spend limit · ask your admin to raise it at claude.ai/settings/usage" put THREE words and an apostrophe between "your" and "limit"; the qualifier pattern tolerated exactly one word without one, so the notice sailed through as the node's answer and died as "structured output invalid: missing required field …" — the precise masking bug the pattern's own comment was written for, re-opened by a new wording (three branch-improve-loop runs on 2026-09-03, runs 01a06694 /…

    </details>

## [3.96.0](https://github.com/SocialGouv/iterion/compare/v3.95.0...v3.96.0) (2026-09-03)

### Features

* **secrets:** per-key concurrency ceiling — the operator's answer to fair-usage limits no provider will quantify ([#640](https://github.com/SocialGouv/iterion/issues/640)) ([b7e8402](https://github.com/SocialGouv/iterion/commit/b7e8402b601a085c5f3d479cb6f5b784f7cd7801))

    <details><summary>why</summary>

    A provider that freezes an account for 'usage pattern' violations publishes NO numeric bound to adapt to: the refusal message names no frequency, the 200s carry no rate headers, and the threshold only reveals itself by tripping it — which is how a whole fleet ended up frozen behind one credential. The bound has to be operator-set.

    </details>

## [3.95.0](https://github.com/SocialGouv/iterion/compare/v3.94.3...v3.95.0) (2026-09-03)

### Features

* Revi ticket conformance (Jira & co) + org governance (provision approval, delegated caps) ([#630](https://github.com/SocialGouv/iterion/issues/630)) ([cf216f2](https://github.com/SocialGouv/iterion/commit/cf216f2c720e0d26b3f09ac0d7e7985e61203347))

    <details><summary>why</summary>

    Given a tracker_api_base (pinned per repo via the integration's launch_vars) and a bound read-only tracker_token file secret, the reviewers fetch the ticket(s) the PR references — explicit ticket_refs or extracted from the PR title/body + source branch — and verify the diff delivers the demand. Gaps surface as findings of the new "requirements" category (gating like any finding); a per-ticket verdict (covered / partial / not covered / unverifiable) is threaded converge -> pr_gate -> publish…

    </details>

## [3.94.3](https://github.com/SocialGouv/iterion/compare/v3.94.2...v3.94.3) (2026-09-03)

### Bug Fixes

* **chart:** let image.digest pin the server, so one ReplicaSet is one build ([#637](https://github.com/SocialGouv/iterion/issues/637)) ([5334b2d](https://github.com/SocialGouv/iterion/commit/5334b2d5b0708deea52c50af4035776e517a2fbc)), closes [#636](https://github.com/SocialGouv/iterion/issues/636)

    <details><summary>why</summary>

    `iterion.image` could only build `repo:tag`, so "pin the server by digest" was not expressible at all — the operator's only lever was a tag, which is the thing that moves. `runner.image` is consumed verbatim and could already carry a digest; the shared server image had no equivalent seam.

    </details>

## [3.94.2](https://github.com/SocialGouv/iterion/compare/v3.94.1...v3.94.2) (2026-09-03)

### Bug Fixes

* **review-pr:** raise max_duration 45m → 90m ([#634](https://github.com/SocialGouv/iterion/issues/634)) ([3985c81](https://github.com/SocialGouv/iterion/commit/3985c81815bf06631801ca3c75ed50295657c0ad))

    <details><summary>why</summary>

    The cost cap (max_cost_usd) is the money guard; the duration cap only needs to catch a genuinely hung run. At 45m it killed real reviews mid-publish — a large diff or a usage-window retry walks past it while spending nothing, and the verdict a gate is waiting on dies with the run (observed 2026-09-02, round 7 of the pilot: converge refused at 92% of the duration axis, the review completed everywhere but the publish).

    </details>

## [3.94.1](https://github.com/SocialGouv/iterion/compare/v3.94.0...v3.94.1) (2026-09-03)

### Bug Fixes

* **mcp:** an ambient MCP server that cannot boot costs its tools, never the run ([#633](https://github.com/SocialGouv/iterion/issues/633)) ([2eedd4d](https://github.com/SocialGouv/iterion/commit/2eedd4db304da5241e07b460f4d4fadd66178bfe))

    <details><summary>why</summary>

    The claw splice added every active MCP server — including servers the node never named, inherited from the target repo's .mcp.json or the plugin catalog — as an mcp.<server>.* wildcard, and expandWildcards hard-failed the node when one of them could not boot. One token-less repo server (the repo-scoped sentry on runner pods, which have no SENTRY_ACCESS_TOKEN) therefore killed every claw node of a run at plan_review — observed 3× on 2026-09-02, neutralising the zero-touch fixer lane on this repo…

    </details>

## [3.94.0](https://github.com/SocialGouv/iterion/compare/v3.93.0...v3.94.0) (2026-09-02)

### Features

* add generation-aware zero-interruption rollouts ([#628](https://github.com/SocialGouv/iterion/issues/628)) ([53c4b61](https://github.com/SocialGouv/iterion/commit/53c4b61e5aade022efc92239710e6d498cf056ba))

## [3.93.0](https://github.com/SocialGouv/iterion/compare/v3.92.0...v3.93.0) (2026-09-02)

### Features

* **webhooks:** GitHub review-thread conversations — reply to a suggestion, get an in-thread answer ([#626](https://github.com/SocialGouv/iterion/issues/626)) ([d4611df](https://github.com/SocialGouv/iterion/commit/d4611df0774ec14480167be9a4b8c5f69b0985b9))

    <details><summary>why</summary>

    Replying inside one of the bot's review threads on GitHub now launches the converse bot (roleBots().ReviConverse), which answers in the same thread — the GitHub half of the GitLab conversational lane (forge-conversations.md).

    </details>

## [3.92.0](https://github.com/SocialGouv/iterion/compare/v3.91.0...v3.92.0) (2026-09-02)

### Features

* **runtime:** support branch-local bounded loops ([#557](https://github.com/SocialGouv/iterion/issues/557)) ([c121be6](https://github.com/SocialGouv/iterion/commit/c121be668848689d6421984e8912b185872a1ab0))

    <details><summary>why</summary>

    The answered branch closed its siblings' resume barrier only through checkpointResumedBranch (the happy path) and the panic recovers. Any other exit between consuming ResumeAnswers and landing the successor cursor — an artifact write failure, an answer satisfying no outgoing edge, a C245/unknown-node guard after an edited source — left the barrier open; under best_effort nothing cancels the siblings and collectBranches only arms its grace timer after a cancellation, so the fan-out hung until…

    </details>

## [3.91.0](https://github.com/SocialGouv/iterion/compare/v3.90.0...v3.91.0) (2026-09-02)

### Features

* **usagecap:** an auth-rejected credential becomes skip evidence — the third refusal family ([#624](https://github.com/SocialGouv/iterion/issues/624)) ([b7f2ef0](https://github.com/SocialGouv/iterion/commit/b7f2ef00eccb51a84ac49a3c414504907a49c154))

    <details><summary>why</summary>

    Re-resolution is already universal server-side (SubmitResume and the retry sweeper both re-resolve), yet a structurally-broken credential kept condemning run after run: it filled its slot on every resolution, gated the pool and platform tiers off, and its failure produced NO evidence for the credential-tier skip to act on. Quota refusals have a family, frequency refusals have a family — the provider rejecting the credential ITSELF had none.

    </details>

## [3.90.0](https://github.com/SocialGouv/iterion/compare/v3.89.0...v3.90.0) (2026-09-02)

### Features

* **secrets:** several keys of one provider become an ordered fallback chain ([#612](https://github.com/SocialGouv/iterion/issues/612)) ([7ff4007](https://github.com/SocialGouv/iterion/commit/7ff4007c06ed7db256bbe824148e03d42735f89d))

    <details><summary>why</summary>

    Completes the credential chain the fair-usage freeze exposed: the BYOK tier resolved ONE fixed key per provider, so a key whose account the provider froze was resealed into every fresh launch until an operator removed it by hand (measured 2026-09-02: two removals, five cancels, three relaunch waves).

    </details>

## [3.89.0](https://github.com/SocialGouv/iterion/compare/v3.88.0...v3.89.0) (2026-09-02)

### Features

* **webhooks:** an explicitly named review identity lights the re-request lane on GitHub ([#605](https://github.com/SocialGouv/iterion/issues/605)) ([9ff26bc](https://github.com/SocialGouv/iterion/commit/9ff26bc47c2bc746911c7de1013c8bc3a7b4afce)), references [#604](https://github.com/SocialGouv/iterion/issues/604) [#608](https://github.com/SocialGouv/iterion/issues/608) [#608](https://github.com/SocialGouv/iterion/issues/608)

    <details><summary>why</summary>

    Rebuilt as one commit on main after #604 merged and #608 landed the GitHub/Forgejo replier gate there (convergent fix — both Revi loops demanded it); this is the remaining delta. Four Revi rounds on the PR (3→3→5→0 findings, all real) shaped it; every behaviour guard is mutation-verified.

    </details>

## [3.88.0](https://github.com/SocialGouv/iterion/compare/v3.87.0...v3.88.0) (2026-09-02)

### Features

* **cli:** a command to stand a credential pool up, and a forfait blob that fails where the file name is known ([#600](https://github.com/SocialGouv/iterion/issues/600)) ([d87ff60](https://github.com/SocialGouv/iterion/commit/d87ff608f6c47d2a3a6e935f415b358a0312739e))

    <details><summary>why</summary>

    Two gaps found while actually setting a pool up on a live instance:

    </details>

### Bug Fixes

* **delegate:** a fair-usage refusal parks the run and feeds the credential skip — not the node's answer ([#610](https://github.com/SocialGouv/iterion/issues/610)) ([dade12e](https://github.com/SocialGouv/iterion/commit/dade12e7626a94ee8af8c7a103470711f5afe27e))

    <details><summary>why</summary>

    Measured 2026-09-02: a provider account under a fair-usage frequency restriction refused EVERY request with a ~330-char relayed 429. The one-liner length cap kept isRateLimitMessage from seeing it, so the refusal text became the agent's output (a campaign node 'finished' with the error as its work_remaining); two modernize lots burned ~7h each overnight and two rites spun full passes on it in minutes.

    </details>

## [3.87.0](https://github.com/SocialGouv/iterion/compare/v3.86.1...v3.87.0) (2026-09-02)

### Features

* **cloudpublisher:** skip a forfait whose window is closed — the credential tiers become a fallback chain ([#601](https://github.com/SocialGouv/iterion/issues/601)) ([e963626](https://github.com/SocialGouv/iterion/commit/e9636266188c27de7056b5de4e9b219c8baef122))

    <details><summary>why</summary>

    The tiers were a fixed first choice, not a chain: a run whose tenant (or the platform) holds an OAuth forfait was never eligible for any later tier, INCLUDING when that forfait's provider window was closed. The run got the exhausted credential, spent one LLM call to be refused, and parked until the window reset - up to a week on the weekly one - while a second forfait or the mutualised pool could have served it immediately.

    </details>

## [3.86.1](https://github.com/SocialGouv/iterion/compare/v3.86.0...v3.86.1) (2026-09-01)

### Bug Fixes

* **webhooks:** the re-request replier gate reaches the prforge lane; an authz error never strands a co-riding resync ([#608](https://github.com/SocialGouv/iterion/issues/608)) ([524dabf](https://github.com/SocialGouv/iterion/commit/524dabf83c77fc858968985cccac51f93abad4e8))

    <details><summary>why</summary>

    TestBudgetGraceCoversDuration slept 350ms against a 570ms graced ceiling, leaving ~220ms for engine overhead — a loaded CI runner spends more than that and the run correctly dies past the ceiling, failing the test (seen ejecting merge-queue entries). Scaled to 2s/2.3s/3.8s: same contract, ~1.5s of slack.

    </details>

## [3.86.0](https://github.com/SocialGouv/iterion/compare/v3.85.1...v3.86.0) (2026-09-01)

### Features

* **server:** the outcome router — a terminal run is decided by its contract, once per episode ([#599](https://github.com/SocialGouv/iterion/issues/599)) ([5436277](https://github.com/SocialGouv/iterion/commit/5436277408f2016d78e218eec1b4409659ce7a45)), references [#1](https://github.com/SocialGouv/iterion/issues/1) [#595](https://github.com/SocialGouv/iterion/issues/595) [#597](https://github.com/SocialGouv/iterion/issues/597) [#607](https://github.com/SocialGouv/iterion/issues/607)

    <details><summary>why</summary>

    The measured class, one campaign, 48h: a converged run waited 8h51 for a human; a run marked 'handled' by its external observer was redelivered, worked 6h, converged, and was never looked at again; a third landed while carrying an explicit blocker and reddened every downstream launch. The decision lived outside the authority that knows the state — an external script guessing from a stale copy.

    </details>

## [3.85.1](https://github.com/SocialGouv/iterion/compare/v3.85.0...v3.85.1) (2026-09-01)

### Bug Fixes

* **routing:** the single trusted reading enforces its own preconditions ([#607](https://github.com/SocialGouv/iterion/issues/607)) ([d7716eb](https://github.com/SocialGouv/iterion/commit/d7716ebee58ed2900f8f1c35a77a60afcefc8f95))

    <details><summary>why</summary>

    Revi's pass on the contract, 5/5 adopted:

    </details>

## [3.85.0](https://github.com/SocialGouv/iterion/compare/v3.84.1...v3.85.0) (2026-09-01)

### Features

* **webhooks:** on-demand re-review via the forge-native re-request button; per-repo merge-gate opt-out ([#604](https://github.com/SocialGouv/iterion/issues/604)) ([c5eb318](https://github.com/SocialGouv/iterion/commit/c5eb31847c545dadaf61f98c888a3787e77fb8c6)), references [iterion#300](https://github.com/iterion/issues/300)

    <details><summary>why</summary>

    An operator who pins gate_enabled=false on a repo integration turns the review bot advisory-only — no commit status ever lands — so the statuses-scope derivation that forces re-review-on-sync (whose sole purpose is keeping a REQUIRED check alive across pushes) no longer applies. The pin now disables the forcing on fresh provisions AND releases an already-forced sync through the backfill, in both cases surviving re-provisions — unlike a bare review_on_sync PATCH, which the next provision…

    </details>

## [3.84.1](https://github.com/SocialGouv/iterion/compare/v3.84.0...v3.84.1) (2026-09-01)

### Bug Fixes

* **server:** refuse team-less identities at the auth choke — no more empty-tenant panics on /api/runs* (Sentry ITERION-13/-1W/-1Z) ([#606](https://github.com/SocialGouv/iterion/issues/606)) ([097de77](https://github.com/SocialGouv/iterion/commit/097de77f2a103d187ac4258ea5eb393b7ccadc2c))

    <details><summary>why</summary>

    An authenticated identity whose TeamID resolved empty (a PAT minted with no team whose owner has no default team, a GitHub-gated user before any grant) sailed through requireAuth and reached the Mongo store with an EMPTY tenant in ctx — the store's fail-closed guard then panicked on every request: a recovered 500 for the caller, steady-state Sentry noise burying real crashes (ITERION-13/-1W/-1Z, 1800+ events since 2026-08-24 on GET /api/runs, GET /api/runs/{id} and the run WebSocket).

    </details>

## [3.84.0](https://github.com/SocialGouv/iterion/compare/v3.83.1...v3.84.0) (2026-09-01)

### Features

* **routing:** the launch-frozen outcome contract — a run carries what success means ([#598](https://github.com/SocialGouv/iterion/issues/598)) ([1253dc4](https://github.com/SocialGouv/iterion/commit/1253dc4b50204f236c81bbe89072e1309a81f48b)), references [#597](https://github.com/SocialGouv/iterion/issues/597)

    <details><summary>why</summary>

    'converged + nothing blocking' has no generic representation: outputs are an opaque per-bot bag, and a consumer reading only a convergence flag would have auto-landed exactly the blocked run a measured incident came from (a converged branch carrying an explicit blocker — 190 min of downstream work reddened). Only a contract can know the fields.

    </details>

## [3.83.1](https://github.com/SocialGouv/iterion/compare/v3.83.0...v3.83.1) (2026-09-01)

### Bug Fixes

* **server:** a queued run waiting for a free runner is not an orphan ([#602](https://github.com/SocialGouv/iterion/issues/602)) ([a6d6fb9](https://github.com/SocialGouv/iterion/commit/a6d6fb987901c3fe9b808ca59b8b3890df243268))

    <details><summary>why</summary>

    The orphan sweeper reads "queued row, stale, no lease" as "the message is gone" and flips the run to failed_resumable. That is one of TWO causes with the same shape, and the other one is normal operation: a runner pod takes ONE run at a time, so a frozen pool is a hard parallelism ceiling — a campaign of multi-hour runs fills every pod and short runs simply wait their turn, unfetched, lease-less, and stale.

    </details>

## [3.83.0](https://github.com/SocialGouv/iterion/compare/v3.82.1...v3.83.0) (2026-09-01)

### Features

* **store:** numbered episodes with continuation ownership — the run document stops saying nothing ([#597](https://github.com/SocialGouv/iterion/issues/597)) ([146c7cc](https://github.com/SocialGouv/iterion/commit/146c7cca8f677e0c7a8b1ee4aea437d4380c4cee))

    <details><summary>why</summary>

    A run document that says failed_resumable and nothing else forces every outcome consumer to guess: is a redelivery in flight? did it hit its budget wall? was it an operator stop? Measured on a live campaign: a budget wall hidden behind an empty final_error (the code existed — in the run_failed EVENT, never on the document), and an external router that marked a redelivered run 'handled' while it quietly converged.

    </details>

## [3.82.1](https://github.com/SocialGouv/iterion/compare/v3.82.0...v3.82.1) (2026-09-01)

### Bug Fixes

* **runview:** merge is an owned, claimed state machine — no more double-squash between replicas ([#595](https://github.com/SocialGouv/iterion/issues/595)) ([b9f8ec4](https://github.com/SocialGouv/iterion/commit/b9f8ec4374dd9d3666ec3047cb2b333dd5933e93))

    <details><summary>why</summary>

    PerformMergeCtx checked merge_status==merged then went to work: on a multi-replica server (prod runs 3) two concurrent calls both passed the check, both built a squash (different commits under the default squash strategy), and the loser — non-FF push refused — then persisted merge_status=failed over the winner's merged via a full-document ReplaceOne. Found by adversarial plan review, verified in code; the merge path is live on every repo-targeted campaign landing.

    </details>

## [3.82.0](https://github.com/SocialGouv/iterion/compare/v3.81.0...v3.82.0) (2026-09-01)

### Features

* **store:** canonical terminal-state contract + persisted failure taxonomy (ADR-095) ([#603](https://github.com/SocialGouv/iterion/issues/603)) ([daa6c8a](https://github.com/SocialGouv/iterion/commit/daa6c8a22d9882500b4fd1aaf656f9c45f33bc15))

    <details><summary>why</summary>

    One place answers every lifecycle-classification question: policy-named predicates on RunStatus (IsFinalSuccess/IsFinalFailure/IsTerminalResumable/ CanOperatorResume/RequiresResumeAnswers/CanAutoResume/ CountsAgainstLaunchLimit) and the FailureCode vocabulary — runtime's ErrorCode values plus INTERRUPTED/FAIL_NODE/PROCESS_ORPHANED/ QUEUE_SCHEMA_MISMATCH — persisted as an open-world, zero-means-unknown field on Run. Truth-table + relation tests pin every set; the negative-space test forbids new…

    </details>

## [3.81.0](https://github.com/SocialGouv/iterion/compare/v3.80.1...v3.81.0) (2026-09-01)

### Features

* **reliability:** silent-failure pack — effect outbox, terminal cancel, cloud operator alerts, sweep nets ([#594](https://github.com/SocialGouv/iterion/issues/594)) ([940cb43](https://github.com/SocialGouv/iterion/commit/940cb43969af297d030cafc4b41d83f0711be3d4))

    <details><summary>why</summary>

    The dispatcher resumed `cancelled` runs from their checkpoint (bilan issue-triage friction 7): an operator's cancel was undone on the next tick. The status was in the auto-resume set because the dispatcher's OWN stops (stall reap, external state change, daemon shutdown) used a bare context.CancelFunc, so the engine persisted `cancelled` for them too — removing the status alone would have turned stall recovery into a permanent park.

    </details>

## [3.80.1](https://github.com/SocialGouv/iterion/compare/v3.80.0...v3.80.1) (2026-09-01)

### Bug Fixes

* **queue:** survive a queue-backend outage — bounded republish retry, typed 503, budget guard before the sandbox ([#593](https://github.com/SocialGouv/iterion/issues/593)) ([e598ebc](https://github.com/SocialGouv/iterion/commit/e598ebc1712a41e1ac6dd72f913777870a6faa1c))

    <details><summary>why</summary>

    A production outage of the queue backend lasted about ten minutes and exposed three measured failure modes:

    </details>

## [3.80.0](https://github.com/SocialGouv/iterion/compare/v3.79.0...v3.80.0) (2026-09-01)

### Features

* **golden-master:** net extension by pure addition — request ledger + the net's own acting subbot ([#588](https://github.com/SocialGouv/iterion/issues/588)) ([64eb486](https://github.com/SocialGouv/iterion/commit/64eb486254b7a6797484246247c8256f2789d80e))

    <details><summary>why</summary>

    Selftest fixtures and run-note examples used a real target repo's directory name as a path literal; replace with neutral placeholders. No behavior change — harness selftest 70/70, sync test green.

    </details>

## [3.79.0](https://github.com/SocialGouv/iterion/compare/v3.78.1...v3.79.0) (2026-09-01)

### Features

* **runner:** park an unbankable attempt's work on its own ref ([#590](https://github.com/SocialGouv/iterion/issues/590)) ([14ea91e](https://github.com/SocialGouv/iterion/commit/14ea91ef065c745d88a6a5718434216c8f36f113))

    <details><summary>why</summary>

    An interrupted delivery, a paused run, and a bankable death on a lease-lost ctx all leave their commits stranded in the git-meta snapshot: the storage branch must not be touched (another pod may own the lease; FinalBranch on a half-done run would be merge-eligible mid-flight), so until now nothing was pushed at all, and turning the snapshot back into a branch takes a manual replay every time — the same measured cost the death bank closed for budget/failure outcomes (nine manual recoveries in…

    </details>

## [3.78.1](https://github.com/SocialGouv/iterion/compare/v3.78.0...v3.78.1) (2026-08-31)

### Bug Fixes

* **queue:** configurable JetStream stream replication (data-HA) ([#592](https://github.com/SocialGouv/iterion/issues/592)) ([578f1c1](https://github.com/SocialGouv/iterion/commit/578f1c134a1c48434e517ef66d2188089aa7ff78))

    <details><summary>why</summary>

    Production on 2026-08-31 exposed a connection-HA/data-HA gap: a three-node JetStream cluster still held the engine streams and locks bucket at R1. During a broker blip, publishing returned 'nats: no response from stream', in-flight work was interrupted, and resume requests failed.

    </details>

## [3.78.0](https://github.com/SocialGouv/iterion/compare/v3.77.2...v3.78.0) (2026-08-31)

### Features

* **sandbox:** permission policy crosses the claw IPC — gated claw fallbacks under sandbox + feed-watch usage_window fallback ([#589](https://github.com/SocialGouv/iterion/issues/589)) ([93878cd](https://github.com/SocialGouv/iterion/commit/93878cdda6121b08a5c24cfe447398c1ec5d93ad))

    <details><summary>why</summary>

    A sandboxed claw node with an enabled permission gate was refused outright: delegate.IOTask carried no policy, so the in-container __claw-runner would have executed bash/file_edit/write_file ungated. That blanket refusal also made a claw fallback route useless for any gated node under the shipped sandbox: auto default — e.g. feed-watch's synthesize, which cannot degrade to the OpenAI forfait when the Anthropic usage window closes.

    </details>

## [3.77.2](https://github.com/SocialGouv/iterion/compare/v3.77.1...v3.77.2) (2026-08-30)

### Bug Fixes

* **test:** deflake the launch tests at their cost, not their timeout ([#586](https://github.com/SocialGouv/iterion/issues/586)) ([003d155](https://github.com/SocialGouv/iterion/commit/003d1559f4fffaa44bac34b834fb981a25a413ea))

    <details><summary>why</summary>

    TestLaunch_AppliesBudgetOverrides (30s) and TestEngineRunner_SubbotChildHoldsRunLock (60s) both failed on wall-clock this week. Neither timeout is raised here.

    </details>

## [3.77.1](https://github.com/SocialGouv/iterion/compare/v3.77.0...v3.77.1) (2026-08-30)

### Bug Fixes

* **changelog:** backfill the three releases the cutover skipped ([#584](https://github.com/SocialGouv/iterion/issues/584)) ([554db02](https://github.com/SocialGouv/iterion/commit/554db02117fb44f508744b0bed692cb99c1368c8)), references [#579](https://github.com/SocialGouv/iterion/issues/579)

    <details><summary>why</summary>

    v3.75.0, v3.75.1 and v3.76.0 shipped while #579 was open, so release-it still ran under `infile: false` and wrote nothing. CHANGELOG.md kept the snapshot taken at v3.74.1 and the v3.77.0 section landed straight on top of it — the only visible trace was a compare link reaching back to a version no section described.

    </details>

## [3.77.0](https://github.com/SocialGouv/iterion/compare/v3.76.0...v3.77.0) (2026-08-30)

### Features

* **release:** commit a CHANGELOG.md fed by release-it, with commit-body context ([#579](https://github.com/SocialGouv/iterion/issues/579)) ([501ef49](https://github.com/SocialGouv/iterion/commit/501ef498efc34bdd0e87e6686fb5eec0062c4c6c))

    <details><summary>why</summary>

    The conventional-changelog plugin ran with `infile: false`, so 264 releases of notes existed only on the GitHub releases page. `infile` settles the drift objection that deferred the file: release-it writes the section in `beforeRelease` and stages it with `git add . --update`, so it lands in the release commit itself and cannot diverge from the tags.

    </details>

## [3.76.0](https://github.com/SocialGouv/iterion/compare/v3.75.1...v3.76.0) (2026-08-30)

### Features

* **fallback:** run-level fallback becomes an ordered chain — v11 rides beside v10 ([#583](https://github.com/SocialGouv/iterion/issues/583)) ([96bc9db](https://github.com/SocialGouv/iterion/commit/96bc9dbc86bbb137c95a1ec70d729f55ae121e8f))

    <details><summary>why</summary>

    One stage was not enough: a run whose primary AND first fallback share a failing provider still dies, and a three-stage route (subscription → facade → cross-family) was inexpressible. The launch field now accepts a single object (promoted to a one-stage chain, wire-compatible) or an ordered array; stages apply in order, a refused stage is logged and skipped, and the existing fallback event carries the zero-based fallback_index of the stage that took over. Budgets stay per-stage on the existing…

    </details>

## [3.75.1](https://github.com/SocialGouv/iterion/compare/v3.75.0...v3.75.1) (2026-08-30)

### Bug Fixes

* **bots,docs:** Revi follow-up on the plan-phase extension ([#578](https://github.com/SocialGouv/iterion/issues/578)) ([#580](https://github.com/SocialGouv/iterion/issues/580)) ([e38c3f8](https://github.com/SocialGouv/iterion/commit/e38c3f8343f3706fae1dde18ace466201e560a0c))

    <details><summary>why</summary>

    Findings R0c5ecf R111a3c R62b46b R9c35e4 R80915d R15f1bc R86427d R255dc8 R2c671d R1c4b91.

    </details>

## [3.75.0](https://github.com/SocialGouv/iterion/compare/v3.74.1...v3.75.0) (2026-08-30)

### Features

* **bots:** extend the cross-model plan phase to feature-gap-fill, test-coverage and e2e-coverage ([#578](https://github.com/SocialGouv/iterion/issues/578)) ([22a29c0](https://github.com/SocialGouv/iterion/commit/22a29c0e8dbf12bb1a664e59a5706beedbafb1c8))

    <details><summary>why</summary>

    Replicates the ADR-091 fragment (plan_topology → plan → plan_review → plan_gate → plan_revise; plan_review/plan_review_policy vars; the peer's action:skip fallback route) verbatim from feature-dev, with per-bot mission hand-offs (gap_spec+scope_notes / target+test kinds / matrix_path+target). All three had the exact feature-dev shape (entry: campaign + one continuation_loop back-edge), so the back-edge blanks and the map-every-field discipline carry over unchanged under selected-incoming-edges…

    </details>

## [3.74.1](https://github.com/SocialGouv/iterion/compare/v3.74.0...v3.74.1) (2026-08-29)

### Bug Fixes

* **gate:** the sweep's last pass over a stuck run must be visible at info ([#577](https://github.com/SocialGouv/iterion/issues/577)) ([f416cbc](https://github.com/SocialGouv/iterion/commit/f416cbcf4a968a2b4ff7b13d8e6d65e3467aa0ab)), references [#564](https://github.com/SocialGouv/iterion/issues/564)

    <details><summary>why</summary>

    A run that owes a merge-gate verdict and abstains logs its reason at Debug on the sweep path, to keep ~60 identical lines an hour per replica out of the log. Deployments run at info, so those passes emitted nothing at all: the single Warn the event path fires dies with the pod, and a required check left unanswered has no trace anywhere naming why.

    </details>

## [3.74.0](https://github.com/SocialGouv/iterion/compare/v3.73.0...v3.74.0) (2026-08-29)

### Features

* **queue:** the run-level fallback route rides the wire to the pod ([#574](https://github.com/SocialGouv/iterion/issues/574)) ([d51fd5d](https://github.com/SocialGouv/iterion/commit/d51fd5da08e7c40ea5acc758e259e642515cd017)), references [#513](https://github.com/SocialGouv/iterion/issues/513)

    <details><summary>why</summary>

    The launch API accepted `fallback` (the operator's single run-level rescue route, ADR-087) and the local executor honoured it — but the cloud publisher dropped it at publish: no RunMessage field, no run-doc stamp, no resume replay. The route meant to rescue a run from a provider's exhausted usage window never fired precisely where runs park unattended (measured on one campaign: 72-85% of two runs' wall-clock lost to usage-window parking a declared fallback would have bypassed).

    </details>

## [3.73.0](https://github.com/SocialGouv/iterion/compare/v3.72.0...v3.73.0) (2026-08-29)

### Features

* **model,cost,studio:** surface model pricing and max-output in ModelCapabilities (ADR-042 follow-through) ([#575](https://github.com/SocialGouv/iterion/issues/575)) ([c94ecce](https://github.com/SocialGouv/iterion/commit/c94eccef868588fe26f90ff6355c09c7447f3978))

    <details><summary>why</summary>

    The aggregator has parsed and cached limit.output since ADR-042, but the field stopped at fetchedSpec: nothing downstream could read a model's completion cap. Carry it through merge onto ModelCapabilities, out via ResolvedCapabilities, and into the `iterion models` table as a MAX OUT column.

    </details>

## [3.72.0](https://github.com/SocialGouv/iterion/compare/v3.71.0...v3.72.0) (2026-08-29)

### Features

* **feed-watch:** deliver a long digest in several messages, never cut ([#570](https://github.com/SocialGouv/iterion/issues/570)) ([8bc4e5a](https://github.com/SocialGouv/iterion/commit/8bc4e5a6610ca4ce610902d07a1b73eabc1c96ac))

    <details><summary>why</summary>

    A digest over 14000 chars was truncated with a notice pointing at "the run artifacts" — somewhere its chat readers cannot go. The 28 août Veille Cyber digest (14803 chars) lost its last 803 characters that way.

    </details>
* **runner:** a dying run banks its branch ([#556](https://github.com/SocialGouv/iterion/issues/556)) ([5d2007f](https://github.com/SocialGouv/iterion/commit/5d2007f87a2481e082f2ce2d4b45ec1d59294aa4))

    <details><summary>why</summary>

    A repo-targeted cloud run that died (budget cap mid-delegate, failure path) left its commits only in the git-meta snapshot: the bank push was gated on runErr == nil, so the branch never reached the forge and the successor restarted from the base commit. Turning the snapshot back into a branch took a manual replay every time — measured: nine manual recoveries in three days of one campaign.

    </details>

### Bug Fixes

* **feed-watch:** size the marker reserve from the marker, and refuse the queue on a truncated digest ([#576](https://github.com/SocialGouv/iterion/issues/576)) ([73fdf0c](https://github.com/SocialGouv/iterion/commit/73fdf0c096831b85c4e84f64442f5a9d2b126054))

    <details><summary>why</summary>

    R79a75e. MARK_RESERVE was 32 while the truncation notice the last capped part carries is 64 chars, so a ceiling-truncated digest overshot the very budget the reserve exists to respect: at limit=1200 the last message measured 1232. An operator who sets a sink's max_chars to the platform's own limit gets that POST rejected — losing precisely the notice saying the digest was cut.

    </details>

## [3.71.0](https://github.com/SocialGouv/iterion/compare/v3.70.1...v3.71.0) (2026-08-29)

### Features

* **platformcfg:** bot vars — ${ITERION_X} resolved from the DB before the pod env ([#568](https://github.com/SocialGouv/iterion/issues/568)) ([06627f3](https://github.com/SocialGouv/iterion/commit/06627f38e38bb37b86d6faf54fc930ac16053995))

    <details><summary>why</summary>

    Bots parameterize their model pins, reasoning effort and tunables as ${ITERION_X:-default} expansions, read from the runner pod's env — so re-tuning a bot meant a Helm values change and a rollout, while credentials, bundles, caps, roles and the sandbox image had all moved to the CLI→API→DB settings surface. Same doctrine, fourth family: bot_vars.

    </details>

## [3.70.1](https://github.com/SocialGouv/iterion/compare/v3.70.0...v3.70.1) (2026-08-29)

### Bug Fixes

* **model:** forward host-probed codex version into the sandboxed claw runner ([#571](https://github.com/SocialGouv/iterion/issues/571)) ([82f98df](https://github.com/SocialGouv/iterion/commit/82f98df521dba4437331c6be4f135999bff97a07))

    <details><summary>why</summary>

    The sandbox image ships no codex binary, so with no ITERION_CODEX_VERSION override the in-container runner fell back to claw's baked-in version string and OpenAI 400'd newer models (gpt-5.6-sol) that the host's own codex install can serve. Forward the host-resolved version through the existing env choke point.

    </details>

## [3.70.0](https://github.com/SocialGouv/iterion/compare/v3.69.1...v3.70.0) (2026-08-28)

### Features

* **bots:** Senti names who is actually vulnerable, not who uses the tech ([#545](https://github.com/SocialGouv/iterion/issues/545)) ([59fb4c2](https://github.com/SocialGouv/iterion/commit/59fb4c2f04247b0a2d8eda1d025441663e151c2f)), references [#veille-vigie-secu](https://github.com/SocialGouv/iterion/issues/veille-vigie-secu)

    <details><summary>why</summary>

    The flow that had never run for real now has: two watch-only GitHub Apps (one per org, since a private App only installs on its owner), both on All repositories, both carrying metadata+vulnerability_alerts read and nothing else. `poll_dependabot` answered `orgs_ok: 2, orgs_failed: 0` — which is the proof that the dependabot_tokens map is keyed by ORG and not by the App's bot handle, the critical the review caught and no test could see.

    </details>

## [3.69.1](https://github.com/SocialGouv/iterion/compare/v3.69.0...v3.69.1) (2026-08-27)

### Bug Fixes

* **cloud:** a resume must keep the budget the launch declared ([#554](https://github.com/SocialGouv/iterion/issues/554)) ([72ab85c](https://github.com/SocialGouv/iterion/commit/72ab85c1e011e67ca3a8c2e05d56bb115770e612))

    <details><summary>why</summary>

    SubmitLaunch published the operator's budget override on the wire, but SubmitResume rebuilt its RunMessage with a nil override — so the first usage-window auto-retry silently reverted the run to the workflow's own cap. Measured: a run launched with max_duration 8h, parked by a cap and resumed, died at 14407s/14400s while its doc still displayed 8h. Cloud resumes are unattended, so nothing could re-state the ask.

    </details>

## [3.69.0](https://github.com/SocialGouv/iterion/compare/v3.68.5...v3.69.0) (2026-08-27)

### Features

* **codex:** expose native web search from DSL tools ([#550](https://github.com/SocialGouv/iterion/issues/550)) ([c4e1528](https://github.com/SocialGouv/iterion/commit/c4e1528b3b592d035b20b00444ff1bb28cda0121))

## [3.68.5](https://github.com/SocialGouv/iterion/compare/v3.68.4...v3.68.5) (2026-08-27)

### Bug Fixes

* **model:** forward ITERION_CODEX_VERSION into the sandboxed claw runner ([#553](https://github.com/SocialGouv/iterion/issues/553)) ([a2ac17a](https://github.com/SocialGouv/iterion/commit/a2ac17a044b77aaf1b872b7607143515f1c2c847))

    <details><summary>why</summary>

    The ChatGPT-forfait wire gates model availability on the codex-cli version header. The sandbox image ships no codex binary, so the in-container runner's `codex --version` probe finds nothing and falls back to claw's baked-in version — which the backend refuses for newer models. Lived on iterion#541's /billy run: plan_review 400'd with "gpt-5.6-sol requires a newer codex-cli" on every cloud attempt, with no operator-side cure since the override never crossed the boundary.

    </details>

## [3.68.4](https://github.com/SocialGouv/iterion/compare/v3.68.3...v3.68.4) (2026-08-27)

### Bug Fixes

* **bots:** plan_review_policy defaults to skip fleet-wide ([#548](https://github.com/SocialGouv/iterion/issues/548)) ([f803fe4](https://github.com/SocialGouv/iterion/commit/f803fe4514782ecf816eb48ac8ce7a6c9dbcba75))

    <details><summary>why</summary>

    Extends branch-improve-loop's default to the three sibling plan-phase campaign bots (app-dev, feature-dev, whole-improve-loop). Two lived incidents the same day made the case: a dead second-family credential blocked a fixer through plan_review auto + wait, and a stale pod OpenAI key paused every cloud campaign the same way. The cross-model peer is an optional enrichment — the primary family alone must always suffice — so a peer failure completes plan_review with the _skipped stamp and the…

    </details>

## [3.68.3](https://github.com/SocialGouv/iterion/compare/v3.68.2...v3.68.3) (2026-08-27)

### Bug Fixes

* **runtime:** build node input from selected incoming edges only ([#539](https://github.com/SocialGouv/iterion/issues/539)) ([bd1ca52](https://github.com/SocialGouv/iterion/commit/bd1ca52630dbfa9e5a014cd413a53b78162f48fc)), closes [#484](https://github.com/SocialGouv/iterion/issues/484)

    <details><summary>why</summary>

    buildNodeInputRS merged with-mappings from every incoming edge whose source had produced output, so a mutually exclusive when/else pair that later converged let the unselected mapping silently overwrite the selected one (Copi's validator verdict never reached the operator).

    </details>

## [3.68.2](https://github.com/SocialGouv/iterion/compare/v3.68.1...v3.68.2) (2026-08-27)

### Bug Fixes

* **usagecap:** the meter follows the credential, not the slot ([#541](https://github.com/SocialGouv/iterion/issues/541)) ([57b7062](https://github.com/SocialGouv/iterion/commit/57b7062d2119f50b50f18cdab9cf489775f9d3f6))

    <details><summary>why</summary>

    A usage reading records which WINDOW is spent, keyed by backend and scope (tenant/platform). When the credential behind that slot is rotated — a fresh OAuth token posted over a team's exhausted one — the old account's seven-day reading stayed on the meter, legitimately fresh until its own reset instant, and parked every run of the new credential for days. Lived on a real deployment: a fresh token blocked at 95% by the reading of the account it replaced, resets five days out.

    </details>

## [3.68.1](https://github.com/SocialGouv/iterion/compare/v3.68.0...v3.68.1) (2026-08-27)

### Bug Fixes

* **supervise:** unpinned evaluator prefers the supervised run's own provider family ([#546](https://github.com/SocialGouv/iterion/issues/546)) ([1de6be2](https://github.com/SocialGouv/iterion/commit/1de6be26af4c152a795e2c701379f6b1c386d015))

    <details><summary>why</summary>

    An unpinned supervisor resolved its model by host auto-detect, so whatever key sat first in the environment decided — on the prod runner pods a dead platform OPENAI key (429, no credits) failed every Persy eval while the supervised campaign ran fine on Anthropic (run 01a042c2). SpecsFromWorkflow now derives a provider hint from the watched nodes (provider: routing, model prefix, or backend family) and the resolver prefers it when that provider is detected available; pin and…

    </details>

## [3.68.0](https://github.com/SocialGouv/iterion/compare/v3.67.0...v3.68.0) (2026-08-27)

### Features

* **bots:** product-docs publication tail — skill-driven Onyxia deploy behind deterministic gates ([#533](https://github.com/SocialGouv/iterion/issues/533)) ([6e8e6ea](https://github.com/SocialGouv/iterion/commit/6e8e6eaf9be4a3a1d6654809e04079ff2716fd93))

    <details><summary>why</summary>

    publish_gate (opt-in + secrets pre-flight) -> publish agent loading the org-private deploy-onyxia-sspcloud skill explicitly (ADR-059 skills: ref) -> verify_publish (external URL truth gate, fails the run when the site is not serving) -> surface_site_link. French admonition titles in the GitBook->MkDocs converter.

    </details>

## [3.67.0](https://github.com/SocialGouv/iterion/compare/v3.66.0...v3.67.0) (2026-08-27)

### Features

* **bots:** product-docs (Prody) — functional documentation from a multi-repo product catalog ([#524](https://github.com/SocialGouv/iterion/issues/524)) ([ff16bf4](https://github.com/SocialGouv/iterion/commit/ff16bf411479ba78cf78cc671c05b912cd178a8c))

    <details><summary>why</summary>

    New catalog bot generating and maintaining BUSINESS-AUDIENCE product documentation in a dedicated docs repository, grounded in the source code of the N repositories a product catalog names.

    </details>

## [3.66.0](https://github.com/SocialGouv/iterion/compare/v3.65.0...v3.66.0) (2026-08-27)

### Features

* **platform:** DB-backed bot overrides + runtime bot-roles/sandbox settings ([#535](https://github.com/SocialGouv/iterion/issues/535)) ([bdafa72](https://github.com/SocialGouv/iterion/commit/bdafa72a0a809fde495e3fd5b7bc53e4c2e49090))

    <details><summary>why</summary>

    Iterating on a native bot on a cloud instance used to cost an image build + rollout. This makes it one CLI call, mirroring the platform LLM credentials pattern (env/baked = default, DB record = runtime override):

    </details>

## [3.65.0](https://github.com/SocialGouv/iterion/compare/v3.64.2...v3.65.0) (2026-08-26)

### Features

* **runtime:** a bounded grace so a spent run still delivers what it paid for ([#532](https://github.com/SocialGouv/iterion/issues/532)) ([96d89ef](https://github.com/SocialGouv/iterion/commit/96d89ef0650e021b7e6ddc23b827f3e5376ac53a))

    <details><summary>why</summary>

    A run whose cap runs out mid-way dies holding work it has already paid for, with no way to hand it over: a documentation campaign overran its budget and left a finished, committed corpus with no pull request. The money was spent either way; refusing the last few nodes only decided whether anything came of it.

    </details>

## [3.64.2](https://github.com/SocialGouv/iterion/compare/v3.64.1...v3.64.2) (2026-08-26)

### Bug Fixes

* **runtime:** a budget overrun must not strand the node that earned it ([#529](https://github.com/SocialGouv/iterion/issues/529)) ([722b760](https://github.com/SocialGouv/iterion/commit/722b760ac8c5272089660998e1a23a083f0fece2))

    <details><summary>why</summary>

    A node whose usage takes the run past a hard cap has already SUCCEEDED: its output is validated, persisted and in the store. Failing inside the post-exec check anchored the checkpoint on that node, so a resume with a raised cap re-executed it — for an agent pass, paying its entire cost a second time to reach a result already on disk. Observed on a docs campaign that overran 442/400: the only way forward was to pay for the whole pass again.

    </details>
* **studio:** default inline subbot frame to live/latest child ([#530](https://github.com/SocialGouv/iterion/issues/530)) ([415b981](https://github.com/SocialGouv/iterion/commit/415b981c9504e23ab3be427f272f31dba6d201f5)), closes [#525](https://github.com/SocialGouv/iterion/issues/525)

    <details><summary>why</summary>

    The Run canvas picked children[0] (created_at asc = oldest) when the operator had not chosen a tab, so a historical failed child painted the expanded graph red after a later child had already succeeded.

    </details>

## [3.64.1](https://github.com/SocialGouv/iterion/compare/v3.64.0...v3.64.1) (2026-08-26)

### Bug Fixes

* **ast:** serialize supervisor declarations across the cloud queue + Persy dogfood bilan ([#528](https://github.com/SocialGouv/iterion/issues/528)) ([129b2a1](https://github.com/SocialGouv/iterion/commit/129b2a164b636a040e6b1c6f71a816bfaf07848f))

    <details><summary>why</summary>

    The AST JSON codec is the queue's wire format; MarshalFile dropped supervisor decls, so a cloud runner pod recompiled every workflow without its supervisors — no spawn, no skip log. Found live by the first repo-targeted dogfood run on the prod runner (01a03d70).

    </details>
* **cli:** local file secrets never reached the sandbox ([#531](https://github.com/SocialGouv/iterion/issues/531)) ([d9c1c70](https://github.com/SocialGouv/iterion/commit/d9c1c703e2cfc73e077ceda487fd60382c264082))

    <details><summary>why</summary>

    A bot declaring a file secret got it mounted nowhere on a local run. The credentials were resolved from the sealed store and stamped into the EXECUTOR's context — but the engine mounts declared file secrets into the sandbox at run start, from the context handed to Run, and that one never carried them. An optional secret was then skipped in silence ("the agent simply won't find the file"), and a required one failed the launch with "has no resolved value" on a host whose store held exactly that…

    </details>
* **sandbox:** fall back when the engine's own image tag was never published ([#526](https://github.com/SocialGouv/iterion/issues/526)) ([ef95dad](https://github.com/SocialGouv/iterion/commit/ef95dad3fbe100ec6eb919ab371eab3d22217b7b))

    <details><summary>why</summary>

    A binary built between releases — or from a release whose sandbox image did not ship — pins ghcr.io/socialgouv/iterion-sandbox-slim:<version>, a tag nobody pushed. The pull then fails with a raw 'manifest unknown' and the run dies at startup, before any node: observed on v3.58.3, where neither 3.58.3 nor v3.58.3 exists at the registry, so every local sandboxed run needed a manual --sandbox-default-image.

    </details>

## [3.64.0](https://github.com/SocialGouv/iterion/compare/v3.63.0...v3.64.0) (2026-08-26)

### Features

* **forge:** watch-only GitHub App for org-wide Dependabot alerts ([#527](https://github.com/SocialGouv/iterion/issues/527)) ([0a77503](https://github.com/SocialGouv/iterion/commit/0a7750318dc546a0e564617f66b00027f6205419))

    <details><summary>why</summary>

    The org-wide alerts endpoint returns only what an installation can see, so covering an org means installing on All repositories. Doing that with the ordinary forge App would grant contents:write — and, when opted in, administration:write — on every repository, as a side effect of wanting to READ alerts.

    </details>

## [3.63.0](https://github.com/SocialGouv/iterion/compare/v3.62.1...v3.63.0) (2026-08-26)

### Features

* **backends:** cool down refused fallback routes ([#511](https://github.com/SocialGouv/iterion/issues/511)) ([c123f89](https://github.com/SocialGouv/iterion/commit/c123f89aa8d7706b68e112d9c9c5e33f0b20e485)), closes [#468](https://github.com/SocialGouv/iterion/issues/468)

    <details><summary>why</summary>

    Keep the typed usage-window cause reachable when a cooled primary is skipped and its fallback also fails. Surface proactive skips in the Studio timeline and clarify the reserved unavailable-reset path.

    </details>

## [3.62.1](https://github.com/SocialGouv/iterion/compare/v3.62.0...v3.62.1) (2026-08-26)

### Bug Fixes

* **queue:** make schema bumps safe across mixed-version runner rollouts ([#510](https://github.com/SocialGouv/iterion/issues/510)) ([b8adb7a](https://github.com/SocialGouv/iterion/commit/b8adb7a3ac2db4b9f9092111ecbd3844052b59d9)), references [#481](https://github.com/SocialGouv/iterion/issues/481)

    <details><summary>why</summary>

    A version mismatch on the runs queue was Naked immediately, so a stale runner fleet could burn the whole MaxDeliver budget in seconds during a rolling schema bump — and JetStream then dropped the message silently, leaving the run document queued forever with no recovery path (#481).

    </details>

## [3.62.0](https://github.com/SocialGouv/iterion/compare/v3.61.0...v3.62.0) (2026-08-26)

### Features

* **dsl,bots:** cross-model peer-reviewed plan phase + fallbacks action:skip / when: (ADR-091) ([#523](https://github.com/SocialGouv/iterion/issues/523)) ([4f3b7b3](https://github.com/SocialGouv/iterion/commit/4f3b7b3fda223432555f3fab3d37205b170f9972))

    <details><summary>why</summary>

    Two new opt-in injections beside review_mode, resolved at launch from the same family set: plan_review (auto -> on iff >=2 distinct credentialed families, family-agnostic) for bots with a peer-reviewed plan phase, and llm_families (the raw sorted family list) so any bot can build its own policy without a new engine role var. InjectAll folds the three into one call + one log summary at the CLI/runview/dispatcher launch surfaces; FamilySet decouples resolution from detect.Report so the cloud…

    </details>

## [3.61.0](https://github.com/SocialGouv/iterion/compare/v3.60.1...v3.61.0) (2026-08-25)

### Features

* **supervise,bots:** Persy perseverance coach + declarative monitors + supervisors kill switch ([#522](https://github.com/SocialGouv/iterion/issues/522)) ([7470085](https://github.com/SocialGouv/iterion/commit/7470085a1e50e862e593a67989cb488610c53330))

    <details><summary>why</summary>

    feature-dev v2.2.0 ships the first use of the DSL supervisor block: Persy, a perseverance coach watching the campaign node. Monitors-first policy (give-up markers, Bash failures, budget warnings) with four intervention classes — premature impossibility, expedient path, failure loop, bank under pressure — and an asymptote guard so it composes with the ADR-058 convergence contract. The campaign contract gains the static PERSISTENCE clause. A dedicated test keeps the coach non-vacuous (C190/C193…

    </details>

## [3.60.1](https://github.com/SocialGouv/iterion/compare/v3.60.0...v3.60.1) (2026-08-25)

### Bug Fixes

* **forge:** Revi round 8 (final) — the last three, all consistent with each other ([#521](https://github.com/SocialGouv/iterion/issues/521)) ([7efe01a](https://github.com/SocialGouv/iterion/commit/7efe01ace0b15fe1b81aa021aaf792b2811c07d7))

    <details><summary>why</summary>

    Revi's gate went GREEN on the previous head (max medium). These are the three it still raised, fixed as the agreed last round.

    </details>

## [3.60.0](https://github.com/SocialGouv/iterion/compare/v3.59.3...v3.60.0) (2026-08-25)

### Features

* **bots:** Senti (vuln-watch) — inventory-scoped vulnerability sentinel, zero LLM ([#515](https://github.com/SocialGouv/iterion/issues/515)) ([d6cc7d6](https://github.com/SocialGouv/iterion/commit/d6cc7d6dacc9c8e7564d901d12e91ac286838a2c))

    <details><summary>why</summary>

    A github_app connection can now opt into SecurityReadEnabled: the refresh worker (and the new PATCH connections endpoint, which mints immediately so a missing grant answers 422 on the spot) mints an org-wide vulnerability_alerts:read installation token and merges it into the team-scoped dependabot_tokens generic secret ({org_login: token} JSON map, egress-pinned to the forge host). The profile is a separate opt-in like DeliveryInstallationPermissions — never folded into the runtime baseline —…

    </details>

## [3.59.3](https://github.com/SocialGouv/iterion/compare/v3.59.2...v3.59.3) (2026-08-25)

### Bug Fixes

* **modernize:** read a scalar exit_gate as one whole command ([#520](https://github.com/SocialGouv/iterion/issues/520)) ([7e9dad5](https://github.com/SocialGouv/iterion/commit/7e9dad56d998a10bd652344273b049e89905150e))

    <details><summary>why</summary>

    A YAML scalar and a sequence are both legitimate contract forms for a lot's exit_gate. plan_read joined the value with "\n" unconditionally, so a bare string was iterated character by character: the verifier's first command became the single letter of the declared gate, failed with exit 127, and the lot could never converge — a red verdict manufactured by the reader, not earned by the tree.

    </details>

## [3.59.2](https://github.com/SocialGouv/iterion/compare/v3.59.1...v3.59.2) (2026-08-25)

### Bug Fixes

* **bots:** revert a guard built on a false positive, pin the real one ([#518](https://github.com/SocialGouv/iterion/issues/518)) ([17df7af](https://github.com/SocialGouv/iterion/commit/17df7afc7d01fcdeed157a967bf021129e7fb146)), references [#508](https://github.com/SocialGouv/iterion/issues/508)

    <details><summary>why</summary>

    The [high] that motivated `ValidateShellSafeRef` in #508 does not exist. Its premise — that a bot's `PUSH_BRANCH={{vars.push_branch}} python3 -c` reaches the shell unquoted — reads the .bot SOURCE as if it were the final command line. The engine shell-escapes every ref at substitution time (resolveCommandTemplate → shellEscapeValue), so the hostile value arrives as `PUSH_BRANCH='x;id;#'`. Proven by execution, with the shell itself as the oracle.

    </details>

## [3.59.1](https://github.com/SocialGouv/iterion/compare/v3.59.0...v3.59.1) (2026-08-25)

### Bug Fixes

* **deps:** update npm (non-major) ([#517](https://github.com/SocialGouv/iterion/issues/517)) ([3d908a7](https://github.com/SocialGouv/iterion/commit/3d908a7f4e5fd6afd222e8342b6343b103de869b))
* **runner:** bank push resolves the LIVE credential through origin ([24092c4](https://github.com/SocialGouv/iterion/commit/24092c416c88bcba3d430237bcf28446e5c02a66))

    <details><summary>why</summary>

    The bank pushed with the claim-time token injected into the URL. A GitHub App installation token lives one hour; a paused-and-resumed run banks far later, and the final push — the run's most valuable action — died on a dead credential (loudly, thanks to FinalBranchError) while refreshGitCredentialsLoop had a live token sitting in the clone's credential store the whole time.

    </details>

## [3.59.0](https://github.com/SocialGouv/iterion/compare/v3.58.5...v3.59.0) (2026-08-24)

### Features

* **cli:** iterion remote admin caps — get/set the runtime usage caps ([7f49737](https://github.com/SocialGouv/iterion/commit/7f4973707248e926a60670a6ee232e79bf7cc209))
* **runner,runview:** usage-cap enforcement reads the live effective policy ([f324cd0](https://github.com/SocialGouv/iterion/commit/f324cd025a8b44d6a7e7a34fb970e37af075d77a))
* **server:** super-admin runtime usage-cap settings API + effective healthz echo ([9d4659c](https://github.com/SocialGouv/iterion/commit/9d4659c019a1b5c359b333f35d7066e39e6340a2))
* **usagecap:** platform runtime-settings record + TTL-cached live policy resolver ([099373f](https://github.com/SocialGouv/iterion/commit/099373f633f8b8d5a9c7aea7e6ab56bf551ae97f))

## [3.58.5](https://github.com/SocialGouv/iterion/compare/v3.58.4...v3.58.5) (2026-08-24)

### Bug Fixes

* **forge:** unbreak the dep-update lane's dead-gate recovery ([#508](https://github.com/SocialGouv/iterion/issues/508)) ([6bfe375](https://github.com/SocialGouv/iterion/commit/6bfe375bc4069483e277e46d23a1f3e81cd2d91b)), references [buildkit-operator#21](https://github.com/buildkit-operator/issues/21) [iterion#504](https://github.com/iterion/issues/504)

    <details><summary>why</summary>

    Four production defects found auditing the Renovate auto-upgrade pipeline (buildkit-operator#21 + iterion#504, 2026-08-17/24):

    </details>

## [3.58.4](https://github.com/SocialGouv/iterion/compare/v3.58.3...v3.58.4) (2026-08-24)

### Bug Fixes

* **cloud:** launch model_overrides now reach the runner's executor ([427a9f4](https://github.com/SocialGouv/iterion/commit/427a9f44e74d998bbd9219a97e05627ac5ce1ee9))

    <details><summary>why</summary>

    A cloud launch's model_overrides were persisted display-only: the studio showed the operator's per-node pins, but the RunMessage never carried them and the runner pod built its executor without them — every delegate ran on the workflow's own models while the Overview claimed otherwise. The local path applies them (service_launch → engine + executor); the cloud path silently didn't.

    </details>

## [3.58.3](https://github.com/SocialGouv/iterion/compare/v3.58.2...v3.58.3) (2026-08-24)

### Bug Fixes

* **auth:** repair remote teams/orgs switch — decode the real /api/auth/me shape ([775f9dc](https://github.com/SocialGouv/iterion/commit/775f9dc904156ac27add525d88866ac021458962))

    <details><summary>why</summary>

    The CLI decoded /api/auth/me with a hand-mirrored struct whose flat `teams` field the server had re-nested under orgs[].teams: every field silently zeroed, so `teams switch` refused every team as 'not a member', `orgs switch` accepted only the already-active org, `orgs list` showed a single org, and `teams list` rendered an all-empty table with every row starred (wrong json tags on the same wire).

    </details>
* **backends:** claude_code nodes get only the resolved MCP set ([#506](https://github.com/SocialGouv/iterion/issues/506)) ([#507](https://github.com/SocialGouv/iterion/issues/507)) ([fc3f15f](https://github.com/SocialGouv/iterion/commit/fc3f15f36bec4812c88b7380b8d889b425b727c5))

    <details><summary>why</summary>

    iterion resolves an explicit per-node MCP server set (mcp_server:/mcp: blocks, the repo's .mcp.json via autoload_project, its own ask_user/board servers) and passes it via --mcp-config — but without --strict-mcp-config the claude CLI MERGED the operator's personal user-scope servers (~/.claude.json) on top. Undeclared tools reached the agent, every node visit booted npx/servers/chromium (a CPU spike per iteration on loop-heavy persist bots — the observation behind #506), and personal API keys…

    </details>
* **feed-watch:** title the digest with its send date, window in the body ([3596683](https://github.com/SocialGouv/iterion/commit/359668383d7e8c681489c7865af23bc61e6d04eb)), references [#452](https://github.com/SocialGouv/iterion/issues/452) [#452](https://github.com/SocialGouv/iterion/issues/452)

    <details><summary>why</summary>

    PR #452 titled the WINDOW the queue covers ('Veille Cyber — 21 → 24 août') whenever span_days > 1 — factually honest, but a Monday reader gets a period headline for what is an ordinary digest whose feeds simply published late over the weekend (observed live: gopuyrust '30 juillet → 24 août' after two quota-dead Mondays). Operator arbitrage: the headline names digest_title + the send date ONLY; the anti-breaking spirit of #452 stays as one body clause ('covers the period since <oldest>') when…

    </details>
* **pipelines:** adopt finished recovery forks on the cloud board ([#379](https://github.com/SocialGouv/iterion/issues/379)) ([#502](https://github.com/SocialGouv/iterion/issues/502)) ([d538808](https://github.com/SocialGouv/iterion/commit/d53880868bd89ab7147773f32561fcb3282e7f8b))

    <details><summary>why</summary>

    reconcileFinishedTickets — the sweep that adopts a finished fork as the ticket's LastRunID and files it done (cascading waiting_deps dependents) — is gated to local mode, while the board projection that lets the fork replace its dead parent on the card also runs in cloud. In cloud a finished fork therefore took the card (Closed) but the ticket stayed in_progress forever and its dependents parked in waiting_deps.

    </details>

## [3.58.2](https://github.com/SocialGouv/iterion/compare/v3.58.1...v3.58.2) (2026-08-24)

### Bug Fixes

* **dsl:** align {{input.*}} in edge with-mappings with runtime ([#500](https://github.com/SocialGouv/iterion/issues/500)) ([cb56808](https://github.com/SocialGouv/iterion/commit/cb56808a593057f9772e6a38deea241f0c12a29d)), closes [#479](https://github.com/SocialGouv/iterion/issues/479)

    <details><summary>why</summary>

    Compiler C034 validated edge {{input.x}} against the source node's input schema; the runtime resolved it from the source output overlaid on run-level inputs. Those are not the same namespace, so a mapping could be rejected though it ran, or compile and resolve from a different source than the author/compiler implied (Copi's {{input.reviewer}} worked because --var reviewer=on also landed in run inputs).

    </details>

## [3.58.1](https://github.com/SocialGouv/iterion/compare/v3.58.0...v3.58.1) (2026-08-23)

### Bug Fixes

* **observability:** record the model that actually ran ([#474](https://github.com/SocialGouv/iterion/issues/474)) ([#501](https://github.com/SocialGouv/iterion/issues/501)) ([481e857](https://github.com/SocialGouv/iterion/commit/481e857e464e7eb6b5222467333566f0f1af61d8))

    <details><summary>why</summary>

    EffectiveModel was captured on delegate.Result, used for a cost fallback and a log-only drift warning, then dropped before anything durable was written. CLI backends have no llm_request.model escape hatch, so a finished run could not say which model served it.

    </details>

## [3.58.0](https://github.com/SocialGouv/iterion/compare/v3.57.2...v3.58.0) (2026-08-23)

### Features

* **backends:** grok and kimi enforce the permission gate ([#498](https://github.com/SocialGouv/iterion/issues/498)) ([a34a071](https://github.com/SocialGouv/iterion/commit/a34a071d24519320bf4b642131f366077d54d31f)), closes [#476](https://github.com/SocialGouv/iterion/issues/476), references [#497](https://github.com/SocialGouv/iterion/issues/497)

    <details><summary>why</summary>

    A bot declaring `permission: ask|deny` could not use grok or kimi at all — C176 refused every route. The refusal was correct; the coverage was the problem, and it locked out precisely the two backends whose credentials are a CLI forfait rather than a metered API key.

    </details>

## [3.57.2](https://github.com/SocialGouv/iterion/compare/v3.57.1...v3.57.2) (2026-08-23)

### Bug Fixes

* **dispatcher:** a dispatched bot can run its subbots ([#499](https://github.com/SocialGouv/iterion/issues/499)) ([4b7ec73](https://github.com/SocialGouv/iterion/commit/4b7ec73fff16148870d44404ab48752534fb1fae))

    <details><summary>why</summary>

    The direct engine path built its engine with ten runtime.With… options and no WithSubbotRunner, so every `subbot` node of a dispatched bot died with "no SubbotRunner is wired". The CLI (pkg/cli/run.go, resume.go) and the studio (pkg/runview/service_launch.go) each wired one; this path never did, and the ADR-046 route that would have borrowed the studio's is inert — WithRunLauncher has no non-test caller, so r.launcher is always nil and ITERION_DISPATCH_VIA_SERVICE cannot switch it on.

    </details>

## [3.57.1](https://github.com/SocialGouv/iterion/compare/v3.57.0...v3.57.1) (2026-08-23)

### Bug Fixes

* **dsl:** refuse an unresolvable tool name before the run starts (C135) ([#497](https://github.com/SocialGouv/iterion/issues/497)) ([28f6ebe](https://github.com/SocialGouv/iterion/commit/28f6ebea29885d154492588476fd5388aca67630)), closes [#478](https://github.com/SocialGouv/iterion/issues/478), references [#478](https://github.com/SocialGouv/iterion/issues/478)

    <details><summary>why</summary>

    `tools: [read_file, list_files]` on a claw node compiled clean. The run started, the worktree was prepared, the sandbox came up — and the first LLM node died on `unknown tool "list_files"`. The name is right there in the .bot: the failure was fully determined by the source, and it cost a launch to discover (found while dogfooding Copi, #478).

    </details>

## [3.57.0](https://github.com/SocialGouv/iterion/compare/v3.56.4...v3.57.0) (2026-08-23)

### Features

* **runtime:** bound the worktree pool — a full checkout per failed run, forever ([#477](https://github.com/SocialGouv/iterion/issues/477)) ([dc9a205](https://github.com/SocialGouv/iterion/commit/dc9a20562c7815c27a87905ffbedcd0e2d64ff31))

    <details><summary>why</summary>

    A `worktree: auto` run parks a FULL checkout of the repository under `<store>/worktrees/<run-id>`. A clean exit removes it; a failure keeps it, deliberately, for inspection. Nothing ever came back for those: `runs prune` only touches runs/, and `iterion clean` is a command you have to know exists. So a store whose runs fail grew by one checkout per failure, with no ceiling and no signal — 355 MB each on this repo, 309 MB of it the vendored tree. A studio left alone for forty minutes reached 32…

    </details>

### Bug Fixes

* **feed-watch:** a push refused for credentials is not a rebase conflict ([#457](https://github.com/SocialGouv/iterion/issues/457)) ([a1059c2](https://github.com/SocialGouv/iterion/commit/a1059c2dad178655dd54087a8ff67a8ebe2cc3be))

    <details><summary>why</summary>

    This morning's ux-metier digest died on

    </details>

## [3.56.4](https://github.com/SocialGouv/iterion/compare/v3.56.3...v3.56.4) (2026-08-23)

### Bug Fixes

* **pipelines:** a dispatcher give-up reaches Needs attention, not Closed ([#495](https://github.com/SocialGouv/iterion/issues/495)) ([95f1460](https://github.com/SocialGouv/iterion/commit/95f1460dc589b8df1e87867b694f885e99077dbe)), closes [#494](https://github.com/SocialGouv/iterion/issues/494)

    <details><summary>why</summary>

    When `iterion dispatch` exhausts `agent.max_attempts` it files the ticket into `agent.failed_state` (default `blocked`) itself — the same terminal state the board's Close writes. The projection read any terminal ticket as "the operator already filed this" and put the card in Closed, so the one class of failure the Needs-attention lane exists for (a pipeline that died and wants a human) was precisely the one it never showed: a deterministic failure burns the whole retry budget on every run.

    </details>

## [3.56.3](https://github.com/SocialGouv/iterion/compare/v3.56.2...v3.56.3) (2026-08-22)

### Bug Fixes

* **runner:** banking verifies the export delivered the pod's final tree ([a5c8b87](https://github.com/SocialGouv/iterion/commit/a5c8b87ae37e5477e14ac127b69e390fc5777de1))

    <details><summary>why</summary>

    An export-based sandbox (kubernetes) hands the runner a COPY of the pod workspace. When that copy arrives stale or empty, the host clone reads exactly like a run that made no commits, and bankRepoWorkspace concluded 'nothing to bank' — a silent total loss of a finished run's work (run 01a02a4b: gate converged citing its commit hashes, FinalBranch empty, zero commits recorded).

    </details>
* **sandbox:** stale host loose refs no longer shadow the exported pod refs ([09fb3bb](https://github.com/SocialGouv/iterion/commit/09fb3bb1a128b5f12101f429575919763319e2e9))

    <details><summary>why</summary>

    Adversarial review of the banking invariant found a reproducible false refusal: tar cannot delete, so when a pod-side `git gc` / `pack-refs --all --prune` moves a ref into packed-refs, the export overlay leaves the host's pre-run LOOSE ref in place — and git resolves loose before packed, so the exported clone reads a pre-run HEAD while every object actually arrived. The guard then refuses work that is sitting right there (and before the guard existed, this exact shape was a SILENT loss — it…

    </details>

## [3.56.2](https://github.com/SocialGouv/iterion/compare/v3.56.1...v3.56.2) (2026-08-22)

### Bug Fixes

* **runtime:** an unknown cost is not a free call ([#475](https://github.com/SocialGouv/iterion/issues/475)) ([12dd63d](https://github.com/SocialGouv/iterion/commit/12dd63d6bc2b7bbfd024e90d781d10c3cc044458)), references [#473](https://github.com/SocialGouv/iterion/issues/473)

    <details><summary>why</summary>

    `cost.Annotate` omits `_cost_usd` when no price resolves, and its doc is explicit that a zero there means "no cost data", never "this call was free" — callers must not record a $0 sample. The budget was the caller that did: `extractUsage` collapsed absent and zero into one float and `RecordUsage` added it to costUsed, so a run whose models are unpriced accumulated 0.00 per node. `max_cost_usd` never fired and the run finished with no budget event, indistinguishable from one that stayed under…

    </details>

## [3.56.1](https://github.com/SocialGouv/iterion/compare/v3.56.0...v3.56.1) (2026-08-22)

### Bug Fixes

* **studio:** show the resolved model and fallbacks on editor nodes ([#472](https://github.com/SocialGouv/iterion/issues/472)) ([0a32460](https://github.com/SocialGouv/iterion/commit/0a32460ad8420eae83f8d54db5f27c763623f68e))

    <details><summary>why</summary>

    The canvas replaced every ${VAR} model spec with the word "env", so gpt-5.6-sol / terra / luna were invisible next to the backend chip. Resolve the literal (authored default + /api/resolve-model), shorten the id, and render the authored fallbacks: chain on the card and in the inspector.

    </details>

## [3.56.0](https://github.com/SocialGouv/iterion/compare/v3.55.0...v3.56.0) (2026-08-21)

### Features

* **arbitrate:** Themis — a doctrine-bound judge for blocked divergence cases ([379ef20](https://github.com/SocialGouv/iterion/commit/379ef208859b242a71b4424a282acf082fe9d693))

    <details><summary>why</summary>

    A modernisation programme stalls each time a lot blocks on a divergence it may not close: the decision (canonicalise the platform noise, re-baseline through the ledger's rite, open a defect lot, or stop) belongs to the contract owner, and until now only a human could hold it. This bot delegates the DERIVABLE part and nothing else: one adversarial judge applies the target repository's WRITTEN arbitration doctrine — refuse by default, every proof read from a committed artifact, anything the…

    </details>

### Bug Fixes

* **dsl:** refuse bounded-iteration edges inside parallel branches (C244) ([#471](https://github.com/SocialGouv/iterion/issues/471)) ([55e44f5](https://github.com/SocialGouv/iterion/commit/55e44f5518804d6c42c38f30ea3d7ca5f055ee66))

    <details><summary>why</summary>

    fan_out_all, fan_out_each, and llm multi run through execBranch, which has no local loop counters. A declared loop compiled and was then skipped; a foreach with no `when` was taken as an unguarded unconditional back-edge. C243 rejects those edges. The runtime skip now covers IsBoundedIteration() (loop and foreach) as defence.

    </details>

## [3.55.0](https://github.com/SocialGouv/iterion/compare/v3.54.0...v3.55.0) (2026-08-21)

### Features

* **merge:** repo-targeted runs merge server-side — clone, merge, push to the forge ([7787036](https://github.com/SocialGouv/iterion/commit/7787036c0d9a36bdbc524e6161cc7d4fe068a057))

    <details><summary>why</summary>

    A repo-targeted run's workspace is wiped when it returns, so `runs merge` had nothing to stand in: mergeRepoRoot resolved to nothing and every merge died with "no resolvable repo root" — the storage branch the runner banked was unreachable by the product's own merge.

    </details>

### Bug Fixes

* **golden-master:** honour route exclusions written in the refusal's own format ([9b2c343](https://github.com/SocialGouv/iterion/commit/9b2c343986c0093e62074748660be0deb43a0f37))

    <details><summary>why</summary>

    The perimeter refusal prints uncovered routes as "METHOD /pattern" and asks for their exclusion in route-coverage.json — but the lookup only matched bare patterns, so an exclusion transcribed verbatim from the message could never count and the gate refused the same 51 routes it had just dictated. Both key shapes are now honoured: a bare pattern excludes the route for any method, a "METHOD pattern" key excludes exactly what the message named. Self-checks falsify both directions (transcribed form…

    </details>

## [3.54.0](https://github.com/SocialGouv/iterion/compare/v3.53.1...v3.54.0) (2026-08-21)

### Features

* **clean:** reclaim ${PROJECT_SCRATCH_DIR} — the one directory nothing swept ([#469](https://github.com/SocialGouv/iterion/issues/469)) ([a72c295](https://github.com/SocialGouv/iterion/commit/a72c295e7e461d7b47a0b2304f3890cfbb681b2a))

    <details><summary>why</summary>

    A workspace's out-of-tree scratch had no reaper at all: `runs prune` only touches runs/, the worktree sweep only worktrees/. One project reached 54 GiB of it — 59 state dirs of 1.1 GiB, none younger than three days, none reachable by any command.

    </details>

## [3.53.1](https://github.com/SocialGouv/iterion/compare/v3.53.0...v3.53.1) (2026-08-21)

### Bug Fixes

* **fork:** a repo-targeted fork carries its clone coordinates, never a dead pod's path — plus a named 400 for unforkable nodes and a budget hint that names the way out ([7c1d577](https://github.com/SocialGouv/iterion/commit/7c1d577d5bef79890274011edcd065c49a6b7777))
* **runner:** bank a repo-targeted run to the forge — worktree finalization never fires on this path, so a finished run's commits existed nowhere the server could reach ([38f1082](https://github.com/SocialGouv/iterion/commit/38f10820b54d10e1bab8d7db387efaccd46f5eee))

## [3.53.0](https://github.com/SocialGouv/iterion/compare/v3.52.0...v3.53.0) (2026-08-20)

### Features

* **health:** the probes echo the usage-cap policy — an unobservable guard cannot be trusted ([a984b10](https://github.com/SocialGouv/iterion/commit/a984b1066ad212f0f6868e74a34df5695d13d13c))
* **probes:** a lame-duck window, so a deploy stops refusing live connections ([#467](https://github.com/SocialGouv/iterion/issues/467)) ([d98af76](https://github.com/SocialGouv/iterion/commit/d98af765e87491e6a98f55d02fa454c889ebd99c))

    <details><summary>why</summary>

    On SIGTERM the server went straight to Shutdown. Endpoint removal is asynchronous, so for the 1-10s it takes to propagate, the listener was already closed while traffic was still routed here — a connection-refused: a 502 in the studio, a dropped delivery for a forge webhook. With server.hpa enabled by default that happened on every scale-down, not just on deploys.

    </details>
* **sandbox:** ship a ready JVM truststore beside the egress CA — six ecosystems read the CA env vars, a JVM reads none ([4ad6768](https://github.com/SocialGouv/iterion/commit/4ad6768334b4521c772913190224afabbdec41d0))

### Bug Fixes

* **delegate:** the CLI's no-credential render is an auth failure, not an answer ([e7a721e](https://github.com/SocialGouv/iterion/commit/e7a721e5e4d382cdaa276b9f614d8c37942abe9d))

## [3.52.0](https://github.com/SocialGouv/iterion/compare/v3.51.1...v3.52.0) (2026-08-20)

### Features

* **platform-creds:** DB-backed platform LLM credentials — rotate the fallback without a redeploy ([#466](https://github.com/SocialGouv/iterion/issues/466)) ([ecea9d5](https://github.com/SocialGouv/iterion/commit/ecea9d5574709c122aacc9ba05af48448ed7d049))

    <details><summary>why</summary>

    The credential a tenant-less run inherits — historically the runner pod's env (CLAUDE_CODE_OAUTH_TOKEN from the iterion-forfait k8s secret, ANTHROPIC_API_KEY et al.) — can now live sealed in Mongo, managed by super-admins and rotated with one call that every new launch AND resume picks up (SubmitResume re-resolves credentials). The env stays as the final backstop: an empty platform store keeps today's behaviour byte-identical.

    </details>

## [3.51.1](https://github.com/SocialGouv/iterion/compare/v3.51.0...v3.51.1) (2026-08-20)

### Bug Fixes

* **bots:** golden-master and modernize declare forge_token — repo-targeted cloud launches cloned credential-less ([8b31a54](https://github.com/SocialGouv/iterion/commit/8b31a54d8a6d14aa4178a3e7c83bc339bf74eb5b))
* **errtrack:** the independent transaction rides a cloned hub ([#464](https://github.com/SocialGouv/iterion/issues/464)) ([40eaaeb](https://github.com/SocialGouv/iterion/commit/40eaaebe950e2fc6871e0b28f4a1543f1e439ef3)), references [#463](https://github.com/SocialGouv/iterion/issues/463) [#463](https://github.com/SocialGouv/iterion/issues/463)

    <details><summary>why</summary>

    Revi's post-merge advisory on #463, verified red-first: sentry.StartSpan installs the new span on the hub's scope (tracing.go:210) and doFinish only restores the previous span for NON-transactions (tracing.go:472) — so StartIndependent on the process-global hub left its finished llm.generate transaction on the global scope forever, and every later captured error/panic inherited that unrelated trace context (reproduced: the regression test failed on the exact trace_id match before the fix). The…

    </details>
* **runner:** a credentials-inject error fails the attempt, and a credential-less clone failure names its probable cause ([3f4a56e](https://github.com/SocialGouv/iterion/commit/3f4a56e2ea61a130f68bf486dba9de9b1829db0a))

## [3.51.0](https://github.com/SocialGouv/iterion/compare/v3.50.2...v3.51.0) (2026-08-20)

### Features

* **golden-master:** standard 3 — the net states its FEATURES, not only its routes ([7b04458](https://github.com/SocialGouv/iterion/commit/7b04458ce937c58bf306cb1a0a36774494e7cd01))

    <details><summary>why</summary>

    A route the corpus touches once is not a feature it exercises. A net declaring standard 3 must ship a feature_probe (two independent witnesses: the served navigation graph and the tree's own catalogues) and a committed feature-coverage.json mapping every feature to corpus entries or excluding it in writing. The gate refuses unmapped, stale and broken-evidence inventories by name; existing nets keep their declared standard, and the verdict carries the figure so a below-standard net is visible,…

    </details>
* **golden-master:** the seal opt-in becomes committable — config.json's seal_committed, auditable like the rest ([d8ee9ef](https://github.com/SocialGouv/iterion/commit/d8ee9ef72d7ef563fb7bf54451d7463ed2d55ec3))
* **modernize:** defect disposition — fixed and recorded by default, preserved only by written business decision ([8af1388](https://github.com/SocialGouv/iterion/commit/8af13882835620afb2613ed50b82bf964ae2dd37))
* **modernize:** outcomes — what the programme owes becomes a conjunction term of its convergence ([f0e5332](https://github.com/SocialGouv/iterion/commit/f0e53329ea66662cd50339aece73f867ad72fa61))
* **observability:** opt-in Sentry tracing — API transactions + LLM-call spans (Obsy dogfood) ([#463](https://github.com/SocialGouv/iterion/issues/463)) ([39ebe8a](https://github.com/SocialGouv/iterion/commit/39ebe8aa7adc8845d22a9f23bf5b04cec130fcbe))

    <details><summary>why</summary>

    Tracing rides the SAME client and DSN as error tracking; sentry-go does not read SENTRY_TRACES_SAMPLE_RATE on its own, so Init resolves it and sets EnableTracing/TracesSampleRate. Unset, 0, unparsable or out of [0,1] ⇒ strictly off even with a DSN — and the refusal is loud, but never costs error tracking. NaN is rejected explicitly: it fails every ordered comparison, so the naive range check would let it through.

    </details>

### Bug Fixes

* **golden-master,modernize:** the adversarial review's findings — a ratchet for the standard, named refusals everywhere, and skills that say exactly what is checked ([a6d5b8f](https://github.com/SocialGouv/iterion/commit/a6d5b8f46589e50c79b8d77057de4fb7f3eb4f46))

    <details><summary>why</summary>

    H2: standard-mark makes a silent 3->2 downgrade a named refusal in both drift directions. M2/M3: malformed feature-coverage and mistyped seal opt-ins bail with their cause instead of a traceback or a silence. M1: holdout_awaiting_gate is a report field, not only a notice. H1/C1/B2: the skills now state exactly what the harness checks (union-level source labels), where the outcomes conjunction is enforced today (the campaign runner, not this graph — roadmap named), and that SSR stacks DO expose…

    </details>
* **golden-master:** a gate never seals a COMMITTED held-out set — it awaits its own gate ([26bd79a](https://github.com/SocialGouv/iterion/commit/26bd79a0798b6b0e953052a73492798158da0166))

    <details><summary>why</summary>

    The seal moves the set out of the tree, which strips TRACKED files when the set was committed by an earlier authoring run: the finalize then refuses the dirty tree, and the set's single scoring burns on a gate that does not own it. A committed set is now left in place and noted; the convergence gate that owns it opts in explicitly with GM_SEAL_COMMITTED=1 — a flag that can only widen what the gate consumes, never soften a verdict. Selftest 45 -> 48.

    </details>
* **golden-master:** round-2 adversarial findings — every refusal named, in every type ([703cd81](https://github.com/SocialGouv/iterion/commit/703cd8183348267aaed385ee5591d2646f57ed9d))

    <details><summary>why</summary>

    Non-string reasons and feature ids, unreadable standard-marks, mistyped GM_SEAL_COMMITTED spellings, valid-JSON-non-object ledger blocks: each was a traceback or a silence, each is now a named refusal. A feature mapped twice or both mapped and excluded refuses (nobody chose). A mid-path ** in a route pattern refuses (the doctrine says a tail). The standard-raise doctrine now states the PAIRED-commit rule the code enforces. Selftest 57 -> 66.

    </details>
* **golden-master:** round-3 findings — the refusal envelope survives every path ([b99964f](https://github.com/SocialGouv/iterion/commit/b99964fad5e6d251c39827d47792099ab3756097))

    <details><summary>why</summary>

    A ledger block that is a dict without a string id escalates as UNPARSEABLE instead of a KeyError two functions later; the seal opt-in refusal reaches the supervisor through bail()'s JSON envelope like every other refusal; a feature excluded twice refuses (two reasons, nobody chose). No false-green found this round. Selftest 66 -> 68.

    </details>
* **modernize:** the boolean-attribute probe becomes mechanical — rendered DOM, anchors included ([4ad6952](https://github.com/SocialGouv/iterion/commit/4ad6952197ea02b005f415561090c7c7933462fc))
* **modernize:** the judge's INPUTS are immutable too — a corpus can be amputated in silence ([fb4a453](https://github.com/SocialGouv/iterion/commit/fb4a453d5b089fbe902df26c21a8cb19b72336e5))

    <details><summary>why</summary>

    Narrowing corpus.json, or widening the coverage exclusions, shrinks what the net measures without moving one reference — and a green cannot report that shrinkage. The immutability check now covers what decides the verdict in all three families: the references compared, the machinery comparing them, and the inputs saying what to measure. Paths are quoted.

    </details>
* **modernize:** the judge's own machinery is as immutable as its references ([9f6ed9a](https://github.com/SocialGouv/iterion/commit/9f6ed9ac861504f59d643572c6fea68c5a50471e))

    <details><summary>why</summary>

    A lot that rewrites verify-oracle.sh or harness.py makes every later gate answer green without measuring anything, and no reference moves — measured as a real gap. refs_untouched now diffs the runner, the harness and the canonicaliser alongside refs/.

    </details>

## [3.50.2](https://github.com/SocialGouv/iterion/compare/v3.50.1...v3.50.2) (2026-08-19)

### Bug Fixes

* **bots:** the clean-tree ratchet reads the whole clause, and the verb reaches the goal ([#462](https://github.com/SocialGouv/iterion/issues/462)) ([62b1d84](https://github.com/SocialGouv/iterion/commit/62b1d84b5d4b8399d12420f04b5ed7937bf435e7)), references [#461](https://github.com/SocialGouv/iterion/issues/461)

    <details><summary>why</summary>

    Revi's re-review of #461 (on the zero-touch lane's own commits) left two verified findings:

    </details>

## [3.50.1](https://github.com/SocialGouv/iterion/compare/v3.50.0...v3.50.1) (2026-08-19)

### Bug Fixes

* **fleet:** fold Revi's advisory findings into the follow-ups ([#461](https://github.com/SocialGouv/iterion/issues/461)) ([2b7abab](https://github.com/SocialGouv/iterion/commit/2b7abab9ec43abad1531d35f5eff552dd5e17cac))

    <details><summary>why</summary>

    The bare `!third_party/codex-agent-sdk-go/.claude/` negation re-included the WHOLE subtree, not just the fork's committed `rules/`: the root-anchored `.claude/skills/` rule on line 145 does not reach that depth, so `…/.claude/skills/JUNK.md` and `…/.claude/settings.local.json` came back as untracked. That is exactly the runtime junk `**/.claude/` exists to stop, and the fork is an active work target whose campaign bots commit with `git add -A`.

    </details>

## [3.50.0](https://github.com/SocialGouv/iterion/compare/v3.49.0...v3.50.0) (2026-08-19)

### Features

* **observability:** Sentry/GlitchTip error tracking + JSON-default logs (Obsy dogfood) ([#459](https://github.com/SocialGouv/iterion/issues/459)) ([e37a117](https://github.com/SocialGouv/iterion/commit/e37a117841cdf6789c182b4fd34510b670c54e10))

    <details><summary>why</summary>

    Adds Logger.SetHook: a callback invoked for every record at or above warn with the record's message and a private copy of its inherited fields. The slot is shared across WithField/WithFields forks like the writer and mutex, so a hook installed on the root logger reaches forks made before or after SetHook.

    </details>

## [3.49.0](https://github.com/SocialGouv/iterion/compare/v3.48.3...v3.49.0) (2026-08-19)

### Features

* **instrument:** Obsy — Sentry/GlitchTip error tracking + log standardization bot ([#458](https://github.com/SocialGouv/iterion/issues/458)) ([40c96da](https://github.com/SocialGouv/iterion/commit/40c96da4f47a23465e55ac7b50b6112c3e319883))

    <details><summary>why</summary>

    New catalog bot on the proven ADR-058 chassis (feature-dev/Billy shape): ONE adaptive campaign agent + deterministic build/test gate + in-loop adversarial review + bounded continuation loop + opt-in PR tail.

    </details>

## [3.48.3](https://github.com/SocialGouv/iterion/compare/v3.48.2...v3.48.3) (2026-08-19)

### Bug Fixes

* **dispatcher:** a paused human-review run survives a restart ([#455](https://github.com/SocialGouv/iterion/issues/455)) ([bdc14ef](https://github.com/SocialGouv/iterion/commit/bdc14ef6cb60e6550912710967d4f16bb34b2b31))

    <details><summary>why</summary>

    After a reboot, sweepStaleLocalClaimsAtBoot frees the claim that parked a card whose last run sits on a human node, and resumableRunID excludes paused_waiting_human by design — so the next tick minted a fresh run from the workflow entry, silently superseding the paused one.

    </details>

## [3.48.2](https://github.com/SocialGouv/iterion/compare/v3.48.1...v3.48.2) (2026-08-19)

### Bug Fixes

* **feed-watch:** a flaky aggregator costs one retry, not a day of veille ([#456](https://github.com/SocialGouv/iterion/issues/456)) ([1f71fed](https://github.com/SocialGouv/iterion/commit/1f71fed21150c99ee67a047174b1d92a19e50c86))

    <details><summary>why</summary>

    hnrss.org returned 502 for 9 of 69 feeds on a production run and served all of them seconds later. A feed missed is not a feed deferred: nothing re-reads the window nobody fetched, so nine sources went silently absent from that day's digest.

    </details>

## [3.48.1](https://github.com/SocialGouv/iterion/compare/v3.48.0...v3.48.1) (2026-08-19)

### Bug Fixes

* **byok:** a key created for another team is stamped with THAT team ([#436](https://github.com/SocialGouv/iterion/issues/436)) ([26dc032](https://github.com/SocialGouv/iterion/commit/26dc0329a76f2e25b8265d7b6cbe25745f2faadb))

    <details><summary>why</summary>

    The api-keys store derives tenant_id from the context — on write it stamps the row, on read it filters — and requireAuth stamps the CALLER'S ACTIVE team. The team-scoped routes never re-scoped, so a key created for a team other than the caller's own landed as (scope_team = target, tenant_id = caller's active team): listable from the context that created it, and invisible to the runs of the team it was meant to fund.

    </details>

## [3.48.0](https://github.com/SocialGouv/iterion/compare/v3.47.2...v3.48.0) (2026-08-19)

### Features

* **devbox:** a run that reads a repo need not build it ([#450](https://github.com/SocialGouv/iterion/issues/450)) ([34ba661](https://github.com/SocialGouv/iterion/commit/34ba6618296c642a2ece2611c307384d2ae21a3a))

    <details><summary>why</summary>

    Two devbox.json files can supply a run's binaries — the bot's own and the target repo's — and until now both installed, always. But "this repo pins a toolchain" and "this run needs that toolchain" are different claims. A review reads a diff and writes comments; it built nothing, and it paid iterion's own 319 Nix paths, 406 MiB downloaded, 1.8 GiB unpacked, a desktop GUI stack included, before its first node. Twice today that cold realise outlasted the window the sandbox had to come up, and the…

    </details>
* **feed-watch:** a digest that has nothing to say, says so ([#454](https://github.com/SocialGouv/iterion/issues/454)) ([2902cac](https://github.com/SocialGouv/iterion/commit/2902caca02f0c92022f2c4488a0a8fa8bc8ac24e))

    <details><summary>why</summary>

    An empty queue makes a digest exit at plan -> load_pending -> done: no LLM, no post, status finished. Correct behaviour, and indistinguishable from a healthy quiet week — every morning, for as long as it lasts. From 13 to 18 August five daily cyber digests did exactly that while the collector fed nothing, and nobody learned anything until the operator asked.

    </details>

## [3.47.2](https://github.com/SocialGouv/iterion/compare/v3.47.1...v3.47.2) (2026-08-18)

### Bug Fixes

* **runtime:** a run drained before its first node is resumable ([#449](https://github.com/SocialGouv/iterion/issues/449)) ([4314aa0](https://github.com/SocialGouv/iterion/commit/4314aa0107c48d65db0d1d3206ebc15909e9027a))

    <details><summary>why</summary>

    Setup runs on the same ctx the node loop does, so the same two interruptions reach it: a runner drained mid-rollout, and an operator cancelling. The node loop classifies both — handleContextDoneWithCheckpoint writes failed_resumable for a drain and cancelled for an operator — while every pre-execLoop phase wrote a flat, terminal "failed".

    </details>

## [3.47.1](https://github.com/SocialGouv/iterion/compare/v3.47.0...v3.47.1) (2026-08-18)

### Bug Fixes

* **store:** a directory holding only a lock is not a run ([#437](https://github.com/SocialGouv/iterion/issues/437)) ([d9a6814](https://github.com/SocialGouv/iterion/commit/d9a6814cd111a3340e2259f5a01551cbcf5cf439))

    <details><summary>why</summary>

    LockRun mkdirs the run directory to place its .lock, so an id that is locked and then never created — an abandoned launch, a crash between the lock and the first write — leaves a directory carrying nothing else. ListRuns reported it as a run, permanently: every LoadRun on it fails, and a consumer that reads the first id it is handed waits on a run that will never load. That is what made TestProcessBoardCardCarriesPRLaunchContext hang for its full 30s while the run it was waiting for sat behind…

    </details>

## [3.47.0](https://github.com/SocialGouv/iterion/compare/v3.46.2...v3.47.0) (2026-08-18)

### Features

* **feed-watch:** a digest dates the window it covers, not today ([#452](https://github.com/SocialGouv/iterion/issues/452)) ([1a6eb42](https://github.com/SocialGouv/iterion/commit/1a6eb42bb649407d218d4ceda9c2208f41e7fa6b))

    <details><summary>why</summary>

    A digest drains a QUEUE, not a day. That distinction is invisible until something interrupts the run: a shut usage window, a paused schedule, a feed that came back. Then the queue holds five days of material and the digest presents it as this morning's news — dated today, ranked as breaking.

    </details>

## [3.46.2](https://github.com/SocialGouv/iterion/compare/v3.46.1...v3.46.2) (2026-08-18)

### Bug Fixes

* **usagecap:** "contains a model node" is not "will call one" ([#453](https://github.com/SocialGouv/iterion/issues/453)) ([785288c](https://github.com/SocialGouv/iterion/commit/785288cad66c33daa91f33f69ae5046a7771a17c)), references [#451](https://github.com/SocialGouv/iterion/issues/451)

    <details><summary>why</summary>

    The guard shipped in #451 asked whether a workflow CONTAINS something that can call a model. That is not the question a pre-flight needs, and on the very bot it was written for it answers wrong.

    </details>

## [3.46.1](https://github.com/SocialGouv/iterion/compare/v3.46.0...v3.46.1) (2026-08-18)

### Bug Fixes

* **usagecap:** a cap on model spend must not stop a run that spends none ([#451](https://github.com/SocialGouv/iterion/issues/451)) ([e88f65e](https://github.com/SocialGouv/iterion/commit/e88f65e7ea048e30133d19217f1e225873f60d0b))

    <details><summary>why</summary>

    The pre-flight refused every claimed run while the window was shut, without asking whether the run could draw on it. A workflow made of tool and compute nodes cannot: there is no model call to bill. Refusing it protects nothing.

    </details>

## [3.46.0](https://github.com/SocialGouv/iterion/compare/v3.45.1...v3.46.0) (2026-08-17)

### Features

* **usage-cap:** stop below the provider's wall, at a percentage you choose ([#438](https://github.com/SocialGouv/iterion/issues/438)) ([ccf178d](https://github.com/SocialGouv/iterion/commit/ccf178dbc7f66036bcbece0977f1717dedbc833c))

    <details><summary>why</summary>

    A subscription meters two rolling windows and refuses every call once one is exhausted. iterion already survived that refusal — the run parks and a durable retry resumes it when the window reopens. It could not stop BEFORE the wall, and the wall is rarely where an operator wants to be: the same subscription usually pays for their own interactive work, so a fleet of bots that drives it to 100% takes the human down with it.

    </details>

## [3.45.1](https://github.com/SocialGouv/iterion/compare/v3.45.0...v3.45.1) (2026-08-17)

### Bug Fixes

* **forge:** a GitLab gate claim over an existing claim is claimed, not failed ([#435](https://github.com/SocialGouv/iterion/issues/435)) ([7411369](https://github.com/SocialGouv/iterion/commit/7411369f957535bccc9f9ff99cb96b3ac2ec99f2))

    <details><summary>why</summary>

    GitLab's commit-status state machine refuses pending -> pending with HTTP 400 ("Cannot transition status via :enqueue from :pending"), where GitHub accepts the same POST as a no-op. The only writer that posts pending is the merge gate's in-flight claim, so the rejection meant the check read "absent" while a review was in fact running -- observed live on a repo where a second bot was invoked on a head another had already claimed.

    </details>

## [3.45.0](https://github.com/SocialGouv/iterion/compare/v3.44.0...v3.45.0) (2026-08-17)

### Features

* **clean:** a run you can resume still owns its worktree ([a902f11](https://github.com/SocialGouv/iterion/commit/a902f117d38d50f544f4041908a6ec2632583209))

    <details><summary>why</summary>

    Eighth pass. The first whose end-to-end campaign — 18 worktrees, every class, both layouts, concurrent sweeps, re-runs — found no way to lose data. What it found instead was a question asked wrong since the first commit.

    </details>
* **clean:** the leftover checkouts reclaimed — landing decides, not age ([c116029](https://github.com/SocialGouv/iterion/commit/c1160294e56473065907c6864c830aa91b4c7a77))

    <details><summary>why</summary>

    A `worktree: auto` run that succeeds removes its checkout; one that fails or is interrupted leaves it behind for inspection and never comes back for it. On a long-lived store that pool is where the disk goes, and `runs prune` cannot reach it — its own doc says it never touches worktrees/.

    </details>
* **golden-master:** a converged campaign leaves a mergeable tree ([d2bbf23](https://github.com/SocialGouv/iterion/commit/d2bbf23e43b3dff6d7fcc4e5c752b8640a586cb3))

    <details><summary>why</summary>

    Two sources of end-of-run dirt, each of which forced a human landing: the harness self-copy now materialises ONE canonical form and writes only on change (no more header churn between the standalone and the inlined node), and emit_runner commits its own emissions — runner, report, harness, a bytecode-cache gitignore — scoped to the oracle dir. A landed-by-hand net is where a hardened runner got clobbered once.

    </details>
* **golden-master:** a pending re-baseline request blocks the gate ([369f6d3](https://github.com/SocialGouv/iterion/commit/369f6d3e33e7286335075ae95c0ed3d3c083ffe5))

    <details><summary>why</summary>

    Four requests sat unacted behind four consecutive green convergences: each one quarantines known-diverging entries out of the verdict, so the gate was green AROUND a narrowing net — the failure this bot exists to catch, one level up. Pending requests are now a conjunction term, parsed from the ledger's machine blocks; supersedence becomes the declared 'replaces' field (prose does not count), and an unparseable block is an escalation, never a guess. Selftest 32 -> 38.

    </details>

### Bug Fixes

* **clean:** a bare repo is still a repo, and a photograph is not the present ([c6f610f](https://github.com/SocialGouv/iterion/commit/c6f610f0e46c1284d50300291bd1f1ad79361454))

    <details><summary>why</summary>

    Third adversarial pass, aimed at what the second one rewrote — which is where it found most of what follows.

    </details>
* **clean:** a guard buried in `err == nil` is a guard that disappears ([3404f19](https://github.com/SocialGouv/iterion/commit/3404f19ddf702d04763e235a53120ec6ff80e409))

    <details><summary>why</summary>

    Sixth pass, on the fifth's own code.

    </details>
* **clean:** a repository that answers about itself proves nothing ([cf3c94b](https://github.com/SocialGouv/iterion/commit/cf3c94bb57e5eb628bd8857017c26bbb96c279db))

    <details><summary>why</summary>

    Fifth pass. It overturns one of the fourth's fixes, which is the honest outcome: what round 4 removed as a false positive was the only thing standing between a self-contained clone and its own destruction.

    </details>
* **clean:** git answers in absolute paths, and a store dir need not ([ec3f9cb](https://github.com/SocialGouv/iterion/commit/ec3f9cb5c7fe94c9a9aeaaf9735cd1d83dcd0da7))

    <details><summary>why</summary>

    Seventh pass. Six rounds had been finding regressions of the round before; this one found something that had been there since the first commit and that every guard added since silently depends on.

    </details>
* **clean:** git must be talking about THIS directory, and merged must mean built upon ([2c0050a](https://github.com/SocialGouv/iterion/commit/2c0050a882a8f966632c4c6c01bce72fb85bbe45))

    <details><summary>why</summary>

    An adversarial pass broke the first cut in ways that destroyed work at the default level. Each of these was reproduced before it was fixed.

    </details>
* **clean:** re-derive the whole verdict before deleting, not just the dirty bit ([5de5151](https://github.com/SocialGouv/iterion/commit/5de51512c060c226d638db84fd60c155b374721d))

    <details><summary>why</summary>

    Fourth and last adversarial pass, aimed at what the third one wrote.

    </details>
* **clean:** take the run's lock, and stop calling "we could not tell" a leftover ([9e53b63](https://github.com/SocialGouv/iterion/commit/9e53b638aa3996259c6a081c3fb6ab267ca66fa5))

    <details><summary>why</summary>

    Second adversarial pass. The first one's fixes were right and incomplete: what it left open destroyed work, and what it over-refused made the command nearly useless.

    </details>
* **golden-master:** the emitted runner keeps the log out of the verdict it re-reads ([61324fe](https://github.com/SocialGouv/iterion/commit/61324feb6082d225eb0ad0496e83951a56fc680a))

    <details><summary>why</summary>

    The template piped the harness's stderr into the JSON report, so the runner died parsing its own progress messages — a campaign found it, hardened its materialised copy, and the next emission reintroduced the defect from this template. Fixed where it is emitted from.

    </details>
* **server:** resume dispatcher child gates from persisted source ([#433](https://github.com/SocialGouv/iterion/issues/433)) ([4876453](https://github.com/SocialGouv/iterion/commit/4876453d12f579a6317446da762f2abe76ef7cb8))

## [3.44.0](https://github.com/SocialGouv/iterion/compare/v3.43.0...v3.44.0) (2026-08-14)

### Features

* **golden-master:** the corpus states its perimeter, and watches what teams skip ([75024e2](https://github.com/SocialGouv/iterion/commit/75024e2726286c91de262dc9ca2d76f66c0ae6e7))

    <details><summary>why</summary>

    Five required corpus probes — a creation, the error-then-corrected journey, a case pair, a text sort, a login case variant — enforced mechanically before boot, exactly like the mutant archetypes: a tag without its shape does not count. A routes_probe + route-coverage gate names every route the corpus never touches, unless its exclusion carries a written reason. Write entries gain multi-step journeys in one session. The write surface gains a create_lost archetype. Fixture doctrine: seeded id…

    </details>
* **modernize:** crossing a major owes a sweep — eight drift classes on the record ([dbdc109](https://github.com/SocialGouv/iterion/commit/dbdc109d3f939085d26f499ce8eabf49e62a5fba))

    <details><summary>why</summary>

    A major redefines semantics under unchanged lines: routing edge forms, implicit binding conversion, query strictness, template attribute semantics, dialect functions, seed allocator state, collation, and defaults that flip silently. The upgrade-archetypes skill carries the eight classes as data; each is instantiated from the migration notes of the exact major crossed, probed mechanically, and recorded in the tree. The plan contract gains crosses_major, and the sweep record is checked by the…

    </details>

### Bug Fixes

* **golden-master:** seal the held-out set where every runtime can write ([7ef20a1](https://github.com/SocialGouv/iterion/commit/7ef20a198758cd8100db501600101ad9567f81a1))

    <details><summary>why</summary>

    The sealed pile lived beside the workspace, which assumes a writable parent — a sandboxed run mounts the worktree's parent read-only and the first seal died on it. The default root is now the system temp dir, and the name hashes the absolute workspace path: sibling worktrees of one repo share a basename, and under a common root they would otherwise seal into each other's pile. GM_SEALED_DIR and GM_SCRATCH still override.

    </details>
* **pipelines:** stop leaking review-scope banners when stepping turns ([#427](https://github.com/SocialGouv/iterion/issues/427)) ([b80ed51](https://github.com/SocialGouv/iterion/commit/b80ed513cf3a87bfb2b5e1a9d84fd7e8b365189e))

    <details><summary>why</summary>

    ReviewScopePanel and HumanPromptForm shared the same React key as siblings. React's remaining-children map keeps only the last child per key, so Prev/Next unmounted the form and left every previous ReviewScopePanel mounted. A card with dozens of human gates then stacked "No file diff for this review" once per visit.

    </details>

## [3.43.0](https://github.com/SocialGouv/iterion/compare/v3.42.0...v3.43.0) (2026-08-13)

### Features

* **studio:** preview JSON, markdown and text on human gates ([#425](https://github.com/SocialGouv/iterion/issues/425)) ([919efc3](https://github.com/SocialGouv/iterion/commit/919efc3f59b5a343bee7a7d0d0652100c46e1a88))

    <details><summary>why</summary>

    Inbound file fields only rendered images, audio and video. A planner outline.json or a review brief therefore collapsed to Download, so the operator answered the gate without seeing the document.

    </details>

### Bug Fixes

* **backends:** restore Codex as a supported backend ([#419](https://github.com/SocialGouv/iterion/issues/419)) ([9db47d2](https://github.com/SocialGouv/iterion/commit/9db47d2e9b2a55c20677c81ea160b2b7af0dc025))

## [3.42.0](https://github.com/SocialGouv/iterion/compare/v3.41.1...v3.42.0) (2026-08-13)

### Features

* **ultra11y:** an accessibility auditor whose findings no model can drop ([#409](https://github.com/SocialGouv/iterion/issues/409)) ([e5ca08f](https://github.com/SocialGouv/iterion/commit/e5ca08fd06a588cba526369df8563ea3b439553f)), references [maxgfr/ultra11y#15](https://github.com/maxgfr/ultra11y/issues/15)

    <details><summary>why</summary>

    Acci's own bilan records the failure this bot exists to remove. On run 019f3d3b-7aea the RGAA review found four real defects, emitted them without the `status` field the gates count by, and the report published "0 non conformes" — four true findings erased between the agent and the deliverable. The gates were right. The DETECTOR was a language model, so a dropped field was indistinguishable from a clean repo, and the fix that day (count a status-less finding as NC) is damage control around the…

    </details>

## [3.41.1](https://github.com/SocialGouv/iterion/compare/v3.41.0...v3.41.1) (2026-08-13)

### Bug Fixes

* **vetty:** a verify that overruns is a red build, not a crashed run ([#422](https://github.com/SocialGouv/iterion/issues/422)) ([004abce](https://github.com/SocialGouv/iterion/commit/004abce72989d4e3337d11a8a7f68757b90bc0e9)), references [iterion#386](https://github.com/iterion/issues/386) [#412](https://github.com/SocialGouv/iterion/issues/412)

    <details><summary>why</summary>

    Observed on iterion#386 today: a 20-minute verify ended the RUN, not the build. TimeoutExpired was caught and then the handler itself raised — e.output is bytes on the timeout path even though text=True was passed, because the decode happens after communicate() returns normally and a timeout attaches the raw buffer instead. bytes + str is a TypeError, so the operator got failed_resumable and a traceback where a verdict belonged.

    </details>

## [3.41.0](https://github.com/SocialGouv/iterion/compare/v3.40.6...v3.41.0) (2026-08-13)

### Features

* **forge:** GitLab reads commit statuses, and a repo launch URL is canonical ([#421](https://github.com/SocialGouv/iterion/issues/421)) ([543910d](https://github.com/SocialGouv/iterion/commit/543910ddfa8edd987a48c137359f0cf4b34cc09a))

    <details><summary>why</summary>

    GitLab's AdminClient gains ListCommitStatuses (the CommitStatusLister capability), the read half of the merge gate: without it the gate reconciler must abstain on GitLab — a review that dies leaves the required context absent forever — and the auto-fix lane can never launch, since it refuses to act on a gate it cannot see. GitLab returns every status row on a commit (retries included), so the result keeps only the newest row per name; a gate reader handed raw history could match a stale verdict…

    </details>

## [3.40.6](https://github.com/SocialGouv/iterion/compare/v3.40.5...v3.40.6) (2026-08-12)

## [3.40.5](https://github.com/SocialGouv/iterion/compare/v3.40.4...v3.40.5) (2026-08-12)

## [3.40.4](https://github.com/SocialGouv/iterion/compare/v3.40.3...v3.40.4) (2026-08-12)

## [3.40.3](https://github.com/SocialGouv/iterion/compare/v3.40.2...v3.40.3) (2026-08-12)

### Bug Fixes

* **vetty:** a hold must say what broke, and a task body is not a command ([#412](https://github.com/SocialGouv/iterion/issues/412)) ([c033cdd](https://github.com/SocialGouv/iterion/commit/c033cdd2d1f1cdc8ffd386d0d1510381a418f1f6))

    <details><summary>why</summary>

    Three holds on 2026-08-12 reported "build/tests not green" over an excerpt containing nothing but a list of ok lines and a bare FAIL. The report carried out[-4000:], and a test runner prints its per-package successes after the package that failed — so the blind tail is systematically the wrong excerpt. It now carries the matched failing lines too.

    </details>

## [3.40.2](https://github.com/SocialGouv/iterion/compare/v3.40.1...v3.40.2) (2026-08-12)

### Bug Fixes

* **expr:** concat() is the array builtin — string joins use +, and a guard now says so ([8125c06](https://github.com/SocialGouv/iterion/commit/8125c0660f3bc7fd3abf9d368efd5a659dff9303))

    <details><summary>why</summary>

    A compute expression written concat('prefix: ', outputs.x.log) parses, compiles and validates clean, then dies at evaluation with "want array". It lives in notice/fail_log fields inside if(converged, …, <the concat>) — the FAILURE branch — so it crashes only once something else has already gone wrong, turning a reported failure into a dead run. It had shipped in a bundled subbot's refusal path; a sibling bot hit the same shape on its first real run and exposed the class.

    </details>
* **review-env:** an attached skill is a FILE, not a registry entry ([82b8eff](https://github.com/SocialGouv/iterion/commit/82b8efffba96580dece24df0f9c63afe2cb2073f))

    <details><summary>why</summary>

    First real run: the plugin mirrored deploy-target.md into the workspace's .claude/skills/, the agent asked the Skill tool for it by name, got "Unknown skill" — that registry carries the bundle's own skills — and correctly refused to deploy. The refusal worked; the premise was wrong. The prompt now names the mirrored PATH as the authority (the pattern app-dev's validated deploy phase already used) and says a missing registry entry is not evidence of a missing skill.

    </details>
* **review-env:** the cluster is the authority on pullability, not an anonymous probe ([e14360c](https://github.com/SocialGouv/iterion/commit/e14360c211e767f1213e0b979396a4789845534c))

    <details><summary>why</summary>

    Two runs refused to deploy a NAMED image because a probe of the private registry answered 401 — a probe the agent was never meant to run: the pull credential lives in the namespace as an operator-provisioned imagePullSecret it cannot read, by design. So it predicted the failure instead of measuring it, and left the only question that matters unanswered. The prompt now says: once you can name the image, apply and let the rollout answer; ImagePullBackOff is a measurement you report.

    </details>
* **review-env:** the mounted path is the rendered template, not the env var ([e18e9bc](https://github.com/SocialGouv/iterion/commit/e18e9bc618ba5d7ac36f4e39b60232ac81211406))

    <details><summary>why</summary>

    Measured on the first deploying run: a secret's env: declaration is injected into TOOL nodes (executor_tool, verified actions), not into a delegated agent's shell — $DEPLOY_CREDENTIAL was unset and the agent had to recover the path from the task header. The prompt now gives {{secrets.deploy_credential}} as the reliable path and says the unset env var is expected, not evidence of a missing credential.

    </details>

## [3.40.1](https://github.com/SocialGouv/iterion/compare/v3.40.0...v3.40.1) (2026-08-12)

## [3.40.0](https://github.com/SocialGouv/iterion/compare/v3.39.0...v3.40.0) (2026-08-12)

### Features

* **review-env:** a live review environment as one leasable capability ([1757a67](https://github.com/SocialGouv/iterion/commit/1757a67d852ec798e55b95465add0071d9739c26))

    <details><summary>why</summary>

    Deploys the workspace's already-CI-published image to the operator-attached platform and hands back a live https URL — realism for end-to-end tests, captures and review that localhost cannot give. The platform lives ENTIRELY in the attached deploy-target skill (one plugin enabled per instance; swapping infrastructure = swapping the plugin + the deploy_credential secret, never the bot), the credential is used strictly by reference, the image is the repo's own CI's, and the URL verdict is…

    </details>

### Bug Fixes

* the last two Vetty holds, and the git-env scrub everywhere it belongs ([#407](https://github.com/SocialGouv/iterion/issues/407)) ([bcc0952](https://github.com/SocialGouv/iterion/commit/bcc0952650e9831a17d98fb4be7c1155269ccaa7)), references [#390](https://github.com/SocialGouv/iterion/issues/390) [#405](https://github.com/SocialGouv/iterion/issues/405) [iterion#398](https://github.com/iterion/issues/398) [iterion#398](https://github.com/iterion/issues/398)

    <details><summary>why</summary>

    Two of the eleven Dependabot PRs from 2026-08-10 were held as "build/tests not green" without a build ever having been established.

    </details>

## [3.39.0](https://github.com/SocialGouv/iterion/compare/v3.38.1...v3.39.0) (2026-08-11)

### Features

* **campaign:** a deterministic supervisor carries a whole programme, lot after lot ([e693a02](https://github.com/SocialGouv/iterion/commit/e693a025cbe3a1144ee179821a5f6e2e80b9b00b))

    <details><summary>why</summary>

    modernize takes ONE lot per run by design; the programme is a suite of runs, and someone has to be the suite. That someone was a human first: a full modernisation programme was replayed end to end under manual supervision and the interventions were counted — nearly all mechanical, and the acceptance criterion held on every re-record act (observed diff == announced set, every time). This bot mechanises exactly what was mechanical and routes the rest to a human.

    </details>
* **golden-master:** the ledger speaks a machine-readable protocol — request, act, verdict ([6eee2c2](https://github.com/SocialGouv/iterion/commit/6eee2c22034b59e74be3702897b348d78c381a54))

    <details><summary>why</summary>

    A worker that may not re-record announces; the party that owns the net answers. Three HTML-comment blocks in REBASELINE.md carry the protocol so a supervising process can execute the separation of powers mechanically: a request names its lot, its cause and the EXACT expected paths; an act is written only when the observed diff equals that announcement; a verdict only when the full counter-test replayed green on the committed tree.

    </details>

### Bug Fixes

* **campaign:** bool/json inputs render as JSON literals — name the atoms ([1545155](https://github.com/SocialGouv/iterion/commit/15451551a783579a2a75e5e3818970a90fecb7f1))

    <details><summary>why</summary>

    {{input.moved}} arrives in a python script as the token 'false', and any json field can carry true/false/null inside. Found by the bot's second real run: the steward crashed on NameError. One line gives the three JSON atoms their Python names before interpolation.

    </details>
* **campaign:** the engine's materialised node script is not work in flight ([15ab7b0](https://github.com/SocialGouv/iterion/commit/15ab7b0b017c6bb9d3205e5bdb7d808fb3c8678c))

    <details><summary>why</summary>

    Every script tool sees its own .iterion-script-*.py in the workspace for the lifetime of the node (executor_tool materialises it there), so the supervisor's clean-tree refusal fired on every run, and the steward would have counted it in every observed diff. Found by the bot's first real run: preflight refused a freshly created worktree. Excluded from all three status reads — it is execution machinery, never part of a verdict.

    </details>

## [3.38.1](https://github.com/SocialGouv/iterion/compare/v3.38.0...v3.38.1) (2026-08-11)

### Bug Fixes

* **git:** the pkg/git suite must not report on its own environment ([#405](https://github.com/SocialGouv/iterion/issues/405)) ([40231ab](https://github.com/SocialGouv/iterion/commit/40231abad7a35c8c66ec389cd6350db1718b1833)), references [#392](https://github.com/SocialGouv/iterion/issues/392) [#395](https://github.com/SocialGouv/iterion/issues/395) [#397](https://github.com/SocialGouv/iterion/issues/397) [#399](https://github.com/SocialGouv/iterion/issues/399) [#394](https://github.com/SocialGouv/iterion/issues/394)

    <details><summary>why</summary>

    Four of the eleven Dependabot PRs opened on 2026-08-10 were held on a red build none of them caused: #392, #395, #397 and #399 all failed `TestLogAllowsTabsInUserControlledFields` with `author: got "iterion-forge-61934180[bot]"`.

    </details>

## [3.38.0](https://github.com/SocialGouv/iterion/compare/v3.37.0...v3.38.0) (2026-08-11)

### Features

* **cloud:** carry loop_budget_guard onto the queue and into detached runs ([#406](https://github.com/SocialGouv/iterion/issues/406)) ([a6b7494](https://github.com/SocialGouv/iterion/commit/a6b7494c00ad033c69aedc8b6e6a77afc9af897b))

    <details><summary>why</summary>

    The run-level override stopped at the launch boundary: a cloud pod re-resolved the guard from the workflow and its own empty environment, so `--loop-budget-guard off` on a bot that declares nothing ran guarded anyway, and `on` against a bot declaring `off` could still strand its work at the cap. An operator's explicit choice, quietly re-made elsewhere — the failure `auto_memory` closed at schema v6.

    </details>

## [3.37.0](https://github.com/SocialGouv/iterion/compare/v3.36.4...v3.37.0) (2026-08-11)

### Features

* **dsl:** loop_budget_guard through the full precedence chain ([#404](https://github.com/SocialGouv/iterion/issues/404)) ([c9014fd](https://github.com/SocialGouv/iterion/commit/c9014fdec268a309a9f85da8c82d04047a0f8aa4))

    <details><summary>why</summary>

    The affordability guard shipped with a process-env escape hatch only, so turning it off was a deployment-wide decision with no per-run or per-bot say — the one engine dial that did not follow the chain every other one uses.

    </details>

## [3.36.4](https://github.com/SocialGouv/iterion/compare/v3.36.3...v3.36.4) (2026-08-11)

## [3.36.3](https://github.com/SocialGouv/iterion/compare/v3.36.2...v3.36.3) (2026-08-11)

### Bug Fixes

* a lost alignment must not merge as a clean bump ([#400](https://github.com/SocialGouv/iterion/issues/400) post-mortem) ([#401](https://github.com/SocialGouv/iterion/issues/401)) ([0e33942](https://github.com/SocialGouv/iterion/commit/0e33942d467e673696bcd6bce8315944095fe09e))

    <details><summary>why</summary>

    `commit_check` decided between the `committed` and `clean` verdicts from the shas alone. An unmoved head has two causes, though, and they are opposite verdicts: the bump genuinely needed no alignment, or `align` produced one that never reached the branch. Both are green under the old wiring, and `clean` is what the required check reports.

    </details>

## [3.36.2](https://github.com/SocialGouv/iterion/compare/v3.36.1...v3.36.2) (2026-08-11)

### Bug Fixes

* **runtime:** decline a loop back-edge the budget cannot fund ([#402](https://github.com/SocialGouv/iterion/issues/402)) ([a62939a](https://github.com/SocialGouv/iterion/commit/a62939a9c54ac8457eef881e3436be77ff27a233))

    <details><summary>why</summary>

    A loop that banks work as it goes — commits in stride, a published report, a PR opened by a tail node — used to start an iteration it could not pay for, die mid-iteration on BUDGET_EXCEEDED, and leave the tail that would have delivered the work unreached. iterion's own docs-refresh weeklies lost 31 and 29 alignment commits that way, on a clone that died with the pod.

    </details>

## [3.36.1](https://github.com/SocialGouv/iterion/compare/v3.36.0...v3.36.1) (2026-08-10)

## [3.36.0](https://github.com/SocialGouv/iterion/compare/v3.35.3...v3.36.0) (2026-08-10)

### Features

* **bots:** non-blocking teach-back on ambiguous missions ([6418393](https://github.com/SocialGouv/iterion/commit/641839340f4cd101df14b08e6c8395ebd79cd8c6))

    <details><summary>why</summary>

    Switch the feature-dev and whole-improve-loop campaign nodes from interaction: human to interaction: async (ADR-081) and rewrite mission item 5: on an ambiguous mission (or a self-picked axis), the campaign posts a teach-back via ask_user_async — the goal restated in its own words plus the load-bearing assumptions — and KEEPS WORKING under those stated assumptions; answers fold in mid-run via the message queue. The blocking ask_user stays reserved for genuine hard stops, and unanswered…

    </details>

## [3.35.3](https://github.com/SocialGouv/iterion/compare/v3.35.2...v3.35.3) (2026-08-10)

### Bug Fixes

* **runner:** a weekly cap parked four reviews with nothing coming back ([#389](https://github.com/SocialGouv/iterion/issues/389)) ([c710817](https://github.com/SocialGouv/iterion/commit/c710817436f5af594e3302539c0f2ae2679c7c50))

    <details><summary>why</summary>

    usageWindowRetryAt documents three evidence sources for "the provider's window is shut": the typed error, a classified runtime code, "and the flattened message is a last resort for a host that has neither — which is not hypothetical, since a runner with no dispatcher wired classifies nothing at all." The third one was never implemented. usageWindowEvidence returned false unless a type or a code survived, so on any host where neither does, the provider's own words — sitting right there in…

    </details>

## [3.35.2](https://github.com/SocialGouv/iterion/compare/v3.35.1...v3.35.2) (2026-08-10)

## [3.35.1](https://github.com/SocialGouv/iterion/compare/v3.35.0...v3.35.1) (2026-08-10)

### Bug Fixes

* **gate:** a review in flight is indistinguishable from one that never ran ([#387](https://github.com/SocialGouv/iterion/issues/387)) ([da6afa1](https://github.com/SocialGouv/iterion/commit/da6afa1e73bb1db59ff38ce48591909eb5801eca)), references [buildkit-operator#19](https://github.com/buildkit-operator/issues/19)

    <details><summary>why</summary>

    The merge gate only ever posted a verdict, at the END of a run that takes minutes. For that whole window the required context carries NO status, which a forge renders as "Expected — waiting for status to be reported" — byte-identical to a review that was never launched. Read next to the reviewer's comment on the previous commit, it looks exactly like "the bot commented but the gate never went green". Reported from production on buildkit-operator#19 today.

    </details>
* **modernize:** a blocked lot was re-attempted on every run, forever ([98324c9](https://github.com/SocialGouv/iterion/commit/98324c9d010739764653d95c46fc8905ff2e842a))

    <details><summary>why</summary>

    Measured on a replay campaign: a lot declared itself blocked after forty-two minutes — the toolchain raise it carried changes observable behaviour, and it said so in a committed report rather than force its gate green. The NEXT run picked the same lot again, redid the same work, and hit the same wall. In a single-run workflow that is harmless, since a human reads the report and decides. In a loop it wedges, and it burns the budget doing it.

    </details>

## [3.35.0](https://github.com/SocialGouv/iterion/compare/v3.34.2...v3.35.0) (2026-08-10)

### Features

* **golden-master:** a write surface, and two archetypes that were required nowhere ([3cd5a6e](https://github.com/SocialGouv/iterion/commit/3cd5a6ed293767ba850f17f35e255058e6d26f71))

    <details><summary>why</summary>

    The net only ever read. A corruption applied when content is STORED — a tag lost, an attribute normalised, an identifier drawn afresh on every save — moved no reference and passed the gate green, so proving it took a script outside the net. That is where such proofs end up when the net cannot write.

    </details>
* **golden-master:** the report names which mutants could no longer be applied ([a994d8c](https://github.com/SocialGouv/iterion/commit/a994d8c1f7752b266c34fcd8ff02a4b10d8f80ec))

    <details><summary>why</summary>

    They were already said, in free text, in the middle of the log. A mutant goes invalid for two very different reasons — it mutates nothing, so it never proved anything; or its anchor vanished under a legitimate change, so it DID prove something and has stopped. Only the second is mechanically repairable, and telling them apart means reading a field rather than matching a phrase.

    </details>
* **modernize:** repair a mutant the lot invalidated, by delegating to the net's own bot ([2e682d2](https://github.com/SocialGouv/iterion/commit/2e682d22b1ab0addb8753111eb3b0fb6896ca14a))

    <details><summary>why</summary>

    A modernisation lot is entitled to rename a method or restructure a template. When it does, a mutant that patched the old form stops patching anything. The harness calls it INVALID, correctly, and the surface it probed stops being covered — while NOTHING goes red, because an invalid mutant is excluded from the score and the figure looks no worse. A lot can go green while the net gets narrower, which is the one failure a green cannot report.

    </details>
* **modernize:** surface which mutants the oracle could no longer apply ([383bf9a](https://github.com/SocialGouv/iterion/commit/383bf9a3a0c707adceaa5f9962d1799a4082902f))

    <details><summary>why</summary>

    A lot may legitimately remove the thing a mutant hooks into — a security major withdraws the matcher idiom one named, a front-end major replaces the configuration block another edited. The patch stops applying, the harness marks the mutant invalid, and an invalid mutant neither scores nor dilutes: it simply stops proving anything, quietly, on whichever lane it covered. The gate can stay green while the counter-test that made that lane worth trusting has gone dark.

    </details>

### Bug Fixes

* **golden-master:** collateral blamed the mutant without ever testing a third cause ([9ee97e8](https://github.com/SocialGouv/iterion/commit/9ee97e88d530b176afb3b97ca885016e2b0e9255))

    <details><summary>why</summary>

    A control entry that differs from its reference was attributed to the mutant, and the message offered exactly two explanations: an under-declared blast radius, or a capture that is not isolated. There is a third, and it is the only one in which the mutant plays no part — THE CONTROL ENTRY DOES NOT REPRODUCE ITSELF. Whichever mutant happened to sample it wears the blame.

    </details>
* **golden-master:** the two copies of the harness had drifted, and the test said otherwise ([c19926e](https://github.com/SocialGouv/iterion/commit/c19926e17e39162b28690189a4fe90ca56c6556a))

    <details><summary>why</summary>

    The harness exists twice: inlined in main.bot's oracle_run node, which is the copy that runs, and as oracle-harness.py, which is the copy a human reads. A test claimed to keep them in sync. It pinned the set of top-level function names and the report fields, on the stated grounds that verbatim comparison was impossible.

    </details>
* **runview:** reset dropped nodes' execution state on run_rewound ([#382](https://github.com/SocialGouv/iterion/issues/382)) ([353c0c2](https://github.com/SocialGouv/iterion/commit/353c0c28567b4d34a66b312ee230d2932e936ec5))

    <details><summary>why</summary>

    A rewind invalidates the dropped nodes' checkpoint outputs, but the snapshot the studio renders node colours and infos from is folded from the append-only event log — and none of the three reducers handled the run_rewound event Rewind appends. The pre-rewind node_started / node_finished records kept folding in, so rewound nodes stayed painted with their pre-rewind status, duration and error instead of resetting to never-run.

    </details>

## [3.34.2](https://github.com/SocialGouv/iterion/compare/v3.34.1...v3.34.2) (2026-08-09)

### Bug Fixes

* **golden-master:** a reference carrying a carriage return never reproduced itself ([ab9c83c](https://github.com/SocialGouv/iterion/commit/ab9c83c720a98916c00bb56d3b4c71cb6816c3a0))

    <details><summary>why</summary>

    The harness wrote references without a newline setting and read them back without one either. On Linux the write default translates nothing and lets carriage returns reach the disk; the read default enables universal newlines and turns them into line feeds. From the moment such a reference is recorded it differs from what the capture produces — permanently, with no code having moved.

    </details>

## [3.34.1](https://github.com/SocialGouv/iterion/compare/v3.34.0...v3.34.1) (2026-08-08)

## [3.34.0](https://github.com/SocialGouv/iterion/compare/v3.33.1...v3.34.0) (2026-08-08)

### Features

* **improve-loops:** the ratchet — name it, and ask for it before the report ([37644da](https://github.com/SocialGouv/iterion/commit/37644dac977e77c986e4cde5039b610b57f12847))

    <details><summary>why</summary>

    The asymptote says why a run stops. Nothing said why the next run does not re-earn what this one banked, though the machinery for it ships everywhere: a gate reading a real exit code, a commit landed per verified unit, a diagnostic that makes a defect class impossible to repeat, a bilan that outlives the run. docs/improvement-ratchet.md gives those parts one name and one image each, and states the divergence from the family of ideas everyone will recognise: continuous improvement is unending,…

    </details>

### Bug Fixes

* **rewind:** scope the workspace restore to what the run recorded changing ([#381](https://github.com/SocialGouv/iterion/issues/381)) ([4e29c0a](https://github.com/SocialGouv/iterion/commit/4e29c0adb0c9205929aad6552bfefb287119f7e6)), closes [#380](https://github.com/SocialGouv/iterion/issues/380), references [#380](https://github.com/SocialGouv/iterion/issues/380) [#380](https://github.com/SocialGouv/iterion/issues/380) [#380](https://github.com/SocialGouv/iterion/issues/380)

    <details><summary>why</summary>

    `iterion rewind` forced the ENTIRE workspace back to the pivot's snapshot. On the default run shape — no `worktree: auto` — that workspace is the operator's live checkout, so one rewind reverted 38 tracked files and deleted 2 that no node of the run had ever written (#380).

    </details>

## [3.33.1](https://github.com/SocialGouv/iterion/compare/v3.33.0...v3.33.1) (2026-08-07)

## [3.33.0](https://github.com/SocialGouv/iterion/compare/v3.32.0...v3.33.0) (2026-08-07)

### Features

* **golden-master:** a canvas lane, and the browser plumbing it shares ([276ee97](https://github.com/SocialGouv/iterion/commit/276ee97b3c64996ed0319464ed4713a3b59094e8))

    <details><summary>why</summary>

    A canvas is the one surface nothing else can observe: the served document carries an empty tag, and the DOM stops changing once the image is painted. An accessibility audit says so itself — a datum rendered only as colour or as canvas is not restituted. A chart that stopped drawing entirely would leave every reference in a repository identical to the byte.

    </details>

### Bug Fixes

* **runtime:** keep DSL-fail runs rewindable by preserving the checkpoint ([#376](https://github.com/SocialGouv/iterion/issues/376)) ([5f56229](https://github.com/SocialGouv/iterion/commit/5f56229ad79e19b8a4bb478b478752cadf820d56)), closes [#373](https://github.com/SocialGouv/iterion/issues/373)

    <details><summary>why</summary>

    A run that reaches the DSL fail node was definitively unrecoverable: the fail path wrote status failed without a checkpoint, and the status transition purged the one that existed, so neither resume, rewind, nor cancel could bring the run back to a rewindable state — even though the on-disk state was coherent and the workspace snapshots survived.

    </details>
* **runview:** let a fork replace its dead parent on the pipeline board ([#377](https://github.com/SocialGouv/iterion/issues/377)) ([e4c17c7](https://github.com/SocialGouv/iterion/commit/e4c17c748e7c42a2a88dc56e16c37adf5db635e3)), closes [#374](https://github.com/SocialGouv/iterion/issues/374)

    <details><summary>why</summary>

    A run launched from a board card and recovered via fork disappeared from its card for good: the card kept showing the dead parent with no way to detach it, while the fork — the operator's actual recovery — ran invisibly. Since fork is the only way to recover a terminal run, recovery came at the price of board tracking.

    </details>
* **runview:** stop the skip-run and finalize log loops ([#378](https://github.com/SocialGouv/iterion/issues/378)) ([a8e3c11](https://github.com/SocialGouv/iterion/commit/a8e3c110d14fba756b88e5dbc681637e0dd5b39e)), closes [#375](https://github.com/SocialGouv/iterion/issues/375)

    <details><summary>why</summary>

    Run ids whose run.json was gone stayed listed by the store and were reloaded on every UI poll, each producing a WARN line — several lines per second, indefinitely, drowning the instance log (50 MB of noise on an active instance). Same family: the finalize recovery re-warned 'cannot read worktree HEAD' every minute on deleted worktrees.

    </details>

## [3.32.0](https://github.com/SocialGouv/iterion/compare/v3.31.1...v3.32.0) (2026-08-07)

### Features

* cross-backend model fallback chain (`fallbacks:`) ([#365](https://github.com/SocialGouv/iterion/issues/365)) ([38d4f07](https://github.com/SocialGouv/iterion/commit/38d4f0761016b8087c2420be71d83c7fcc48f057)), references [#1](https://github.com/SocialGouv/iterion/issues/1)

    <details><summary>why</summary>

    Discharges the cross-API deferral ADR-004 recorded in its Decision (5) and Alternative #1, and records why the obvious generalisation (flip providerFallbackEligible) is unsafe: a backend swap re-shapes seven delegate.Task fields, three pre-run analyses read a single static backend name, and both the run-level usage-window retry and the credential-pool donor cooldown key on the terminal error's type.

    </details>
* **e2e-coverage:** Endy — matrix-anchored e2e coverage completion bot ([f3de156](https://github.com/SocialGouv/iterion/commit/f3de1569feea7f8a9a49ad04eeda53f5ad5881f9))

    <details><summary>why</summary>

    ADR-058 v2 shape (one campaign + deterministic gate + bounded continuation), specialized for FEATURE-level e2e completeness: a committed feature×coverage matrix is the inventory, the living todo, the done-oracle and the audit trail. The verify_run gate enforces the matrix contract deterministically — parse, allowed statuses, justified exceptions, and a claims check where every covered-* row must cite a test that resolves in the tree (an orphan claim is a red gate). new_test_code is…

    </details>

### Bug Fixes

* **cli+e2e:** two round-2 test findings — a base64 blind spot and a 210s detection ([c0d3080](https://github.com/SocialGouv/iterion/commit/c0d3080421d5f1e24863dec8381e16d16f1aee04))

    <details><summary>why</summary>

    The secret round-trip claimed 'no plaintext on disk' but grepped only the raw bytes; the sealed field is a []byte, which json writes as base64, so a Seal/Open pass-through mutation (no encryption at all) survived the assertion. It now greps the base64 forms too — measured: the mutation that survived is killed, naming the store file.

    </details>
* **e2e-coverage:** close eight false-green bypasses in the matrix gate ([7dcf452](https://github.com/SocialGouv/iterion/commit/7dcf452695b72860f8be92a56a879ce93080ed3c))

    <details><summary>why</summary>

    An adversarial review executed seven distinct ways to make the gate say matrix_ok=true on a matrix that proves nothing, counts less than it claims, or is not even the table the operator reads:

    </details>
* **e2e-coverage:** inventory the nine surfaces the audit found missing ([c5521d5](https://github.com/SocialGouv/iterion/commit/c5521d527bfc5bee03e29a91f0a212edbc365bc9))

    <details><summary>why</summary>

    The matrix claimed completeness while nine operator-observable surfaces had no row at all — an omission is exactly what the inventory promise forbids. Six were already covered and only needed citing (bots install core, `bots templates`, plugin lifecycle run, /api/v1/pipeline-board, /api/v1/limits/cost, /api/backends/detect); three are real gaps now visible: `iterion server` and `iterion runner` CLI boot, and /api/effort-capabilities (reached today only as a readiness probe that asserts nothing…

    </details>
* **e2e-coverage:** repair eleven more mis-citations found by the second audit ([9ecb92b](https://github.com/SocialGouv/iterion/commit/9ecb92be2fd1d11e417551b30988aa1be223d260))

    <details><summary>why</summary>

    A second adversarial pass sampled 40 fresh rows (70 across both audits) and found the same failure mode as the first: a row cites the mechanics of a helper while the WIRING that invokes it goes untested.

    </details>
* **e2e-coverage:** repair four façade citations found by the matrix audit ([dc7d422](https://github.com/SocialGouv/iterion/commit/dc7d42263fc69e89b4c7f32e344fc860cb559f5f))

    <details><summary>why</summary>

    An adversarial audit sampled 30 covered rows and found four whose cited test would pass while the promised feature is broken:

    </details>
* **e2e-coverage:** round-2 gate hardening — a false POSITIVE and six more bypasses ([83e588d](https://github.com/SocialGouv/iterion/commit/83e588d5e6fe9d10aa9d14d9a731cda4a2a75a69))

    <details><summary>why</summary>

    The round-1 hardening was itself reviewed adversarially. The worst finding is a false positive, which in a blocking gate costs as much as a hole: the test-file regex required a slash on BOTH sides of tests/ and spec/, so a ROOT-level tests/ (Rust, pytest), spec/ (RSpec) or __tests__/ (Jest) was rejected — this gate would have refused the legitimate matrix of most non-Go repos and could never converge there.

    </details>
* **e2e-coverage:** round-3 — the round-2 hardening had narrowed the gate to Go ([2e8d7a7](https://github.com/SocialGouv/iterion/commit/2e8d7a784afc5d6b0dc8849b5ac45346b0146a9c))

    <details><summary>why</summary>

    Round 2 fixed a false positive and introduced two more, in the same place: the gate had quietly narrowed to matrices whose citations look like Go test function names — which is this repo's shape and almost nobody else's.

    </details>
* **e2e-coverage:** verify.sh must be overwritten and workspace-relative ([0af0da7](https://github.com/SocialGouv/iterion/commit/0af0da7b847e570cfcc917d0af2d694da7938c61))

    <details><summary>why</summary>

    The scratch dir is per-project, shared across runs: V3's verify_build found V1's script pinned to a dead worktree path. The prompt now mandates overwrite + $PWD-relative commands (the gate already runs the script with the repo root as cwd).

    </details>
* **e2e:** two live fixtures died on a cd into an unexpanded variable ([3c822d1](https://github.com/SocialGouv/iterion/commit/3c822d15eccc8d26b3118c84fea951b5a8eaaa42))

    <details><summary>why</summary>

    feat_worktree.bot and feat_compress.bot both opened their tool command with cd "${PROJECT_DIR}". That placeholder is only expanded in a bot var DEFAULT, never inside a command, so the shell got cd "" and the node failed before doing anything — TestLive_Feat_Worktree and TestLive_Feat_Compress could never pass, whatever the feature did.

    </details>
* **queue:** a message from a newer server is transient, not malformed ([ca2ef58](https://github.com/SocialGouv/iterion/commit/ca2ef588e1ffcf730951584aab5a3a96f048f3cf))

    <details><summary>why</summary>

    A runner that could not decode a delivery Termed it, whatever the reason. For a malformed payload that is right — no consumer will ever decode it. For a payload from a NEWER server it destroys a run: the queue entry is gone while the run document stays `queued` forever, and the only trace is one line in one pod's log. Nothing surfaces to the operator, who sees a run stuck in "queued" with no explanation.

    </details>
* **runtime:** a structured LLM call now anchors a turn, like a text one ([5d162d8](https://github.com/SocialGouv/iterion/commit/5d162d871e27c6f389946cc19c08d18af123c358))

    <details><summary>why</summary>

    Turn capture lived only in GenerateTextDirect. Every node declaring an output: schema — which in iterion is most of them — runs through GenerateObjectDirect and so anchored NOTHING: no TurnCheckpoint, an empty per-node timeline, and `iterion fork` failing with "turn not found" on a run that had plainly executed. The Fork API and the timeline were effectively blind to the majority of claw nodes.

    </details>
* **studio:** a workflow with no LLM nodes no longer crashes the Launch view ([457374d](https://github.com/SocialGouv/iterion/commit/457374dddc6b849acf17a36f1c5213c6f8e0aedd))

    <details><summary>why</summary>

    POST /api/runs/preview-cost answers {"nodes": null} for a workflow with no agent/judge node, and CostPreviewChip dereferenced data.nodes.length — the whole Launch view fell into its error boundary, so a tool+compute-only bot could not be launched from the studio at all. Found by the new studio UI Playwright suite (V4 dogfood, run 019fd6e6); the KNOWN-BUG tripwire test now asserts the positive contract instead.

    </details>
* **test:** detect nested checkouts by their .git, not by directory name ([bbe2dd9](https://github.com/SocialGouv/iterion/commit/bbe2dd9de0fefb2af29f19cd7b1b94df30634731))

    <details><summary>why</summary>

    The previous fix hardcoded .claude and .works — one is this harness deal, the other is a purely local convention of one operator. Where someone parks their worktrees and sibling clones is not this repo business.

    </details>
* **test:** the bot-identity audit walked into the operator nested checkouts ([8c42f09](https://github.com/SocialGouv/iterion/commit/8c42f098cc62701bb403b716af9cf90b11d29c64))

    <details><summary>why</summary>

    TestEveryExecutorConstructionDecidesTheBotIdentity walks the tree to find executor constructions that skip the bot identity, but its skip list did not cover .claude/worktrees or .works — git worktrees and sibling repos an operator keeps on disk. None of their files are tracked here, and their older copies report as offenders of a rule they predate, so the test passed inside a worktree and failed in the main checkout.

    </details>

## [3.31.1](https://github.com/SocialGouv/iterion/compare/v3.31.0...v3.31.1) (2026-08-05)

### Bug Fixes

* **runtime:** iterion's own scaffolding kept converged runs from landing ([597b066](https://github.com/SocialGouv/iterion/commit/597b066eaeb30cdecb356c0d1dddcc6ad4f55617))

    <details><summary>why</summary>

    At run start iterion mirrors the bundle's skills into the worktree under `.claude/skills/`. Finalize then read those untracked files as "the bot left work uncommitted", banked them as a wip commit — and a wip-banked HEAD is never merged, by design. The result: a run whose gate CONVERGED did not land, and the only thing standing in the way was iterion's own scaffolding.

    </details>

## [3.31.0](https://github.com/SocialGouv/iterion/compare/v3.30.13...v3.31.0) (2026-08-05)

### Features

* **dsl:** add auto_memory: — a per-node MEMORY.md switch, off by default ([0283edc](https://github.com/SocialGouv/iterion/commit/0283edc8a1cc9d8dd0af6533f13e2ec26aca5a15))

    <details><summary>why</summary>

    Auto-memory behaved differently on each backend, silently, and no .bot author could control it: claude_code's own default is ON, so every node of every run read and wrote the operator's personal ~/.claude/projects/<cwd>/memory/, while claw and pi had no MEMORY.md at all. On a cloud pod, anything written to a pod-local directory died with the run.

    </details>

### Bug Fixes

* **runtime:** a run launched from a linked worktree described another branch ([ca72827](https://github.com/SocialGouv/iterion/commit/ca728277cc4c2deb55007f1eb0e4262e89be2465))

    <details><summary>why</summary>

    `iterion run` resolves the repo root up to the MAIN repository — that is where .git lives and where worktrees are registered, and it is correct. It then read `HEAD` there too, which is not: a linked worktree has its own HEAD and its own branch, and the run silently anchored on whatever the main checkout happened to be on.

    </details>
* **sandbox:** host_state=none dropped the git identity along with the mount ([365c048](https://github.com/SocialGouv/iterion/commit/365c0482ea2fd98651cb05654178496c869d299c))

    <details><summary>why</summary>

    Under host_state: auto the operator's ~/.gitconfig is bind-mounted, and that mount is what gives an in-sandbox `git commit` an author. Turning host state off — the documented setting for multi-tenant and cloud runners — removed the mount and, with it, the identity: every commit-producing bot then dies on "Author identity unknown", for a reason unrelated to what it was asked to do.

    </details>
* **sandbox:** the shipped images declare no locale, so the JVM read ASCII ([0223a9e](https://github.com/SocialGouv/iterion/commit/0223a9e49caa959668bbfcce0d83bb71f75a56d1))

    <details><summary>why</summary>

    Measured on the slim image: `LANG` and `LC_ALL` are both empty. That is not a neutral state — it IS the C/POSIX locale, and a JVM derives `sun.jnu.encoding` from it and decodes filenames as ASCII. A build whose resources carry an accented name fails on "Problems opening file input stream", naming a file that is plainly on disk. An agent working in the sandbox has to discover this and prefix its own commands to get past it.

    </details>

## [3.30.13](https://github.com/SocialGouv/iterion/compare/v3.30.12...v3.30.13) (2026-08-05)

### Bug Fixes

* **dep-update-guard:** the drift-gate precheck loops back instead of discarding the run ([#370](https://github.com/SocialGouv/iterion/issues/370)) ([722bacf](https://github.com/SocialGouv/iterion/commit/722bacfee3cf53193154a9e7ba2a9b5c8308c56c))

    <details><summary>why</summary>

    Twice in one day the aligner's correct Vite 8 migration was thrown away because verify.sh omitted the repo's CI drift gate and the deterministic precheck fired at VERDICT time (rc=3 → hold_unstable, alignment discarded). The omission is an authorship defect, not a red build: the agent scopes 'bump-relevant' and rationalises away repo-wide gates (a studio bump 'cannot drift' the Go openapi — but §1b gates are never scoped out).

    </details>

## [3.30.12](https://github.com/SocialGouv/iterion/compare/v3.30.11...v3.30.12) (2026-08-05)

### Bug Fixes

* **dep-update-guard:** verify.sh mirrors CI's exact strictness ([#369](https://github.com/SocialGouv/iterion/issues/369)) ([d514f52](https://github.com/SocialGouv/iterion/commit/d514f52170242b34acd88196f48f9da32822457f)), references [#19](https://github.com/SocialGouv/iterion/issues/19)

    <details><summary>why</summary>

    EstimateUSD consults claw's LIVE pricing registry first, so the price tests' expectations depended on whatever the network returned that day — flaky on the host, and reliably wrong inside sandbox pods, where this failure has now twice painted a Vetty verify red on an otherwise CI-green PR (#19's Vite 8 alignment being the second). Pin the tests to the fallback path via CLAW_DISABLE_LIVE_REGISTRY=1 except where the live-cache behaviour is itself under test (seeded explicitly).

    </details>
* **golden-master:** the dirty-tree notice mangled the first path it named ([8314a52](https://github.com/SocialGouv/iterion/commit/8314a52586a61dd19bc74465eddf3eab2a5fc34f))

    <details><summary>why</summary>

    `git status --porcelain` writes `XY <path>`, and X is a space for an unstaged modification. Stripping the whole output before splitting ate that leading space on the first line only, so it shifted by one and lost a character: `build.gradle` came out as `uild.gradle`. Later lines were intact.

    </details>
* **modernize:** refuse when the contract cannot be READ, instead of finishing green ([7276c3c](https://github.com/SocialGouv/iterion/commit/7276c3cc9a6bc35d90a5590907c7c701df19b010))

    <details><summary>why</summary>

    A run completed with Status FINISHED having executed no lot at all. The plan reader could not find yq, emitted nothing_to_do, and the graph routed straight to done. Every failure path in that reader called the same emit(), which exits 0 — so three very different outcomes collapsed into one benign one:

    </details>
* **sandbox:** make devbox actually work for every bot and every repo ([ac609c9](https://github.com/SocialGouv/iterion/commit/ac609c91d5e1ef1da2b9c405d4ce1b6e06980441))

    <details><summary>why</summary>

    A bundle or a repo could declare devbox packages and get NONE of them, with the run continuing as if they were there. Three causes, each measured on a real run rather than reasoned about, and each fixed where it belongs.

    </details>

## [3.30.11](https://github.com/SocialGouv/iterion/compare/v3.30.10...v3.30.11) (2026-08-05)

## [3.30.10](https://github.com/SocialGouv/iterion/compare/v3.30.9...v3.30.10) (2026-08-04)

## [3.30.9](https://github.com/SocialGouv/iterion/compare/v3.30.8...v3.30.9) (2026-08-04)

## [3.30.8](https://github.com/SocialGouv/iterion/compare/v3.30.7...v3.30.8) (2026-08-04)

## [3.30.7](https://github.com/SocialGouv/iterion/compare/v3.30.6...v3.30.7) (2026-08-04)

### Bug Fixes

* **runtime:** a failing llm half of llm_or_human degrades to the human pause ([#367](https://github.com/SocialGouv/iterion/issues/367)) ([5819b04](https://github.com/SocialGouv/iterion/commit/5819b048f63af712ab383bd92b41008079d157d8))

    <details><summary>why</summary>

    A human node's llm_or_human half runs through GenerateObjectDirect, which takes provider/model-id and has no backend to infer the provider from; the escalate node carried a bare 'claude-opus-5' default from birth. Every prior run took the clean/committed routes around it, and the FIRST needs_decision bump in production (plugin-react 4→6, 2026-08-04, run 019fcd8e-fe0b) crashed with 'invalid spec' at the exact moment the workflow existed to hand over.

    </details>

## [3.30.6](https://github.com/SocialGouv/iterion/compare/v3.30.5...v3.30.6) (2026-08-04)

## [3.30.5](https://github.com/SocialGouv/iterion/compare/v3.30.4...v3.30.5) (2026-08-04)

### Bug Fixes

* **forge:** keep the issue-lane label allowlist across re-provisions ([#363](https://github.com/SocialGouv/iterion/issues/363)) ([2dec4f1](https://github.com/SocialGouv/iterion/commit/2dec4f131f4ae15a05aa026a6f9cff0c15f4cec5))

    <details><summary>why</summary>

    Narrowing which freshly-applied issue label dispatches the implementer (`label_allowlist`) was a webhook-config PATCH, and Provision rebuilds that config as a whole literal from the manifests. Any bot-set change — the studio Integrations tab PATCHes bot_ids and nothing else — therefore dropped the narrowing, silently and fail-OPEN: an empty allowlist matches every label, so the repo returned to starting a feature-dev campaign on any label added to any issue.

    </details>

## [3.30.4](https://github.com/SocialGouv/iterion/compare/v3.30.3...v3.30.4) (2026-08-04)

### Bug Fixes

* **dep-update-guard:** the escalate node could never fire — bare model spec crashed the direct generation path ([#366](https://github.com/SocialGouv/iterion/issues/366)) ([a78b6a7](https://github.com/SocialGouv/iterion/commit/a78b6a72941e1879bce429d76e5d193e53342c4e))

    <details><summary>why</summary>

    A human node's llm_or_human half runs through GenerateObjectDirect, which takes provider/model-id and has no backend to infer the provider from; the escalate node carried a bare 'claude-opus-5' default from birth. Every prior run took the clean/committed routes around it, and the FIRST needs_decision bump in production (plugin-react 4→6, 2026-08-04, run 019fcd8e-fe0b) crashed with 'invalid spec' at the exact moment the workflow existed to hand over.

    </details>

## [3.30.3](https://github.com/SocialGouv/iterion/compare/v3.30.2...v3.30.3) (2026-08-04)

## [3.30.2](https://github.com/SocialGouv/iterion/compare/v3.30.1...v3.30.2) (2026-08-04)

## [3.30.1](https://github.com/SocialGouv/iterion/compare/v3.30.0...v3.30.1) (2026-08-04)

## [3.30.0](https://github.com/SocialGouv/iterion/compare/v3.29.0...v3.30.0) (2026-08-04)

### Features

* show a node's file changes in the run console ([#352](https://github.com/SocialGouv/iterion/issues/352)) ([b6f1b11](https://github.com/SocialGouv/iterion/commit/b6f1b11916cb76747c3c9c59c12305ed22313c8c)), references [#351](https://github.com/SocialGouv/iterion/issues/351) [#349](https://github.com/SocialGouv/iterion/issues/349) [#349](https://github.com/SocialGouv/iterion/issues/349) [#351](https://github.com/SocialGouv/iterion/issues/351)

    <details><summary>why</summary>

    Iterating on a bot's configuration meant relaunching from scratch: edit a prompt, and the only way to test it was to re-pay for every upstream node. `iterion rewind` re-anchors an existing run's checkpoint on a node it already executed and invalidates what the replay will regenerate, so `iterion resume` picks up from there. Same run id — distinct from `fork`, which mints a child for an alternative future and leaves the parent intact.

    </details>

## [3.29.0](https://github.com/SocialGouv/iterion/compare/v3.28.1...v3.29.0) (2026-08-04)

### Features

* a review gate shows everything changed since the previous gate ([#351](https://github.com/SocialGouv/iterion/issues/351)) ([d9bd4cc](https://github.com/SocialGouv/iterion/commit/d9bd4ccc227b43ec94bdf62f97f2d68bb3db0325)), closes [#349](https://github.com/SocialGouv/iterion/issues/349), references [#349](https://github.com/SocialGouv/iterion/issues/349) [#349](https://github.com/SocialGouv/iterion/issues/349) [post-#349-squash](https://github.com/post-/issues/349-squash) [#349](https://github.com/SocialGouv/iterion/issues/349)

    <details><summary>why</summary>

    Iterating on a bot's configuration meant relaunching from scratch: edit a prompt, and the only way to test it was to re-pay for every upstream node. `iterion rewind` re-anchors an existing run's checkpoint on a node it already executed and invalidates what the replay will regenerate, so `iterion resume` picks up from there. Same run id — distinct from `fork`, which mints a child for an alternative future and leaves the parent intact.

    </details>

## [3.28.1](https://github.com/SocialGouv/iterion/compare/v3.28.0...v3.28.1) (2026-08-04)

### Bug Fixes

* **credpool:** a donated credential must reach the agent, and be accounted for ([#360](https://github.com/SocialGouv/iterion/issues/360)) ([a4926ab](https://github.com/SocialGouv/iterion/commit/a4926ab04edd4b85cf4a64dc2c303980ce6388b3))

    <details><summary>why</summary>

    Three defects a live end-to-end run on production surfaced. None was reachable by reading: the chain works right up to the runner pod, and breaks in the layer below it.

    </details>
* **runtime,runner:** a budget death is terminal-acked, never redelivered ([#361](https://github.com/SocialGouv/iterion/issues/361)) ([5f64a87](https://github.com/SocialGouv/iterion/commit/5f64a87c032bf22f1ab36ff928309506a1688445))

    <details><summary>why</summary>

    The engine's per-node budget checks built a bare RuntimeError (code only, no sentinel Cause), so the runner's terminal-ack carve-out — which matches errors.Is(err, ErrBudgetExceeded) — missed it and naked the delivery back to JetStream. Observed live (run 019fcc30-b9be): a 96% duration hard limit at the last node turned into six ~40s resume/refail turns, each re-provisioning a sandbox to instantly re-hit the same spent budget. The branch scheduler's twin checks already wrapped the sentinel;…

    </details>

## [3.28.0](https://github.com/SocialGouv/iterion/compare/v3.27.2...v3.28.0) (2026-08-04)

### Features

* iterion-owned workspace versioning ([#349](https://github.com/SocialGouv/iterion/issues/349)) ([88fb897](https://github.com/SocialGouv/iterion/commit/88fb897e2e29ab5219a9daf1038f20684931c133))

    <details><summary>why</summary>

    Iterating on a bot's configuration meant relaunching from scratch: edit a prompt, and the only way to test it was to re-pay for every upstream node. `iterion rewind` re-anchors an existing run's checkpoint on a node it already executed and invalidates what the replay will regenerate, so `iterion resume` picks up from there. Same run id — distinct from `fork`, which mints a child for an alternative future and leaves the parent intact.

    </details>

## [3.27.2](https://github.com/SocialGouv/iterion/compare/v3.27.1...v3.27.2) (2026-08-04)

## [3.27.1](https://github.com/SocialGouv/iterion/compare/v3.27.0...v3.27.1) (2026-08-04)

### Bug Fixes

* **studio:** file in-progress pipeline tickets as done when their run finishes ([#359](https://github.com/SocialGouv/iterion/issues/359)) ([3f73783](https://github.com/SocialGouv/iterion/commit/3f737834434829476c6b601cae1b3b27e5e3e91f))

    <details><summary>why</summary>

    The studio admission loop moved a launched ticket to in_progress and stamped last_run_id, but nothing moved the ticket back out once the run reached a terminal status. The run's status drives the /pipelines column, yet hard blockers only count ticket state == done (native.BlockerSatisfied), so a cleanly finished ticket stranded in in_progress forever and every dependent parked in waiting_deps.

    </details>

## [3.27.0](https://github.com/SocialGouv/iterion/compare/v3.26.1...v3.27.0) (2026-08-04)

### Features

* **mcp:** operator MCP server exposing local + remote iterion (iterion mcp) ([0727783](https://github.com/SocialGouv/iterion/commit/0727783473cbc92d68f3d04314344f84936e19ef))

    <details><summary>why</summary>

    Add the public `iterion mcp` command: a stdio MCP server any client (Claude Code, desktop, Cursor) registers to drive iterion end to end, with two tool families (41 tools, readOnlyHint annotations):

    </details>

### Bug Fixes

* **mcp:** harden the operator MCP server per adversarial review ([7a37113](https://github.com/SocialGouv/iterion/commit/7a37113dcfc5f662a01367e28aeb84d09d38c0a3))

    <details><summary>why</summary>

    Address every finding of the opus adversarial pass on 603161446:

    </details>
* **server:** relaunch claim is per-bot; rune-safe reason truncation ([#358](https://github.com/SocialGouv/iterion/issues/358)) ([3080fb2](https://github.com/SocialGouv/iterion/commit/3080fb292cca223daf3aeeb7427da956c6641069)), references [#357](https://github.com/SocialGouv/iterion/issues/357)

    <details><summary>why</summary>

    Two follow-ups from Revi's review of #357 (R41df5e, R60c7c8):

    </details>

## [3.26.1](https://github.com/SocialGouv/iterion/compare/v3.26.0...v3.26.1) (2026-08-04)

### Bug Fixes

* **server,dep-update-guard:** a dead merge-gate run recovers instead of silently blocking its PR ([#357](https://github.com/SocialGouv/iterion/issues/357)) ([bc9a99c](https://github.com/SocialGouv/iterion/commit/bc9a99c2e80c9066f4c696619dfdcd507ec19bf0)), references [SocialGouv/iterion#354](https://github.com/SocialGouv/iterion/issues/354) [353/#355](https://github.com/SocialGouv/iterion/issues/355)

    <details><summary>why</summary>

    A run that owed a merge-gate status and died as failed_resumable was never reconciled — on the theory that it would resume. Only usage-window failures arm a retry; a budget-exceeded or exhausted run sat forever and its PR stayed silently unmergeable behind an absent required check (observed in production 2026-08-03: Vetty run 019fc8e5 on SocialGouv/iterion#354).

    </details>

## [3.26.0](https://github.com/SocialGouv/iterion/compare/v3.25.0...v3.26.0) (2026-08-04)

### Features

* **credpool:** lend keys of any provider + fixes from Revi and a live prod run ([#356](https://github.com/SocialGouv/iterion/issues/356)) ([d417368](https://github.com/SocialGouv/iterion/commit/d417368bed965b6ee85606b51b01c821d79ef98a))

    <details><summary>why</summary>

    A pledge now offers a Credential — a (source, ref) pair — instead of an OAuth kind alone: `oauth/claude_code` as before, or `api_key/anthropic`, `api_key/openai`, … for a personal BYOK key of any provider iterion knows.

    </details>

## [3.25.0](https://github.com/SocialGouv/iterion/compare/v3.24.1...v3.25.0) (2026-08-04)

### Features

* rewind a run in place to an earlier node ([#348](https://github.com/SocialGouv/iterion/issues/348)) ([582e891](https://github.com/SocialGouv/iterion/commit/582e891862ac2da10021f4dfc6446e880a536e95))

    <details><summary>why</summary>

    Iterating on a bot's configuration meant relaunching from scratch: edit a prompt, and the only way to test it was to re-pay for every upstream node. `iterion rewind` re-anchors an existing run's checkpoint on a node it already executed and invalidates what the replay will regenerate, so `iterion resume` picks up from there. Same run id — distinct from `fork`, which mints a child for an alternative future and leaves the parent intact.

    </details>

## [3.24.1](https://github.com/SocialGouv/iterion/compare/v3.24.0...v3.24.1) (2026-08-03)

## [3.24.0](https://github.com/SocialGouv/iterion/compare/v3.23.2...v3.24.0) (2026-08-03)

### Features

* **credpool:** mutualise contributors' unused LLM subscription quota ([#350](https://github.com/SocialGouv/iterion/issues/350)) ([8ad6af0](https://github.com/SocialGouv/iterion/commit/8ad6af0960d8a95f9700a15e52ed8902db71ac50))

    <details><summary>why</summary>

    Developers lend the unused part of their Claude Pro/Max or ChatGPT subscription; a run with no credential of its own draws on it, bounded by ceilings the lender sets and revocable at any moment.

    </details>

### Bug Fixes

* **delegate:** include the recovery formatting pass's CLI cost in annotation ([6c6383e](https://github.com/SocialGouv/iterion/commit/6c6383e8d79680d292562f4d99dc2240c1096aed))

    <details><summary>why</summary>

    runRecoveryFormatterPass folded its tokens into the running totals but its ResultMessage never reached annotateCost, so the cost annotated after a recovery pass was Pass 1's stale total_cost_usd (or missed the pass entirely under per-invocation accounting). Return the pass's ResultMessage and feed it to the same max-across-messages selection the two-pass path already uses. Found by adversarial review.

    </details>
* **delegate:** price claude_code cost with the effective model and CLI-reported cost ([d304cf3](https://github.com/SocialGouv/iterion/commit/d304cf355e3eee3d04dc1b9f3a676772bde5e1d3))

    <details><summary>why</summary>

    cost.Annotate received task.Model — the node-declared model, empty on every node that relies on backend auto-detection — so EstimateUSD priced against an unknown model, emitted no _cost_usd, and the whole run reported tokens but no cost (the studio report then shows its 'no cost recorded' placeholder forever; observed on the feed-watch cloud runs, where system/init resolved claude-opus-5 but the node declares no model).

    </details>
* **feed-watch:** canonicalize item URLs through redirects before synthesis ([03467f3](https://github.com/SocialGouv/iterion/commit/03467f3c8d1dcd65a175d3a813ae2acf920338e6))

    <details><summary>why</summary>

    Aggregator feeds (FeedBlitz, FeedBurner) serve tracking URLs whose host is the aggregator's, not the article's. The synthesize agent web_fetches those items, lands on the canonical article and naturally cites its final URL — which the deterministic verify_message gate then rejects as off-item, failing the whole digest (observed live: the java digest run 019fc65e died on two baeldung.com links served as feeds.feedblitz.com items).

    </details>
* **forge:** re-mint the github_app managed token at every repo-targeted launch ([0c14674](https://github.com/SocialGouv/iterion/commit/0c146741e1489627253501bc3fb694ef7a1ef08f))

    <details><summary>why</summary>

    EnsureManagedSecret returned the stored managed-secret id without touching its plaintext — a ONE-HOUR GitHub App installation token minted at provision time. On a quiet connection (no provision, no worker rotation) every repo-targeted launch pinned that dead token and failed its clone with 'Invalid username or token' (observed live on prod relaunching the feed-watch java digest; the daily schedules never noticed because they resolve the team's forge_token binding instead). Re-mint at the point…

    </details>
* **runner:** fire run-outcome side effects only on final delivery dispositions ([41c7e6f](https://github.com/SocialGouv/iterion/commit/41c7e6f28cf21e32491056e663759efdfec85ab4))

    <details><summary>why</summary>

    A generic engine failure Naks for redelivery, and every failed delivery fired the completion webhook + run.<outcome> event anyway — one 'run failed' notification episode per redelivery (the episode key folds updated_at precisely so a later REAL re-failure notifies again), i.e. up to MaxDeliver pushes for a single deterministic failure within a minute (observed live: feed-watch run 019fc65e re-failed 8 times in 70s on its checkpointed verify input).

    </details>
* **studio:** render token-only run reports instead of the no-cost placeholder ([86f2bfb](https://github.com/SocialGouv/iterion/commit/86f2bfb48951b2c7b9d2d043dae219d25c061864))

    <details><summary>why</summary>

    A run whose model could not be priced records _tokens without _cost_usd; the Report tab treated that as 'no cost yet' and showed the fills-in-as- nodes-finish placeholder forever, even on finished runs. The report now renders whenever any usage (cost OR tokens) exists: bars and rankings scale by tokens when no cost was recorded, and cost cells show an explicit '—' rather than a fake $0. Buckets tie-break by tokens so a tokens-only report still ranks meaningfully.

    </details>
* **studio:** show per-bucket cost as unavailable instead of a fake $0.00 ([2143f6a](https://github.com/SocialGouv/iterion/commit/2143f6a516968fb2fb23282f3b74a9cc3aec90f4))

    <details><summary>why</summary>

    In a mixed run (some nodes priced, some not) the report-level hasCost flag rendered unpriced buckets as "$0.00 / 0%" next to their real token counts, reading as "this node was free". The cost and percent cells now decide per bucket: no recorded cost renders as an explicit unavailable marker. Found by adversarial review.

    </details>

## [3.23.2](https://github.com/SocialGouv/iterion/compare/v3.23.1...v3.23.2) (2026-08-02)

## [3.23.1](https://github.com/SocialGouv/iterion/compare/v3.23.0...v3.23.1) (2026-08-02)

### Bug Fixes

* **studio:** closed pipelines read newest-first ([#346](https://github.com/SocialGouv/iterion/issues/346)) ([cb62642](https://github.com/SocialGouv/iterion/commit/cb62642359d26d16636cbcc111490d32bf8212b4))

    <details><summary>why</summary>

    The inventory's sort was one value shared by both tabs, defaulting to priority. But priority is a launch-order key, and a pipeline that already ran will never be launched by it again: on Closed the ranking buried this morning's run under a months-old P9.

    </details>

## [3.23.0](https://github.com/SocialGouv/iterion/compare/v3.22.3...v3.23.0) (2026-08-02)

### Features

* **runtime:** a tool node can hand a file it produced to the run ([#347](https://github.com/SocialGouv/iterion/issues/347)) ([fd0d0b8](https://github.com/SocialGouv/iterion/commit/fd0d0b8a89e22585c7f1c7cb80ed6df9a38b89dd)), references [iterion#332](https://github.com/iterion/issues/332) [#336](https://github.com/SocialGouv/iterion/issues/336)

    <details><summary>why</summary>

    A gate previews a `file` value by fetching `GET /api/runs/{id}/attachments/{name}` — the descriptor's path is a host or sandbox bind-mount path and is not reachable from a browser (iterion#332, #336). So attachments only ever entered a run from a PERSON: the launch form, a `file`-typed gate field, the 📎 button.

    </details>

## [3.22.3](https://github.com/SocialGouv/iterion/compare/v3.22.2...v3.22.3) (2026-08-02)

### Bug Fixes

* **permission:** a grant lasted exactly one tool call ([#345](https://github.com/SocialGouv/iterion/issues/345)) ([e7a80bf](https://github.com/SocialGouv/iterion/commit/e7a80bf85eae5bd846dac0c5ba8f1c4998e415c3))

    <details><summary>why</summary>

    The pause tells the operator their answer is added "to the allowlist for the rest of this run". It was added to one policy, for one re-invocation, and then dropped.

    </details>

## [3.22.2](https://github.com/SocialGouv/iterion/compare/v3.22.1...v3.22.2) (2026-08-02)

### Bug Fixes

* **tool:** a `json` field holding a list broke out of its shell assignment ([#341](https://github.com/SocialGouv/iterion/issues/341)) ([4a517a2](https://github.com/SocialGouv/iterion/commit/4a517a288691f2c1c1fe42d44177075e871d9fa8))

    <details><summary>why</summary>

    A schema field declared `json` reaches shellEscapeValue as []any and falls into the scalar-slice arm, which space-joins individually-quoted elements. In an assignment position that is not one value, it is several words:

    </details>

## [3.22.1](https://github.com/SocialGouv/iterion/compare/v3.22.0...v3.22.1) (2026-08-01)

## [3.22.0](https://github.com/SocialGouv/iterion/compare/v3.21.0...v3.22.0) (2026-08-01)

### Features

* **studio:** a needs-attention lane that reserves its concurrency slot ([#344](https://github.com/SocialGouv/iterion/issues/344)) ([b49d74b](https://github.com/SocialGouv/iterion/commit/b49d74b542d3e1c1374bfa1e01540a6994446e8e))

    <details><summary>why</summary>

    A pipeline that died mid-flight used to file itself under Closed and hand its concurrency slot straight to the next queued card. Closed means "this reached its end", and a crash did not — so the failure read as done, and by the time the operator fixed it the slot was gone.

    </details>

## [3.21.0](https://github.com/SocialGouv/iterion/compare/v3.20.0...v3.21.0) (2026-08-01)

### Features

* **runner:** graceful lame-duck drain of in-flight runs on deploy ([3ec777f](https://github.com/SocialGouv/iterion/commit/3ec777fa708797edb599934084975bcdd89e139c))

    <details><summary>why</summary>

    In cloud mode a rolling deploy SIGTERMs the runner pods. Previously a gracefully-drained in-flight run was checkpointed as `cancelled`, which the redelivery reconciliation deliberately drops (anti-resurrection guard) — so the run needed a MANUAL resume and every deploy emitted a false "run cancelled" web-push. Only the crash/timeout paths auto-resumed.

    </details>

### Bug Fixes

* **drain:** close what auto-resume re-opened ([3e3c2f0](https://github.com/SocialGouv/iterion/commit/3e3c2f09df65e5e438aea4495886736c07b6b1ed))

    <details><summary>why</summary>

    Making an interruption auto-resume turns a rare, operator-driven recovery into a routine one, so paths that were safe because nobody took them are now taken on every scale-down. Found by an adversarial pass over the rebase.

    </details>
* **gate-autofix:** the unattended launch reached Mongo with no tenant ([b68f8a3](https://github.com/SocialGouv/iterion/commit/b68f8a39fd6240e5679db6eb08ac6d5b91236e20))

    <details><summary>why</summary>

    The auto-fix lane stamped the auth identity the admission gate reads and not the store identity every tenant-scoped query asserts on. A bus handler is not an HTTP request and carries neither; the inbound-webhook middleware stamps both and this lane copied half of it.

    </details>
* **runner:** don't resurrect an operator cancel during a lame-duck drain ([f700b5b](https://github.com/SocialGouv/iterion/commit/f700b5b7197b556008ac02752db043c3b36218dc))

    <details><summary>why</summary>

    Adversarial review found a HIGH regression: the shutdown-vs-operator distinction inferred "shutdown cancel" from parent.Err() (the loop ctx), which only means "a SIGTERM happened" — not "this run's cancel came from shutdown". In the default lame-duck mode the pod holds the run for up to DrainTimeout (8h), during which an operator cancel (via the iterion.cancel.<run_id> subject) would be misclassified as a shutdown interruption, promoted cancelled→failed_resumable, and auto-resumed —…

    </details>
* **runtime:** route mid-node interruptions through the cause-aware handler ([b14e11c](https://github.com/SocialGouv/iterion/commit/b14e11c39bce44dabfb64184307956d66b7fcc68))

    <details><summary>why</summary>

    Adversarial review of the cause-threading refactor found the clean handling only fired for the between-node/pre-run window (the top-of-loop select). The COMMON real case — a deploy drains a run WHILE an LLM node is executing — surfaced the cancellation as the node's execErr and routed it through failRunWithCheckpoint, which stringifies the error and loses the ErrRunInterrupted sentinel: the runner then fired a spurious "run failed" notification and stamped finalStatus=failed, even though the…

    </details>

## [3.20.0](https://github.com/SocialGouv/iterion/compare/v3.19.0...v3.20.0) (2026-08-01)

### Features

* **studio:** resizable pipeline card drawer + expandable input/output values ([#335](https://github.com/SocialGouv/iterion/issues/335)) ([c871a29](https://github.com/SocialGouv/iterion/commit/c871a29d56ae1f3ad8ec9f00ac955fc6fcd21358))

    <details><summary>why</summary>

    A value box that never puts its content permanently out of reach: short values render whole, long ones collapse to a preview with a "Show all N lines" toggle that expands IN PLACE. Every value carries a copy button, and a JSON value (structured, or a JSON string as bot_args carry them) gets a raw/pretty toggle.

    </details>

### Bug Fixes

* **runtime:** an edge mapping wrapped in literal text lost the literal ([#337](https://github.com/SocialGouv/iterion/issues/337)) ([262ff04](https://github.com/SocialGouv/iterion/commit/262ff041927bcf0125554fe4db011b33a55beff5))

    <details><summary>why</summary>

    `resolveMapping` never interpolated. With exactly one reference it returned that reference's value and dropped everything around it; with two or more it returned the raw template, unresolved. Only a mapping whose whole value was a single `{{…}}` came out right — which is why this survived: that is the shape almost every mapping has.

    </details>

## [3.19.0](https://github.com/SocialGouv/iterion/compare/v3.18.1...v3.19.0) (2026-08-01)

### Features

* **studio:** render a human gate's inbound payload so operators see what they are validating ([#336](https://github.com/SocialGouv/iterion/issues/336)) ([79d6110](https://github.com/SocialGouv/iterion/commit/79d611067d42d750983002a82267f0e94e2a4fae)), closes [#332](https://github.com/SocialGouv/iterion/issues/332), references [iterion#332](https://github.com/iterion/issues/332)

    <details><summary>why</summary>

    A paused gate could not show the operator WHAT they were validating. The data was already there end to end — `Engine.persistPause` resolves the node's incoming `with {}` mappings and stores them as `Interaction.Questions`, which the run console and the pipeline board both receive — but the form is driven by the node's OUTPUT schema, so the inbound half reached the browser and was dropped on the floor. Authors worked around it by stringifying the plan/diff/verdict into `instructions:`; anyone…

    </details>

### Bug Fixes

* **e2e:** pin the fixture's model so it compiles without a host credential ([82371ea](https://github.com/SocialGouv/iterion/commit/82371ea4a7726ba26f872c7c83773ffa4147c047))

    <details><summary>why</summary>

    The hand-off publish test passed locally and failed on main. C018 rejects an agent node that names neither `model:` nor `backend:` UNLESS the runtime can detect a credential — so a developer machine with Claude Code OAuth compiles the fixture and CI, which has none, does not.

    </details>
* **handoff:** publishing the fallback let a crashed review shadow a complete one ([4bd82c8](https://github.com/SocialGouv/iterion/commit/4bd82c830b7ddc6bc707b64709ed16638d14c54d))

    <details><summary>why</summary>

    Found by an adversarial pass over the publish fix, and caused by it.

    </details>
* **handoff:** the node a manifest names as a source has to publish something ([f066c0c](https://github.com/SocialGouv/iterion/commit/f066c0ce8457b832f502244397246fbc35fa475e))

    <details><summary>why</summary>

    The live dogfood found the hand-off resolving to nothing, and the first read of the evidence — no artifacts on any cloud run, zero `artifact_written` events — pointed at cloud storage. That was wrong, and the correction is the whole fix.

    </details>
* **launch:** a bot launched off the board could not publish anything ([5157144](https://github.com/SocialGouv/iterion/commit/51571448218e848abbdb6364853cb7292c71fd5d))

    <details><summary>why</summary>

    The cloud coordinator launches a card from its BotArgs alone, so two things the webhook tail composes inline never reached it: the forge-publish grant and the repo's launch policy. A board-mode fixer therefore pushed its commits and then had nowhere to post — no verdict, no ledger, no merge-gate status — leaving the repo's required check on the pre-push revision, which blocks the PR on a check that is absent rather than red. Measured side by side with the reviewer on the same PR.

    </details>
* **launch:** the stale provisioning was winning a re-provisioned repo ([cdfedc1](https://github.com/SocialGouv/iterion/commit/cdfedc12406e5a37d2c3702a67b787d5cabc2cc2))

    <details><summary>why</summary>

    Resolving one repo among several provisionings by lowest id is deterministic and arbitrary. On the e2e repo it picks the row from 2026-07-17, on a personal token, over the GitHub App it was deliberately re-provisioned onto — so the verdict would be posted under the operator's own account, which is the identity whose comments the loop guard refuses.

    </details>
* **webhooks:** the fixer's push was the one delivery the gate never saw ([db2676c](https://github.com/SocialGouv/iterion/commit/db2676c5b9605185c77988696f3afed9b3ed0579))

    <details><summary>why</summary>

    The iterion-bot guard skips a pull request our own loop produced, keyed on the sender. On a merge-gate resync the sender is by construction our own forge bot — a fixer that just pushed onto someone else's PR — so the guard swallowed the delivery the whole gate depends on.

    </details>

## [3.18.1](https://github.com/SocialGouv/iterion/compare/v3.18.0...v3.18.1) (2026-07-31)

## [3.18.0](https://github.com/SocialGouv/iterion/compare/v3.17.7...v3.18.0) (2026-07-31)

### Features

* **branch-improve-loop:** let the operator choose where they sit in the loop ([d81c0bf](https://github.com/SocialGouv/iterion/commit/d81c0bf7f5663c11d09f1fdb1d1f8002015f3ed9))

    <details><summary>why</summary>

    A reviewer alone already puts the human in the middle: findings land, the human decides which to act on and how. The fixer is the more invasive option, and offering only it would take that arbitration away — you either read findings yourself, or you hand the whole thing over.

    </details>
* **branch-improve-loop:** speak for the head you pushed, and never green it for free ([6398c48](https://github.com/SocialGouv/iterion/commit/6398c487d7587a7ca6ee87e5b102b99603ab2b6f)), references [#322](https://github.com/SocialGouv/iterion/issues/322)

    <details><summary>why</summary>

    A fixer that pushes moves the head, and the merge-gate status stays on the old one. A required check that is ABSENT is indistinguishable from one still running: the pull request waits for a context that will never arrive, with every other check green and nothing pointing at why. That is the failure #322 was written for, and pushing commits creates it on purpose.

    </details>
* **detect:** report pi, so the preference variable and the studio can see it ([a4be02b](https://github.com/SocialGouv/iterion/commit/a4be02b10dc0f8e153755e62c2fe86fd1d1e1045))

    <details><summary>why</summary>

    pi was reachable only by naming it on a node. Auto-selection filters on what Detect reports, so ITERION_BACKEND_PREFERENCE=pi was inert, and the studio's backend panel and Settings → Backends could not show it at all — for the one backend whose whole proposition is "you already hold a credential for one of ~36 providers".

    </details>
* **gate:** a red merge gate can launch the fixer itself, if the repo asks ([057be3a](https://github.com/SocialGouv/iterion/commit/057be3aae1fcaeba0d2c0c2f9b6a73241cb17364))

    <details><summary>why</summary>

    only those. The default stays as it was, and that is the decision rather than caution: a reviewer already leaves the developer in the middle (findings land, they choose what to act on, and a comment hands the work over whenever they want). Turning the hand-over automatic everywhere would take that arbitration from every developer on the repo to save one comment.

    </details>
* **handoff:** a reviewer and a fixer cooperate without the engine naming either ([fc3e342](https://github.com/SocialGouv/iterion/commit/fc3e3423b396d9932ca66286d4cf92685da04756))

    <details><summary>why</summary>

    The run-to-run hand-off was written as a pair of hardcoded identities: a `botID != "branch-improve-loop"` guard, a producer pinned to "review-pr", and that bot's three node names (`converge`, `merge_reviews`, `diff_precheck`) as engine constants. So the engine knew which bot reviews, which bot fixes, and the internal graph shape of one of them — the coupling CLAUDE.md forbids, and the reason a second reviewer or a second fixer needed an engine PR rather than a bundle.

    </details>
* **handoff:** the fixer answers the review, finding by finding ([cbdad61](https://github.com/SocialGouv/iterion/commit/cbdad61a1f198d779d4df28511524c6ac22e0d20))

    <details><summary>why</summary>

    The hand-off ran one way. A reviewer handed over N findings with stable ids, and the reply was a prose summary: the PR author was shown N problems and then told "hardened this PR", with no way to see which were answered, which were dismissed, and on what grounds. A later review, given nothing, re-raised whatever the fixer had silently decided against — the oscillating relay ADR-058 removed from the catalog, reconstituted across two bots.

    </details>
* **pi:** drive the openai-codex provider from the host's Codex login ([83cb5e5](https://github.com/SocialGouv/iterion/commit/83cb5e56ca12a3ecca396cdd3f111339cbc9bcbf))

    <details><summary>why</summary>

    pi reaches ~30 providers through an API-key environment variable and `openai-codex` through none of them: that provider is OAuth-only, its tokens live in pi's own auth.json, and they are minted by an interactive `/login`. So a host already holding a working Codex credential — the very file iterion reads for claw's ChatGPT-forfait path — could not hand it to pi at all, and `backend: "pi"` on a ChatGPT plan was unreachable. Verified two ways before building anything: pi's provider table lists an…

    </details>
* **pi:** keep per-run state out of the target repo's checkout ([74b71f0](https://github.com/SocialGouv/iterion/commit/74b71f0c7fbc2763c7a32fb1abb631fef631c456))

    <details><summary>why</summary>

    Five review rounds in a row found holes in the guards protecting <WorkDir>/.iterion/pi — a symlink at a component, at the leaf, at the .gitignore inside it; a relative root that made containment fail open; a pre-seeded .gitignore whose last effective rule re-included our files. Each fix was correct and each left an adjacent hole, because they all patch the same premise: iterion writes a credential and a session transcript into a directory the target repository controls.

    </details>
* **reviews:** the review hand-off carries the patch it already wrote ([208852f](https://github.com/SocialGouv/iterion/commit/208852fb4be4f2fc00b15dd97928387123b07838))

    <details><summary>why</summary>

    Seeding the fixer with the reviewer's findings exists to save it a round. The digest was dropping the fields that do exactly that.

    </details>
* **runtime:** the engine reports which skills it owns, so a backend can trust them ([b4abbdf](https://github.com/SocialGouv/iterion/commit/b4abbdfb3e6a2dc637df7e207e12322e41e6357f))

    <details><summary>why</summary>

    The durable fix for a boundary my last two attempts only pretended to hold. pi is launched with `--no-approve` to refuse the target repo's project-local resources, but `--skill` paths bypass that gate — so handing pi `<workDir>/.claude/skills` loaded whatever the repository itself ships there, as a trusted skill, on every node. For a webhook-launched review or triage bot against an untrusted repo that is attacker-authored prompt text.

    </details>

### Bug Fixes

* **delegate:** the other two writers under <WorkDir>/.iterion refuse a repo symlink too ([8183b6b](https://github.com/SocialGouv/iterion/commit/8183b6b73516068df26deaa8026c59a83258cb0b))

    <details><summary>why</summary>

    Revi raised this as an open question rather than a finding: if a repo-committed `.iterion` symlink is in the threat model for the credential, it is in scope for the composed system prompt and the session guard, which MkdirAll through the same path. It is — and the primitive is arguably worse there: a repo choosing where a host file lands, with directories created along the way, out of a path iterion picked.

    </details>
* **gate:** the zero-touch lane had no working brake ([b066d3a](https://github.com/SocialGouv/iterion/commit/b066d3aec8e22a990be42d89df140fd5881fe82c))

    <details><summary>why</summary>

    A simplify pass and an adversarial pass over the new lane. The two agreed on the same root cause from opposite directions: the launch tail was hand-rolled instead of reusing `launchWebhookTarget`, and everything that copy silently dropped was load-bearing.

    </details>
* **handoff:** five ways the gate went green when it should not have ([e862915](https://github.com/SocialGouv/iterion/commit/e86291598a166d3b109b3ffa56d6ceeb0b93eb86))

    <details><summary>why</summary>

    An adversarial pass over the branch. Two of these would have merged unfixed code through a required check.

    </details>
* **handoff:** the board lane is where a /command bot actually launches ([cb60ac0](https://github.com/SocialGouv/iterion/commit/cb60ac0ce5f7e327b0e3ad2ffbd64aa8899e18dc))

    <details><summary>why</summary>

    Moving the seed into the shared launch tail covered every lane but the one that matters most for a `/command` bot: with a cloud dispatcher active, a board-mode command materialises a card and RETURNS — the card is the launch, and the coordinator launches from BotArgs only. The tail is never reached, so the seed was dropped on exactly the path `/billy` takes in production, with no error and the bot falling back to its DSL default.

    </details>
* **pi:** --skill must not smuggle the target repo's own skills past --no-approve ([a951e6f](https://github.com/SocialGouv/iterion/commit/a951e6f63ad45edb491bc05cbc4c8db844ed4243))

    <details><summary>why</summary>

    A security regression I introduced with the bundle-skill fix, caught by Revi. Widening the gate from `len(task.SkillHints) > 0` to "the mirror directory is non-empty" changed WHAT gets handed over, not just how much: `<workDir>/.claude/skills` is a checkout of the TARGET repository under `worktree: auto`, so a repo committing its own `.claude/skills/x/SKILL.md` had that markdown loaded into every pi node as an explicitly-supplied skill.

    </details>
* **pi:** a bundle's skills must reach pi ([1dd232c](https://github.com/SocialGouv/iterion/commit/1dd232c0c900c1f1bd9c48887608860f37bdcd43))

    <details><summary>why</summary>

    The --skill flag was gated on `len(task.SkillHints) > 0`, which is the wrong signal. SkillHints carries ONLY the DSL `skills:` field — the skill LIBRARY — while every BUNDLE skill is mirrored into <workDir>/.claude/skills/ without ever touching it. So for a bundle bot the flag was never emitted: pi had zero skill awareness, and an agent whose own prompt ordered "LOAD YOUR SKILLS FIRST" was left hunting for files it had no way to see.

    </details>
* **pi:** a relative workdir disarmed the guard entirely, and the walk covered only the credential ([67fd79e](https://github.com/SocialGouv/iterion/commit/67fd79e50c25130df3e39f90e32b296222f0d2bb))

    <details><summary>why</summary>

    Adversarial round 3, again aimed at round 2's fix. Third time the defect was in the previous round's patch.

    </details>
* **pi:** absolutise the seed root — a relative --store-dir broke the symlink guard OPEN ([56a4116](https://github.com/SocialGouv/iterion/commit/56a411676dc62628aa3865b99caca5548389fed6))

    <details><summary>why</summary>

    store.ResolveStoreDir returns a --store-dir override VERBATIM, and `iterion schedule` renders it into cron lines as given, so the seed root can be relative. Three consequences, and the first fails open:

    </details>
* **pi:** containment decides the location for every branch, not just the operator's ([1f5f744](https://github.com/SocialGouv/iterion/commit/1f5f7448a0a7d32f186d39dae33da2373739f681))

    <details><summary>why</summary>

    Adversarial round 5. One finding, and it is a regression against the parent rather than an unclosed gap.

    </details>
* **pi:** containment fails CLOSED, the leaf guard is unconditional, and restore 7 deleted tests ([0006090](https://github.com/SocialGouv/iterion/commit/00060908f366114329c3f91fbcd44670990172cd))

    <details><summary>why</summary>

    Adversarial review (opus, max effort). Two HIGH findings, both verified by execution, and both regressions this series introduced.

    </details>
* **pi:** gitignore is last-match-wins, so a `*` anywhere proved nothing ([1d13476](https://github.com/SocialGouv/iterion/commit/1d1347619068098ec877abcd5e51990fe5e90fa0))

    <details><summary>why</summary>

    Same fail-open shape as the symlink series, one layer up in the PARSING. piWriteIgnoreGuard short-circuited on any line trimming to `*`, but git resolves ignore rules last-match-wins: a repo committing `.iterion/pi/.gitignore` with `*\n!auth.json` (or plainly `*\n!*`) made the function return "already guarded" having written nothing, leaving the seeded ChatGPT access + refresh token stageable by a campaign agent's `git add -A` and fast-forwarded onto the operator's branch.

    </details>
* **pi:** guard the directory pi actually writes into, and stop failing a legitimate store symlink ([5a39c1e](https://github.com/SocialGouv/iterion/commit/5a39c1ec63d55afe62fe50b15f3fb87539034e17))

    <details><summary>why</summary>

    The previous commit put the symlink refusal in writeSystemPromptFile, which was wrong twice over. It could never be the boundary — that function is SKIPPED for a node with an empty system prompt, and for pi `AppendToNative` means an agent node with only a `prompt:` yields exactly that — so `.iterion/pi` as a tracked symlink still redirected the extension bundle and pi's own session transcripts, which carry the node's full conversation. And it was over-broad: it refused a symlinked `.iterion`,…

    </details>
* **pi:** guard the token wherever it lands in the worktree, not just under sandbox ([0581b17](https://github.com/SocialGouv/iterion/commit/0581b1718f2c38384014aaf51c72dc8e939af59d))

    <details><summary>why</summary>

    Revi caught an asymmetry I created in the previous commit: I added the ignore-guard verification to the SANDBOXED branch only, while the store branch writes to <StoreDir>/pi with no equivalent check — and this repo's own dogfood instructions prescribe `--store-dir "$PWD/.iterion"`, which puts the store inside the git repo. So the non-sandboxed path carried the exact `git add -A` exposure the sandboxed one had just been hardened against, and the new refusal made the inconsistency look deliberate.

    </details>
* **pi:** ITERION_PI_BIN is a host path, and a source name is not a set variable ([f36e7e1](https://github.com/SocialGouv/iterion/commit/f36e7e1b33890066fadff531bc2159861998f1af))

    <details><summary>why</summary>

    Revi round 19, both real.

    </details>
* **pi:** keep the ChatGPT token out of git, and stop refusing nodes we do not fund ([7903c78](https://github.com/SocialGouv/iterion/commit/7903c781a2cf3a48a1fe0011bbb41d7661c0008c))

    <details><summary>why</summary>

    Two more from Revi, both medium.

    </details>
* **pi:** make state files reach a copy-based sandbox ([040b59e](https://github.com/SocialGouv/iterion/commit/040b59e61f7ebf8df95c52ec162d3b4cc2cf7fb5))

    <details><summary>why</summary>

    A driver whose workspace is a COPY of the host's (kubernetes tar-streams it at pod start) never sees a host-side write made afterwards. pi is handed three files BY PATH — the iterion extension, the composed system prompt, the openai-codex credential — and resolves them inside the pod, so all three were missing there. Caught by the first live cloud run of the backend (019fb968): every attempt died on `Extension path does not exist`, and the other two would have degraded silently, which is worse.

    </details>
* **pi:** make the zero-skill warning reachable, and stop the guard eating a .gitignore ([b4c9e20](https://github.com/SocialGouv/iterion/commit/b4c9e209f36a5682af90de398f7184ff03d976d6))

    <details><summary>why</summary>

    The diagnostic that says "pi got no skills" was written and then never wired: piSkillArgs took a logger, both transports passed nil, and the failure it exists to report — the agent hunting for files its own prompt told it to load — stayed as silent as before. Both argv builders now carry the backend's logger, and the warning fires on the case that matters: the engine named skills and none of them resolved.

    </details>
* **pi:** one --skill per skill, and make detection read what the run reads ([b56fbf3](https://github.com/SocialGouv/iterion/commit/b56fbf300ea55507ee36051cd1862f560a5a5f90))

    <details><summary>why</summary>

    Three from Revi, all consequences of the previous two commits.

    </details>
* **pi:** StateDir reports WHO can plant, and the pre-flight call is finally tested ([a2ada61](https://github.com/SocialGouv/iterion/commit/a2ada611bfabad6dd644897df1e6c9617364d2fd))

    <details><summary>why</summary>

    Adversarial round 4. Two findings, and both are about the previous round's claims rather than its logic — the helper work held.

    </details>
* **pi:** the codex bridge refreshed on every node and broke under sandbox ([8cf9bab](https://github.com/SocialGouv/iterion/commit/8cf9babc8ef00c3d81a5a510b913f1b4c9ae5bc2))

    <details><summary>why</summary>

    Six defects from an adversarial review of the bridge. The two that mattered were both mine reasoning backwards from an untested assumption.

    </details>
* **pi:** the codex bridge worked on `run` and not on `resume` ([c5b0e76](https://github.com/SocialGouv/iterion/commit/c5b0e763a55d703a32e4c7be3869a357540e36ea))

    <details><summary>why</summary>

    Two defects a resumed run exposed, neither reachable from `iterion run`.

    </details>
* **pi:** the containment fix was killing the credential it protects ([2e919f1](https://github.com/SocialGouv/iterion/commit/2e919f10d1ff99a07dd5b1b04121f9dcf9673d0c))

    <details><summary>why</summary>

    Adversarial round 2, aimed at the previous round's FIXES because that is where the last five rounds found the defect. It held.

    </details>
* **pi:** the ignore guard must not follow a symlink the checkout shipped ([a37a99d](https://github.com/SocialGouv/iterion/commit/a37a99d9760802b5a5418f2a75f04cda2fec3ce2))

    <details><summary>why</summary>

    The path guards walk TO the seed root; none of them looks INSIDE it. And MkdirAll is a no-op on a `.iterion/pi` the checkout pre-populated, so a repo can ship `.iterion/pi/.gitignore` as a tracked symlink. Two things follow, and the second is the one that matters:

    </details>
* **pi:** the last two Revi findings, one of them my own argument turned on me ([4fcef55](https://github.com/SocialGouv/iterion/commit/4fcef55128417c2c64468b6cd0d4bd2bfda80900))

    <details><summary>why</summary>

    **detect's Codex probe was more optimistic than the bridge that consumes the credential.** codexChatGPTAvailable hand-rolled a parse accepting `auth_mode: "chatgpt"` alone, while piCodexSeed gates on CodexCredentialsView.IsChatGPTMode — which also requires an access token and an account id. So a partially-written or logged-out Codex state made detect report pi available, the preference variable resolved there, and the bridge then stepped aside and left the node to die with "No API key found for…

    </details>
* **pi:** the sibling ignore guard had the same symlink hole, one level up ([c83e885](https://github.com/SocialGouv/iterion/commit/c83e885ecc8abbcb9c1b54f69963698320126b17))

    <details><summary>why</summary>

    Third instance of one shape, so this shares the CHECK rather than adding a third copy of it. piHideWorkspaceSessionDir used a FOLLOWING os.Stat on <WorkDir>/.iterion/.gitignore, which a repo can ship as a tracked symlink. Both outcomes were bad and the second was silent: a DANGLING link made os.WriteFile create an attacker-chosen host file, and a link to any existing path made the stat succeed, so the function returned as if the workspace were guarded — leaving pi's session transcripts and the…

    </details>
* **pi:** treat Revi's five findings on the pi follow-ups ([d269907](https://github.com/SocialGouv/iterion/commit/d269907f6c19b8c9ed1c2bb848c0d58e5b80e2ec))

    <details><summary>why</summary>

    **high — the stale-seed sweep deleted a live peer's credential.** I added the sweep to fix an adversarial-review finding (a SIGKILL strands tokens) and asserted an invariant that is false: "anything already here when a new node starts is by definition abandoned". The root is SHARED — by every node of a run under sandbox, and off it by every run in the store — while iterion permits parallel branches and the studio runs several pipelines at once. A second openai-codex node therefore swept the…

    </details>
* **pi:** write our own ignore guard instead of trusting one we did not author ([1ee62d1](https://github.com/SocialGouv/iterion/commit/1ee62d10e0a9ff3271ca8e49453bf9f5ab82e671))

    <details><summary>why</summary>

    Revi's high finding, and it is the root cause of the two patches before it: the guard verified `<workDir>/.iterion/.gitignore` no matter where the seed root actually landed. That file is best-effort, is deliberately never overwritten when the repo already tracks one, and says nothing about a root `--store-dir` put elsewhere under the worktree. So the check could pass while the credential sat somewhere it did not cover.

    </details>
* **runtime:** back the sandbox scratch with a shared, persistent host dir ([#330](https://github.com/SocialGouv/iterion/issues/330)) ([9c8e0e2](https://github.com/SocialGouv/iterion/commit/9c8e0e2a278db652ede08ce3482fe553c8b4b01d))

    <details><summary>why</summary>

    A sub-bot child runs in its OWN container. ${PROJECT_SCRATCH_DIR} resolved to a container-local /tmp/iterion-scratch, so the file a child wrote there was invisible to the parent that later read the same path. The child reported success, the parent read an empty directory, and the run only failed much later — as "not enough results", far from the cause.

    </details>
* **runtime:** library skills ride the ownership channel, and the seed root refuses a symlink ([8121570](https://github.com/SocialGouv/iterion/commit/8121570d858e30b8fccf2a15c5c8fe34f20ea4c5))

    <details><summary>why</summary>

    A skill hint is not provenance. One is recorded for every skill the workflow's `skills:` field references — INCLUDING one the target repo pre-empted, because the hint describes what the agent will see, not who wrote it. piSkillArgs derived `<workspace>/.claude/skills/<name>` from that name, so an untrusted checkout that shipped a same-named skill got its own file handed to pi as a trusted `--skill`: precisely the routing-around-`--no-approve` the gate exists to close.

    </details>
* **runtime:** own the FILE, not the directory a repo can pre-populate ([cbee1e1](https://github.com/SocialGouv/iterion/commit/cbee1e1d4c27202cb45b874e9795e0b90e43e816))

    <details><summary>why</summary>

    A flat bundle source writes <stem>/SKILL.md, and MkdirAll succeeds happily on a <stem>/ the checkout already ships. Reporting that DIRECTORY as owned therefore vouched for whatever the target repo planted beside our file: a repo committing .claude/skills/whats-next/evil.md (no SKILL.md, so nothing shadows) got the whole directory back on the owned list. Naming the one file we wrote cannot carry a sibling. Library skills had the identical shape and get the same treatment; the directory-form…

    </details>
* **runtime:** plugin skills reach pi, and directory skills survive a resume ([65227b0](https://github.com/SocialGouv/iterion/commit/65227b04ffdfc2a41c247c45148a413ec167210a))

    <details><summary>why</summary>

    Two regressions the engine-owned skill list introduced, both silent.

    </details>
* **secrets:** state only the billing arrangement that was measured ([2fdcb80](https://github.com/SocialGouv/iterion/commit/2fdcb8057145a556d20c050b4b8720c87ec5083e))

    <details><summary>why</summary>

    The shared subscription-OAuth warning asserted that "third-party apps bill against your EXTRA USAGE balance, not your plan limits" for ANY provider, interpolating only the name. That sentence describes Anthropic's arrangement, measured live during the pi work. Applied to OpenAI it states a billing model nobody here verified — and an operator reading a confident sentence acts on it.

    </details>

## [3.17.7](https://github.com/SocialGouv/iterion/compare/v3.17.6...v3.17.7) (2026-07-31)

### Bug Fixes

* **cli:** anchor the run store on the working directory ([#328](https://github.com/SocialGouv/iterion/issues/328)) ([61af254](https://github.com/SocialGouv/iterion/commit/61af25410dae61a789057aca0f0953de056d80f8))

    <details><summary>why</summary>

    `iterion run project/bots/x/main.bot` resolved its store from the .bot's own directory, so a bot living inside the project it drives was keyed on that subdirectory: the run landed in ~/.iterion/projects/<project-bots-x-key>/ while resume, inspect, issue, dispatch and the studio all resolve <project>/.iterion. Launching succeeded and every follow-up reported "run not found" — the run was invisible to the board and could not be resumed.

    </details>

## [3.17.6](https://github.com/SocialGouv/iterion/compare/v3.17.5...v3.17.6) (2026-07-31)

## [3.17.5](https://github.com/SocialGouv/iterion/compare/v3.17.4...v3.17.5) (2026-07-31)

## [3.17.4](https://github.com/SocialGouv/iterion/compare/v3.17.3...v3.17.4) (2026-07-30)

### Bug Fixes

* **golden-master:** five defects the a11y lane only revealed on a real runner ([6fbf1ba](https://github.com/SocialGouv/iterion/commit/6fbf1ba5f81cf6e8ecebe683af47f0a49ed64bad))

    <details><summary>why</summary>

    The lane worked on the machine that wrote it and failed six times in a row on a CI runner. Every fix below is now verified by a green pipeline — full conjunction, 19 of 19 mutants, on a real GitLab runner with the lane active.

    </details>

## [3.17.3](https://github.com/SocialGouv/iterion/compare/v3.17.2...v3.17.3) (2026-07-30)

### Bug Fixes

* **golden-master:** the browser stalls in a container instead of failing ([f29dd4e](https://github.com/SocialGouv/iterion/commit/f29dd4e7d98440d4ff0fba7f8abdce6fe6f7286e))

    <details><summary>why</summary>

    /dev/shm defaults to 64 MB in a container, and the renderer blocks there rather than erroring. The symptom is not a crash but a page that never finishes loading: the load event does not come, and the load ceiling fires while blaming the host's speed. Measured on a real runner — 240 seconds on the first page, instant on the same page locally.

    </details>
* **server+studio:** show the paused node's instructions on review cards ([#326](https://github.com/SocialGouv/iterion/issues/326)) ([a46527d](https://github.com/SocialGouv/iterion/commit/a46527d1415e2e083e277a1e1bf8bbef77b1981b))

    <details><summary>why</summary>

    A board review card rendered an answer box with no question above it. The operator saw "Awaiting input", a run link and an empty "Message" field — nothing else.

    </details>

## [3.17.2](https://github.com/SocialGouv/iterion/compare/v3.17.1...v3.17.2) (2026-07-30)

### Bug Fixes

* **server:** a review that dies must still leave a verdict on the PR ([#322](https://github.com/SocialGouv/iterion/issues/322)) ([d337007](https://github.com/SocialGouv/iterion/commit/d337007d1b3975cb70f5bbadfd1c7e1aba730473)), references [#314](https://github.com/SocialGouv/iterion/issues/314) [#318](https://github.com/SocialGouv/iterion/issues/318) [#314](https://github.com/SocialGouv/iterion/issues/314)

    <details><summary>why</summary>

    A required check that is ABSENT is indistinguishable from one still running. The PR waits for a context that will never arrive, and nothing — not the run, not the PR, not the check — says why. Only someone who knows to re-trigger the bot can unstick it.

    </details>

## [3.17.1](https://github.com/SocialGouv/iterion/compare/v3.17.0...v3.17.1) (2026-07-30)

### Bug Fixes

* **review-pr:** a mono review must not report cross-family confirmation ([#320](https://github.com/SocialGouv/iterion/issues/320)) ([34bd008](https://github.com/SocialGouv/iterion/commit/34bd0087976af9f8d8e9bdef5bbfbb3e2ed98e42)), references [socialgouv/buildkit-operator#6](https://github.com/socialgouv/buildkit-operator/issues/6)

    <details><summary>why</summary>

    Every comment ended with "0 finding(s) cross-confirmed by both model families", including under the default mono topology where a single family reviews. That reads as two families having looked and agreed on nothing — a statement about a comparison that never took place. Spotted on socialgouv/buildkit-operator#6.

    </details>

## [3.17.0](https://github.com/SocialGouv/iterion/compare/v3.16.1...v3.17.0) (2026-07-30)

### Features

* **golden-master:** an `a11y` lane that audits the rendered page, not the markup ([c354a8a](https://github.com/SocialGouv/iterion/commit/c354a8ae7ee37155ed6c3b2303ad24ea5ce35127))

    <details><summary>why</summary>

    A net that watches HTTP responses sees the markup change; it cannot say what degraded. Removing an accessible name moves an HTML reference and nothing in the report distinguishes it from a reworded label.

    </details>
* **golden-master:** put the target's own test suite through the same trials ([7ab1596](https://github.com/SocialGouv/iterion/commit/7ab1596778520783be228894e04bf464d22eca73))

    <details><summary>why</summary>

    The net and the existing test suite both watch the same repository and both claim to protect against regression. Comparing them by assertion is worthless; `suite-vs-net.py` applies every mutant in turn and runs the target's own suite, so one set of trials produces both figures.

    </details>

## [3.16.1](https://github.com/SocialGouv/iterion/compare/v3.16.0...v3.16.1) (2026-07-30)

### Bug Fixes

* **dep-update-guard:** don't let the merge check claim an alignment that never happened ([#317](https://github.com/SocialGouv/iterion/issues/317)) ([4476135](https://github.com/SocialGouv/iterion/commit/44761351d33c4e921eaf935a284d2d99ea30dfc4)), references [socialgouv/buildkit-operator#15](https://github.com/socialgouv/buildkit-operator/issues/15)

    <details><summary>why</summary>

    Observed on socialgouv/buildkit-operator#15, the first PR to travel the whole loop unattended: the required check displayed "supply-chain audit clean; alignment committed, build verified" while the commit step had reported committed=false — the branch needed no alignment at all.

    </details>
* **review-pr:** the stale-anchor guard compared a template string to a sha ([#318](https://github.com/SocialGouv/iterion/issues/318)) ([7b87b5f](https://github.com/SocialGouv/iterion/commit/7b87b5f372f1c69e05a854c31d0cf6c6a45ad23a))

    <details><summary>why</summary>

    Revi has published nothing since the runner picked up #290's reviewed-SHA guard. Every review ends:

    </details>

## [3.16.0](https://github.com/SocialGouv/iterion/compare/v3.15.0...v3.16.0) (2026-07-29)

### Features

* **golden-master:** an `asset` lane that inventories the build, not the worktree ([0abe16a](https://github.com/SocialGouv/iterion/commit/0abe16abf7dd426029737b10620dba4e394c8fa4))

    <details><summary>why</summary>

    A net that watches HTTP responses and rendered documents does not watch the files a page loads. On the repository this bot was exercised against, the whole client layer — every stylesheet, every vendor script, the view framework itself — was absent from the environment and answered 404, and not one reference moved. A total absence of the client layer was indistinguishable from its presence.

    </details>

### Bug Fixes

* **runtime:** a subbot child carries its parent from its first write ([3a61d2d](https://github.com/SocialGouv/iterion/commit/3a61d2da444cd634f0fa91b4970a5c1a56464486))

    <details><summary>why</summary>

    The engine created every run with CreateRun and stamped ParentRunID in a follow-up SaveRun. Between the two writes the row existed, was `running`, and had no parent — and a row with no parent is indistinguishable from a top-level run, which is exactly what the orphan reconciler judges. Every subbot child goes through this path: the runtime spawns them, and only Service.Launch pre-creates its own rows.

    </details>

## [3.15.0](https://github.com/SocialGouv/iterion/compare/v3.14.0...v3.15.0) (2026-07-29)

### Features

* **pi:** pi as a first-class execution backend (ADR-085) ([#308](https://github.com/SocialGouv/iterion/issues/308)) ([0710335](https://github.com/SocialGouv/iterion/commit/07103357bbae836ebef5fb77bd6cd004520faa0e)), references [#168](https://github.com/SocialGouv/iterion/issues/168)

    <details><summary>why</summary>

    pi (pi.dev) is a multi-provider agent harness reaching ~36 first-class providers behind one agent loop. It is the backend to reach for when a node needs a model claude_code and claw cannot run.

    </details>

## [3.14.0](https://github.com/SocialGouv/iterion/compare/v3.13.0...v3.14.0) (2026-07-29)

### Features

* **dsl+studio:** file schema fields — operator uploads at a human gate ([#315](https://github.com/SocialGouv/iterion/issues/315)) ([a6edd19](https://github.com/SocialGouv/iterion/commit/a6edd19f83e235f39f63168497d54a813a0e4847))

    <details><summary>why</summary>

    Adds a `file` schema field type so a human node can ask the operator for bytes, not just text: the studio renders a file picker at the gate, the answer is uploaded to the run, and the runtime promotes it to a run attachment before the workflow resumes. `iterion resume --answer key=@./path` accepts the same fields from the CLI.

    </details>

### Bug Fixes

* **dep-update-guard:** merge the PR the forge already reports as green ([#314](https://github.com/SocialGouv/iterion/issues/314)) ([a140746](https://github.com/SocialGouv/iterion/commit/a140746ba91b455766a2d85f74e4a78da14ea279)), references [socialgouv/buildkit-operator#5](https://github.com/socialgouv/buildkit-operator/issues/5)

    <details><summary>why</summary>

    enablePullRequestAutoMerge only accepts a PR with something left to wait for; GitHub answers UNPROCESSABLE "Pull request is in clean status" otherwise. The audit takes longer than CI, so that is the ordinary case: a live run on socialgouv/buildkit-operator#5 posted its gate green and then armed nothing, leaving the PR open with every check passing.

    </details>
* **golden-master:** a gate on an uncommitted tree judges a tree that never existed ([0f99ce5](https://github.com/SocialGouv/iterion/commit/0f99ce5d7031d83ce77ee144038f6016eac2f43e))

    <details><summary>why</summary>

    Mutant reverts are `git checkout -- <file>`, restoring HEAD. The gate captures references from the working tree it starts with, the first file mutant snaps those files back to HEAD, and every capture after that describes something else. Uncommitted work is destroyed on the way, silently, and the verdict belongs to no tree that ever existed.

    </details>
* **pluginsource:** publish a plugin checkout atomically, one clone per key ([#313](https://github.com/SocialGouv/iterion/issues/313)) ([9065d52](https://github.com/SocialGouv/iterion/commit/9065d52bd13568f3e1f08df911b1c3bc28de1c85))

    <details><summary>why</summary>

    `git init` creates .git before the fetch and checkout land, and Fetch treated the presence of .git as "this tree is complete". On a cold pod taking several launches at once, the losers of that race were handed a directory holding nothing but .git — and the plugin loader then reported it as "has no plugin.yaml and no skills/", a 502 that names the wrong cause and blocks every launch for the tenant.

    </details>

## [3.13.0](https://github.com/SocialGouv/iterion/compare/v3.12.1...v3.13.0) (2026-07-29)

### Features

* projected improvements from the AIDD framework (skill lint, fit/rot lens, memory supersede, dependency gating, reviewed-SHA guard, hold-labels) ([#290](https://github.com/SocialGouv/iterion/issues/290)) ([eb0ecbe](https://github.com/SocialGouv/iterion/commit/eb0ecbec1bdcf68d7a564cad36360fb3c5915fb1)), references [#13](https://github.com/SocialGouv/iterion/issues/13)

    <details><summary>why</summary>

    ScanFrontmatter (the shared SKILL.md parser used by both the skill library and runview's bundle-skill catalog) only read the value on the same line as `description:`. For the common `description: >` / `description: |` block-scalar form it therefore returned just ">" or "|" — so the router (Nexie) and discovery saw a one-character description for every skill authored that way.

    </details>

### Bug Fixes

* **bots:** mono must not hand the merger a raw template ([74c46e2](https://github.com/SocialGouv/iterion/commit/74c46e2a72fc1b1f87309d3a05c01bb206149a72))

    <details><summary>why</summary>

    In mono only one reviewer runs, and an {{outputs.<absent node>.<field>}} reference renders as a LITERAL placeholder rather than as nothing — so the merging agent was shown what looks like a broken template instead of "that family did not run". Observed live: a mono review's own reviewer mistook it for a reviewer failure and said so in its report.

    </details>
* **golden-master:** the emitted runner names a missing interpreter instead of blaming its own tests ([446f9bf](https://github.com/SocialGouv/iterion/commit/446f9bfb1f06cd2b77511e733fd8eefbc44bc8a7))

    <details><summary>why</summary>

    A missing python3 surfaced as "the canonicaliser tests FAIL" — a message that accuses the net when the environment is what is absent. Seen for real on a CI image nobody had checked carried an interpreter. The whole net is Python; saying so plainly costs one line and saves an hour of looking in the wrong place.

    </details>
* **golden-master:** the replayability check no longer reports success when it cannot tell ([fd7bd99](https://github.com/SocialGouv/iterion/commit/fd7bd990ea0663beb6cc091ac3019efcf8f8ef8d))

    <details><summary>why</summary>

    `git check-ignore` returns 0 for ignored, 1 for not, and 128 when there is no repository or no git. The check only looked at 0, so on any workspace that is not a checkout it discriminated NOTHING and left `runner_replayable: true` — the good outcome, reported for the one reason it could not see. Some CI runners hand the job a COPY of the tracked files rather than a checkout, which is exactly where this happens.

    </details>

## [3.12.1](https://github.com/SocialGouv/iterion/compare/v3.12.0...v3.12.1) (2026-07-28)

## [3.12.0](https://github.com/SocialGouv/iterion/compare/v3.11.1...v3.12.0) (2026-07-28)

### Features

* **cli:** add --commit flag to version command ([#52](https://github.com/SocialGouv/iterion/issues/52)) ([a8af8ee](https://github.com/SocialGouv/iterion/commit/a8af8eef89c0c03ced48bcf09588ca70801b4051))

    <details><summary>why</summary>

    `iterion version --commit` prints only the bare git commit SHA on a single line (via cli.RawCommit()), so scripts can capture the SHA directly without parsing the full human-readable version string. The default `iterion version` output is unchanged. Output now goes through cmd.OutOrStdout() so the command is testable against a captured buffer.

    </details>

## [3.11.1](https://github.com/SocialGouv/iterion/compare/v3.11.0...v3.11.1) (2026-07-28)

## [3.11.0](https://github.com/SocialGouv/iterion/compare/v3.10.4...v3.11.0) (2026-07-28)

### Features

* **studio+runtime:** pipeline-board & human-review overhaul + subbot editor UX (WIP snapshot) ([#300](https://github.com/SocialGouv/iterion/issues/300)) ([991ee1d](https://github.com/SocialGouv/iterion/commit/991ee1d3aec4fb939456431322870c6a863fb40d)), references [#244](https://github.com/SocialGouv/iterion/issues/244)

    <details><summary>why</summary>

    Answering a paused `human` node from the studio (pipeline-board card or run console) could silently do nothing and lose the reviewer's notes. Verified end-to-end in a real browser (Playwright against the built SPA).

    </details>

### Bug Fixes

* **golden-master:** promote_audit commits what it promotes ([ae26833](https://github.com/SocialGouv/iterion/commit/ae268339e95b5c6bd1d01fc21af74f04b720e7db))

    <details><summary>why</summary>

    Moving files into the worktree is not publishing them. The worktree is destroyed when the run ends, so a promotion that only moves has produced nothing that outlives the run -- while reporting promoted: 8.

    </details>
* **reviewtopology:** make mono the default review topology ([03309fd](https://github.com/SocialGouv/iterion/commit/03309fdb365b9f6a7a15896af024b0d22599bbac))

    <details><summary>why</summary>

    Revi ran BOTH family reviewers on every review, unconditionally: it never declared the ADR-052 topology vars (its only review_mode-looking var is pr_review_mode, an unrelated inline/summary publish setting), so InjectIfDeclared no-op'd on it and there was no frugal path at all. With the merge gate wired, review_on_sync re-reviews on every push — so each push cost two full reviewer passes on an instance that should be running mono.

    </details>

## [3.10.4](https://github.com/SocialGouv/iterion/compare/v3.10.3...v3.10.4) (2026-07-28)

### Bug Fixes

* **dep-update-guard:** a redirect must not degrade the publish POST into an unexplainable 401 ([#312](https://github.com/SocialGouv/iterion/issues/312)) ([9d5efc6](https://github.com/SocialGouv/iterion/commit/9d5efc6c995568e32e649aea77db2091dcdca525))

    <details><summary>why</summary>

    `forge_publish_url` is the FULL endpoint URL, not a base — Revi uses it verbatim. Vetty appended the path to it, so it POSTed to `…/api/v1/forge/publish-review/api/v1/forge/publish-review`. That path is not the auth-exempt route, so the global auth middleware answered 401 "authentication required" and no commit status was ever posted.

    </details>

## [3.10.3](https://github.com/SocialGouv/iterion/compare/v3.10.2...v3.10.3) (2026-07-28)

## [3.10.2](https://github.com/SocialGouv/iterion/compare/v3.10.1...v3.10.2) (2026-07-28)

## [3.10.1](https://github.com/SocialGouv/iterion/compare/v3.10.0...v3.10.1) (2026-07-28)

### Bug Fixes

* make paused resumes reliable and compact run details ([#301](https://github.com/SocialGouv/iterion/issues/301)) ([876e847](https://github.com/SocialGouv/iterion/commit/876e8477c2c3063ecf60a90784ed8b7b92bbc2f6))

## [3.10.0](https://github.com/SocialGouv/iterion/compare/v3.9.1...v3.10.0) (2026-07-28)

### Features

* **webhooks:** /revi approve — maintainer override for the merge gate ([#292](https://github.com/SocialGouv/iterion/issues/292)) ([737d2fe](https://github.com/SocialGouv/iterion/commit/737d2fe92fc719c3605991eefd00824893ab7052))

    <details><summary>why</summary>

    The human-arbitration escape hatch for the Revi merge gate: a trusted maintainer comments `/revi approve [reason]` on a PR to force-green the `revi/review` commit status on the current head, for a finding they dispute — without launching a re-review and without needing admin merge-queue bypass.

    </details>

### Bug Fixes

* **dep-update-guard:** publish to the endpoint the server injected, not under it ([#309](https://github.com/SocialGouv/iterion/issues/309)) ([1867b14](https://github.com/SocialGouv/iterion/commit/1867b14e2db3556d3aad859863ef78465f283242))

    <details><summary>why</summary>

    `forge_publish_url` is the FULL endpoint URL, not a base — Revi uses it verbatim. Vetty appended the path to it, so it POSTed to `…/api/v1/forge/publish-review/api/v1/forge/publish-review`. That path is not the auth-exempt route, so the global auth middleware answered 401 "authentication required" and no commit status was ever posted.

    </details>

## [3.9.1](https://github.com/SocialGouv/iterion/compare/v3.9.0...v3.9.1) (2026-07-28)

### Bug Fixes

* **golden-master:** one rule for the sealed path, asked rather than copied ([b8ab790](https://github.com/SocialGouv/iterion/commit/b8ab7901d5ac5135676d78e20e3e52a5bd116885))

    <details><summary>why</summary>

    The morning's fix made the campaign and the gate DERIVE the sealed path from the same rule instead of one dictating it to the other. The very next commit added a third party -- the node promoting a spent set to published evidence -- carrying its own hand-written copy of that derivation. It resolved into a different repository's scratch entirely, promoted nothing, and reported success.

    </details>
* **runview+cli:** a paused run advertised resumable before it was, and the group guard demanded a contract three groups already had ([#307](https://github.com/SocialGouv/iterion/issues/307)) ([2cf72f3](https://github.com/SocialGouv/iterion/commit/2cf72f37cdfad8464d526fe28456841c99ce9761)), closes [#5](https://github.com/SocialGouv/iterion/issues/5)

    <details><summary>why</summary>

    When a run parks on a human gate the engine writes paused_waiting_human to the STORE, returns ErrRunPaused, and only then does the goroutine carrying it call Deregister on its way out. Between those, the public signal says "resumable" while the handle is still held — and the studio and the pipeline-board sidebar offer Resume on exactly that signal. A resume landing in the window failed with `run "..." is already registered`, which reads as a bug to an operator and is one to any automated chain…

    </details>

## [3.9.0](https://github.com/SocialGouv/iterion/compare/v3.8.1...v3.9.0) (2026-07-28)

### Features

* **bots:** modernize (Morphy) — gate-to-gate lots against an oracle it cannot rewrite ([f7b72e9](https://github.com/SocialGouv/iterion/commit/f7b72e9c0609b09b2955316811969c766500d271))

    <details><summary>why</summary>

    The unit of work is the LOT, not the package. A dependency-upgrade pipeline whose failure path is revert-this-package-and-continue cannot express a runtime move that touches nine hundred files at once, so this is a separate bot rather than a widening of that one.

    </details>
* **golden-master:** a spent held-out set becomes published evidence ([8ce6b92](https://github.com/SocialGouv/iterion/commit/8ce6b922a0cd33778fc8c9f822f3c033a5b204cf))

    <details><summary>why</summary>

    The sealed set protected the hardening loop and then vanished with the run, which left the committed net unable to support its own headline claim. A third party could read '7/7 held-out detected' and had exactly as much reason to believe it as they have to believe any delivery's self-reported figures — which is to say none, and that is precisely the criticism this bot exists to make.

    </details>
* **model:** move the fleet to the Claude 5 family, and read the generation instead of listing it ([1978c3a](https://github.com/SocialGouv/iterion/commit/1978c3a773bae49575748b7165e7cd8172d9e07f))

    <details><summary>why</summary>

    Reasoning capability was decided by a list of known model ids. A list is silent when it is wrong: a model absent from it is classified as non-reasoning, extended thinking is never requested, and the run pays full price for a degraded answer without a single warning. claude-opus-5 matched none of the five patterns.

    </details>
* **models:** audit committed prices against the ones already being fetched ([02d84c0](https://github.com/SocialGouv/iterion/commit/02d84c0f172efc7a381c43f9472490d5b19443ed))

    <details><summary>why</summary>

    iterion downloads model pricing from the spec aggregator, caches it for 24h, and never reads it: InputCostPerM and OutputCostPerM were parsed and dropped, with no consumer anywhere in the tree. Meanwhile the cost estimator asks a different live source and falls back to a hand-maintained table. Two sources of truth for the same number, never compared.

    </details>
* **reviews+deps:** one merge gate per repo, and Vetty guards Renovate PRs end-to-end ([#306](https://github.com/SocialGouv/iterion/issues/306)) ([eac354f](https://github.com/SocialGouv/iterion/commit/eac354f80affd55a8570611743d4dd7cb9ce4c98)), references [#300](https://github.com/SocialGouv/iterion/issues/300)

    <details><summary>why</summary>

    A repo webhook could only ever launch a single bot per delivery: SelectBot() returns "" as soon as two bots are enabled, so the lane fell back to the hardcoded "review-pr". Co-enabling a dependency guard and a reviewer therefore lost the guard entirely, and — because the shared AuthorAllowlist is the union of every bot's, nil as soon as one bot is open — the guard's author filter was discarded too.

    </details>

### Bug Fixes

* **golden-master:** a spent held-out set is not a broken seal ([b14faca](https://github.com/SocialGouv/iterion/commit/b14facac8c0c3797ef1faab944ae54e15aef96bf))

    <details><summary>why</summary>

    Publishing the set created a third way to have nothing to score, and the harness knew only two. A replay after promotion accused the operator of having cleared the seal, when the set had in fact been scored once and published exactly as designed.

    </details>
* **golden-master:** enforce the corpus width floor, on distinct references ([0b428ec](https://github.com/SocialGouv/iterion/commit/0b428ec2d21523e1c57eb67e73f5099cf2a6ca40))

    <details><summary>why</summary>

    min_corpus existed only in the campaign prompt. The harness never read it and the gate never checked it: a corpus of three entries passed as long as the three were seen. Same shape as the seal that was guaranteed by a sentence in a skill — an obligation stated to the agent with nothing behind it.

    </details>
* **golden-master:** fail the gate when the emitted net cannot be replayed ([5b23373](https://github.com/SocialGouv/iterion/commit/5b23373328fa89f78e79f603556c0a918f7dc1c8))

    <details><summary>why</summary>

    The first net this bot emitted was not runnable from a clean checkout. The campaign gitignored harness.py — defensible-looking, since it is a copy of a bundle file — so the committed oracle was references plus a runner shelling out to a script that does not travel. Checked out fresh, verify-oracle.sh exits 2 with 'No such file'.

    </details>
* **golden-master:** resolve the base URL instead of baking it ([b3b6804](https://github.com/SocialGouv/iterion/commit/b3b680449f8390576d3fdbac97ca8f3824526f11))

    <details><summary>why</summary>

    The emitted net recorded base_url as a literal. Ports are derived from the repository path — the fix for two checkouts fighting over one port, where the bad case is not a refused start but the second copy capturing the first one's application and recording a net that describes a different tree. A literal therefore pins the net to the machine AND the path that recorded it.

    </details>
* **golden-master:** the emitted runner exits red when the gate is red ([f77b3f0](https://github.com/SocialGouv/iterion/commit/f77b3f01b134f66f53e61bd1921c6507256991cb))

    <details><summary>why</summary>

    verify-oracle.sh printed the report and exited 0 no matter what the verdict was. The graph never noticed, because it reads the JSON and computes the conjunction itself — but this script is the entry point for CI and for humans, and both read exit codes. A runner that reports a red gate and exits 0 IS a blind judge, one level above the one this bot was built to catch.

    </details>
* **golden-master:** the negative control covers the whole corpus ([e773e6c](https://github.com/SocialGouv/iterion/commit/e773e6c581e064df91f00183c34457457cc63466))

    <details><summary>why</summary>

    It sampled the first six entries, which left every later entry never once confronted with its own reference. A reference could be stale, or frozen against a world that had since moved, and nothing would say so unless a mutant happened to target it. That is a hole in the one guard whose job is to prove the comparators are not noisy.

    </details>
* **model:** resolve bare model names deterministically, by consensus ([c2d00a3](https://github.com/SocialGouv/iterion/commit/c2d00a30411a40ec432cf1d8974ed961743c8f60))

    <details><summary>why</summary>

    The bare-name index was built by assigning into a map while ranging over one. Go randomises map iteration, so a model published by several providers resolved to a DIFFERENT provider's numbers on every process start. Five consecutive runs of the same command produced five different prices for glm-5.2, one of them zero.

    </details>

## [3.8.1](https://github.com/SocialGouv/iterion/compare/v3.8.0...v3.8.1) (2026-07-28)

### Bug Fixes

* **server:** a malformed comment no longer costs the review and the merge gate ([#305](https://github.com/SocialGouv/iterion/issues/305)) ([189caa6](https://github.com/SocialGouv/iterion/commit/189caa653fc0f757104ed490e64365e489f42768)), references [#304](https://github.com/SocialGouv/iterion/issues/304) [#304](https://github.com/SocialGouv/iterion/issues/304)

    <details><summary>why</summary>

    Two couplings, both hit live on PR #304.

    </details>

## [3.8.0](https://github.com/SocialGouv/iterion/compare/v3.7.5...v3.8.0) (2026-07-28)

### Features

* **bots:** golden-master (Goldy) — behavioural non-regression net that proves it can see ([e404343](https://github.com/SocialGouv/iterion/commit/e4043438b297669243a4cf03d747160492bad3a8))

    <details><summary>why</summary>

    Records what an existing app observably does, then PROVES the references are not blind with a deterministic mutation counter-test: injected divergences must all be seen, a no-op mutation must leave the oracle silent.

    </details>
* **golden-master:** binary lane — PDF/spreadsheet capture and the blind-judge diagnostic ([238f8ff](https://github.com/SocialGouv/iterion/commit/238f8ffab839dab7c2645f6eb4f62b8cbfaf36bd))

    <details><summary>why</summary>

    Ajoute poppler (pdftotext, pdftoppm) au bundle et un skill binary-lane cable sur la campagne. Les deux archetypes binaires (content_empty, value_change) etaient deja exiges par le harnais ; il manquait l'outillage et le guide.

    </details>
* **golden-master:** selfcheck mode, mode-aware reports, and a mechanical seal ([53a9c18](https://github.com/SocialGouv/iterion/commit/53a9c186de006aa741af155577fc094892a66fc8))

    <details><summary>why</summary>

    Trois faiblesses residuelles du run 005.

    </details>
* **model:** show the rejected payload when a tool call fails ([addfc99](https://github.com/SocialGouv/iterion/commit/addfc990e17b93476ced4c5163836b9b5951595a))

    <details><summary>why</summary>

    Une erreur d'outil qui nomme une propriete manquante n'est pas exploitable sans la charge qui l'a omise. Aujourd'hui la ligne de log dit seulement

    </details>

### Bug Fixes

* **golden-master:** derive the seal path so campaign and gate agree on it ([065fe48](https://github.com/SocialGouv/iterion/commit/065fe48f76200e3354092684f3a04219689efc92))

    <details><summary>why</summary>

    The previous fix scoped the seal to the run by forcing GM_SEALED_DIR at the gate. The campaign seals too — the golden-master skill has it run selfcheck — in another process, without that environment. It therefore fell back to the shared gm-holdout path and MOVED the held-out set there, after which the gate looked in the run-scoped path, found nothing, and would have bailed on a seal it had itself broken.

    </details>
* **golden-master:** emit_runner crashed on shell brace expansion ([997a55e](https://github.com/SocialGouv/iterion/commit/997a55e05a4cf47c80b39520863204439fb28e98))

    <details><summary>why</summary>

    Le DSL expanse les expressions d'environnement, y compris la forme ${VAR:-defaut}, AVANT d'executer le script. La clause de defaut court jusqu'a la premiere accolade fermante -- qui etait celle de la substitution de template suivante. Une accolade mangee, script Python impossible a parser, et le run echouait sur son dernier noeud APRES que la porte ait converge.

    </details>
* **golden-master:** four defects found by the first real run ([2fd1a08](https://github.com/SocialGouv/iterion/commit/2fd1a08c325968805130c322df84e58bf4dd2ad5))

    <details><summary>why</summary>

    1. The harness forced `sh` on mutant scripts, ignoring the shebang. On most systems /bin/sh is dash, which has no `source`: a helper file never loaded, every function it defined was 'not found', and the mutant died with a bare exit 127 giving no hint the interpreter had been swapped. Scripts now run honouring their shebang, with an `sh` fallback when not executable.

    </details>
* **golden-master:** per-run seal, and a campaign schema of one field ([efb46be](https://github.com/SocialGouv/iterion/commit/efb46beccb00b8e278ea6ed6bb21103093df774e))

    <details><summary>why</summary>

    Deux defauts trouves en surveillant le run 006, tous deux de conception.

    </details>
* **golden-master:** the emitted runner advertised a flag it did not implement ([6ca64c2](https://github.com/SocialGouv/iterion/commit/6ca64c2de70deb70693d9c9fa41743a37e493a14))

    <details><summary>why</summary>

    L'en-tete annoncait un --self-check absent du script, et laissait croire que le mode par defaut ne rejouait pas le contre-test -- alors qu'il fait exactement cela. Un runner qui ment sur ce qu'il fait est un runner que personne ne relit.

    </details>
* **runtime:** stop warning that bundle skills are absent from the skill library ([086d1cf](https://github.com/SocialGouv/iterion/commit/086d1cf39bf7f03edb433feb4c93e8a01f2a6f63))

    <details><summary>why</summary>

    Le miroir de bibliotheque tourne APRES ceux du bundle et des plugins, qui le supplantent (ADR-059). Quand un bundle fournit ses propres skills, la reference est deja satisfaite -- mais on avertissait quand meme, une ligne par skill a chaque demarrage. C'est vrai, inutile, et ca se lit comme un run casse : le bundle golden-master en emettait six a chaque lancement.

    </details>
* wait out a provider quota window instead of burning 8 pods against it ([#304](https://github.com/SocialGouv/iterion/issues/304)) ([41d3330](https://github.com/SocialGouv/iterion/commit/41d33302ec9c8815ae602ea5658383b04642f259))

    <details><summary>why</summary>

    A terminal node failure was rebuilt as a plain string, so both the classified error code and the original error were destroyed at the point the engine gave up on the node. Two consequences, neither visible from either side alone:

    </details>

## [3.7.5](https://github.com/SocialGouv/iterion/compare/v3.7.4...v3.7.5) (2026-07-27)

### Bug Fixes

* **review-pr:** recover findings when the merge step degrades + honest gate note ([#302](https://github.com/SocialGouv/iterion/issues/302)) ([cd2ffeb](https://github.com/SocialGouv/iterion/commit/cd2ffeb858cabd6253a45d23ff080bd7a69de895)), references [#300](https://github.com/SocialGouv/iterion/issues/300)

    <details><summary>why</summary>

    Second occurrence, live on PR #300 (run 019fa02b): converge returned `findings` as the prose "See structured findings array." while total_findings said 8. The publish step parsed nothing, so the review published "0 findings kept" with 0 inline comments — 8 real findings never reached the author — and the fail-closed gate reported "1 blocking finding(s) >=high", sending the operator hunting for a finding that was never published. The 0.5.4 prompt hardening was necessary but not sufficient: an…

    </details>

## [3.7.4](https://github.com/SocialGouv/iterion/compare/v3.7.3...v3.7.4) (2026-07-26)

## [3.7.3](https://github.com/SocialGouv/iterion/compare/v3.7.2...v3.7.3) (2026-07-25)

## [3.7.2](https://github.com/SocialGouv/iterion/compare/v3.7.1...v3.7.2) (2026-07-25)

### Bug Fixes

* **plugin:** repair two codeindex rewriter defects found on review ([#297](https://github.com/SocialGouv/iterion/issues/297)) ([472f7d0](https://github.com/SocialGouv/iterion/commit/472f7d02f634c1136f4683d41a1097191787c369))

    <details><summary>why</summary>

    Both would have shipped broken, and neither is caught by manifest validation.

    </details>

## [3.7.1](https://github.com/SocialGouv/iterion/compare/v3.7.0...v3.7.1) (2026-07-25)

### Bug Fixes

* **review-pr:** harden emit `findings` contract (JSON array, not prose) ([#299](https://github.com/SocialGouv/iterion/issues/299)) ([6496bb4](https://github.com/SocialGouv/iterion/commit/6496bb427c080da9e7e869fca656d23eee7959f0)), references [#292](https://github.com/SocialGouv/iterion/issues/292)

    <details><summary>why</summary>

    Found live on the merge gate's own PR #292 (run 019f98ed): revi/review posted `failure | 1 blocking finding` while the review body said "0 findings kept". Root cause: the converge/emit LLM returned `findings` as a PROSE STRING ("4 findings kept (0 critical, 1 high, 2 medium, 1 low)…") instead of the JSON array of objects the schema intends. `findings: json` accepts a string (the DSL has no object-array type), so nothing rejected it. Downstream, publish_review's `JSON.parse(FINDINGS)` fails →…

    </details>

## [3.7.0](https://github.com/SocialGouv/iterion/compare/v3.6.1...v3.7.0) (2026-07-25)

### Features

* **forge:** iterion remote forge refresh — re-sync a connection's grants now ([#298](https://github.com/SocialGouv/iterion/issues/298)) ([75c02e9](https://github.com/SocialGouv/iterion/commit/75c02e91e3a0f48d62bd3da582ca2987b61435a8))

    <details><summary>why</summary>

    Operability tool motivated by the merge-gate rollout: after changing a GitHub App's permissions (e.g. granting Commit statuses: write), an operator had to wait for the periodic refresh worker or restart the whole server for iterion to pick up the new grant. This adds a targeted, explicit refresh.

    </details>

## [3.6.1](https://github.com/SocialGouv/iterion/compare/v3.6.0...v3.6.1) (2026-07-25)

### Bug Fixes

* **forge:** request statuses:write in the runtime App token (unblocks merge gate) ([#295](https://github.com/SocialGouv/iterion/issues/295)) ([0a3f9ed](https://github.com/SocialGouv/iterion/commit/0a3f9ed2afb5af3b8cbc5773de915f799655713c))

    <details><summary>why</summary>

    Live prod e2e of the merge gate proved the full chain works end-to-end (Revi reviews → bot sends the deterministic gate verdict → server resolves the head SHA → SetCommitStatus), but the status did not land: the GitHub App lacks "Commit statuses: write", so SetCommitStatus returns 403 "insufficient scope". The code handles it exactly as designed — non-fatal, reported in gate_error, logged (`forge gate: … not posted: … insufficient scope`) — so the gate advises instead of blocking until the…

    </details>

## [3.6.0](https://github.com/SocialGouv/iterion/compare/v3.5.2...v3.6.0) (2026-07-25)

### Features

* **plugin:** ship codeindex as a builtin plugin ([#296](https://github.com/SocialGouv/iterion/issues/296)) ([193a33a](https://github.com/SocialGouv/iterion/commit/193a33a33f4fc6f331a86cecc771b7417f1ea9d6))

    <details><summary>why</summary>

    codeindex (https://github.com/maxgfr/codeindex) is a deterministic, zero-dependency repo-indexing engine on npm. This wires it in as a disabled-by- default builtin, alongside the other knowledge-graph explorers.

    </details>

## [3.5.2](https://github.com/SocialGouv/iterion/compare/v3.5.1...v3.5.2) (2026-07-25)

### Bug Fixes

* **review-pr:** second publish_review shell bug — bare double-quote truncates ([#294](https://github.com/SocialGouv/iterion/issues/294)) ([c06e0e1](https://github.com/SocialGouv/iterion/commit/c06e0e1c57439a57e2af7f593fa1b60300ae37d7)), references [#293](https://github.com/SocialGouv/iterion/issues/293) [#293](https://github.com/SocialGouv/iterion/issues/293) [#292](https://github.com/SocialGouv/iterion/issues/292)

    <details><summary>why</summary>

    The e2e re-run (after #293) still failed: publish_review produced EMPTY output (exit 0, no forge review, no revi/review status), then publish_health crashed on the empty inputs. Root cause: a python COMMENT in the publish_review body contained bare double-quotes ("high"/"blocker"/"major"). The body is wrapped by the shell in `python3 -c "…"`, so a bare double-quote ends the string and silently truncates the script. Same trap as backticks; distinct from the #293 questions-array bug (both were…

    </details>

## [3.5.1](https://github.com/SocialGouv/iterion/compare/v3.5.0...v3.5.1) (2026-07-25)

### Bug Fixes

* **review-pr:** publish_review shell exit-127 on multi-question reviews ([#293](https://github.com/SocialGouv/iterion/issues/293)) ([34df1c1](https://github.com/SocialGouv/iterion/commit/34df1c1230124341f1a054682cf3ec160c10d653)), references [#292](https://github.com/SocialGouv/iterion/issues/292) [#292](https://github.com/SocialGouv/iterion/issues/292)

    <details><summary>why</summary>

    The v0.5.0 questions channel passed a JSON array of strings through the publish_review tool node as QUESTIONS={{input.questions}}. A `json`-typed field holding an all-string array decodes to []string, which the tool-command substitution SPACE-JOINS instead of JSON-encoding (known engine bug, executor_tool.go:1047) — so the 2nd+ question landed in shell command position and bash exit-127'd, crashing the entire review (no forge review, no revi/review gate status). findings escaped this because…

    </details>

## [3.5.0](https://github.com/SocialGouv/iterion/compare/v3.4.0...v3.5.0) (2026-07-24)

### Features

* **docs-refresh:** /doki is a direct PR-scoped command (amend-on-PR activation) ([5d2e481](https://github.com/SocialGouv/iterion/commit/5d2e481e7e72ddec6654fa33e6ac0c9d4ce60ecc))

    <details><summary>why</summary>

    The /doki comment command switches from board/any to direct/pr, mirroring /revi: a developer commenting /doki on a PR launches docs-refresh directly on the PR head (no tracking card), self-switching to incremental amend via the generic pr_url/base_ref/source_branch the webhook stamps. The manifest half of activating the amend-on-PR trigger — the /command→bot route derives from this invocation, no engine code. v3.5.4.

    </details>

## [3.4.0](https://github.com/SocialGouv/iterion/compare/v3.3.0...v3.4.0) (2026-07-24)

### Features

* **review-pr:** falsifiable questions channel + deterministic Revi merge gate ([#291](https://github.com/SocialGouv/iterion/issues/291)) ([ad8e2d6](https://github.com/SocialGouv/iterion/commit/ad8e2d6eabd545ecc8394dc0aa9421eb02c67e5e)), references [#285](https://github.com/SocialGouv/iterion/issues/285) [#290](https://github.com/SocialGouv/iterion/issues/290)

    <details><summary>why</summary>

    Two improvements to Revi, motivated by a "0 findings / no comment" review (PR #285) that gave no signal of depth, and by the wish to let Revi arbitrate a merge without an LLM being the yes/no gate.

    </details>

## [3.3.0](https://github.com/SocialGouv/iterion/compare/v3.2.0...v3.3.0) (2026-07-24)

### Features

* **docs-refresh:** agnostic amend-on-PR (v3.5.2) + engine stays bot-agnostic ([d184a1f](https://github.com/SocialGouv/iterion/commit/d184a1f939614510279a619cbb082327ff03437a))

    <details><summary>why</summary>

    Doki self-aligns a PR's docs and amends it (pushes onto the PR head + comments) when launched ON a pull request — keyed entirely on the GENERIC PR-context the engine already provides for ANY bot, not on bot-specific engine code:

    </details>

### Bug Fixes

* **docs-refresh:** scope_check bases on run-start HEAD, not oldest reflog ([40ae433](https://github.com/SocialGouv/iterion/commit/40ae43322b6e1d9fa47529c640937895bc2e00f9))

    <details><summary>why</summary>

    In amend-on-PR mode the cloud runner clones the base branch (HEAD=main) then checks out the PR head, so the OLDEST reflog entry is main. scope_check diffed against it and folded the PR author's OWN code into the changed set, raising a phantom writeable-set violation that pinned scope_ok=false — so `converged` never fired and every amend run burned all its passes (live run 019f9429). Base the diff instead on the run-start HEAD: the newest reflog entry that is not one of this run's own `Bot:…

    </details>
* **studio:** "Open child bot" 404 → resolve child path against the right parent ([#285](https://github.com/SocialGouv/iterion/issues/285)) ([c7f6aab](https://github.com/SocialGouv/iterion/commit/c7f6aab39c3f7fa88636921b76825a6e783a6f52))

    <details><summary>why</summary>

    Clicking "Open child bot" from the editor inspector could 404 (and, in a follow-up, silently open nothing): the child .bot path is resolved from the parent file with `resolveSubbotSource(parentFilePath, source)`, but `parentFilePath` (document store `currentFilePath`) can be null during the short route-hydration window — arriving from Pipelines activates the editor tab one render before EditorTabHost copies the file into the document store. With a null parent, a parent-relative `source` was…

    </details>

## [3.2.0](https://github.com/SocialGouv/iterion/compare/v3.1.3...v3.2.0) (2026-07-24)

### Features

* **docs-refresh:** v3.4 — drop noop cache, author_docs, mark_issue (native paradigm) ([1d2c482](https://github.com/SocialGouv/iterion/commit/1d2c482fcb3eda2844f2659c52917f32cd52779f))

    <details><summary>why</summary>

    Three more non-essential nodes removed, converging Doki on the native shape (one adaptive agent + a truth gate + the PR tail):

    </details>
* **docs-refresh:** v3.5 — incremental (git-detected base) + amend-PR modes ([1a5eddf](https://github.com/SocialGouv/iterion/commit/1a5eddf294770346ee2dd5fc81a4398c274b95b6))

    <details><summary>why</summary>

    Two alignment strategies so Doki keeps docs fresh cheaply, on the native paradigm (one agent + truth gate + PR):

    </details>

### Bug Fixes

* **docs-refresh:** anchor incremental base detection to the trailer line ([bb2291e](https://github.com/SocialGouv/iterion/commit/bb2291edd377ed5910beb3858866e42867581f55)), references [#288](https://github.com/SocialGouv/iterion/issues/288)

    <details><summary>why</summary>

    git log --grep 'Bot: docs-refresh' matched any commit MENTIONING the trailer in prose — including this bot's own v3.5 feature commit (caught on the first live check against origin/main, where it picked 1a5eddf29 over the real last alignment #288). Anchor to a line start (-E --grep '^Bot: docs-refresh') so only actual trailer lines count. Test now commits a prose-mention after the alignment commit and asserts the base stays the real alignment commit.

    </details>
* **studio,desktop:** point Documentation links to the Pages site, not the repo docs folder ([474eecd](https://github.com/SocialGouv/iterion/commit/474eecd73b09ff9051b44084c0fed59abf7e2521))

    <details><summary>why</summary>

    The cloud landing, the About and Backends settings tabs, the desktop menu, and the desktop app-info binding all linked to github.com/.../tree/main/docs (raw repo folder) instead of the published docs site. Point them at https://socialgouv.github.io/iterion/ (and /backends for the backends deep link).

    </details>

## [3.1.3](https://github.com/SocialGouv/iterion/compare/v3.1.2...v3.1.3) (2026-07-24)

## [3.1.2](https://github.com/SocialGouv/iterion/compare/v3.1.1...v3.1.2) (2026-07-23)

## [3.1.1](https://github.com/SocialGouv/iterion/compare/v3.1.0...v3.1.1) (2026-07-23)

### Bug Fixes

* **feed-watch:** make the SSRF guard proxy-aware for sandboxed runs ([#287](https://github.com/SocialGouv/iterion/issues/287)) ([bc6c16d](https://github.com/SocialGouv/iterion/commit/bc6c16d0c13073b9a60df48e0e0aaaf7f3360b1b))

    <details><summary>why</summary>

    A cloud/sandboxed run reaches the internet through iterion's egress proxy, injected as HTTPS_PROXY and advertised at the runner's own (necessarily private) pod IP — the trusted egress boundary and the secret-redaction point (started even in `network: open` whenever a SecretRewriter is present). urllib then dials the PROXY, not the feed host, so Vigie's socket-level getaddrinfo guard rejected our own proxy as "SSRF-unsafe address <pod-ip>" and every feed failed (run 019f8feb: all 69 fetches…

    </details>

## [3.1.0](https://github.com/SocialGouv/iterion/compare/v3.0.0...v3.1.0) (2026-07-23)

### Features

* **studio:** surface a run's PR/deploy links as headline result-links ([#286](https://github.com/SocialGouv/iterion/issues/286)) ([421f901](https://github.com/SocialGouv/iterion/commit/421f90104f99d4fd0c655e2c09f32a486f82fa80))

    <details><summary>why</summary>

    A run that opens a PR (finalize_mr) or deploys an app (Appy) buried the resulting URL in a node's structured output — nowhere prominent. Surface it like a CI run's "View deployment" button, at the top of the run summary.

    </details>

## [3.0.0](https://github.com/SocialGouv/iterion/compare/v2.0.1...v3.0.0) (2026-07-23)

### ⚠ BREAKING CHANGES

* **docs-refresh:** plan-then-execute comprehensive, asymptote on honest declaration (3.1.0)

### Features

* **docs-refresh:** plan-then-execute comprehensive, asymptote on honest declaration (3.1.0) ([3b8dd24](https://github.com/SocialGouv/iterion/commit/3b8dd2486f1c820cacbddfdaf9942a8579cc60d3))

    <details><summary>why</summary>

    3.0 was fast/cheap but under-delivered — it handled the ~10 advisory hints, surveyed shallowly, and honestly-but-myopically declared docs_aligned after one ~15-min pass, producing tiny PRs on a 250-doc corpus with weeks of unread semantic drift. The fix is framing, not a coverage gate: an audited.json exhaustiveness checklist was drafted and REJECTED as exactly the excess determinism 3.0 removed (it makes the agent do bookkeeping instead of the job).

    </details>
* **docs-refresh:** self-orchestrated coverage — campaign fans out its own subagents (3.2.0) ([cd6c92b](https://github.com/SocialGouv/iterion/commit/cd6c92bd0a816e596ecbd59d23e565a16e5d44b9))

    <details><summary>why</summary>

    A live 3-way benchmark (this repo, 2026-07-23) settled why 3.0/3.1 shipped tiny PRs: a SINGLE campaign agent — exactly like a free native agent handed the same one-liner — self-scopes to the headline docs and misses the long tail. By rising coverage: Doki (~3 commits/pass, docs/ only) < native one-liner (6 fixes, missed cloud + bot READMEs) < native with a demanding prompt (reached the WHOLE corpus). The only run that got there DECOMPOSED into per-cluster sub-auditors ON ITS OWN.

    </details>
* **server,studio:** full cloud bot editing — team-authored bot store ([8850fd6](https://github.com/SocialGouv/iterion/commit/8850fd630020be5f84bceb6da074d09d90e95711))

    <details><summary>why</summary>

    Adds a writable, team-scoped bot store so the studio editor works in cloud, not only on a local filesystem. The cloud catalog stays baked read-only; tenant bots are editable and forkable.

    </details>
* **studio:** multi-file bundle editor for cloud bots ([4444448](https://github.com/SocialGouv/iterion/commit/4444448a3e6b91eedbbfce1a9091ed09881831b3))

    <details><summary>why</summary>

    Adds a "Bundle files" drawer (Toolbar, shown only for a botsource:// tenant bot) listing the bundle's files. main.bot opens in the DSL Canvas; skills/*.md, manifest.yaml and any other file edit inline in a Monaco buffer and save per-file to the bot-source store. New files can be added and non-main files removed. Reuses the FileEditDialog Monaco pattern + inferMonacoLanguage; the tab-kind system is untouched.

    </details>
* **webhooks:** PR-open auto-reviews only (Revi); Billy on /billy with Revi's review ([#283](https://github.com/SocialGouv/iterion/issues/283)) ([463646d](https://github.com/SocialGouv/iterion/commit/463646dc66235863df2a4611f76cc7d1ec35d53c))

    <details><summary>why</summary>

    Decouple the mutating branch-improve loop (Billy) from PR-open auto-launch: a PR/MR open now ONLY ever auto-reviews (Revi / review-pr). Removes the selectForgePRBot ticket-PR→Billy routing; the merge-queue auto-heal path (NeedsAutoHeal) is unchanged.

    </details>

### Bug Fixes

* **docs-refresh:** budgets sized for self-orchestration (3.2.1) ([7213fc6](https://github.com/SocialGouv/iterion/commit/7213fc6c2dc0afad2cc7a9694c3cd283b71d3058))

    <details><summary>why</summary>

    First 3.2.0 live run (019f8e08) aligned 40 docs across the whole corpus in pass 1 (~70 min / ~$16) — the win — but the old 2h/$60 caps guillotined it mid-pass-2 as a hard failed_resumable BEFORE finalize, and all 40 in-pod commits were lost (the exporter only runs on a clean finalize; engine gap tracked separately). Comprehensive self-orchestrated passes are long: max_duration 2h→6h, max_cost_usd 60→120, max_passes 8→4 so the asymptote reaches GRACEFUL exhaustion (which finalizes + exports +…

    </details>
* **native:** unique-title prefix must respect the caller's rune budget ([89787a9](https://github.com/SocialGouv/iterion/commit/89787a938024ac6adf37bc22b9b4eb1112948435)), references [#N](https://github.com/SocialGouv/iterion/issues/N) [#N](https://github.com/SocialGouv/iterion/issues/N) [#N](https://github.com/SocialGouv/iterion/issues/N)

    <details><summary>why</summary>

    The atomic CreateUniqueTitle prepended "#N - " to the desired title without re-truncating, so a pipeline-board title already compacted to 80 runes became 85 once made unique — deterministically failing TestPipelineBoardTaskCreateEnsuresUniqueTitle (the server's list-then-check fallback already re-compacted; the atomic path didn't). CreateUniqueTitle now takes an optional `normalize func(string) string` applied to every candidate inside the lock; the pipeline board passes compactPipelineTitle so…

    </details>
* **runner:** budget-exceeded acks (no auto-resume) — stop the git-meta clobber ([266e6ad](https://github.com/SocialGouv/iterion/commit/266e6ad98739b23265a33a2e2fad1cd3306abd81))

    <details><summary>why</summary>

    ErrBudgetExceeded fell through to the generic Nak, so a budget-exceeded run (a resumable checkpoint) was auto-redelivered and resumed. That was doubly destructive: the same message carries the same already-spent budget, so a duration cap re-fails instantly in a pod-provisioning loop; and each redelivery re-provisions a FRESH pod whose recordRunGitMeta overwrites the first attempt's good git metadata with base==head — silently destroying the run's exported commits. Live: run 019f8e08 (Doki 3.2)…

    </details>
* **server:** tenant bot gallery slug — botregistry Path is the bundle dir ([6e679a1](https://github.com/SocialGouv/iterion/commit/6e679a164629f64eac6ecc2c5188307823dc12f2))

    <details><summary>why</summary>

    tenantBotEntries re-keys a discovered tenant bundle to its store slug, since a forked bot's manifest name (e.g. "docs-refresh") differs from its slug and would otherwise collide with the catalog bot of that name and never surface under its own id. slugFromMaterializedPath wrongly required ≥2 path segments, but botregistry sets Entry.Path to the bundle DIRECTORY ("<root>/<slug>"), a single segment — so the slug was never applied and the tenant bot vanished from the gallery / 404'd on GET.

    </details>
* **studio:** reach editor home with tabs open + in-editor "Duplicate & edit" ([d877725](https://github.com/SocialGouv/iterion/commit/d8777255bc80db5eefa61e64c2735e359f0243a5))

    <details><summary>why</summary>

    Two cloud editor UX gaps:

    </details>
* **studio:** read-only editor for catalog bots in cloud (no more 500 on Save) ([05102a0](https://github.com/SocialGouv/iterion/commit/05102a0c7fcc15bd19921441e07b1bd89a48c51d))

    <details><summary>why</summary>

    Opening a baked catalog bot in the cloud editor bound a filesystem path (/opt/iterion/bots/<bot>/main.bot); Save then hit /api/files/save and 500'd with "permission denied" (the image is read-only, and cloud has no writable workspace). Only a team-authored bot (botsource:// path) is writable in cloud.

    </details>
* **studio:** replace window.confirm/prompt with accessible dialogs ([5511004](https://github.com/SocialGouv/iterion/commit/551100473e5616234a96770091d236da81c45c41))

    <details><summary>why</summary>

    The a11y source-discipline test bans window.confirm/alert (design-system Don'ts); the bundle-files delete used window.confirm and the new-file / fork-slug flows used window.prompt — reddening main's Tests check on the previous commit. Delete now goes through useConfirm(); the text prompts through a new promise-based usePromptText() hook (styled Dialog + Input, validation, Enter-to-submit), mirroring useConfirm's shape.

    </details>
