package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

func resolveMaxConsecutiveToolErrors() int {
	if v := os.Getenv("ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultMaxConsecutiveToolErrors
}

// suppressedForfaitDir is the poisoned CLAUDE_CONFIG_DIR handed to the
// CLI when the caller explicitly wants forfait auth REFUSED (a facade
// hint with no facade key). mergeCmdEnv turns an empty
// value into an absent env var, and the claude CLI then defaults
// CLAUDE_CONFIG_DIR to $HOME/.claude — on a developer laptop that
// has run `claude login`, a valid forfait sits there and the CLI
// authenticates as that operator's account, re-opening the leak
// #1390 pinned. Pointing at a well-formed absolute path we know
// does not exist forces the CLI's `.credentials.json` read to fail
// and no fallback path is left. The value is never emitted anywhere
// external; the file at that path is never opened by iterion itself.
const suppressedForfaitDir = "/nonexistent/iterion-suppress-forfait"

// ForfaitSuppressedEnvKey is the iterion-internal marker that names a
// suppression map (a facade hint with no facade key). Every reader
// of CLAUDE_CONFIG_DIR inside iterion (providerFingerprint,
// SessionFilesRoot, and any future one) tests THIS key before deciding
// the OAuth forfait is set — pointing the CLI at a non-existent path
// alone does not tell iterion's own code that the caller wants forfait
// auth refused, so `providerFingerprint` would render the poisoned dir
// as `anthropic-oauth` and persist it into node output / session
// fingerprint / usagecap Reading.Source, and `SessionFilesRoot` would
// write session transcripts under it. The marker travels with the map
// (a plain env entry): the CLI subprocess sees an unknown var and
// ignores it, while iterion's readers key on it. See #1390's PR-review
// finding R0a39d6.
const ForfaitSuppressedEnvKey = "ITERION_FORFAIT_SUPPRESSED"

// FacadeSlotEnvKey is the iterion-internal marker naming WHICH anthropic-wire
// facade built an env map, written by facadeEnvFor's builders and read by
// providerFingerprint so the slot travels with the routing label.
//
// Without it the slot has to be re-derived from ANTHROPIC_BASE_URL at read
// time, and that URL does not identify a vendor: it is operator-controlled,
// and two facades legitimately hold the same value. The generic way to put
// Claude Code on Kimi is `export ANTHROPIC_BASE_URL=<moonshot endpoint>` —
// the variable zaiEnv reads — and one internal gateway in front of both
// vendors does the same. A reader deciding between equal labels answers with
// whichever it compared first, so a Moonshot wall lands on the z.ai
// fingerprint: the meter then parks the healthy key and keeps handing out the
// walled one.
//
// Same shape as ForfaitSuppressedEnvKey above, for the same reason: the fact
// is written by the code that KNOWS it and travels with the value it
// qualifies. The CLI subprocess sees an unknown variable and ignores it.
const FacadeSlotEnvKey = "ITERION_FACADE_SLOT"

// facadeSourcePrefix opens every facade routing label
// ("facade:<slot>:<base-url>"). One constant, read by the renderer and by the
// parser: two spellings of a wire shape is how a reading ends up charged to
// the wrong key.
const facadeSourcePrefix = "facade:"

// isForfaitSuppressed reports whether the given env map declares the
// suppression marker. Cheap enough to call from every reader of
// CLAUDE_CONFIG_DIR.
func isForfaitSuppressed(env map[string]string) bool {
	if env == nil {
		return false
	}
	return env[ForfaitSuppressedEnvKey] != ""
}

// settingSourcesFromEnv returns the CLI --setting-sources for claude_code
// nodes. Default "user,project": load the operator's user-level CLAUDE.md /
// settings.json and the target repo's project CLAUDE.md / .claude/settings.json
// so the agent honours the same conventions native Claude Code would — a core
// part of closing the adaptivity gap. Override via
// ITERION_CLAUDE_CODE_SETTING_SOURCES (comma-separated user/project/local);
// "" or "none" disables it, restoring the CLI's headless no-settings default.
// "local" is omitted from the default: .claude/settings.local.json is
// machine-specific and may carry absolute paths that don't resolve in a sandbox.
func settingSourcesFromEnv() []claudesdk.SettingSource {
	raw, ok := os.LookupEnv("ITERION_CLAUDE_CODE_SETTING_SOURCES")
	if !ok {
		raw = "user,project"
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil
	}
	var out []claudesdk.SettingSource
	for _, part := range strings.Split(raw, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "user":
			out = append(out, claudesdk.SettingSourceUser)
		case "project":
			out = append(out, claudesdk.SettingSourceProject)
		case "local":
			out = append(out, claudesdk.SettingSourceLocal)
		}
	}
	return out
}

