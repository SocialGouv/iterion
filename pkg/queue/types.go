// Package queue defines the message contract exchanged between the
// iterion server (publisher) and the iterion runner (consumer).
//
// Today only the type definitions live here — the NATS publisher /
// consumer impl lands in plan §F T-25 (`pkg/queue/nats/`). Keeping
// the schema package separate is deliberate so studio backend tests
// can import the types without pulling in the NATS client.
//
// See cloud-ready plan §C.2 for the wire format and §J for the
// rationale behind the IRRef fallback.
package queue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// SchemaVersion is incremented at every breaking change to the wire
// payload. Producers always set RunMessage.V = SchemaVersion; consumers
// reject any V outside [MinSchemaVersion, SchemaVersion] in both directions,
// so a rolling upgrade has to choose which side rolls first — see the
// ordering bullet below.
//
// Wire compatibility policy (enforced — see docs/cloud-queue-schema-rollout.md):
//   - Deploy the server (producer) first by default. Both orders can park a
//     run, and a resume recovers it — not at the same moment. Old runners
//     rejecting the new version park runs a resume recovers as soon as the
//     fleet is upgraded; new runners rejecting a version below their
//     MinSchemaVersion park runs a resume recovers only once the server
//     publishes a version they accept. Roll
//     the runners first only when nothing below MinSchemaVersion(new) can
//     still be queued — automatic when the bump leaves MinSchemaVersion
//     alone, otherwise a check against the queue, never against the old
//     server's SchemaVersion. A mismatch in
//     either direction is TRANSIENT, never terminal: the consumer holds the
//     message with a delayed Nak and, once MaxDeliver is exhausted, parks it
//     on the DLQ with the run document flipped to an actionable status —
//     never dropped, never left `queued` in silence.
//   - Any ADDITIVE field whose omission changes operator intent (a knob the
//     caller explicitly set, that a stale runner would silently fall back
//     from) is a BREAKING change: bump SchemaVersion. The publisher must
//     reject a launch carrying such a field until both the wire carrier and
//     its version bump ship; it must never silently drop the field.
//
// v=3 (2026-06-10): added BotID so cloud runners can qualify structured bot memory.
// v=4 (2026-07-11): added Budget so launch-time budget overrides reach the
// runner instead of being rejected at publish time. The version bump makes a
// stale runner reject the message loudly rather than silently dropping the
// caps the caller asked for.
// v=5 (2026-07-20): added Contributions so enabled-plugin skills and DSL
// `skills:` library references reach the runner pod, whose iterion home is
// empty. Bumped for the same reason as v=4: a stale runner must reject the
// message loudly rather than run the workflow WITHOUT the skills it was
// launched with — a run missing its deploy/platform skill still "succeeds"
// while doing the wrong thing, which is the exact façade this field prevents.
// v=6 (2026-08-05): added AutoMemory so a launch-time `--auto-memory` decision
// reaches the runner. Bumped for the same reason again, and here the direction
// matters: dropping the field makes the run fall back to the workflow's own
// value, so an operator who asked for `off` on a bot whose DSL says `on` gets a
// run that reads and writes shared memory anyway — the one thing the knob
// exists to prevent, failing OPEN and in silence.
// v=7 (2026-08-11): added LoopBudgetGuard so a launch-time
// `--loop-budget-guard` decision reaches the runner. Same direction of
// failure as v=6, one layer down: dropping the field makes the run fall back
// to the workflow's own value, so an operator who asked for `off` on a bot
// that says nothing gets the guard anyway — and one who asked for `on` on a
// bot declaring `off` gets a run that can still strand its work at the cap.
// The knob decides whether a loop stops early or dies at its ceiling, which
// is exactly the kind of choice that must not be quietly re-made on the pod.
// v=8 (2026-08-25): added Supervisors so a launch-time `--supervisors off`
// reaches the runner. Same failure direction as v=6/v=7: dropping the field
// makes the pod re-decide from its own env and spawn LLM watchers the operator
// explicitly declined — spend outside the run's own budget.
// v=9 (2026-08-26): added BotBundle (the stored-bot bundle ref a runner
// rebuilds from the DB instead of attaching the stale baked bundle) and
// SandboxImage (the platform-resolved default sandbox image, pinned at
// publish so redelivery reruns in the same environment). Dropping either
// silently serves STALE code/skills for an overridden bot — the exact façade
// the platform-override feature exists to prevent. Consumers accept BOTH v8
// and v9 (MinSchemaVersion): the change is purely additive, so a NEW runner
// consumes old queued v8 messages. (The reverse does not hold — a pre-bump
// runner rejects v9 — so the server-first ordering, or a same-release roll of
// both, remains the default; dual-accept removes only the stranded-v8-message
// half of the window. Rolling the runners first removes the other half, but
// only when nothing below the new Min can still be queued: a runner-first roll
// parks such a message UNREPLAYABLY, while a server-first one parks the new
// version replayably. See the runbook's Deploy ordering section.)
//
// v=10 (2026-08-29): added Fallback so the operator's single run-level
// fallback route (`--fallback` / launch `fallback`) reaches the runner.
// Same failure direction as v=7: the server accepted the field and the
// local executor honoured it, but the cloud path dropped it at publish —
// so the one route meant to rescue a run from a provider's exhausted
// usage window never fired precisely where runs park unattended.
// v=11 (2026-08-30): Fallback became an ordered chain. New consumers accept
// both the v10 object and the v11 array, while producers always emit the
// array. A v10 runner must reject the new payload rather than run with a
// partially decoded rescue policy.
// v=12 (2026-09-02): added RunnerEpoch, the generation fence that prevents a
// stale runner from admitting work published after a rollout cutover. New
// consumers continue to accept v10/v11 messages as epoch 0; older consumers
// must reject v12 because ignoring the field would fail the fence open.
//
// KNOWN DEBT: ModelOverrides shipped earlier inside v7 (427a9f44e) without a
// version bump. A v7 runner built before that commit can silently ignore the
// operator's model/backend pins. That historical gap cannot be repaired by a
// later bump; the additive-intent rule above prevents repeating it.
// v=13: BotBundle snapshots include sibling workflows and resources. An old
// runner must reject them instead of silently attaching its own catalog.
// v=14: ExecutionContext carries the resolved run/workspace/workflow/lineage
// contract to the claiming runner. Dropping it would make cloud admission
// disagree with local admission, so the wire version is bumped.
// v=15: ModelOverride.Effort and the run-level Permission override from the
// assistant branch. Stale v14 runners must reject explicit choices they
// cannot enforce. The branch-local v10/v11 additions are renumbered here.
// v=16 (2026-08-29): ResumeSpec.HostInputs carries host-attested derived
// context (currently bounded chat history) to the runner without recording it
// as operator answers. A stale runner dropping it would accept an assistant
// turn while silently removing the continuity fallback, so it must reject.
// v=17 (2026-09-11): mission resume receipts cross the queue and are stamped
// on run_resumed. A stale runner would execute the mutation but drop its
// reconciliation identity, so this additive field changes operator intent.
// v=18: loop cap expressions are evaluated at each crossing. A pre-v18
// runner cannot compile this AST semantics and must reject before admission.
// v=19: literal template delimiters require the new renderer. A pre-v19
// runner must reject before compiling or executing their source text.
// v=20: Contributions.Degraded flags a payload the publisher could not fully
// enumerate (broken plugin.yaml, unreadable home, degraded team source). A
// stale runner dropping it reads an amputated payload as the whole
// declaration and prunes a still-enabled plugin's launch-pass mirrors on the
// first resume, so it must reject.
// v=21: ambient_context (ADR-119). A launch-time override rides the new
// AmbientContext field, and the compiled IR may carry node and workflow
// policies. A stale runner ignores both and applies its own default — for
// claude_code the operator's whole personal setup — so a run launched or
// declared with `none` would receive everything: it must reject.
// v=22 (2026-10-04): RunFallbackEntry grows On and Policy for the
// adaptive-routing ladder (ADR-121). Breaking by the additive-field rule:
// a stale runner ignoring On would run a policy-computed stage on the
// chain DEFAULT trigger set ([usage_window, unavailable]) — dropping
// auth and transient_exhausted, the exact failures the launch-time
// selection exists to fall through — silently reverting operator intent.
// Server-first rollout; MinSchemaVersion unchanged.
// v=23: RunnerPool (#2029). A launch mapped to a sovereign runner pool
// rides the new field on the message and the envelope. A stale runner
// ignores it and serves the run outside the team's pool — the leak the
// pool exists to make impossible — so it must reject.
// v=24: BundleDEK (#2029, ADR-123). Run bundles seal under a per-run
// DEK that travels in the message; runner pods hold no platform key
// material. Runner-first rollout: a pre-v24 runner refuses a v24
// message at the schema check instead of failing later at credential
// injection with a bundle it cannot open.
// v=25 (2026-10-19): LLMProviders (ADR-129 §5) — the run's FROZEN provider
// allowlist, stamped from the launch's resolved routing policy and replayed
// from the run doc on resume. Breaking by the additive-field rule, same
// direction as v=23's RunnerPool: a stale runner that dropped the field
// would dispatch every route the tenant forbids — the leak the list exists
// to make impossible — so it must reject the message at the schema check.
// Server-first rollout; MinSchemaVersion unchanged.
// v=26 (2026-10-22): a tool node's `env_expansion:` (ADR-130) rides the
// compiled AST — IRCompiled, or the IRRef blob it is offloaded to when the
// marshalled AST would exceed max_payload. Breaking by the additive-field
// rule: a stale runner decodes the AST with unknown keys ignored, drops the
// declaration and expands the braced environment forms of a body the author
// declared verbatim — failing open, on a runner, and only there. The version
// travels on the MESSAGE either way, so the gate holds on both carriers.
// Same direction as v=21's ambient_context, the other IR-carried node
// policy. Server-first rollout; MinSchemaVersion unchanged, so a stale
// runner parks the message with a delayed Nak and a resume recovers it.
// v=27: BudgetOverrides.DonorMaxCostUSD carries the absolute donor ceiling
// separately from the declared cap. Dropping it loses the bound on exit grace
// and live raises. Both schema bounds move to 27: older donor payloads may
// omit even CapImposed when the ask fits below the allowance, so no safe
// migration can recover their ceiling. Drain or park/resume; server first.
// v=28: BotSourceTier (the #1959 isolation floor) — which bot-resolution
// tier served the launch, mirrored from the run document so the runner
// applies the operator isolation floor BEFORE any document read, exactly
// like Trust. Breaking by the additive-field rule: a stale runner dropping
// it would honour a team-authored `sandbox: none` the floor exists to
// neutralize — failing open, on the pod. MinSchemaVersion stays 27: every
// v27 publisher already stamps BotSourceTier on the run document, so a new
// runner consuming a v27 message re-reads the tier from the document (the
// floor's second carrier) instead of dropping the decision — the one
// direction in which ignoring the field cannot fail open.
// v=29 (2026-11-09): ForkAuthorSubject (ADR-162 U6, decision 5) — the fork
// review lane's per-contributor metering subject, so the runner's
// recordOrgSpend charges the author's dollars beside the org's. Breaking by
// the additive-field rule: a stale runner dropping it would leave the
// per-contributor DOLLAR bound unenforced for the run's lifetime — a
// perimeter failing open, in silence, on the pod. Server-first rollout;
// MinSchemaVersion stays 27, because a v27 or v28 message can only carry a
// fork run's trust in one case, and that case parks: no v28 server LAUNCHES
// untrusted work, but a v28 replica still serving mid-rollout can REPUBLISH
// a fork run a v29 replica created (its resume, a launch naming it as a
// parent), replaying the trust with no subject. Validate parks any untrusted
// message older than ForkAuthorSubjectSchemaVersion as a version mismatch,
// so an upgraded runner never runs it unmetered; a v28 runner, which knows
// neither the field nor the rule, still would — so the fork lane is enabled
// only once every server and runner replica runs v29
// (docs/cloud-queue-schema-rollout.md). A v29 message on a stale runner is
// rejected at the schema check and parks replayably.
// v=30 (2026-11-10): ContentBoundary gets its producer (ADR-131 §4, #2005
// PR-C'). The publisher stamps every launch its credential walk resolves —
// team_gateway when the walk attributes a gateway route to the team's key and
// seals it, own_keys otherwise — and replays the frozen stamp on resume; the
// run's bundle carries AmbientLLMRefused on the same decision. Breaking by
// the additive-field rule: a stale runner dropping the stamp would land
// vendor crossings on the run's gateway nodes, run its supervisors on the
// pod's environment and admit the server-side helpers the boundary refuses —
// serving auxiliary gateway spend off the team's boundary, failing open on
// the pod. It rejects: its window is [27, 29]. MinSchemaVersion stays 27 —
// the fence is the maximum that rises; raising the minimum would force a
// drain of v27–v29 messages for no added safety. A v29 publisher — and a v28
// one since PR-B'2 (#2721) added the carriage — never DERIVES a stamp, but
// its resume replays the run document's frozen one: a v29 replica still
// serving mid-rollout (or a server rolled back) resuming a run a v30 replica
// stamped team_gateway publishes a pre-v30 message carrying the stamp over a
// bundle walked by the old rules — no gateway key sealed, no
// AmbientLLMRefused — whose gateway routes would fall to the pod's
// environment. Validate parks a message older than
// ContentBoundaryProducerSchemaVersion carrying team_gateway as a version
// mismatch, so a resume from an upgraded server recovers it; every other
// v27–v29 message carries own_keys or no stamp, the readings the new runner
// applies. A v27 publisher, or a v28 one built before PR-B'2, replays
// nothing: its resume of a stamped run carries no stamp, which no rule can
// tell from an unstamped run — the runner reads the message alone — so a
// server rolled back below PR-B'2 drops a team_gateway run's boundary on
// resume. Runners and pools roll FIRST, the server after: with the minimum
// unchanged a new runner admits every trusted message the old server still
// publishes (the v29 rule above still parks an untrusted message older than
// v29), and an old runner never sees a v30 message.
const SchemaVersion = 30

