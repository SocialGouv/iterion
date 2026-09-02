package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// anthropicCredOptsForCLI returns claudesdk.WithEnv options that point
// the spawned Claude Code subprocess at the right credentials.
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
//   - "" / "auto" — current process-env-driven precedence (below).
//
// Default precedence (first match wins, returned options are mutually
// exclusive — never set both ANTHROPIC_API_KEY and CLAUDE_CONFIG_DIR):
//
//  1. Per-run BYOK z.ai key: ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN
//     (z.ai's Coding-Plan token routes through Anthropic-shaped wire to
//     z.ai's gateway, which aliases the model to GLM-4.5/4.6 internally).
//  2. Per-run BYOK Anthropic key: ANTHROPIC_API_KEY.
//  3. Per-run OAuth-forfait credentials.json (desktop): CLAUDE_CONFIG_DIR.
//     NB: on the cloud the same kind is scheduled for removal under
//     Anthropic Consumer Terms — see .plans/zai-glm-oauth.md.
//  4. Process-env fallback ZAI_API_KEY: same shape as case 1, lets
//     desktop users put `ZAI_API_KEY=...` in ~/.iterion/env without
//     also having to set ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN by
//     hand. ANTHROPIC_API_KEY in env (if present) takes precedence
//     via the CLI's own resolution; we don't set anything in that
//     case so the inherited env wins.
func anthropicCredOptsForCLI(ctx context.Context, providerHint string, sandboxed bool) []claudesdk.Option {
	return credEnvToOpts(anthropicCredEnvForCLI(ctx, providerHint, sandboxed))
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
//   - parent fingerprint set AND differs from current → drop.
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
	if currentFingerprint != "" && task.SessionFingerprint != currentFingerprint {
		return true, fmt.Sprintf("parent session was built on %q but current provider is %q (signed thinking blocks would 400 on cross-provider reuse)",
			task.SessionFingerprint, currentFingerprint)
	}
	return false, ""
}

// providerFingerprint derives a stable identifier for the routing
// decision encoded by a cred env map. Two calls to anthropicCredEnvForCLI
// with the same provider precedence return the same fingerprint, so
// sessions produced under one provider can be detected (and dropped)
// when a later run targets a different one. Key values are NOT
// included — fingerprints are safe to log and to ferry through the
// recipe output map.
func providerFingerprint(env map[string]string) string {
	if env == nil {
		return "anthropic-env"
	}
	if base := env["ANTHROPIC_BASE_URL"]; base != "" {
		return "facade:" + base
	}
	if env["ANTHROPIC_API_KEY"] != "" {
		return "anthropic-direct"
	}
	if env["CLAUDE_CONFIG_DIR"] != "" {
		return "anthropic-oauth"
	}
	// Explicit zeroing of BASE_URL/AUTH_TOKEN (the providerHint==anthropic
	// path) lands here too — it means "use the inherited ANTHROPIC_API_KEY
	// from the process env", which is also Anthropic-direct semantically.
	return "anthropic-env"
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
	// token deterministically win. Best-effort: on any read/parse failure we
	// fall back to the file path alone (prior behaviour).
	if tok := readForfaitAccessToken(dir); tok != "" {
		env["CLAUDE_CODE_OAUTH_TOKEN"] = tok
	}
	return env
}

// readForfaitAccessToken extracts claudeAiOauth.accessToken from the
// materialised Claude Code credentials.json in dir. Returns "" (never an error)
// when the file is absent or malformed — the caller degrades to the file path.
func readForfaitAccessToken(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return ""
	}
	var v struct {
		ClaudeAIOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	return v.ClaudeAIOauth.AccessToken
}

