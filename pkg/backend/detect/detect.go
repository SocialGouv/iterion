// Package detect probes the host environment for available LLM credentials
// and CLI binaries, producing a Report consumed by the studio (UI hints)
// and the runtime resolver (auto backend selection).
package detect

import (
	"context"
	"os"
	"path/filepath"
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

// Detect runs the probes synchronously. ctx is honored for timed probes
// (currently none — all probes are local stat / env reads).
func Detect(ctx context.Context) Report {
	prov := detectProviders()
	backends := detectBackends(prov)
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

func detectBackends(prov []ProviderStatus) []BackendStatus {
	return []BackendStatus{
		detectOAuthCLI(
			BackendClaudeCode, findClaudeBinary, claudeConfigDir,
			// Linux/WSL ships Claude Code OAuth as `.credentials.json`
			// (hidden); older docs/macOS keychain export use the
			// non-hidden form. Probe both.
			[]string{".credentials.json", "credentials.json"},
			"claude CLI",
			"~/.claude/.credentials.json (Claude Code OAuth)",
		),
		detectClaw(prov),
		detectOAuthCLI(
			BackendCodex, findCodexBinary, codexHomeDir,
			[]string{"auth.json"},
			"codex CLI",
			"$CODEX_HOME/auth.json (Codex OAuth)",
		),
	}
}

// detectOAuthCLI reports Available=true only when both a binary and an
// OAuth credentials file are present. The API-key-only path (e.g.
// ANTHROPIC_API_KEY + claude binary, no OAuth) is intentionally NOT
// auto-eligible: claw uses the same key in-process and is faster, so
// users wanting the CLI in that case must set `backend:` explicitly.
func detectOAuthCLI(
	name string,
	findBin func() (string, bool),
	configDir func() string,
	credFiles []string,
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
			SuggestedModel: "anthropic/claude-sonnet-4-6",
		},
		{
			Name:           "zai",
			Available:      zaiAvailable,
			Source:         envSource("ZAI_API_KEY", "ANTHROPIC_BASE_URL"),
			SuggestedModel: "anthropic/glm-4.6",
		},
		detectOpenAIProvider(),
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

// findCodexBinary probes the host for the codex CLI. PATH lookup wins;
// then the well-known fallback locations that GUI launchers / nix
// wrappers tend to miss. Declared as a var so tests can stub it.
var findCodexBinary = func() (string, bool) {
	return clilocate.Locate("", clilocate.Spec{
		Name:      "codex",
		Fallbacks: clilocate.CommonBinaryCandidates("codex"),
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