// ContentBoundaryProducerSchemaVersion is the first wire version whose
// publisher seals a team_gateway run's gateway key and arms its
// AmbientLLMRefused on the stamp's decision: a message older than it that
// carries team_gateway is a stale replica's replay of a frozen stamp over an
// old-rules bundle, and Validate parks it.
const ContentBoundaryProducerSchemaVersion = 30

// ForkAuthorSubjectSchemaVersion is the first wire version that carries
// RunMessage.ForkAuthorSubject: an untrusted message older than it cannot
// name the author whose bound it spends, and Validate parks it.
const ForkAuthorSubjectSchemaVersion = 29

// MinSchemaVersion is the oldest wire version a consumer still accepts.
// Pre-v27 messages cannot prove an absolute donor ceiling. Resume republishes
// them with a fresh grant and its explicit amount; replaying old bytes cannot.
// v27, v28 and v29 stay acceptable beside v30 (see the v28, v29 and v30
// notes above SchemaVersion).
const MinSchemaVersion = 27

// RunMessage is the JSON envelope published on
// `iterion.queue.runs`. The runner deserialises it, takes the
// distributed lock, and runs the workflow described by IRCompiled
// (or fetches IRRef when the IR exceeds the NATS message size limit).
//
// Field order is stable to keep readable JSON diffs in tests.
type RunMessage struct {
	V            int    `json:"v"`
	RunnerEpoch  uint64 `json:"runner_epoch,omitempty"`
	RunID        string `json:"run_id"`
	WorkflowName string `json:"workflow_name"`
	WorkflowHash string `json:"workflow_hash"`
	// ExecutionContext is the launcher's resolved, versioned context
	// contract. The queued run document also carries it; the wire copy lets a
	// runner fail closed even when it has not yet loaded the document.
	ExecutionContext *store.ExecutionContext `json:"execution_context,omitempty"`
	IRCompiled       json.RawMessage         `json:"ir_compiled,omitempty"`
	IRRef            *IRRef                  `json:"ir_ref,omitempty"`
	RepoURL          string                  `json:"repo_url,omitempty"`
	RepoSHA          string                  `json:"repo_sha,omitempty"`
	// RepoSHAExpected is the commit the LAUNCH admitted, when RepoSHA is a
	// ref an untrusted party can move between the admission and this
	// message being consumed (a fork pull request's head). The runner
	// fetches RepoSHA and refuses the run when what arrived is not this
	// commit. It rides the wire — not only the run document — because the
	// comparison happens in the runner pod, before it has any reason to
	// load the document. Empty disables the comparison.
	RepoSHAExpected string `json:"repo_sha_expected,omitempty"`
	// Trust is the run document's RunTrust, mirrored onto the wire so the
	// runner can withhold workspace-level capabilities without a document
	// read. Empty is the trusted default.
	Trust store.RunTrust `json:"trust,omitempty"`
	// ForkAuthorSubject is the fork review lane's per-contributor metering
	// subject (orgusage.ForkAuthorSubject's exact spelling, composed on the
	// metering id the org budget used), mirrored from the run document so
	// the runner's recordOrgSpend charges the author's dollars beside the
	// org's — where spend is measured, not from a document a failed run may
	// never have loaded. Empty on every trusted run (and every run that
	// predates the fork lane).
	ForkAuthorSubject string `json:"fork_author_subject,omitempty"`
	// BotID is the stable bundle/bot identifier for this run. It qualifies
	// structured visibility=bot memory and is preserved on resume.
	BotID string `json:"bot_id,omitempty"`
	// BotSourceTier names which bot-resolution tier served the launch
	// (store.BotSourceTierTeam/Platform/Baked), mirrored from the run
	// document so the runner applies the #1959 operator isolation floor —
	// a non-operator source's `sandbox: none` is neutralized — before it
	// has any reason to read the document, and so a subbot's child message
	// (a struct copy of this one) inherits it. Empty is the operator's own
	// launch (local run, or a doc that predates the stamp): no floor.
	BotSourceTier string `json:"bot_source_tier,omitempty"`
	// Contributions carries the plugin markdown contributions and
	// skill-library skills the LAUNCHING instance resolved for this run (the
	// wire mirror of runtime.Contributions). A runner pod's iterion home is
	// ephemeral and empty, so without this an operator-installed plugin's
	// skill — or a DSL `skills:` library reference — silently never reaches
	// the workspace and only compiled-in builtins do. The publisher ships the
	// field on every dispatch, possibly EMPTY (a statement: "nothing enabled
	// on the launching instance"). Nil means the field did not arrive (an
	// anomaly — a lost field or a pre-payload publisher); the runner never
	// reads it as "nothing enabled" and the engine treats the ambient
	// declaration as unverifiable, so the orphan pruner is skipped for that
	// pass instead of deleting still-declared files.
	Contributions *Contributions `json:"contributions,omitempty"`
	Vars          map[string]any `json:"vars,omitempty"`
	// AllowUnknownInputs is the operator's explicit opt-out of the #1757
	// input check (CLI --allow-unknown-inputs, the dispatcher's warn-and-
	// proceed contract): inputs that name no declared var ride the launch
	// instead of refusing it. It rides the wire because the runner
	// re-applies the check before engine.Run — a queued run cannot skip it
	// by having been admitted on an older build. False = the check runs.
	AllowUnknownInputs bool   `json:"allow_unknown_inputs,omitempty"`
	SecretsRef         string `json:"secrets_ref,omitempty"`
	// BundleDEK is the per-run 32-byte key the run's sealed credential
	// bundle encrypts under (ADR-123). It travels only here — the run's
	// own pool stream — so runner pods hold no platform key material:
	// the per-run sealer is built from it at claim time. A message that
	// carries it without a SecretsRef (or a "dek"-stamped record whose
	// message carries none) is corrupt and refused.
	BundleDEK  []byte `json:"bundle_dek,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	// PoolGrantless marks a publication that asked the credential pool and
	// was granted nothing: the attempt runs on env/fallback credentials,
	// holds no pool lease, and its spend report must never reach the
	// broker — it would be charged to the lease of the attempt this
	// publication took the run from. Inverted (grantless, not served) so
	// publications older than the field read as pool-served, which is what
	// they were.
	PoolGrantless bool `json:"pool_grantless,omitempty"`
	// PoolAttemptID is the attempt identity the publication's credential
	// resolution stamped on every pool lease it opened
	// (credpool.Lease.AttemptID): the attempt's spend report settles
	// exactly those leases. Empty when the attempt holds no pool lease, and
	// on a publication older than the field — its report then pins its
	// lease by PublishedAtRFC.
	PoolAttemptID string `json:"pool_attempt_id,omitempty"`
	// Budget carries launch-time budget-cap overrides ("non-zero wins,
	// zero inherits" — the wire mirror of ir.BudgetOverrides). The runner
	// applies it after loading the workflow and BEFORE its multitenant
	// cloud ceiling, so a tenant can only lower the effective caps.
	Budget *BudgetOverrides `json:"budget,omitempty"`
	// ModelOverrides carries the launch-time per-node/-group model/backend/
	// provider pins so the claiming runner APPLIES them to its executor —
	// the wire mirror of store.RunModelOverride, same doctrine as Budget
	// above. Without this field the cloud path persisted the pins
	// display-only: the studio showed an override the delegates never
	// honoured.
	ModelOverrides []ModelOverride `json:"model_overrides,omitempty"`
	// Fallback carries the operator's ordered run-level fallback chain so
	// the claiming runner APPLIES it to its executor (ir.ApplyRunFallback,
	// same screen as a local launch) — the wire mirror of
	// store.RunFallback, same doctrine as ModelOverrides above. Without
	// this field the cloud path accepted the launch's `fallback` and
	// dropped it at publish: the one route meant to rescue a run from an
	// exhausted usage window never fired on the path where runs park
	// unattended.
	Fallback RunFallback `json:"fallback,omitempty"`
	// FallbackModelClasses is the run's RESOLVED model_classes overrides
	// (ADR-121 § Delivery 2, the launch-frozen snapshot) — the crossings'
	// class targets on this run. A message-level sibling of the entries
	// slice (per-entry would duplicate one map per rung): a stale runner
	// ignoring it runs the shipped table — never a partial override, the
	// conservative-on-ignore direction (no RunMessage version bump: a
	// lost capability, never a wrong execution).
	FallbackModelClasses map[string]map[string]string `json:"fallback_model_classes,omitempty"`
	// LLMProviders is the run's FROZEN provider allowlist (ADR-129 §5) —
	// the launch's resolved routing policy's `providers` list, replayed
	// from the run doc on every resume, never re-resolved. The claiming
	// runner drops every dispatch chain element whose resolved provider is
	// outside it, so a rescue route the tenant forbids burns no retry
	// budget on a call it may not spend. Nil/empty = no allowlist (a run
	// launched before the field, or a tenant that set none).
	LLMProviders []string `json:"llm_providers,omitempty"`
	// AutoMemory is the launch-time auto-memory (MEMORY.md) override — the
	// wire half of the knob's strongest precedence level. Empty means the
	// caller expressed nothing and the workflow/env decide.
	AutoMemory string `json:"auto_memory,omitempty"`
	// AmbientContext is the launch-time ambient-context override (ADR-119).
	// Empty means the caller expressed nothing and the workflow/env decide.
	AmbientContext string `json:"ambient_context,omitempty"`
	// LoopBudgetGuard is the launch-time back-edge affordability override —
	// the wire half of that knob's strongest precedence level. Empty means
	// the caller expressed nothing and the workflow/env decide.
	LoopBudgetGuard string `json:"loop_budget_guard,omitempty"`
	// Supervisors is the launch-time kill switch for DSL-declared
	// supervisor watchers — the wire half of that knob's strongest
	// precedence level. Empty means the caller expressed nothing and the
	// pod's ITERION_SUPERVISORS (then the default on) decides.
	Supervisors string `json:"supervisors,omitempty"`
	// Permission is the operator's run-level tool-permission-gate override.
	// It must remain distinct from the workflow permission because it sits
	// ABOVE node declarations in the precedence chain.
	Permission string `json:"permission,omitempty"`
	// BotBundle carries the server-resolved immutable collection in v13.
	// Legacy refs without a snapshot still resolve a stored row with a version
	// check. Nil retains the legacy baked/loose-bot resource lookup.
	BotBundle *BotBundleRef `json:"bot_bundle,omitempty"`
	// SandboxImage is the effective `sandbox: auto` fallback image resolved
	// by the PUBLISHER (platform runtime setting over the env default) and
	// pinned here so a redelivery or checkpoint re-claim reruns in the same
	// environment. Empty = the runner's own env/default resolution.
	SandboxImage   string        `json:"sandbox_image,omitempty"`
	BackendConfig  BackendConfig `json:"backend"`
	Resume         *ResumeSpec   `json:"resume,omitempty"`
	Trace          TraceContext  `json:"trace"`
	PublishedAtRFC string        `json:"published_at"`
	// TenantID is the team_id the run belongs to. Required in v=2.
	// Runners verify the loaded run document's tenant_id matches
	// before claiming the lock; a mismatch is treated as a corrupted
	// queue entry and the run is naked.
	TenantID string `json:"tenant_id"`
	// OrgID is the parent-org id the run's monthly LLM spend is
	// charged to — the same usage key the launch gate metered the
	// launch on (gateMonthlyCaps keys the counter by org so caps sum
	// across the org's teams). Optional: empty (message published
	// before this field existed, or a pre-backfill team with no org)
	// makes the runner fall back to TenantID, matching the gate's own
	// fallback for org-less teams.
	OrgID string `json:"org_id,omitempty"`
	// OwnerID is the user_id of the principal who initiated the run.
	// Used for audit logging; runners do NOT gate execution on it.
	OwnerID string `json:"owner_id,omitempty"`
	// RunnerPool names the sovereign runner pool the run MUST execute on
	// (#2029). Empty means the shared default pool. The publisher stamps it
	// from the team's CURRENT mapping (fresh read, refusal on a store
	// error); a runner serves a message only when this matches its own
	// pool and the persisted document — a disagreement is a corrupted or
	// replayed publish, parked with the run failed, never executed. The
	// field requires v22 on the wire: a stale runner that dropped it would
	// execute the run outside the team's pool, which is the leak the pool
	// exists to make impossible.
	RunnerPool string `json:"runner_pool,omitempty"`
	// ContentBoundary mirrors the run document's frozen content boundary
	// (ADR-131 §4) so the claiming runner knows which credential class
	// funds the run without a second store read: the publisher stamps it at
	// launch (ContentBoundaryProducerSchemaVersion) and replays the
	// document's stamp on resume (since PR-B'2, at v28, before any producer
	// existed — Validate parks a pre-producer replay of team_gateway). Nil on a
	// message whose run predates the producer, or whose resolution never
	// walked; a nil pointer serialises as absent.
	ContentBoundary *store.RunContentBoundary `json:"content_boundary,omitempty"`
	// CredPlan is the launch-frozen node-and-provider → credential-
	// fingerprint plan the PUBLISHER resolves (the walk that sealed the
	// bundle, joined with the bundle's own fingerprint stamps) and stamps
	// so the claiming runner can attribute each delegation's spend to the
	// credential that served it — the producer half of the pool's fine
	// door-lease booking (#2288, ADR-121 § Delivery 2). Keys are
	// `<node>|<provider>` (the provider the serving chain element resolved,
	// which is what the delegate events carry); values are the same
	// fingerprint strings the runner reads from its own credentials, so
	// the fine table it reports keys in the lease's space. The publisher
	// stamps it only when the routing walk is narrow-safe and a pool lease
	// serves the run — a walk it cannot read may spend anything, so no
	// plan it could write is total. The runner holds the same line at run
	// time: an observation the attempt paid for that joins none of the
	// keys voids its table. Nil (an older publisher, or a walk that
	// widened) keeps the runner's coarse whole-attempt booking: the
	// conservative-on-ignore direction, which is why the field needs no
	// RunMessage version bump (the same doctrine as FallbackModelClasses
	// above).
	CredPlan map[string]string `json:"cred_plan,omitempty"`
	// ParentRunID is set on child runs spawned by a parent workflow
	// (e.g. by `iterion __scan-shards`). Empty for root runs. When
	// non-empty, the runner copies it into the persisted Run document
	// so the studio and inspect surfaces can render the parent/child
	// tree. See docs/security-bots-distributed.md.
	ParentRunID string `json:"parent_run_id,omitempty"`
	// ShardIndex is the 0-based index of this run within the parent's
	// shard set. Only meaningful when ParentRunID is set.
	ShardIndex int `json:"shard_index,omitempty"`
	// ShardCount is the total number of shards the parent split its
	// work into. Only meaningful when ParentRunID is set.
	ShardCount int `json:"shard_count,omitempty"`
	// ShardLabel is an optional human-friendly tag for the shard
	// (e.g. "files 100-119" or "ecosystem:npm"). Display-only.
	ShardLabel string `json:"shard_label,omitempty"`
	// CallbackURL, CallbackToken, CallbackAnswerNode carry the
	// run-completion webhook parameters (see pkg/notify) across the
	// queue so the runner pod that executes the run knows where to POST
	// the terminal-state callback and what correlation token to echo.
	// Empty for runs launched without a callback (the common case).
	CallbackURL        string `json:"callback_url,omitempty"`
	CallbackToken      string `json:"callback_token,omitempty"`
	CallbackAnswerNode string `json:"callback_answer_node,omitempty"`
}

// BotBundleRef is the wire mirror of runview.BotBundleRef (kept local so
// this schema package stays dependency-free — the BudgetOverrides pattern):
// origin tuple plus an immutable collection inline or through a blob reference.
type BotBundleRef struct {
	TenantID       string          `json:"tenant_id"`
	Slug           string          `json:"slug"`
	Version        int             `json:"version"`
	Snapshot       json.RawMessage `json:"snapshot,omitempty"`
	SnapshotDigest string          `json:"snapshot_digest,omitempty"`
	SnapshotRef    *IRRef          `json:"snapshot_ref,omitempty"`
}

// Contributions is the wire mirror of runtime.Contributions: the plugin
// markdown files and skill-library skills the launching instance resolved from
// ITS iterion home, shipped so the runner pod can mirror the same set into the
// workspace. Sizes are small (markdown, a few KB each) and capped at publish
// time — see cloudpublisher.
type Contributions struct {
	Plugin  []ContributionFile `json:"plugin,omitempty"`
	Library []LibrarySkillFile `json:"library,omitempty"`
	// Degraded flags a payload the publisher could NOT fully enumerate: a
	// plugin whose plugin.yaml is broken (skipped by the registry, invisible
	// to Enabled()), an unreadable iterion home, or a team-scoped source that
	// failed to materialise. The runner pod cannot distinguish an amputated
	// payload from a complete one by inspecting it — the missing entries are
	// missing — so the publisher says so on the wire. The mirror pass reads
	// it as "the declaration is partial": nothing beyond the carried files is
	// verified, the pass reports incomplete, and the orphan pruner is
	// skipped so a still-enabled plugin's launch-pass mirrors survive
	// (the cloud twin of the local path's Registry.LoadSkips veto).
	Degraded bool `json:"degraded,omitempty"`
}

// ContributionFile is one plugin markdown file bound for
// <workspace>/.claude/<kind>/<name>.
type ContributionFile struct {
	// Kind is the .claude/ leaf dir: "skills" | "commands" | "agents".
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Content []byte `json:"content"`
}

// LibrarySkillFile is one resolved skill-library skill. Description carries the
// frontmatter description so the runner reproduces the "## Skills" prompt hint
// without the store it does not have.
type LibrarySkillFile struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Content     []byte `json:"content"`
}

// BudgetOverrides is the wire mirror of ir.BudgetOverrides (kept local so
// this schema package stays dependency-free). Each field uses the
// "non-zero wins, zero inherits" convention; an all-zero value must be
// published as a nil pointer, not an empty object.
type BudgetOverrides struct {
	MaxCostUSD          float64 `json:"max_cost_usd,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	MaxDuration         string  `json:"max_duration,omitempty"`
	MaxIterations       int     `json:"max_iterations,omitempty"`
	MaxParallelBranches int     `json:"max_parallel_branches,omitempty"`
	UnlimitedWorkflow   bool    `json:"unlimited_workflow,omitempty"`
	CapImposed          bool    `json:"cap_imposed,omitempty"`
	DonorMaxCostUSD     float64 `json:"donor_max_cost_usd,omitempty"`
}

