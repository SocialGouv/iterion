package envtrust

import (
	"fmt"
	"strings"
)

// Package-level file: the operator-grade name inventory.

// The lists lived in pkg/platformcfg as the bot-var gate's deny lists;
// they live here since the dotenv loaders ask the SAME inventory
// (OperatorGradeRefused): the H-10 review measured a project `.env`
// planting ITERION_SANDBOX_OVERRIDE=none, ITERION_DISABLE_AUTH and
// ITERION_BOOTSTRAP_ADMIN_PASSWORD silently on every lane — the file
// had MORE power over the engine than a bot's stored record, whose
// gate refused these very names. One inventory, two gates: a stored
// bot var must never set them (pkg/platformcfg botVarNameOK), and
// neither may the file a project holds.
// operatorInfraPrefixes are the env namespaces a DB record must never
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
var operatorInfraPrefixes = []string{
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
	// Whether a leased net's term may end on the work failing: a knob over
	// the election itself, which decides which REPLICA runs a net, not what
	// a run does.
	"ITERION_LEASE_",
	// Endpoints and identities presented outside: the metrics listener,
	// the CLI's remote instance, the forge app, web push, scan shards.
	"ITERION_PROMETHEUS_", "ITERION_REMOTE_", "ITERION_FORGE_GITHUB_APP_",
	"ITERION_WEBPUSH_", "ITERION_SHARD_",
	// Written by the engine for a child: a run's identity and stores, the
	// ticket it serves — and the bounds on a run's interactive shell.
	"ITERION_RUN_", "ITERION_ISSUE_",
	// A node session's variable (docs/adr/127-an-agent-nodes-processes-end-with-the-node.md):
	// a stored one would make a spawn carry another delegation's session, and
	// that delegation's end would stop it.
	"ITERION_NODE_SESSION_",
	// The sovereign-pool run-stream prefix (#2029) is JetStream topology,
	// not an environment knob: a stored bot var that could move a run's
	// stream would route it outside its pool.
	"ITERION_RUNS_POOL_",
}

