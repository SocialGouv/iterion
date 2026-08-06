---
name: feature-inventory
description: How to enumerate an application's FEATURES from the outside (docs, CLI surface, API routes, configuration, grammar/DSL, UI routes) and turn them into stable coverage-matrix rows grouped by family. Read when building or refreshing the feature×coverage matrix.
---

# feature-inventory — enumerating what the application does

A feature is a **user/operator-observable capability**: something the
application promises to do, at the granularity a regression report would
name ("resume from a failed run", "the `--json` output mode", "webhook
launches a bot per PR"). Not a package, not a function, not a code path —
those are the *implementation*; the matrix rows are the *promises*.

## Where to look (sweep ALL of these — each sees features the others miss)

1. **Docs first**: README, `docs/`, CHANGELOG, ADRs, man pages, website.
   The features the project *documents* are its strongest promises. Note
   claimed flags, modes, subcommands, integrations.
2. **CLI surface**: run the binary's `--help` tree (every subcommand,
   recursively) or read the command registration code. Every subcommand +
   every flag that changes behaviour is a feature or a feature variant.
3. **API surface**: HTTP route tables, OpenAPI/Swagger specs, RPC/proto
   definitions, GraphQL schemas. Every route/method is a promise.
4. **Configuration**: environment variables, config-file schemas, feature
   flags. A toggle that changes behaviour is a feature (both positions).
5. **Grammar/DSL**: if the application consumes a language (a DSL, a
   config format, query syntax), every construct/keyword/edge-kind is a
   feature. Look for grammar files, parser test corpora, reference docs.
6. **UI routes/views** where applicable: pages, panels, and the flows
   they promise.
7. **Event/integration surface**: webhooks consumed/emitted, queue
   messages, schedulers, notification sinks.

Reconcile with any **existing coverage documentation** the repo already
has (a test-matrix doc, a scenarios file, CI job descriptions): fold its
knowledge in rather than starting a rival inventory — and where this
matrix supersedes an older partial doc, say so in the older doc when the
operator asks for it.

## Granularity — the regression-report rule

Pick the granularity at which a regression would be REPORTED, then stop:

- Too coarse: "the CLI works" (one row hiding fifty promises).
- Too fine: one row per flag alias or per error string.
- Right: "run: budget caps override per run", "resume: `--force` on a
  changed source", "router: condition mode picks the matching edge".

A feature with materially distinct behaviour modes gets one row per mode
ONLY when a mode could break independently (e.g. `await: wait_all` vs
`await: best_effort` — two rows; `--help` output phrasing — zero rows).

## Families and IDs

Group rows into **families** — stable, kebab-case buckets that mirror how
the application is organised from the outside (e.g. `cli`, `runtime`,
`persistence`, `server-api`, `scheduling`, `integrations`). The family is
the `target` a scoped run selects.

Row IDs are `family.feature-slug`, kebab-case, **stable across runs** —
never renumber or rename an ID once committed (downstream runs, scoped
targets and review diffs key on it). If a feature is renamed upstream,
keep the ID and update the Feature label.

## Mapping existing tests (legitimate no-new-code work)

For each feature, search the repo's test suites for tests that already
exercise it end-to-end: grep the feature's flag/route/keyword in test
files, read the e2e/integration suite's names, check CI jobs. When a
real pre-existing test covers the feature:

- cite it (`TestName (path/to/file)`) and set `covered-deterministic`
  (or `covered-live` when it lives in an opt-in/live layer — justify);
- do NOT write a duplicate test just to have authored one.

When a test *touches* the feature but asserts nothing about its
observable contract (see the doctrine in [[e2e-coverage]]), the feature
is NOT covered — leave `uncovered` and note the near-miss.

**Unit tests never make a row `covered-*`.** A feature whose behaviour
is fully asserted at unit level and whose e2e would only re-test the
harness is `unit-only` — with the justification, citing the unit tests
in Notes if useful.

## Refreshing (later passes / later runs)

- Re-read the matrix; verify rows still match reality (a feature removed
  upstream → drop the row in the same commit that notes why; a new
  feature since the last inventory → add its row `uncovered`).
- Never mass-rewrite the matrix: refresh is row-level, so the diff stays
  reviewable and claims stay auditable.
