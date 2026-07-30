// Package detect probes the host environment for available LLM credentials
// and CLI binaries, producing a Report consumed by the studio (UI hints)
// and the runtime resolver (auto backend selection).
package detect

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/clilocate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Backend names. Values must stay in sync with the delegate.Backend*
// constants — kept as plain strings here so the dsl/ir compiler can
// import detect without pulling in delegate (and its transitive deps).
const (
	BackendClaudeCode = "claude_code"
	BackendCodex      = "codex"
	BackendClaw       = "claw"
	BackendPi         = "pi"
)

// Auth kinds.
const (
	AuthOAuth  = "oauth"
	AuthAPIKey = "api_key"
	AuthNone   = "none"
)

// Report is the snapshot returned by Detect. The shape is mirrored in
// studio/src/api/backends.ts.
type Report struct {
	// PreferenceOrder is the effective order used when resolving "auto".
	// Default ["claude_code", "claw"]; codex is intentionally absent so it
	// is never auto-selected (matches the C030 discouraged stance).
	PreferenceOrder []string `json:"preference_order"`
	// ResolvedDefault is the first available backend in PreferenceOrder,
	// or "" when none are available.
	ResolvedDefault string `json:"resolved_default"`
	// Backends lists status for the three delegate backends.
	Backends []BackendStatus `json:"backends"`
	// Providers lists claw provider availability (anthropic, openai, …).
	Providers []ProviderStatus `json:"providers"`
}

// BackendStatus describes a single delegate backend.
type BackendStatus struct {
	Name      string   `json:"name"`
	Available bool     `json:"available"`
	Auth      string   `json:"auth"`
	Sources   []string `json:"sources"`
	Hints     []string `json:"hints,omitempty"`
}

// ProviderStatus describes a claw-driven provider.
type ProviderStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	// Source is the credential that will actually be used at runtime.
	// For providers with a single auth path, this is "ENV_VAR_NAME". For
	// providers that admit multiple paths (e.g. OpenAI: API key + ChatGPT
	// OAuth), it is the winner; the others are surfaced via
	// OverriddenSources so the UI can render them struck-through.
	Source         string `json:"source"`
	SuggestedModel string `json:"suggested_model,omitempty"`
	// OverriddenSources lists detected credentials for this provider
	// that are present but will NOT be used because Source takes
	// precedence. Each entry is a free-form human-readable label
	// (e.g. "OPENAI_API_KEY (overridden by ChatGPT-OAuth)").
	OverriddenSources []string `json:"overridden_sources,omitempty"`
}

// DefaultPreferenceOrder is the compiled-in fallback order.
// codex is intentionally absent — explicit opt-in only.
var DefaultPreferenceOrder = []string{BackendClaudeCode, BackendClaw}

// Detect runs the probes synchronously. ctx is honored for timed probes:
// most are local stat / env reads, but the claude_code fallback may shell
// out to `claude auth status --json` (3 s timeout) when no credentials file
// is present. Results are cached by CachedDetector, so the subprocess runs
// at most once per TTL window.
func Detect(ctx context.Context) Report {
	prov := detectProviders()
	backends := detectBackends(ctx, prov)
	pref := PreferenceFromEnv()
	return Report{
		PreferenceOrder: pref,
		ResolvedDefault: Resolve(pref, backends),
		Backends:        backends,
		Providers:       prov,
	}
}