// operatorInfraNames are single infra names outside those namespaces —
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
var operatorInfraNames = map[string]bool{
	"ITERION_PUBLIC_URL": true,
	// Written by the engine for a child (a run's identity in its
	// environment), and the escape hatch on the engine-child `.env` skip
	// (#2844): a stored record must never forge the first nor re-open the
	// workspace file for the second.
	EnvEngineChild: true,
	EnvDotEnvForce: true,
	// The schedule install's cron-line PATH bound (bytes): the process's
	// own posture, never a bot variable.
	"ITERION_SCHEDULE_PATH_MAX_BYTES": true,
	// The pin list is the process's own: a stored record cannot choose
	// which names it does not reach. The name itself is
	// pkg/platformcfg's EnvBotVarsEnvPins — ITERION_BOT_VARS_ENV_PINS —
	// spelled out here so this inventory stays a pure table (envtrust
	// cannot import the platformcfg package for one constant).
	"ITERION_BOT_VARS_ENV_PINS": true,
	"ITERION_DISABLE_AUTH":      true,
	"ITERION_SIGNUP_MODE":       true,
	// The pinned-version fallback dial (#1517): the SERVER's own posture —
	// whether a missing botsource pin recovers to the nearest older
	// version or refuses. It configures the process, never a bot's run.
	"ITERION_BOTSOURCE_PIN_FALLBACK": true,
	// The refresh reuse grace (#1668): the SERVER's own posture — how long
	// a replayed refresh token still shares its successor. It tunes the
	// session guard, never a bot's run.
	"ITERION_REFRESH_REUSE_GRACE": true,
	// The checkpoint coalescing window (#561): the ENGINE's own durability
	// posture — how much replay a crash may lose on a fan-out. It
	// configures the process, never a bot's run.
	"ITERION_PARALLEL_SAVE_WINDOW": true,
	"ITERION_BIN":                  true,
	"ITERION_PI_BIN":               true,
	"ITERION_OPENCODE_BIN":         true,
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
	"ITERION_CANONICAL_REDIRECT":          true,
	"ITERION_STUDIO_INSECURE_NONLOOPBACK": true,
	// Private-network reach (SSRF) and host allowlists.
	"ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE": true,
	"ITERION_LLM_ENDPOINT_ALLOW_PRIVATE":       true,
	"ITERION_OPENAI_COMPATIBLE_RESOLVED":       true,
	"ITERION_CONNECTOR_ALLOW_PRIVATE":          true,
	"ITERION_WEBHOOK_FORGE_HOSTS":              true,
	// The connector-action admission refusal: `off` lets a run start that
	// nothing here can serve, so it fails at the node as it used to. A stored
	// value must not decide that for a tenant's run.
	"ITERION_CONNECTOR_ADMISSION": true,
	// Code loaded from the repository under review or the host: MCP servers,
	// plugins, CLI settings sources, context files, slash commands.
	"ITERION_MCP_AUTOLOAD":                             true,
	"ITERION_MCP_HEALTHCHECK":                          true,
	"ITERION_SKIP_MCP_HEALTH":                          true,
	"ITERION_PI_MCP_SERVERS":                           true,
	"ITERION_MCP_EXPAND_UNTRUSTED_ENV":                 true,
	"ITERION_MCP_SERVER_ENV_PASSTHROUGH":               true,
	"ITERION_PLUGINS_ENABLE":                           true,
	"ITERION_PLUGINS_DISABLE":                          true,
	"ITERION_CLAUDE_CODE_SETTING_SOURCES":              true,
	"ITERION_CLAUDE_CODE_STRICT_MCP":                   true,
	"ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS": true,
	"ITERION_CLAUDE_CODE_BACKGROUND_TASKS":             true,
	"ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE":         true,
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
	"ITERION_OPENAI_STREAM_USAGE":       true,
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
	"ITERION_RETRY_PROBE_FLOOR":            true,
	"ITERION_RETRY_PROBE_BUDGET":           true,
	"ITERION_ROUTE_COOLDOWN":               true,
	"ITERION_MODEL_SPECS":                  true,
	"ITERION_CLAW_SLASH_COMMAND_MAX_BYTES": true,
	// The read budget of a bot — its .bot files together, or its YAML
	// twin: a bound on what an uploaded bot may cost the server that reads
	// it, 0 removing it.
	"ITERION_DSL_READ_BUDGET": true,
	// The server-wide aggregate of concurrent readings and its refusal grace
	// (#2693): bounds on what untrusted input may cost the PROCESS, 0
	// removing the first. A stored bot var must never widen either.
	"ITERION_DSL_READ_ADMISSION_MAX_BYTES": true,
	"ITERION_DSL_READ_ADMISSION_WAIT":      true,
	// The boot-time GOMEMLIMIT derivation's off switch (#2693): the
	// process's own memory posture, never a bot's run.
	"ITERION_MEMLIMIT": true,
	// Bounds on work that spends: node re-executions (no DSL field sets
	// them), schema-correction re-asks, a session spinning without progress
	// or failing its tools in a streak (0 disables the last three).
	"ITERION_NODE_MAX_RETRIES":                true,
	"ITERION_NODE_MAX_TRANSIENT_RETRIES":      true,
	"ITERION_OUTPUT_CORRECTION_BUDGET":        true,
	"ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT": true,
	"ITERION_PI_NO_PROGRESS_TIMEOUT":          true,
	"ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS":     true,
	// How patient a claude_code session is with the background work it
	// launched — the wave's budget, when to nudge a CLI that takes no turn,
	// how long an answer to that nudge may take, how long a report is asked
	// for, how long an idle must hold, how long a result on its way is
	// waited for. Refused like their sibling NO_PROGRESS_TIMEOUT above: at
	// their edges they disable what they bound (a wave budget of 0 is
	// unbounded, a grace of 0 is no grace), so a stored bot var never tunes
	// the engine's patience — the operator's environment does.
	"ITERION_CLAUDE_CODE_BACKGROUND_WAIT":             true,
	"ITERION_CLAUDE_CODE_BACKGROUND_AUTOTURN_GRACE":   true,
	"ITERION_CLAUDE_CODE_BACKGROUND_ANSWER_WAIT":      true,
	"ITERION_CLAUDE_CODE_BACKGROUND_FINALIZE_TIMEOUT": true,
	"ITERION_CLAUDE_CODE_BACKGROUND_IDLE_SETTLE":      true,
	"ITERION_CLAUDE_CODE_BACKGROUND_RESULT_WAIT":      true,
	// The drain bound on every child process the engine starts (#2469):
	// at its edge (0, "off") it REMOVES the bound, so a stored value could
	// park a node for as long as a descendant holds a pipe. Process
	// supervision is the operator's environment, never a bot's variable.
	"ITERION_PROC_WAIT_DELAY": true,
	// Defaults a node that names nothing inherits and a bot's author may
	// have relied on: its backend (what its tools and its gate can be),
	// auto-memory, off so a run neither reads nor writes the operator's, and
	// the ambient context, the repository's instructions without the
	// operator's setup (ADR-119).
	"ITERION_DEFAULT_BACKEND":    true,
	"ITERION_BACKEND_PREFERENCE": true,
	"ITERION_AUTO_MEMORY":        true,
	"ITERION_AMBIENT_CONTEXT":    true,
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
	// The forge publish's read-back budget (pkg/forge/gitlab), and the
	// escape hatch on the GitLab quick-action guard (pkg/forge).
	"ITERION_FORGE_READBACK_BUDGET":      true,
	"ITERION_FORGE_GITLAB_QUICK_ACTIONS": true,
	// Identities presented to forges and providers.
	"ITERION_FORGE_BRAND_AVATAR": true,
	"ITERION_GIT_AUTHOR_NAME":    true,
	"ITERION_GIT_AUTHOR_EMAIL":   true,
	"ITERION_LLM_USER_AGENT":     true,
	"ITERION_CODEX_VERSION":      true,
	// Written by the engine for a child process; a stored value would forge
	// the directory, capabilities or provenance the child is promised.
	"ITERION_ARTIFACT_FILES_DIR": true,
	"ITERION_ENGINE_BIN":         true,
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
	// The dotenv loader's opt-in (internal/envtrust): it widens what a
	// project `.env` may plant — the routing class — so it is the process's
	// own posture, never a bot variable. A stored record must not widen what
	// the repositories a bot reviews get to name.
	"ITERION_DOTENV_ENDPOINTS": true,
	"ITERION_SCHEDULE":         true,
	"ITERION_SCHEDULE_BOT":     true,
}

