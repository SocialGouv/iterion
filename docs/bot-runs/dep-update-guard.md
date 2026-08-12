# Vetty — `dep-update-guard` run bilans

Reactive security + alignment guard for automated dependency-update PRs
(Dependabot / Renovate): audit the bump (supply-chain + CVE), align the
consuming code, prove the tree with the deterministic verify gate,
commit onto the PR branch, post the verdict comment. Never merges past a
check — and only ever the commit it audited. See
[bots/dep-update-guard/](../../bots/dep-update-guard/).

## 2026-08-12 (evening) — both missing proofs land: an alignment merged, and the advisory path fires blind

- Status: **validated.** The two things this bot had never been seen to do, it
  did — within four hours of each other, on the deployed build.
- Versions: Vetty **2.7.5** · iterion **v3.40.3** (`82ecebe0`), verified in the
  pod rather than assumed (`kubectl exec deploy/iterion-runner -- grep -m1
  '^version:' /opt/iterion/bots/dep-update-guard/manifest.yaml`).

### The alignment landed end to end, in a merged PR

**#394 merged at 18:49 as `88600c5fa`**, and the squash commit carries both
halves of the work:

```
 pnpm-lock.yaml      | 210 ++++++++++++++++++-------------
 studio/package.json |   2 +-
 Co-authored-by: iterion-forge-61934180[bot]
 * chore(deps): regenerate the pnpm lockfile for @types/node 26.1.2 → 26.2.0
```

Dependabot moved two lines of `studio/package.json`; **Vetty wrote the other
210.** CI installs frozen, so without that regeneration the install fails
before a line compiles — the bump was un-mergeable as opened, and is merged
now because the bot fixed it. That is the whole contract of this lane, observed
for the first time from bump to merge commit.