// PreferenceFromEnv parses ITERION_BACKEND_PREFERENCE (CSV).
// Returns DefaultPreferenceOrder when the env var is unset / empty.
func PreferenceFromEnv() []string {
	raw := os.Getenv("ITERION_BACKEND_PREFERENCE")
	if raw == "" {
		out := make([]string, len(DefaultPreferenceOrder))
		copy(out, DefaultPreferenceOrder)
		return out
	}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func detectBackends(ctx context.Context, prov []ProviderStatus) []BackendStatus {
	return []BackendStatus{
		detectClaudeCode(ctx),
		detectClaw(prov),
		detectOAuthCLI(
			BackendCodex, findCodexBinary, codexHomeDir,
			[]string{"auth.json"},
			nil, // codex OAuth lives only in auth.json (no OS-credential-store path)
			"codex CLI",
			"$CODEX_HOME/auth.json (Codex OAuth)",
		),
		detectPi(prov),
	}
}

// detectPi probes for the pi backend.
//
// pi is reported but never auto-SELECTED: DefaultPreferenceOrder stays
// {claude_code, claw}, so surfacing it here changes no existing workflow. What
// it does change is that the studio can show it and that
// ITERION_BACKEND_PREFERENCE=pi resolves at all — auto-selection filters on
// what this function reports, so without an entry the variable was inert.
//
// Availability is deliberately broad, because pi's whole proposition is "you
// already hold a credential for one of ~36 providers": its own login store,
// any provider env key iterion already probes for claw (pi reads the same
// variables), or a ChatGPT/Codex login, which iterion bridges into pi's
// OAuth-only openai-codex provider.
func detectPi(prov []ProviderStatus) BackendStatus {
	st := BackendStatus{Name: BackendPi, Auth: AuthNone}

	if _, ok := findPiBinary(); !ok {
		st.Hints = []string{"pi CLI not found on PATH"}
		return st
	}

	var sources []string
	if kind := piOwnLoginKind(); kind != AuthNone {
		sources = append(sources, "~/.pi/agent/auth.json (pi login)")
		st.Auth = kind
	}
	for _, p := range prov {
		if !p.Available || !piReadsProvider(p.Name) || !piEnvVarSource(p.Source) {
			continue
		}
		sources = append(sources, p.Source)
		if st.Auth == AuthNone {
			st.Auth = AuthAPIKey
		}
	}
	if codexChatGPTAvailable() {
		sources = append(sources, "$CODEX_HOME/auth.json (ChatGPT-Codex OAuth)")
		if st.Auth == AuthNone {
			st.Auth = AuthOAuth
		}
	}

	if len(sources) == 0 {
		st.Hints = []string{
			"pi CLI found, but no credential: run `pi` then /login, or set a provider key",
		}
		return st
	}
	st.Available = true
	st.Sources = sources
	return st
}

// piEnvVarSource reports whether a provider's winning credential is an
// environment variable — the only shape pi resolves a provider from.
//
// The openai provider is also marked available on the Codex ChatGPT OAuth
// alone, labelled with a file path rather than a variable name. Counting that
// listed the same credential twice (codexChatGPTAvailable reports it properly,
// as oauth) and left the row reading `api_key`, while implying pi could resolve
// its `openai` provider from a file only the openai-codex bridge can use.
func piEnvVarSource(source string) bool {
	if source == "" {
		return false
	}
	for _, r := range source {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// piReadsProvider reports whether an available provider is one pi can actually
// resolve a credential for.
//
// bedrock and vertex are marked available on `AWS_REGION` / `GOOGLE_CLOUD_PROJECT`
// alone — variables an AWS host or CI runner sets by default — and pi reads
// neither AWS nor GCP application-default credentials; they are absent from the
// env-key set the pi backend forwards. Counting them would report pi available
// on a host where every call fails, and because auto-selection filters on this
// report, `ITERION_BACKEND_PREFERENCE=pi` would then resolve there.
func piReadsProvider(name string) bool {
	switch name {
	case "bedrock", "vertex":
		return false
	default:
		return true
	}
}

// piOwnLoginKind reports what pi's own credential store holds: AuthOAuth,
// AuthAPIKey, or AuthNone when there is nothing usable.
//
// A fresh install writes `{}`, so existence is not enough — reporting pi as
// available on an empty store would put a backend in the studio that fails on
// its first call. The entry's own `type` decides the kind: pi's store holds
// both shapes, and labelling an api_key credential "oauth" mislabels the
// studio's auth badge for the one backend whose selling point is credential
// breadth.
func piOwnLoginKind() string {
	// Match execution's precedence: piResolveEnv pins PI_CODING_AGENT_DIR from
	// ITERION_PI_AGENT_DIR, and piCodexSeed treats the latter as an operator
	// pin — so probing only the former reads a store the node will never open,
	// and would report pi available on a credential it cannot reach. Same
	// reasoning as findPiBinary and codexChatGPTAvailable: a probe must not be
	// more optimistic than what will actually use the credential.
	dir := strings.TrimSpace(os.Getenv("ITERION_PI_AGENT_DIR"))
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR"))
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return AuthNone
		}
		dir = filepath.Join(home, ".pi", "agent")
	}
	data, err := os.ReadFile(filepath.Join(dir, "auth.json")) // #nosec G304 — a fixed filename under the agent dir.
	if err != nil {
		return AuthNone
	}
	var creds map[string]struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return AuthNone
	}
	kind := AuthNone
	for _, c := range creds {
		if c.Type == "oauth" {
			// An OAuth login is the more notable of the two; report it as soon
			// as one is present.
			return AuthOAuth
		}
		kind = AuthAPIKey
	}
	return kind
}

