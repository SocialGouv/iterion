package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// forgeAgentBindingCookie is the per-flow CSRF-binding cookie for the
// forge OAuth connect flow (the analogue of oidcAgentBindingCookie).
const forgeAgentBindingCookie = "iterion_forge_agent"

// forgePending is the server-side state held between the forge OAuth
// /connect and /callback. Unlike oidc.PendingAuth it carries the tenant +
// forge base URL, because the callback (a public IdP redirect) resolves
// the team from the signed state, not from a path or JWT.
type forgePending struct {
	State        string
	CodeVerifier string
	Provider     forge.Provider
	ForgeBaseURL string
	TenantID     string
	UserID       string
	AgentBinding string
	NextURL      string
	// OAuthAppID pins WHICH of the tenant's GitHub Apps this flow is for, so
	// the install callback stamps the right one on the Connection instead of
	// re-deriving it from (tenant, provider, host) — ambiguous once a tenant
	// holds one app per owning org. Empty for non-app flows and for installs
	// started before the picker existed.
	OAuthAppID string
	// SecurityReadOnly carries the watch-only choice from the manifest /start
	// to its /callback, which is the only place that can stamp it on the
	// created App record. It must survive the hop across replicas — the
	// Valkey backend marshals this struct wholesale, so an exported field is
	// enough.
	SecurityReadOnly bool
	IssuedAt         time.Time
}

// forgeStateBackend stores forgePending CSRF state keyed by State, with a
// one-time `take`. The in-memory impl is single-replica; the Valkey impl
// (forge_state_valkey.go) shares it across replicas so the OAuth/manifest
// /start and /callback can land on different pods.
type forgeStateBackend interface {
	// put persists the pending state; a non-nil error means the OAuth
	// round-trip cannot complete (the callback would miss the state), so
	// callers must fail the connect start instead of handing out a doomed
	// authorize URL.
	put(p forgePending) error
	take(state string) (forgePending, bool)
}

// forgeStateStore is the TTL-bounded in-memory backend, mirroring
// oidc.MemoryStateStore. Used in local/desktop and single-replica deployments.
type forgeStateStore struct {
	mu  sync.Mutex
	m   map[string]forgePending
	ttl time.Duration
}

func newForgeStateStore(ttl time.Duration) *forgeStateStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &forgeStateStore{m: make(map[string]forgePending), ttl: ttl}
}

func (s *forgeStateStore) put(p forgePending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[p.State] = p
	return nil
}

func (s *forgeStateStore) take(state string) (forgePending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[state]
	if !ok {
		return forgePending{}, false
	}
	delete(s.m, state)
	if time.Since(p.IssuedAt) > s.ttl {
		return forgePending{}, false
	}
	return p, true
}

// appendQueryParam sets k=v on a same-origin redirect path, preserving any
// existing query. The connect wizard resumes on ?connected=/?installed=
// after the OAuth / App-install / manifest round-trips. Called on paths
// that already went through safeNext at connect time.
func appendQueryParam(path, key, val string) string {
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	q := u.Query()
	q.Set(key, val)
	u.RawQuery = q.Encode()
	return u.String()
}

// ---- helpers ----

// setForgeAgentBindingCookie issues the per-flow CSRF-binding cookie for a
// forge connect flow (the OAuth + GitHub-App callbacks verify it; the PAT
// path has no redirect and skips it). Mirrors clearForgeAgentBindingCookie.
func (s *Server) setForgeAgentBindingCookie(w http.ResponseWriter, binding string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.authCookieWriteName(forgeAgentBindingCookie),
		Value:    binding,
		Path:     s.agentBindingCookiePath("/api/forge/"),
		Domain:   s.cfg.CookieDomain,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})
}

func clearForgeAgentBindingCookie(w http.ResponseWriter, domain string, secure bool) {
	// Both spellings: a flow started before the migration holds the bare name,
	// and a single-use cookie that is not cleared is a replayable one.
	http.SetCookie(w, &http.Cookie{
		Name:     forgeAgentBindingCookie,
		Value:    "",
		Path:     "/api/forge/",
		Domain:   domain,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:  hostCookiePrefix + forgeAgentBindingCookie,
		Value: "",
		// A __Host- deletion is only honoured on the terms of its write.
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// canonicalForgeBaseURL normalises an operator-supplied forge base URL to
// scheme+host (https assumed when no scheme), or returns the provider's
// canonical SaaS host when empty.
func canonicalForgeBaseURL(raw string, provider forge.Provider) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return forge.DefaultBaseURL(provider)
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	return s
}

// canonicalWebhookBaseURL normalises the base a connection's inbound hook
// URLs are built from. Empty means "clear the pin", handing the connection
// back to the deployment's public URL.
//
// Unlike canonicalForgeBaseURL it REFUSES what it cannot make sense of
// rather than guessing: this value is handed to a forge as the address to
// deliver to, so a wrong one does not fail here — it fails much later, as
// hooks that were registered successfully and never arrive. A scheme is
// required for the same reason (assuming https for a host that only speaks
// http would produce exactly that silent shape), and a path is refused
// because the route is appended to it.
func canonicalWebhookBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("webhook_base_url is not a URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("webhook_base_url must be an absolute http(s) URL (got %q) — the forge dials this address, so the scheme cannot be inferred", s)
	}
	if u.Host == "" {
		return "", fmt.Errorf("webhook_base_url has no host: %q", s)
	}
	if u.User != nil {
		return "", fmt.Errorf("webhook_base_url must not carry credentials")
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		return "", fmt.Errorf("webhook_base_url must be scheme+host only (got path %q) — the /api/webhooks/... route is appended to it", u.Path)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("webhook_base_url must not carry a query or fragment: %q", s)
	}
	return u.Scheme + "://" + u.Host, nil
}
