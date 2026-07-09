// Package oidc owns the SSO connectors: Google, GitHub, and a
// generic OIDC discovery-based provider. Each connector exposes the
// same Connector interface so the server's auth_routes can iterate
// uniformly.
//
// State + PKCE storage is intentionally minimal — we hold a small
// in-memory map keyed by the random state token; the timeout is
// short (10 minutes) and the store is per-process. Multi-replica
// deployments can swap in a Mongo-backed store later.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// Connector is the per-provider façade. The server obtains a
// Connector for each enabled provider at startup and routes
// /auth/oidc/<name>/start and /auth/oidc/<name>/callback through it.
type Connector interface {
	// Name returns the URL slug for the provider (e.g. "google").
	Name() string

	// Display returns the name shown on the SPA login button.
	Display() string

	// AuthorizeURL builds the URL the user is redirected to. The
	// returned state value MUST be passed back in the callback;
	// callers persist it for verification.
	AuthorizeURL(ctx context.Context, redirectURI, state, codeVerifier string) (string, error)

	// ExchangeCode trades the authorization code for an access
	// token (and ID token where applicable), then fetches the
	// external user profile. Returns the canonical ExternalUser.
	ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (ExternalUser, error)

	// SupportsPKCE reports whether the provider's authorize call
	// must include a PKCE challenge. Modern providers all do; we
	// gate it so a stub provider in tests can opt out.
	SupportsPKCE() bool
}

// ExternalUser is the post-exchange identity returned by every
// connector. The Subject is the provider's stable per-account
// identifier; we never use Email as a key because providers let
// users change their email.
type ExternalUser struct {
	Provider string
	Subject  string
	Email    string
	Name     string
	// Groups is the provider-side group/team membership of the user, used by
	// the per-org grant logic. GitHub populates lowercased "<org>/*" (one per
	// org the user belongs to) + "<org>/<team-slug>" (one per team). Empty for
	// providers that don't surface groups (Google). A future OIDC group-claim
	// mapping can populate the same field.
	Groups []string
}

// Sentinel errors raised by connectors. The HTTP layer maps them.
var (
	ErrUnknownProvider  = errors.New("oidc: unknown provider")
	ErrProviderDisabled = errors.New("oidc: provider is disabled")
	ErrStateNotFound    = errors.New("oidc: state expired or unknown")
	ErrEmailMissing     = errors.New("oidc: provider returned no email")
	ErrEmailNotVerified = errors.New("oidc: provider returned an unverified email")
)

// PendingAuth captures the per-flow state held server-side between
// /start and /callback. Stored in a StateStore (memory by default).
type PendingAuth struct {
	Provider     string
	State        string
	CodeVerifier string
	RedirectURI  string
	NextURL      string // post-login redirect target (sanitized to relative paths)
	IssuedAt     time.Time
	// AgentBinding is a per-flow random token the HTTP layer sets as
	// an HttpOnly cookie at /start and verifies at /callback. RFC 9700
	// (OAuth 2.0 Security BCP) §4.7.1 mandates a CSRF mechanism beyond
	// `state`; the state parameter alone proves freshness/uniqueness
	// but does not bind the flow to the user agent that initiated it.
	// Without this binding, an attacker who completes /start in their
	// browser and lures a victim into hitting /callback with that
	// state pins the victim into the attacker's account on iterion
	// (the classic login-CSRF / session-fixation against OAuth).
	// Empty string for non-browser callers (CLI / SDK) where the
	// transport guarantees agent binding by other means.
	AgentBinding string

	// TenantID + OrgProviderID are set when the flow was initiated against a
	// per-org provider (a tenant's own Keycloak). The callback resolves the
	// tenant's policy (membership grant, default role, auto-link) from these,
	// read SERVER-SIDE via the state lookup — never from the URL or a cookie —
	// so a per-org indirection cannot enable provider/tenant confusion (start
	// org A's Keycloak, complete the callback resolved as org B). Empty for
	// global providers (github / google / the deployment "sso").
	TenantID      string
	OrgProviderID string

	// LinkUserID is set when the flow was initiated by an already-authenticated
	// user explicitly connecting this SSO identity to their account (the
	// /api/auth/oidc/<provider>/link/start path). The callback then attaches the
	// resolved external identity to this user instead of running the normal
	// login/signup logic. Empty for an ordinary sign-in flow.
	LinkUserID string

	// Desktop marks a flow initiated by the DESKTOP app. Instead of setting
	// browser cookies and redirecting to an SPA path, the callback mints a
	// single-use exchange ticket and redirects to DesktopRedirect (a loopback
	// URL the desktop listens on); the desktop then redeems the ticket for
	// tokens at /api/auth/desktop/exchange over its native client. Keeps the
	// IdP redirect_uri on the stable cloud origin (no IdP reconfiguration) and
	// keeps the refresh token out of any URL. Empty for browser flows.
	Desktop         bool
	DesktopRedirect string
}