// codexChatGPTAvailable reports whether a ChatGPT-mode Codex login exists.
//
// It defers to the SAME authority the pi bridge consumes —
// CodexCredentialsView.IsChatGPTMode, which requires an access token and an
// account id, not merely `auth_mode: "chatgpt"`. A hand-rolled check on the
// mode alone was weaker than the consumer's gate, so a partially-written or
// logged-out Codex state made detect report pi available while the bridge
// stepped aside, and the node then died with "No API key found for
// openai-codex". That is the same false-positive class piReadsProvider argues
// against just above; a probe must not be more optimistic than what will
// actually use the credential.
func codexChatGPTAvailable() bool {
	dir := codexHomeDir()
	if dir == "" {
		return false
	}
	view, err := secrets.LoadCodexCredentialsFrom(dir)
	return err == nil && view.IsChatGPTMode()
}

// detectClaudeCode probes for the claude_code backend in three escalating
// steps, each cheaper than the next is general:
//  1. credentials file (.credentials.json / credentials.json in ~/.claude) —
//     the Linux/WSL hidden form and legacy macOS export;
//  2. macOS Keychain (darwin only, ~10ms existence check) — where Claude
//     Code 2.x stores OAuth with no file written (see claudeKeychainOAuthSource);
//  3. `claude auth status --json` — the cross-platform fallback that asks the
//     CLI itself, covering any other store / future platform where the first
//     two find nothing but the user is in fact logged in (loggedIn=true).
func detectClaudeCode(ctx context.Context) BackendStatus {
	// Steps 1-2: credentials file, then the macOS Keychain fast-path. Both
	// are handled inside detectOAuthCLI via the keychain extraOAuth probe.
	st := detectOAuthCLI(
		BackendClaudeCode, findClaudeBinary, claudeConfigDir,
		[]string{".credentials.json", "credentials.json"},
		// Modern Claude Code (2.x) on macOS stores OAuth in the Keychain
		// instead of a file — darwin-only, never reads the token.
		claudeKeychainOAuthSource,
		"claude CLI",
		"~/.claude/.credentials.json (Claude Code OAuth)",
	)
	if st.Available {
		return st
	}

	// Binary not found — nothing more to try.
	binPath, hasBin := findClaudeBinary()
	if !hasBin {
		return st
	}

	// Step 3: `claude auth status --json`. Cross-platform truth from the CLI;
	// covers claude.ai web-auth on hosts where no file/keychain entry surfaced.
	if claudeAuthStatusFn(ctx, binPath) {
		return BackendStatus{
			Name:      BackendClaudeCode,
			Available: true,
			Auth:      AuthOAuth,
			Sources:   []string{"claude auth status (claude.ai OAuth)", "PATH:" + binPath},
		}
	}
	return st
}

// claudeAuthStatusFn invokes `claude auth status --json` and reports whether
// the user is logged in via claude.ai OAuth (the "forfait" web-auth).
//
// It deliberately requires authMethod == "claude.ai" and does NOT accept
// authMethod == "api_key": an ANTHROPIC_API_KEY makes `claude auth status`
// report loggedIn=true, but the API-key path is intentionally not
// OAuth-eligible for auto-resolution — claw uses the same key in-process and
// is preferred (see detectOAuthCLI). Without this guard, any host with
// ANTHROPIC_API_KEY set would wrongly resolve to claude_code.
//
// Declared as a var so tests can stub it without requiring a real claude
// binary.
var claudeAuthStatusFn = func(ctx context.Context, binPath string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "auth", "status", "--json").Output()
	if err != nil {
		return false
	}
	return claudeAuthStatusIsForfait(out)
}

// claudeAuthStatusIsForfait parses `claude auth status --json` output and
// reports whether it represents a claude.ai OAuth ("forfait") login. Split out
// as a pure function so the api_key-exclusion logic is unit-testable without a
// real claude binary (claudeAuthStatusFn itself is stubbed in tests).
func claudeAuthStatusIsForfait(out []byte) bool {
	var result struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &result) != nil {
		return false
	}
	// Forfait OAuth only — exclude api_key / none.
	return result.LoggedIn && result.AuthMethod == "claude.ai"
}

