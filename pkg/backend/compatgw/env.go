// Package compatgw serves `openai_compatible/<gateway model id>` from the
// deployment's environment: OPENAI_COMPATIBLE_BASE_URL names an
// OpenAI-compatible gateway (LiteLLM, vLLM, a router), OPENAI_COMPATIBLE_API_KEY
// authenticates it, and the gateway model id reaches the wire verbatim.
//
// The pair is read from the process environment — the same trust class as
// OPENAI_BASE_URL, and a project `.env` can fill either name as well, which
// is why a planted half refuses the gateway by name instead of configuring it
// (FromEnv). What the endpoint dials is guarded whoever named it
// (pkg/secure/httpdial): the model spec that lands here is workflow text, and
// a workflow must not be able to steer the gateway client at private
// infrastructure by naming a spec whose gateway happens to redirect.
package compatgw

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// Environment variables serving the gateway. Named for the operator's
// compose file, not for iterion: they configure the deployment, exactly like
// OPENAI_BASE_URL does.
const (
	// BaseURLEnv names the gateway root (no trailing /v1 required; the OpenAI
	// provider appends its own paths).
	BaseURLEnv = "OPENAI_COMPATIBLE_BASE_URL"
	// APIKeyEnv authenticates the gateway, and is REQUIRED. The one client
	// this package builds is claw's OpenAI provider, which refuses an empty
	// key outright — "provide either OPENAI_API_KEY or a (OAuthToken +
	// OpenAIChatGPTAccountID) pair sourced from Codex CLI auth.json", two
	// channels that serve no gateway — so a keyless gateway is not a
	// configuration that works, whatever a network-authenticated one would
	// deserve. FromEnv refuses it here instead, by the name the operator set.
	APIKeyEnv = "OPENAI_COMPATIBLE_API_KEY"
)

// ErrNotConfigured is refused before anything is dispatched when the
// environment does not serve a gateway. It names the variable that is
// missing — never a value.
var ErrNotConfigured = errors.New("openai_compatible: no gateway in the environment: set " + BaseURLEnv + " and " + APIKeyEnv)

// ErrNoKey is refused for a gateway the environment names but authenticates
// with nothing. Refused HERE, on the launch-and-validation seam, rather than
// at the dispatch the client builder dies at: that refusal comes back naming
// OPENAI_API_KEY and Codex's auth.json, neither of which serves a gateway,
// and it arrives after the chain has stopped being able to walk to a
// fallback.
var ErrNoKey = errors.New("openai_compatible: " + BaseURLEnv + " names a gateway but " + APIKeyEnv + " is empty: the gateway client presents that key on every call and is refused without one — set it in the environment the deployment runs with")

// plantedPairRemedy is said on every refusal a project `.env` caused: the
// channel that answers for the operator is the environment the deployment
// exports, and `.env` loading stops at the first file found walking up, so a
// repository's own file shadows a deployment one higher in the tree.
const plantedPairRemedy = "export it in the environment the deployment runs with — a project `.env` is read for every run started in that tree, including a run reviewing it"

// Config is the gateway endpoint as the environment serves it.
type Config struct {
	BaseURL string
	APIKey  string
}

// FromEnv reads the gateway config through get (os.Getenv, or a test's map).
// Both variables unset — or the base URL blank — is ErrNotConfigured; a named
// endpoint with no key is ErrNoKey.
//
// The gateway is served by the environment this process INHERITED, and by
// nothing else: a half a project `.env` planted refuses the pair by name
// (plantedPairError) instead of configuring it. The question is asked for the
// PAIR because the two names are one credential presentation — this key
// authenticates to THAT endpoint:
//
//   - a planted KEY is not the operator's. The host crosses no key for a
//     gateway it has none for, and the in-container runner auto-loads the
//     `.env` of the repository under review over exactly the names the
//     crossing left absent; on a router-style gateway that key selects which
//     virtual key — hence which budget, model allowlist and tenant — its
//     reviewer's calls are billed to.
//   - a planted ENDPOINT would have the operator's own key presented to a
//     host the repository chose, which is the credential leaving the
//     deployment. `.env` loading stops at the first file found walking up, so
//     a repository's file shadows a deployment one higher in the tree.
//
// Refused rather than degraded, because the degradation does not exist: a
// Config with no key builds no client at all (claw's OpenAI provider refuses
// it, naming OPENAI_API_KEY and Codex's auth.json), so dropping the key
// bought a dead node whose message pointed at neither this gateway nor the
// file that caused it. The refusal names the variable and travels where the
// chain can still walk to a fallback. For the laptop the drop was meant to
// serve, the remedy is in the message: the same two lines, exported.
//
// The host-side plant is closed one step earlier, at the loader
// (internal/envtrust.InheritedCredentialsAt): an endpoint is not applied at
// all while the key presented there is inherited. This refusal is what still
// answers for the pair a `.env` was allowed to fill — a laptop's own gateway,
// a marker a parent exported — and for the container, where the runner reads
// the workspace file itself.
func FromEnv(get func(string) string) (Config, error) {
	base, key := strings.TrimSpace(get(BaseURLEnv)), strings.TrimSpace(get(APIKeyEnv))
	if base == "" {
		return Config{}, ErrNotConfigured
	}
	if err := plantedPairError(); err != nil {
		return Config{}, err
	}
	if key == "" {
		return Config{}, ErrNoKey
	}
	return Config{BaseURL: base, APIKey: key}, nil
}

// plantedPairError names the half a project `.env` filled, or nil when the
// pair is the environment's own. The endpoint is asked first: it is the half
// that decides where the other one goes.
func plantedPairError() error {
	for _, name := range []string{BaseURLEnv, APIKeyEnv} {
		if envtrust.Planted(name) {
			return fmt.Errorf("openai_compatible: %s comes from a project `.env`, which does not speak for the operator: the endpoint and the key it is presented at are one credential presentation, so the gateway is refused rather than half-configured — %s", name, plantedPairRemedy)
		}
	}
	return nil
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
			return remedyPrivate(fmt.Errorf("%s host is not a public address", BaseURLEnv))
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

// remedyPrivate names the escape hatch on every private-address refusal —
// an IP-literal endpoint's refusal would otherwise come back from the
// resolver with no hint that a self-hosted gateway has a way through.
func remedyPrivate(err error) error {
	return fmt.Errorf("openai_compatible: %w (self-hosted gateway? set ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1)", err)
}
