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
}

// Sentinel errors raised by connectors. The HTTP layer maps them.
var (
	ErrUnknownProvider = errors.New("oidc: unknown provider")
	ErrStateNotFound   = errors.New("oidc: state expired or unknown")
	ErrEmailMissing    = errors.New("oidc: provider returned no email")
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
