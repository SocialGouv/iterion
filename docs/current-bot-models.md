# Catalog model defaults and deployment

As of September 2026, catalog Anthropic routes default to `claude-opus-5-5`.
Fable, Sonnet and Haiku are no longer default choices. OpenAI routes use the
GPT-6 family:

| Role | Default |
| --- | --- |
| General OpenAI work, Copi entry/reflection, implicit Codex/claw selection | `gpt-6-sol` |
| Advanced GPT review and Evolve work | `gpt-6-astra` |
| Lightweight OpenAI work, available as an explicit route override | `gpt-6-luna` |
| Anthropic work, review, routing, supervision and CLI auxiliary calls | `claude-opus-5-5` |

These are defaults, not an allowlist. Existing explicit model/env/per-run
choices remain valid, including legacy IDs and the independent GLM/Kimi/Grok
routes. Some bots deliberately use those other providers. Revi's current
catalog reviewer pins use GLM; a deployment's GPT reviewer route is chosen
through its existing overrides. The CLI auxiliary defaults are applied only
on the direct Anthropic route, after credential and per-task env resolution.
Facade gateways and Bedrock/Vertex/Foundry keep their own model IDs. Process
and task env settings, including explicit empty values, retain precedence.
Container-only env settings invisible to the runner are not inspected.

The application image pins Claude Code 2.1.280 and Codex 0.156.1; the security
image and its Codex bootstrap use the same Codex pin. These versions include
[Opus 5.5 support](https://code.claude.com/docs/en/model-config) and
[GPT-6 Sol/Luna support](https://developers.openai.com/codex/changelog).

## Transport and output contracts

Opus 5.5 always uses adaptive thinking and rejects forced tool choice. Its
structured path therefore supplies the existing synthetic schema tool with
`auto` and a final instruction naming it. Plain text, another tool and invalid
JSON fail the existing parser; a DSL `json` field still accepts arbitrary
nested keys. Native strict JSON schemas cannot preserve that open-object
contract. Tool-equipped agents must actually start an allowed tool before
returning a verdict; announcing a tool, an unknown tool, malformed arguments
or exhausting the step limit without execution does not satisfy the guard.
An executed tool returning an error still counts as execution.

The SDK strips unsupported sampling and rejects explicit incompatible thinking
or forced-tool controls before network I/O. Legacy explicit IDs keep their
previous behavior. GPT-6 uses Responses for both API-key and OAuth clients,
including requests inheriting a client model; other compatible providers keep
their existing transport. Sol/Luna also accept effort `none`; Astra does not.
All three accept low, medium, high, xhigh and max at the provider API. These
capabilities do not extend the DSL effort vocabulary: `none` is not yet
authorable there, and CLI adapters retain their existing effort mapping.
Explicit authored efforts and token limits remain unchanged. Opus 5.5's
implicit effort is medium.

Offline context/output limits are 1M/128k for Opus and 1.05M/128k for GPT-6.
The static cost fallback has nonzero current short-context rates; it remains
an estimate, not a long-context or cache-aware invoice. Live pricing sources
still take precedence. See [OpenAI models](https://developers.openai.com/api/docs/guides/latest-model)
and [Opus migration](https://platform.claude.com/docs/en/models/opus-5-5/migration-guide).

## Roll out code before configuration

1. Deploy the engine containing the reviewed SDK update and new CLI versions
   before assigning new model IDs to cloud bots. On digest-pinned installs,
   update both `image.digest` and `runner.image` in GitOps and verify the
   deployed images: a restart or tag change alone keeps the old binary.
   See the [deployment runbook](cloud-deployment.md).
2. Snapshot platform bot-vars, platform and team bundle overrides, and the affected active,
   queued and resumable runs. Classify operator overrides explicitly; do not
   replace every old-looking value without its owning workflow's context.
3. **Bot-vars are live**, resolved again when a node is constructed. An immutable
   bundle does not freeze `${ITERION_*}` values. A resume also reloads the cloud
   source and checks its hash. Do not change global variables while a reserved
   campaign, DSL v2 or Argus run depends on the affected keys. Wait for that work
   or isolate its workers/configuration through an agreed deployment operation;
   do not edit its snapshots or cancel it to force a migration.
4. Review the exact prepared diff, re-read the current source immediately before
   applying it and abort if it changed. Preserve gates, funding routes, and
   explicit overrides. Test one canary per actual transport/credential route
   used in that deployment, then inspect errors, tool execution, publication
   and the merge gate separately. A completed review does not prove a posted gate.
5. Update remaining eligible defaults and record the applied values and evidence
   on the tracking issue. Keep a before-snapshot for rollback under the same
   active-run precautions. Do not declare the migration deployed while global
   defaults are deferred.

Source precedence is always **team override → platform override → baked
catalog**, independent of version. Deploying an image does not update or
remove a stored bundle. Reconcile each affected override against its own
snapshot, retaining its customizations; do not delete forks to force the
baked version. The administrative CLI lists/pulls/pushes platform bots
(`iterion remote admin bots`); for a guarded replacement use
`PUT /api/admin/bots/{slug}` with the complete `files` map and the last-read
numeric `version`. Team replacements use
`PUT /api/teams/{id}/bot-sources/{slug}` with the same payload. A concurrent
write returns 409; re-read and review the diff instead of dropping the token.
The CLI whole-bundle push currently omits that concurrency token.

Changed catalog bundles bump their manifest patch versions so built-in
marketplace entries refresh and newer baked sources can be signalled beneath
overrides. This semantic version is distinct from the storage revision above.
It does not change source precedence or upgrade a team fork automatically.

## Evidence and repeatable checks

`go test ./bots -run TestCatalogCurrentModelDefaults -v` compiles every shipped
catalog unit, including imports, and inspects model, interaction, fallback,
recovery and supervisor routes using the real default-expression resolver.
The backend tests separately cover implicit defaults, profiles, usage retention,
legacy overrides and the execution guard. No LLM calls occur in that layer.

For a bounded real compatibility probe (synthetic data, memory-only tool):

```sh
ITERION_LIVE_CURRENT_MODELS=anthropic/claude-opus-5-5,openai/gpt-6-astra,openai/gpt-6-sol,openai/gpt-6-luna \
  devbox run -- go test -tags live ./pkg/backend/model -run '^TestLiveCurrentModels$' -count=1 -v
```

On 2026-09-25, all four passed a two-turn tool loop and dynamic JSON output,
with 2048 output tokens per call and low effort. Total reported token-based
estimate: about $0.0087. This single sample per path validates provider acceptance
under the local registry credentials, not production capacity, every credential
route, or absence of regressions under long context. The SDK event interface
does not expose the provider's returned model ID; the probe records the requested
spec and never invokes a fallback. API-key/OAuth wire payload parity is also
covered by deterministic SDK tests. Claude Code's new auxiliary env is tested
through actual spawn builders with fake processes; its live subagent behavior
is not re-proven by this direct-API canary.

The cross-family plan review (Opus 5.5, reported list-price cost $0.992252)
resulted in forced-initial-tool handling, CLI auxiliary defaults/version pins,
API-key transport tests and deferred live-variable rollout. Its proposed native
strict JSON replacement was declined after a probe showed it loses arbitrary
DSL JSON keys. Dropping all thinking blocks was not demonstrated to cause a
provider rejection; the existing conversation behavior is preserved. Speculative
budget increases and an arbitrary ten-sample gate were not adopted; actual paid
probes are reported above. A provider error remains a failure, never evidence of
successful migration or a reason to switch silently to a legacy default.
