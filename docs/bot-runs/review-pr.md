# Revi — `review-pr` run bilans

Read-only code reviewer. Revi reviews with one selected family by default
(`review_mode: mono`) or independent Claude + GPT reviewers when dual mode is
explicitly selected; findings are normalised/de-duplicated, and one issue per
finding is published to the native board (label `source:revi`). With `--var
pr_url` it also posts an inline forge review and an optional deterministic
commit-status gate. Never edits or commits. See
[bots/review-pr/](../../bots/review-pr/).

## 2026-08-13 — first review on a self-hosted GitLab, and the two engine gaps it surfaced (runs 019ffad9 / 019ffadb / 019ffb04)

- Status: **validated on the direct-launch path** (webhook lanes still blocked
  by the target instance's outbound allowlist — see below).
- Versions: iterion cloud prod v3.40.3 → v3.40.6 · bot `review-pr` 0.5.7
- Method: `POST /api/runs` with `repo_url` + `connection_id` (repo-targeted
  launch, no webhook), mono topology, `gate_context: iterion/review`,
  `post_to_board=false`. Target: a seeded merge request on a private GitLab
  19.2.1-ee instance (subgroup path five levels deep), forge credential a
  **group access token** so the bot posts under its own identity.
- Result: converged in ~8 min. 7 findings on a 14-line handler (1 critical
  SSRF-with-echo, 3 high, 1 medium, 2 low), 3 carrying one-click suggestions,
  posted as inline discussions + one summary note. The merge gate landed as
  `iterion/review=failure` on the MR head and renders as an external pipeline —
  advisory, since a GitLab commit status blocks nothing unless the project
  requires pipelines to succeed.
- Value: the reviewer **executed** the code it was judging. It copied the
  handler into a throwaway module outside the repo, reproduced the nil-deref
  panic and the content-sniffing XSS against a local server, then deleted the
  copy and showed `git status` clean. It also reported that `go vet` already
  fails on the branch — a claim about the build gate, not a taste judgement.

### Engine gaps (both fixed in #421, deployed as v3.40.6)

- **A repo-targeted launch could not clone from GitLab.** The runner clones
  with `http.followRedirects=false` (SSRF hardening) and GitLab 301s a bare
  repo path to its `.git` twin, so the clone died — through all 8 NATS
  deliveries, into the DLQ, with the redirect never named as the cause. GitHub
  serves both spellings, so nothing had ever exercised it. The launch handler
  now canonicalizes `repo_url` through `forge.CloneURLFor`.
- **GitLab could not READ commit statuses.** `CommitStatusLister` was
  GitHub-only, and both consumers fail closed without it: the gate reconciler
  abstains (a review that dies leaves the required context absent forever) and
  the zero-touch auto-fix lane refuses to act on a gate it cannot see. So the
  gate could be *posted* on GitLab but never *repaired* — the exact hole that
  had already been closed for GitHub. Implemented as the read-side mirror,
  keeping the newest row per name.

### Lessons for the next run

- **A BYOK key is a team-wide default, so billing isolation is a team
  boundary.** `secrets.Resolve` returns the first key of a provider visible to
  the run's team — `is_default` only orders candidates, it does not gate them.
  Putting a second sponsor's key on a team that already runs other repos
  silently moves those repos onto that sponsor's credit. A dedicated team per
  billing owner is the only clean separation. Once a key resolves, the CLI
  receives exactly `{ANTHROPIC_API_KEY}` and the pod's ambient forfait token is
  not forwarded, so the fallback cannot quietly take over.
- **Neither cost nor token counts prove which credential paid.** Both are
  estimated from token counts whatever the path (cost-signal work, native
  ticket), `env_keys=` in the spawn log counts the whole SDK env, and
  `last_used_at` is not serialised by the api-keys endpoint. The provider
  console is the only local proof.
- **A self-hosted forge may refuse outbound webhooks by policy.** GitLab's
  Admin → Network → Outbound allowlist rejects hook creation with a flat
  `Invalid url given` (HTTP 422) that names neither the setting nor the host —
  it looks like a malformed URL. Probe it early with a throwaway hook: the
  whole webhook half of an integration depends on it, while direct launches
  work regardless.
- Verifying a status on GitLab needs the **full** SHA: `GET
  /repository/commits/<short-sha>/statuses` returns `[]` rather than an error.

## 2026-08-04 — five review passes over the credential pool: what Revi caught that adversarial subagents did not (runs 019fc939 / 019fc94a / 019fc95c / 019fc972 / 019fcb7b)

- Status: **validated, and the highest-value reviewer signal measured so far.**
- Versions: iterion cloud prod v3.23.2 → v3.24.0 · bot `review-pr` as deployed
- Method: mono topology, gate `revi/review` required by the merge queue. Five
  passes across two PRs (#350, then #356) on a ~6000-line feature
  (`pkg/credpool` + publisher tier + runner accounting + studio + CLI).
- Result: 14 findings kept across the five passes (3 high, 8 medium, 3 low).
  Every high was real and fixed. #350 merged once the gate went green.

### The finding that matters most for calibration

The feature had already been through **four parallel `/simplify` agents and
two adversarial review agents at max effort** before Revi saw it. Those layers
produced 38 fixes. Revi then found, among others:

- **a `[high]` fail-open on the donor's bot allow-list.** `LaunchSpec.BotID` is
  empty for an inline uploaded `.bot`, and the availability check treated an
  empty bot id as "no filter to apply". A donor who restricted their
  subscription to one bot would have had it handed to arbitrary uploaded code —
  the restriction yielding on the single input the requester fully controls.
- **claw cost counted twice**, which would have drained every donor at 2× and
  silently tightened every tenant's monthly cap.
- **a `[high]` defect in one of my own fixes**: the per-slot allowance share
  was a no-op on the *renew* path, because `decideRenew` re-synthesises the
  `Limits` it judges with and dropped `MaxConcurrentRuns`. My regression test
  for that fix only exercised fresh acquisitions — which is exactly why it
  passed.

**Lesson.** Adversarial subagents share the context that produced the code, so
they inherit its blind spots. Revi reads the diff cold, and that is where its
value is: not in finding *more* problems, but in finding a *different class* of
them. Worth the ~15–20 min and ~$1–2 per pass on anything touching money,
credentials, or consent.

### The one no reviewer could have found

`iterion remote pool` could never work at all: `identityFromPAT` never sets
`OrgID`, so every CLI caller resolved to no pool and got "no credential pool
accepts contributions on this instance". Three review layers read that handler;
none *ran* it. It took one real call against prod. **A reviewer verifies what
the code says; only an execution verifies what it does.**

### Frictions

- Revi cannot run the repo's tests (no network in its sandbox; devbox cannot
  realise its nix closure), and says so in a verification caveat. Its findings
  are reading-derived — accurate here, but it means the gate is a *review*
  gate, not a test gate.
- The prod instance's Claude forfait hit its **weekly** window mid-session,
  blocking Revi and therefore the merge — the single-point-of-failure this very
  feature exists to remove. It could not have saved itself: not deployed, and
  no donor had pledged.

## 2026-08-01 — the hand-off measured working in production, and a second hole under it (runs 019fbc1d / 019fbc26)

- Status: **the hand-off is validated live.** A separate, pre-existing defect
  keeps the fixer from posting its verdict on the board lane.
- Versions: iterion cloud prod v3.18.1 @ `4bd82c830` (the `publish:` fix)
- Method: same PR, redeployed instance, `/revi` then `/billy`.

### Measured

`prior_review` reached the fixer at **5829 characters** — it was **0** before the
fix. It carried the stable id (`R727eac`), the anchor note pinned to the current
head, `confidence: medium`, the reviewer's ready-made replacement block, and the
open-questions channel. The engine emitted **6 `artifact_written` events** for
`diff_precheck`, `merge_reviews` and `converge`, under the exact publish names
declared; there were **zero** before.

That also settles the caveat left on the previous entry: the cause was the
missing `publish:`, not cloud storage. `GET /api/runs/<id>/artifacts` still
returns `[]` — that endpoint has no mongo listing behind it and is a red herring;
the targeted read the hand-off actually performs works.

The loop is real, not just wired: this review reads the commits the fixer pushed
in the earlier run and raises a second-order defect **in the fixer's own fix**
(`Warm` re-introducing an unrecoverable crash through the panic-recovery the
same branch had just added).

### The remaining hole: a board-launched bot cannot post anything

The fixer pushed, then `publish_verdict` returned *"no forge publish grant on
this run"*. Measured side by side on the same PR:

| | forge_publish_url | forge_publish_token | gate_context |
|---|---|---|---|
| reviewer (`mode: direct`) | yes | yes | `iterion/review` |
| fixer (`mode: board`) | — | — | absent |

So the fixer posts no verdict table, no finding ledger and no merge-gate status,
and the required check stays on the revision before its push. The grant is minted
in the webhook launch tail (`injectForgePublishVars`) and the operator's
`gate_context` is layered there too; a board-mode command materialises a card and
the cloud coordinator launches from `BotArgs` ONLY, so both are dropped. The
declared hand-off vars survive because `ensureBoardCard` copies them explicitly —
nothing else does.

Not caused by this work, and it is the exact pre-flight the plan for the gate
phase demanded ("verify `gate_context` and the publish grant actually reach a
board-launched fixer before designing on top"). The measured answer is no.

Fix direction: mint the grant at board-launch time rather than carrying it (a
grant has a TTL and a card can be claimed much later), and layer the
integration's operator launch vars there too.

## 2026-07-31 — the Revi→Billy hand-off read an artifact no node ever wrote (runs 019fb9bc / 019fb9c6)

- Status: **partial — Revi validated end to end, the hand-off to the fixer proved
  non-functional in cloud.** The defect predates the declarative rework: the
  shipped `stampPriorReview` read the same artifact.
- Versions: review-pr 0.5.7 · branch-improve-loop 1.1.0 · iterion cloud prod
  v3.17.7 @ `af787562c` (verified to contain the hand-off work)
- Method: `SocialGouv/iterion-test-appy-e2e` PR #2, seeded with a real module and
  three planted defects (unsynchronised map written from a `Warm` fan-out;
  a failed fetch cached for the whole TTL; a loop-variable capture that is NOT a
  bug under the declared `go 1.22`). Revi auto-launched on PR open; `/billy` by
  comment afterwards. Repo provisioned with both bots, `gate_context` pinned.

### What worked, verified on the forge

- **Revi found the real bugs and refused the planted false positive.** critical:
  the concurrent map access, *"reproduced empirically … crashed in 4 of 5 runs"*;
  high: the cached failure, *"call 1 returns connection refused, call 2 returns
  body=\"\" err=<nil>"*. The loop-capture did **not** become a finding — it went
  to `questions` with the reason (`go.mod` declares 1.22, per-iteration
  semantics, verified empirically). The falsifiability channel did its job.
- **Stable finding ids are live**: `Ra34eca`, `R1dce3f`, plus the arbitration
  line the review now carries — *"Fix them yourself, or comment /billy … adding
  e.g. skip Ra34eca and your reason leaves that one alone."*
- A `replacement` was produced and rendered ("Proposed replacement:").
- The gate landed: `revi/review = FAILURE` on the head.
- Mono topology reported honestly, no cross-confirmation claimed.

### The defect: the producing node never wrote an artifact, anywhere

`/billy` launched with the right PR context (`pr_url`, `head_sha`,
`push_branch`) and **`prior_review` empty**.

The first read of the evidence — `GET /api/runs/<id>/artifacts` returning `[]`
for every run checked, and 89 events with **zero `artifact_written`** — looked
like a cloud-storage gap. It is not (see the caveat below), and the correction
matters: the engine
persists an artifact **only for a node that declares `publish:`**
(`runtime.persistArtifactIfPublished` returns early otherwise, [engine_exec.go](../../pkg/runtime/engine_exec.go)).
Neither bot declared it on any node — `grep -c "publish:"` was **0** for both.

So the hand-off resolved `LoadLatestArtifact(runID, "converge")` against an
artifact that had never existed, on any run, local or cloud. Not a regression of
the declarative rework either: the version it replaced read the same node the
same way. It was recorded as shipped and never once exercised end to end.

Every test stayed green because every test **wrote the artifact by hand** —
the one thing that had to be true in production was the one thing never asserted.

**Fixed**: the four nodes the manifests name as hand-off sources now declare
`publish:`, and two guards make the omission impossible to repeat — a catalog
test requiring `publish:` on any node a manifest declares as a source, and an
e2e running the REAL engine over one node per source kind (agent, compute,
tool) plus an unpublished twin.

**Caveat, stated because the evidence does not cover it.** That the cause is the
missing `publish:` and *not* a cloud-storage gap is an inference from the engine
code plus a local run, not a measurement on prod: no cloud run has been observed
writing an artifact since. The mongo store implements `LoadLatestArtifact` and
the conformance suite pins multi-version latest-wins, so it is very likely — but
the honest status is unverified until a redeployed instance is re-dogfooded.

### Lessons for next run

- **A declaration is not a mechanism.** `produces: node: converge` reads as if
  naming the node makes its output available; it does not — the node must also
  publish. Any future hand-off kind needs the same pairing, and the catalog
  guard now enforces it.
- **A test that writes the fixture by hand cannot prove the producer writes it.**
  That is what hid this for the whole build. Where a contract spans producer and
  consumer, at least one test has to run the producer for real.
- The forge identity matters: the first `/billy` was refused *"self comment
  (loop-guard)"* because the repo was provisioned on a **PAT connection whose
  account is the operator's own**. Re-provisioning onto the GitHub App
  connection fixed it. The guard was right; the provisioning was the mistake.
- A freshly provisioned repo shows `auto_fix_on_gate_failure` absent — the
  zero-touch lane is off unless asked for, confirmed on real config.

## 2026-07-30 — Revi had stopped publishing on every repo, and finished green doing it

- Status: **defect found and fixed** — the runs were fine, the publishing step
  was dead. Found while wiring `iterion/review` as a required check, which is
  the only reason it surfaced at all.
- Versions: bot review-pr 0.5.6 · iterion `main` @ `7b87b5f37` + `34bd00879`
- Method: `/revi` on buildkit-operator #4 (run `019fb403-530a`, 2m42s) and #7
  (`019fb403-5f28`, 3m39s), plus the iterion PRs of the day (#323 →
  `019fb408`, ~12min). Cloud prod, mono topology.
- Result: after the fix, all three posted their review **and** their commit
  status. Before it, every one of them finished `finished` having posted
  nothing. Verbatim from #323 — both fixes visible in one line:

  > ## Code review by Revi (iterion)
  > 4 finding(s) kept after threshold/cap. — medium: 2, low: 2
  > Reviewed by a single model family (mono topology): no finding is
  > cross-confirmed, and none is meant to be.

  with `revi/review=SUCCESS` on the head.

### The defect: a template that never resolved

`publish_review` built its guard input as `REVIEWED_SHA={{outputs.…}}` inside a
tool node's `command:`. That body is resolved by
[resolveCommandTemplate](../../pkg/backend/model/executor_tool.go), which
substitutes `{{input.X}}`, `{{vars.X}}`, `{{secrets.X}}` and `{{run.id}}` —
**`{{outputs.…}}` resolves only in edge mappings**. Written in a body it
survives as literal text.

So the stale-anchor guard compared the literal string `{{outputs.…}}` to the
PR's head sha, concluded the anchors were stale, and skipped the whole publish:
review, inline comments, and gate status. No error anywhere — the node
succeeded, the run finished, and the PR simply never heard from Revi. This had
been true **repo-wide**, on every review, for as long as the guard existed.

Had the required check been switched on before this was found, it would have
blocked every pull request on the repo — an outage caused by a check that was
never posted, on runs reporting success.

### Second defect on the same path: the guard took the gate down with it

Even a *genuinely* stale anchor set skipped the entire publish. But a stale
inline anchor only means the line numbers moved — it says nothing about the
verdict. Dropping the gate along with the comments turns a cosmetic problem
into a permanently absent required check. `stale_anchors` now drops the inline
comments and **keeps publishing** the summary and the status.

### Third: mono claimed a cross-family confirmation that never happened

The summary printed `N finding(s) cross-confirmed by both model families` even
in mono topology, where one family ran. Spotted by jo on the real comment on
buildkit-operator #6 — the reviewer was describing a corroboration it had no
way to perform. Mono now says so in as many words.

### Guards added

- `bots/catalog_command_refs_test.go` — catalog-wide: **no** `{{outputs.…}}` in
  any tool `command:`/`script:`/`postcondition:`, walking every `.bot`. The
  class, not the instance: the same silent no-op was available to every bot in
  the catalog.
- `bots/review_pr_stale_anchor_test.go` — drives the real publish body against
  a stub, shell-quoting substitutions the way the engine does, so the guard is
  exercised on the code that ships rather than on a paraphrase of it.

### Lessons for next run

1. **`{{outputs.…}}` in a command body is a silent no-op**, not an error. Any
   comparison against one is a comparison against a constant string — it will
   take whichever branch that constant happens to select, forever.
2. **A guard that suppresses output must never suppress the verdict.** Degrade
   the part that is unsafe (the anchors), keep the part a required check
   depends on.
3. A bot that publishes nothing looks exactly like a bot with nothing to say.
   Neither the run status, nor the logs, nor a green test suite distinguished
   them here — making the check *required* is what finally did.

## 2026-07-08 — GitHub PR webhook e2e on iterion cloud prod

- Status: **validated — full end-to-end via the inbound webhook.**
- Versions: bot review-pr 0.2.0 (post the `emit`→`converge` rename below) · iterion cloud prod `:edge` @ 93bc604+
- Method: cloud prod (ovh-prod). Connected a GitHub forge (PAT) on a fresh team,
  enabled Revi on a test repo (`SocialGouv/iterion-e2e-mathkit`), opened a PR
  with an intentional defect (`subtract` skipping the module's `assertFinite`
  input-validation invariant). The `pull_request` webhook launched Revi on a
  cloud runner (no sandbox).
- Result: both reviewer families ran (reviewer_claude/claude-code +
  reviewer_gpt/gpt-5.5), `converge` merged them, `publish_review` posted a GitHub
  review (COMMENTED) — "2 findings (1 medium, 1 low; 1 cross-confirmed)" + 2
  inline comments (src/calc.mjs:26 medium correctness, test/subtract.test.mjs:8
  low tests). Both families independently caught the planted defect;
  cross-confirmation worked.
- Engine hardening surfaced by this run:
  - **The bot didn't parse in prod** (`agent emit:` shadowed the reserved `emit`
    node keyword, ADR-051 → E002 → webhook 502). Fixed by renaming the node to
    `converge`; added a CI guard (`TestCatalogBotsParseAndCompileClean`) that
    fails on any catalog bot that doesn't parse+compile — the gap that let it
    ship (both catalog-loading tests skipped on parse failure).
  - **Webhook idempotency poisoned by a failed launch**: the initial `opened`
    delivery 502'd but still consumed the idempotency key, so redeliveries
    returned `duplicate` (empty run_id) forever. Fixed: a StatusLaunchError row
    is now retryable. Only a NEW head sha (close/reopen after a push) unblocked
    the validation.
- Lessons for next run: `synchronize` does NOT re-trigger Revi by design
  (opened/reopened only) — to re-review, close/reopen or push a new head sha.
  Revi posts as the PAT's account (`devthejo` here); a dedicated bot account
  would read cleaner.

## 2026-06-13 — review the campaign diff (run 019ec0e8)

- Status: **validated — high value.**
- Versions: bot review-pr 0.2.0 · iterion 7fea84cd (binary refreshed mid-campaign)
- Method: `POST /api/runs`, `base_ref=9197bcfd` (review the campaign's own fresh
  commits `9197bcfd..HEAD` — the `scan_shards`/`botregistry` fixes + the bilans),
  `severity_threshold=low`, `post_to_board=true`. Read-only, no sandbox. Backends:
  `claude_code` (reviewer_claude, emit) + `claw` gpt-5.5 (reviewer_gpt). ~37k tokens,
  ~$1.18, 151 steps, status `finished`.
- Result: `diff_precheck` (found changes) → fan-out **reviewer_claude ‖ reviewer_gpt**
  (parallel, confirmed) → `emit` → **1 deduped board issue** (`source:revi`,
  `severity:medium`, `type:correctness`). No commits (read-only, as designed).

### Value (genuinely high — caught a real second-order bug)
- The single finding is excellent: **"Cloud request-construction failures block until
  shard timeout" at `cmd/iterion/scan_shards.go:458`** — i.e. Willy's fix `4c525a6e`
  (handle the dropped `http.NewRequestWithContext` error) is *masked* by `awaitTerminal`,
  which polls a run document that never exists for a never-launched shard, hanging until
  `--timeout` (default 2h) instead of failing fast. Precise anchor, correct mechanism,
  actionable fix sketch. **Verified against the code and fixed** (`59cfedcc`, with a
  regression test). The pre-existing `ITERION_SERVER_URL`-unset / read-workflow paths
  had the same latent hang.
- **No noise:** the diff was mostly docs (≈280 of 387 lines) + two small code changes;
  Revi flagged 0 in the clean botregistry dedup, 0 in docs, and 1 real issue in the
  changed Go. Cross-family dedup worked; severity/type/confidence labels are clean.
- **Dogfood dynamic worth keeping:** a *breadth* bot (Revi) caught an incompleteness in
  a *depth* bot's (Willy) committed fix. Running review-pr over each loop bot's output is
  a cheap, high-signal second line of defence.

### Findings / misses
- The finding came from the **gpt** reviewer only (confidence `medium`) — Claude's
  reviewer didn't independently raise it. Single-family findings are real but lower-
  confidence; the cross-family agreement signal didn't fire here (still correctly
  published at the `low` threshold). No false positives.
