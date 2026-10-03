package compatgw

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
	hc := &http.Client{
		Transport:     guardedTransport(strict),
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
// an address validated at every new connection (DNS-rebinding-proof), with
// no proxy: an ambient HTTPS_PROXY is never trusted for the gateway — if a
// deployment routes egress through an inspecting proxy, that is
// engine-owned sandbox configuration, not this process's environment.
//
// No client Timeout: a generation is long by design and the caller's
// cold-stream watchdog bounds pre-header silence. ResponseHeaderTimeout
// stays as the floor for the watchdog-off case.
func guardedTransport(strict bool) *http.Transport {
	t := httpdial.SafeTransport(strict)
	t.Proxy = nil
	t.ResponseHeaderTimeout = 60 * time.Second
	return t
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