What made the difference was not the alignment — that had worked for weeks (otel
1.45 on #400, Vite 6→8 on buildkit-operator #19). It was removing the two
reasons a correct alignment kept being thrown away: a base tree that could not
pass its own tests inside the sandbox (#413), and a report that could not say
why (#412).

### The advisory path fires on a bump nothing in the tree betrays

**#414** bumps `node-ipc` 9.2.1 → 10.1.1 — the `peacenotwar` release
(GHSA-97m3-w2cp-4xx6 / CVE-2022-23812, CWE-506, 9.8). Verdict **MALICIOUS**,
`hold_security`, no alignment, no commit. Four auditors agreed independently
(osv-scanner on the lockfile, the OSV API, `npm audit --package-lock-only`,
`trivy fs`), and the report states the property that makes this run worth more
than #410's:

> Nothing in the tree betrays it: index.js is inert, the lockfile integrity hash
> is valid, and the two newly added transitives are clean per OSV — the signal
> exists only in the registry advisory.

It also noted, unprompted, that the current pin is clean, so the bump travels
**safe → malicious** rather than remediating anything; and it disclosed two
coverage gaps (the fixture lockfile lists transitives without top-level entries,
so osv-scanner resolved 1 package not 3 — covered by per-package API queries;
and no `npm install`, so lifecycle scripts were not inspected on disk).

Together with the morning's runs the matrix is complete: **payload analysis →
MALICIOUS, advisory lookup → MALICIOUS, control → SAFE.**

### The fix, measured on the real batch

`pkg/cli` needed a container runtime it could not have inside the sandbox, so
iterion's own suite was red there and the fail-closed gate held every bump for a
reason no bump caused. After #413, on re-review: **#392 green, #399 green, #394
green and merged.** #396 stays `hold_security` on its own merits (mongodb chart
16→19). The remaining reds are #390, #395, #397 — #395 now conflicts because its
stale lockfile commit collides with the one #394 landed, so Dependabot was asked
to recreate it.

---

## 2026-08-12 — the anti-malware half fires for the first time, and the verify gate is proven to hold on a red base

- Status: **validated for the security half, blocked for the alignment half**
  (both resolved the same evening — see the section above).
  Four runs: #410 (hostile fixture), #411 (its control), #394 and #395 (real
  Dependabot bumps).
- Versions: Vetty 2.7.4 (runs) → **2.7.5** (fixes below) · iterion v3.40.0.
- Method: `/vetty` on two purpose-built fixture PRs plus a re-review of two
  refreshed Dependabot PRs. Fixtures in `e2e/fixtures/malware-detection/`;
  branches `test/vetty-malware-*`, based on a scratch branch so nothing could
  reach `main`.

### The detection worked, and the control is what proves it

Three candidate 0.0.2 releases share one package **name and version** and
differ only in contents, so the bump is identical in shape across PRs and the
payload is the only variable. Candidate directories are named opaquely
(`release-0.0.2-a|b|c`) because Vetty explores the tree it audits — a
directory called `hostile` would hand it the verdict.

| PR | candidate | verdict | gate |
|---|---|---|---|
| #410 | `b` — adds a `postinstall` hook | **MALICIOUS** | `supply-chain risk: the bump was not applied` |
| #411 | `a` — adds one function | **SAFE** | `build/tests not green after alignment` |

The two gate descriptions differ: the hostile bump was stopped at the security
gate before any alignment, the control cleared it and went on. A hold that
reproduced on both would have measured novelty-aversion, not detection.

What the report got right beyond the verdict:

- **It refused the façade.** Every deterministic scanner — osv-scanner, npm
  audit, the OSV API, semgrep `p/default`, trivy, gitleaks — returned clean,
  and it said so: *"that cleanliness is meaningless: the package is unpublished
  (npm view → E404) and linked by file: path… Emitting 'safe' on that basis
  would have been a pure facade."*
- **It gave zero weight to the in-band disclaimer.** The payload carries a
  header saying it is inert by construction; the audit discounted it —
  *"an in-band claim of innocence is exactly what a hostile package ships."*
- **It falsified its own detectors.** The control run discounted semgrep as
  evidence *because* it ran the hostile sibling through it and also got 0:
  "non-discriminating for this malware shape". It named the exhaustive source
  review and the lifecycle-hook scan as the detectors that actually discriminate.
- **Signals are anchored** to `file:line` (`vendor/release-0.0.2-b/postinstall.js:11-15`
  for the `process.env` serialisation, `:31-37` for the `spawn`), which is the
  property the `no-package-malware` prior art was going to be mined for.

Coverage this montage does NOT establish: with a `file:` dependency the hostile
source sits inside the diff, so this proves the plumbing (audit →
`hold_security` → red gate → no merge) and the signal reporting, not the
judgement on an unseen registry package. **That half ran the same evening as
#414 and also held** — see the section above.

### The alignment half is blocked — but not by the alignment

*(Resolved the same evening: #394 merged with its alignment. Kept as written
because the diagnosis is the useful part.)*

#394 and #395 both held `build/tests not green after alignment`, and neither
red was caused by its bump:

- **#394 (@types/node 26.1.2 → 26.2.0)** — the audit was exemplary (registry
  ECDSA signature verified against the live key, tarball sha512 matched, 88
  `.d.ts` files and zero executables, no hook added, publisher unchanged). The
  alignment was **correct and complete**: it caught the stale root
  `pnpm-lock.yaml` and regenerated it — the rule added to the skill after the
  last batch did its job. Then the verify script died on its own construction:

  ```
  devbox run -- sh -c files=$(find . -name "*.go" ...)
  sh: -c: line 1: syntax error near unexpected token `then'
  ```

  The agent had transcribed a multi-line Taskfile command body into a single
  `sh -c` argument; the YAML block's newlines and tabs arrived literal.
- **#395 (@vitejs/plugin-react 5 → 6)** — audit SAFE with SLSA provenance
  traced to `vitejs/vite-plugin-react`'s publish workflow; no alignment needed
  on the refreshed head. Held on a Go test-suite `FAIL`.
- **#411, the control**, changes two JSON fixture files and **no Go code at
  all** — and also ended in `FAIL` on the same suite. That is the proof the red
  belongs to the tree, not the change. `go test ./...` on `origin/main` passes
  locally (including `bots`, `cmd`, `e2e`, and under the runner's injected
  `GIT_AUTHOR_*`), so the failure is specific to the sandboxed run.

Which test fails is **not knowable from the reports**, and that is the second
defect: the excerpt was `out[-4000:]`, a blind tail. A test runner prints its
per-package successes after the package that failed, so all three reports
carried a wall of `ok` lines and a bare `FAIL`, with the failing name scrolled
out.

### Fixes (Vetty 2.7.5)

- **The excerpt keeps the failing lines**, matched, in addition to the tail
  (`verify_run`). Falsifiable: reverting it reddens
  `TestDepUpdateGuardVerifyPrechecks/the_excerpt_keeps_the_failing_line` with
  exactly the observed symptom.
- **Skill rule — call the target, never transcribe its body** (`verify-build`
  §1c), with the reason: a task-runner block does not survive being passed as
  one `sh -c` argument, and transcription also drops the target's `dir:`,
  `env:` and prerequisites.

### Engine finding (not fixed here)

`prepareRepoWorkspace` clones the **default** branch plus the run's ref
([pkg/runner/loop_gitws.go](../../pkg/runner/loop_gitws.go)), so a PR whose base
is neither leaves `merge-base(base, HEAD)` unresolvable. #410 hit it and
degraded loudly (`degraded_reason`, scope narrowed to `HEAD~1..HEAD`) exactly as
designed — #411 even re-resolved the base itself and said the approximation did
not change its verdict. It bites stacked PRs and PRs targeting a release branch;
the fix is for the runner to fetch the run's `base_ref` too, which is generic
and not bot-specific.

### Lessons for next run

- **Refresh a stale PR branch before re-reviewing it.** Vetty verifies
  `merge-base(base, HEAD)..HEAD` and does not merge the base, so a PR cut before
  a fix landed still carries the old red and a re-review only reproduces it at
  ~$10 a run. `@dependabot rebase`, or `gh pr update-branch` when Vetty has
  already committed to the branch (Dependabot then refuses: *"edited by someone
  other than Dependabot"*).
- **`review_on_sync` is on for this repo**: pushing to the branch relaunches
  Vetty by itself. Commenting `/vetty` is only needed on a hand-opened PR.
- **A flaky or environment-sensitive test in the target repo holds every bump.**
  The gate is fail-closed by design and that stays right, but the base tree's
  health is now a precondition of the whole lane — worth establishing before a
  batch, not after three holds.
- **The target repo must be able to pass its own tests where the bot runs it.**
  The generalisable form of the above, and the one worth carrying to every other
  loop bot: a suite that is green on a developer's machine and red in a
  container is not a suite the bot can use as an oracle. Establish it with one
  command before blaming the bot —
  `docker run --rm -v $PWD:/src:ro <the bot's sandbox image> sh -c 'cp -a /src
  /tmp/w && cd /tmp/w && <the repo's test command>'`. Four holds and most of a
  day were spent reading bot reports for a defect that was ours, sitting one
  container run away.
- **A control run is not ceremony.** #411 changes two JSON files and no code;
  it is the only reason the morning's holds could be attributed to the tree
  rather than to the bumps, and the only reason #410's MALICIOUS verdict means
  detection rather than novelty-aversion. Budget one whenever a run is meant to
  establish something.

## 2026-08-11/12 — why the other ten held, and a guard that had to be deleted

- Status: **the batch is fully diagnosed and every cause is fixed.** No new
  dogfood run: this is the post-mortem of the eight `hold_unstable` verdicts
  from the batch below, read out of the Vetty reports themselves.
- Versions: Vetty 2.6.5 → **2.7.4** · iterion v3.36.1 → **v3.40.0** (deployed).
  PRs #401, #405, #407.
- **Four of the eight held on one test of OURS.** #392/#395/#397/#399 all failed
  `TestLogAllowsTabsInUserControlledFields` with `author: got
  "iterion-forge-61934180[bot]"`. The test sets a tab-bearing `user.name` on a
  throwaway repo and expects to read it back — but `GIT_AUTHOR_*` /
  `GIT_COMMITTER_*` OVERRIDE repo config, and a sandboxed run with
  `host_state: none` injects exactly those four so an in-container `git commit`
  has an identity at all (`runtime.seedGitIdentityEnv`). Every commit the suite
  made was authored by the run's own bot identity. Vetty was right that the
  build was red; it was red for a reason no bump caused.
- Chasing that found the same hole in production: every function in `pkg/git`
  names its repository through an explicit `dir`, then ran git with the caller's
  environment whole. `Status(dir)` under an inherited `GIT_INDEX_FILE` reports
  the tree as deleted-and-untracked; under `GIT_DIR` it answers about another
  repository. Now `git.SanitizeEnv` drops the redirection family (identity and
  transport preserved) and is applied at every call site in the tree —
  `pkg/runtime/worktree.go`, `pkg/runner/loop_gitws.go`, `pkg/runview/fork.go`,
  `pkg/dispatcher/commands.go`, `pkg/pluginsource/fetch.go`,
  `pkg/sandbox/kubernetes/driver.go`. Reviewing that by hand did not work (two
  of four sites in one file missed, one of them a `worktree remove --force`
  where `--git-dir` does NOT neutralise `GIT_COMMON_DIR`), so
  `TestEveryGitCallerSanitizesEnv` now sweeps it mechanically — and found a site
  I had declared absent.
- The other four: #394 was a stale lockfile Dependabot never regenerated (CI
  installs frozen, so the install fails before a line compiles — the skill now
  says regenerating is part of the alignment, and explicitly not
  `--no-frozen-lockfile`); #390 produced no `verify.sh` at all for a digest
  refresh whose correct alignment was empty; #398's `verify.sh` was ~2300 lines
  of bare paths that `sh` answered with `Permission denied`; #393 was a genuine
  `hold_security`.
- **The lesson worth keeping — a guard that was written, patched five times, and
  then deleted.** #390 and #398 became precheck rejections that loop back
  instead of terminal reds. The first (no script) is decidable from the file.
  The second was not: rejecting a script whose lines are paths rather than
  commands means judging shell text statically, and every review round found the
  next spelling it read wrong — a leading `cd`, a here-doc body, a subdirectory
  helper, a backslash continuation, an unresolvable `cd` target, an artifact the
  script builds before invoking. Each false positive held a bump that builds:
  the exact failure the guard was added to prevent, inflicted by the guard.
  Changing the axis (proportion of path-lines, not the line) bought one round.
  At the eighth it was removed. What survives are the two conditions decidable
  from the file alone — no script, and a script that runs nothing — plus the
  rule in the skill, where getting it wrong costs a rewrite instead of a hold.
- Revi ran **ten rounds** on #407 and found a real defect in every one, four of
  them inside guards added earlier in the same PR (`R12dc35`, `R78f11d`,
  `Rdbf846` high). Three findings were about the guard's own tests being
  vacuous — a fixed window letting a scrubbed neighbour vouch for an unscrubbed
  call site, an assertion sitting behind a `t.Fatalf` that could never run, a
  sweep that walked only `pkg/` and matched only single-line calls.
- Lessons for next run: (1) **prefer a rule in the skill to a guard in the
  gate** when the property is not decidable from static text — a wrong skill
  costs a rewrite, a wrong gate costs a hold; (2) a guard no mutation can red is
  decoration, and in the `verify_run` body it is a trap for the next edit —
  two pieces of code were deleted this round for exactly that; (3) the
  `verify_run` command is a shell double-quoted argument, so a backtick in a
  *comment* closes the DSL string (`E012 unknown tool property`) — it happened
  twice.

## 2026-08-10/11 — the first Dependabot batch merged unattended, and the one that merged was the one to catch (run 019fecf3)

- Status: **partial — the audit is production-grade, the verdict was not.**
  Dependabot opened 11 PRs (#390–#400) at 18:26–18:33 UTC; one run each.
  **#400 merged unattended at 21:04** (gate 20:50:18 → armed 20:50:23 → merge
  queue 21:04:43). The other ten held RED and none merged: 8 `hold_unstable`,
  2 `hold_security` (#393 TypeScript 7.0.2, #396 mongodb chart 16→19). Fail-
  closed works. Same night, buildkit-operator **#18 (Renovate, undici
  [security]) merged at 19:10** — 9s after the gate, no queue on that repo.
  Renovate and Dependabot are now both proven, on both merge topologies.
- Versions: Vetty 2.6.5 · iterion 3.36.1. Cost of run 019fecf3: **~$10.8**
  (audit $2.24 + align $4.58 + verify_build $3.27 + commit $0.70).
- **Value — the audit is the best this bot has produced.** On #400 (30 Go
  modules) it found the decisive property nobody asked for: *this repo
  vendors, so `go build -mod=vendor` never hash-checks `vendor/` and a clean
  `go.sum` proves nothing*. It regenerated the vendor tree from checksum-
  verified modules into a temp dir and diffed byte-for-byte. It caught a CVE
  resolution absent from Dependabot's own table (grpc 1.81.1→1.83.0 clearing
  GHSA-hrxh-6v49-42gf HIGH, transitive). It re-ran govulncheck under
  `CGO_ENABLED=0` after a gcc failure "rather than report a hollow pass", and
  refused to count 68 trivy hits confined to `e2e/fixtures/` — one of which is
  the deliberate protestware detection fixture. On bko#18: npm trusted-
  publisher OIDC with the *same* `oidcConfigId` across both versions (no
  maintainer change — the classic compromise shape absent), SLSA provenance,
  tarball sha512 vs lockfile integrity, byte-identical wasm blobs, and an
  honest coverage-gap statement (trivy parsed 0 npm packages from
  pnpm-lock.yaml, proven by a control run against the base — so trivy was NOT
  counted as evidence).
- **The defect: a lost alignment merged as a clean bump.** `align` (24 min,
  $4.58) detected a real BREAKING change — otel 1.45's `WithEndpointURL` no
  longer appends `/v1/traces` to a path-less endpoint — and wrote a
  `withTracesPath` helper plus a regression test it verified red without the
  fix. Then `commit` died on the forfait's session limit
  (`USAGE_LIMIT_BLOCKED`), the retry resumed 24 min later, and **the runner
  re-cloned**: `prepareRepoWorkspace` does `os.RemoveAll` + `git clone` on
  every claim, and `executeRun` deletes the dir again on return. The
  checkpoint restores node OUTPUTS, not files. The commit agent reported the
  gap precisely in prose ("ACTION NEEDED: re-run alignment … spans silently
  POST to /"), but `commit_check` read only the shas: unmoved head →
  `did_commit=false` → verdict `clean` = "the bump needed no alignment" →
  gate `success` → armed → merged. **The regression went to main.** Latent
  only because no chart configures `OTEL_EXPORTER_OTLP_*`.
- The irony worth keeping: the anti-façade rule that made `did_commit` read
  shas instead of the agent's self-report is exactly what hid this. The truth
  lived only in the agent's prose, and the graph does not read prose.
- Engine + bot hardening (this change): `commit_check` now computes
  `alignment_lost` = align claimed edits ∧ the head did not move, routed to a
  fifth, BLOCKING verdict `hold_lost_alignment` (Vetty 2.7.0); the runner
  emits `run_workspace_reset` when a repo-backed run re-executes (keyed on the
  checkpoint, so a `running`-status redelivery counts too) so the discarded
  tree stops being invisible.
- **And the alignment itself was wrong** — found by the adversarial review of
  this very fix, not by the run. Vetty's `withTracesPath` helper (which I
  re-applied verbatim, so I repeated its mistake) re-implemented what the SDK
  already does correctly: `otlpconfig.NewHTTPConfig` applies the env config
  BEFORE explicit options, and it honours the OTLP spec's two DIFFERENT
  endpoint semantics — `OTEL_EXPORTER_OTLP_ENDPOINT` is a base URL that gets
  the signal path joined onto it, `…_TRACES_ENDPOINT` is used as-is. Passing
  the resolved endpoint back through `WithEndpointURL` flattens both into one
  reading; **that override was the bug's enabler, not its victim** (it is why
  1.45's behaviour change reached us at all), and it left
  `ENDPOINT=https://collector/otlp` POSTing to `/otlp` — the same silent loss
  one level over. The real fix is to stop overriding. The premise Vetty
  reasoned from was a false comment already in the file, claiming the SDK
  honours only one of the two variables.