// StateStore is the persistence interface for PendingAuth records.
type StateStore interface {
	Put(ctx context.Context, p PendingAuth) error
	Take(ctx context.Context, state string) (PendingAuth, error)
}

// MemoryStateStore is the default StateStore.
type MemoryStateStore struct {
	mu  sync.Mutex
	m   map[string]PendingAuth
	ttl time.Duration
}

// NewMemoryStateStore returns a store with a per-entry TTL (entries
// older than ttl are evicted on access).
func NewMemoryStateStore(ttl time.Duration) *MemoryStateStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &MemoryStateStore{m: make(map[string]PendingAuth), ttl: ttl}
}

func (s *MemoryStateStore) Put(_ context.Context, p PendingAuth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[p.State] = p
	return nil
}

func (s *MemoryStateStore) Take(_ context.Context, state string) (PendingAuth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[state]
	if !ok {
		return PendingAuth{}, ErrStateNotFound
	}
	delete(s.m, state)
	if time.Since(p.IssuedAt) > s.ttl {
		return PendingAuth{}, ErrStateNotFound
	}
	return p, nil
}

// Sweep evicts every PendingAuth older than the configured TTL. Take
// already discards expired entries lazily, but a user who clicked
// "Sign in with Google" then closed the tab never returns — without
// this the entry sits in memory until process restart. Returns the
// number of entries evicted so a caller can wire it into a metric.
//
// Safe to call concurrently with Put/Take.
func (s *MemoryStateStore) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted := 0
	now := time.Now()
	for state, p := range s.m {
		if now.Sub(p.IssuedAt) > s.ttl {
			delete(s.m, state)
			evicted++
		}
	}
	return evicted
}

// StartSweeper runs Sweep on a fixed cadence until ctx is cancelled.
// Blocks; callers typically launch it in a goroutine. Recommended
// interval is the store's TTL so even an attacker spamming Put never
// keeps more than ~2× TTL worth of entries in memory.
func (s *MemoryStateStore) StartSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = s.ttl
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.Sweep()
		}
	}
}

// GenerateStateAndPKCE returns a random state value (~32 bytes
// base64url) and a PKCE pair (verifier + S256 challenge).
func GenerateStateAndPKCE() (state, verifier, challenge string, err error) {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", "", "", err
	}
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(stateBytes)
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return state, verifier, challenge, nil
}

// Registry maps provider slugs to Connectors. Used by the HTTP
// layer to dispatch start/callback.
type Registry struct {
	connectors map[string]Connector
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{connectors: make(map[string]Connector)} }

// Register attaches a connector. Idempotent overwrite.
func (r *Registry) Register(c Connector) {
	r.connectors[c.Name()] = c
}

// Get looks up a connector by name.
func (r *Registry) Get(name string) (Connector, error) {
	c, ok := r.connectors[name]
	if !ok {
		return nil, ErrUnknownProvider
	}
	return c, nil
}

// Enabled returns the registered connectors in declaration order.
func (r *Registry) Enabled() []Connector {
	out := make([]Connector, 0, len(r.connectors))
	for _, c := range r.connectors {
		out = append(out, c)
	}
	return out
}