// InfraGradeName reports whether name is one of the operator-grade config
// names above — the inventory the bot-var gate asks (pkg/platformcfg's
// botVarNameOK refuses a stored record any of them). Asked by that gate and
// by OperatorGradeRefused alike, so the two gates cannot drift: a name the
// bot-var gate refuses is a name no dotenv loader plants, and a name added
// to a list answers both the day it lands.
func InfraGradeName(name string) bool {
	upper := strings.ToUpper(name)
	if operatorInfraNames[upper] {
		return true
	}
	for _, prefix := range operatorInfraPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// OperatorGradeRefused answers the loader question behind the H-10 finding:
// may the `.env` a project holds answer for THIS name? Everything of the
// operator's grade is refused — the infra names and namespaces the bot-var
// gate refuses a stored record (a project file has no more authority than
// a bot), widened to every credential-shaped ITERION_ name
// (CredentialShapedName — the very list the bot-var gate's name half asks,
// so the parity its comment proclaims holds on BOTH halves: the H-10
// review measured the drift, twenty names refused to a bot yet planted by
// the file — ITERION_GH_PAT, ITERION_DB_PW, ITERION_SESSION_COOKIE — all
// test-only that day, structural until this closure). The substring half
// of that list keeps the mid-name spellings answered as they were here —
// ITERION_SERVER_TOKEN (cmd/iterion scan_shards.go, the token a shard
// presents to the server) and ITERION_COMPLETION_WEBHOOK_SECRET
// (pkg/notify) are not namespaced and carry the secret mid-name. Scoped to
// ITERION_ on purpose — a provider credential (OPENAI_API_KEY,
// AWS_SECRET_ACCESS_KEY) in a `.env` next to the project is what the file
// is FOR, and the operator's own exported one always wins; it is the
// engine's own secrets a project has no business answering for. The
// opt-in does NOT answer here: ITERION_DOTENV_ENDPOINTS widens what a file
// may plant as ROUTES or host-env execution names; the operator's posture
// — whether the sandbox runs, whether auth is on, which secret signs the
// sessions — is never a file's to set, so the refusal reads the same with
// the opt-in exported and without.
//
// The loaders ask it through DotenvPlantRefused (one entry point per
// plant); the reads ask it through Inherited at the chokepoints that serve
// the operator's own configuration (pkg/config's lookup, pkg/server's
// envOperator), so a value that slipped past a loader an older binary ran
// — the marker travels to children — answers nothing there either.
func OperatorGradeRefused(name string) bool {
	upper := strings.ToUpper(name)
	if !strings.HasPrefix(upper, "ITERION_") {
		return false
	}
	return InfraGradeName(upper) || CredentialShapedName(upper)
}

// HasCommandSubstitution reports whether a VALUE spells shell execution —
// a command substitution (`$(...)` or a backtick pair), a process
// substitution (`<(...)` / `>(...)`, which forks the named command the
// moment a shell evaluates the value — the H-10 probe planted one and the
// eval created the marker), or a bash parameter-expansion operator
// reference (`${name@P}` and kin). Asked
// about the value a dotenv loader is about to apply, before any shell sees
// it: the studio snapshot used to hand values to a child bash (the legacy
// instance manager's frame), and a sourced `FOO=$(...)` EXECUTED on the
// host — the H-10 probe measured the marker file created, before the
// routing question was even asked. With the parse-first loaders no lane of
// ours evaluates a value any more (pkg/projectenv expands the simple
// references itself, in Go), and the refusal keeps the TRIGGER from
// travelling: the value reaches a run one fork away, where a shell — or an
// eval, or a prompt — might. `${name@P}` is in the class for the same
// reason the round-6 probe measured: bash's prompt expansion RUNS the
// command substitutions the referenced variable's value carries, so a
// planted `KEY=${BAR@P}` executed the base environment's `BAR=$(touch …)`
// at the expansion itself; and any operator form is refused, not just the
// prompt one — the letters bash grows are the next round's finding. The
// scan is on the raw bytes and knows no quoting: a single-quoted literal
// `'$('` is refused too — the price of deciding before any shell runs.
func HasCommandSubstitution(value string) bool {
	if strings.Contains(value, "$(") || strings.Contains(value, "`") ||
		strings.Contains(value, "<(") || strings.Contains(value, ">(") {
		return true
	}
	return HasExpansionOperatorReference(value)
}

// HasExpansionOperatorReference reports whether a value carries a
// `${name@op}` bash parameter-expansion operator: the `@` plus a letter
// inside a `${...}` pair, in any case (`@P` is the prompt expansion that
// executes; the other letters grow with bash). An unterminated `${name@P`
// is not one: bash reports a bad substitution for it, it executes nothing,
// and the Go expander keeps it literal.
func HasExpansionOperatorReference(value string) bool {
	rest := value
	for {
		open := strings.Index(rest, "${")
		if open < 0 {
			return false
		}
		rest = rest[open+2:]
		close := strings.Index(rest, "}")
		if close < 0 {
			return false // unterminated: bash reports a bad substitution, it runs nothing
		}
		segment := rest[:close]
		for i := 0; i+1 < len(segment); i++ {
			if segment[i] == '@' && isASCIILetter(segment[i+1]) {
				return true
			}
		}
		rest = rest[close+1:]
	}
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isNameStart(b byte) bool {
	return b == '_' || isASCIILetter(b)
}

func isNameByte(b byte) bool {
	return isNameStart(b) || (b >= '0' && b <= '9')
}

// CarriesExpansionReference reports whether a value carries a simple
// `$NAME` / `${NAME}` reference — the shapes the studio lane's Go
// expander (pkg/projectenv expandReferences) replaces. The CLI and
// desktop lanes plant NO expansion: a reference there is applied as the
// literal bytes the file spelled, where the same file on the studio lane
// arrives resolved — the H-10 review measured `GIT_EDITOR=$HOME/bin/gedit`
// planted literal on the CLI lane (a child then execs a path named
// `$HOME/...`) while the studio lane expanded it. The lanes that plant
// raw ask this to WARN, not to refuse: a literal `$` is a legitimate
// value, and the operator decides. A single-quoted value is outside the
// question — the studio lane keeps those bytes too. The escapes and forms
// the expander keeps dead are not references: `\$` (resolved to one
// dollar), `$$`, `${VAR:-default}`, `${VAR@P}`, a trailing `$`.
func CarriesExpansionReference(value string) bool {
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '\\' && i+1 < len(value) && value[i+1] == '$':
			i++ // the expander resolves the pair to one literal dollar
		case c == '\\' && i+1 < len(value):
			i++ // the expander keeps the pair literal, both bytes
		case c != '$':
		case i+1 < len(value) && value[i+1] == '{':
			end := i + 2
			for end < len(value) && isNameByte(value[end]) {
				end++
			}
			if end > i+2 && end < len(value) && value[end] == '}' {
				return true
			}
		case i+1 < len(value) && isNameStart(value[i+1]):
			return true
		}
	}
	return false
}