// ModelOverride is one launch-time retargeting rule: every LLM node matching
// Selector (a node id, an id glob like "reviewer_*", or a node kind keyword
// such as "agent"/"judge") runs on the named backend/model/provider/effort
// instead of its DSL value. The wire mirror of store.RunModelOverride, kept
// local so this schema package stays dependency-free.
//
// Empty fields inherit: a rule carrying only Model retargets the model and
// leaves the node's backend alone.
type ModelOverride struct {
	Selector string `json:"selector"`
	Backend  string `json:"backend,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// RunFallbackEntry is one stage of the operator's run-level fallback
// chain on the wire — the queue twin of runview.FallbackEntry. The On and
// Policy fields (v=22) carry the ADAPTIVE-ROUTING ladder's stages
// (ADR-121): On is the resolved policy's trigger set the stage accepts
// (nil = the chain default — what operator stages carry), Policy marks
// the stage as policy-computed (the usagecap preflight judges its spend;
// operator rescue stages stay excluded).
type RunFallbackEntry struct {
	Backend  string `json:"backend,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// On is never omitempty: a CLAMPED-EMPTY set is the "never switch"
	// answer, and omitempty would erase it into the chain default on the
	// way back (the M4 round-trip).
	On     []string `json:"on"`
	Policy bool     `json:"policy,omitempty"`
	// CrossHarness is the run's resolved cross-harness posture (ADR-121
	// § Delivery 2) riding this policy stage: an active mode lifts the
	// stage's session refusal at the authoritative screen and marks the
	// dispatch's model_fallback. omitempty + additive: a stale runner
	// ignoring it re-refuses at the screen — a lost capability, never a
	// wrong execution (no schema bump).
	CrossHarness string `json:"cross_harness,omitempty"`
}

