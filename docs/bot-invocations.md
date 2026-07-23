# Bot invocations — how a bot is triggered on a repo

A bot's `manifest.yaml` declares a typed **`invocations:`** block: *how* the bot
can be triggered and *which execution mode* each path uses. It is the
machine-read routing contract, orthogonal to the two pre-existing fields:

| Field | Purpose |
|---|---|
| `forge:` | Forge-access requirements (token scopes, secret name). Optional. |
| `triggers:` (`[]string`) | Free-form advisory labels for the catalog / Nexie. |
| **`invocations:`** | **How the bot is triggered + execution mode.** |

The studio Integrations picker offers every bot that declares an
`invocations:` block (or a legacy `forge:` block); orchestrators that declare
neither (Nexie, Evoly) stay out of the picker.

## Schema

```yaml
invocations:
  - kind: forge | command | schedule | board | keepalive   # required, closed set
    mode: direct | board                        # optional, default direct
    args_var: <workflow var>                    # optional: where the trigger payload lands
    context_vars: { k: v }                       # optional: vars stamped on every run from this path
    # exactly one payload block, selected by kind:
    forge:    { event: pull_request | pull_request_comment | issue_labeled, actions: [opened, reopened] }
    command:  { name: featurly, aliases: [feature-dev], scope: pr|issue|any,
                min_replier_role: maintainer, disambiguator: when_args_empty|when_args_present }
    schedule: { suggested_cron: "0 2 * * 1", default_vars: { k: v } }
    board:    { on: [card.moved], to_states: [ready], all_labels: [triage:auto],
                consume_labels: false }          # optional; empty = plain dispatcher target
    keepalive:{ interval: 5m, stale_after: 15m, default_vars: { k: v } }
```

Validation runs at manifest parse time (`bundle.validateInvocations`): unknown
kind/mode, unknown forge event, malformed command name (`^[a-z][a-z0-9_-]*$`),
bad scope/disambiguator, and intra-bot duplicate command names all fail fast.
`botregistry.ListWithSchema` additionally warns (soft) when an `args_var` names
a var the bot's workflow does not declare.

## Trigger surfaces

- **`forge`** — a webhook event (PR/MR open). Reactive, typically `direct`.
- **`command`** — a `/slash-command` in a PR/MR/issue comment. The universal
  manual trigger, on all three forges (GitLab notes, GitHub/Forgejo
  `issue_comment`). Resolved through the per-webhook command map.
- **`schedule`** — a suggested cron the Integrations UI proposes. In cloud
  mode the `cloudsched` CAS ticker fires due schedules
  ([pkg/cloudsched](../pkg/cloudsched/ticker.go), wired in
  `pkg/server/server_lifecycle.go`); self-hosted uses `iterion schedule` on
  the host crontab.
- **`board`** — a native-board trigger. With no `board:` block the bot is a
  plain dispatcher target (an issue whose `Bot` is this bot is picked up and
  run). A `board:` block (`on`/`to_states`/`all_labels`) fires the bot the
  moment a matching card transition lands (via a derived
  `trigger.Subscription`) instead of waiting for the poll; `consume_labels:
  true` (with `mode: direct`) strips the matched labels atomically so they act
  as a one-shot, re-armable trigger.
- **`keepalive`** — always-on: a fresh, individually-budgeted run relaunches
  every `interval` with at-most-one-live semantics (a run silent past
  `stale_after` is reaped, not stacked). Sub-minute cadence needs the resident
  in-process scheduler (host crontab floors at 1m).

## Execution mode

- **`direct`** — launch the run immediately (the Revi path: webhook → publisher
  → queue → runner). For fast / read-only / PR-bound work.
- **`board`** — materialises a tracking kanban card for the bot and, when a
  cloud board + dispatcher are present, lets the dispatcher own execution and
  state transitions (tracked, retryable, human gates) — no direct launch, so
  the card can't run twice (`ensureBoardCard` in
  [pkg/server/invocation_dispatch.go](../pkg/server/invocation_dispatch.go)).
  With a cloud board but no dispatcher it creates a tracking card **and**
  launches directly; with no cloud board at all it falls back to a plain
  direct launch.

## Slash-command routing

`/<name> <args>` in a comment resolves to a bot via the webhook's command map
(computed by the forge orchestrator from the co-enabled bots' command
invocations), with a live `botregistry` fallback for hand-created wildcard
webhooks. Two bots may share a command only when they disambiguate by args
presence — the `review-pr` (bare `/revi`) vs `revi-converse` (`/revi
<question>`) pattern; any other cross-bot collision is rejected at provision
time.