- Lessons for next run: (1) a node that MUTATES the workspace and a node that
  PERSISTS the mutation must not be separated by a resumable boundary without
  a check that they agree — the gap between them is a pod restart wide;
  (2) an agent's honest prose report is not a signal the graph can act on —
  if it matters, it must reach a schema field a gate reads; (3) 8/10 PRs
  landing on `hold_unstable` deserves its own look — fail-closed is correct
  but a loop that holds everything delivers nothing; (4) **an aligner inherits
  the consuming code's wrong assumptions** — Vetty read a comment asserting the
  SDK ignored one of the two env vars, believed it, and wrote a fix around it.
  It reproduced the file's existing false premise faithfully. Worth adding to
  the align prompt: when a bump breaks a call site, check whether the call site
  was right to exist.

## 2026-08-04/05 — the backlog majors + the Vite 8 alignment: capability proven, and the verify seam found its shape

- Status: **capability validated; landing required four bot/engine fixes found
  in stride.** Of the 6 pre-group major PRs `/vetty`'d: **#239, #38, #32
  merged by the loop** (#38 dissolved the App-workflows-permission fear — on a
  queue repo the QUEUE merges, not the App; #32's recreated branch re-audited
  `clean` with a pro-grade risk trade-off: the one introduced sidecar HIGH has
  an upstream fix while holding would keep a CRITICAL JetStream auth-bypass);
  **#20, #21, #33 closed on grounded hold verdicts** (a self-contradictory
  Dependabot recreate — trailer 6.0.3 vs diff 7.0.2; typescript-eslint peers
  capping TS <6.1; Bitnami Secure Images mutable-`latest` posture). The
  escalate lane ran its full arc live: bare-model-spec crash (fix 2.6.2 +
  fleet lint) → 401 on a forfait-only deployment (engine fix: llm_or_human
  degrades to a HUMAN PAUSE, #367) → a real operator answer resumed the run
  through `iterion remote runs resume --answers`.
- **Vite 8 (PR #19), the alignment objective jo set explicitly:** the aligner
  performed the toolchain migration REPEATEDLY and correctly — first as
  manualChunks object→function, then (recreated branch) as Rolldown's
  `output.codeSplitting.groups` with the three named chunks verified, plus
  plugin-react `^5.2.0` chosen as the minimal peer move, lockfile regenerated,
  `import.meta.dirname` for the CJS `__dirname` deprecation. GitHub CI went
  fully green on the migrated head. What kept discarding the work was the
  VERIFY seam, three ways in one day: an env-dependent cost test (fix #368 —
  recovered from the orphaned cross-branch commit), a verify.sh omitting the
  repo's CI drift gate (the agent scopes "bump-relevant" and rationalises
  away repo-wide gates), and a verify.sh stricter than CI (eslint failing on
  534 pre-existing warnings / 0 errors).
- Fixes shipped: **#368** (hermetic cost tests), **#369** (skill §1c: mirror
  CI's exact strictness — never stricter, never looser; the litmus "name the
  CI step that reds on it"), **#370** (the drift-gate precheck LOOPS BACK
  into verify_build with the exact rejection — an authorship defect self-heals
  in-loop instead of discarding the alignment at verdict time; budget 4h/$25
  for the looped heavy case). #370 took three Revi rounds: R1acb67 (budget vs
  loop, real), then Rc24903 (my rc=4 loop-back would have permanently
  defeated a DELTA gate whose baseline swallows first-pass leftovers on
  retry — accepted and reverted). Revi's blocking gate caught in review what
  none of my local tests did, twice.
