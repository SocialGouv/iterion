package api

import (
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
)

// Client identity — the User-Agent (and arbitrary extra headers) sent on
// outgoing provider requests.
//
// By default every provider identifies itself honestly as
// "claw-code-go/<version>" (previously no User-Agent was set at all, so
// Go's stdlib default "Go-http-client/2.0" leaked onto the wire — the
// worst of both worlds: neither honest nor recognisable). The identity is
// operator-overridable because some Anthropic-compatible endpoints gate
// service on the calling tool's fingerprint (e.g. z.ai's GLM Coding Plan
// risk-control restricts "SDK-like" traffic). Presenting as another tool
// is a matter between the operator and that provider; claw never does it
// by default.
const (
	// clawModulePath is this module's import path, used to look up the
	// version recorded in the embedding binary's build info.
	clawModulePath = "github.com/SocialGouv/claw-code-go"

	// EnvUserAgent overrides the default client identity for every
	// provider (explicit ProviderConfig.UserAgent still wins over it).
	EnvUserAgent = "CLAW_USER_AGENT"

	// EnvCustomHeaders mirrors Claude Code's ANTHROPIC_CUSTOM_HEADERS:
	// newline-separated "Name: Value" pairs added to every outgoing
	// request, applied last so they can override any default header
	// (including User-Agent). Explicit ProviderConfig.ExtraHeaders win
	// over pairs from the environment.
	EnvCustomHeaders = "ANTHROPIC_CUSTOM_HEADERS"

	defaultAppName = "claw-code-go"
)

var defaultUserAgent = sync.OnceValue(func() string {
	return defaultAppName + "/" + clawModuleVersion()
})

// clawModuleVersion returns the claw-code-go module version recorded in the
// running binary's build info: the main-module version when claw itself is
// the main module, or the dependency (vendored) version when claw is
// embedded (e.g. in iterion). Falls back to "dev" when no version is
// recorded (local builds report "(devel)").
func clawModuleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if bi.Main.Path == clawModulePath {
		if v := cleanModuleVersion(bi.Main.Version); v != "" {
			return v
		}
		return "dev"
	}
	for _, dep := range bi.Deps {
		if dep.Path != clawModulePath {
			continue
		}
		mod := dep
		if dep.Replace != nil {
			mod = dep.Replace
		}
		if v := cleanModuleVersion(mod.Version); v != "" {
			return v
		}
		break
	}
	return "dev"
}

func cleanModuleVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	return v
}

// DefaultUserAgent returns the honest claw identity, "claw-code-go/<version>".
func DefaultUserAgent() string { return defaultUserAgent() }

// ResolveUserAgent returns the User-Agent to send: the explicit config value
// when set, else the CLAW_USER_AGENT environment override, else the default
// claw identity.
func ResolveUserAgent(explicit string) string {
	return ResolveUserAgentWithDefault(explicit, DefaultUserAgent())
}

// ResolveUserAgentWithDefault is ResolveUserAgent with a caller-supplied
// fallback identity, for paths whose protocol requires a specific default
// (the ChatGPT-Codex backend expects the codex_cli_rs identity).
func ResolveUserAgentWithDefault(explicit, fallback string) string {
	if explicit != "" {
		return explicit
	}
	if ua := strings.TrimSpace(os.Getenv(EnvUserAgent)); ua != "" {
		return ua
	}
	return fallback
}

// ParseCustomHeaders parses ANTHROPIC_CUSTOM_HEADERS-format text —
// newline-separated "Name: Value" pairs (Claude Code's format). Blank
// lines are skipped; a non-blank line without a "Name:" part is an
// explicit error rather than being silently dropped.
func ParseCustomHeaders(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	headers := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			return nil, fmt.Errorf("%s: malformed header line %q (expected \"Name: Value\")", EnvCustomHeaders, line)
		}
		headers[name] = strings.TrimSpace(value)
	}
	return headers, nil
}

// ResolveExtraHeaders merges the ANTHROPIC_CUSTOM_HEADERS environment
// variable with the explicit config headers; explicit entries win over
// environment entries on name collision.
func ResolveExtraHeaders(explicit map[string]string) (map[string]string, error) {
	merged, err := ParseCustomHeaders(os.Getenv(EnvCustomHeaders))
	if err != nil {
		return nil, err
	}
	if len(explicit) == 0 {
		return merged, nil
	}
	if merged == nil {
		merged = make(map[string]string, len(explicit))
	}
	for name, value := range explicit {
		merged[name] = value
	}
	return merged, nil
}

// ApplyIdentityHeaders sets the User-Agent and then the extra headers on h.
// It must run after all default headers so that extra headers — like Claude
// Code's ANTHROPIC_CUSTOM_HEADERS — can override any of them.
func ApplyIdentityHeaders(h http.Header, userAgent string, extra map[string]string) {
	if userAgent != "" {
		h.Set("User-Agent", userAgent)
	}
	for name, value := range extra {
		h.Set(name, value)
	}
}
