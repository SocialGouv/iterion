package compatgw

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	openaiprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/openai"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// UserAgent is the identity the gateway client presents. claw's own honest
// default (claw-code-go/<version>) applies — NoAmbientHeaders keeps the
// environment from overriding it, and the gateway needs no codex-cli
// masquerade.
const UserAgent = ""

// NewClient builds the claw client for a validated gateway: the chat wire,
// stream usage requested (the gateway's usage account reaches PR-2's meter),
// the model id verbatim (the gateway id is whatever the gateway serves — no
// prefix stripping, no alias substitution), no ambient identity overrides,
// and a guarded transport.
func NewClient(cfg Config, modelID string, strict bool) (api.APIClient, error) {
	// Cheap syntax validation ONLY — no DNS here. The full Validate (which
	// resolves the host) runs at the dispatch seams (the element builder,
	// the Execute head, the sandbox forward), which re-read the env fresh
	// every attempt; a factory that resolved DNS would bake one transient
	// resolver blip into the registry's per-key cache for the process
	// life. The dial-time guard enforces the same address rule on every
	// connection regardless.
	if err := cfg.ValidateSyntax(); err != nil {
		return nil, err
	}
	transport, err := guardedTransport(strict)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Transport:     transport,
		CheckRedirect: redirectRefusedPolicy,
	}
	client, err := openaiprovider.New().NewClient(api.ProviderConfig{
		APIKey:              cfg.APIKey,
		BaseURL:             cfg.BaseURL,
		Model:               modelID,
		OpenAIWireAPI:       api.OpenAIWireChat,
		OpenAIStreamUsage:   true,
		OpenAIModelVerbatim: true,
		NoAmbientHeaders:    true,
		UserAgent:           UserAgent,
		HTTPClient:          hc,
	})
	if err != nil {
		return nil, fmt.Errorf("openai_compatible: client: %w", err)
	}
	return client, nil
}

// guardedTransport dials only the host the operator's env named, pinned to
// an address validated at every new connection (DNS-rebinding-proof). By
// default it uses no proxy: an ambient HTTPS_PROXY is never trusted for the
// gateway. The ONE exception is engine-owned — a policy-isolated sandbox
// only lets egress through the run's network proxy, and the engine hands
// that endpoint to the in-container runner under SandboxProxyEndpointEnv
// (a name only the engine sets; the container image and the operator's
// shell never speak for it). With it, the transport dials the VALIDATED
// gateway host through that proxy — the host check runs exactly as the
// direct dial would, so the proxy is a path, never an authority that
// bypasses the guard. The proxy's own address is private by nature (the
// runner pod IP, the docker host gateway): a proxified transport dials it
// plainly — the SafeTransport guard is the DIRECT dial's guard, and the
// request-level host validation carries the boundary.
//
// No client Timeout: a generation is long by design and the caller's
// cold-stream watchdog bounds pre-header silence. ResponseHeaderTimeout
// stays as the floor for the watchdog-off case. A set-but-unusable
// engine value is an ENGINE FAULT, not a silent fallback to direct: the
// error names the variable (a silent direct dial inside a policy-isolated
// sandbox reproduces the exact park this channel exists to kill).
func guardedTransport(strict bool) (*http.Transport, error) {
	t := httpdial.SafeTransport(strict)
	t.ResponseHeaderTimeout = 60 * time.Second
	endpoint := os.Getenv(SandboxProxyEndpointEnv)
	if endpoint == "" {
		// The SafeTransport ships http.ProxyFromEnvironment: an ambient
		// HTTPS_PROXY would reach the gateway through it. Never: the
		// gateway trusts the engine's variable only.
		t.Proxy = nil
		return t, nil
	}
	proxyURL, err := url.Parse(endpoint)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("openai_compatible: %s is set but is not a usable proxy URL — refusing to fall back to a direct dial the sandbox policy would drop; fix the engine value",
			SandboxProxyEndpointEnv)
	}
	return &http.Transport{
		Proxy:                 sandboxProxyFunc(strict, proxyURL),
		ResponseHeaderTimeout: 60 * time.Second,
	}, nil
}

// SandboxProxyEndpointEnv is set by the ENGINE on a sandboxed in-container
// runner: the run's network-proxy URL, the only egress the sandbox's
// network policy allows. An ambient value (the container image, the
// operator's shell) is refused — this name is the runtime's channel, and
// the dial guard validates the gateway host exactly as it would without a
// proxy.
const SandboxProxyEndpointEnv = "ITERION_SANDBOX_PROXY_ENDPOINT"

// sandboxProxyFunc returns the transport's Proxy resolution: the validated
// gateway host crosses through the engine-provided proxy. The host is
// resolved and validated per request, before any byte is handed to the
// proxy.
func sandboxProxyFunc(strict bool, proxyURL *url.URL) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if _, err := httpdial.ResolvePublicHost(req.Context(), req.URL.Hostname(), strict); err != nil {
			return nil, err
		}
		return proxyURL, nil
	}
}

// resolveHost validates a gateway HOST the same way the dial will: through
// httpdial, so Validate cannot accept what the transport would refuse.
func resolveHost(host string) (net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Non-strict: the ADDRESS judgement is Validate's job (with the
	// remedy attached), not the resolver's.
	return httpdial.ResolvePublicHost(ctx, host, false)
}

// redirectRefusedPolicy is the client's redirect policy: a gateway that
// answers a model request with a redirect is not serving the model, and
// following it would send the credential to a host the operator never
// named. The typed error travels as-is through claw's error path —
// non-retryable, stable message.
func redirectRefusedPolicy(req *http.Request, _ []*http.Request) error {
	return &RedirectRefusedError{To: req.URL.Host}
}

// RedirectRefusedError is what a 3xx from the gateway becomes: the endpoint
// the operator named did not serve the model, and following it would send
// the credential to a host they did not. Non-retryable by construction — a
// new request redirects the same way.
type RedirectRefusedError struct {
	To string
}

func (e *RedirectRefusedError) Error() string {
	return fmt.Sprintf("openai_compatible: the gateway redirected to %q — redirect refused; set %s to the final URL", e.To, BaseURLEnv)
}
