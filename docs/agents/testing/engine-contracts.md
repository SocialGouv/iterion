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

## Contract facts the tests pin (ultra11y 5.42.1)

- `verify --apply` folds **partially by default**: exit 0 as soon as one
  verdict lands, refusals named in `issues` / `rejectedCriteria`, exit 1
  only when nothing lands or under `--strict` (which leaves the audit
  untouched; a complete adjudication folds under it). A `C` without
  citations is refused; every criterion absent from the file is a coverage
  gap.
- Three different rates: the `pack-audit` document's root `conformancePct`
  is the *automatic* pass rate (engine-decided C ÷ engine C + NC); `report
  --json` and the fold JSON echo the *core* (WCAG) rate; the **RGAA** rate
  (validated ÷ validated + NC + à évaluer, NA excluded) is in no JSON — it
  is computed from `criteria[]` and asserted against the report's « Taux de
  conformité » line, as the « Décidés » line is asserted against the
  document.
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

## Known upstream defects, pinned as subtests that fail when fixed

A subtest named `known_upstream_defect_…` asserts the defect; the day
upstream fixes it, the subtest reddens and the bot's workaround can be
lifted. On 5.42.1:

1. A criterion the engine marks "no subject" that stays open after a fold
   (coverage gap or agent `manual` verdict) is written back `inapplicable`
   while `manual`; the report counts it both as « conforme faute de sujet »
   and « à évaluer », and `check --report` notices only when NA exceeds C.
   The bot decides every engine-flagged criterion before the final report
   and compares the « Décidés » line with the document.
2. Re-folding a criterion the previous fold decided crashes with a
   TypeError and no JSON. The bot folds the real audit once, probes on
   copies, and reads JSON — never exit codes alone.

## Bumping the pin

A bump is a migration: update the pin file, regenerate the lockfile
(`npm install --package-lock-only` in `scripts/ultra11y-engine/`), run
`task test:contract:rgaa`, and read every red subtest as a contract change
to carry into the bot — including a `known_upstream_defect_…` subtest
going red, which is good news to act on.
