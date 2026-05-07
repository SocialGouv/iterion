// Package detect probes the host environment for available LLM credentials
// and CLI binaries, producing a Report consumed by the editor (UI hints)
// and the runtime resolver (auto backend selection).
package detect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
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
// editor/src/api/backends.ts.
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
	Name           string `json:"name"`
	Available      bool   `json:"available"`
	Source         string `json:"source"`
	SuggestedModel string `json:"suggested_model,omitempty"`
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
	out := []ProviderStatus{
		{
			Name:           "anthropic",
			Available:      os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "",
			Source:         envSource("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"),
			SuggestedModel: "anthropic/claude-sonnet-4-6",
		},
		{
			Name:           "openai",
			Available:      os.Getenv("OPENAI_API_KEY") != "",
			Source:         envSource("OPENAI_API_KEY"),
			SuggestedModel: "openai/gpt-5.4-mini",
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

// findClaudeBinary duplicates pkg/backend/delegate/claudesdk/process.go:findCLI
// (no-explicit-path branch) to avoid widening the claudesdk export surface
// for a 5-line probe.
func findClaudeBinary() (string, bool) {
	if path, err := exec.LookPath("claude"); err == nil {
		return path, true
	}
	if home, err := os.UserHomeDir(); err == nil {
		local := filepath.Join(home, ".claude", "local", "claude")
		if isExecutable(local) {
			return local, true
		}
	}
	return "", false
}

// findCodexBinary duplicates codex-agent-sdk-go/internal/cli/discovery.go;
// the upstream helper is unexported and the search list is small.
func findCodexBinary() (string, bool) {
	if path, err := exec.LookPath("codex"); err == nil {
		return path, true
	}
	home, _ := os.UserHomeDir()
	candidates := []string{}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".volta", "bin", "codex"),
			filepath.Join(home, ".local", "bin", "codex"),
		)
	}
	candidates = append(candidates, "/usr/local/bin/codex", "/usr/bin/codex")
	for _, p := range candidates {
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
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