// strictMCPFromEnv reports whether claude_code nodes should run with
// --strict-mcp-config. Default TRUE: the MCP set iterion resolves for the
// node (the .bot's `mcp_server:`/`mcp:` blocks, the target repo's .mcp.json
// via autoload_project, plus iterion's own ask_user/board servers — all
// passed via --mcp-config) is the complete truth, and the operator's
// personal user-scope servers (~/.claude.json) do NOT boot inside bot
// nodes. Without it the CLI merges those on top: undeclared tools reach
// the agent, per-visit npx/server boots spike CPU on loop-heavy bots, and
// personal API keys land on the subprocess argv (issue #506).
// ITERION_CLAUDE_CODE_STRICT_MCP=0 (or false/off/no) is the escape hatch
// that restores host-config inheritance.
func strictMCPFromEnv() bool {
	raw, ok := os.LookupEnv("ITERION_CLAUDE_CODE_STRICT_MCP")
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// orchestrationTools is the claude_code tool surface that spawns background
// work (Agent, and Task on older CLIs) or waits on it (TaskOutput, Monitor).
// Withheld as one unit by ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS:
// a waiter without a spawner is only a way to deadlock, and a spawner
// without a waiter leaves background results unreadable.
var orchestrationTools = []string{"Agent", "Task", "TaskOutput", "Monitor"}

// workflowOrchestrationTools is the multi-agent surface ultracode grants:
// withheld from every node that is not in ultracode mode, knob or not.
var workflowOrchestrationTools = []string{"Workflow"}

// disallowOrchestrationToolsFromEnv reads
// ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS (unset/other → false;
// "1"/"true"/"on"/"yes" → true).
func disallowOrchestrationToolsFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// taskSandboxed reports whether the task's CLI subprocess executes inside
// a REAL sandbox container. The noop driver is a host passthrough — its
// Run handle is non-nil but every command still runs on the host with
// host paths, so it must NOT trigger in-container credential remapping.
func taskSandboxed(task Task) bool {
	return task.Sandbox != nil && task.Sandbox.Driver() != "noop"
}

// ambientAnthropicEnvForSandbox returns the Anthropic-flavoured
// credential vars present in THIS process' environment, for explicit
// forwarding into a sandboxed CLI spawn.
//
// On the host path the claude subprocess inherits os.Environ()
// (hostSpawnEnv / the SDK's default spawn), so an ambient
// CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY — the prod runner's
// forfait delivery channel (the `iterion-forfait` secret sets
// CLAUDE_CODE_OAUTH_TOKEN on the runner pod) — authenticates every run
// with NO ctx credentials. A sandboxed spawn inherits the CONTAINER env
// instead (kubectl/docker exec + the SDK env map only), so that ambient
// credential silently vanished: observed live on the first sandboxed
// cloud run (019f8a6c) — every claude exec, main pass and formatting
// pass alike, ran with env_keys=1 and died `Not logged in · Please run
// /login` (4s, 0 tokens), surfacing as the opaque "structured output
// invalid: missing required field …". Values are forwarded VERBATIM —
// the CLI applies its own precedence among them, exactly as it does for
// inherited host env.
func ambientAnthropicEnvForSandbox() map[string]string {
	env := map[string]string{}
	for _, k := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_OAUTH_TOKEN",
	} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// credEnvToOpts converts a credential env map into claudesdk.Option
// values with a stable key order. Extracted so the cross-provider
// fingerprint path can compute the env map once, derive a fingerprint,
// and pass the same map to the SDK without recomputing.
func credEnvToOpts(env map[string]string) []claudesdk.Option {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	opts := make([]claudesdk.Option, 0, len(keys))
	for _, k := range keys {
		opts = append(opts, claudesdk.WithEnv(k, env[k]))
	}
	return opts
}

// shouldDropSessionFork decides whether to skip --resume + --fork-session
// for the incoming task. Thinking blocks in a Claude session carry
// provider-specific signatures; reusing a session built on a different
// provider surfaces HTTP 400 "Invalid signature in thinking block"
// the moment the new provider reads the prior conversation.
//
// Drop policy (forks only — a bare resume from the same daemon process
// is always same-provider continuation, so signatures are trustworthy):
//
//   - parent fingerprint set AND names another route than current
//     (sameSessionRoute) → drop.
//   - parent fingerprint EMPTY (legacy output produced by a binary
//     that predates the stamp, or by a daemon restarted across a
//     provider switch) → drop conservatively. The alternative —
//     "proceed when unknown" — was the actual observed failure mode:
//     a fresh Anthropic daemon attempting to fork a session-id
//     produced by an older ZAI-side binary blew up on the 400 with
//     nothing flagging the mismatch. Losing head-session continuity
//     for one node is recoverable; a 400 is not.
//   - parent fingerprint set, current fingerprint EMPTY (provider
//     env is currently unresolved — e.g. cred ctx not wired) → keep
//     the fork. The CLI subprocess will fall back to inherited env,
//     and if that's a mismatch the surface error is the same 400 we
//     started with; we don't gain anything by dropping pre-emptively
//     when we can't classify ourselves.
//
// Returns (drop, reason). The reason string carries no secrets and is
// safe to log verbatim.
func shouldDropSessionFork(task Task, currentFingerprint string) (bool, string) {
	if !task.ForkSession {
		return false, ""
	}
	if task.SessionFingerprint == "" {
		return true, "parent session has no recorded provider fingerprint (legacy output or pre-stamp binary) — starting fresh to avoid cross-provider thinking-block 400s"
	}
	if currentFingerprint != "" && !sameSessionRoute(task.SessionFingerprint, currentFingerprint) {
		return true, fmt.Sprintf("parent session was built on %q but current provider is %q (signed thinking blocks would 400 on cross-provider reuse)",
			task.SessionFingerprint, currentFingerprint)
	}
	return false, ""
}

// sameSessionRoute reports whether two provider fingerprints name the same
// route for session reuse. Equal labels do. So does a facade label with no
// slot against one with a slot, on the same rendered base URL: the slot-less
// form names no credential — it is what a binary that predates the slot stamp
// persisted, and what an ambient base URL renders — so it cannot contradict a
// slot, and before the stamp both rendered the same string. Without this, every
// facade session persisted before the stamp would be dropped on its first fork
// after an upgrade. Two DIFFERENT slots on one base URL are two vendors behind
// one gateway, and stay apart.
func sameSessionRoute(a, b string) bool {
	if a == b {
		return true
	}
	slotA, baseA, okA := splitFacadeLabel(a)
	slotB, baseB, okB := splitFacadeLabel(b)
	return okA && okB && baseA == baseB && (slotA == "" || slotB == "")
}

// providerFingerprint derives a stable identifier for the routing
// decision encoded by a cred env map. Two calls to anthropicCredEnvForCLI
// with the same provider precedence return the same fingerprint, so
// sessions produced under one provider can be detected (and dropped)
// when a later run targets a different one. Key values are NOT
// included — fingerprints are safe to log and to ferry through the
// recipe output map. The one component that is operator-supplied text
// rather than a fixed label, the facade base URL, goes through
// facadeLabel first for exactly that reason.
func providerFingerprint(env map[string]string) string {
	if env == nil {
		return "anthropic-env"
	}
	// The suppression marker wins over CLAUDE_CONFIG_DIR because the
	// poisoned sentinel written by the zai no-key branch is NOT a
	// real OAuth forfait — rendering it as `anthropic-oauth` would
	// persist the sentinel into node output, cross-provider fork
	// guards and usagecap Readings (R0a39d6). Fall through to the
	// env label instead so the fingerprint reads as "suppressed
	// Anthropic" and does not collide with the direct label.
	suppressed := isForfaitSuppressed(env)
	if base := env["ANTHROPIC_BASE_URL"]; base != "" {
		// The slot the env was built FOR, when one stamped it: the base URL
		// alone does not name a vendor (FacadeSlotEnvKey says why). An env
		// whose base URL came from somewhere else — an operator's ambient
		// value forwarded into a sandboxed spawn — carries no slot and keeps
		// the bare form, which names no credential and is read as such.
		if slot := env[FacadeSlotEnvKey]; slot != "" {
			return facadeSourcePrefix + slot + ":" + facadeLabel(base)
		}
		return facadeSourcePrefix + facadeLabel(base)
	}
	if env["ANTHROPIC_API_KEY"] != "" {
		return "anthropic-direct"
	}
	if !suppressed && env["CLAUDE_CONFIG_DIR"] != "" {
		return "anthropic-oauth"
	}
	if suppressed {
		return "anthropic-suppressed"
	}
	// Explicit zeroing of BASE_URL/AUTH_TOKEN (the providerHint==anthropic
	// path) lands here too — it means "use the inherited ANTHROPIC_API_KEY
	// from the process env", which is also Anthropic-direct semantically.
	return "anthropic-env"
}

// facadeLabel renders an operator-supplied ANTHROPIC_BASE_URL as a
// fingerprint component that carries no credential.
//
// The URL is operator input and may embed one — https://<token>@host/…,
// or ?api_key=… — and the fingerprint is not a debug string: it rides
// the node's output map (SessionFingerprintKey), the session slots in
// run.json, NodeServed.Fingerprint, events.jsonl and a usagecap
// Reading.Source, all readable by anyone with run-read access. The
// secret guard is no backstop: it masks values it was SEEDED with, and
// a token typed into a base URL never passed through the secret
// plumbing, so it is unredacted on the event path too. Hence the fix
// here, at the single point that builds the value.
//
// Two properties are load-bearing, because this value is an equality key
// for session reuse (shouldDropSessionFork):
//   - STABLE — one URL always renders one label, or every call decides
//     the parent session came from a different provider and drops it.
//   - NON-COLLIDING — two distinct URLs never render alike, or a session
//     built on one facade is resumed on another and its provider-signed
//     thinking blocks 400. So the stripped components are replaced by a
//     digest of the WHOLE original rather than simply dropped.
//
// scheme+host+path survives verbatim: it is the readable half, it is
// what distinguishes facades in practice, and a URL carrying none of
// the three credential-bearing components — the ordinary case — is
// returned unchanged, so sessions stay forkable across this change.
// A secret in the PATH itself is out of reach of this (stripping the
// path would collapse facades that differ only there); the three
// components handled are the ones a URL is credential-bearing by
// convention.
func facadeLabel(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		// Unparseable: keep none of it. The digest alone is still
		// stable and still tells two different values apart.
		return redactedLabel("unparseable-url", base)
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return base
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return redactedLabel(u.String(), base)
}

// redactedLabel joins what survived redaction to the collision guard for
// what did not: enough bits that two facade URLs never share one, one-way
// so the original is not recoverable from a run record.
//
// Deliberately NOT url-shaped. An operator reading a run record has to be
// able to tell what they typed from what iterion removed, and a bare
// "#<hex>" suffix would read as a fragment they wrote themselves — on a
// value that exists precisely to answer "what actually served this node"
// without ambiguity.
func redactedLabel(kept, original string) string {
	sum := sha256.Sum256([]byte(original))
	return kept + " [redacted:" + hex.EncodeToString(sum[:6]) + "]"
}

// stampUsageSource wraps an OnUsageWindow hook so every reading leaving a
// session names the provider routing it ran on — the runner's meter then
// charges a refusal to the credential that was actually spent. Task and
// TaskHooks travel by value, so the wrap lives and dies with one Execute
// call: a fallback attempt that re-enters with a different provider stamps
// its own label. A reading that already names its source keeps it.
func stampUsageSource(inner func(usagecap.Reading) error, fingerprint string) func(usagecap.Reading) error {
	if inner == nil {
		return nil
	}
	return func(r usagecap.Reading) error {
		if r.Source == "" {
			r.Source = fingerprint
		}
		return inner(r)
	}
}

// anthropicCredEnvForCLI is the testable core: it returns the env
// variables (key → value) the claude_code subprocess should be invoked
// with, based on the context-bound credentials and the optional
// providerHint. anthropicCredOptsForCLI wraps it into claudesdk.Option
// values for the SDK call site. Separated so unit tests can assert
// routing decisions without reflecting on closures.
//
// An empty key string with a non-empty key entry means "clear this
// inherited env var" (e.g. {"ANTHROPIC_BASE_URL": ""} actively
// suppresses a stale z.ai value in the parent env when the hint asks
// for Anthropic-direct).
// claudeForfaitEnv wires the CLI to a per-run OAuth-forfait credentials.json
// (desktop `claude login` shape) via CLAUDE_CONFIG_DIR — and actively
// SUPPRESSES any Anthropic-flavoured credential inherited from the process
// env. The claude CLI prefers ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN over the
// OAuth token in CLAUDE_CONFIG_DIR, so a cloud runner that carries a shared
// ANTHROPIC_API_KEY in its pod env (e.g. an operator-configured key, possibly
// dead) would otherwise override the forfait — the run then fails with the
// inherited key's error ("Credit balance is too low") even though a valid
// forfait was resolved. Setting the vars to "" overrides the inheritance so the
// CLI falls through to the OAuth token. Mirrors how the z.ai/anthropic hints
// clear the base-URL/token to stop a stale value leaking in.
// When sandboxed is true the CLI runs INSIDE a sandbox container where the
// host temp dir does not exist, so CLAUDE_CONFIG_DIR is pointed at the
// in-sandbox writable config dir the runtime seeded from the ADR-070
// forfait file secret (secrets.ClaudeCodeSandboxConfigDir) — while the
// CLAUDE_CODE_OAUTH_TOKEN below is still read from the HOST file, which the
// runner's refresher keeps fresh per spawn (ADR-082 Phase 3 blocker 3).
func claudeForfaitEnv(dir string, sandboxed bool) map[string]string {
	configDir := dir
	if sandboxed {
		configDir = secrets.ClaudeCodeSandboxConfigDir
	}
	env := map[string]string{
		"CLAUDE_CONFIG_DIR":    configDir,
		"ANTHROPIC_API_KEY":    "",
		"ANTHROPIC_AUTH_TOKEN": "",
		"ANTHROPIC_BASE_URL":   "",
	}
	// Also pass the OAuth access token via CLAUDE_CODE_OAUTH_TOKEN — the
	// headless auth path the Claude Code CLI checks BEFORE the credentials file
	// (and before any apiKeyHelper), the same one its own UI hints at
	// ("export CLAUDE_CODE_OAUTH_TOKEN=<token>"). The file path
	// ($CLAUDE_CONFIG_DIR/.credentials.json) works standalone — verified with
	// the runner's exact CLI build — but a cloud runner's full inherited pod
	// env can shadow it, so the CLI reports "Not logged in" despite a valid
	// materialised forfait. Reading it here from the materialised file (kept
	// fresh by the runner's refresh worker; re-read per spawn) makes the env
	// token deterministically win.
	//
	// ALWAYS write the key, empty when there is no usable token. Leaving it
	// ABSENT is not neutral: this variable outranks the credentials file, the
	// host spawn inherits os.Environ(), and a prod runner pod carries an
	// ambient CLAUDE_CODE_OAUTH_TOKEN of its own — the PLATFORM forfait. An
	// absent key therefore lets that platform token serve in place of the
	// per-run CLAUDE_CONFIG_DIR just pointed at, so a tenant whose blob is
	// missing or stale (the refresh worker lagged) authenticates and bills
	// against someone else's Claude account instead of failing. The three
	// ANTHROPIC_* siblings above are cleared for exactly this reason; this one
	// is the same class.
	env["CLAUDE_CODE_OAUTH_TOKEN"] = readForfaitAccessToken(dir)
	return env
}

// readForfaitAccessToken extracts claudeAiOauth.accessToken from the
// materialised Claude Code credentials.json in dir. Returns "" (never an error)
// when the file is absent or malformed — the caller degrades to the file path.
func readForfaitAccessToken(dir string) string {
	return secrets.AnthropicForfaitAccessToken(dir)
}

// sandboxed reports that the CLI subprocess will execute inside a REAL
// sandbox container (docker/kubernetes — not the host-passthrough noop), so
// forfait credential paths must resolve to in-container locations.
// zaiEnv is the ONE place a z.ai key becomes CLI env, so the endpoint it is
// sent to cannot depend on where the key came from. An operator's
// ANTHROPIC_BASE_URL is an explicit routing choice — a self-hosted
// z.ai-compatible endpoint, a regional facade, a debugging proxy — and it
// applies to a tenant-provisioned key exactly as it does to one read from the
// process env. Unset, the vendor default stands.
func zaiEnv(key string) map[string]string {
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		baseURL = secrets.ZAIDefaultBaseURL
	}
	return facadeRouteEnv(secrets.ProviderZAI, baseURL, key)
}