- Minor: the `emit`/`reviewer_*` node outputs aren't surfaced in `run.json.checkpoint`
  in a easily-parsed shape (had to read the board to see findings) — cosmetic.
- **Repo scatter (low — repo-agnostic):** `report_path` defaults to
  `.review-pr/findings.md`, so Revi drops an **untracked `.review-pr/` dir into the
  target repo root** (not gitignored). Per CLAUDE.md "Catalog bots are repo-agnostic",
  a default that writes into the target tree should be gitignore-friendly. Fixed here by
  adding `.review-pr/` to iterion's `.gitignore`; for a pure dry-run pass
  `--var report_path=/tmp/revi-findings.md`. (A nicer bot-side default would append the
  dir to the target's `.gitignore`, or write under a path the operator already ignores.)

### Engine hardening
- `awaitTerminal` pre-dispatch-failure hang — **fixed `59cfedcc`** (+ regression test
  `TestAwaitTerminal_PreDispatchFailureDoesNotHang`). Directly attributable to this run.

### Lessons for next run
- Revi is a strong, low-noise read-only reviewer; point `base_ref` at the commit before
  the work to review a clean range (`base..HEAD`). Default `post_to_board=true` lands one
  issue per finding under `source:revi` — fine for real triage, set `false` for a pure
  dry-run.
- Use Revi as a routine second pass over Willy/Featurly/Billy output — it catches
  second-order issues the implementer's own review loop can miss.
