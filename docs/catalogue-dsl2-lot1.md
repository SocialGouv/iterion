# Catalogue DSL 2 — first reviewed lot (#1159)

This lot prepares `docs-refresh` 3.5.8 and `adr-cartograph` 2.0.1 for DSL
profile 2. It does not activate a platform override or move the deployment.
All other bundles remain for later, small reviewed lots.

The migration was run from source release **3.143.0**, commit `882c76afbb`:

```sh
devbox run -- go run ./cmd/iterion dsl migrate --to 2 --floor 3.143.0 \
  --show-prompts bots/docs-refresh bots/adr-cartograph
```

Both workflows need only the `dsl: 2` header. Neither contains a quoted
backslash literal to re-spell. The migrator proves the program and catalogue
identity are unchanged apart from the named prompts' paragraph breaks. The
manifests declare `requires.iterion: ">= 3.143.0"`, the known source release
used to prepare them; YAML formatting was retained to keep their diff small.

## What reaches the model differently

The before/after request fixtures live under
[`bots/testdata/dsl2-lot1/`](../bots/testdata/dsl2-lot1/). Each `.v1.txt` and
`.v2.txt` pair holds the **resolved authored system and user prompts** captured
at the real `ClawExecutor` backend boundary. The recording backend stops
before a model call. Model quality, tool use and campaign success are not
inferred from these deterministic captures.

| Bundle | Prompt | Paragraph breaks restored |
| --- | --- | ---: |
| docs-refresh | campaign_system | 16 |
| docs-refresh | campaign_user | 7 |
| docs-refresh | finalize_mr_system | 6 |
| docs-refresh | finalize_mr_user | 4 |
| adr-cartograph | survey_code_system | 7 |
| adr-cartograph | survey_code_user | 5 |
| adr-cartograph | campaign_system | 14 |
| adr-cartograph | campaign_user | 9 |
| adr-cartograph | verify_system | 2 |
| adr-cartograph | verify_user | 2 |
| **Total** | **10 prompts / 5 agent requests** | **72** |

Review any pair directly, for example:

```sh
git diff --no-index \
  bots/testdata/dsl2-lot1/docs-refresh/campaign.v1.txt \
  bots/testdata/dsl2-lot1/docs-refresh/campaign.v2.txt
```

The Doki mission, scope restriction, four issue outcomes and honest
`docs_aligned` contract now occupy their authored paragraphs. Adry's survey,
decision-versus-mechanic boundary, `adrs_aligned` contract and build
verification instructions receive the same treatment. No instruction was
reworded. Fixture inputs are visible sentinels containing their own blank
line and `{{input.*}}` text; these remain literal input data under both
profiles. Variables use the workflow defaults and fixed fixture paths.
The code-scope example is `src/**`; an empty scope would render a trailing
space in its inventory line, obscuring whitespace-only review of the goldens.

`TestCatalogDSL2RenderedPrompts` compares the complete compiled workflow after
normalizing only prompt paragraphs, then compares all five real requests to
both committed renderings. It therefore retains tools, scopes, gates, schemas,
edges, budgets and continuation limits. `TestCatalogDSL2TerminationGates`
evaluates all 4 Doki and all 16 Adry truth-table rows: convergence requires
every gate fact. `task test:goldens` replays the existing recorded
`campaign_docs_aligned` and `campaign_adrs_aligned` output contracts.

The migrated catalogue is mixed-profile. The corpus migration test still
proves every remaining v1 file can migrate, and now checks byte-idempotency
for already-v2 files.

## Mirrors and verification

`pkg/cli/templates/dispatch_bots/` is generated and gitignored. Regenerate it
with `devbox run -- task templates:dispatch-bots`: Doki's embedded mirror then
matches its source bundle, including the manifest floor. Adry is not among
the embedded dispatch templates. No studio fixture mirrors either bundle;
unrelated fixtures are intentionally left for another lot.

```sh
devbox run -- task templates:dispatch-bots
devbox run -- go test ./bots -run TestCatalogDSL2 -count=1
devbox run -- go test ./pkg/dsl/migrate ./pkg/botreplay -count=1
devbox run -- task test:goldens
```

Fixtures are updated only after reviewing the rendered change, using
`UPDATE_DSL2_PROMPTS=1` with the first Go test command. A changed fixture is
review evidence, not an automatic approval of new prompt semantics.

Preparation checks on 2026-09-14 passed: the full `bots`, `pkg/dsl/...`,
`pkg/botreplay`, `pkg/bundlelint` and `pkg/cli` suites under `-race`; the
standalone `task test:goldens` replay; and `golangci-lint` on both changed Go
packages. Sensitivity was checked with two temporary mutations: removing the
blank line before Doki's `THE MISSION` failed its v2 request golden; removing
the scope conjunct failed the row where `docs_aligned=true` but
`scope_ok=false`. The source was restored and the prompt/gate tests passed
again. No campaign or model was invoked for these checks.

## Activation prerequisites — still outstanding

Keep the PR draft while the fleet and campaign prerequisites are unverified.
The delivery plan must provide the following checks in order; the actual
catalogue release can only be recorded once that release exists:

1. Record the actual release carrying this catalogue lot; raise both
   manifest floors to that released build before publishing the overrides.
   **No future release number is assumed here.** Profile 2 itself first
   shipped in 3.141.0, a separate fact from this lot's delivery version.
2. Verify the server and **all** runner builds can meet that floor before
   the catalogue push. `pkg/server/engine_floor.go` uses the minimum over
   the server and up to 200 runner builds observed during seven days;
   reading one team's recent runs cannot certify the whole deployment.
3. Verify across the deployment that no long campaign is in flight; retain
   the existing sources for paused/resumable campaigns until their owner
   decides how to resume them. Every textual workflow hash changes, so a
   changed-source resume needs an explicit `--force`; retry identities and
   reliability history also split. Do not bulk-force resumes.
4. After the approved deployment, compare the baked/platform bundle source,
   rerun the fleet check, then validate a representative fresh launch before
   taking the next catalogue lot.

Read-only preparation observations on **2026-09-14**: the remote health
endpoint reports `v3.142.1` / `f1230b15956f0cbd45da2d8945b87475cbeb5382`,
below this preparation floor. The current team's running-run list was empty;
its recent Doki run `01a09e12-74dc-784f-a47d-14e4d200a65b` was
`failed_resumable`. Neither observation proves the global fleet or campaign
prerequisite. No override push, rollout, cancellation or resume was performed.