// RunFallback is the ordered wire chain. Producers marshal it as an array;
// UnmarshalJSON also accepts the v10 single-object form and promotes it to a
// one-stage chain. Eligibility is decided by ir.ApplyRunFallback after decode.
type RunFallback []RunFallbackEntry

func (f *RunFallback) UnmarshalJSON(data []byte) error {
	raw := bytes.TrimSpace(data)
	if len(raw) == 0 {
		return fmt.Errorf("queue: empty fallback JSON")
	}
	switch raw[0] {
	case 'n':
		if !bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("queue: invalid fallback JSON %q", raw)
		}
		*f = nil
		return nil
	case '[':
		var entries []RunFallbackEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("queue: decode fallback chain: %w", err)
		}
		*f = entries
		return nil
	case '{':
		var entry RunFallbackEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("queue: decode legacy fallback: %w", err)
		}
		*f = []RunFallbackEntry{entry}
		return nil
	default:
		return fmt.Errorf("queue: fallback must be an object or array")
	}
}

// IRBackend is the storage backend an IRRef points at.
type IRBackend string

const (
	IRBackendS3    IRBackend = "s3"
	IRBackendMongo IRBackend = "mongo"
)

// IRRef points at an out-of-band IR blob. Used when ast.MarshalFile
// output exceeds the NATS message size budget (~1 MB).
type IRRef struct {
	StorageKey string    `json:"storage_key"`
	Backend    IRBackend `json:"backend"`
}

