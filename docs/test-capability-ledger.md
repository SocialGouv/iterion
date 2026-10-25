# Test capability ledger

Generate the skip and sleep census from the working tree, without running tests
or calling a service:

```sh
devbox run -- task test:capabilities
# Also observe literal prerequisites on this machine (optional):
devbox run -- task test:capabilities -- -observe
# Choose another output directory:
devbox run -- task test:capabilities -- -out /tmp/my-test-census
```

The generator is [`scripts/ci/testcaps`](../scripts/ci/testcaps/main.go).
It writes `.iterion/test-capabilities/census.json` and `summary.md`. They are
build artifacts, not a manually maintained inventory. The JSON has
`schema_version: 1`, counts by reason/class/scope and one row per call site:
repository-relative file, physical line and column, enclosing function,
explicit build constraint, original expression and the analysis result.
Paths and counters are sorted; there is no timestamp, absolute checkout path,
random identifier or machine observation in these two files. The same source
bytes generate identical bytes, including from another checkout location.

## Source boundary and skip reasons

`git ls-files --cached --others --exclude-standard -z` supplies tracked and new
nonignored Go sources. Deleted paths, `vendor/` and `testdata/` are excluded;
`testdata/` contains synthetic Go source, including deliberately invalid input.
All build tags and platform suffixes are scanned without executing or loading
packages. `third_party/` is retained as a separate scope; `_test.go` is `test`,
and other first-party Go files are `support-or-production` (including shared
test helpers). Neither third-party code nor production sleeps are silently
counted as first-party test sites.

The AST recognises direct `Skip`, `Skipf`, `SkipNow` calls on standard
`testing.T`, `B`, `F` and `TB`, including import aliases, local receiver aliases
and same-file fixture fields. Comments and quoted source are not calls. This
is a syntax census, not whole-program type or data-flow analysis: cross-file
receiver type aliases, user-defined wrappers and calls through function values
are outside its resolution. Function names identify the enclosing test/helper;
computed subtest names are not guessed.

Each skip carries the message or format string and its full call expression.
`named` means a nonblank constant message; `missing` names `Skip()`, empty
messages and `SkipNow()`; `dynamic` retains an unresolved message expression
for inspection. The summary lists missing/dynamic sites by name. A computed
reason is not evidence of a missing runtime reason. Prefer a static explanatory
prefix with the detail as a formatting argument. The repository witness
`TestRepositorySkipsCarryReasons` fails on missing reasons.

Classes are generated hints from the message: `mode`, `credentials`, `service`,
`tool`, `platform`, `filesystem`, `fixture`, `capability`, `environment`, or
`condition` for unmatched prose. They do not certify that a skip is justified.
An explicit `[class] ` prefix in the message overrides inference; the rules
live in `classify`, not in a parallel list of sites. Source expressions remain
available for reviewing an inferred class.

The mission's historical 506 skips / 291 sleeps are not a baseline to freeze.
At implementation, AST inventory finds 757 skips and 486 sleeps: 731 / 423 in
first-party `_test.go`, 14 / 23 in support or production, and 12 / 40 in
`third_party`. Re-run the generator for the current counts. The starting raw
inventory is reproducible with:

```sh
rg -n '\.(Skip|Skipf|SkipNow)\(' --glob '*.go' --glob '!vendor/**'
rg -n 'time\.Sleep\(' --glob '*.go' --glob '!vendor/**'
```

Those text counts (760 / 488 at the starting revision) include unrelated
`Skip` methods and quoted/commented sleep text. They are not AST call counts.

## Sleep duration and bounds

This is the **static inventory** variant, not a wall-clock measurement.
A sleep row contains:

| Field | Meaning |
|---|---|
| `expression` | Exact argument rendered as Go source. |
| `status` | `constant` or `dynamic`. |
| `requested_ns` | Constant duration in nanoseconds; null when unresolved. |
| `minimum_ns` | max(0, requested), in the sleep's clock domain; null when unresolved. |
| `maximum_ns` | Always null: scheduling gives `time.Sleep` no finite upper bound. |
| `clock` | `synctest` inside a syntactically visible bubble; otherwise `caller-dependent`. |

