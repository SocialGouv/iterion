package server

import (
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
	IssuedAt   time.Time
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
		Name:     forgeAgentBindingCookie,
		Value:    binding,
		Path:     "/api/forge/",
		Domain:   s.cfg.CookieDomain,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})
}

func clearForgeAgentBindingCookie(w http.ResponseWriter, domain string, secure bool) {
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
