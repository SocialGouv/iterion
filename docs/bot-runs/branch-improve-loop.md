# Billy — branch-improvement validation

## 2026-08-01 (fin de journée) — the zero-touch lane, and the crash it took to prove it (run 019fbd98)

- Status: **validated after a crash-level fix.** The opt-in lane had never fired once since it was written; its first real firing panicked a server pod.
- Method: `auto_fix_on_gate_failure` enabled on the e2e integration, a concurrency defect planted by hand (`Snapshot` handing the internal map out past the lock, `RecordHit` mutating it without one), pushed as a human. **No `/billy` comment at any point.**

### First attempt: the lane crashed the server

The gate went red at 12:51:30 and no fixer launched. The reason was in the pod's
pre-restart log:

```
panic: store/mongo: tenant-scoped query without tenant in ctx
  ← cloudpublisher.SubmitLaunch ← runview.Launch ← launchWebhookTarget
  ← autofixForRun ← eventbus NATSBus subscriber
```

The lane stamped the **auth** identity the admission gate reads and not the
**store** identity every tenant-scoped query asserts on. The inbound-webhook
middleware stamps both; the retry sweeper stamps both; this lane had copied half
the precedent. Missing, the launch does not fail — it trips the tenancy guard
deep inside `SaveRun` and takes the process down.

Its own tests could not see it: they stub `webhookLaunchBot`, which is the seam
directly **above** where the launch reaches the store. Fixed in `b68f8a39f`,
with the positive control now asserting on the context handed across that seam.

A second, wider fix rode along: the bus fans one event out to independent
consumers that share nothing but the dispatch, so a panic there is the widest
blast radius for the narrowest bug. It is now recovered per delivery, logged
with its stack, and dropped exactly as a returned error already is.

### Second attempt: it works, untouched

```
gate auto-fix: iterion/review red on SocialGouv/iterion-test-appy-e2e#2@de9df24
               → launched branch-improve-loop (run 019fbd98)
```

`restarts=0` on all three replicas. The launched run carried everything needed
to close the loop back: `head_sha` exactly the head the gate was red on,
`gate_context: iterion/review` (the repo's pin, not the bot's default), the
publish grant, 7141 characters of prior review, and a `scope_notes` saying why
it was launched.

### Lessons for the next run

- An opt-in feature that has never fired is not shipped, it is written. This one
  passed a `/simplify`, an adversarial review and a refusal table, and still had
  a process-killing bug on line one of its only untested path.
- When a lane runs off the event bus rather than an HTTP request, it inherits
  none of the request's context. Copy the middleware's stamping in full, or copy
  the retry sweeper, which already had it right.


## 2026-08-01 (soir) — the loop closes: red gate → fix → push → independent supersede (run 019fbd1b)

- Status: **validated** — the four links of the Revi↔Billy loop exercised live, two of them for the first time ever.
- Versions: bot 1.1.0 · iterion `a9f32534b` (v3.19.0, carrying `db2676c5b`).
- Method: a real defect planted by hand on `feat/cache-layer` (a `Purge` ranging over the entry map and a `Len` reading it, both without the mutex, on a type documented "safe for concurrent use"), pushed as a human. Revi reviewed it, the gate went red, `/billy`.
- Result: Revi found 4 findings (1 critical, 1 high, 2 medium) and the gate went `failure — 2 blocking finding(s) ≥high`. Billy fixed all four, pushed, and posted `iterion/review=success on a3a734324f33`. The independent re-review landed 14 minutes later and superseded it with `success — no blocking findings (≥high); 2 total`.

### The measurement that mattered

The same actor, the same event, the same repo — before and after `db2676c5b`:

| heure (UTC) | `synchronize` pushed by | delivery | sha |
|---|---|---|---|
| 10:29:33 | the fixer, before the fix | **filtered** | `6dd691c1b` |
| 11:28:28 | a human | launched | `e8ca8c0be` |
| 11:56:49 | the fixer, after the fix | **launched** | `a3a734324` |

And the two verdicts that then sat on that one head, in order:

