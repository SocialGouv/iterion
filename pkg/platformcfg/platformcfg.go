// Package platformcfg holds platform-scoped runtime-settings families
// beyond the usage caps that established the doctrine (ADR-090): env var =
// deployment default, DB record = runtime override effective without a
// restart, super-admin API/CLI as the write surface. Each family is one
// document in the shared `platform_settings` Mongo collection, keyed by a
// fixed _id (the layout usagecap's settings store reserved for exactly
// this growth).
//
// Families shipped here:
//   - bot_roles — the webhook role→bot-id bindings that were hardcoded
//     engine constants (reviewer/revi_converse/brancher/implementer), so
//     re-pointing a role at another bot no longer needs a rollout.
//   - sandbox — the `sandbox: auto` fallback image, resolved at PUBLISH
//     time and pinned on the RunMessage so a redelivery reruns in the same
//     environment.
//   - bot_vars — DB-resolved overrides for the ${ITERION_X:-default}
//     expansions bots declare (model pins, reasoning effort, tunables),
//     consulted before the pod env through ir.SetEnvOverlay.
//
// A nil field means "no override — inherit the code/env default", which is
// what keeps a deployment that never touches the API at exactly its
// baked-in behaviour.
package platformcfg

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// BotRoles is the role→bot-id override record. Each field, when non-nil,
// replaces the corresponding hardcoded default at every consuming site
// (webhook auto-review fan-out, /revi approve, the merge-queue auto-heal,
// the issue-labeled implementer lane).
type BotRoles struct {
	// Reviewer handles PR/MR review (default "review-pr").
	Reviewer *string `bson:"reviewer,omitempty" json:"reviewer"`
	// ReviConverse answers conversational /revi questions (default
	// "revi-converse").
	ReviConverse *string `bson:"revi_converse,omitempty" json:"revi_converse"`
	// Brancher improves an existing branch (default "branch-improve-loop").
	Brancher *string `bson:"brancher,omitempty" json:"brancher"`
	// Implementer handles issue-labeled feature work (default "feature-dev").
	Implementer *string `bson:"implementer,omitempty" json:"implementer"`

	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// Validate rejects override values that could never resolve to a bot: role
// ids follow the bot-slug rule (lowercase alphanumerics, '-', '_').
func (r BotRoles) Validate() error {
	for name, v := range map[string]*string{
		"reviewer": r.Reviewer, "revi_converse": r.ReviConverse,
		"brancher": r.Brancher, "implementer": r.Implementer,
	} {
		if v == nil {
			continue
		}
		if *v == "" {
			return fmt.Errorf("platformcfg: %s: bot id must not be empty (clear the override instead)", name)
		}
		// The one slug grammar bots are actually stored under — a role value
		// that could not name a stored/baked bot must fail at write time.
		if err := botsource.ValidSlug(*v); err != nil {
			return fmt.Errorf("platformcfg: %s: %w", name, err)
		}
	}
	return nil
}

// Sandbox is the sandbox runtime-settings record.
type Sandbox struct {
	// DefaultImage overrides ITERION_SANDBOX_DEFAULT_IMAGE / the built-in
	// version-pinned image as the `sandbox: auto` fallback. Cloud guidance:
	// use an @sha256 digest ref so the pinned message stays reproducible
	// against a mutable tag push too.
	DefaultImage *string `bson:"default_image,omitempty" json:"default_image"`

	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// EffectiveImage resolves the override to its effective value: "" (inherit
// the env default / built-in pin) when unset, the trimmed ref otherwise.
// The ONE definition every consumer (server echo, publisher pin) shares.
func (s *Sandbox) EffectiveImage() string {
	if s == nil || s.DefaultImage == nil {
		return ""
	}
	return strings.TrimSpace(*s.DefaultImage)
}

// Validate rejects a blank override (clearing is expressed by nil, never by
// an empty string that would pin "no image" onto every RunMessage).
func (s Sandbox) Validate() error {
	if s.DefaultImage != nil && strings.TrimSpace(*s.DefaultImage) == "" {
		return fmt.Errorf("platformcfg: default_image override must not be blank (clear it to fall back to the env default)")
	}
	return nil
}

// BotVars is the bot-variable override record: the `${ITERION_X:-default}`
// expansions every .bot declares (model pins, reasoning effort, tunables)
// resolved from the DB before the pod's env — so re-tuning a bot no longer
// needs a Helm values change and a rollout. The record stays typed and
// validated here per the ADR-090 rejection of a generic KV table; only the
// VALUE SPACE is a map, because the keys are declared by bots, not by the
// engine.
type BotVars struct {
	// Vars maps ITERION_* names to their override values. A key present
	// here beats the pod's env var; an absent key inherits env then the
	// .bot's `:-` default. Clearing = removing the key (never an empty
	// value).
	Vars map[string]string `bson:"vars,omitempty" json:"vars"`

	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// botVarsInfraPrefixes are the env namespaces a DB record must never
// shadow. Honest scope: most of these are read via plain os.Getenv at
// boot and never through the overlay, so this list is namespace HYGIENE
// and defence in depth (the read side additionally gates on ITERION_),
// not the containment boundary — that is the overlay's reach itself.
// It still matters for the sites that DO resolve through
// ir.LookupEnv/ExpandEnvWithDefault today or gain it tomorrow: keeping
// transport/auth/enforcement names unwritable means routing one more
// os.Getenv site through the choke point can never turn a stored var
// into an infra override. Grepped from the real config surface
// (pkg/config, cmd), not written from memory. The usage-cap and
// sandbox-image families are excluded because they already have their
// own audited, validated settings surface.
var botVarsInfraPrefixes = []string{
	"ITERION_MONGO_", "ITERION_NATS_", "ITERION_QUEUE_", "ITERION_REDIS_",
	"ITERION_JWT_", "ITERION_SECRETS_", "ITERION_S3_",
	"ITERION_OIDC_", "ITERION_OAUTH_", "ITERION_AUTH_",
	"ITERION_ACCESS_", "ITERION_PAT_", "ITERION_COOKIE_",
	"ITERION_SMTP_", "ITERION_OTLP_", "ITERION_SANDBOX_",
	"ITERION_USAGE_CAP", "ITERION_BOOTSTRAP_",
	"ITERION_MODEL_SPECS_", "ITERION_UPDATE_", "ITERION_DISPATCHER_",
	// The platform credential policy (keys_first, facade_default): which
	// credential serves a route is the operator's, never a bot's.
	"ITERION_PLATFORM_",
	// Spend and quota ceilings: the cloud budget and retry ceilings, the
	// tenant quota defaults, the knowledge-memory quotas.
	"ITERION_CLOUD_", "ITERION_ORG_DEFAULT_", "ITERION_MEMORY_",
	// Bounds on what untrusted input may cost: .botz extraction, the studio
	// assistant's writes and its interpreter.
	"ITERION_BUNDLE_", "ITERION_ASSISTANT_",
	// The workspace safety net: tracking, checkpoints, captured file size.
	"ITERION_WORKSPACE_",
	// Marketplace visibility scopes (tenant isolation) and seed paths.
	"ITERION_MARKETPLACE_",
	// Plugin configuration has its own audited surface, and a URL key
	// carries the plugin's credential to whatever host it names.
	"ITERION_PLUGIN_",
	// Processes and fleet rather than runs: the runner, the board
	// dispatcher and its MCP capabilities, the studio's alerts, the instance
	// registry, lifecycle and logs.
	"ITERION_RUNNER_", "ITERION_BOARD_", "ITERION_ALERTS_", "ITERION_INSTANCES_",
	"ITERION_SHUTDOWN_", "ITERION_LOG_",
	// The merge-gate sweep's cadence: the server's net under the lossy
	// outcome event, and what it spends of the forge's request budget.
	"ITERION_GATE_SWEEP_",
	// Endpoints and identities presented outside: the metrics listener,
	// the CLI's remote instance, the forge app, web push, scan shards.
	"ITERION_PROMETHEUS_", "ITERION_REMOTE_", "ITERION_FORGE_GITHUB_APP_",
	"ITERION_WEBPUSH_", "ITERION_SHARD_",
	// Written by the engine for a child: a run's identity and stores, the
	// ticket it serves — and the bounds on a run's interactive shell.
	"ITERION_RUN_", "ITERION_ISSUE_",
}

// botVarsInfraExact are single infra names outside those namespaces —
// exact matches, so a legitimate sibling (ITERION_BIN_PACKING…) is not
// collaterally blocked the way a bare prefix would.
//
// The rule both lists serve: a stored bot var tunes how a bot's runs
// behave. It never sets a name that lifts or weakens a guard (security,
// spend and quota ceilings, bounds on untrusted input, workspace safety),
// configures the process or the fleet rather than a run, names an endpoint
// or an identity presented outside, or is written by the engine for a
// child process — a stored value would forge it. The infra tests hold every
// ITERION_ name the engine spells (its Go source and the vendored claw's, the
// manifests and scripts it embeds under pkg) to that rule, and every knob a
// shipped bot reads to the other side of it.
var botVarsInfraExact = map[string]bool{
	"ITERION_PUBLIC_URL":   true,
	"ITERION_DISABLE_AUTH": true,
	"ITERION_SIGNUP_MODE":  true,
	"ITERION_BIN":          true,
	"ITERION_PI_BIN":       true,
	"ITERION_OPENCODE_BIN": true,
	// The two project-trust switches are ENFORCEMENT, not paths: each one
	// lifts a refusal that stops an agent CLI executing code out of the
	// repository under review.
	"ITERION_PI_TRUST_PROJECT":       true,
	"ITERION_OPENCODE_TRUST_PROJECT": true,
	// Every other switch that lifts or weakens a guard — enumerated from
	// what each one does, not from how its name reads. A stored value must
	// never turn one of them on, or off.
	// Origins, headers, cookies, redirects: the browser-facing guards.
	"ITERION_ALLOWED_ORIGINS":             true,
	"ITERION_REQUIRE_ORIGIN":              true,
	"ITERION_REQUIRE_WS_ORIGIN":           true,
	"ITERION_SECURITY_HEADERS":            true,
	"ITERION_LEGACY_REFRESH_COOKIE":       true,
	"ITERION_CANONICAL_REDIRECT":          true,
	"ITERION_STUDIO_INSECURE_NONLOOPBACK": true,
	// Private-network reach (SSRF) and host allowlists.
	"ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE": true,
	"ITERION_CONNECTOR_ALLOW_PRIVATE":          true,
	"ITERION_WEBHOOK_FORGE_HOSTS":              true,
	// Code loaded from the repository under review or the host: MCP servers,
	// plugins, CLI settings sources, context files, slash commands.
	"ITERION_MCP_AUTOLOAD":                             true,
	"ITERION_MCP_HEALTHCHECK":                          true,
	"ITERION_SKIP_MCP_HEALTH":                          true,
	"ITERION_PI_MCP_SERVERS":                           true,
	"ITERION_MCP_EXPAND_UNTRUSTED_ENV":                 true,
	"ITERION_PLUGINS_ENABLE":                           true,
	"ITERION_PLUGINS_DISABLE":                          true,
	"ITERION_CLAUDE_CODE_SETTING_SOURCES":              true,
	"ITERION_CLAUDE_CODE_STRICT_MCP":                   true,
	"ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS": true,
	"ITERION_PI_NO_CONTEXT_FILES":                      true,
	"ITERION_CLAW_SLASH_COMMANDS":                      true,
	"ITERION_WEB_SEARCH":                               true,
	// The permission gate's mode, and the LLM classifier, off unless its
	// model is named: a classifier Allow skips the workflow's tool_policy.
	"ITERION_PERMISSION":           true,
	"ITERION_PI_PERMISSION":        true,
	"ITERION_LLM_CLASSIFIER_MODEL": true,
	// Spend and billing guards.
	"ITERION_FORBID_SUBSCRIPTION_OAUTH": true,
	"ITERION_OPENAI_USE_OAUTH":          true,
	"ITERION_LOOP_BUDGET_GUARD":         true,
	// Trust roots, host files and filesystem exposure.
	"ITERION_JAVA_TRUSTSTORE": true,
	"ITERION_ENV_FILE":        true,
	"ITERION_BROWSE_ROOT":     true,
	// Code and configuration taken from the repository or the host: the
	// project connector tier, the repo's devbox toolchain, extra skills,
	// bot search paths, pi's agent directory, the builtin plugins' binaries.
	"ITERION_CONNECTOR_PROJECT_CATALOG": true,
	"ITERION_REPO_DEVBOX":               true,
	"ITERION_SKILLS":                    true,
	"ITERION_BOTS_PATH":                 true,
	"ITERION_PI_AGENT_DIR":              true,
	"ITERION_RTK_BIN":                   true,
	"ITERION_CODEINDEX_BIN":             true,
	// Spend ceilings and brakes outside the namespaces above. ROUTE_COOLDOWN
	// off is the fail-open probe; MODEL_SPECS disables the pricing table the
	// cost accounting reads.
	"ITERION_MAX_COST_PER_DAY_USD":         true,
	"ITERION_MAX_CONCURRENT_PIPELINES":     true,
	"ITERION_FORFAIT_CAP_PCT":              true,
	"ITERION_BUDGET_EXIT_GRACE":            true,
	"ITERION_PLAN_REVIEW":                  true,
	"ITERION_RETRY_CIRCUIT_THRESHOLD":      true,
	"ITERION_RETRY_CIRCUIT_COOLDOWN":       true,
	"ITERION_ROUTE_COOLDOWN":               true,
	"ITERION_MODEL_SPECS":                  true,
	"ITERION_CLAW_SLASH_COMMAND_MAX_BYTES": true,
	// Bounds on work that spends: node re-executions (no DSL field sets
	// them), schema-correction re-asks, a session spinning without progress
	// or failing its tools in a streak (0 disables the last three).
	"ITERION_NODE_MAX_RETRIES":                true,
	"ITERION_NODE_MAX_TRANSIENT_RETRIES":      true,
	"ITERION_OUTPUT_CORRECTION_BUDGET":        true,
	"ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT": true,
	"ITERION_PI_NO_PROGRESS_TIMEOUT":          true,
	"ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS":     true,
	// Defaults a node that names nothing inherits and a bot's author may
	// have relied on: its backend (what its tools and its gate can be), and
	// auto-memory, off so a run neither reads nor writes the operator's.
	"ITERION_DEFAULT_BACKEND":    true,
	"ITERION_BACKEND_PREFERENCE": true,
	"ITERION_AUTO_MEMORY":        true,
	// Workspace safety outside ITERION_WORKSPACE_: pruning a checkout the
	// run does not own, and the host disk the worktree pool and scratch
	// sweep bound.
	"ITERION_PRUNE_MIRROR_IN_CHECKOUT": true,
	"ITERION_WORKTREE_POOL_MAX":        true,
	"ITERION_SCRATCH_RETENTION":        true,
	// Rollout levers. RELIABILITY_MODE is authoritative over
	// EXECUTION_CONTEXT_POLICY, the emergency lever it generalises.
	"ITERION_EXECUTION_CONTEXT_POLICY": true,
	"ITERION_RELIABILITY_MODE":         true,
	"ITERION_OUTCOME_ROUTER":           true,
	"ITERION_DISPATCH_VIA_SERVICE":     true,
	"ITERION_RUNS_DETACHED":            true,
	"ITERION_MODE":                     true,
	"ITERION_DESKTOP":                  true,
	"ITERION_DESKTOP_ATTACH_DAEMON":    true,
	// Sessions, run ownership and the process itself.
	"ITERION_REFRESH_TTL":               true,
	"ITERION_LOCK_TTL":                  true,
	"ITERION_HEARTBEAT_INTERVAL":        true,
	"ITERION_METRICS_PORT":              true,
	"ITERION_POD_IP":                    true,
	"ITERION_HOME":                      true,
	"ITERION_PROJECT":                   true,
	"ITERION_PROJECT_STORE":             true,
	"ITERION_PROJECTS_CONFIG":           true,
	"ITERION_SCHEDULES_FILE":            true,
	"ITERION_SCHEDULER_INTERVAL":        true,
	"ITERION_SERVER_URL":                true,
	"ITERION_NATIVE_INDEX_RESCAN":       true,
	"ITERION_ORPHAN_RECONCILE_INTERVAL": true,
	"ITERION_WEBHOOK_SYNC_DEBOUNCE":     true,
	// Identities presented to forges and providers.
	"ITERION_FORGE_BRAND_AVATAR": true,
	"ITERION_GIT_AUTHOR_NAME":    true,
	"ITERION_GIT_AUTHOR_EMAIL":   true,
	"ITERION_LLM_USER_AGENT":     true,
	"ITERION_CODEX_VERSION":      true,
	// Written by the engine for a child process; a stored value would forge
	// the directory, capabilities or provenance the child is promised.
	"ITERION_ARTIFACT_FILES_DIR": true,
	"ITERION_WORKSPACE":          true,
	"ITERION_TENANT":             true,
	"ITERION_PARENT_RUN_ID":      true,
	"ITERION_TREE_NOISE":         true,
	"ITERION_STORE_DIR":          true,
	"ITERION_SOURCE_ISSUE_ID":    true,
	"ITERION_FACADE_SLOT":        true,
	"ITERION_FORFAIT_SUPPRESSED": true,
	"ITERION_CODEX_HOST_VERSION": true,
	"ITERION_PI_CONTRACT":        true,
	"ITERION_PI_CTRL":            true,
	"ITERION_PI_INTERACTION":     true,
	"ITERION_PI_ITERATION":       true,
	"ITERION_PI_NODE_ID":         true,
	"ITERION_PI_RUN_ID":          true,
	"ITERION_TOOL_NAME":          true,
	"ITERION_TOOL_INPUT":         true,
	"ITERION_DOTENV_PLANTED":     true,
	"ITERION_SCHEDULE":           true,
	"ITERION_SCHEDULE_BOT":       true,
}

// botVarsMax bounds the record so a runaway writer cannot grow the
// document the TTL cache re-reads on every expiry.
const (
	botVarsMaxKeys     = 200
	botVarsMaxValueLen = 256
)

// Validate rejects names outside the bot-var grammar, credential-shaped
// names (the same doctrine as the model-env allowlist: a var whose name
// suggests a secret must never transit a non-secret surface), infra
// namespaces, and unbounded values. One bad entry fails the whole write —
// nothing is persisted partially.
func (b BotVars) Validate() error {
	if len(b.Vars) > botVarsMaxKeys {
		return fmt.Errorf("platformcfg: bot_vars: %d keys exceeds the %d-key bound", len(b.Vars), botVarsMaxKeys)
	}
	for name, val := range b.Vars {
		if err := botVarEntryError(name, val); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWrite is the admin write's rule for a record that held prevKeys
// entries before the write: the key bound for a write that grows it, and the
// entry rule for the keys the write SETS. Entries already stored are not
// re-judged: a record written under an older rule stays editable and
// clearable — BotVarsOverlay keeps refusing its non-conforming entries at
// read time, and RefusedEntries names them — instead of every write failing
// on an entry the operator never touched.
func (b BotVars) ValidateWrite(prevKeys int, set []string) error {
	if len(b.Vars) > botVarsMaxKeys && len(b.Vars) > prevKeys {
		return fmt.Errorf("platformcfg: bot_vars: %d keys exceeds the %d-key bound", len(b.Vars), botVarsMaxKeys)
	}
	for _, name := range set {
		if err := botVarEntryError(name, b.Vars[name]); err != nil {
			return err
		}
	}
	return nil
}

// RefusedEntries maps each stored entry the entry rule refuses to the
// reason — the overrides BotVarsOverlay does not hand out. Nil when every
// entry conforms.
func (b BotVars) RefusedEntries() map[string]string {
	var out map[string]string
	for name, val := range b.Vars {
		if err := botVarEntryError(name, val); err != nil {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = err.Error()
		}
	}
	return out
}

// botVarEntryError is the rule one override must satisfy, shared by the
// writes (Validate, ValidateWrite) and the read (BotVarsOverlay) so they
// cannot drift.
func botVarEntryError(name, val string) error {
	if !botVarNameOK(name) {
		return fmt.Errorf("platformcfg: bot_vars: %q is not an overridable bot var (want ITERION_A_Z0_9 outside the infra/credential namespaces)", name)
	}
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("platformcfg: bot_vars: %s: value must not be blank (remove the key to clear the override)", name)
	}
	if strings.ContainsAny(val, "\n\r") {
		return fmt.Errorf("platformcfg: bot_vars: %s: value must be a single line", name)
	}
	if len(val) > botVarsMaxValueLen {
		return fmt.Errorf("platformcfg: bot_vars: %s: value exceeds %d bytes", name, botVarsMaxValueLen)
	}
	if bad := strings.IndexFunc(val, func(r rune) bool { return !botVarValueRune(r) }); bad >= 0 {
		r, _ := utf8.DecodeRuneInString(val[bad:])
		return fmt.Errorf("platformcfg: bot_vars: %s: value carries %q — allowed: letters, digits and ._:/@+=,%%-[]", name, r)
	}
	return nil
}

// BotVarsOverlay is the lookup cmd wiring installs with ir.SetEnvOverlay.
// Each stored value is checked against the write-time rule before it is
// handed out: Validate guards only the admin write, while a record can also
// be written by a binary carrying an older rule (server and runner roll out
// independently) or by hand — and a value that bypassed the charset reaches
// tool bodies, where a `{{…}}` inside it would be resolved by the reference
// pass that runs after the env expansion. A refused entry reads as unset, so
// the pod env and then the .bot default answer instead, and it is logged
// once per stored value.
func BotVarsOverlay(res *Resolver[BotVars], warn func(string, ...any)) func(name string) (string, bool) {
	var warned sync.Map
	return func(name string) (string, bool) {
		rec := res.Get(context.Background())
		if rec == nil {
			return "", false
		}
		v, ok := rec.Vars[name]
		if !ok {
			return "", false
		}
		if err := botVarEntryError(name, v); err != nil {
			if _, seen := warned.LoadOrStore(name+"\x00"+v, true); !seen && warn != nil {
				warn("platformcfg: bot_vars: stored override IGNORED, the pod env and the .bot default apply — %v", err)
			}
			return "", false
		}
		return v, true
	}
}

// botVarValueRune is the value charset. A stored value is substituted RAW
// into tool and script bodies — `${ITERION_*:-default}` there reads the
// overlay like every other expansion — so a shell metacharacter would run as
// code in every tenant's runs. Model ids (brackets included:
// claude-opus-5-5[1m]), provider chains, efforts, durations, numbers and
// paths all fit.
func botVarValueRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("._:/@+=,%-[]", r)
}

// withoutSegment is name with every `_`-separated segment equal to seg
// dropped.
func withoutSegment(name, seg string) string {
	parts := strings.Split(name, "_")
	kept := parts[:0]
	for _, p := range parts {
		if p != seg {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "_")
}

// botVarNameOK is the name gate: ITERION_-prefixed upper snake case, no
// credential-shaped words, no infra namespace.
func botVarNameOK(name string) bool {
	if !strings.HasPrefix(name, "ITERION_") || len(name) <= len("ITERION_") {
		return false
	}
	for _, r := range name {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	// Whatever the engine redacts as secret (store.IsSecretEnvName: TOKEN,
	// KEY, SECRET, PASSWORD, CREDENTIAL, AUTH anywhere) is no bot var — one
	// definition, read without an AUTHOR segment, a shipped bot's knob.
	if store.IsSecretEnvName(withoutSegment(name, "AUTHOR")) || strings.Contains(name, "PRIVATE") {
		return false
	}
	// Credential words too short, or too common, to match inside a name
	// (PAT in PATH): matched as whole segments. A webhook URL carries its
	// own bearer.
	for _, segment := range strings.Split(name, "_") {
		switch segment {
		case "PAT", "PATS", "BEARER", "BEARERS", "PASS", "PASSWD", "PASSPHRASE", "PASSCODE",
			"PASSFILE", "USERPASS", "HTPASSWD", "PW", "PWD", "CRED", "CREDS", "COOKIE", "COOKIES",
			"JWT", "JWTS", "PEM", "PEMS", "NETRC", "OTP", "OTPS", "TOTP", "HOTP", "HMAC", "HMACS",
			"SIGNATURE", "SESSIONID", "DSN", "WEBHOOK":
			return false
		}
	}
	if botVarsInfraExact[name] {
		return false
	}
	for _, p := range botVarsInfraPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// Store persists one settings family: Get returns (nil, nil) when no record
// exists yet; Put replaces the whole record (ReplaceOne semantics, so a
// cleared override really disappears).
type Store[T any] interface {
	Get(ctx context.Context) (*T, error)
	Put(ctx context.Context, rec T) error
}

// DefaultTTL bounds how long a replica serves a cached family record. The
// mutating replica invalidates immediately; the others converge within the
// TTL (the ADR-090 read-cache posture — Mongo stays the authority).
const DefaultTTL = 30 * time.Second

// PlatformCredentials is the audience record for the PLATFORM credential
// tier: the deployment's own DB-backed keys and forfaits, which until now
// every tenant without a credential of its own drew on silently.
//
// Enforcement is deliberately OPT-IN, and that shape is the whole design.
// A record whose Enforce is nil (or absent entirely) admits EVERY team —
// byte-identical to the behaviour before this family existed. Naming a team
// does not by itself lock the others out: an operator adding one team to
// the list would otherwise cut the fleet off from its only credential in a
// single write, discovering it as a fleet of 401s. Enforcement starts when
// they say so, once, explicitly.
//
// This is the platform-level sibling of identity.CredentialAudience, and
// the asymmetry between them is intentional: an ORG key is lent by someone
// who chose to lend it, so its zero value admits nobody; the PLATFORM key
// is what a deployment already runs on, so its zero value keeps running.
type PlatformCredentials struct {
	// Enforce turns the audience on. nil or false = every team may draw on
	// the platform tier (the historical behaviour). true = only Teams/Orgs.
	Enforce *bool `bson:"enforce,omitempty" json:"enforce"`
	// Teams is an explicit allow-list of team ids.
	Teams []string `bson:"teams,omitempty" json:"teams"`
	// Orgs admits every team of these orgs — the grain an operator actually
	// governs at, since a team is created inside an org without asking the
	// platform.
	Orgs []string `bson:"orgs,omitempty" json:"orgs"`
	// KeysFirst is the shared-tier fill order on one wire family. nil or
	// false = a forfait takes the family before an API key, which then
	// funds only the routes that name its provider (or the wire a closed
	// forfait leaves free); true = the key fills first and the forfait is
	// the backstop. It governs the platform AND org tiers — the deployment's
	// posture on spending a subscription it already pays for before a key
	// billed per token.
	KeysFirst *bool `bson:"keys_first,omitempty" json:"keys_first"`
	// FacadeDefault says whether a facade key (z.ai, Moonshot — another
	// vendor behind the anthropic wire, answering a claude id with its own
	// model) may become that wire's DEFAULT credential in a shared tier:
	// "auto" (only in a tier holding no Anthropic-native credential — a
	// Claude forfait, open or closed, or an anthropic key), "never"
	// (pinned-only: it funds the routes that name its provider and nothing
	// else), "always" (whenever the family is free, a closed forfait
	// falling through to it). nil = the env default, else "auto".
	FacadeDefault *string `bson:"facade_default,omitempty" json:"facade_default"`

	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	UpdatedBy string    `bson:"updated_by,omitempty" json:"updated_by,omitempty"`
}

// The deployment defaults of the two shared-tier ordering knobs (ADR-090:
// env = default, the record's field = runtime override). ValidateEnv refuses
// a boot on a value these do not read.
const (
	EnvKeysFirst     = "ITERION_PLATFORM_KEYS_FIRST"
	EnvFacadeDefault = "ITERION_PLATFORM_FACADE_DEFAULT"
)

// FacadePolicy is FacadeDefault's value space.
type FacadePolicy string

const (
	FacadeAuto   FacadePolicy = "auto"
	FacadeNever  FacadePolicy = "never"
	FacadeAlways FacadePolicy = "always"
)

func (f FacadePolicy) valid() bool {
	return f == FacadeAuto || f == FacadeNever || f == FacadeAlways
}

// PrefersKeys reports whether the shared tiers fill API keys before forfaits
// on a wire family: the record's value, else the env default, else false.
func (p *PlatformCredentials) PrefersKeys() bool {
	if p != nil && p.KeysFirst != nil {
		return *p.KeysFirst
	}
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(EnvKeysFirst)))
	return err == nil && v
}

// Facade is the effective facade policy: the record's value, else the env
// default, else auto.
func (p *PlatformCredentials) Facade() FacadePolicy {
	if p != nil && p.FacadeDefault != nil {
		if f := FacadePolicy(*p.FacadeDefault); f.valid() {
			return f
		}
	}
	if f := FacadePolicy(strings.ToLower(strings.TrimSpace(os.Getenv(EnvFacadeDefault)))); f.valid() {
		return f
	}
	return FacadeAuto
}

// ValidateEnv reports an env default neither knob can read — a deployment
// that set one meant something, and reading it as the built-in default in
// silence would decide the opposite of what the operator wrote.
func ValidateEnv() error {
	if v := strings.TrimSpace(os.Getenv(EnvKeysFirst)); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("platformcfg: %s=%q is not a boolean", EnvKeysFirst, v)
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvFacadeDefault)); v != "" && !FacadePolicy(strings.ToLower(v)).valid() {
		return fmt.Errorf("platformcfg: %s=%q — want auto, never or always", EnvFacadeDefault, v)
	}
	return nil
}

// Enforced reports whether the audience gates anything at all.
func (p *PlatformCredentials) Enforced() bool {
	return p != nil && p.Enforce != nil && *p.Enforce
}

// Allows reports whether (orgID, teamID) may draw on the platform tier.
// A nil record, or an unenforced one, admits everyone.
func (p *PlatformCredentials) Allows(orgID, teamID string) bool {
	if !p.Enforced() {
		return true
	}
	for _, id := range p.Orgs {
		if id != "" && id == orgID {
			return true
		}
	}
	for _, id := range p.Teams {
		if id != "" && id == teamID {
			return true
		}
	}
	return false
}

// Validate refuses a record that would enforce an audience admitting
// nobody. That state is reachable by accident (enable enforcement, forget
// the lists) and its symptom is every tenant-less run failing at its first
// LLM call — a fleet-wide outage expressed as a config typo.
func (p PlatformCredentials) Validate() error {
	if p.FacadeDefault != nil && !FacadePolicy(*p.FacadeDefault).valid() {
		return fmt.Errorf("platformcfg: facade_default %q — want auto, never or always", *p.FacadeDefault)
	}
	if p.Enforce == nil || !*p.Enforce {
		return nil
	}
	for _, id := range append(append([]string{}, p.Teams...), p.Orgs...) {
		if strings.TrimSpace(id) != "" {
			return nil
		}
	}
	return fmt.Errorf("platformcfg: enforcing the platform credential audience with no team and no org would refuse every run that has no credential of its own — name at least one, or leave enforce off")
}