// UnexpandedReferenceWarningLine renders the warning for a value whose
// references the planting lane applies unexpanded: the same shape as the
// refusal lines (the name, the file, what happens), with the operator's
// two ways out. It is a WARNING, not a refusal — the bytes are the
// operator's to plant, and the divergence with the studio lane is what
// the line exists to make loud.
func UnexpandedReferenceWarningLine(name, source, path string) string {
	return fmt.Sprintf("%s comes from %s %s with a $-reference in its value ($VAR / ${VAR}); this lane plants no expansion, so the reference is applied as the literal bytes the file spelled — the studio lane would expand the same file. Put the resolved value in the file, or single-quote it to keep the bytes on every lane", name, source, path)
}

// CommandSubstitutionRefusalLine renders the refusal for a value carrying
// shell execution — a command substitution or a `${name@op}` operator
// reference: the same shape as RoutingRefusalLine (the name, the file,
// why), with no opt-in half — ITERION_DOTENV_ENDPOINTS widens routes and
// host-env names, never execution, so nothing the operator exports lets a
// file hand a shell a trigger. The caller puts the line on stderr
// directly, like the routing refusal.
func CommandSubstitutionRefusalLine(name, source, path string) string {
	return fmt.Sprintf("%s comes from %s %s with shell execution spelled in its value ($(...), <(...)/>(...), backticks, or a ${name@op} expansion operator), so it is not applied: a dotenv file does not run commands — put the wanted value in the file, or export %s in the environment iterion runs with", name, source, path, name)
}