| heure | `iterion/review` on `a3a73432` | by |
|---|---|---|
| 11:56:50 | `success — 4 finding(s) fixed, re-review by the fixer clean, build green` | the fixer, about its own code |
| 12:10:57 | `success — no blocking findings (≥high); 2 total` | the reviewer, independent |

### The return ledger, exercised for the first time

`prior_pushback` reached the re-review at 1920 characters, finding by finding with the commit that fixed each. The reviewer re-raised **none** of the four and instead found two new low ones — plus a question worth more than the findings: *does CI run `go test -race`? I could not here, and I confirmed by mutation that removing Len's mutex leaves the whole suite green.* A locking discipline with no automated guard, surfaced by the reviewer's own falsifiability channel.

### Lessons for the next run

- A guard keyed on the sender treats "our bot pushed" and "our bot opened this PR" as the same thing. They are opposites: the second converged in its own loop, the first is exactly the moment an independent judgement is needed.
- Plant a defect and drive the whole loop rather than asserting each link separately. Both defects fixed today were compositions, not units.


## 2026-08-01 — the board lane could not publish, and the fixer's push was invisible to the gate (run 019fbcbb)

- Status: **validated** — first end-to-end run where Billy answers a review, pushes, posts its verdict AND closes the merge gate. Two engine defects found live, both fixed and deployed in-session.
- Versions: bot 1.1.0 · iterion `cdfedc124` (server), the run itself on `claude_code` / `claude-opus-5`.
- Method: `/billy` comment on [iterion-test-appy-e2e#2](https://github.com/SocialGouv/iterion-test-appy-e2e/pull/2) (`feat/cache-layer`, a real Go module with planted defects), cloud, board lane, GitHub App connection, `gate_context: iterion/review` pinned on the integration.
- Result: converged in **one pass** — 38m12s active, 390 events, 152 tool calls, 11 nodes, `continuation_loop` never re-entered. Pushed 6 commits onto the PR branch; `publish_verdict` returned `verdict posted; iterion/review=success on 6dd691c1be95`.
- Value: the hand-off paid for itself. Billy received 3388 characters of prior review carrying the stable id `Rcae144`, **reproduced the finding before fixing it** (a 503 origin was memoized and served with a nil error, and a recovered origin was never re-contacted), fixed it in `57c7a00`, then found four more real issues the review had not — among them a `Warm` semaphore allocated per call instead of per cache, so the fan-out cap was not a cap.

### The two defects the run surfaced

**1. A bot launched off the board could not publish anything** (fixed, `515714482` + `cdfedc124`).
The cloud coordinator launches a card from its BotArgs alone, so the forge-publish grant and the repo's launch policy — both composed inline by the webhook tail — never reached it. Measured side by side with the reviewer on the same PR:

| | `forge_publish_url` | `forge_publish_token` | `gate_context` |
|---|---|---|---|
| Revi (`mode: direct`) | oui | oui | `iterion/review` |
| Billy (`mode: board`), avant | — | — | absent |
| Billy (`mode: board`), après | oui | oui (64 car.) | `iterion/review` |

Both are now resolved at claim time, never carried on the card: a grant expires, so a card claimed hours later would hold a dead token, and a board document is the wrong place for a credential at rest.

A second defect rode along, found while preparing the fixture: the repo carried **two integrations** (a stale personal-token one from 2026-07-17 and the GitHub App it was re-provisioned onto), two live webhooks, and a double delivery on every comment. Resolving them by lowest id — deterministic but arbitrary — elected the stale one, which would have posted the verdict under the operator's own account, the identity the loop guard refuses. The latest provisioning now wins.

**2. The fixer's push was the one delivery the gate never saw** (fixed, `db2676c5b`).
The iterion-bot guard skips a PR our own loop produced, keyed on the SENDER. On a merge-gate resync the sender is by construction the forge bot — the fixer that just pushed — so the resync on `6dd691c1` was recorded `filtered — PR authored by iterion's forge bot`. Two consequences, both the failures the merge gate exists to remove: the required check stayed on the pre-push revision (absent on the head that needs judging), and the fixer's own verdict about code it wrote was never superseded by the independent re-review [docs/merge-gate.md](../merge-gate.md) promises. A PR the bot *opens* is still skipped — that one did converge in its own loop.

### Lessons for the next run

- A `mode: board` bot gets none of the launch context the webhook lane composes. When a board-mode bot behaves as if a var were unset, read the RUN's inputs before reading the bot.
- Both defects were invisible to every test because each test supplied by hand the thing production had to produce. The same shape as the `publish:` defect in [review-pr.md](review-pr.md): when a contract crosses two components, at least one test must run the producer for real.
- Deprovision a stale integration when re-provisioning a repo onto another connection. Two live webhooks on one repo double every delivery, and the extra one refuses everything with a misleading reason.


## 2026-07-10 — /billy command on a Dependabot PR: 4 engine gaps peeled live, then a clean push-back under the App identity (runs 019f4bd4 / 019f4c46 / 019f4c86 / 019f4ccb)

- Status: **validated (cloud E2E, command path) — 5 engine fixes found live, all landed in-session.**
  Mission: make Dependabot PR [#80](https://github.com/SocialGouv/iterion/pull/80)
  mergeable (bump `x/crypto` to 0.52.0 past a CRITICAL advisory cluster Vetty
  flagged + fix the failing CI OpenAPI drift). Final run 019f4ccb: 22 min,
  `push_back: true`, **2 commits pushed onto the PR** as
  `iterion-forge-83fde406[bot]` (`52a4072d9` crypto bump + tidy/vendor,
  `d766e1d03` openapi regen) + verdict comment posted under the App.
- Versions: bot v2 · iterion `499957c31`→`6dd452c2a` · chart 0.37.2→0.37.4.
- Method: `/billy <mission>` PR comment (args → `scope_notes`) → board-mode
  card → dispatcher → cloud runner. ~$4/run. The mission text included the
  memory guidance (skip the vite build, `go test -p 2`) after an OOM.
- The onion, one layer per run (each failure = a real engine fix):
  1. run 019f4bd4: campaign did PERFECT work (crypto bump + openapi regen,
     verified) but `mr_gate.push_back: false` → commits stranded on the
     runner's ephemeral storage branch. Fix `642b1ba0d`: the command path now
     stamps `push_branch`/`open_mr` (parity with the pull_request-event path).
  2. Same run also exposed that `ensureBoardCard` rebuilt BotArgs from scratch
     — cloud launches use card BotArgs ONLY, so `pr_url`/branch vars never
     reached the run (also why Billy never commented). Fix `bc2918024`.
  3. run 019f4c46: **OOMKilled at 4Gi** during the verify suite (exit 137, pod
     restart, banked commits lost with the pod FS). Infra fix: runner limit
     8Gi (infra-apps `44b7fb4`). JetStream redelivered the run — recovery
     worked — but the re-attempt then pushed with a DEAD App token.
  4. runs 019f4c46/019f4c86: `Invalid username or token` on push + gh 401 on
     comment — the sealed bundle snapshots the 1h installation token at
     LAUNCH; long/redelivered runs outlive it. Fix `6dd452c2a` (#99): the
     publisher records generic-secret store IDs on the bundle and the runner
     re-reads them every 5 min, atomically rewriting
     `/run/iterion/secrets/<name>`; tools `cat` the file per use.
- Engine hardening (this session, all deployed): `499957c31` PR head/base
  resolution for PR-surface commands · `642b1ba0d` push-back vars ·
  `bc2918024` board-card PR context · `6031a357e` executor agent-stream lines
  now persist to the run log (the studio per-node Logs tab was empty on every
  cloud run) · `6dd452c2a` mid-run file-secret refresh (#99).
- Lessons for next run: a dep-bump branch is typically BEHIND main — the CI
  drift gate runs on the merge-ref, so regen-style fixes must happen on an
  updated branch (`gh pr update-branch` first, or Billy should update it);
  keep verify memory-aware on 8Gi pods (skip frontend builds, cap `-p`).

## 2026-07-09 — first CLOUD runs: PR-webhook → Billy on the devbox runner (runs 019f43a3 / 019f43c7 / 019f4551)

- Status: **validated (cloud E2E) — 3 engine/bot gaps found live, all fixed in-session.**
  The full production trigger chain ran for the first time: real GitHub PR
  ([#84](https://github.com/SocialGouv/iterion/pull/84), `Fixes #83`) → forge
  webhook (`selectForgePRBot` routes Billy, not Revi) → NATS queue → devbox
  runner pod (uid 1000) → campaign/verify/gate.
- Versions: bot @ `1d076a618` (adds the push-back tail) · iterion `:edge`
  36fb786b5→56a5680f1 · runner image `iterion-runner-devbox:edge` ·
  webhook `d291059c` (bots review-pr + branch-improve-loop + feature-dev,
  block_fork_prs, author allowlist Viczei+devthejo).
- Method: PR-open/reopen events on SocialGouv/iterion#84 (a real fix: the
  vv0.32.0/vmain version-injection bug). Re-triggers need a NEW head sha +
  close/reopen — the delivery idem key is (PR#, head sha) and launch-success
  rows are terminal; `/billy` comments from the connection identity are
  loop-guard-filtered (self).
- Result per run:
  - `019f43a3` **failed in 17s**: Billy's inline `sandbox:` block → kubernetes
    driver → `ITERION_POD_IP env var is empty` (the chart deliberately doesn't
    provision sibling-pod sandboxing). → **Fix 1** `36fb786b5`:
    `ITERION_SANDBOX_OVERRIDE=none` (CLI-strength, beats the workflow block;
    chart auto-sets it when `runner.sandbox.enabled=false`) — the runner pod IS
    the isolation boundary.
  - `019f43c7` **finished, 1-pass converged (~7 min)**: verify.sh settled on
    `devbox run -- go build ./...` + targeted tests — the repo's devbox-pinned
    Go toolchain, i.e. the devbox-first-class runner goal proven live. campaign
    caught a REAL defect (the PR's new test file wasn't gofmt-clean — CI lint
    would have rejected it) and committed the fix (`ef540115`)… which **died
    with the pod-ephemeral worktree**: `mr_gate` open_mr=false went straight to
    done. → **Fix 2** `1d076a618`: deterministic push-back — webhook passes
    `push_branch` (PR source branch); `mr_gate → push_auth_probe →
    push_back_tool` (no-LLM python3 push, rev-list oracle so a
    converged-no-commits pass no-ops, token redacted from failures).
  - `019f4551` **finished; new path routed** (`…gate>mr_gate>push_auth_probe>done`)
    but the probe found NO credential: `materializeFileSecretsNoSandbox` gated
    on the STATIC `wf.Sandbox != nil` — under the override the run executes
    in-pod and nobody materialized `forge_token` (the launch HAD sealed it into
    the bundle: the stored secret's last_used_at == launch instant). → **Fix 3**
    `56a5680f1`: gate on the RESOLVED decision via new
    `runtime.WorkflowSandboxActive(wf, override, default)`.
- Value: the run 019f43c7 catch (gofmt) was real and manually reapplied as
  `a2cf464a9`; the three fixes harden the entire cloud-runner class of bots
  (any bot with a sandbox block + file secrets), not just Billy.
- Findings / misses: secret-resolution failures are SILENT at several layers
  (`buildGenericResolution` ok=false without a log; publisher skips empty
  plaintexts) — an erreurs-explicites hardening candidate. CI race job flake:
  `TestReconcileStalled_ForceReapsCtxIgnoringWorker` (pkg/dispatcher) failed on
  56a5680f1, 5× green locally with -race — known concurrency-flake family.
- Engine hardening: `36fb786b5` (+ chart 0.33.0, umbrella `3a20e24`),
  `1d076a618`, `56a5680f1`; stale factory comment `afbdd6be3`.
- Lessons for next run: consult the RESOLVED sandbox mode everywhere run
  inputs depend on it; self-triggering a webhook from the connection identity
  hits the loop-guard (use a fresh head sha + close/reopen, or another actor);
  first devbox run per pod re-downloads the Nix toolchain (~2-4 min — the PVC
  warm-store follow-on).

### The issue → Featurly → PR half of the cycle (runs 019f4582 / 019f4590, fixes 4–6)

Labeling an issue `implement` is meant to route to the implementer (Featurly)
and open a PR that then re-triggers Billy — the other half of the loop. Two
more gaps surfaced, both fixed live:

- **Routing (fix 4, `2281a2eff`)**: a 3-bot webhook with no pinned default
  routed `issues/labeled` to **review-pr** (run 019f4582), which stopped at
  `diff_precheck` — an issue has no diff to review. `resolveReviewBot`'s
  SelectBot→review-pr fallback is right for a PR delivery, wrong for an issue.
  New `selectIssueLabeledBot` (pinned default → feature-dev → fallback), the
  issue-path counterpart to `selectForgePRBot`, wired on GitHub+GitLab.
  Validated: issue #86 → run **019f4590 = feature_dev**, and Featurly shipped a
  genuine, high-quality feature (threaded a `*log.Logger` through the whole
  generic-secret resolution path so silent credential drops become greppable —
  the erreurs-explicites finding the debugging itself surfaced), build+test
  green.
- **Forge token on the board-launch path (fixes 5+6, `483e69a3f` + `9b339c999`)**:
  Featurly implemented but `forge_auth_probe` found no `forge_token`, so it
  never opened its PR (`secrets_ref` null). Root cause: an issue-labeled
  delivery makes a **board card**, and the board coordinator
  ([boarddispatch.go](../../pkg/server/boarddispatch.go)) launches via
  `runs.Launch(BotID)` — which resolves generic secrets by (tenant, bot)
  **binding**, NOT the webhook secret override (that only reaches the direct
  webhook path). Forge provisioning set only the override; the tier-3 name
  fallback misses (stored `forge_github_<conn>` ≠ workflow `forge_token`). Fix:
  the orchestrator now upserts a per-bot `forge_token` binding at provision
  (fix 5), reconciled even on an idempotent re-provision so an
  already-provisioned integration is backfilled (fix 6). This is a general
  cloud-platform fix: EVERY board-launched bot that pushes (Featurly, Billy on the
  board path) needed it, not just this cycle.
- Standing gap (deferred): fully zero-touch issue handling still needs the
  cloud dispatcher (the webhook `issues` path is labeled-only by design); and
  carrying the webhook's secret_overrides/launch_vars onto the board card
  itself would make the board path robust without a binding (belt-and-braces).

## 2026-07-08 — re-dogfood post-improvement (P1-P4), same PR #72 target, $1.80 (run 019f41af)

- Status: **validated — reliability + integration confirmed; production excellent.**
  Improvements are RIGHT and SAFE; this run's shape (1 pass, no MR) did not trigger
  the two biggest savers (see "P1/P2 not exercised live" below — an honesty note, not
  a regression).
- Versions: bot branch-improve-loop @ `b7ea4bd78` (P1 forge_auth_probe + P4
  tool_max_steps 40→20 in `db812f0dc`; P2 verify_probe + P3 verify_build effort in
  `b7ea4bd78`) · iterion static binary `v0.31.0+b7ea4bd78` (built CGO_ENABLED=0,
  invoked directly so `os.Executable()` bind-mounts the fresh binary into the sandbox).
- Method: **apples-to-apples with the pilot below.** Same target: independent clone
  `/tmp/iterion-pr72-clone` **reset to the RAW PR #72 tip** `4b2394b94` (the pilot's
  added test removed, so Billy re-finds the gap). `sandbox` inline
  (iterion-sandbox-full:edge), `worktree: auto` bases on the clone's HEAD (CWD =
  clone), `--store-dir <main>/.iterion` so the run is **visible in the operator's
  studio** while operating on the clone (setupWorktree bases on CWD's git root but
  writes the worktree under `--store-dir`). `base_ref=main`, **`open_mr=false`**,
  `--merge-into none`, `--max-cost-usd 12`.
- Result: **converged in ONE pass, 8.5 min, $1.80 (31.8k tok).** Billy reviewed
  `main…4b2394b94` (the per-node `timeout:` feature) across all layers and re-found the
  exact same gap the pilot did — the parser/AST/IR/C199 were tested but the RUNTIME
  enforcement path was not — and added `pkg/backend/model/executor_timeout_test.go`
  (+115: a `blockingBackend` that blocks on the ctx deadline + `immediateBackend`
  happy path, asserting the bounded context cuts the node off AND that a
  context-deadline error is **not** retried). Commit `f5f55d5` on storage branch
  `iterion/run/ash-pulse-starforge-1c89`. `verify_run` re-ran `go build ./... && go
  test ./pkg/dsl/… ./pkg/backend/model/…` → **green** (so the added test compiles and
  passes). Graph flow: campaign → verify_probe → verify_build → verify_run → gate
  (converged) → mr_gate (open_mr=false) → done.
- Cost story vs the pilot's $2.26 (honest breakdown): campaign ~$1.17 (18.9k tok, was
  $1.31/21.2k — run variance on a slightly different pass) + verify_build ~$0.68 (12.9k
  tok, P3 effort now `medium`) + verify_run (deterministic tool, 9.8s) + **no
  finalize_mr** (open_mr=false avoided the pilot's $0.26). Net −$0.46 (~20%), of which
  ~$0.26 is the avoided MR finalize and ~$0.20 is campaign variance; **P3's verify_build
  delta was only ~$0.01** — verify.sh authoring uses little thinking, so `high→medium`
  barely moves it here (it will matter more on repos with a heavier build-capture step).
- **P1/P2 not exercised live (by design of this run's shape) — structurally proven:**
  - P2 (verify_probe skips verify_build) only fires on **pass 2+**; this run converged
    in 1 pass, so verify_probe correctly ran at `iteration=0` → `fresh=false` → routed
    to verify_build (the new node integrates without breaking the happy path). The
    **skip** path rests on `TestVerifyProbeLoopIterationWiring` (proves the loop
    iteration reaches the tool via the campaign→verify_probe edge with-mapping into node
    input — {{loop.*}} does NOT resolve inside a tool command) + the python-logic cases
    (fresh=true only when iteration>0 AND verify.sh valid) + `iterion validate`.
  - P1 (forge_auth_probe short-circuits finalize_mr when no push credential) only fires
    when `open_mr=true`; this run used `open_mr=false` (mr_gate → done), so the probe
    wasn't reached. Its logic is deterministic + unit-covered.
- Reliability: **HIGH.** Same target → same high-value conclusion (add the runtime
  enforcement test), reproduced independently. The two new deterministic nodes
  (verify_probe, forge_auth_probe) slotted into the graph with zero runtime surprise.
- Engine hardening: none needed. The e2e scenario sweeps gained stubs for the two new
  nodes (`aad7bddce`, `test(e2e): stub verify_probe + forge_auth_probe`) so
  `go test ./e2e/` stays green.
- Lessons for next run: to **measure P2's $0.69/pass saving live**, point Billy at a
  ≥2-pass target (a branch with 2+ real issues, or a synthetic branch with a planted
  build-affecting change so pass 2 re-runs). To **measure P1's skip live**, run
  `open_mr=true` with no forge_token and no host `gh` auth → forge_auth_probe → done.
  For a pure production/cost demonstration this 1-pass/no-MR run is the right shape and
  $1.80/8.5min is a good point; the structural savers show on longer/MR runs.

## 2026-07-08 — pilot on a real contributor PR (#72), converged in 1 pass (run 019f415c)

- Status: **validated — high-quality single-commit improvement.**
- Versions: bot branch-improve-loop (feat/agent-node-timeout tree) · iterion 1f5082f (static, F7 skills fix in the engine)
- Method: local run, `sandbox: auto` (iterion-sandbox-full:edge, devbox toolchain
  so the build/test gate has Go — the cloud runner does NOT, cf. F6). Independent
  clone checked out on the PR branch `feat/agent-node-timeout` so `worktree: auto`
  bases on the PR (NOT main — a linked git worktree bases on the shared repo HEAD,
  which is main; an independent clone was required). `base_ref=main`,
  `max_passes=2`, `open_mr=true`, `mr_branch=iterion/billy/pr-72-pilot`.
- Result: **converged in ONE pass.** Billy reviewed `main...feat/agent-node-timeout`
  (the per-node `timeout:` feature), found the parser + C199 validation were tested
  but the RUNTIME enforcement path (`executeBackend` deriving the bounded context —
  the feature's behavioral heart) was not, and added
  `pkg/backend/model/executor_timeout_test.go` (+108: `TestNodeTimeout_Enforced` with
  a ctx-blocking backend + 20ms timeout, plus a happy-path immediate backend). The
  deterministic build/test gate (`verify_run`) passed green. Exactly the test a good
  human reviewer would add. Pushed by hand to `iterion/billy/pr-72-pilot` → draft PR
  #82 (base = the PR branch, so the diff is just Billy's addition).
- Cost: campaign $1.31 (21.2k tok) · verify_build $0.69 (13.1k tok) · verify_run
  (deterministic tool, 9.7s) · finalize_mr $0.26 (6.8k tok) = **~$2.26, ~9 min**.
- F7 (engine) re-validated here too: `finalize_mr` successfully `Launching skill:
  forge-mr-create` — the directory-form skills fix works on the local sandboxed path,
  not just cloud.
- Efficiency/reliability misses to fix (operator asked to bring Billy to a Willy-grade
  production/cost ratio):
  - **finalize_mr burns budget discovering there are no credentials**: with no
    forge_token mounted and no `gh auth`, the agent ran 4 probe commands before
    abandoning the push. A deterministic pre-check should skip finalize_mr (or
    short-circuit it) when no push credential is present.
  - **verify_build ($0.69)** re-authors the build/test script each run — candidate for
    caching / lower effort.
- Lessons for next run: base the worktree on the PR via an INDEPENDENT clone (a linked
  worktree bases on main). Provide a `forge_token` (or run in a `gh`-authed shell) if
  you want Billy to self-push; otherwise recover the commit from the `iterion/run/<name>`
  storage branch the engine always creates and push by hand.

## 2026-06-14 — re-validation on a clean clone + good dead-code judgment (run 019ec5bc)

- Status: **validated.** Re-ran in the C082 worktree studio (non-watchexec) on a
  clean iterion clone with a synthetic `billy-test` branch: one added file
  `pkg/log/billy_demo.go` exporting `WriteMarker`, which had no godoc AND
  swallowed the `os.Create` error. `base_ref=main`, `merge_into=none`.
- Result: cross-family review loop **converged** (review → fix → re-review →
  streak), `Run finished`, committed to storage branch
  `iterion/run/feral-crash-duskvane-127c` (`final_commit 9c5a5891`), not merged
  (merge_into=none respected).
- Value — **correct judgment, not a cosmetic fix.** Rather than just adding godoc
  + handling the error, Billy recognized `WriteMarker` as *unreferenced dead code
  that swallows an error* and **removed it** (`refactor(log): remove unused
  WriteMarker demo helper`, "No code in the tree calls it, so remove the dead
  code."). That's the right call — exactly what a demanding reviewer should do.
- Finding (minor, non-fatal): `fix_claude` (claude_code) emitted one
  `Tool error: StructuredOutput — No such tool available: StructuredOutput`
  before recovering and producing its output normally. The agent appears to try a
  `StructuredOutput` tool that isn't registered in the claude_code delegate — a
  wasted step, same broad family as the Devy claude_code-structured-output gap.
  Worth wiring/​silencing, but it did NOT block convergence.
- Convergence machinery (shared with Willy) is reference-correct; this run
  re-confirms it + the asymptote (no oscillation) on a fresh target.

---

**Status:** validated end-to-end (2026-06). **Scope of this report:** the
capability and the engineering hardening it drove. The target here is
iterion's own repository (the `feat/cloud-control-plane` epic), so target
details are included.

## Summary

Billy (the `branch-improve-loop` bot) was exercised against a real, large
branch — a ~7000-line / 42-chunk epic — and demonstrably:

1. **converges** a cross-family review/fix loop on a big diff (monotonic
   decrease to zero blockers + a two-family approval streak), rather than
   oscillating or stalling;
2. **finds real, high-value issues** the human author missed, and
   **authors complete fixes** for them — including an ADR for a design-level
   change;
3. **drives the full pipeline** plan_chunks → alternating cross-family review
   (Claude ↔ GPT) → same-family fix → `streak_check` → `prepare_commit` →
   semantic `commit_changes`, and stops at the asymptote.

The exercise also hardened the engine: **one significant runtime bug was
root-caused and fixed** while driving real runs — without it the GPT family
could not review a diff this size at all.

## What was validated

- The loop: `plan_chunks (deterministic diff measure + chunking) →
  round_robin reviewer (claude-opus-4-8 ↔ openai/gpt-5.5) → family-matched
  fixer → streak_check (2 consecutive opposite-family approvals) →
  prepare_commit → commit_changes`.
- **Chunked review at scale:** a 42-chunk diff, each reviewer reading chunks
  one at a time then merging into one whole-diff verdict (cross-family
  approval is on the whole diff, never chunk-by-chunk).
- **Convergence to an asymptote**, not oscillation — the bot settled into a
  stable approved state and committed.
- **Both backends working through the whole run**, including the GPT family on
  the local ChatGPT-forfait path.

## Method

A single end-to-end run (`019eb168`) on `feat/cloud-control-plane`, ~7000
lines of diff. Reviewers/fixers: `claude-opus-4-8` (Claude Code) and
`openai/gpt-5.5` (claw, ChatGPT-forfait). Sandboxed (per-run container),
`--merge-into none` so the result lands on a storage branch for review.
Budget raised to 8h/250$ for this run (the 2h/60$ default is too small for a
diff this size); convergence took ~2h of effective run time.

## Result

Converged: status `finished`, commit `1ffc4bc` —
`fix(secrets,memory): enforce bot-secret binding egress and isolate bot
memory by id`, 745 insertions / 84 deletions across 30 files, build + tests
green. Fast-forward-merged into the epic, then merged to `main`.

Convergence trajectory (blockers per verdict):

```
claude 2 → gpt 2 → claude 1 → gpt 1 → claude 1 → gpt 1
→ claude 0 (approved) → cross-family streak → commit
```

A slight oscillation near the end (GPT re-raised one blocker before settling)
is within the accepted asymptote behaviour; the `prior_pushback` /
`previous_scanned_areas` feedback kept verdicts from re-litigating resolved
items in a loop.

## What Billy found and fixed

Genuine issues in the reviewed epic, fixed with tests:

- `cloudpublisher` did not persist `RepoURL`/`RepoSHA`/`BotID`, breaking
  cloud/webhook **resume** and bot-bound secret resolution. Fixed across the
  publisher, `queue/types`, and the run store.
- `secretguard` did not intersect a bot-secret binding's egress hosts. Fixed,
  with ADR 018.
- Bot memory was not isolated by bot id. Fixed (`fsstore`, `scope`).
- Binding-route validation tightened; new tests added.

## Engineering hardening (the enabler)

Before the fix, `gpt-5.5` on the ChatGPT forfait died on the 42-chunk review
with `context_length_exceeded` — not a fundamental limit but a bug: the
forfait's effective context window is smaller than the model's advertised
1.05M, so claw's preemptive compaction (sized to the advertised window) never
triggered in time, and nothing reacted to the backend's rejection.

Fix: **reactive force-compaction** — on a context-window rejection the tool
loop force-compacts the running history to a shrinking target
(256k→128k→64k→32k, independent of the advertised window) and retries.
Surfaced as an `llm_retry`, reusing claw's existing pure compactor. With it,
Billy ran for hours with both GPT nodes (reviewer **and** fixer) and zero
context-overflow deaths.

## Operational resilience

The run absorbed several transient infrastructure interruptions — network
drop, ChatGPT-forfait cap, and an intermittent sandbox bootstrap flake — via
delegate-level network retries and an auto-resume loop that relaunches from
the checkpoint (no progress lost) until convergence. None were the
context-overflow bug; all were absorbed without operator intervention.
