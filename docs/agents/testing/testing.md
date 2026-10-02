# Testing patterns — the helpers, and the traps that ejected PRs

How tests are written here, and the four failure modes that cost real merge-queue
time: a git subprocess that outlives its temp dir, a run whose workspace is the
operator's own checkout, Mongo conformance that only runs in CI, and a UI suite
that touches the operator's store.

**Before anything else, read [The proof discipline](#the-proof-discipline)
below**: which layer proves your change, and when a session is expected to
spend LLM money to find out.

## The proof discipline

There are two layers, and the whole discipline is knowing which one your
change needs.

| layer | proves | cost | runs |
|---|---|---|---|
| **deterministic** | the real seams, credential-free, stub executor | free | **every push and PR** (`go test ./e2e/...` + `-race`, `mongo-conformance`, `nats-conformance`, `cloud-e2e` on kind) |
| **live** (`-tags live`) | what a real harness actually does | real LLM spend | only when a human or an agent asks — **no CI job carries the tag** |

The free layer carries **361 of the 387 rows** of
[`e2e-coverage-matrix.md`](../../e2e-coverage-matrix.md); only **7** are
`covered-live`. So the cheap reflex covers almost everything, and the paid one
stays affordable because it is rare and targeted.

### What a session owes, by what it touched

1. **Always** — `devbox run -- task check` (lint + the free layer). Not
   negotiable, not a judgement call.
2. **Touched a core surface** — add or adjust the **deterministic** row first.
   A capability reachable without a credential belongs in the free layer;
   moving it to `live` because that was quicker to write is how a suite
   becomes unaffordable to run.
3. **Touched something only a real model can prove** — a backend's behaviour,
   a bot's judgement, structured output on a real harness, the permission gate
   at `ask`, a session resume — then **run the matching live target**, and say
   in the PR which one and what it cost:

   ```bash
   devbox run -- task test:live:bot:review-pr     # one bot
   devbox run -- task test:live:feat:permission   # one feature
   devbox run -- task test:live:quality:unit      # free: the snapshot engine
   ```

   Never the `*:all` aggregates, and never wired into blocking CI
   ([cost discipline](../../live-e2e-coverage.md#cost-discipline)).
4. **No live target covers what you changed** — that is the interesting case.
   Write one, or **adapt the nearest**, in the same change. A live layer that
   never grows with the engine is a live layer that slowly stops proving
   anything.
5. **A live target fails** — it is a **finding, not a chore**. File it under
   the epic it belongs to. A live test *relaxed* until it passes again is a
   regression that got away; say so in the ticket, not in the diff.

### Deciding to spend

The spend is real, so it is arbitrated, not assumed:

- **Say what it will cost before running it** — per-target estimates live in
  each test's doc comment and in its Taskfile `desc`.
- **Prefer one target to a sweep.** Bounded and repeatable beats exhaustive
  and unaffordable.
- **Record the result** — the last-green ledger
  ([#1422](https://github.com/SocialGouv/iterion/issues/1422)) exists so the
  next session can answer "does this still pass?" for free. Until it lands,
  say it in the PR.
- **If the budget says no, say no in writing.** "Not re-proven, would cost
  ~$X" in the PR is honest and actionable. Silence reads as "proven" to every
  later reader, which is the one thing it never means.

### The trap this repo has already paid for

More tests is not more proof. A test exercises the **site**, not the
guarantee; a stub that accepts anything certifies nothing; a guard that
enumerates spellings never converges. **Name a test for what its mutation
reddens** — if no mutation separates the elaborate version from the simple
one, ship the simple one. A matrix row added without any mutation going red
has cost money and bought nothing.

Full reference for the live layer (harness, judge panel, env knobs, authoring
a new target): [`docs/live-e2e-coverage.md`](../../live-e2e-coverage.md).
Chantier and current state: [epic #1421](https://github.com/SocialGouv/iterion/issues/1421).

## Testing Patterns

- `tmpStore()` — creates temp directory-backed RunStore for test isolation
- `compileFixture()` — loads and compiles .bot files from `examples/` directory
- **Scenario executor** (`e2e/e2e_test.go`) — configurable stub with `.on(nodeID, handler)` for per-node behavior
- **Every git subprocess a test spawns goes through [`internal/gittest`](../../../internal/gittest/gittest.go)**
  (`Run` / `Try` / `Cmd`, `SourceRepo`, `RemoveWorktree`) — not a style
  preference, two defects: (1) since git 2.48 a writing command detaches
  `git maintenance run --auto`, which keeps writing under `.git/objects`
  after `CombinedOutput` returned and races `t.TempDir()`'s removal (that
  ejected PRs from the merge queue, #821/#828), and (2) the operator's
  `~/.gitconfig` otherwise decides whether a test passes (a global
  `commit.gpgsign` hangs every fixture commit on a pinentry with no TTY).
  The helper bakes both in; `pkg/git.TestEveryTestGitCallerDisablesAutoMaintenance`
  sweeps `_test.go` and fails a new site that assembles its own argv.
- **Run tests with devbox's pinned Git.** The assistant's attested authoring
  commits use guarded ref transactions, which need **Git ≥ 2.46**.
  `devbox.json` pins Git 2.55.0 and `devbox.lock` resolves it for each platform;
  check the effective executable with `devbox run -- git --version`.
  Running outside devbox can still pick an older host Git and fail with
  *"attested authoring commits require Git 2.46 or newer for guarded ref
  transactions"*. Re-enter devbox before diagnosing those failures as a
  regression in the code under test.
- **A run's workspace must be a repository the TEST owns.** An engine built
  without `WithWorkDir` defaults to `os.Getwd()` — the package directory,
  inside the developer's checkout — so `worktree: auto` (the IR default)
  registers the run's worktree in the REAL repository and the registration
  outlives the `t.TempDir()` that held the checkout (#870: 1 773 dead
  entries / 2.5 GB measured). Pass `gittest.SourceRepo(t)` (e2e goes through
  `newEngine`), or `t.Chdir` into one where the CLI takes its workspace from
  the cwd. `gittest.NoWorktreeLeaks(m)` in `TestMain` (e2e, `pkg/runview`,
  `pkg/cli`) fails the suite naming the test behind any entry left behind,
  and reclaims it **by recorded path** — never `git worktree prune`, which
  would also drop an operator's checkout on an unmounted volume.
- **A test never touches the operator's iterion home.** Under `go test`,
  `store.IterionHome()` never resolves to `~/.iterion`: without
  `ITERION_HOME` it is a directory the test process created. A package whose
  tests reach the iterion home — run stores, installed plugins, the global
  runs view, the cross-store proxy — wraps its `TestMain` in
  `hometest.Isolate` ([`internal/hometest`](../../../internal/hometest/hometest.go)),
  which points `ITERION_HOME` at a directory it removes at exit and fails the
  suite when one is left behind (#2016: 130 966 test run stores had piled up
  in `~/.iterion/projects`). Readers of the home resolve it through
  `store.IterionHome()` like the writers do, never `$HOME/.iterion` by hand;
  a test of the no-home paths calls
  `store.ResolveIterionHomeAsInProductionForTests`. One deliberate exception:
  the `live` e2e tests keep their run store in `~/.iterion` so the studio
  shows them (`ITERION_TEST_STORE_DIR=workspace` isolates them). A server a
  test starts and moves to another project (`swapWorkDir`) is shut down in
  `t.Cleanup` (`shutdownOnCleanup` in `pkg/server`): its assistant sweep
  otherwise re-creates the removed per-project store every 10 s. Likewise an
  engine built without `WithWorkDir` works in a throw-away directory that
  only `Run` and `ResumeWithHostInputs` release — a helper that returns
  before the run is done never removes it, and `pkg/runtime`'s `TestMain`
  fails the suite on one left behind.
- Table-driven subtests with standard `testing` package
- **Mongo conformance locally** — one `mongo:8.0` replica-set container on port 27018, launched exactly as the CI job does; recipe and the two traps in [docs/development.md](../../development.md#running-the-mongo-conformance-harness-locally). A store-twin or conformance-row change is unverified until it ran there.
- **Studio UI e2e** (`task test:e2e:ui`) — Playwright against the REAL server, seeded with genuine engine output (no LLM credential, no network); bootstrapped by the e2e-coverage bot's dogfood ([docs/bot-runs/e2e-coverage.md](../../bot-runs/e2e-coverage.md)). `ITERION_HOME`/`HOME`/`ITERION_SECRETS_KEY` are redirected into the throwaway workspace — a rule any new spec keeps. Specs assert rendered content and interactions, the store being the oracle for what the UI writes. The browser download is opt-in (`task test:e2e:ui:install`); workers: 1, so a spec that mutates state leaves it in a shape the others tolerate.
- **E2E coverage matrix** ([docs/e2e-coverage-matrix.md](../../e2e-coverage-matrix.md)) — the feature×coverage inventory the **e2e-coverage bot (Endy)** grep-verifies; when you add a feature or an e2e test, update the matching row. Contract: [bots/e2e-coverage/skills/coverage-matrix.md](../../../bots/e2e-coverage/skills/coverage-matrix.md).
- **Bot golden replay** (`task test:goldens`, wired into `check`) — freezes a bot's LLM node output as a committed fixture and re-validates it against the current schema + invariants with no API calls; record mode hits the real LLM (ADR-008).
