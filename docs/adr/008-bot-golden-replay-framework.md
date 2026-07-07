# ADR-008: Bot golden-test framework records at the NodeExecutor seam and replays statically

- **Status**: Accepted
- **Date**: 2026-05-29
- **Authors**: devthejo
- **Code context**: [`pkg/botreplay/`](../../pkg/botreplay/),
  [`pkg/botreplay/testdata/bot-goldens/`](../../pkg/botreplay/testdata/bot-goldens/),
  `Taskfile.yml` (`test:goldens`, `test:goldens:record`, `check`)

## Context

Iterion's flagship bots (`feature_dev`, `whats-next`, `docs-refresh`) emit
structured LLM output that downstream nodes and the dispatcher depend on:
a reviewer's `verdict_output.family` routes the fix loop, `emit_action`'s
`created_issues[].assignee` tells the dispatcher which bot to run, and a
schema tightening in any `.bot` file can silently invalidate output shapes
the LLM was previously producing. The existing live tests (`task
test:live`) exercise these end-to-end but cost real money, need API keys,
and are too slow/flaky to gate every PR.

We wanted a cheap, deterministic regression gate that freezes a
representative LLM node output and continuously re-checks it against the
*current* declared schema and a set of bot-quality invariants:

1. output still validates against the node's declared output schema
   (catches schema drift),
2. semantically-required fields are present and non-empty (e.g.
   `created_issues` for `emit_action` — a `json`-typed field that schema
   validation accepts even when empty), and
3. no hallucinated assignees — every non-empty `assignee`/`bot` in the
   output resolves to a bot that actually exists in the catalog.

The open question was **which seam to record/replay at**, and **whether
replay should drive the runtime engine**.

## Decision

Record and replay at the **`runtime.NodeExecutor` seam**
(`Execute(ctx, node, input) → output`), and make replay a **static
verifier** over committed JSON fixtures rather than a runtime-driven
replay.

- A fixture (`testdata/bot-goldens/<bot>/<scenario>.json`) stores one
  node's `(input → output)` plus provenance (bot, node, backend, model).
- **Record mode** (`pkg/botreplay/record.go`, build tag `goldens_record`)
  invokes a single node through the production `*model.ClawExecutor`
  built by `runview.BuildExecutor`, hits the real provider, and writes
  the fixture. It is excluded from the default build, so `go test ./...`
  never compiles the heavy executor stack and never needs credentials.
- **Replay mode** (`pkg/botreplay/verify.go` + `goldens_test.go`,
  default build) loads each fixture, recompiles the bot to IR, and runs
  `VerifySchema` (reusing the production `model.ValidateOutput`),
  `VerifyRequiredNonEmpty`, and `VerifyNoHallucinatedAssignees` (against
  the live `botregistry` catalog). No LLM, no engine, no credentials.
- `task test:goldens` runs the replay gate and is added to `task check`.

The assignee scan is a recursive walk keyed on the `assignee`/`bot` JSON
keys, so it finds both `emit_output.created_issues[].assignee` and the
nested `roadmap_item.assignee` arrays without hardcoding either path, and
tolerates kebab/snake/case via `botregistry.NormalizeName`.

## Trade-offs

| Dimension | NodeExecutor seam + static replay (chosen) | Fake `api.APIClient` + runtime replay (rejected) |
|---|---|---|
| Fixture shape | Clean `(input → output)` maps — exactly what `Execute` exchanges | Raw streaming wire events; needs re-aggregation |
| Injection seam | Already exists; e2e stubs use it | None — `*model.ClawExecutor` has no client-injection option |
| Replay determinism | Total — pure JSON + schema | Blocked: all three bots have `human` + `tool` (git/python) nodes that pause/shell out |
| Credentials in CI | None | None for replay, but the runtime path can't complete unattended |
| What it catches | Schema drift, missing fields, bad assignees, bot-compile breakage | Same, plus executor parsing/coercion fidelity |
| Implementation weight | One leaf package, no engine import in default build | New `WithClientFactory` option + node-type stubbing across the engine |

The one capability we give up is exercising the executor's own
parse/coerce/format pass in replay. We accept this: that pass is already
covered by `pkg/backend/model`'s own unit tests, and the golden gate's
job is to pin *the LLM's structured output against the bot's contract*,
not to re-test the executor.

## Alternatives considered

### 1. Inject a fake `api.APIClient` and replay through the runtime