// moonshotEnv is the ONE place a Moonshot key becomes CLI env, the twin of
// zaiEnv for the Kimi family.
//
// The override knob is MOONSHOT_BASE_URL, NOT ANTHROPIC_BASE_URL, and that
// asymmetry with zaiEnv is deliberate: ANTHROPIC_BASE_URL is already z.ai's
// own documented wiring knob, so on a host configured for z.ai it holds
// z.ai's endpoint — honouring it here would ship the Moonshot key to z.ai's
// gateway, which is the "different provider, different bill" failure naming
// the provider exists to prevent. A dedicated variable keeps the route as
// explicit as the credential.
func moonshotEnv(key string) map[string]string {
	baseURL := os.Getenv("MOONSHOT_BASE_URL")
	if baseURL == "" {
		baseURL = secrets.MoonshotDefaultBaseURL
	}
	return facadeRouteEnv(secrets.ProviderMoonshot, baseURL, key)
}

// facadeRouteEnv is the env of a FUNDED facade route: the vendor's endpoint,
// its key, the slot stamp — and every Anthropic channel the spawned CLI could
// otherwise inherit, cleared.
//
// Setting the facade's two variables is not enough on its own. The spawn
// inherits the process env (host) or the container env (sandbox), and the
// CLI's documented auth precedence puts the cloud-provider switches
// (CLAUDE_CODE_USE_BEDROCK / _VERTEX / _FOUNDRY) ABOVE ANTHROPIC_AUTH_TOKEN: an
// ambient switch sends a node pinned to a facade to that cloud account
// instead, while its fingerprint still names the facade. ANTHROPIC_API_KEY and
// CLAUDE_CODE_OAUTH_TOKEN rank below the facade token, but nothing makes the
// CLI withhold them from the request it sends to the facade's gateway — a
// host carrying an Anthropic key or forfait would hand it to another vendor.
// The refusal (suppressAnthropicWireEnv) clears the same channels, from the
// same function.
func facadeRouteEnv(slot secrets.Provider, baseURL, key string) map[string]string {
	env := clearedAnthropicChannels()
	env["ANTHROPIC_BASE_URL"] = baseURL
	env["ANTHROPIC_AUTH_TOKEN"] = key
	env[FacadeSlotEnvKey] = string(slot)
	return env
}

