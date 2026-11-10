# ADR-131 — A tenant's OpenAI-compatible gateway is a credential: base URL + key, sealed per run, spent in and out of the sandbox, never the pod environment

- **Status**: Proposed (2026-10-21 — the plan it records was arbitrated by the operator on six points; it lands with PR-A of #2005 and is reviewed there; **Delivery** below says which decisions PR-A makes true and which wait for PR-B'/PR-C'/PR-R)
- **Date**: 2026-10-21
- **Deciders**: jo (direction, six arbitrations of 2026-10-21), Claude (analysis, design); plan reviewed cross-family by codex gpt-6-astra (16 findings, all confirmed and folded in)
- **Extends**: [ADR-121](121-adaptive-routing-policy.md) (the pair vocabulary gains `claw+openai_compatible_key`; §1 "env-funded selects nothing" narrowed to keyless runs), [ADR-122](122-gateway-models-resolved-exactly.md) (§6 "the gateway is env-funded … never to a bundle credential" superseded for a run that holds a gateway key), [ADR-129](129-per-integration-typed-routing-overrides.md) (§4/§7: the acceptance run is team-funded and seals exactly its key; override cells reach supervisors)
- **Serves**: [#2005](https://github.com/SocialGouv/iterion/issues/2005), epic [#2000](https://github.com/SocialGouv/iterion/issues/2000); the operator's decision of 2026-10-21 that the Revi bots move to the PLIAGE LiteLLM gateway (Scaleway models) one key per team, a key per repository riding the webhook pin

## Context

A deployment serves `openai_compatible/<gateway model id>` from its own environment only: `OPENAI_COMPATIBLE_BASE_URL` / `_API_KEY` are read by the registry factory, the dispatch check, the sandbox forward and the netproxy pin, and `ParseProvider` refuses the provider as a BYOK kind. A team that must spend its own gateway therefore needs a dedicated runner pool carrying the key in its pod environment (#2029, honorabilite), one pool per team, a forked bot per team. The walk classifies every gateway route as "env-funded, books on nobody": no fingerprint, no tier, no spend per credential, and `llm_fallback=none` refuses nothing on such a route. The operator wants the opposite shape: one gateway key per team, registered like any key, metered like any key, one key per repository where PLIAGE meters per virtual key, and the sovereignty posture kept by `llm_fallback=none`.

## Decision

1. **The record.** `secrets.ProviderOpenAICompatible`; `ApiKey.BaseURL` required for the provider and refused elsewhere (blank included, on update); https only, no userinfo/query/fragment, canonical (host lower-cased and in punycode, literal addresses as `net.IP` prints them, trailing dot and default port dropped, trailing `/` and every trailing `/v1` stripped — idempotent on its own output, a property a seeded fuzz asserts, so re-sending a stored URL is a no-op); the input and the STORED value each at most 2048 bytes (a name grows in punycode; the second cap reads the result); literal non-public addresses, single-label hosts (every /etc/hosts alias, `_gateway`, `instance-data`, a Service name — a public gateway has a dot) and the names a cluster, a cloud or a container reserves (`localhost.localdomain`, `kubernetes.default`, `*.localhost`, `*.localdomain`, `*.localdomain4`, `*.localdomain6`, `*.local`, `*.internal`, `*.svc`, `*.cluster.local`) refused at write — judged on the CONVERTED host (punycode first, so a full-width or invisible spelling of a refused host is the host it spells), a name that resolves to such an address refused at dial time; whitespace, non-printable characters and percent-escapes refused anywhere in the value (a pasted secret glued to a URL, an IPv6 zone), the path restricted to ASCII letters, digits and `-._~:@/` (a blank-looking letter must not glue one either; a backslash or a sub-delimiter reads as something else to a browser's parser); a host written as a number in any base (`0x7f.0.0.1`, `2130706433`, `127.1`) and the names a container resolves to itself refused with the reserved names; labels and names bounded at the DNS wire limits; a refusal names a rule and never echoes the value; every tier may hold one (team, personal, org through the org audience, platform through the allow-list) — sovereignty stays the team's `llm_fallback=none`, which skips the shared tiers; key routes decode strictly (an unknown field or a second JSON value is a 400); a `base_url` change is a **rotation**: the secret is re-sent and sealed under the new URL in the same write, under a compare-and-swap on the sealed blob that every rotation of a gateway key carries — the stored secret is never opened to follow a URL, so a principal with mutate rights who never held the secret (an org admin of the team's org) cannot aim it at a host of theirs.
2. **Identity.** The seal AAD of a gateway key binds its normalised base URL (a row repointed in the store fails closed; ids never carry the delimiter). Fingerprints are endpoint-aware for this provider only, through one helper `secrets.APIKeyFingerprint(provider, secret, baseURL)` used at create, rotate and the `base_url` change, and carried into the bundle beside a gateway key (`RunBundle.APIKeyFingerprints`) so the runner stamps what the server stamped; a vendor key's identity stays `sha256(secret)[:8]`, which the runner recomputes as it always has, and the pool and the fallback door never lend a gateway key at all — a grant that names the provider is refused by name and its lease released (the walk's vocabulary is pinned by `cloudpublisher`'s provider-coverage test, the pool's refusal by `pkg/server`'s lendability test).
3. **Repository audience.** `ApiKey.Repos` (any provider; an entry is the forge's full path of one repository, never a URL, a host or a glob) is checked in `Resolve` before the pin exemption, from the launch's repository identity stamped on the walk's context; a launch without a repository cannot spend a repo-restricted key; which audience refused travels with the withheld key (`secrets.AudienceRefusal`) into the log line and the pin diagnostic, and the preview recomputes the same predicates (`repo_filtered`, naming the launch's repository but never the key's own list — a shared-tier key's repositories are another tenant's); the facade policy's native probe reads the same audiences. The webhook lane, the HTTP repo-targeted launch, the trigger spine (`plan.Repo`, asserted by the emitter), resume and fork name a repository; the scheduled, board and pipeline lanes do not yet (PR-R stamps them from the integration) and so never spend a restricted key; a personal-source preview cannot name one. The audience is operator intent, not an authorisation boundary: the generic webhook's caller asserts `project_path`, the trigger emitter asserts `repo`, and a pledged key is served by the pledge's own rules, as `bots` is.
4. **Carriage.** `RunBundle.APIBaseURLs` / `PinnedAPIBaseURLs` / `APIKeyFingerprints`, `RunBundle.AmbientLLMRefused`; every write into the key maps goes through one chokepoint (`setAPIKey`/`clearAPIKey`, pinned by a source-scanning test); a gateway key travels only with its canonical URL and the server's fingerprint, at the seal as at the run; the pool and the fallback door release a grant they do not seal; queue `SchemaVersion` 27 ships with the producer (PR-C'), the consumers having landed with PR-B'1 and PR-B'2 (26 was claimed by the debt-landing convention).
5. **The walk.** Gateway routes record `openai_compatible` as a pinned provider; the walk vocabulary and the pool vocabulary split (the pool never lends a gateway key); "team holds a gateway key" is computed before the walk without the ceiling predicate; a run is env-funded only when every route is a gateway route AND the team holds no gateway key; under `none` a gateway route nothing of the team's own funds is refused (pool-stamped runs excepted), and the gateway slot never pays an unattributed route (#2485 closed for this shape); `DefaultPairOrder` gains `claw+openai_compatible_key` as a seal-whitelist entry, never a ladder rung; a frozen pre-upgrade list is replayed as stored with one named warning; a `ContentBoundary` stamp freezes the funding boundary on the run.
6. **Pins.** A webhook pin on a gateway key is exclusive (withheld, deleted or undecryptable → named refusal); the scheduled and board lanes rebuild the pin from the integration's webhook; reserved `__iterion_*` card keys are server-owned.
7. **Runtime.** The run's credential is served before the environment, in-process (`compatgw.NewClient` per registry instance) and in the sandbox (the pair crosses with no vendor key; the private-endpoint hatch crosses always-explicit, `""` for a tenant URL; the tenant host is a strict per-host netproxy pin admitted by a per-run rule; a run holding a gateway key is always proxied). Under `AmbientLLMRefused` every env fallback of the registry and of the provider hint returns a typed refusal, auxiliaries included (classifier, human-node LLM, companion, recovery, verified-action rungs, supervisors); server-side helpers refuse a stamped run; supervisor spend is metered (PR-B'2).
8. **Parity.** A non-claw route to the gateway is refused at launch with a typed error mirrored by the runview screen; override cells gain the `supervisor:<name>` selector; the launch screen warns when a cell or dial moves a tool-less node onto claw.
9. **Catalog and presence.** The catalog stays operator-wide (`OPENAI_COMPATIBLE_MODELS`, `_CATALOG_PROVIDER`); the cloud presence row makes a gateway key unlock claw only.
10. **Rollout.** PR-0 (patching writes, unknown-provider refusal) is the server rollback floor: a rollback to it keeps gateway rows closed (unknown provider, 409 on update) but no longer reads `repos` on vendor keys — such a key funds every repository on a PR-0 server, so the repository audience is relied upon only once PR-A is the floor; runners and pools roll first on schema 27, the server after; keys are created only after the server phase; resumable runs are drained before each data flip.

## Delivery

- **PR-A** (this ADR's landing): §1 except the dial-time check of resolved addresses (PR-C'); §2 at the key routes (create, rotate, the `base_url` change); §3 (the gate, the diagnostics, the preview, the native probe; the scheduled, board and pipeline stamps are PR-R's); the studio create form and list; the CLI flags. Two guards hold the inert state honest until the walk spends the key: a webhook pin on a gateway key is refused at write (PR-C' lifts it and makes the pin exclusive, §6) and the pool refuses to lend one (§5, permanent).
- **PR-B'1** (consumer first, inert at v25): §4's carriage on the consuming side — the bundle gains the URL maps paired with the key maps and the ambient-refusal flag, absent on a bundle sealed today (the fingerprint map is written only beside a gateway key, so a bundle of vendor keys serialises byte for byte as before); the seal refuses a gateway key travelling without its canonical URL or the server's fingerprint; the publisher writes its key maps through ONE chokepoint (`setAPIKey`/`clearAPIKey`, pinned by a source-scanning test) that carries the URL and the fingerprint with the key; the pool and the fallback door refuse a gateway grant by name and release a grant they do not seal (unreachable by construction today — the pool vocabulary excludes the provider — defence in depth); the runner reads the pair, the flag and the server's fingerprint. No queue schema change: the bump is the producer's (PR-C'), and `pkg/queue` is untouched.
- **PR-B'2** (still inert, lands BEFORE PR-C': the runner must consume what the producer will emit and fence what it must refuse): §7's consumer half — the registry's uncached gateway arm reading the run's pair first, the typed ambient refusal at every environment fallback and every auxiliary (classifier, interaction LLM, companion, recovery, verified-action rungs, supervisors with the stamp-aware filter), the sandbox crossing always-explicit with the netproxy pin and the always-proxied rule, supervisor spend metered — and §7's server-helper refusal with the `ContentBoundary` stamp's carriage. **PR-C'**: §4's producer (the walk seals the pair, `SchemaVersion` 27), §5, §9, the pin exclusivity of §6. **PR-R**: §6's lane stamps and the §8 parity. Each amendment appended to ADR-121/122/129 names the PR that makes it true.

## Alternatives considered

- A sovereign runner pool per team (the honorabilite recipe): proven, but a Deployment, a Secret, a chart entry, a mapping and a forked bot per team — linear cost, rejected by the operator for the general case.
- One shared gateway key on the generic runners plus per-integration routing: cheapest, but one LiteLLM identity for every team (no per-team metering or quota) and the key in every sandbox — rejected.
- Catalog hints on the record: a per-run table and a run-aware pricer; deferred.
- Secret-only fingerprints: a token reused across gateways or tenants would share a ceiling, `alive_runs` and the spend ledger; rejected after the astra review.

## Consequences

- A team's gateway key is a first-class credential: registered, listed, rotated, audited, metered, pinned per repository and restricted to repositories, with the ceiling as advisory as for every key.
- The honorabilite pool, its forked bot and its chart entry become unnecessary; they are retired after the migration runbook.
- Old runners never serve a team-funded route from their environment: schema 26 fences them; old servers cannot corrupt a gateway row: PR-0 patches and refuses.
- Known limits, filed as tickets: hard per-key concurrency cap; trigger-spine pins; server-helper credential channel and the N-pools guard; local `iterion models` gateway row; gateway capabilities (#2103); studio editor for pins; `review-pr` claude-slot tools on claw beyond PR-R.

## Amendment — 2026-11-08: pools lend gateway keys through an attachment (ADR-164)

§5's "the pool never lends a gateway key" and §2's "the pool and the fallback door never lend
a gateway key at all" are AMENDED by [ADR-164](164-configurable-key-pools-attach-per-scope-with-a-fallback-strategy.md),
not rewritten: a NAMED, ATTACHED pool may hold `openai_compatible` entries, and the attached pool
tier seals them as the PAIR this ADR already defined (key + canonical URL through the `setAPIKey`
chokepoint, the seal's AAD binding the entry's own URL, the endpoint-aware fingerprint riding
`RunBundle.APIKeyFingerprints`). A gateway grant without its URL is refused and released — the
seal refuses it independently. The DOOR keeps its by-name refusal (the pool tier is the only
gateway lending path); `openai_compatible` stays out of `DefaultPairOrder` and
`KnownRoutingProviders` — it enters the pool wants only through the attachment. The legacy
(empty-bundle) pool tier keeps refusing gateway grants by name, byte for byte: the two
postures coexist, and the tests pinning them (the lendability refusal, the provider-coverage
walk, the whitelist closure) pass unchanged — they pin the legacy half; the attached half is
pinned by the attached-tier suite. PR: #3114.

## Amendment — 2026-11-10: the walk spends a team's gateway key (#2005 PR-C')

§5's walk lands. The provider vocabulary is two lists, one derivation:
`model.KnownRoutingProviders` is `model.KnownPoolProviders` plus `openai_compatible`.
The credential walk, its funding refusals, the allowlist gate and the preview read the
first; the pool wants, the legacy pool and the fallback door read the second. "The team
holds a gateway key" is a lazy probe of the tiers the walk consults (the team's own;
the org's and the platform's unless the team is sovereign or pool-mapped), under the
audiences `secrets.Resolve` applies and without the usability predicate; it attributes
the routes only to a key that can seal for the run — the run's frozen lists admit the
gateway slot and the key's sealed secret opens. A held key that cannot seal leaves
the routes to the pod's gateway (§5's replay clause), named on the env-funded line and
in every unfunded refusal naming the provider, as is a tier read as holding one
because its key list failed; the sovereign posture keeps attributing. Its answer
rides `model.RouteView.GatewayKeyFunded`, under which a decided gateway route records
the `openai_compatible` provider pinned at its site; an env-templated route stays
undecided (ADR-129 §4). Under `none` the sovereign posture attributes the gateway
routes outside a runner pool, so the unfunded refusal names the one nothing of the
team's own funds. The 2026-11-08 amendment's "`openai_compatible` stays out of
`DefaultPairOrder` and `KnownRoutingProviders`" is superseded: `claw+openai_compatible_key`
closes the default as a seal-whitelist entry (`llmroute.SealOnly` — the ladder
composer drops it, `StageModel` maps it nowhere), and the walk's vocabulary names the
provider. The pool's does not, so an attached pool still brings the gateway want in
through the attachment alone, and law 1 holds: a team key fills the slot first. The
gateway pair seals for a run that ATTRIBUTES a gateway route to a key, or — under a
dedicated runner pool (D13), whose pod carries the operator's gateway dials — for a
run with a route the walk cannot decide (not narrow-safe, or an env-templated
`${DIAL:-…}`). A default-runner run with undecided routes never seals it: its dials
do not expand to the gateway, and the pair would start the run's sandbox proxy,
reserve a slot of the key's concurrency cap and open the gateway host in an
untrusted run's fork lane for a key no route spends (operator arbitration, round 1
of the PR-C' review). One predicate (`gatewayRoutes.sealable`) gates every tier, the
attached pool and the preview. The frozen pre-upgrade list is `store.RunLLMRoutePolicy.PairOrder` (its twin
`store.Run.PinnedProviders` is frozen beside it), replayed as stored on a resume; its
one named warning is `pairOrderRefusals`.

### The content boundary producer (#2005 PR-C', same date)

§4's stamp has its producer. A launch's credential walk decides the run's
`store.Run.ContentBoundary` after the tier walk — the last step that may seal the
gateway key — and before the fallback door and the policy ladder, which compose the
crossings it refuses. The run is `team_gateway` when the walk ATTRIBUTES a gateway
route to a key (the keyed view's attributed routes name `openai_compatible`) AND the
gateway key sealed, in either channel; a mixed run, its vendor routes funded beside
it, is `team_gateway` too. Every other launch the walk resolves is stamped
`own_keys` explicitly, never left nil. An env-templated route is never attributed
(ADR-129 §4), so a pool-mapped run whose gateway key seals only for such a dial
stays `own_keys`.
`KeyID` is the id of the key row that filled the gateway slot, recorded beside the
fingerprint at every seal site (`noteSealedAPIKey`: the team's BYOK tier, a webhook
pin included, the restore, the org and platform tiers, an attached pool's entry);
`GatewayHost` is the host its sealed URL dials (`compatgw.Config.Host`, the host the
runner pins on the run's proxy).

The launch writes the stamp through `credResolution.applyTo`, outside `stamp()`: a
resume replays the document's bytes (`boundaryFrozen(prior.ContentBoundary)`) and
never re-derives them. `RunBundle.AmbientLLMRefused` is armed on the same value —
derived at launch, the frozen one at resume — ORed with ADR-162's untrusted arm,
never clearing it; a resume replaying `team_gateway` seals its bundle even with
nothing in it, so the refusal keeps its carriage when the team's key is gone. The same
value arms the unfunded refusal strictly: a `team_gateway` run's route nothing in the
bundle funds can only die at the runner, so it is refused by name at launch (or resume)
whatever `ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL` says, with the boundary's own sentence
(`UnfundedProvidersError.TeamGateway`); the preview mirrors it.

Under `team_gateway` the policy ladder lands no stage on a node whose model routes to
the gateway: `llmroute.StageModel` refuses it with `ReasonContentBoundary`, since the
wholesale crossings (`claw+openai_key`, codex's forfait and key) would carry the
node's content to a vendor credential. The fallback door's probe screens the same
way, so no donor is asked for such a crossing. The publisher's screen, its door, the
credential preview and the runner's authoritative screen (`ExecutorSpec.ContentBoundary`,
and the usage-cap pre-flight's escape estimate) all read the boundary the run
carries. The vendor-routed nodes of a mixed run keep their rungs: the arbitration
refuses a mixed run's auxiliaries (classifier, supervisors, server-side helpers),
not a node's own vendor route and its fallbacks. An unstamped run (the operator's
environment gateway, a run launched before the producer) keeps the crossing.

### The schema bound (#2005 PR-C', same date)

The producer ships with queue `SchemaVersion` **30**, not 27: 26 to 29 were
claimed meanwhile (ADR-130's `env_expansion`, the donor ceiling, the bot-source
tier, ADR-162's fork-author subject). §4's and the Delivery's "`SchemaVersion`
27", §10's "schema 27" and the Consequences' "schema 26 fences them" read 30. A
stale runner that dropped the stamp would serve auxiliary gateway spend off the
team's boundary, so it must reject — it does, its window is [27, 29].
`MinSchemaVersion` stays 27: the fence is the maximum that rises, and a v27 to
v29 message carries no stamp because its publisher wrote none, which is the
unstamped reading; raising the minimum would force a drain for no added safety.
Runners and pools roll first, the server after (§10): with the minimum unchanged
no trusted message parks in either direction; the v29 rule still parks an
untrusted message older than v29 (docs/cloud-queue-schema-rollout.md, v29 and
v30).

### The pin's exclusivity (#2005 PR-C', same date)

§6's pin lands; the Delivery's write-time guard is lifted. `validateKeyOverrides` admits a
pin on an `openai_compatible` key like a pin on any key of the team — its spendability arm
stays, so a user-scoped gateway key is still refused under the lane's synthetic actor. The
exclusivity is ONE chokepoint in the walk (`gatewayPinRefusal`), asked after the tier walk and
before the content boundary is derived: when a run that may ride a gateway route
(`gatewayRoutes.mayRide` — a decided gateway route on the keyed reading whatever the probe
found, or, under a runner pool, an undecided route) carries a gateway pin, its gateway slot must hold the very
key the pin names, or the launch is refused with `GatewayPinRefusedError` naming the key and
why: missing (deleted), withheld by its bot or repository audience, invisible to the launch's
identity, undecryptable, or held out by the run's providers allowlist or frozen pair order.
It reads what the walk SEALED, so the shapes the walk would otherwise end in — the team's
other gateway key, a shared tier's, an attached pool's entry, or an env-funded run whose
walk consulted no tier once the pinned key was out of the probe's sight — are all refused
the same way. A resume replays the run's pins through the same walk and is held to the same
rule. A run that rides no gateway route leaves the pin idle. `warnRefusedPins` keeps its
warn-only lines for vendor pins and says nothing of a gateway pin's fate. The rule holds
wherever a pin reaches the walk (the webhook lane, the trigger spine, a resume); §6's lane
stamps for the scheduled and board lanes stay PR-R's.
