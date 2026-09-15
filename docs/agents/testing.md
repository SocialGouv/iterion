# Testing patterns — the helpers, and the traps that ejected PRs

How tests are written here, and the four failure modes that cost real merge-queue
time: a git subprocess that outlives its temp dir, a run whose workspace is the
operator's own checkout, Mongo conformance that only runs in CI, and a UI suite
that touches the operator's store.

## Testing Patterns

- `tmpStore()` — creates temp directory-backed RunStore for test isolation
- `compileFixture()` — loads and compiles .bot files from `examples/` directory
- **Scenario executor** (`e2e/e2e_test.go`) — configurable stub with `.on(nodeID, handler)` for per-node behavior
- **Every git subprocess a test spawns goes through [`internal/gittest`](../../internal/gittest/gittest.go)**
  (`Run` / `Try` / `Cmd`, `SourceRepo`, `RemoveWorktree`) — not a style
  preference, two defects: (1) since git 2.48 a writing command detaches
  `git maintenance run --auto`, which keeps writing under `.git/objects`
  after `CombinedOutput` returned and races `t.TempDir()`'s removal (that
  ejected PRs from the merge queue, #821/#828), and (2) the operator's
  `~/.gitconfig` otherwise decides whether a test passes (a global
  `commit.gpgsign` hangs every fixture commit on a pinentry with no TTY).
  The helper bakes both in; `pkg/git.TestEveryTestGitCallerDisablesAutoMaintenance`
  sweeps `_test.go` and fails a new site that assembles its own argv.
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
- Table-driven subtests with standard `testing` package
- `task test:live` — runs E2E with real Claude/Codex CLIs (requires API keys)
- **Mongo conformance locally** — the `mongo-conformance` CI job is reproducible with one `mongo:8.0` replica-set container on port 27018 (`--ulimit nofile=131072:131072`, member advertised as `localhost:27018`); recipe + the two traps in [docs/development.md](../development.md#running-the-mongo-conformance-harness-locally). A store-twin or conformance-row change is unverified until it ran there
- **Studio UI e2e** (`studio/e2e/`, `task test:e2e:ui`) — Playwright against the
  REAL server: the built binary serving the embedded SPA over a throwaway
  workspace `studio/e2e/serve.mjs` rebuilds per run and seeds with genuine
  artifacts (runs the engine actually executed from tool + compute fixtures, so
  no LLM credential and no network; a board card created through the CLI).
  Bootstrapped by the e2e-coverage bot's V4 dogfood (see
  [docs/bot-runs/e2e-coverage.md](../bot-runs/e2e-coverage.md)).
  `ITERION_HOME`/`HOME`/`ITERION_SECRETS_KEY` are redirected into that
  workspace, so the suite never touches the operator's own store, secrets or
  keychain — a rule any new spec must keep. Specs assert **rendered content and
  interactions**, never bare HTTP status codes, and take the store/filesystem as
  the oracle for anything the UI writes. Not in the blocking CI job: the browser
  download is opt-in (`task test:e2e:ui:install`) and the target skips cleanly
  without it. New specs go in `studio/e2e/specs/`; shared seed metadata is read
  through `studio/e2e/lib/state.ts`. Because one server and one store are shared
  by the whole suite (workers: 1), a spec that mutates state must leave it in a
  shape the others tolerate — see the board spec's note on not parking its card
  in a dispatcher-claimable column.
- **E2E coverage matrix** ([docs/e2e-coverage-matrix.md](../e2e-coverage-matrix.md))
  — the single feature×coverage inventory (one row per operator-observable
  promise, every row terminal or an honest `uncovered` gap; every `covered-*`
  row cites the test that proves it). Maintained by the **e2e-coverage bot
  (Endy)**, whose deterministic gate parses the file and grep-verifies every
  claim — when you add a feature or an e2e test, update the matching row (or
  run Endy scoped to the family). Contract:
  [bots/e2e-coverage/skills/coverage-matrix.md](../../bots/e2e-coverage/skills/coverage-matrix.md).
- **Bot golden replay** (`pkg/botreplay/`, `task test:goldens`, wired into `check`) — freezes a bot's LLM node output as a committed fixture under `pkg/botreplay/testdata/bot-goldens/<bot>/<scenario>.json` and re-validates it against the current schema + invariants (required-field presence, no hallucinated assignees) with no API calls. Record mode (`task test:goldens:record`, build tag `goldens_record`) hits the real LLM to (re)generate fixtures — impractical for the v2 `campaign` nodes (whole-session claude_code agents), whose fixtures are hand-authored seeds frozen on the termination-contract schema. Wired scenarios: feature-dev `campaign_feature_complete`, docs-refresh `campaign_docs_aligned`, whats-next `nexie_turn_basic`. See [docs/adr/008-bot-golden-replay-framework.md](../adr/008-bot-golden-replay-framework.md).