// Backend is the LLM execution backend a runner picks for the run.
// "claw" is in-process; every other value forks an external agent CLI.
//
// The set must stay in sync with delegate's registration names: a backend
// missing here cannot be expressed as a queued run's default, so a cloud
// run silently falls back to claw. That is how kimi and grok were
// unreachable in cloud mode until pi's arrival surfaced the gap.
type Backend string

const (
	BackendClaw       Backend = "claw"
	BackendClaudeCode Backend = "claude_code"
	BackendCodex      Backend = "codex"
	BackendKimi       Backend = "kimi"
	BackendGrok       Backend = "grok"
	BackendPi         Backend = "pi"
	BackendOpenCode   Backend = "opencode"
)

// BackendConfig carries the LLM backend selection per run.
type BackendConfig struct {
	Default       Backend `json:"default"`
	DelegateModel string  `json:"delegate_model,omitempty"`
}

// ResumeSpec is non-nil for resume publishes; the runner threads its
// fields into `runtime.Engine.Resume`.
type ResumeSpec struct {
	Answers        map[string]any  `json:"answers,omitempty"`
	HostInputs     map[string]any  `json:"host_inputs,omitempty"`
	Force          bool            `json:"force"`
	ExpectedStatus store.RunStatus `json:"expected_status,omitempty"`
	ReceiptID      string          `json:"receipt_id,omitempty"`
	// PriorStatus is the status the publisher's claim moved the run from
	// to queued. When the engine refuses the resume before claiming it —
	// nothing ran, and the refusal is acked, never redelivered — the runner
	// puts the run back there. Empty from an older publisher: the run goes
	// back to failed_resumable. A runner that predates it leaves the run
	// queued, as it always did, so the field changes no operator intent.
	PriorStatus store.RunStatus `json:"prior_status,omitempty"`
	// AcceptScratchLoss is the operator's consent to resume although the
	// run's scratch does not travel — Force never gives it. It is spent by
	// the claim of this publication: a runner applies it only while the run
	// is still queued for this message, never to a redelivery after that
	// claim or an adoption, whose losses nobody was shown. Absent from an
	// older publisher: no consent, the safe side.
	AcceptScratchLoss bool `json:"accept_scratch_loss,omitempty"`
}