Stub the LLM at the lowest level (`api.APIClient.StreamResponse`, the
seam the `model` package's own tests already mock) and drive the full
`runtime.Engine.Run` with recorded responses.

**Rejected because**: (a) `*model.ClawExecutor` exposes no option to
inject a client — every `ClawExecutorOption` builds clients internally
via the backend registry, so this needs new public plumbing; and (b)
even with the seam, an unattended runtime replay of these three bots is
impractical — each contains `human` nodes (`interaction: human` →
pause), real `tool` nodes (git commit, python scanners, HTTP calls), and
reviewer escalation. Replaying to completion would require stubbing the
entire tool + human + compute layer, far more surface than the gate
warrants.

### 2. Record a whole live workflow run and tee every node

Wrap the production executor, run the bot end-to-end live, and capture
all nodes.

**Rejected because**: `whats-next` cannot run unattended (its
`human_review` / `ask_*` nodes pause), and `feature_dev` / `docs-refresh`
mutate real code and commit. Single-node record is the only path that
works uniformly across the three and keeps a fixture tied to the one
node whose contract we assert.

### 3. Validate fixtures with a bespoke schema checker

Re-implement required-field/type/enum checking inside `botreplay`.

**Rejected because**: `model.ValidateOutput` is the exact validator the
runtime applies in production. Reusing it means the golden gate fails
when — and only when — a real run would fail, with zero drift between
the two code paths.

## Deviations from the source plan

- **Initial fixtures are hand-authored seeds, not live recordings.** The
  plan's canonical path is record-then-commit, but no LLM credentials
  were available at implementation time, and CI must be green on the
  first commit. The four seed fixtures are authored from each bot's
  declared schema (and the already-schema-valid shapes in the existing
  e2e stubs) and carry a `_note` field flagging their provenance.
  `task test:goldens:record` overwrites them with real recordings once a
  maintainer runs it with credentials. The verification logic is
  identical regardless of provenance, so the gate is meaningful
  immediately; the only thing a real recording adds is fidelity of the
  *frozen* output to an actual model response.

- **Record keeps node tools, strips only sandbox.** Unlike the e2e
  `compileFixtureStubSafe` (which strips both), record must let
  read-only reviewer/proposer nodes read the repo, so it clears only
  workflow/node `Sandbox` specs (no docker) and `chdir`s into the
  workspace so the claw backend's in-process filesystem tools resolve
  against the intended tree.

## Consequences

- **Cheap, credential-free PR gate.** `task test:goldens` runs in
  milliseconds and is wired into `task check`. `go test ./...` already
  picks up `TestGoldens` (the explicit task is a named fast gate); record
  tests are build-tagged out of both.

- **Schema drift is a loud failure, by design.** Tightening a `.bot`
  schema that an existing golden no longer satisfies fails the gate.
  Maintainers regenerate fixtures (`task test:goldens:record`) as part of
  such a change — this is the intended signal, not noise.

- **New bots/scenarios are a fixture + a `Scenario` entry.** The
  `Scenarios()` registry in `pkg/botreplay/scenarios.go` is the single
  source linking scenario → fixture → invariants → record inputs.
  `TestGoldens` fails if a registered scenario has no committed fixture,
  so coverage cannot silently regress.

- **Assignee field coupling.** The hallucination scan keys on
  `assignee`/`bot`. A future schema that names a bot field differently
  (e.g. `set_bot`) must extend `assigneeKeys` in `verify.go`.

- **`pkg/runview` import is isolated behind the build tag.** The default
  replay binary imports only `pkg/dsl/*`, `pkg/backend/model`
  (for `ValidateOutput`), and `pkg/botregistry`; the heavy executor
  stack compiles only under `goldens_record`.

## Addendum (2026-06-14) — re-challenge pass

Decision confirmed. The strongest survey signal was record-keeping, not
drift: the `api.APIClient` injection seam still does not exist
(`pkg/backend/model/executor.go`), so Alternative 1 stays infeasible for
exactly the reason given, and the assignee-coupling consequence has not
fired (`set_bot` is only the MCP routing tool, never a recorded field).
Two divergences to note for the historical record: (1) this ADR's prose
names the pre-rename bots `feature_dev`/`doc-align`, since renamed to
`docs-refresh`/kebab-case (`c9996d98`, `8784d677`) — `scenarios.go`
already tracks the new names; (2) the four golden fixtures are still the
original 2026-05-29 hand-authored seeds, never regenerated via record
mode despite credentials now being available — the "Deviations" note's
follow-up remains open.


## Addendum (2026-07-07) — v2 campaign scenarios are hand-authored by design

The ADR-058 fleet rollout retired the `reviewer_gpt` nodes the
feature-dev and docs-refresh scenarios pointed at. Both scenarios were
swapped in the same commits as the bot conversions (atomic — the
goldens gate never went red) to the surviving `campaign` node's
termination contract: `feature-dev/campaign_feature_complete` and
`docs-refresh/campaign_docs_aligned`.

Recording a `campaign` node live is impractical by construction — it is
a whole-session claude_code agent (`interaction: human`, real repo
side-effects, forfait cost), not a single structured call. Its fixtures
are therefore **hand-authored seeds, permanently**: frozen on the
termination-contract schema, regenerated by hand only when that schema
changes. This is the same provenance tier as the original 2026-05-29
seeds (all four were hand-authored; see the deviation note above), so
the gate's value is unchanged: schema drift on the contract the ENGINE
converges on breaks the fixture loudly. `TestVerifySchema_RealBot`
retargeted from `reviewer_gpt`/verdict_output to
`campaign`/campaign_output in the same change.