// detectOAuthCLI reports Available=true only when both a binary and an
// OAuth credential are present. The credential may live in a file under
// configDir (Linux/WSL `.credentials.json`, exported keychain) or — for
// backends whose CLI stores OAuth in the OS credential store — be surfaced
// via extraOAuth (macOS Keychain for Claude Code). The API-key-only path
// (e.g. ANTHROPIC_API_KEY + claude binary, no OAuth) is intentionally NOT
// auto-eligible: claw uses the same key in-process and is faster, so users
// wanting the CLI in that case must set `backend:` explicitly.
func detectOAuthCLI(
	name string,
	findBin func() (string, bool),
	configDir func() string,
	credFiles []string,
	extraOAuth func() string,
	binDesc, oauthDesc string,
) BackendStatus {
	st := BackendStatus{Name: name, Auth: AuthNone}
	binPath, hasBin := findBin()
	if !hasBin {
		st.Hints = []string{binDesc + " not found in PATH or common locations"}
		return st
	}
	if dir := configDir(); dir != "" {
		for _, credFile := range credFiles {
			credPath := filepath.Join(dir, credFile)
			if fileExists(credPath) {
				st.Available = true
				st.Auth = AuthOAuth
				st.Sources = []string{credPath, "PATH:" + binPath}
				return st
			}
		}
	}
	// OS credential store (macOS Keychain for Claude Code — the default
	// store on darwin, where no .credentials.json file is written). Same
	// OAuth semantics as the file form.
	if extraOAuth != nil {
		if label := extraOAuth(); label != "" {
			st.Available = true
			st.Auth = AuthOAuth
			st.Sources = []string{label, "PATH:" + binPath}
			return st
		}
	}
	st.Hints = []string{
		binDesc + " found at " + binPath,
		"no " + oauthDesc + " — set `backend: " + name + "` explicitly to use API-key auth",
	}
	return st
}

func detectClaw(prov []ProviderStatus) BackendStatus {
	st := BackendStatus{Name: BackendClaw}
	var sources []string
	for _, p := range prov {
		if p.Available {
			sources = append(sources, p.Source)
			// Surface overridden sources too so the UI can render them
			// struck-through (entries containing "(overridden by " are
			// the convention).
			sources = append(sources, p.OverriddenSources...)
		}
	}
	if len(sources) > 0 {
		st.Available = true
		st.Auth = AuthAPIKey
		st.Sources = sources
		return st
	}
	st.Auth = AuthNone
	st.Hints = []string{
		"no ANTHROPIC_API_KEY",
		"no OPENAI_API_KEY",
		"no XAI_API_KEY",
		"no AZURE_OPENAI_API_KEY",
		"no AWS region (Bedrock)",
		"no GOOGLE_CLOUD_PROJECT (Vertex)",
	}
	return st
}

func detectProviders() []ProviderStatus {
	// z.ai exposes an Anthropic-API-compatible endpoint via
	// ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN (or its dedicated
	// ZAI_API_KEY). Treat that combination as a distinct provider so
	// "anthropic" only flags as Available when the user really has
	// Anthropic credentials — otherwise the BYOK selection UI happily
	// claimed anthropic was available when in fact only the z.ai
	// façade was configured, and the resolver would pick anthropic
	// models that 401'd against z.ai.
	anthropicBase := strings.ToLower(os.Getenv("ANTHROPIC_BASE_URL"))
	hasZAIBaseURL := strings.Contains(anthropicBase, "z.ai") ||
		strings.Contains(anthropicBase, "bigmodel")
	hasZAIKey := os.Getenv("ZAI_API_KEY") != ""
	zaiAvailable := hasZAIKey ||
		(hasZAIBaseURL && os.Getenv("ANTHROPIC_AUTH_TOKEN") != "")
	anthropicAvailable := os.Getenv("ANTHROPIC_API_KEY") != "" && !hasZAIBaseURL

	out := []ProviderStatus{
		{
			Name:           "anthropic",
			Available:      anthropicAvailable,
			Source:         envSource("ANTHROPIC_API_KEY"),
			SuggestedModel: "anthropic/claude-opus-5",
		},
		{
			Name:           "zai",
			Available:      zaiAvailable,
			Source:         envSource("ZAI_API_KEY", "ANTHROPIC_BASE_URL"),
			SuggestedModel: "anthropic/glm-5.2",
		},
		detectOpenAIProvider(),
		{
			// xAI Grok — claw drives it through the OpenAI-compatible
			// client (registry providers["xai"]). Detection is a pure
			// env probe: XAI_API_KEY. Suggested model matches claw's
			// built-in registry alias (`grok` → grok-3).
			Name:           "xai",
			Available:      os.Getenv("XAI_API_KEY") != "",
			Source:         envSource("XAI_API_KEY"),
			SuggestedModel: "xai/grok-3",
		},
		{
			Name:           "foundry",
			Available:      os.Getenv("AZURE_OPENAI_API_KEY") != "" && os.Getenv("AZURE_OPENAI_ENDPOINT") != "",
			Source:         envSource("AZURE_OPENAI_API_KEY"),
			SuggestedModel: "",
		},
		{
			Name:           "bedrock",
			Available:      os.Getenv("AWS_REGION") != "" || os.Getenv("AWS_DEFAULT_REGION") != "",
			Source:         envSource("AWS_REGION", "AWS_DEFAULT_REGION"),
			SuggestedModel: "",
		},
		{
			Name:           "vertex",
			Available:      os.Getenv("GOOGLE_CLOUD_PROJECT") != "",
			Source:         envSource("GOOGLE_CLOUD_PROJECT"),
			SuggestedModel: "",
		},
	}
	return out
}