- Lessons: an agent-authored verify script is the least deterministic node in
  a deterministic pipeline — pin it three ways (mirror CI exactly §1c, never
  scope out repo-wide gates §1b, and loop the precheck back instead of
  judging at verdict time); a delta-based gate must never sit inside a retry
  loop; suggestions in a review are hypotheses, not fixes — adopting Revi's
  rc=4 idea without thinking its semantics cost one full round.

## 2026-08-04 — the retest: all 3 PRs merged by the system, every recovery lane exercised live

- Status: **validated end-to-end.** After the fix waves below shipped (#357
  reconciler+relaunch, #358 per-bot claim, #361 budget terminal-ack; Vetty
  2.6.1, `arm_automerge` flipped on the iterion integration), one `/vetty` per
  PR was the only human act. Final: **#353 merged 10:57, #362 (recreation of
  #355) merged 14:27, #354 merged 15:30** — each through Vetty's own arm →
  merge-queue path.
- Versions: bot 2.6.0→2.6.1 · iterion prod `f272e2306` puis `5f64a87c0` ·
  runs `019fcc30-*` (first pass ×3), `019fcc7a`/`019fccc0`/`019fcd0c` (#354
  chain), `019fcca9`/`019fccd8` (#362 chain).
- What each PR proved:
  - **#353** — arm-first on a merge-queue repo: Vetty's
    `enablePullRequestAutoMerge` auto-enqueued the audited head at green
    (`added_to_merge_queue` 10:24:33); the queue survived two merge-group
    rebuilds caused by concurrent release pushes and merged. The
    merge-now-first ordering of 2.5.0 could never have landed this.
  - **#354** — the full recovery gauntlet, unplanned and perfect: run 1 died
    on the 75m budget mid-verify → **reconciler posted the synthetic
    `failure` with the budget reason** + run link, **relaunch lane fired in
    12s** (run 019fcc7a) → that run died on 75m too (verify ~30% slower on
    cold nix) → second death **relaunched again under the per-bot key**
    (post-#358 deploy, run 019fccc0, now 2h budget) → **aligned the MCP
    go-sdk 1.7.0 break with a root-cause fix** (`fix(mcp): probe liveness
    with tools/list on protocol revisions without ping` — revision
    2026-07-28 / SEP-2575 removed ping; the health-check now probes
    `tools/list` on ≥ that revision; no test weakened) → push
    self-superseded → final run 019fcd0c re-audited, `test`+`race` went
    green on the aligned head, gate success, armed, queue, merged.
  - **#355→#362** — the maintenance lesson: **Dependabot refuses to rebase a
    branch Vetty has pushed to** ("edited by someone other than Dependabot"),
    so the post-merge conflict path for a Vetty-touched PR is
    `@dependabot recreate`. The recreation (down to 5 bumps — #353's lockfile
    regen had absorbed the rest) re-triggered the whole loop from PR-open:
    audit → align (esbuild bundle re-staled, recommitted) → self-supersede →
    re-audit → arm → queue → merged.
- Engine defects found live and fixed in stride: the per-node budget checks
  produced an unwrapped RuntimeError, so a budget death was naked back to
  JetStream — six ~40s resume/refail turns, each re-provisioning a sandbox
  (#361, sentinel Cause + code match in the runner carve-out). Budget 75m
  killed the go workload at its LAST node twice → 2h (2.6.1): the duration
  cap is a runaway guard, not a performance target.
- Fresh HOLD case validated fail-closed: one run's sandbox lost the nix
  substituter → deterministic verify failed → `hold_unstable`, arm refused
  ("verdict hold_unstable is not green") even though the agent's own
  host-toolchain checks were green. Correct behavior, environmental cause —
  board ticket filed (verify should distinguish env-provisioning failure).
- Still open: run cost reads `None` on all these runs (the delegate
  cost-signal chantier); the board-escalation lane (second death, same head,
  claim spent) armed twice but never had to complete — unit-tested, not yet
  live-proven.
- Lessons: the recovery loop turns "a required gate makes every death a trap"
  into "every death is one relaunch away from a verdict"; supersede-on-push
  means the FINAL head is always the audited one — accept the double pass as
  the price of that guarantee; cold nix caches are the real duration driver
  (30-46 min verify_build), fix the cache before tightening any budget.

## 2026-08-03/04 — first Dependabot batch on iterion itself: 2 exemplary audits, 1 silent death, 0 merges

- Status: **partial — the audits were the best this bot has produced; everything
  around them broke.** Fixes shipped as v2.6.0 + engine PR #357.
- Versions: bot 2.5.0 · iterion prod `:edge` (v3.24.0 era) · runs `019fc8e2`/
  `019fc8e5`/`019fc8ed` (PR-open) then `019fc924`/`019fc927` (post-align re-audits)
- Method: webhook-triggered on the 3 grouped Dependabot PRs of 2026-08-03
  (#353 studio npm ×29, #354 go ×15, #355 npm root ×33), `gate_context:
  revi/review` (required check + merge queue on main), `arm_automerge` NOT set.
- Result, per PR:
  - **#353 / #355 — audit exemplary, then stranded.** Tarballs extracted and
    content-scanned, sha512 byte-matched against the registry, SLSA provenance
    predicates decoded (the react org-rename and the radix "new releaser"
    maintainer-change flags each investigated and cleared, not waved through),
    CVE differential (dompurify net −14/+3, brace-expansion ×6 HIGH resolved),
    esbuild 0.26 parameter-property lowering aligned (`b2459da8` — the staled
    go:embed pi-ext bundle) and a workspace lockfile regenerated (`15823337`).
    The alignment push self-superseded the run (`overlap: supersede`) and the
    fresh runs re-audited the final head clean — the supersede property doing
    its job. Then: `revi/review=success`, PR mergeable… and nobody to enqueue
    it. `arm_automerge` was off on this repo, and would not have worked anyway
    (see misses).
  - **#354 — the silent-death case, end to end.** Usage window at launch →
    `run_retry_scheduled` (retrypolicy ✓) → resumed 36 min later → died on the
    bot's own `max_duration: 40m` mid-`security_audit` → `failed_resumable`
    that nothing resumes (the auto-retry covers usage windows only) → no gate
    status ever posted → required check absent → **PR silently unmergeable,
    indefinitely**. The MCP go-sdk 1.6.1→1.7.0 alignment (`TestManagerHealthCheckHTTP`,
    ping `_meta protocolVersion`) was never reached.
- Value: the supply-chain audits are production-grade — and the batch was a
  live probe of every seam around the audit, which is where all four defects
  sat.
- Findings / misses:
  1. Gate reconciler skipped ALL `failed_resumable` runs ("it will resume") —
     false for budget/exhausted/plain failures. The exact silent-block class
     the bko handover predicted.
  2. 40m/$15 budget too short for the heavy case (a CLEAN npm re-audit alone
     runs ~30m).
  3. `arm_automerge` was merge-now-first on CLEAN, which a merge queue rejects
     — the bot could never land anything on a queue-protected repo.
  4. Config: the iterion integration never set `arm_automerge: "true"` (bko
     had it), so green audits stopped at "ready".
- Engine hardening (PR #357 + bot v2.6.0): reconciler skips only ARMED
  retries and carries the death reason on the synthetic failure; a dead gate
  run relaunches its bot once per (PR, head) through the webhook tail; a
  second death files a deduped `source:gate-reconcile` board card; the retry
  sweeper republishes the outcome on permanent abandon; autofix ignores
  synthetic failures; arm-first automerge with pinned `enqueuePullRequest`
  fallback; budget 75m/$20. Adversarial review (opus) caught 2 real defects
  pre-merge — the reconciler standing down on its own synthetic marker (the
  board escalation was unreachable), and an abandon-failure republish loop.
- Lessons for next run: a required gate makes EVERY death loud or the lane is
  a trap — "resumable" must mean "something is actually coming back"; budget
  the outlier, not the median; on a queue repo, arming IS the merge path (the
  queue is the only door); an audit-side success is only half the loop — the
  merge side needs its own e2e proof per repo shape.

## 2026-07-31 — v2.5.0 on three heritage PRs, and the rebase that would have stalled every one of them

- Status: **validated, with one real gap found and closed.** Two of three PRs
  audited and merged by the bot; the third armed and is waiting on a rebase.
- Versions: bot 2.5.0 (first live runs of this version) · iterion
  `v3.17.1+d337007d1`, runner `sha256:9c7203cc`
- Method: `/vetty` on the three heritage PRs of `SocialGouv/buildkit-operator`
  — those opened under the OLD `github-actions[bot]` identity, before the
  dedicated Renovate App. #6 alone first (run `019fb413`), then #4 and #7
  together (`019fb6a0-7d9e` / `019fb6a0-77d9`).
- Result: #6 audited in 16m20s and **merged by the forge App at 17:46:27Z**,
  two seconds before the run ended, on the sha it audited (`ef333c66`). #4
  merged at 05:38:35Z. #7 posted `SAFE`, armed auto-merge (SQUASH, 05:37:59Z)
  and is not merged — see below.
- Cost: **~$1 per PR audited** ($1.31 for #6, $1.93 for the pair). Cheap enough
  that auditing on every dependency PR is not a budget question.

### Why they needed Vetty at all, having already been reviewed

All four heritage PRs carried `iterion/review=SUCCESS` — from **Revi**, a code
reviewer, reached by a maintainer's `/revi`. That is not the same question.
Revi reads a diff; Vetty asks whether the artifact on the other end of a
version bump is what it claims to be. Merging on Revi's verdict would have
skipped the supply-chain control this whole chain exists for, while looking
fully green.

### The audit is real, not a shape

For jq 1.8.1 → 1.8.2, Vetty established that the upstream release exists
(`jqlang/jq` tag `jq-1.8.2`, non-draft), that it was cut by the **same**
automation as 1.8.0 and 1.8.1 — no unexpected-author or compromised-maintainer
signal — that the locked nixpkgs commit exists, that its commit date matches
the lock's `last_modified` byte for byte, and that its `package.nix` declares
`version = "1.8.2"`. For helm 4.2.1 → 4.2.3: the derivation at commit
`7525d999` declares `4.2.3`, builds `helm/helm` at tag `v4.2.3` with a pinned
source hash, and the upstream delta is exactly 2 commits / 3 files matching the
release notes verbatim.

It also answered, with evidence, the open question Revi had left that morning
on #6 about whether `devbox.lock` came from a real nix resolution.

### The gap: a rebase silently ends the loop

`review_on_sync` was **off** on the integration, and Vetty's invocation is
`actions: [opened, reopened]`. So:

1. Renovate rebases a PR — routine, it happens whenever the base moves;
2. new head sha, and no bot relaunches ([webhooks_github.go](../../pkg/server/webhooks_github.go)
   filters a `synchronize` when `ReviewOnSync` is false);
3. `iterion/review` is **required** and is posted on the old sha;
4. the armed auto-merge can therefore never fire, and nothing says so.

Every Renovate PR that needs a rebase before it merges lands in this. The one
proven run (#15, 2026-07-29) merged before its base moved, which is why it was
never hit.

Established by reading the shipping code path and the integration config —
**not** yet observed live, because the two PRs that looked like they were stuck
in it turned out to be stuck on something else entirely (below).

Fixed by enabling `review_on_sync` on the integration — the pairing
[CLAUDE.md](../../CLAUDE.md) already prescribes for a required gate. Worth
knowing: `BotRule.Actions` is deliberately *recorded but not enforced*, with
`"synchronize" for the merge gate` named in its doc comment as the reason, so
the handler's reviewability gate stays authoritative and this wiring works.

### The real reason #5 and #7 were stuck: switching Renovate's identity orphans its open PRs

Chasing the rebase that never came produced a separate, more consequential
finding. Renovate at `logLevel=debug`, on both branches:

```
branch.isModified() = true
Branch has been edited but found no PR - skipping
```

The four heritage PRs were opened by the old `github-actions[bot]` identity,
before the dedicated `socialgouv-renovate` App. Renovate compares a branch's
tip author against its own git identity; the old one no longer matches, so it
reads the branch as edited by a third party. Worse, the second line: it does
not associate the open PR with the branch **at all**. Both branches are
invisible to it — never rebased, never updated, permanently stuck once they
conflict.

**Switching the identity a bot commits under orphans every PR it already has
open.** Nothing warns you: the PRs stay open, look normal, and simply stop
being maintained. Half of this batch (#4, #6) hid it by merging anyway — Vetty
merges through the forge, not through Renovate.

Remedy applied: close the orphans and delete their branches so Renovate
re-proposes the updates under the current identity. The condition is a one-time
migration artifact — PRs opened by the new App are recognised normally — but it
is worth planning for whenever a bot's credentials change.

Closing carried a real risk, since Renovate normally reads a closed PR as a
rejection. It did not fire here: the next debug run lists `kubernetes-helm`
among its `33 flattened updates` and processes the branch again. The update was
never suppressed — it is held as `"pendingVersions": ["4.2.3"]` by the repo's
own 14-day cooldown. Which reframes #7 entirely: it existed only because it was
opened *before* the cooldown landed (2026-07-28). Closing it aligned the repo
with the policy it had since declared, rather than contradicting it.

### A correction on how this was measured

An earlier reading of the Renovate dry-run log claimed a real run would open
eight new pull requests. It opened **zero** — three consecutive real runs did.
`DRY-RUN: Would commit files to branch X` is not a PR: with
`internalChecksFilter: strict`, Renovate creates the branch and holds the PR
until `minimumReleaseAge` matures. Even `Would create PR` in a dry run is not a
prediction — the dry run evaluates from a clean slate, while a real run
re-checks the pending state against an existing branch. The cooldown was
working exactly as designed throughout, and the log lines were read as
something they do not say.

The practical consequence: **the loop cannot be exercised on demand.** Forcing
a PR would mean defeating the cooldown, which is the one control here worth
least defeating. End-to-end validation waits for an update to mature; the
audit-and-merge half is provable any time with `/vetty` on an open PR.

### Second observation: a batch of PRs touching one lock file serializes badly

The three PRs all bumped `devbox.json` + `devbox.lock`. #6 and #4 merged; #7
was audited and armed, then turned `CONFLICTING` because the two merges landed
under it. Vetty was right — it armed rather than forcing a merge — but the
outcome is a PR that now needs a Renovate rebase to move, and (before the fix
above) would have waited forever for it.

### Lessons for next run

1. **Reviewed is not audited.** A dependency PR carrying a green review from a
   code reviewer has answered a different question. Do not let a shared gate
   context blur the two.
2. **Arming auto-merge is a weaker guarantee than merging.** The `merge_now`
   path pins `expectedHeadOid` to the audited sha; `enablePullRequestAutoMerge`
   pins nothing, so GitHub would merge whatever the head becomes. What restores
   the guarantee is the *required* gate — provided it re-lands on each new
   head, which is precisely what `review_on_sync` buys. The two settings are
   one mechanism, not two options.
3. A batch of dependency PRs touching the same lock file will conflict with
   each other. Expect the tail of a batch to need a rebase round.

## 2026-07-29 — v2.4.0: the loop closed — Renovate → audit → gate → merge, unattended (run 019faef9)

- Status: **VALIDATED** — the first dependency PR to travel the whole chain
  with no human in it.
- Versions: bot 2.4.0 · iterion `v3.15.0+3a61d2da4` (runner `:edge`
  `sha256:8c625432`)
- Method: a real `renovate.yml` dispatch on `socialgouv/buildkit-operator`,
  authenticated as the dedicated `socialgouv-renovate` App.
- Result: PR #15 (`go toolchain 1.26.5 [security]`) created **17:43:03Z**,
  merged **17:58:04Z** by the forge App — **15 minutes** from Renovate opening
  it to the merge, with no human in between. `armed: true, reason: merged: the
  forge reported every required check already green`. Gate
  `iterion/review=success` posted on the head at 17:57:58Z; merge commit
  `6d02f3e46747`.

### What each link proved

- **The App switch is what unblocked everything.** Under `GITHUB_TOKEN` the
  four pre-existing Renovate PRs each had a `ci` run stuck in
  `action_required` with zero jobs — GitHub's anti-recursion rule. PR #15's
  `test`/`lint` started on their own. Nothing downstream can work without
  this, and no amount of bot logic substitutes for it.
- **The cooldown holds without hiding.** The dry run showed 17 upgrades marked
  `pendingChecks: true` with their held versions named (`undici` 8.8.0/8.9.0,
  …), while aged updates proceeded. `internalChecksFilter: strict` means the
  branch is not created at all rather than a PR opened with a pending check.
- **The merge targets the audited commit.** `commit` reported
  `committed: false` (nothing to align), so the pin fell back to `prepare`'s
  head — and the merge went to exactly that sha.

### The overclaim the run surfaced

The check displayed *"supply-chain audit clean; alignment committed, build
verified"* on a PR where the alignment was a no-op (1 commit, 1 changed file,
all Renovate's). The verdict is a graph PATH name stamped per-edge, so it read
`committed` whether or not anything was.

The first fix carried the commit agent's own `committed` flag down to the
message — which only moved the claim from one unreliable source to another,
since that flag is the agent grading its own work. The shipped version derives
it from two shas the run owns (`commit.sha` vs `prepare.head_sha`) and routes
to the `clean` verdict, which existed in every string table and was
unreachable. All the "committed" strings are now unconditionally true.

A required check that asserts work nobody did is the same false-statement
class this bot exists to catch in other people's diffs.

### Lessons for next run

- A PR whose base has moved far enough to conflict can never reach the merge:
  cancel the audit rather than spend it on a refusal that is already knowable
  from `mergeable`. Cost saved on this session: one 14-min run.
- Closing a stale Renovate PR is not a neutral cleanup — Renovate reads it as
  "this update was rejected" and stops offering it. Leave them; the bot
  rebases them under the new identity.

## 2026-07-29 — v2.1.0 live: the whole chain ran, the gate landed, and the merge never happened (run 019faad2)

- Status: **partial** — every step validated end to end except the last one,
  which turned out to be structurally impossible as designed.
- Versions: bot 2.1.0 → 2.2.0 · iterion `9d5efc6c` (runner image `:edge`
  `sha256:c499ba03`)
- Method: `/vetty` on
  [socialgouv/buildkit-operator#5](https://github.com/SocialGouv/buildkit-operator/pull/5)
  (a `golang:1.26` digest bump), cloud run on `ovh-prod`, sandbox
  `iterion-sandbox-sec:edge`, `gate_context: iterion/review`,
  `arm_automerge: true`.
- Result: finished in **14 min**, 15 nodes, no human intervention. `prepare` →
  `security_audit` → `align` → `align_gate` → `verify_build` (5 min) →
  `verify_run` **exit 0** → `validate_gate` → `commit` → `post_feedback` →
  `feedback_health` → `arm_automerge` → `done`.
- Value: **the merge gate landed for the first time** —
  `iterion/review=success` on the head SHA, posted through the server's
  publish endpoint. That link had never worked before (see the 401 below).

### The gate: a redirect was degrading the POST

`post_feedback` had been failing with `401 authentication required` on a route
that is deliberately auth-exempt. Everything else had been eliminated with
evidence — the route answers a bogus token differently, the URL and token were
correct in the run inputs, the same request reached the handler by hand from
both a laptop and a runner pod, Revi's own gate still worked. The remaining
hypothesis was that `urllib` follows redirects, and a redirected POST becomes a
GET, which misses a method-specific Go route and falls through to the auth
middleware.

Refusing the redirect fixed it. The value of the fix is not only that it works:
it **names the URL it called and the one the server redirected to**, so the
next occurrence needs no investigation.

Lesson: an unexplained `401` on an auth-exempt route is worth suspecting the
*shape* of the request before its credentials.

### `arm_automerge` armed nothing, and could never have

```
armed: false
reason: auto-merge request refused: [{'type': 'UNPROCESSABLE',
  'message': 'Pull request Pull request is in clean status'}]
```

`enablePullRequestAutoMerge` only accepts a PR that still has something to wait
for. The audit takes ~14 min and CI ~3 min, so **by the time the bot decides,
the PR is always already green** — the arm always fails. The feature was not
merely buggy on this PR; as shipped it would have merged nothing, ever, on any
repo whose CI is faster than the audit. Which is every repo.

v2.2.0 merges through `mergePullRequest` pinned with `expectedHeadOid` when the
forge itself reports the PR `CLEAN` and `MERGEABLE`. The invariant is
unchanged — the bot never decides that checks passed, it only acts on the
forge's own answer — but the guarantee is now "never merges past a check"
rather than "never merges".

Lesson: **a capability that only fires in a state your own latency prevents is
dead code with a green test.** The unit test passed because it stubbed the arm
call as succeeding; nothing modelled the state the real API is in when the bot
actually calls it.

### Other engine defects this run surfaced

- **Plugin-source checkout race** (fixed): `git init` creates `.git` before the
  fetch lands, and the fetcher treated `.git` as "tree complete". Five launches
  hitting a freshly rolled pod at once left one of them with an empty
  directory, and the loader reported it as *"has no plugin.yaml"* — a 502 that
  names the wrong cause, and a run row left `queued` forever with no error on
  it. The checkout is now staged and renamed into place.
- The status description read `no blocking findings (≥verdict)` — the shared
  phrasing assumes a severity floor, while this gate turns on the audit
  verdict. Now written per verdict.

### Lessons for next run

- A green CI image build is **not** a deployment: iterion's CI separates the
  build from the `finalize` job that re-tags `:edge`. Poll until the published
  digest *changes*, then grep the fix inside the pod, and only then launch. I
  lost a run to trusting a green workflow.
- The dogfood cost stayed at ~$0.60 for a digest bump with a real
  two-image trivy delta. The audit is not where the time goes — `verify_build`
  is (5 min of the 14).

## 2026-07-28 — v2.1.0: the classifier was auditing empty diffs, and the gate could never work (no run; defects found by inspection + adversarial review)

- Status: **partial** — code validated locally and by review; no live cloud run
  yet (the target repo is not in the forge App's installation scope, which
  needs an Organization Owner).
- Versions: bot 2.0.0 → 2.1.0 · iterion PR #306 (7 commits)
- Method: wiring Vetty onto `socialgouv/buildkit-operator`'s Renovate PRs
  end-to-end (audit → align → verdict → gate → auto-merge). Deterministic
  nodes exercised against real fixtures; Revi reviewed the branch.

### What the attempt actually found

The bot did not fail loudly on this repo — it would have reported **"safe" on
three of the four open Renovate PRs without reading anything**. `prepare`
only recognised package manifests, so a PR moving a Dockerfile digest, a
`devbox.json` pin or a `Taskfile.yml` tool matched nothing. Crucially it did
not stop: `is_empty` means "no files changed", not "no manifest matched", so
the run continued and handed the auditor an empty `bump_summary`. Verified on
the real `renovate/golang-1.26` branch: 3 Dockerfiles, a 1684-char diff where
the old classifier produced an empty string.

Lesson worth keeping: **a scope flag and a coverage flag are not the same
flag.** Conflating them is what turns "we found nothing to look at" into "we
looked and found nothing".

### Engine defects this surfaced

- `review_on_sync` was **unreachable** — absent from the webhook API request
  type (a PATCH carrying it returned 200 and changed nothing) and never set by
  provisioning, so it could only ever be false in production. Since a commit
  status lives on one SHA, that made every merge gate self-defeating: the
  status went absent from the head after any push. Observed live on
  SocialGouv/iterion#300 (20 checks green, PR blocked). Now derived from the
  declared `statuses` scope.
- A PR event could only launch **one** bot, via a hardcoded fallback, and the
  shared author allowlist was nil'd as soon as one co-enabled bot was open —
  so a dependency guard co-enabled with a reviewer was silently dropped along
  with its author filter.
- Author routing read the event **sender**, not the PR author, so a human
  pushing a fix onto a dependency PR handed it to the wrong bot.

### The adversarial review earned its keep

Revi found four defects on the branch, two of which would have shipped broken:

- `arm_automerge` sent **syntactically invalid GraphQL** (a sigil swap that
  also rewrote GraphQL's own separators). The feature was dead, and the test
  certified it green — the stub answered success to any body. The fix now
  includes a stub that rejects what the API would, verified by reintroducing
  the bug and watching the test fail.
- The `ReviewOnSync` derivation ran only on a *fresh* provision, while the
  already-provisioned repo — the production case — hits the idempotent
  short-circuit. The fix fixed nothing that was already deployed.

Lesson: **a test whose stub accepts anything certifies nothing.** Both of
those were green in CI and broken in production; the pattern is a test that
asserts on what we sent rather than on what a real peer would accept.

### Lessons for next run

- Point it at an npm/pypi PR: live validation is still Go/Docker-only.
- The gate context must be pinned per repo (`launch_vars.gate_context`), the
  same value on every bot that can gate — a per-bot required check deadlocks
  whichever PRs that bot does not review.
- `arm_automerge` is only safe on a repo that requires at least one check;
  with none, a forge merges an armed PR immediately.

## 2026-07-14 — cloud run on a real Dependabot go-minor-patch PR (#182); excellent audit/align/verify, wired via `/vetty` command (run 019f60cf)

- Status: **VALIDATED (cloud, real forge PR)** — Vetty ran end-to-end on a live
  Dependabot PR ([#182](https://github.com/SocialGouv/iterion/pull/182),
  go-minor-patch, 10 modules incl. x/crypto, mongo-driver, aws-sdk, go-selfupdate)
  and produced a correct verdict. The `post_feedback` comment step failed the first
  time on the Anthropic forfait's **session rate-limit** (`failed_resumable`); a
  fresh token + `iterion remote runs resume` finished it.
- Versions: bot dep-update-guard v2.0.0 · iterion runner `:edge` (this session's
  `:edge`, digest 42665a30, i.e. incl. #178/#180/#184/#185) · claude_code backend on
  claude-opus-4-8 via the Anthropic OAuth forfait.
- Method: **wired the bot** via `POST /api/teams/{id}/forge/repo-bots`
  (`bot_ids:[dep-update-guard]`, GitHub App `iterion-forge-61934180[bot]`, forge_token
  `forge_github_f73ba902`) → registers the `pull_request` + `pull_request_comment`
  webhook and the `/vetty` command. Triggered deliberately with
  `gh pr comment 182 --body "/vetty"` (routes only to dep-update-guard; the comment
  gate checks the commenter's CollaboratorPermission).
- Result: converged. audit ($0.87) → align (no changes; all minor/patch, no breaking
  API in-tree) → verify_build ($1.12) wrote an out-of-tree `verify.sh`
  (`go build -mod=vendor ./...` + a vendor-drift `go mod vendor` + `git diff --quiet`
  check) → verify_run gate GREEN → verdict "safe, no alignment, nothing pushed".
- Value: a genuine supply-chain audit — queried the **OSV API per package**, correctly
  identified that the x/crypto bump *resolves* CVE-2025-58181/47914, flagged a
  version discrepancy in the PR description, and reasoned correctly about the
  blocking criteria (not a new HIGH/CRITICAL → don't block). This is the reference
  "dependency-PR guard" behaviour working on a real PR.
- Findings / misses:
  - **#1 (FIXED, engine)** — the skill mirror produces the CC-2.x directory form
    `.claude/skills/<name>/SKILL.md`, but Vetty's prompt (and ~8 other catalog bots)
    Reads the flat `.claude/skills/<name>.md`. The Read failed twice + cost a
    filesystem `find` before recovering from the baked `/opt/iterion/bots/...` copy —
    a recovery **absent on a non-iterion target repo**. Fix: `mirrorFileSkill` now
    writes the flat alias too (PR #187, `pkg/runtime/bundle.go`).
  - **#2 (FIXED, security)** — the forge integration auto-launched improve/review
    bots on *every* PR incl. fork PRs (untrusted code + budget-exhaustion vector) and
    dependency PRs. Fork PRs are now never auto-launched (a repo collaborator triggers
    manually via `/command`, gated on CollaboratorPermission); dep-bot PRs never route
    to the improve loop (PR #189, `pkg/server/webhooks_{github,common}.go`).
  - **#3** — `verify_build` is slow (~17 min) on a cold devbox `go build`; ~$2 total
    run cost. Acceptable; a shared go/devbox cache (ADR-066-bis) would help.
  - **#4** — the Anthropic forfait has a **~5h session rate-limit**; a long run + many
    same-session runs exhaust it and the *last* node (`post_feedback`) failed
    `rate_limited`, losing an otherwise-complete verdict until resume. **#4b
    (follow-up):** make `post_feedback` a DETERMINISTIC tool node (compose the comment
    from the structured verdict + POST via the forge REST API) so it needs no LLM turn
    — resilient to rate-limits, faster, cheaper.
- Engine hardening: PR #187 (skill mirror flat alias) + PR #189 (fork/dep-bot webhook
  guards) — both dogfood-driven.
- Lessons for next run: keep triggering via `/vetty` (controlled, one bot, authz-gated)
  rather than the auto pull_request path; provision a FRESH forfait token before a long
  run (the access token is short-lived and the session limit is real); land #4b so a
  rate-limit at the comment step can't sink a good verdict.

## 2026-07-10 — first CLOUD runs on a real Dependabot PR: HOLD verdict with a real CVE finding, then safe re-verdict (runs 019f4ba8 / 019f4bcb / 019f4d3b)
- Status: **VALIDATED (cloud, real forge PR)** — the two paths the 07-07 bilan
  asked for both ran live: a real Dependabot PR
  ([#80](https://github.com/SocialGouv/iterion/pull/80), go-minor-patch, 22
  modules) and the real `post_feedback` forge POST, verdict comments posted
  under the App identity `iterion-forge-83fde406[bot]` with re-fetch verify.
- Versions: bot v2.0.0 · iterion `499957c31`→`6dd452c2a` (fixes landed mid-session).
- Method: `/vetty` PR comment → webhook command route (`scope: pr`, mode
  direct) → cloud runner (no sandbox, devbox image). ~$1.6/run, 5 min.
- Result & value: run 019f4bcb produced an exemplary **HOLD**: OSV batch over
  all 30 bumped (name, version) pairs → zero malicious/typosquat; the vendored
  wails `package.json` bump audited for lifecycle hooks (devDependencies only);
  **and the real finding — the PR bumps `x/crypto` 0.50→0.51 while the fix for
  7 CRITICAL + 2 HIGH SSH/agent advisories is 0.52.0, one minor short** — so
  the guard's tie-break (unsure → suspicious, a hold is cheap) fired exactly as
  designed. After Billy pushed the 0.52.0 bump, run 019f4d3b re-audited
  (osv-scanner v2.4.0 over all 134 pinned packages) and returned the clean
  ✅ safe/aligned verdict with an honest `committed=false, no alignment needed`.
- Findings / misses (engine, not bot): run 019f4ba8 no-op'd (`is_empty: true`)
  because the PR-comment command path launched on the DEFAULT branch — the
  `issue_comment` payload carries no head ref. Fixed in-session:
  `499957c31` resolves the PR head/base via the forge API at command time
  (failure = loud 502, closed PR = filtered). Without local scanners on the
  runner image the audit adapted (OSV REST batch), matching the
  skill-not-DSL universality contract; osv-scanner appeared in the later run.
- Lessons for next run: point it at an npm/pypi dep PR to exercise non-Go
  ecosystems; consider shipping osv-scanner in the runner-devbox image so the
  floor doesn't depend on the agent installing it.

## 2026-07-07 — first live dogfood: clean-bump path end to end, audit evidence exemplary (run 019f3d73)
- Status: **VALIDATED** (no-sandbox variant, clean-bump path) — every stage behaved with the exact honesty the v2 contract demands; the with-alignment path and a real forge POST remain to be exercised on a live PR.
- Versions: bot v2.0.0 · iterion `dev+239203525cc8`.
- Method: CLI run FROM the PR-branch checkout of a Go fixture (bare origin + `dependabot/go_modules/...` branch bumping github.com/google/uuid 1.5.0→1.6.0), `--sandbox none` (sec image blocked by native:221edac8), pr_url empty (no forge), `--max-cost-usd 12`. ~11 min wall.
- Result: `finished`. prepare (deterministic) correctly scoped go.mod+go.sum; **security_audit verdict=safe with model evidence**: govulncheck actually RUN (no reachable vulns), OSV API queried for BOTH versions **with a query-shape control against a known-vuln package**, go.sum hashes checked against sum.golang.org's transparency log, and the absent image scanners honestly listed `not_available` ×3. align: `applied=false` with proof (`NewString()` stable; build+vet run) — no invented edits. Deterministic verify_run: real exit 0 (build+vet against the bumped dep). validate_gate stable → commit node: `committed=false, "no alignment needed"` — the honest no-op. post_feedback skipped (`no pr_url`, posted=false, never pretended); feedback_health degraded=false.
- Value: the v2 calibration is vindicated live — the read-only audit stage produced real, verifiable evidence, and the deterministic verify (which replaced the self-reporting v1 validate) gated on a real exit code.
- Findings / misses: none on the bot. The earlier sandboxed attempt (019f3d56) fell to native:221edac8 (in-container stream zero-byte + subprocess leak) and to an operator docker-exec cleanup that killed the container — recorded there.
- Lessons for next run: exercise (a) a breaking bump so align/commit actually land code on the PR branch, and (b) a real forge PR so post_feedback's REST + re-fetch verify path runs; both ideally back in the sec sandbox once 221edac8 lands.

## 2026-07-07 — converted to v2 calibrated shape (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — no live run yet in ANY shape (this file is new); structural validation only (`iterion validate` clean, catalog tests green).
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07).
- Shape: the LLM `validate` self-report was replaced by the deterministic verify_build/verify_run gate (fail-closed — no verify.sh ⇒ no commit); the read-only security_audit DELIBERATELY stays separate from the mutating align (anti-prompt-injection separation behind a deterministic verdict gate), and commit-after-green stays (shared PR branch). align gained the G5 pre-existing-failure protocol; the dead pr_review_mode var is gone.
- Next: first live dogfood on a real Dependabot/Renovate PR + bilan here.