The evaluator handles time units, integer arithmetic, duration conversion and same-file
untyped constant references (and `time.Duration` constants). Width-sensitive
typed constants, floating-point/complex literals, variables, cross-file constants, `iota`, function calls
and unsupported expressions stay dynamic; their initializers are not invented
bounds. At implementation, 448 arguments resolve and 38 remain explicit.
Negative/zero durations return immediately. Helpers can be called from a
`testing/synctest` bubble, so even a constant duration does not establish wall
elapsed time. Loops and repeated calls are single sites: summing rows would
not measure suite duration. No sleeps are changed or executed by the census.

## CI publication and runner observations

The existing `test-unit` job runs the same Go command after tool setup, before
the unit suite, and appends `summary.md` to the GitHub job summary. Generation
and upload use `always()` so an earlier failure does not silently suppress
evidence. The `test-capabilities-unit` artifact contains the two census files
and `runner.json`; missing artifacts fail publication. Upload explicitly includes
hidden files because the output lives under `.iterion/`. Retention is 30 days.
The job retains its existing trigger/skip policy; a skipped job produces no
artifact. The census is not a claim that every listed test ran.

A run without `-observe` removes an earlier `runner.json`.
`-observe` adds **separate, environment-dependent** evidence in `runner.json`:
GOOS/GOARCH and boolean observations of literal `os.Getenv`/`os.LookupEnv` and
`exec.LookPath` calls in functions containing skips. This is a function-level
association, not proof that each probe guards each skip. Dynamic probe names,
helper-transitive probes and build-tag availability are not inferred.
Environment availability means nonempty; executable availability means PATH
lookup succeeds. No values, executable paths, credential validity or service
reachability are published, and no executable or service is invoked.

The observations describe the **unit runner** at the census step, not the
Mongo/NATS jobs, the e2e runner or actual per-test skip outcomes. Only the two
static census files promise source-only determinism; runner observations vary
with platform, PATH and environment.

## Acceptance witnesses and mutations

Run `devbox run -- go test -mod=vendor -count=1 ./scripts/ci/testcaps`.
The witnesses are in [`main_test.go`](../scripts/ci/testcaps/main_test.go):

| Witness | Mutation that must fail its own assertion |
|---|---|
| `TestCensusFindsTestingReceiversAndIgnoresText` | Remove field/alias receiver resolution or count source strings as calls. |
| `TestCensusNamesMissingAndDynamicReasons` | Mark empty messages named, discard `SkipNow`, or classify a computed reason as missing. |
| `TestSleepBoundsUseConstantsAndTheSleepClock` | Evaluate mutable initializers as constants, use rational integer division, or lose the synctest clock. |
| `TestCensusBytesDoNotDependOnRootOrInputOrder` | Remove source sorting or include absolute paths. |
| `TestSourceBoundaryIncludesNewFilesAndAllPlatforms` | Drop `--others`, filter Windows sources, include vendor/testdata, or swallow parse errors. |
| `TestRunnerObservationsContainPresenceNeverValues` | Force probe availability true, retain duplicates, or serialize probe values/paths. |
| `TestRepositorySkipsCarryReasons` | Replace a repository skip message with an empty string. |
| `TestCIPublishesTheSameCensusEvenAfterFailure` | Remove upload, its `always()`/`include-hidden-files`, the generation command, or change the local task's generator. |
| `TestCensusPreservesGoMessageAndTypeSemantics` | Follow defined types as aliases, inspect only the first `Skip` argument, or merge dot imports. |
| `TestStaticRegenerationRemovesOldRunnerObservations` | Leave the previous `runner.json` after a run without `-observe`. |
| `TestSleepNeverInventsBoundsFromUntypedNumericCoercion` | Fold untyped float/complex values without Go duration coercion; `Duration(3.0)/2*2` must stay unresolved, not become 3 ns. |