// detectOpenAIProvider reports the OpenAI provider availability across both
// auth paths: `OPENAI_API_KEY` (metered API), and ChatGPT-forfait OAuth
// sourced from Codex CLI's auth.json. The active path mirrors registry.go:
// OPENAI_API_KEY wins by default; ChatGPT-OAuth is used only when no API
// key is set, or when ITERION_OPENAI_USE_OAUTH=1 forces it. OAuth is
// disabled entirely when ITERION_OPENAI_USE_OAUTH=0 or OPENAI_BASE_URL is
// set (we never masquerade codex_cli_rs headers to a third-party endpoint).
//
// The non-active source is surfaced via OverriddenSources so the studio
// can render it struck-through with a "overridden by ..." annotation.
func detectOpenAIProvider() ProviderStatus {
	const (
		labelAPIKey = "OPENAI_API_KEY"
		labelOAuth  = "ChatGPT-OAuth (~/.codex/auth.json)"
	)
	hasAPIKey := os.Getenv("OPENAI_API_KEY") != ""

	hasOAuth := false
	if view, err := secrets.LoadCodexCredentialsFromDisk(); err == nil && view.IsChatGPTMode() {
		hasOAuth = true
	}

	st := ProviderStatus{
		Name:           "openai",
		SuggestedModel: "openai/gpt-5.4-mini",
	}
	if !hasAPIKey && !hasOAuth {
		return st
	}
	st.Available = true

	// Same precedence as pkg/backend/model/registry.go's openai factory.
	oauthPref := os.Getenv("ITERION_OPENAI_USE_OAUTH")
	oauthDisabled := oauthPref == "0" || os.Getenv("OPENAI_BASE_URL") != ""
	oauthForced := oauthPref == "1"
	oauthWins := hasOAuth && !oauthDisabled && (!hasAPIKey || oauthForced)

	switch {
	case oauthWins:
		st.Source = labelOAuth
		if hasAPIKey {
			reason := "ITERION_OPENAI_USE_OAUTH=1"
			if !oauthForced {
				// Shouldn't be reachable given oauthWins requires
				// !hasAPIKey || oauthForced, but spell it out
				// defensively so the override label stays accurate
				// if the predicate is ever refactored.
				reason = "ChatGPT-OAuth"
			}
			st.OverriddenSources = []string{
				labelAPIKey + " (overridden by " + reason + ")",
			}
		}
	default: // API-key path wins
		st.Source = labelAPIKey
		if hasOAuth {
			reason := "OPENAI_API_KEY"
			switch {
			case oauthPref == "0":
				reason = "ITERION_OPENAI_USE_OAUTH=0"
			case os.Getenv("OPENAI_BASE_URL") != "":
				reason = "OPENAI_BASE_URL"
			}
			st.OverriddenSources = []string{
				labelOAuth + " (overridden by " + reason + ")",
			}
		}
	}
	return st
}

func envSource(names ...string) string {
	for _, n := range names {
		if os.Getenv(n) != "" {
			return n
		}
	}
	return ""
}

func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func codexHomeDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// findClaudeBinary probes the host for the claude CLI via the shared
// clilocate package. Declared as a var so tests can stub host probing.
var findClaudeBinary = func() (string, bool) {
	return clilocate.Locate("", clilocate.Spec{
		Name:      "claude",
		Fallbacks: clilocate.ClaudeLocalFallback(),
	})
}