The command args land in the route's `args_var` (e.g. `/featurly add an export
endpoint` → `feature_prompt = "add an export endpoint"`). The commenter is
gated: the bot's own comments are ignored (loop-guard), and the commenter must
be in `AuthorizedRepliers` OR hold a repo role ≥ the command's
`min_replier_role` (GitHub/Forgejo collaborator permission and GitLab access
level are both mapped onto the role scale; default `developer`). Mutating bots
declare `min_replier_role: maintainer`.

## Curated commands

| Bot (persona) | Command | Surfaces | Mode |
|---|---|---|---|
| review-pr (Revi) | `/revi` (bare) | PR open + comment | direct |
| revi-converse | `/revi <question>` | PR comment | direct |
| feature-dev (Featurly) | `/featurly <spec>` | PR/issue | board |
| feature-gap-fill (Fini) | `/fini <gap>` | PR/issue | board |
| branch-improve-loop (Billy) | `/billy` | PR/issue | board |
| whole-improve-loop (Willy) | `/willy <prompt>` | PR/issue | board |
| bmady (Bmady) | `/bmady <brief>` | PR/issue | board |
| secured-renovacy (Renovacy) | `/renovacy` + weekly | PR/issue | board |
| devbox-setup (Devy) | `/devy` | PR/issue | board |
| docs-refresh (Doki) | `/doki` + nightly | PR/issue | board |
| adr-cartograph (Adry) | `/adry` + weekly | PR/issue | board |
| adr-rechallenge (ReArchi) | `/rearchi` | PR/issue | board |
| sec-audit-source (Seki) | `/seki` + Mon 02:00 | PR/issue | board |
| sec-audit-deps (Depsy) | `/depsy` + Mon 03:00 | PR/issue | board |

The technical bot name is always an alias (`/feature-dev` ≡ `/featurly`).

## Adding a new trigger

Adding a way to invoke a bot is a manifest edit — no DSL or engine change.
Drop an `invocations:` entry; the orchestrator picks up the command/events at
the next provision, and the picker shows the new trigger. Keep `args_var`
pointed at a declared workflow var (else the payload is dropped + a warning is
surfaced).

## Validating live (dogfood)

The slash-command routing is unit-tested per forge (the replier gate is mocked
via a test seam). A full live validation needs a real forge connection — the
gate calls the forge API to authorize the commenter — so dogfood it against a
connected repo:

1. studio → Integrations: connect a repo (GitLab / GitHub / Forgejo) and enable
   a command bot — e.g. feature-dev (Featurly). The enable dialog lists it under
   "Run by /command" and shows `Commands: /featurly`. (Command-only bots no
   longer show as conflicts — that was the P1.7 preview fix.)
2. Open a pull/merge request on that repo.
3. Comment `/featurly add a healthcheck endpoint` — a GitLab note, or a
   GitHub/Forgejo PR comment (issue_comment). The commenter must hold a repo
   role ≥ the bot's `min_replier_role` (maintainer for mutating bots).
4. Observe: the delivery is recorded (studio webhook deliveries), the gate
   authorizes the commenter, and a feature-dev run launches with
   `feature_prompt = "add a healthcheck endpoint"`. With a cloud board +
   dispatcher the command materialises a tracking card the dispatcher runs;
   without a dispatcher (or a board) it launches directly.
5. Validate the path with a READ-ONLY command first: `/seki` (sec-audit-source)
   or `/revi` (review) don't mutate code. Contain side-effects by enabling on a
   throwaway repo. The binary is `CGO_ENABLED=0` (static) so it runs inside the
   sandbox container.

## Key files

- `pkg/bundle/manifest.go` — `Invocation` types + validation; `migrate.go` —
  `SyntheticInvocations`/`EffectiveInvocations`.
- `pkg/webhooks/types.go` — `Config.CommandMap` + `ResolveCommand`;
  `router.go` — `ResolveCommandRoute`.
- `pkg/forge/orchestrator.go` — `buildCommandMap` + events derived from
  invocations (forge: optional).
- `pkg/server/webhooks_gitlab.go` (`handleGitLabCommandNote`),
  `webhooks_prforge.go` (`handlePRForgeComment`), `invocation_dispatch.go`
  (`dispatchInvocation`).