// TraceContext propagates the originating studio span across NATS so
// runner-side spans inherit the parent. Encoded redundantly in the
// `traceparent` NATS header for fast extraction without decoding the
// body.
type TraceContext struct {
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
}

// ErrSchemaVersion marks the one decode failure that is TRANSIENT rather than
// terminal: the message was published by a newer server than this consumer.
//
// It matters because the two failures deserve opposite handling. A malformed
// payload will never decode, on any consumer, so terminating it is right. A
// version this build does not know is the ordinary state of a rolling upgrade
// — the server is deployed first, deliberately — and a consumer that
// terminates it destroys a run no one can recover: the queue entry is gone
// while the run document sits `queued` forever, with the refusal visible only
// in one pod's logs. Observed in production during exactly such a rollout.
var ErrSchemaVersion = errors.New("queue: schema version")

// StaleProducerError is the version mismatch of a message whose version this
// consumer accepts but whose publisher predates a decision the message needs
// (ForkAuthorSubjectSchemaVersion, ContentBoundaryProducerSchemaVersion): a
// server replica older than Want republished a run a newer one created. The
// runner fleet is not what lags — only a resume from a server replica that
// publishes Want or later recovers the run. ErrSchemaVersion is in its chain,
// so it parks like every version mismatch.
type StaleProducerError struct {
	V    int // the message's wire version
	Want int // the first wire version whose publisher makes every decision the message needs
	msg  string
}