// sandboxed reports that the CLI subprocess will execute inside a REAL
// sandbox container (docker/kubernetes — not the host-passthrough noop), so
// forfait credential paths must resolve to in-container locations.
func anthropicCredEnvForCLI(ctx context.Context, providerHint string, sandboxed bool) map[string]string {
	creds, hasCreds := secrets.CredentialsFromContext(ctx)

	// providerHint=="anthropic": force Anthropic-direct. Skip the z.ai
	// branches entirely, even if ZAI_API_KEY is in the process env.
	if providerHint == "anthropic" {
		if hasCreds {
			if k := creds.APIKey(secrets.ProviderAnthropic); k != "" {
				return map[string]string{"ANTHROPIC_API_KEY": k}
			}
			if d := creds.OAuthDir(string(secrets.OAuthKindClaudeCode)); d != "" {
				return claudeForfaitEnv(d, sandboxed)
			}
		}
		// Process-env path: rely on ANTHROPIC_API_KEY inherited by the
		// CLI. Actively clear ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
		// so a stale z.ai value from the parent env doesn't leak in. A
		// sandboxed spawn inherits the CONTAINER env, not this process'
		// — forward the ambient Anthropic-direct credentials explicitly
		// (keeping the z.ai suppression: the hint forces direct).
		env := map[string]string{
			"ANTHROPIC_BASE_URL":   "",
			"ANTHROPIC_AUTH_TOKEN": "",
		}
		if sandboxed {
			for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
				if v := os.Getenv(k); v != "" {
					env[k] = v
				}
			}
		}
		return env
	}

	// providerHint=="zai": force the z.ai facade. Prefer in-context
	// creds; fall back to ZAI_API_KEY in the process env.
	if providerHint == "zai" {
		if hasCreds {
			if k := creds.APIKey(secrets.ProviderZAI); k != "" {
				return map[string]string{
					"ANTHROPIC_BASE_URL":   secrets.ZAIDefaultBaseURL,
					"ANTHROPIC_AUTH_TOKEN": k,
				}
			}
		}
		if zai := os.Getenv("ZAI_API_KEY"); zai != "" {
			baseURL := os.Getenv("ANTHROPIC_BASE_URL")
			if baseURL == "" {
				baseURL = secrets.ZAIDefaultBaseURL
			}
			return map[string]string{
				"ANTHROPIC_BASE_URL":   baseURL,
				"ANTHROPIC_AUTH_TOKEN": zai,
			}
		}
		// No z.ai key reachable — clear hostile env and let downstream
		// surface the "no credential" error rather than silently
		// falling back to a different provider.
		return map[string]string{
			"ANTHROPIC_BASE_URL":   "",
			"ANTHROPIC_AUTH_TOKEN": "",
		}
	}

	// Default precedence (providerHint is "" / "auto").
	if hasCreds {
		switch {
		case creds.APIKey(secrets.ProviderZAI) != "":
			return map[string]string{
				"ANTHROPIC_BASE_URL":   secrets.ZAIDefaultBaseURL,
				"ANTHROPIC_AUTH_TOKEN": creds.APIKey(secrets.ProviderZAI),
			}
		case creds.APIKey(secrets.ProviderAnthropic) != "":
			return map[string]string{"ANTHROPIC_API_KEY": creds.APIKey(secrets.ProviderAnthropic)}
		case creds.OAuthDir(string(secrets.OAuthKindClaudeCode)) != "":
			return claudeForfaitEnv(creds.OAuthDir(string(secrets.OAuthKindClaudeCode)), sandboxed)
		}
	}
	// Env-fallback: ZAI_API_KEY is the convenience knob for desktop
	// users. Only honoured when no Anthropic-flavoured creds are
	// already wired by env — ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN
	// from the inherited env stays authoritative.
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		if zai := os.Getenv("ZAI_API_KEY"); zai != "" {
			baseURL := os.Getenv("ANTHROPIC_BASE_URL")
			if baseURL == "" {
				baseURL = secrets.ZAIDefaultBaseURL
			}
			return map[string]string{
				"ANTHROPIC_BASE_URL":   baseURL,
				"ANTHROPIC_AUTH_TOKEN": zai,
			}
		}
	}
	// Host path: nil = let the spawned CLI inherit whatever ambient
	// Anthropic env this process carries (os.Environ passthrough). A
	// sandboxed spawn has no such inheritance — forward the ambient
	// credentials explicitly so a runner-pod-level forfait
	// (CLAUDE_CODE_OAUTH_TOKEN) or API key reaches the in-container CLI
	// exactly as it would a host spawn.
	if sandboxed {
		return ambientAnthropicEnvForSandbox()
	}
	return nil
}
