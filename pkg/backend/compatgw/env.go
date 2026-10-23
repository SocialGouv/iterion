// Package compatgw serves `openai_compatible/<gateway model id>` from the
// deployment's environment: OPENAI_COMPATIBLE_BASE_URL names an
// OpenAI-compatible gateway (LiteLLM, vLLM, a router), OPENAI_COMPATIBLE_API_KEY
// authenticates it, and the gateway model id reaches the wire verbatim.
//
// The endpoint is the OPERATOR's choice, read from the process environment
// the operator controls — the same trust class as OPENAI_BASE_URL — but what
// it dials is guarded all the same (pkg/secure/httpdial): the model spec that
// lands here is workflow text, and a workflow must not be able to steer the
// gateway client at private infrastructure by naming a spec whose gateway
// happens to redirect.
package compatgw

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/internal/envtrust"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// Environment variables serving the gateway. Named for the operator's
// compose file, not for iterion: they configure the deployment, exactly like
// OPENAI_BASE_URL does.
const (
	// BaseURLEnv names the gateway root (no trailing /v1 required; the OpenAI
	// provider appends its own paths).
	BaseURLEnv = "OPENAI_COMPATIBLE_BASE_URL"
	// APIKeyEnv authenticates the gateway. Empty is valid: a gateway may sit
	// behind network auth alone, and injecting name=<empty> would read as an
	// attempted auth to some providers.
	APIKeyEnv = "OPENAI_COMPATIBLE_API_KEY"
)

// ErrNotConfigured is refused before anything is dispatched when the
// environment does not serve a gateway. It names the variable that is
// missing — never a value.
var ErrNotConfigured = errors.New("openai_compatible: no gateway in the environment: set " + BaseURLEnv + " (and " + APIKeyEnv + " when the gateway authenticates)")

// Config is the gateway endpoint as the environment serves it.
type Config struct {
	BaseURL string
	APIKey  string
}

// FromEnv reads the gateway config through get (os.Getenv, or a test's map).
// Both variables unset — or the base URL blank — is ErrNotConfigured; a set
// base URL with no key is accepted (network-authenticated gateways exist).
func FromEnv(get func(string) string) (Config, error) {
	base, key := strings.TrimSpace(get(BaseURLEnv)), strings.TrimSpace(get(APIKeyEnv))
	if base == "" {
		return Config{}, ErrNotConfigured
	}
	return Config{BaseURL: base, APIKey: key}, nil
}

// Validate refuses an endpoint that cannot be what the operator meant,
// before a request or a credential goes anywhere. strict (the default)
// requires a public-unicast host — the dial-time guard would refuse it
// anyway, and this says so before the sandbox is built; the
// ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1 escape hatch lifts it for a
// self-hosted gateway on private infrastructure.
//
// It resolves the host: call it at the dispatch seams, which re-read the
// env fresh per attempt — never inside the registry factory, whose
// per-key cache would keep a transient resolver failure for the process
// life.
func (c Config) Validate(strict bool) error {
	if err := c.ValidateSyntax(); err != nil {
		return err
	}
	if strict {
		ip, err := resolveHost(c.url().Hostname())
		if err != nil {
			return fmt.Errorf("openai_compatible: %w", err)
		}
		if !httpdial.IsPublicUnicast(ip) {
			return RemedyPrivate(fmt.Errorf("%s host is not a public address", BaseURLEnv))
		}
	}
	return nil
}

// ValidateSyntax is the DNS-free half: a parseable http(s) URL naming a
// host, no embedded userinfo, no query or fragment. Safe to run wherever
// the config is read — including a registry factory whose error the
// per-key cache would keep for the process life.
func (c Config) ValidateSyntax() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("openai_compatible: %s does not parse as a URL: %w", BaseURLEnv, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("openai_compatible: %s must be an http or https URL, got scheme %q", BaseURLEnv, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("openai_compatible: %s names no host", BaseURLEnv)
	}
	if u.User != nil {
		return fmt.Errorf("openai_compatible: %s must not embed credentials — set "+APIKeyEnv+" instead", BaseURLEnv)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("openai_compatible: %s must not carry a query or fragment", BaseURLEnv)
	}
	return nil
}

// Host is the host the gateway client dials — the base URL's — or "" for a
// base URL ValidateSyntax refuses, which no request reaches.
func (c Config) Host() string {
	if c.ValidateSyntax() != nil {
		return ""
	}
	return c.url().Hostname()
}

// url re-parses the base URL for the strict half; ValidateSyntax has just
// proven it parses.
func (c Config) url() *url.URL {
	parsed, _ := url.Parse(c.BaseURL)
	return parsed
}

// RemedyPrivate names the escape hatch on every private-address refusal —
// an IP-literal endpoint's refusal would otherwise come back from the
// resolver with no hint that a self-hosted gateway has a way through. When
// the hatch value came from a project .env, the refusal says so: that value
// does not speak for the operator, and re-reading it in the same file
// teaches the wrong remedy.
func RemedyPrivate(err error) error {
	if envtrust.Planted("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE") {
		return fmt.Errorf("openai_compatible: %w — ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1 was set by a project .env, which does not speak for the operator: export it in your shell", err)
	}
	return fmt.Errorf("openai_compatible: %w (self-hosted gateway? set ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1)", err)
}