func (e *StaleProducerError) Error() string { return e.msg }

func (e *StaleProducerError) Unwrap() error { return ErrSchemaVersion }

// staleRule is one stale-producer rule that fired: the version whose
// publisher makes the decision, and the sentence naming it.
type staleRule struct {
	want int
	text string
}

// staleProducer is the mismatch of every rule that fired: Want is the
// highest of their versions — a replica publishing a lower one is still
// stale for another rule — and the sentence names each decision the message
// predates, in rule order.
func staleProducer(v int, rules []staleRule) error {
	want := 0
	texts := make([]string, 0, len(rules))
	for _, r := range rules {
		want = max(want, r.want)
		texts = append(texts, r.text)
	}
	return &StaleProducerError{V: v, Want: want, msg: ErrSchemaVersion.Error() + ": " + strings.Join(texts, "; and ")}
}

// Validate enforces the invariants a runner must rely on before
// touching the workflow:
//   - schema version matches (rolling-upgrade safety)
//   - mandatory identifiers present
//   - exactly one of IRCompiled / IRRef is set (J-IR-too-large fallback)
func (m *RunMessage) Validate() error {
	if m == nil {
		return fmt.Errorf("queue: nil RunMessage")
	}
	if m.V < MinSchemaVersion || m.V > SchemaVersion {
		return fmt.Errorf("%w: %d unsupported (want %d–%d)", ErrSchemaVersion, m.V, MinSchemaVersion, SchemaVersion)
	}
	if m.RunID == "" {
		return fmt.Errorf("queue: RunID required")
	}
	// The stale-producer rules, every one judged: a message breaking two
	// is parked under both, and its remedy names the later producer.
	var stale []staleRule
	// A message older than the fork author subject cannot carry it, so an
	// UNTRUSTED one would run with the author's per-contributor dollar bound
	// unenforced: a server replica still on v28 mid-rollout republishing the
	// resume of a fork run an upgraded replica created. Parked as a version
	// mismatch — transient, never Termed — so a resume from an upgraded
	// server, whose gate meters the author, recovers it.
	if m.V < ForkAuthorSubjectSchemaVersion && !m.Trust.Trusted() {
		stale = append(stale, staleRule{ForkAuthorSubjectSchemaVersion, fmt.Sprintf("a v%d message carries trust %q but predates the fork author subject (v%d) — parked for a resume from an upgraded server rather than run with the author's bound unenforced", m.V, string(m.Trust), ForkAuthorSubjectSchemaVersion)})
	}
	// A message older than the stamp's producer replays a team_gateway stamp
	// it did not derive (a stale replica resuming a run an upgraded one
	// stamped) over a bundle sealed by the old rules: no gateway key, no
	// ambient refusal, so the run's gateway routes would spend the pod's
	// environment. Parked as a version mismatch, like the rule above.
	if m.V < ContentBoundaryProducerSchemaVersion && m.ContentBoundary.TeamGateway() {
		stale = append(stale, staleRule{ContentBoundaryProducerSchemaVersion, fmt.Sprintf("a v%d message carries the team_gateway content boundary but predates its producer (v%d) — parked for a resume from an upgraded server rather than run with the run's gateway unsealed and the pod's environment admitted", m.V, ContentBoundaryProducerSchemaVersion)})
	}
	if len(stale) > 0 {
		return staleProducer(m.V, stale)
	}
	// The pool name is grammar-checked, not tenant-checked, and only when
	// set: an empty pool is the shared default (pre-v22 messages included),
	// and WHO may map a team to a pool is the publisher resolver's
	// fail-closed read, not a wire-format question.
	if m.RunnerPool != "" && !ValidPoolName(m.RunnerPool) {
		return fmt.Errorf("queue: RunnerPool %q invalid (want 1–31 chars [a-z0-9-], starting alphanumeric)", m.RunnerPool)
	}
	if m.BundleDEK != nil && m.SecretsRef == "" {
		return fmt.Errorf("queue: BundleDEK without SecretsRef — corrupt publish")
	}
	if len(m.BundleDEK) != 0 && len(m.BundleDEK) != 32 {
		return fmt.Errorf("queue: BundleDEK must be 32 bytes, got %d", len(m.BundleDEK))
	}
	if m.WorkflowName == "" {
		return fmt.Errorf("queue: WorkflowName required")
	}
	hasIR := len(m.IRCompiled) > 0
	hasRef := m.IRRef != nil && m.IRRef.StorageKey != ""
	if hasIR == hasRef {
		// Both set OR both unset is an error: the runner must know
		// where the IR comes from.
		return fmt.Errorf("queue: exactly one of IRCompiled / IRRef must be set (got ircompiled=%t ref=%t)", hasIR, hasRef)
	}
	if hasRef {
		switch m.IRRef.Backend {
		case IRBackendS3, IRBackendMongo:
		default:
			return fmt.Errorf("queue: IRRef.Backend %q invalid (want s3|mongo)", m.IRRef.Backend)
		}
	}
	if b := m.BotBundle; b != nil && (b.SnapshotDigest != "" || len(b.Snapshot) > 0 || b.SnapshotRef != nil) {
		if m.V < 13 {
			return fmt.Errorf("%w: bundle snapshots require v13", ErrSchemaVersion)
		}
		if len(b.SnapshotDigest) != 64 || strings.Trim(b.SnapshotDigest, "0123456789abcdef") != "" {
			return fmt.Errorf("queue: bundle snapshot digest must be lowercase SHA-256")
		}
		if (len(b.Snapshot) > 0) == (b.SnapshotRef != nil) {
			return fmt.Errorf("queue: exactly one bundle snapshot payload/ref is required")
		}
		if b.SnapshotRef != nil {
			if b.SnapshotRef.StorageKey == "" || (b.SnapshotRef.Backend != IRBackendS3 && b.SnapshotRef.Backend != IRBackendMongo) {
				return fmt.Errorf("queue: invalid bundle snapshot reference")
			}
		}
	}
	return nil
}