// clearedAnthropicChannels names the credential channels the CLI resolves on
// its own besides the base-URL/auth-token pair — the Anthropic key, the forfait
// token and the cloud-provider switches — each set to "", which mergeCmdEnv and
// the sandbox exec both read as "unset the inherited value". The pair itself
// is left to each caller: a facade route fills it, the refusal clears it.
func clearedAnthropicChannels() map[string]string {
	env := map[string]string{
		"ANTHROPIC_API_KEY":       "",
		"CLAUDE_CODE_OAUTH_TOKEN": "",
	}
	for _, k := range cloudProviderSwitches {
		env[k] = ""
	}
	return env
}

// cloudProviderSwitches are the variables that boot the CLI into a cloud
// provider's mode. They rank first in its auth precedence, above every token.
var cloudProviderSwitches = []string{
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
}

// ambientAnthropicAuthConfigured reports whether this process' env already
// configures an Anthropic route of its own: a key, a token, or a
// cloud-provider switch. The ZAI_API_KEY shortcut stands down when it does —
// it auto-routes only a host with no Anthropic auth configured.
func ambientAnthropicAuthConfigured() bool {
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return true
	}
	for _, k := range cloudProviderSwitches {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// facadeEnvFor builds the CLI env for one anthropic-wire facade slot, or nil
// when the slot names no facade. One function so a caller that walks
// secrets.AnthropicWireSlotOrder cannot honour a facade at one site and miss
// it at the next.
func facadeEnvFor(slot, key string) map[string]string {
	switch slot {
	case string(secrets.ProviderZAI):
		return zaiEnv(key)
	case string(secrets.ProviderMoonshot):
		return moonshotEnv(key)
	}
	return nil
}

// suppressAnthropicWireEnv is the refusal: the env handed to the CLI when a
// node is pinned to a facade provider and NO key for it is reachable. Every
// channel the CLI could otherwise resolve to reach Anthropic-direct is
// actively suppressed, so downstream surfaces "no <provider> credential"
// instead of silently routing the node to a different provider and a
// different bill. The hostile set:
//   - Anthropic-flavoured env tokens (ANTHROPIC_API_KEY,
//     ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN),
//   - the ANTHROPIC_BASE_URL routing hint,
//   - the forfait FILE channel (CLAUDE_CONFIG_DIR: the CLI reads
//     $CLAUDE_CONFIG_DIR/.credentials.json when no higher-priority env token
//     is set, and on a sandboxed spawn the container's baked value from
//     `exportForfaitConfigDirs` survives unless we override it here — the
//     fifth authoritative auth path in claudeForfaitEnv),
//   - the alt-provider switches CLAUDE_CODE_USE_BEDROCK / _USE_VERTEX /
//     _USE_FOUNDRY (claw-code-go detectProvider checks these BEFORE
//     ANTHROPIC_API_KEY, so an operator with the switch and cloud creds
//     ambient would silently boot the CLI into Bedrock/Vertex mode against
//     their cloud account for a facade-pinned node with no facade key).
//
// CLAUDE_CONFIG_DIR is set to a POISONED absolute path instead of `""`:
// mergeCmdEnv turns an empty value into an absent env var on the spawned
// CLI, which then defaults to $HOME/.claude — on a developer laptop that has
// run `claude login`, that resolves a valid forfait and re-opens the leak. A
// path we know does not exist forces the CLI's read to fail and no fallback
// to authenticate. The other four vars stay `""` (real "clear this inherited
// value") because they carry secrets, not paths.
//
// Clearing only a subset leaves the leak intact: issue #1390 pinned this on
// a wave-4 fallback route that 404'd on the GLM id after silently landing at
// api.anthropic.com. It is one function rather than one map per facade
// provider for the same reason — a copy is where the next provider's set
// goes stale.
func suppressAnthropicWireEnv() map[string]string {
	env := clearedAnthropicChannels()
	env["ANTHROPIC_BASE_URL"] = ""
	env["ANTHROPIC_AUTH_TOKEN"] = ""
	env["CLAUDE_CONFIG_DIR"] = suppressedForfaitDir
	// Marker every iterion-internal reader of CLAUDE_CONFIG_DIR tests
	// before treating it as a real OAuth forfait — providerFingerprint
	// would otherwise render the poisoned dir as `anthropic-oauth` and
	// persist it into the node's output map; SessionFilesRoot would
	// write transcripts under the non-existent path. See R0a39d6.
	env[ForfaitSuppressedEnvKey] = "1"
	return env
}

// normalizeProviderHint folds a routing hint into the form the switches in
// this file are written in: lower-case, no surrounding space.
//
// The hint is operator TEXT and nothing upstream folds it — a bot's
// `provider:` field is trimmed but never cased (ir.SplitProviderStep), a
// launch-time override is stored verbatim, and the compiler's unknown-hint
// diagnostic is a warning. Matching the exact literal therefore let
// `provider: "Moonshot"` miss every facade branch and fall through to the
// DEFAULT precedence: another vendor's account, another bill, and no refusal —
// the failure ErrNoFacadeCredential exists to make loud.
//
// Folded ONCE at the top of the two functions that take a raw hint — this file
// routes with one (anthropicCredEnvForCLI) and refuses with the other
// (facadeHintRefusal) — rather than at each comparison, so a branch added
// later cannot be the one that forgot.
func normalizeProviderHint(hint string) string {
	return strings.ToLower(strings.TrimSpace(hint))
}

// facadeEnvKey names the process-env fallback a facade hint honours when the
// run carries no BYOK key for it, or "" when the slot has none. Its argument
// is a NORMALIZED hint (normalizeProviderHint) or a slot name read from
// secrets.AnthropicWireSlotOrder.
//
// ZAI_API_KEY is z.ai's own variable and nothing else reads it, so an
// ambient value means "route me to z.ai". MOONSHOT_API_KEY is NOT symmetric:
// it is already the credential channel of `backend: "kimi"` (ADR-065), whose
// CLI resolves it from the host env. An operator who exported it configured
// that CLI, not a reroute of every anthropic-wire node — so it is honoured
// only under an explicit `provider: moonshot` hint, never by the default
// precedence.
func facadeEnvKey(slot string) string {
	switch slot {
	case string(secrets.ProviderZAI):
		return "ZAI_API_KEY"
	case string(secrets.ProviderMoonshot):
		return "MOONSHOT_API_KEY"
	}
	return ""
}

// facadeCredEnvForHint resolves a facade-pinned node: the run's own BYOK key
// first, then the provider's process-env fallback, then the refusal. Shared
// by every facade hint so a new provider cannot land with a weaker refusal
// than its siblings.
func facadeCredEnvForHint(slot string, creds secrets.Credentials, hasCreds bool) map[string]string {
	if hasCreds {
		if k := creds.APIKey(secrets.Provider(slot)); k != "" {
			return facadeEnvFor(slot, k)
		}
		// Then a key a shared tier funded BECAUSE a route pins this
		// provider (secrets.RunBundle.PinnedAPIKeys). Read here and not in
		// the default-precedence walk below: this branch runs only under an
		// explicit `provider:` hint, which is the whole licence such a key
		// carries. After the run's own key, never before it — a tenant's
		// instrument outranks the deployment's.
		if k := creds.PinnedAPIKey(secrets.Provider(slot)); k != "" {
			return facadeEnvFor(slot, k)
		}
	}
	if envKey := facadeEnvKey(slot); envKey != "" {
		if k := os.Getenv(envKey); k != "" {
			return facadeEnvFor(slot, k)
		}
	}
	return suppressAnthropicWireEnv()
}

// ErrNoFacadeCredential is the refusal a node pinned to an anthropic-wire
// facade provider gets when no key for that provider is reachable. It NAMES
// the provider and the variable that would have supplied it, because the
// alternative the CLI produces on its own is "Not logged in" — a message
// that does not say which of the run's credentials was expected, and reads
// identically whether the operator pinned the wrong provider or the key
// simply expired.
type ErrNoFacadeCredential struct {
	Provider string
	EnvVar   string
}

func (e *ErrNoFacadeCredential) Error() string {
	return fmt.Sprintf("delegate: node pinned to provider %q but no %s credential is reachable "+
		"(no BYOK key for this run, no %s in the environment) — refusing rather than routing to Anthropic, "+
		"which is a different account and a different bill",
		e.Provider, e.Provider, e.EnvVar)
}

// facadeHintRefusal turns a facade-pinned node whose resolved env is the
// suppression map into that named refusal, or nil when the node is funded (or
// is not facade-pinned at all).
//
// It reads the ENV the CLI would actually receive rather than re-running the
// resolution: a second copy of "is there a key?" is free to disagree with the
// one that routes, and the disagreement would be invisible — the node would
// either run suppressed (opaque 401) or be refused while funded.
func facadeHintRefusal(providerHint string, env map[string]string) error {
	providerHint = normalizeProviderHint(providerHint)
	envVar := facadeEnvKey(providerHint)
	if envVar == "" || !isForfaitSuppressed(env) {
		return nil
	}
	return &ErrNoFacadeCredential{Provider: providerHint, EnvVar: envVar}
}

// AnthropicWireFacadeSlot maps a usage Reading.Source label back onto the
// credential slot that paid for it, or "" when the label names no facade.
//
// The runner's meter needs this because every facade renders as
// "facade:…": a reading charged to the wrong vendor parks the healthy key and
// keeps handing out the walled one. The slot is READ from the label, not
// re-derived from its base URL — the URL is operator-controlled and two
// facades can carry the same one, so re-deriving at read time answers with
// whichever case was compared first (FacadeSlotEnvKey carries the measurement
// that made this concrete). The stamp is written by the code that built the
// env, so an operator's base-URL override travels with it and changes nothing
// here.
//
// A facade label with no slot — an ambient base URL forwarded into a
// sandboxed spawn, a label written by a binary that predates the stamp — names
// no credential, and a guess is the mis-charge itself: it answers "".
func AnthropicWireFacadeSlot(source string) string {
	switch source {
	case PiUsageSourceZAI:
		return string(secrets.ProviderZAI)
	case PiUsageSourceMoonshot:
		return string(secrets.ProviderMoonshot)
	}
	slot, _, _ := splitFacadeLabel(source)
	return slot
}

// splitFacadeLabel parses a facade routing label into the slot that built it
// and the rendered base URL: "facade:<slot>:<base>" → (slot, base, true), the
// slot-less "facade:<base>" → ("", base, true), anything else → ok=false. The
// one parser for providerFingerprint's facade shape, so the meter and the
// session-fork guard cannot read it two ways.
func splitFacadeLabel(label string) (slot, base string, ok bool) {
	rest, ok := strings.CutPrefix(label, facadeSourcePrefix)
	if !ok {
		return "", "", false
	}
	// facadeEnvFor is the one place that knows which slots are facades:
	// asking it beats a third list of the same two names. A base URL's
	// scheme ("https") is not one, which is what keeps the slot-less form
	// from reading as a slot.
	if head, tail, cut := strings.Cut(rest, ":"); cut && facadeEnvFor(head, "") != nil {
		return head, tail, true
	}
	return "", rest, true
}

// UsageMeterBackendForProvider names the meter backend a provider's refusals
// are recorded under, "" for one that carries no metered evidence.
// Anthropic-wire keys (the direct one and the facades) are spent by
// claude_code sessions, so that is where the runner meters them.
//
// One function, read by the launch walk and by the credential view alike:
// when they disagreed, the view reported "never refused" for a credential the
// walk was actively skipping.
func UsageMeterBackendForProvider(prov secrets.Provider) string {
	if secrets.WireFamily(string(prov)) == secrets.WireFamilyAnthropic {
		return BackendClaudeCode
	}
	return ""
}

// anthropicCredEnvForCLI resolves the credential environment for a CLI.
// Task-aware spawns use anthropicCredEnvForTask to compose provisioning too.
//
// providerHint, when non-empty, overrides the default precedence with
// a per-node routing decision (from the DSL `provider:` field):
//   - "anthropic" — force Anthropic-direct (API key or OAuth dir),
//     skip z.ai even if ZAI_API_KEY is set on the process. Use when a
//     specific node needs Anthropic's full context window (1M on
//     Claude Opus 4.7) instead of the smaller z.ai window.
//   - "zai" — force z.ai routing (Anthropic-shaped facade backed by
//     GLM-4.6) even if Anthropic credentials are present. Use to pin
//     a node to GLM regardless of process-env precedence.
//   - "moonshot" — force Moonshot routing (Anthropic-shaped facade
//     backed by the Kimi family), same contract as "zai".
//   - "" / "auto" — current process-env-driven precedence (below).
//
// The hint is matched folded (normalizeProviderHint): it is operator text, and
// which account pays must not depend on its capitalisation.
//
// A facade hint with no key reachable REFUSES rather than degrades: every
// Anthropic-flavoured channel is actively suppressed so the node surfaces
// "no <provider> credential" instead of quietly spending a different
// account (see suppressAnthropicWireEnv).
//
// Default precedence (first match wins, returned options are mutually
// exclusive — never set both ANTHROPIC_API_KEY and CLAUDE_CONFIG_DIR). The
// order is secrets.AnthropicWireSlotOrder, shared with the usage meter and
// the spend ledger:
//
//  1. Per-run BYOK z.ai key: ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN
//     (z.ai's Coding-Plan token routes through Anthropic-shaped wire to
//     z.ai's gateway, which aliases the model to GLM-4.5/4.6 internally).
//  2. Per-run BYOK Moonshot key: same shape, pointed at Moonshot's
//     Anthropic-compatible endpoint for the Kimi family.
//  3. Per-run BYOK Anthropic key: ANTHROPIC_API_KEY.
//  4. Per-run OAuth-forfait credentials.json (desktop): CLAUDE_CONFIG_DIR.
//     NB: on the cloud the same kind is scheduled for removal under
//     Anthropic Consumer Terms — see .plans/zai-glm-oauth.md.
//  5. Process-env fallback ZAI_API_KEY: same shape as case 1, lets
//     desktop users put `ZAI_API_KEY=...` in ~/.iterion/env without
//     also having to set ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN by
//     hand. ANTHROPIC_API_KEY in env (if present) takes precedence
//     via the CLI's own resolution; we don't set anything in that
//     case so the inherited env wins. MOONSHOT_API_KEY has no such
//     step — facadeEnvKey says why.
func anthropicCredEnvForCLI(ctx context.Context, providerHint string, sandboxed bool) map[string]string {
	selected, inheritAmbient := selectedAnthropicCredEnvForCLI(ctx, providerHint, sandboxed)
	if sandboxed && inheritAmbient {
		env := ambientAnthropicEnvForSandbox()
		if env == nil && len(selected) > 0 {
			env = make(map[string]string, len(selected))
		}
		for key, value := range selected {
			env[key] = value
		}
		return env
	}
	return selected
}

// anthropicCredEnvForTask composes both CLI passes with the same precedence:
// ambient forwarding < task additions (including empty values) < the resolved
// credential route. The last layer includes suppression entries: an extra
// variable must not redirect a selected facade's bearer or revive its forfait.
func anthropicCredEnvForTask(ctx context.Context, task Task) map[string]string {
	selected, inheritAmbient := selectedAnthropicCredEnvForCLI(ctx, task.ProviderHint, taskSandboxed(task))
	env := map[string]string{}
	if inheritAmbient && taskSandboxed(task) {
		for key, value := range ambientAnthropicEnvForSandbox() {
			env[key] = value
		}
	}
	for _, kv := range task.ExtraEnv {
		if key, value, ok := strings.Cut(kv, "="); ok && key != "" {
			env[key] = value
		}
	}
	// These labels belong to the resolver, never to process provisioning.
	// Otherwise an extra env entry can forge a usage-meter slot or turn a
	// funded route into the "no credential" suppression sentinel.
	env[FacadeSlotEnvKey] = ""
	env[ForfaitSuppressedEnvKey] = ""
	if selected[FacadeSlotEnvKey] == "" && (selected["ANTHROPIC_API_KEY"] != "" || selected["CLAUDE_CONFIG_DIR"] != "") {
		// A bound direct credential owns its route as a whole. Applying just
		// its key after ExtraEnv could restore a context key at an unrelated
		// endpoint, or let a cloud switch spend a different account instead.
		for key, value := range clearedAnthropicChannels() {
			env[key] = value
		}
		env["ANTHROPIC_BASE_URL"] = ""
		env["ANTHROPIC_AUTH_TOKEN"] = ""
	}
	if normalizeProviderHint(task.ProviderHint) == "anthropic" {
		// The explicit direct hint also rules out cloud-provider modes when
		// auth itself is inherited rather than bound from the run context.
		for _, key := range cloudProviderSwitches {
			env[key] = ""
		}
	}
	for key, value := range selected {
		env[key] = value
	}
	return env
}

// anthropicFingerprintEnvForTask accounts for a host-inherited endpoint without
// changing the spawn environment. A sandbox already has explicit forwarding.
// Preserve an explicit empty override and the historical cloud-mode labels:
// a cloud switch makes an inherited Anthropic endpoint irrelevant to routing.
func anthropicFingerprintEnvForTask(task Task, env map[string]string) map[string]string {
	if taskSandboxed(task) {
		return env
	}
	if _, explicit := env["ANTHROPIC_BASE_URL"]; explicit {
		return env
	}
	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		return env
	}
	for _, key := range cloudProviderSwitches {
		value, explicit := env[key]
		if !explicit {
			value = os.Getenv(key)
		}
		if value != "" {
			return env
		}
	}
	out := make(map[string]string, len(env)+1)
	for key, value := range env {
		out[key] = value
	}
	out["ANTHROPIC_BASE_URL"] = base
	return out
}