// claudeCodeKeychainService is the macOS Keychain service name Claude Code
// stores its OAuth token under (verified against Claude Code 2.x).
const claudeCodeKeychainService = "Claude Code-credentials"

// claudeKeychainOAuthSource returns a human-readable source label when
// Claude Code OAuth credentials are present in the macOS Keychain — the
// default credential store for Claude Code on darwin, where no
// `.credentials.json` file is written. Returns "" on other platforms or
// when the entry is absent, so the file-based probe stays authoritative
// there. Declared as a var so tests can stub it deterministically (the real
// implementation shells out to /usr/bin/security and must not run during
// unit tests).
//
// Existence check only: `security find-generic-password` without `-w`/`-g`
// never reads the token. Claude Code's own keychain ACL lets the owning
// user's processes find the item with no authorization prompt (the CLI
// reads it on every invocation), so this is non-intrusive even on the
// studio's 30s detection poll.
var claudeKeychainOAuthSource = func() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	if keychainHasItem(claudeCodeKeychainService) {
		return "macOS Keychain: " + claudeCodeKeychainService
	}
	return ""
}

// keychainHasItem reports whether a generic-password item with the given
// service exists in the user's keychain. Existence only — it never reads or
// prints the secret.
func keychainHasItem(service string) bool {
	// Bounded: this runs on the studio's periodic (30s) detection poll.
	// The ACL is expected to avoid a prompt (see doc comment above), but
	// a re-locked keychain can still pop a GUI auth dialog — without a
	// timeout that would wedge the poll goroutine indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Absolute path: a GUI-launched iterion may have a minimal PATH that
	// omits /usr/bin, but /usr/bin/security is always present on macOS.
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// findCodexBinary probes the host for the codex CLI. PATH lookup wins;
// then the well-known fallback locations that GUI launchers / nix
// wrappers tend to miss. Declared as a var so tests can stub it.
var findCodexBinary = func() (string, bool) {
	return clilocate.Locate("", clilocate.Spec{
		Name:      "codex",
		Fallbacks: clilocate.CommonBinaryCandidates("codex"),
	})
}

// findPiBinary probes the host for the pi CLI, honouring the same override the
// backend itself uses so detection and execution agree on one binary.
//
// The override goes through Locate as its EXPLICIT path rather than being
// returned directly: Locate rejects a path that does not exist, so a typo'd or
// stale ITERION_PI_BIN no longer makes detection report pi as present — which,
// since availability now feeds auto-selection, would resolve to a backend that
// dies at exec. Trimming matches what NewPiBackend does with the same variable.
var findPiBinary = func() (string, bool) {
	return clilocate.Locate(strings.TrimSpace(os.Getenv("ITERION_PI_BIN")), clilocate.Spec{
		Name:      "pi",
		Fallbacks: clilocate.CommonBinaryCandidates("pi"),
	})
}

// CommonBinaryCandidates is a thin re-export of
// clilocate.CommonBinaryCandidates so external callers (notably
// iterion-desktop's CLI probe at cmd/iterion-desktop/external_cli.go,
// which cannot reach pkg/internal/...) get the same fallback list.
func CommonBinaryCandidates(name string) []string {
	return clilocate.CommonBinaryCandidates(name)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// --- Cached detector ----------------------------------------------------

// CachedDetector wraps Detect with a TTL cache. Used by the HTTP server
// (30s TTL) and by the runtime executor (longer TTL on first call).
type CachedDetector struct {
	mu       sync.Mutex
	report   Report
	loaded   bool
	loadedAt time.Time
	ttl      time.Duration
}

// NewCachedDetector returns a detector with the given TTL.
// A zero ttl disables expiry (useful for tests).
func NewCachedDetector(ttl time.Duration) *CachedDetector {
	return &CachedDetector{ttl: ttl}
}

// Get returns the cached report, refreshing if past TTL.
func (c *CachedDetector) Get(ctx context.Context) Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded && (c.ttl == 0 || time.Since(c.loadedAt) < c.ttl) {
		return c.report
	}
	c.report = Detect(ctx)
	c.loaded = true
	c.loadedAt = time.Now()
	return c.report
}

// Invalidate forces a refresh on the next Get.
func (c *CachedDetector) Invalidate() {
	c.mu.Lock()
	c.loaded = false
	c.mu.Unlock()
}
