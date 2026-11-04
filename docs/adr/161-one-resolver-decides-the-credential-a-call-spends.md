# ADR-161 — One resolver decides which credential a call spends; dispatch, admission, meter and plan read its answer verbatim

- **Status**: Proposed — the operator chose the structural change on 2026-11-04 (debt-leap #107, option A: one resolver, a short ADR, a new lane, the #2022 branch as witness bench). Design only. Decisions 5, 6, 8 and the ordering in 9 wait for the operator; 1–4 and 7 are proposed.
- **Date**: 2026-11-04
- **Deciders**: jo (direction, #107), Claude (analysis, design)
- **Extends**: [ADR-131](131-tenant-gateway-credential.md) §7 (the stamp's typed refusal is decided once, by the resolver)
- **Relates to**: [ADR-085](085-pi-as-execution-backend.md) · [ADR-132](132-a-run-reads-no-environment.md) · [ADR-155](155-reserved-capacity-is-a-durable-allocation.md) (units 4 and 6 rebase on U2) · #2288 (`CredentialPlan`, amended by 8)
- **Serves**: [#2022](https://github.com/SocialGouv/iterion/issues/2022); witness bench `leap/usagecap-ambient-key` at `6e2389644`, based on v5.3.1, 301 commits behind main at `5ff39279f`

## Context

Four readers answer "which credential does this call spend?", each its own way (sites on main; † added by the bench):

- **Dispatch** — claw `Registry.ResolveWithContext` and the crossing (`forwardableProviderEnv`), re-walked in the container; claude_code `selectedAnthropicCredEnvForCLI`; pi `piResolveEnv`; codex `codexCredEnvForCLI`.
- **Admission** — `admitSandboxCredentials` (#1768, fork lane only) over three probes that recompose the dispatch's env and parse the hint themselves.
- **Meter** — the pre-flight's `routeKey`, `supervisorKey`, `clawUnslottedKey`†, `AmbientEnvMeter`†; the ledger's `credentialSlotForRoute`, `planProviderFor`.
- **Plan** — `model.CredentialPlan` (#2288, landed 2026-11-04): the publisher's frozen `(node, provider) → fingerprint` map, joined by the `provider` word the executor stamps; the pool's fine table books donors from it. It never sees the pod.

Each #2022 review round (fleet t49, t50) found a new disagreement:

- **meter ≠ dispatch** — a stamped sandboxed claw walk metered nothing while the container spent the pod's key (t49 r1); under `ITERION_FORBID_SUBSCRIPTION_OAUTH` the pre-flight parks a run on the pod's account for a node `GuardSubscriptionOAuth` refuses (t50 r2, **open**).
- **crossing ≠ in-process**, **meter ≠ meter** — a z.ai key beside a forfait refused a node the forfait funds (t49 r2); the crossing judged the forfait on the raw `ANTHROPIC_BASE_URL`, and the route and supervisor meters disagreed behind it (t50 r1).
- **admission ≠ dispatch** — the probe splits the hint on its first comma, the dispatch parses the chain (t50 r2, **open**).
- **backend ≠ backend** — pi and codex read no stamp (t49 r2, deferred).
- **plan ≠ dispatch** (this ADR's review, t50 r1's row read against main) — on a pool-served run, a claw `anthropic/claude-*` node, the donor's forfait, a gateway `ANTHROPIC_BASE_URL` beside the pod's `ANTHROPIC_API_KEY`. The call spends the pod key; the plan maps `node|anthropic` to the donor, who is booked. #2288 fixed the class inside its own three derivations (`258bc2fbf`): a fifth answer.

Each fix missed a reader the next round found: the Nth-variant test failing (philosophy 2). Hint-blind auxiliaries ask too (`anthropicFunding`, `ctxFundsProvider`, `forfait.Check`, ADR-131 §7's list). The stamp (`RunBundle.AmbientLLMRefused`) has no producer before #2005 PR-C': what it binds is free to decide today.

## Decision

1. **One resolver.** `pkg/backend/credroute`: `Resolve(ctx, Call) (Spend, error)` over a registry of per-backend **arms**. An arm is its backend's precedence moved out of the dispatch verbatim, registered from its own package; a backend with no arm answers `no-arm` (7), never an engine `if`.
2. **Inputs.** `Call`: backend, spec, ONE leg's hint, role (node, supervisor, auxiliary), sandbox (none, or the host-state mount admission resolves today, `SandboxHomeState`). Legs come from the dispatch's parser (`resolveProviderChain`), exported as `credroute.Legs`. Run: `secrets.Credentials`. **Posture**: the pod environment, whole, with each name's provenance (`envtrust`: inherited or planted), captured once per attempt and carried through the #2808 seam (`envGetenv`). No name list can drift: the OpenAI, xAI, Moonshot and Azure wires, the cloud switches and the private-endpoint hatch read like the Anthropic channels. In process a planted value funds (the local lane spends a project `.env`); across the sandbox it never does (D20, ADR-132).
3. **Output.** `Spend{Channel, Slot, Fingerprint, BaseURL, Source}` (Channel: run key, pinned key, run forfait, gateway pair, pod env, host login, own config; Source in `slotOfSource`'s grammar), or a refusal (7). The dispatch builds the client and the spawn env from it and never walks again. The crossing hands the container the Spend and the env today's per-name classes forward (routing names, header identity lines, the hatch, the forbid switch, planted and stamp-withheld names as explicit empties). Inside, the runner calls the same `Resolve` over what crossed: the node's call must land on the handed Spend or refuse, and a subagent's own spec (the `agent` tool) resolves by the same arm. The meter key stays the meter's: its scope (tenant, org, pinned) × the Spend's slot and fingerprint.
4. **Readers.** *Dispatch* builds from the Spend and stamps it on its usage events (8). *Admission* resolves the primary-chain legs (`provider:` elements, which fall through on any error) of every node and supervisor: a node with none resolving is refused, all such nodes named, sorted; a refused `fallbacks:` entry is warned. It judges fork-lane runs and every stamped run, whatever its trust. *Meter*: the pre-flight keys the Spend; `defaultKey` stays for a route the walk cannot read. The publisher's walk and `CredentialPlan` keep predicting what to seal, sharing only the leg grammar (`ir.SplitProviderStep`).
5. **(a) What the stamp binds — operator.** (i) *Recommended*: every backend, as the bundle field reads ("funded by the credentials of this bundle ONLY"). pi and codex refuse the pod's money. kimi, grok and opencode read no run credential, so their stamped legs are refused unconditionally: a node made only of them refuses the launch, a `fallbacks:` rung is warned and never serves (feature-dev's `kimi_quota`, copilot's kimi and grok). (ii) claw and claude_code only (ADR-131 §7's letter, the bench): the rest spend and meter the pod. (iii) (i) for pi and codex; own-config backends admitted with a named warning, metered on the pod. No run is stamped before PR-C'.
6. **(b) pi's pod money under the stamp — operator.** (i) *Recommended*: everything not sealed in the run — `piAnthropicEnvChannels`, every name `piCredentialEnvNames` forwards, pi's own store (`auth.json`, `ITERION_PI_AGENT_DIR`); the spawn gets explicit empties, the bound key, an empty agent dir. (ii) The same, but an operator-pinned `ITERION_PI_AGENT_DIR` may serve. (iii) The Anthropic channels only; thirty-odd provider keys stay spendable. The refusal's provider coverage follows.
7. **(c) The refusal — proposed.** Scope: one leg of one call; chain semantics unchanged. Type: (i) per-backend types as today, which every reader must learn; (ii) *recommended*: one `*credroute.Refusal{Backend, Provider, Spec, Leg, Reason}` unwrapping to the error each site raises today (claude_code's stamp refusal stays an `*ErrNoFacadeCredential`), so every `errors.As` keeps matching; (iii) one type, no unwrap, every reader edited. Reason: ambient-stamp, subscription-switch, hint-unfunded, no-credential, no-arm. Admission keeps `SANDBOX_CREDENTIAL_UNREACHABLE` for no-credential; stamp and switch get their own codes and remedies. The meter reads a refusal as not metered. `GuardSubscriptionOAuth` becomes an arm refusal, closing t50 r2's open MEDIUM.
8. **(d) What the ledger and the fine table learn — operator** (amends #2288). (i) claw's events carry the hint, the ledger re-derives: one more derivation. (ii) Main's join, `provider` word to plan row: the plan ≠ dispatch row stays. (iii) *Recommended*: every usage event carries the Spend's `credential_source` and `fingerprint` beside `provider`, written by the process that built the call (claude_code's `fingerprint` already is this value). The ledger and the fine table book on it. The plan stays a prediction: a disagreeing observation books on its fingerprint and warns. An event without one is a miss: booked on nobody, the fine table voided to the coarse booking (#2288's rule).
9. **Migration — from main, dispatch first, claw first.**
   - *U0*: rebase the bench onto main (it and #2288 rewrote `registry.go`, `claw_backend.go`, `loop_cred_spend.go`; each conflict becomes a parity row). Each row (posture with provenance × run credentials × backend × spec × leg × sandbox × stamp × plan) asserts that the built client or crossed env, the admission verdict, the meter key and the booked fingerprint agree; the two open t50 r2 rows and the plan row are seen red.
   - *U1, claw*: the registry walk, the crossing and the switch guard move into the arm; `ClawSandboxCredential` becomes `Resolve`; the bench's witnesses keep their assertions.
   - *U2, the meter*: the pre-flight on the Spend, the source and fingerprint on claw's events, the fine table on the fingerprint. U0–U2 land as one PR (no red row lands); #2022 closes. ADR-155's units 4 and 6 rebase on it; its other units start from main.
   - *U3* claude_code; *U4* pi and codex under 5 and 6, whose rows alone change expected values; `credentialSlotForRoute` and `planProviderFor` go. *U5* the auxiliaries.
   - *U6*: the #2808 seam's source scan widens to `pkg/backend/delegate` and the meter, and its process-scoped sites (the registry factories, `OpenAIForfaitServes`) move behind the arms: no env read on the LLM path bypasses the posture.
   - **Ordering with #2005 — operator.** Recommended: PR-C' after U4, so the first stamped run meets one answer on every backend.

## Consequences

- A new posture or credential kind is one arm edit plus its parity rows.
- The meter and the fine table book only what the call spent. Admission cannot admit a node the dispatch refuses on the runs it judges; a trusted unstamped run's refusals still arrive at dispatch.
- The container resolves through the same arms over what crossed, and its node call is checked against the host's Spend. Behaviour changes only where a parity row says so.
- Usage events gain `credential_source` and `fingerprint`, additively; the re-derivations are deleted, not kept as a fallback.
- The bench does not land as it stands: rebased in U0, its witnesses are the parity spec; ADR-155's wait on it moves to U2.
- Out of scope: the publisher's walk, the plan's derivation, the pool's lending.