// Envelope carries the STABLE identity fields of a RunMessage, decodable
// without validating the schema version. These fields are part of the
// wire contract's immutable core: their JSON names must never be renamed or
// repurposed by any schema bump, because they are the only way a consumer
// that rejects the version can still identify the run and queue attempt — to
// park the payload on the DLQ and flip only that attempt to an actionable
// status instead of leaving it `queued` in silence (issue #481).
type Envelope struct {
	V           int    `json:"v"`
	RunnerEpoch uint64 `json:"runner_epoch,omitempty"`
	RunID       string `json:"run_id"`
	TenantID    string `json:"tenant_id"`
	OwnerID     string `json:"owner_id,omitempty"`
	// RunnerPool mirrors RunMessage.RunnerPool in the stable identity core:
	// a consumer that rejects the wire version can still see which pool the
	// run claims, park it on the right DLQ, and flip the right attempt.
	RunnerPool     string `json:"runner_pool,omitempty"`
	PublishedAtRFC string `json:"published_at"`
}

// ValidPoolName reports whether s is a well-formed runner-pool name:
// 1–31 chars, lowercase alphanumerics and inner dashes, starting with an
// alphanumeric. It doubles as the subject/stream suffix grammar, so a name
// that fails this can never address a pool topic.
func ValidPoolName(s string) bool {
	if len(s) == 0 || len(s) > 31 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// PeekEnvelope extracts the identity envelope from a raw wire payload WITHOUT
// validating it. Unlike Delivery.Decode it succeeds on any schema version —
// that is precisely its use case: a version this build rejects must still be
// recoverable. A payload so malformed that even the envelope won't decode is
// reported as an error (callers fall back to Term-and-log).
func PeekEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("queue: envelope decode: %w", err)
	}
	if env.RunID == "" {
		return Envelope{}, fmt.Errorf("queue: envelope decode: run_id missing")
	}
	return env, nil
}