// selectedAnthropicCredEnvForCLI returns authoritative route/credential
// overrides and whether auth is ambient. An explicit direct hint can clear
// stale routing fields while still inheriting credentials; those credentials
// must be composed BEFORE task additions, not promoted to selected overrides.
func selectedAnthropicCredEnvForCLI(ctx context.Context, providerHint string, sandboxed bool) (env map[string]string, inheritAmbient bool) {
	providerHint = normalizeProviderHint(providerHint)
	creds, hasCreds := secrets.CredentialsFromContext(ctx)

	// providerHint=="anthropic": force Anthropic-direct. Skip the z.ai
	// branches entirely, even if ZAI_API_KEY is in the process env.
	if providerHint == "anthropic" {
		if hasCreds {
			if k := creds.APIKey(secrets.ProviderAnthropic); k != "" {
				return map[string]string{"ANTHROPIC_API_KEY": k}, false
			}
			// Same licence as the facade branch below: an explicit pin may
			// spend a key a shared tier funded for it.
			if k := creds.PinnedAPIKey(secrets.ProviderAnthropic); k != "" {
				return map[string]string{"ANTHROPIC_API_KEY": k}
			}
			if d := creds.OAuthDir(string(secrets.OAuthKindClaudeCode)); d != "" {
				return claudeForfaitEnv(d, sandboxed), false
			}
		}
		// Process-env path: rely on ANTHROPIC_API_KEY inherited by the
		// CLI. Actively clear ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
		// so a stale z.ai value from the parent env doesn't leak in. A
		// sandboxed spawn needs explicit forwarding; the caller composes
		// that ambient layer before any task additions, then these overrides.
		return map[string]string{
			"ANTHROPIC_BASE_URL":   "",
			"ANTHROPIC_AUTH_TOKEN": "",
		}, true
	}

	// A facade hint ("zai", "moonshot") forces that vendor's endpoint: the
	// run's own key first, then the provider's process-env fallback, then a
	// refusal that leaves the CLI no Anthropic channel to fall through to.
	// Each facade goes through the same function so none can land with a
	// weaker refusal than its siblings.
	if facadeEnvKey(providerHint) != "" {
		return facadeCredEnvForHint(providerHint, creds, hasCreds), false
	}

	// Default precedence (providerHint is "" / "auto"), in
	// secrets.AnthropicWireSlotOrder — the one list every reader of this
	// wire shares. Only credentials the RUN carries participate: a facade's
	// process-env fallback answers to its own hint, never here (facadeEnvKey
	// says why MOONSHOT_API_KEY in particular must not reroute an unpinned
	// node).
	if hasCreds {
		for _, slot := range secrets.AnthropicWireSlotOrder {
			switch slot {
			case string(secrets.ProviderAnthropic):
				if k := creds.APIKey(secrets.ProviderAnthropic); k != "" {
					return map[string]string{"ANTHROPIC_API_KEY": k}, false
				}
			case string(secrets.OAuthKindClaudeCode):
				if d := creds.OAuthDir(string(secrets.OAuthKindClaudeCode)); d != "" {
					return claudeForfaitEnv(d, sandboxed), false
				}
			default:
				if k := creds.APIKey(secrets.Provider(slot)); k != "" {
					if env := facadeEnvFor(slot, k); env != nil {
						return env, false
					}
				}
			}
		}
	}
	// Env-fallback: ZAI_API_KEY is the convenience knob for desktop
	// users. Only honoured when no Anthropic route is already configured
	// by env — ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN or a cloud-provider
	// switch from the inherited env stays authoritative. The switches count
	// because zaiEnv clears them: honouring the shortcut past one would
	// turn a Bedrock/Vertex/Foundry host into a z.ai one.
	if !ambientAnthropicAuthConfigured() {
		if zai := os.Getenv("ZAI_API_KEY"); zai != "" {
			return zaiEnv(zai), false
		}
	}
	// The caller composes ambient inheritance/forwarding with task additions.
	return nil, true
}
