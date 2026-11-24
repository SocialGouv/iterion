# ADR-172 — A forge delivery is matched against the subscriptions its integration projects, and every bot that claims it launches through the webhook tail, in its own mode and with its own vars

- **Status**: Proposed — the operator decided the direction on 2026-11-23 (complete the ADR-046 forge cutover; per-bot rules on the issue lane and one webhook per bot rejected). This record states how. P1a (the issue-label lane) is implemented in the change that adds this record.
- **Date**: 2026-11-23
- **Deciders**: jo (the decision), Claude (the synthesis of three designs and three adversarial verdicts)
- **Relates to**: [ADR-046](046-event-driven-runs-trigger-spine.md) (its staged forge cutover, lines ~150-157, and the stale cloud-evaluator line ~142) · [ADR-049](049-forge-as-interchangeable-substrate.md) (GitHub/GitLab parity) · [ADR-094](094-trigger-effect-outbox.md) (the outbox forge does not use, §1) · [ADR-138](138-forge-provisioning-immutable-identities.md) (one hook per integration) · [ADR-162](162-the-fork-review-lane-is-a-gesture-on-floors-that-land-first.md) (provenance floors the tail keeps) · [ADR-168](168-an-event-source-is-a-connector-package.md) (bound subscriptions, matched outside the bus) · [ADR-170](170-an-issue-trigger-is-a-re-armable-lane.md) (amended: a lane carries its bot)
- **Serves**: [#3530](https://github.com/SocialGouv/iterion/issues/3530) · [#3516](https://github.com/SocialGouv/iterion/issues/3516)

## Context

One repo integration (unique per tenant × connection × repository) owns one webhook, and its `webhooks.Config` lists every bot enabled on the repository. On `issues/labeled` the GitHub handler (`handleGitHubIssues`, `pkg/server/webhooks_github.go:456`) and the GitLab handler (`handleGitLabIssueEvent`, `webhooks_gitlab.go:289`) apply the repository-wide gates. These are the label allowlist, the hold labels, the event and project allowlists, and on GitLab the first matching added label in payload order (`FirstMatchingLabel`, `:299`). Both handlers then elect ONE bot (`selectIssueLabeledBot`, `webhooks_common.go:656`). The order of that election is:

1. `DefaultBotID`, set only on a single-bot integration (`singleBotDefault`, `pkg/forge/orchestrator.go:895`);
2. else the implementer role;
3. else `SelectBot` (single-bot only, `pkg/webhooks/types.go:586`);
4. else the reviewer.

`checkBotPermitted` then answers **403** (`:686`). With two label bots on one repository (nestor and saboteur on the demo repository, prod v7.24.2, 2026-11-23), a label starts **neither** (#3530). The 403 is also the answer that teaches GitHub to disable a hook.

The route the election produces comes from `boardRouteForLabel` (`invocation_dispatch.go:108`). It hard-codes `Mode: board` and takes `args_var` and `opens_mr` from the bot's *command* invocation. A `kind: forge` invocation's `mode` and `context_vars` never reach the run (#3516). The defect is wider than that lane: `buildBoardCard` (`invocation_dispatch.go:443`) copies `ArgsVar`, a fixed carry list and the hand-offs into `BotArgs`, never `route.ContextVars`. Cloud `processBoardCard` launches from `BotArgs` alone, so every board-mode lane loses its invocation's `context_vars`.

The per-bot table that would name each bot's labels exists only as dead data. `BotRule.LabelAllowlist` (`types.go:530`) is filled from `board.all_labels` (`orchestrator.go:2029`) and read by nobody, and `BotRule.Mode` is never read either. Only the pull-request lane fans out per bot (`resolveForgeEventBots` → `forgePREventTargets` → `insertAndLaunchWebhookMulti`, keys `forgeIdemKey(base, bot, multi)`). `docs/webhooks.md` ("Per-bot routing") claims a per-bot label filter that does not exist.

ADR-046 staged the cure: "when a forge subscription matches, skip the inline launch and let the spine launch; parity-gate via the `webhooks.Delivery` audit, then retire the inline path". Several facts shape how this record carries that out.

- **The lane has no bot.** ADR-170's lane is `(project, subject, trigger)` (`webhooks_trigger_lane.go:50`), and its keys and card ids (`boardLabel` `:137`, `laneCardID` `:215`) carry no bot. Fixing the election alone would let bot A's serving claim *absorb* bot B on the same label. On the no-coordinator path, `ensureBoardCard` (`invocation_dispatch.go:426`) dedupes on the subject label `cmd:<subject>`, so a second bot's tracking card is skipped silently.
- **The tail is the one launcher.** The webhook tail (`prepareWebhookTarget`, `webhooks_retry.go:152` onward) owns provenance and the fork lane (ADR-162), admission and quota, the Delivery claim under a unique index (`pkg/webhooks/mongo.go:59`), the publish grant, gate-context, supersede and the retry sweeper. It is about 600 lines, all proven in production.
- **The cloud evaluator is lossy.** It already runs on the NATSBus (`StartCloudTriggerCoordinator`, `trigger_cloud.go:442`, so ADR-046 line ~142 is stale), but on core NATS, which neither acknowledges nor redelivers. `emitForgeTriggerEvent` (`trigger_forge_emit.go:49`) emits nothing in cloud, and elsewhere emits only after a launch, marked `launched_run_id`.
- **The subscription store would lose forge rows.** `ListCandidates` filters repositories by exact case in Mongo (`mongostore.go:353`). `ReconcileManifests` (`reconcile.go:111`) deletes every derived (`drv-`) row a discovered bot no longer wants, which would include any forge row derived from its manifest. `bundle.InvocationKey` (`manifest.go:954`) returns `""` for `kind: forge`, so no forge row can be derived today.
- **The forge package cannot reach the trigger package.** `pkg/trigger` imports `pkg/forge` and `pkg/webhooks`, so the orchestrator cannot import `pkg/trigger`.
- **A crash can lock a lane.** A claim left `accepted` with no run by a crash between acknowledgement and launch is never swept: `ListRetryDue` (`mongo.go:187`) selects `launch_error` only, and `laneClaimStateOf` (`webhooks_trigger_lane.go:307`) reads the row as serving forever. A fan-out launches its targets one after another after the acknowledgement (`acceptWebhookTargets`), so it widens that window.
- **Re-provisioning rebuilds both documents.** The full Provision path builds a fresh `webhooks.Config` literal (`orchestrator.go:883-922`) and a fresh `RepoIntegration` literal (`:1012-1054`, which carries only `SyncIssuesEnabled`, `LastSyncedAt` and `MinAuthorRole`), so a field neither carries is lost on the next re-provision. The short-circuit writes only what its `changed` predicate (`:649-661`) compares.
- **A lane's claims live in two stores.** A direct target, or a board target without a coordinator, claims a Delivery row (generation rows and absorbed rows). A board target with a coordinator claims a card (`cardTriggerLane`, `invocation_dispatch.go:318`), and writes no Delivery row when it succeeds. In cloud the legacy label route is always board (`boardRouteForLabel` hard-codes it) with a coordinator (`cmd/iterion/server.go:823`), so legacy's issue-lane claims there are cards. Today each reader reads only its own mode's store.
- **Provision fences are single-use.** `WithProvisionVersion` (`repo_integration_store.go:514`) CASes on the version and `$inc`s it. `syncProvisionSchedules` receives the integration by value, and its callers never bump the version afterwards. A second fence or a whole-document `Integrations.Update` on the same version therefore conflicts.

The operator's constraints frame the decision:

- exactly one launch per bot across the two server replicas;
- `200 filtered` when nothing claims an event, and a 4xx never;
- everything the tail guarantees is kept;
- GitHub and GitLab stay at parity;
- no silent fallback;
- existing integrations keep working through the migration;
- the first deliverable fixes the issue-label lane in time for a live demo this week.

## Decision

### 1. The spine decides which bots, in the handler; the webhook tail decides how

A forge delivery becomes a `trigger.Event`, and the handler matches it, synchronously and before it answers, against the subscriptions **bound to its integration**. Every subscription that claims the event becomes one target of the existing tail. The ADR-046 sentence "the spine becomes the forge launcher" is restated: **the spine becomes the forge router; the webhook tail stays the forge effect.** A forge launch never rides the bus and never goes through `serviceLauncher`.

There are three reasons:

- **The contract needs the match.** The answer (`200 filtered`, `202 accepted`, the duplicate, a quota denial, `503`) depends on the result of matching, so matching runs before the answer anyway.
- **The Delivery row is already a durable outbox.** The tail claims a uniquely-indexed Delivery row per target before it acknowledges, saves a `RetryTarget`, and has a sweeper that replays it. That row is a per-(event, bot) outbox with a retry owner. A second outbox would mean a second retry machine and a second implementation of the tail.
- **Exactly-once stays on proven primitives.** Two replicas that race on one delivery collide on the same Delivery key or the same deterministic card id, as they do today. Earlier claims of a lane may sit in either store: a flip either way, or a mode change, moves the lane from one store to the other. Every lane reader therefore reads **both** stores before it claims in its own (§5).

### 2. A forge subscription is projected from the integration, bound to it, and selected by that binding

- **One row per (integration, bot, invocation).** `Subscription.ForgeBinding` is `"integration:<repo_integration_id>"`. A hand-created webhook, which has no integration, binds as `"webhook:<config_id>"`, so it keeps a path after the legacy router is deleted. Origin is `"forge:<integration_id>"`, the value `subscription.go:191` reserves. The id is `DerivedSubscriptionID(tenant, bot, forge, InvocationKey, "forge:"+integrationID)`. That reuses the length-prefixed fifth slot under a namespace, so every existing id is unchanged.
- **The handler selects by binding, never by repository.** It reads `ListByForgeBinding(tenant, binding)` through an index `{tenant_id, forge_binding, enabled}`. Two integrations of one repository never fire each other's bots, and the exact-case repository filter never applies to a forge row.
- **A projector writes the rows, through a seam.** `pkg/forgetrigger` holds a pure planner (`PlanIntegration`) and a `Projector`. `pkg/forge` declares the seam (`SubscriptionProjector`, field `Orchestrator.Subscriptions`). `pkg/server/server.go` wires it in the `forge.Orchestrator` literal (`:1217`), from `s.cfg.TriggerStore` and `s.roleBots`. The projector runs where schedules are projected today: the Provision short-circuit (`orchestrator.go:729`), the full path (`:1086`) and Deprovision (`:1347`). It runs inside the **same single** `Integrations.WithProvisionVersion` call as the schedule projection, never a second fence on the same version. The integration's sync stamps are written there by a targeted `$set`, not by a whole-document `Update`. It also runs from an on-demand sync endpoint, from the routing switch, and on every write or publish of a bot manifest that a spine-routed integration lists. From P2 it also runs from an elected periodic reconciler. It rewrites the derived fields: `Match`, `Mode`, `ArgsVar`, `Vars`, `OpensMR`, `Enabled`, `DisabledReason`, `ProjectionDigest`. It deletes only rows of its own Origin, so a row an operator authored against the binding is never touched.
- **A row says what it was projected from.** `ProjectionDigest` hashes the planner's manifest-side inputs: the bound bot set, each bot's derived issue invocations and manifest launch vars, and the role bots. Every write of `BotTriggers` projects synchronously, so it needs no check at routing time. The router recomputes the digest from the live catalogue, which legacy already reads on every delivery (`boardRouteForLabel` → `effectiveEntriesFor`). A mismatch is never routed on and never routed legacy: the delivery answers a typed `503 subscriptions_stale`, with an errtrack event and the sync route. The rows are a view of the manifests and `BotTriggers`, checked every time they are used (philosophy rule 5).
- **Operator state lives on the integration, not on the rows.** `RepoIntegration.BotTriggers[bot]` holds `{Labels, Enabled, Residual, LaunchVars}`, and nil keeps the current value, the same rule as `HoldLabels`. It survives a re-provision (both rebuilt literals carry it), a manifest edit that changes a row id, and removing and re-adding a bot. The Provision short-circuit's `changed` predicate compares it, and the org approval gate (`expandsProvisionSurface`) judges it: widening a bot's labels, enabling it, or flipping a lane to spine expands the automated surface. An Automations edit of a forge row writes through to it. A derived row is disabled, never deleted (a `DELETE` answers a typed 409), so forge rows need no tombstones. `BotTriggers` is keyed per bot, so its `Labels` apply to every issue row of the bot, and they are refused on a bot that derives several issue rows (see below).
- **Which bot is enabled is decided once, in one place.** Only bots that claim `issue_labeled` derive issue rows: an explicit `kind: forge` invocation, or the `forge:` block's events, in the order `buildBotRules` uses (`orchestrator.go:2000`). A row is **enabled** when one of these holds:
  1. the operator says so (`BotTriggers[bot].Enabled`);
  2. it has explicit labels (operator, `forge.labels`, or the alias in §4);
  3. it has no labels, and it is either the bot today's router elects or the only bot of the integration that claims the event. Such a row is enabled on the **residual**: every added label that no other enabled row of the integration claims explicitly (`Matcher.ExcludeAddedLabels`). A label a bot declares goes to that bot. The legacy winner keeps exactly the labels nobody else declares, which is what it serves today.

  Every other no-label row is derived **disabled**, with a `DisabledReason` that the Automations view shows. Today's election is extracted as one pure function, `webhooks.LegacyIssueWinner(cfg, implementer, reviewer) (bot string, permitted bool)`, next to `SelectBot`. `selectIssueLabeledBot` and the projector both call it, so neither can drift from the other. The reviewer fallback (a reviewer launched by an issue label) is not derived.
- **Adoption freezes the first projection.** The first projection of any integration pins each bot's derived state into `BotTriggers` (`Residual` for the winner, `Enabled=false` for a disabled bot). A label that a manifest or the catalogue gains later therefore neither narrows the winner nor enables a bot fleet-wide without an operator gesture. The studio offers "apply the manifest's suggestion". A `bot_triggers` write that sets labels and says nothing of `enabled` enables the bot: the gesture is explicit.
- **One bot claims an event through one row.** The projector refuses a bot whose issue rows' label sets may intersect: a wildcard on either side, or a literal they share, compared case-insensitively. A `BotTriggers[bot].Labels` override on a bot with several issue rows is refused the same way. The refusal is a typed projection error, and the routing switch answers 409. At match time, two matched rows of one bot give a per-bot `filtered` "ambiguous" naming both rows, never first-wins. One bot therefore never gets two targets, in one store or in two, for one event.

### 3. One matching prelude, two candidate sources

The admission rules of `matchingSubscriptions` (`evaluator.go:102`) move into shared helpers: machine-caused events, the `launched_run_id` marker, observational events, connector events, and the per-row predicate (enabled, then `Match`). The bus path keeps listing `ListCandidates`. The forge router lists `ListByForgeBinding` and calls `trigger.MatchForgeBound`, which runs the same helpers, so no admission rule can be added to one path and missed on the other. Three exclusions keep a forge row off the bus. Each has its own witness and mutation, so no two guards cover for each other:

- `ListCandidates` (memory, file, Mongo) excludes rows whose `forge_binding` is set;
- `matchingSubscriptions` skips them as defence in depth, the same shape as the connector skip;
- `ReconcileManifests` never removes them.

The matcher gains `AddedLabels`, an any-of over `Event.AddedLabels` that calls `webhooks.MatchLabel` for its grammar (`*`, a leading-`*` suffix such as `*-chaos`, case-insensitive; there is no prefix wildcard) instead of reimplementing it. It also gains `ExcludeAddedLabels`, with the same grammar: an added label is claimed when it matches `AddedLabels` (any, when empty) and does not match `ExcludeAddedLabels`. The event is claimed when one of its added labels is. It also gains `Explain(ev) (ok bool, failed string)`, so a filtered answer names, per candidate, the dimension that failed. `Event` gains `AddedLabels`. A router event's `Kind` is the manifest vocabulary (`issue_labeled`), and the provider's own name travels as `Payload["forge_kind"]`. Its `ID` is `forge:<provider>:<binding>:<laneEvent.key()>`: the forge's delivery id, or the body hash when the forge sends none. The ID contains no run id.

### 4. The manifest says which labels start a bot

- `InvocationForge` (`manifest.go:1022`) gains `labels:`, an any-of set using the `MatchLabel` grammar. It is allowed on `event: issue_labeled` only; any other event is a typed parse error.
- A `kind: forge` invocation that carries `board.all_labels` and no `forge.labels` is read as if it declared them, with a warning on the bot's `InvocationWarnings` (`pkg/botregistry/schema.go:43`): "board.all_labels on kind=forge is read as forge.labels; move it". It is warned, never refused, so a manifest that worked yesterday keeps working. When both are set, `forge.labels` wins, with the same warning.
- `ForgeRequirements` gains `labels:` for bots that claim `issue_labeled` only through `forge.events`. The catalogue's label bots are feature-dev, app-dev and instrument.
- `InvocationKey` gains a `kind: forge` case: event, sorted actions, and a 16-hex digest of the sorted lower-cased labels. A duplicate forge key in one bot warns and derives once, and is not refused at parse time. Stored team bots parsed yesterday therefore still parse.
- The two new keys (`invocations[].forge.labels`, `forge.labels`) carry a release floor, `ForgeLabelsSince`, in the syntax-floors table (`onManifestKey`, registered in `DeclaredManifestKeys`). The manifest decoder is strict (`manifest.go:1399`), so an older build refuses the whole manifest; the floor turns that into "needs iterion >= X" at authoring time.
- A bot that declares `issue_labeled` only through its `forge:` block derives a synthetic invocation that reproduces `boardRouteForLabel` exactly: board mode, `args_var` from its first command invocation or `feature_prompt`, and that command's `opens_mr` or `true`. It carries no `context_vars`, because the label route never did. A bundle with no `invocations:` block counts as one: the registry synthesizes its effective invocations from that block (`bundle.EffectiveInvocations`), and a set equal to that synthesis declares nothing. A manifest that declares by hand exactly the synthesized set is therefore read the same way, which is the board route it runs today.
- An explicit `kind: forge` invocation keeps manifest semantics. An empty `mode` means direct.

The repository-wide `LabelAllowlist` and `HoldLabels` stay **repository pre-gates**, ANDed before any bot's labels, with their semantics and their bytes unchanged. A manifest label narrows a bot; it never widens past the operator's repository gate.

### 5. A lane is per bot

`triggerLane` gains `Bot`, and the rest of the lane follows from it:

- **Label.** A bot lane's board label is `cmd:lane:@<bot>/<project>#<subject>#<trigger>`. A project path never starts with `@` on GitHub, GitLab or Forgejo, and a bot id is a slug, so the label can never equal a legacy rendering, whatever `#` a trigger label holds. Keeping the `cmd:lane:` prefix makes an old replica's `ownsCard` read a bot card as *laned and not its own*.
- **Keys.** The idempotency base becomes `ChecksumHex(<legacy string> + "|bot:" + bot)`, so the legacy `gh|issue|…|<trigger>` and `gl|issue|…|<trigger>` strings are extended rather than replaced. `laneCardID` hashes the bot through the label.
- **Trigger.** A bot's trigger is the smallest lower-cased added label its matcher claims (`trigger.MatchedAddedLabel`), a stable key that does not depend on payload order. A GitLab update that adds two labels one bot claims launches that bot once.
- **Ownership is a function of (bot, spine trigger) only.** What a lane holds decides whether it serves and what its next card id is, so it never depends on payload order. A bot lane owns:
  - its own label;
  - the legacy lane card of the **same trigger** (`cmd:lane:<project>#<subject>#<trigger>`) whose `Bot` is that bot;
  - a card from before lanes existed (no lane label) whose `Bot` is that bot or empty, within its project.

  A stale card of bot A never absorbs bot B, and a legacy card for label b never absorbs bot A's lane for label a. Two concurrent GitLab events on one bot lane hold the same set, so they compute the same next card id and collide, as ADR-170 requires.
- **Event identity may read wider than ownership.** Whether *this event* was already claimed is checked against every record that may carry its identity. Those are the Delivery rows under its legacy key, which uses the legacy trigger (GitHub: the label; GitLab: `FirstMatchingLabel(cfg.LabelAllowlist, added)` in payload order). They also include every card of the subject whose `Bot` is that bot and that carries the event's `cmd:delivery:` marker. A match is a duplicate. It never changes what the lane holds.
- **Lane reads are mode-agnostic, in both directions.** One function reads a (bot, trigger) lane and is used by every lane reader: the spine's direct and board targets, and legacy's `claimTriggerLane` and `cardTriggerLane`. It reads:
  - the Delivery chains, meaning absorbed rows and generations under the bot-scoped base and under the bot-less legacy base, the legacy rows filtered on `BotID == bot`;
  - when the server wires a board coordinator (the predicate that makes a card a claim), the cards the lane owns.

  A claim found in **either** store answers: claimed for this event is a duplicate, serving is absorbed. Only then does the target claim, in its own mode's store. On a server without a coordinator, a tracking card is not a claim and is not read as one.
  - Spine routing always reads the legacy half, so an event claimed before the flip stays a duplicate, in whichever store legacy claimed it, and a legacy claim still serving absorbs its own bot.
  - On an integration that has ever routed by spine (`SpineSince` set), legacy routing reads the elected bot's bot-scoped half in both stores, one lane per added label that passes the repository gate. Rolling back never launches twice, in either mode.
  - A never-flipped integration reads nothing new.

  A secondary chain is read and never written: its retryable rows count as serving, because the sweeper owns them.

### 6. Each launch carries its invocation's mode and vars

A target's route is built from its subscription: `CommandRoute{BotID, Mode, ArgsVar, ContextVars: sub.Vars, OpensMR}`. The mode resolves through `bundle.ResolveMode(InvocationKindForge, …)`, and `deliver` is refused, typed and per bot. The vars are layered, lowest first:

1. the issue context: the issue text under `ArgsVar`, `open_mr`, `source_issue_ref`;
2. the bot's own manifest `forge.webhook.launch_vars`;
3. the invocation's `context_vars`;
4. `cfg.OperatorLaunchVars`, read live;
5. `BotTriggers[bot].LaunchVars`, the more specific operator pin.

The operator always wins, as today. Untrusted targets skip layers 4 and 5, as `applyWebhookVarLayers` requires (ADR-162). The issue lane never produces an untrusted target, so this rule binds the later lanes (P4's fork lane).

`cfg.LaunchVars` is **not** applied to a spine target. The provisioner writes it as the union of every co-enabled bot's manifest vars, which is the cross-bot leak `forgePREventTargets` already works around (`webhooks_common.go:620`). The webhook CRUD, however, lets a team admin PATCH `launch_vars` on a managed config directly (`webhooks_routes.go:709-716`, `:790`), so the field can also hold hand pins that no manifest declares. Today those reach both the issue lane (`applyWebhookVarLayers`) and carded runs (`applyCardIntegrationVarLayers`). Dropping them would silently replace an operator's choice, so:

- the routing switch refuses spine (409 `hand_pinned_launch_vars`, naming the keys) while `cfg.LaunchVars` differs from the manifest union the projector computes. They move to the integration's `launch_vars` (`OperatorLaunchVars`, layer 4), which `force` does not bypass;
- the webhook CRUD refuses a `launch_vars` PATCH on a managed config any lane of which routes spine, with a 409 that names the integration's `launch_vars`;
- a hand-created webhook (P2 webhook-bound rows) has no manifest union, so its `cfg.LaunchVars` is projected as those rows' operator layer.

On a spine-routed config, `cfg.LaunchVars` is therefore exactly the manifest union, and leaving it out drops nothing an operator set.

On the board lane, `buildBoardCard` stamps `route.ContextVars` into `BotArgs`. A spine card carries the reserved key `__iterion_own_vars`, which is added to `native.ReservedLaunchArgs`, so no client write can set it. `liftBoardLaunchContext` lifts it into a `boardLaunchContext.OwnVars` field and strips it from the vars. Both callers of `applyCardIntegrationVarLayers` (`boarddispatch.go:2016`, `pipeline_admission.go:792`) pass that field, and under it the function skips `cfg.LaunchVars` and keeps the live operator layer. `ensureBoardCard` dedupes on the lane's label when it is given a lane, not on `cmd:<subject>`. The command lanes get the same `ContextVars` carry in the hardening phase (P1b), behind its own switch and with its own witness: one helper, every carding lane.

### 7. The answer keeps the contract

The repository pre-gates run first and unchanged. Then the router works in two phases.

- **Read phase.** For every matched target, sorted by `(BotID, SubscriptionID)`, it walks the lane in both stores (§5). A read error answers **503**, with a `launch_error` row under a random key, and nothing is claimed or metered (ADR-170 §3).
- **Claim phase.** In the same order, it creates the board cards (with the admission pre-check rolled back, as today) and inserts the direct targets' Delivery rows. The ack is flushed only once every claim is durable.
- **Every target leaves a Delivery row.** A direct target leaves its claim row. A carded target leaves an audit row with the new status `carded`, its `card_id` and its `subscription_id`, under a non-claiming key (`ChecksumHex(base+"|carded|"+cardID)`). No lane walk reads that key. `ListRetryDue`, `ListStranded`, `CountLaunched` and `laneClaimStateOf` never select a `carded` row, so the audit row neither claims, retries nor counts as a launch. ADR-046's parity gate "via the `webhooks.Delivery` audit" therefore sees every spine launch.

The answer then follows from the claims:

| Outcome | Answer |
|---|---|
| No row claims the event | `200 {"status":"filtered","reason":…}`, naming each candidate and why it did not claim |
| One target | Today's exact body |
| Several targets | `202 {"status":"accepted","delivery_id":<first claimed>,"launches":[{bot,subscription_id,status,delivery_id,card_id,run_id,error}…]}` (the existing `webhookLaunchResult` JSON, plus two fields) |
| Every target a duplicate | `200 duplicate` |
| Denials with nothing claimed | The first denial in sorted order (the existing multi-target rule) |
| A claim write fails after another target claimed | `503 {"status":"partial","launches":[…]}`, while the claimed targets still launch and dedupe on a redelivery. GitLab retries a failed delivery; **GitHub never does**, so the unclaimed bots wait for a manual *Redeliver*. A `launch_error` audit row under a random key names the bots that were not claimed, and an errtrack event is sent. The partial loss is an assumed limit |
| A spine-routed integration with no row bound at all | `503` with an errtrack event and the reason "not projected" (the same GitHub caveat applies) |
| Rows projected from a manifest that has changed since (§2 digest) | `503 subscriptions_stale`, with an errtrack event and the sync route (the same GitHub caveat applies) |

A bot the webhook scope does not allow, a disabled bot, or an unsupported mode is a per-bot `filtered` row with its reason. **No 4xx is written for a bot.** Unprojected integrations are never silently routed legacy.

### 8. Routing is a per-integration, per-lane switch the operator flips and can flip back

`webhooks.ForgeRouting{Issues, PullRequests, Comments}` takes the values `"" | legacy | shadow | spine`. It is stored on `forge.RepoIntegration`, together with `SpineSince` (the first flip), because Provision rebuilds the config. The full path carries both onto the rebuilt `Config`, and the reconciler re-projects them. A config with no value inherits the server default `ITERION_FORGE_ROUTING_DEFAULT`, which starts at legacy everywhere. Legacy is today's code, byte for byte.

`PUT /api/teams/{id}/forge/repo-bots/{integration_id}/routing` changes the switch:

- it projects, then writes the switch, inside **one** provision fence;
- it refuses spine (409, with the reason) when the lane has no enabled row, when a bot's issue rows overlap (§2), or when `cfg.LaunchVars` holds hand pins (§6);
- a flip to spine is an expansion of the automated surface, so it answers to the org approval gate like a provisioning that adds a bot. Until P2 the approval record has no seat for a routing switch, so a caller the org's gate would park gets a typed 409 `approval_required` naming the org, and an org admin makes the flip; from P2 the flip parks like a provisioning. A flip back to legacy never waits for the gate;
- from P2, it also refuses spine while the shadow audit holds an unexpected divergence or fewer deliveries than the minimum for the window. `force: true` is the greppable, audited escape hatch for this gate only.

Rolling back is the same PUT, back to legacy, and it keeps `SpineSince`, so legacy keeps reading the spine's claims (§5).

**Neither a lost field nor an image rollback is a safe revert.** A replica that predates this record rewrites whole documents. It drops `ForgeRouting` **and** `SpineSince` together, and then the new image's legacy no longer reads the spine's claims. An old image has no cross-store reads at all, and its `ownsCard` reads a bot-lane card as "laned, not mine". A flipped single-bot integration then relaunches on a redelivery, and a fresh label is not absorbed by a serving spine card. So:

- the flip is made only once a rollout has completed;
- before an image rollback past P1, every spine lane is flipped back to legacy and the rollback waits until the spine-era claims have ended (no non-terminal bot-lane card, no live bot-keyed row; the sync report, and from P1 `GET …/subscriptions`, gives the count);
- what stays open after that is an assumed limit: a manual redelivery, on the old image, of an event routed by spine.

### 9. Exactly-once is closed in the hardening phase

The demo phase relies on the unique Delivery key, the deterministic card id and the cross-store lane reads. Retries carry the spine attribution from the start: `DeferredTarget` gains `SubscriptionID` and `RoutedBy`, and the retry claim copies them onto the rewritten row. The hardening phase (P1b), which ships before any integration other than the demo canary is flipped, closes the remaining gaps:

- **Stranded issue-lane claims are re-driven, within bounds.** `ListStranded` and `ClaimStranded` (a CAS on `claimed_at`) let one replica re-drive an `accepted` row with no run that is older than a window. The window is asserted in a test to exceed the worst sequential tail (N × `launchBookingTimeout`). The re-drive runs the same attempt, with no second metering. Its scope is narrow:
  - it selects only rows the issue lane marks (`Delivery.Lane = "issue"`, written by legacy and spine from P1b on). PR-lane rows stay with their existing owners, the re-request salt (`headReviewClaim`) and the gate relaunch, so one stranded PR row never gets two relaunches. Rows written before P1b carry no mark and are never re-driven;
  - a row older than the horizon (`ITERION_WEBHOOK_STRANDED_MAX_AGE`, default 24 h) is never re-driven. It is closed as `launch_exhausted` ("stranded past the re-drive horizon"), which re-arms its lane and launches nothing;
  - before a re-drive the issue must still be open, read through the integration's forge client. A closed issue closes the row the same way. A state that cannot be read leaves the row for the next sweep, logged and sent to errtrack, never launched blind.
- **Webhook launches carry a SubmissionKey.** The key is `webhook-delivery:<delivery_row_id>#<attempt>`, answered by `runview.AnswerSubmission`, so a re-drive that races a slow live launch returns the same run. The lane keeps reading such a row as serving: it is re-driven under its own key, never re-claimed under a new generation.
- **The routed set is frozen.** A `webhook_routes` collection, keyed uniquely on `route|<config_id>|<event key>` with the Delivery TTL, holds the sorted subscription ids the event matched. A redelivery dispatches that frozen set ∩ the rows enabled now: a bot disabled since is skipped, and a bot enabled since never fires on an old event. It is a separate collection, so readers of Delivery rows are unchanged.

## Consequences

- On a flipped integration, a label starts every bot whose subscription claims it. Each bot runs in its own mode, with its own `context_vars` and its own lane, and leaves its own Delivery row (a claim row, or a `carded` audit row). The demo's nestor and saboteur coexist on one repository and one hook. #3530 and #3516 stay open until the default flips (P3): unflipped integrations, and the zero-touch `opened` lane everywhere, keep today's single-winner election until then.
- The operator answers "which bot starts on which label" per bot, in the enable dialog, the Automations view, the REST API and the CLI. The repository allowlist and hold labels mean what they meant.
- A label that several bots claim launches several runs, and several branches and pull requests. That is intended, and it is visible in the `launches` array and the Delivery audit.
- Integrations that are not flipped answer byte for byte as before. A flipped single-bot integration answers with the same shapes; only its idempotency keys gain the bot.
- Forge-bound rows appear in `/triggers`, but they are a view of what the integration projects (philosophy rule 5). Their truth is the manifest plus `BotTriggers`, and the router checks every row's `ProjectionDigest` against the live catalogue before it uses the row.
- The pull-request lane's key scheme stops depending on whether `BotRules` is present. P4 persists it per config (`Config.KeyScheme = "bot"`, written wherever `BotRules` is written and by a migration). P6 sets it in the same update that `$unset`s `bot_rules`, so removing the table never collapses N bots onto one key. This is not the issue-lane `KeyLineage` rejected below: the scheme is written once and no lane reader branches on it.
- `docs/webhooks.md`'s per-bot routing section, ADR-046 (cutover and the cloud-evaluator line) and ADR-170 (the lane carries its bot) are rewritten in the change that moves each fact.
- Retirement removes these, after the parity gates: `selectIssueLabeledBot`, `checkBotPermitted` on the issue lane, `boardRouteForLabel`, the BotRules and CommandMap matching, `DefaultBotID` routing, the routing switch, and, after a data audit, the read-only legacy chains.

## Alternatives rejected

- **Extending `BotRules` to the issue lane** (operator, 2026-11-23). It is the smallest fix for #3530, but it grows a second routing truth that ADR-046 already plans to retire, so the next lane would need the same patch. It is not the long-term target.
- **One forge webhook per bot** (operator, 2026-11-23). It breaks the unique integration index and ADR-138's immutable identities, meets GitHub's ~20-hook cap per repository, and delivers every pull-request event N times.
- **Launching forge events from the bus evaluator.** Core NATS loses an event after the ack with no redelivery. The answer could no longer say filtered or accepted. `serviceLauncher` would bypass provenance, admission, the Delivery audit and the publish grant.
- **Materializing forge effect rows in the ADR-094 outbox, with a registered forge executor that re-enters the tail after the ack.** It is the purest reading of ADR-046, and its executor registry passes the Nth-variant test. It loses on four counts. Per-bot admission, quota and duplicate verdicts disappear behind a bare 202. Flipped integrations depend on the cloud coordinator's prerequisites. Two retry machines own one delivery until a late phase. The demo phase gets its largest blast radius. It stays the seam to reach for if forge effects ever need to outlive the request (Not decided here).
- **Keeping the adopted winner on the legacy key forever** (`KeyLineage`). It makes a rollback safe for the winner, and so does the symmetric read of §5, but lineage keeps two write schemes alive and makes every lane reader branch on lineage.
- **Selecting candidates by repository, with a case-folded `repo_key` migration.** Binding by integration avoids both the casing problem and the cross-integration double fire, and needs no migration.
- **Manifest labels taking precedence over the repository allowlist.** That silently replaces an operator's explicit choice (philosophy rule 1).
- **Freezing redeliveries by comparing a subscription's activation time with the issue's `updated_at`.** It compares two clocks, misses toggles, and lets replicas disagree.
- **Answering 202 with effect ids and launching after the ack.** It hides per-bot outcomes the forge and the audit need.

## Assumed limits (stated in `docs/webhooks.md`)

- **Before P1b:**
  - a crash between the ack and a launch still strands that bot's claim;
  - a bot enabled between an event and its redelivery launches on the redelivery.
- **The flip instant.** One lane's two deliveries, handled concurrently by two replicas on either side of a flip or of a re-projected mode change, can claim in two stores and both launch. The cross-store reads close every sequential case; nothing shared by both stores exists to close the concurrent one.
- **A partial claim on GitHub.** When a claim write fails after another bot claimed, the unclaimed bots wait for a manual *Redeliver*. The `launch_error` audit row and the errtrack event name them.
- **An image rollback past P1** forgets the spine's claims. The runbook flips back and drains first (§8). A manual redelivery, on the old image, of an event routed by spine can still launch again.
- **Stale rows answer 503 until re-synced.** A manifest edit that the eager re-projection missed (a failed projection) makes a spine-routed integration answer `503 subscriptions_stale` until the rows are synced. On GitHub, that event waits for a manual *Redeliver*.
- **The stranded re-drive horizon.** A claim stranded longer than the horizon is closed, not launched, so its request is dropped visibly.
- **Rollback is not a time machine.** After a flip back to legacy, a redelivery follows legacy's election. It never launches a bot twice, but it may start the legacy winner for an event that spine routed only to another bot.
- **One label, several bots.** A label several bots claim on one issue opens one branch per bot. Chaining (a bot applying another bot's label) is not guarded before P2.
- **A pre-lane card can block its bot.** A card with no lane label and no bot (from before #3373) blocks every bot lane of its subject in its project until it reaches a terminal column.
- **A manifest label edit changes a row id.** The row's last-run fields restart, and a redelivery frozen on the old id does not launch that bot. Operator state survives on `BotTriggers`.
- **Pre-ack work grows with the bots.** Synchronous routing adds one indexed query, then per bot a lane walk and, in board mode, a board list and create, all before the answer. GitHub's ~10 s delivery budget is measured on the demo repository.

## Not decided here

- Moving forge effects to the ADR-094 outbox behind an executor registry (the alternative above), should a forge effect ever need to outlive its request.
- A replayer for failed GitHub deliveries (the hook-deliveries API), which would make "no lost delivery" hold for a crash before the ack. GitLab already retries.
- Whether the server default moves from the environment to the database-backed runtime settings.
- A default chaining guard: `Matcher.ExcludeActors` seeded with the integration's bot login, with a per-bot override. It is proposed for P2, pending the demo's needs.

## Witnesses this record owes

Each is seen red on main (or under its named mutation) on its own assertion, then green. They are listed in the implementation plan under the phase that owes them. The issue-lane core is: `TestSpineIssueLabel_TwoBotsEachStartOnTheirOwnLabel` (GitHub and GitLab), `TestSpineIssueLabel_OneLabelClaimedByBothBots_StartsBoth`, `TestSpineIssueLabel_CarriesInvocationModeAndContextVars`, `TestSpineIssueLabel_ManifestVarsDoNotLeakAcrossBots`, `TestTriggerLane_BotLanesDoNotAbsorbEachOther`, `TestSpineIssueLabel_LegacyClaimStillDedupes`, `TestLegacyAfterRollback_SeesSpineClaims`, `TestTriggerLane_CrossStore_LegacyCardBlocksSpineDirect`, `TestTriggerLane_CrossStore_SpineDirectBlocksLegacyCard`, `TestTriggerLane_CrossStore_LegacyAbsorbedRowBlocksSpineBoard`, `TestTriggerLane_OwnershipIndependentOfPayloadOrder`, `TestSpineIssueLabel_OneBotTwoRows_AmbiguousNotTwoRuns`, `TestPlan_WinnerResidualExcludesDeclaredLabels`, `TestSpineIssueLabel_CardedTargetLeavesAuditRow`, `TestSpineIssueLabel_StaleDigest_Is503`, `TestSpineIssueLabel_ReplicaRace`, `TestSpineIssueLabel_NoClaim_Is200FilteredWithReason`, `TestSpineIssueLabel_LaneUnreadable_Is503NothingClaimed`, `TestIssueLabeled_LegacyRouting_ByteIdentical`, `TestEvaluator_NeverFiresForgeBoundSubscription`, `TestReconcileManifests_KeepsForgeBoundRows`, `TestProvision_CarriesForgeRoutingAcrossRebuild`.

## Phases

| Phase | Delivers | Switch | Gate to leave it |
|---|---|---|---|
| **P1a** | The demo cut: the issue-label lane on the spine for GitHub and GitLab, exactly-once kept (bot lanes, cross-store reads, one fence, digest check). Projection runs through Provision, the sync route and the routing switch; operated by `curl`. The demo integration is flipped. Refs #3530 and #3516 | `RepoIntegration.ForgeRouting.Issues` (default legacy) | P1a witnesses red then green; `task check`, race and mongo-conformance green; the demo verified live with its cost |
| **P1** | The rest of the issue lane: eager re-projection on manifest write and publish, the subscriptions listing, the CLI, the `board.all_labels` alias (unless the demo needs it in P1a), the remaining witnesses and the docs | same | Every P1 witness; the docs and doc map regenerated |
| **P1b** | Exactly-once hardening (§9) and the `ContextVars` carry on the command lanes | `ITERION_WEBHOOK_STRANDED_SWEEP` (default on), `ITERION_BOARD_COMMAND_CONTEXT_VARS` (default off until a canary per provider) | Stranded and route-freeze witnesses; ships before any second flip |
| **P2** | The elected reconciler over every integration and hand-created webhook (P1 re-projects only spine-routed ones); per-bot labels, vars and enable in the studio, REST and CLI; operator-authored forge rows; the shadow audit, with `declared_label_claim` as an expected class; the parity endpoint; the 409 flip gate | `ITERION_FORGE_ROUTING_DEFAULT=issues=shadow` | Shadow live everywhere; every integration projected |
| **P3** | The issue lane flipped across the fleet (`--only-clean`) and then by default; #3530 and #3516 closed. Zero-touch `opened` derived for the elected bot (`issueAuthorTrusted` as a per-target refusal). One release later, `selectIssueLabeledBot`, `boardRouteForLabel` and the issue lane's legacy branch are deleted | default `issues=spine` | 7 days at zero unexpected divergences, with a minimum number of deliveries |
| **P4** | The pull-request lane: authors (`Matcher.Authors`, `ExcludeAuthors`), actions recorded but not yet enforced, debounce, supersede and the fork lane unchanged, through the same router. The persisted key scheme (`Config.KeyScheme`) replaces `HasBotRules()` in `forgeIdemKey` and `headReviewClaim` | `ForgeRouting.PullRequests` | Strict shadow parity with `resolveForgeEventBots` |
| **P5** | The comment and command lanes: `Matcher.Commands` and `CommandArgs`. Collisions are refused at projection, and an ambiguity at match time is refused, never first-wins | `ForgeRouting.Comments` | Shadow parity with CommandMap resolution |
| **P6** | Retirement of BotRules, CommandMap and `DefaultBotID` routing, after one release of dual-write; the switch itself; the read-only legacy chains after a data audit; ADR-046 closed. The migration sets `key_scheme` in the same update that `$unset`s `bot_rules` | none (removal) | No reader left; migration idempotent; two bots still launch on one PR event after the `$unset` |
