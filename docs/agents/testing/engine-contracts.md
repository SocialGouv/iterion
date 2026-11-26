# Engine contracts — testing an external CLI against the real pinned binary

**Read it when** a bot delegates its judgement to an external CLI (ultra11y
for rgaa-audit today; deepsec for sec-audit-source was the counter-example),
when you bump such a pin, or when a contract test goes red in CI.

## The rule

An external CLI a bot delegates to is tested against the **real pinned
binary**, offline, never against a stub kinder than the real thing. The
tests pin what the bot reads — exit codes, JSON shapes, counters, file
layout — and every `--json` call must print JSON, so an engine crash can
never read as a refusal.

The pattern exists because ultra11y is deterministic and offline for
everything but `scan`; deepsec, which spends LLM money on every call, could
only be stubbed, and that is where a 2.0→2.3 drift of its `init` semantics
went unseen ([the deepsec lesson](../bots/toolchain-devbox.md)).

## The ultra11y instance

| Piece | Where | What it holds |
|---|---|---|
| The pin | `bots/rgaa-audit/ultra11y.pin` | version, size and sha256 of the entry file `scripts/ultra11y.mjs`, the tarball integrity — exact values, no surrounding whitespace |
| The install-time pin | `scripts/ultra11y-engine/{package.json,package-lock.json}` | the whole dependency closure, installed with `npm ci --ignore-scripts` (the engine's own dependencies are caret ranges); a scoped `.gitignore` exception keeps this npm lockfile in a pnpm repo |
| The resolver | `scripts/ultra11y-engine.sh` | `$ULTRA11Y_BIN` → global npm install → private `npm ci` under `~/.cache/iterion/ultra11y/<version>`; a candidate is fingerprinted **before** anything of it runs, an explicit `ULTRA11Y_BIN` that is not the pin is refused loudly (never silently replaced), the printed path is absolute and symlink-resolved |
| The grep point | `scripts/ultra11y-pin.sh [version\|sha256\|size\|integrity]` | what CI, the Taskfile and the runner image read |
| The tests | `bots/rgaa_audit_engine_contract_test.go` on `bots/testdata/rgaa-audit-site/` | the contract below, on a planted fixture |

Run them with `task test:contract:rgaa`. Without `ULTRA11Y_BIN` they skip
locally and **fail** in CI — every job that runs `./bots` installs the pin
first — so a green CI always ran the engine. The subtests share one run
directory and are sequential: run the whole test, not one subtest.

The same target runs the bot's own tool nodes on that engine
(`bots/rgaa_audit_bot_nodes_test.go`): every `script:` body is taken from
the compiled IR, rendered the way the runtime renders it (each `{{ref}}` a
JSON literal) and executed with `node` in the workflow's order, fixture
verdicts standing in for the agents, one mutation per typed refusal. A
node body asserted on its source text would pin a spelling.

## What the hash does and does not cover

The entry-file hash covers the verdict path: `audit`, `verify`, `report`,
`check` and `tickets` open only that file. `status --browser` and `scan`
also run Playwright, resolved from `--cwd` **first** — `--cwd` must never
point at an untrusted tree; the bot points it at the engine's own package.
The dependency closure is pinned at install time by the lockfile, not at
load time.

## Contract facts the tests pin (ultra11y 9.6.6)

- `verify --apply` folds **partially by default**: every accepted verdict
  lands, refusals named in `issues` / `rejectedCriteria` (each line opens
  with `criterion <id>:`). Exit 0 only when the gate refused nothing; any
  refusal exits 1 — an *incomplete fold*: the accepted verdicts are written
  (`auditPath`), `ok: false` (SocialGouv/ultra11y#223) — and so does a fold
  where nothing landed (no audit written) or `--strict` with a refusal
  (which leaves the audit untouched; a complete adjudication folds under
  it). 2 is usage. The bot reads every fold through `L.foldJSON`: exit 0/1
  with JSON whose `ok` matches is data, anything else a crash. A `C` without
  citations is refused. A criterion absent from the file is a coverage gap:
  no refusal — it is named apart in `notInWorklist`, out of `rejected`,
  `rejectedCriteria` and `issues` (only `--strict` lists it there) — and it
  stays « à évaluer »; `fold_probe` re-asks it like a refusal.
- `verify --manual` writes the verdict contract every brief shares once, as
  `ADJUDICATE.contract.md` beside `ADJUDICATE.md`; the briefs refer to it
  instead of repeating it. The worklist node hands its path to the
  adjudicators (`contract_path`).
- `scan --sample --merge --json` opens its document with a `pages` summary
  (`{sample, recorded, refused: [{id, reason}]}`). A partial sample (a page
  that redirects elsewhere or answers HTTP ≥ 400) exits 1 with the merged
  audit written; `render` accepts it (`L.scanOutcome`), counts the engine's
  refused pages and the annex names them. No page recorded exits 1 with
  nothing written, and fails the layer.
- Since 9.0.1 an `<img>` without `alt` is a decisive engine NC on 1.1, so
  1.1 never reaches the fixture's worklist; the tests that leave a judgment
  criterion open use 1.2.
- Three different rates: the `pack-audit` document's root `conformancePct`
  is the *automatic* pass rate (engine-decided C ÷ engine C + NC); the fold
  JSON's `conformancePct` is the *core* (WCAG) rate, beside
  `automaticPassRatePct` (the same number) and `conformanceRatePct` (the
  **RGAA** rate); `report --json`'s `conformancePct` is the RGAA rate
  (validated ÷ validated + NC + à évaluer, NA excluded), its operands under
  `conformance` and the automatic rate as `automaticPct`. The bot still
  computes the RGAA rate from `criteria[]` and asserts it against the
  report's « Taux de conformité » line, as the « Décidés » line is asserted
  against the document. The rate lives in the business summary; the counts and automatic
  rate live in the linked technical annex. Both `check` and `verify` load it.
- The C gate grounds citations on the **source** (file exists, snippet
  matches when given), not on the evidence the item was shown: a stray
  citation is accepted, so the bot carries its own `citations ⊆ evidence`
  check.
- `check --require-decided=pages --json` nests the undecided list under
  `decided.undecided`; `--allow-undecided` has no default and a missing
  file exits 2.
- `tickets` writes `issues-<date>.json` (`schemaVersion 1`, one issue per
  criterion) even without a token and makes exactly one provider request
  (`list()`), which degrades on failure.
- `status --browser --browser-path=<file>` and `scan --browser-path=<file>`
  (or `ULTRA11Y_BROWSER_PATH`) launch **that** executable: an explicit
  choice the engine never replaces by a fallback, and the probe answers
  about that file (`browser.ok` + `browser.executable`, or a reason naming
  the path). The bot resolves `browser_path` → `$ULTRA11Y_BROWSER_PATH` →
  the engine's own Playwright once in `prepare`, and the sandbox browser
  image names its Debian `/usr/bin/chromium` that way.
- The conformity trial (`verify --report`) puts every clearing claim an
  agent landed on the bench — the conformities **and** the
  non-applicabilities, which a pack adjudication keeps as `NA` — except the
  NA the engine's own absence fold proves. A claim without a citation
  stands trial at the scope level: `kind: "c"`, `uncited: true`, `file: ""`,
  `line: 0`. The bot carries `uncited` into each refutation batch; the
  refuters are told to look for a subject the criterion applies to instead
  of opening a file.

## Regressions fixed upstream and retained as positive contracts

The 5.45.0 migration verifies that partial/manual folds keep undecided
criteria out of the NA count, re-folding a decided criterion returns JSON
instead of crashing, bare NA verdicts on harvested evidence are refused,
and trial grounding accepts the evidence-anchor fallback used by the fold.
The 5.49.0 migration moved one contract: the trial's scope grew from the
agents' conformities to their clearing claims (above), and the bot's
dispatch and refuter prompts followed it. The 6.0.0 migration moved two:
coverage gaps left the fold's refusals for `notInWorklist`
(SocialGouv/ultra11y#136), and `report --json` carries the RGAA rate
instead of the core one (#116, #125). The 7.0.0–7.2.0 migration moved
none: 7.0.0 removed commands and flags the bot never called (`mcp`, `dev`,
`judge`, `affected`, `import`, `audit --lite`, `--format sarif|github`,
`init --ci`, `--baseline`, `render --e2e`, `pages --diff`, `scan --runtime
docker`; the bot runs `scan --runtime local`), and 7.1.0 adds Jest/Vitest
`__snapshots__/*.snap` files to what `audit` reads (`scope.unitSnapshots`).
7.2.0 pins axe-core by source: `scan` injects ultra11y's own exact
`axe-core` 4.13.0 (nested under `node_modules/ultra11y/` in the lockfile,
beside the 4.14.0 `@axe-core/playwright` brings) and records it as
`axe: {version, origin: "ultra11y"}` (the merged audit: `scope.axe`).
Its route inventory (`pages discover
--write` → `.ultra11y/routes.json`, `scan --all-routes`, annex section F,
`scope.routes`) is not called by the bot yet.
The 7.2.0–9.6.6 migration moved three: 9.6.1 makes an incomplete fold exit
1 (SocialGouv/ultra11y#223) and writes the verdict contract once as
`ADJUDICATE.contract.md`; `scan --sample` exits 1 on a partial sample with
its result written; 9.0.1 decides 1.1 on a missing `alt`. 8.0.0 removed
`capture write`, `render --setup` / `--scaffold` and the `.snap` audit;
9.0.0 made RGAA the only shown standard (engine result under `core`,
`--standard wcag` exits 2) and removed `pack`, `--pack`, `--override` and
`--guidance`. The bot called none of them and already read `core`. 9.6.2–9.6.6 moved
nothing the bot reads: page verdicts on still-manual criteria (with a
`notShownHere` field), absence-grounded page NA, a missing default ledger
exiting 0 on `verify --apply` (the bot always passes an explicit one),
sample pages counted with their journey states folded in, decision
protocols in the briefs, and (9.6.5) distinct no-page labels in the report
and a French default fix text, and (9.6.6) axe-core's own French locale
under `--lang fr` — rendered findings' messages are French, their identity
unchanged. The tests passed unchanged.
The workflow retains its count/rate comparison, relevance gate, literal
citation restoration and once-only mutation records. They provide independent
checks and preserve replay behavior; historical defect tests are not treated
as expected failures on the new pin.

Split-report tests also exercise the annex link, the two draft readers,
missing or invalid companions, optional HTML and portable file export.

## Bumping the pin

A bump is a migration: update the pin file (version, the tarball's
`scripts/ultra11y.mjs` size and sha256, the tarball integrity — all read
from the registry), regenerate the lockfile (`npm install
--package-lock-only --ignore-scripts` **in** `scripts/ultra11y-engine/` —
under devbox, `devbox run -- bash -c 'cd scripts/ultra11y-engine && npm …'`:
`devbox run` otherwise executes at the project root, where npm reads the
pnpm workspace links and arborist crashes), move the runner image's
`ARG ULTRA11Y_VERSION` and the bot's `engine_*` defaults (asserted equal by
`bots/rgaa_audit_engine_pin_test.go`), run `task test:contract:rgaa`, and
read every red subtest as a contract change to carry into the bot —
including a `known_upstream_defect_…` subtest going red, which is good news
to act on.
