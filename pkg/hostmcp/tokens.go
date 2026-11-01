package hostmcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/store"
)

// DefaultGrantTTL caps how long a run token stays alive without an explicit
// Revoke. A run that crashes mid-flight would otherwise leak its token until
// process exit. The window is long enough to cover any realistic single-run
// duration (sandboxed agent runs are typically minutes to single-digit
// hours).
const DefaultGrantTTL = 24 * time.Hour

// MaxTokens bounds a registry so a misbehaving caller that Registers without
// Revoking can't grow it unbounded.
const MaxTokens = 1024

// Grant is what a minted token authorizes: a capability set, the tenant the
// token is pinned to, and the run facts the serving side dispatches under.
type Grant struct {
	Capabilities boardops.Capabilities
	// TenantID pins token-authenticated calls to the run's tenant. Empty in
	// local mode. An MCP request has no operator JWT context, so the tenant
	// comes from the grant — minted by the host from the authoritative run
	// message, never from anything the caller sends.
	TenantID string
	// SourceIssueID is the ticket that owns the granted run, carried so
	// board.create over this transport auto-stamps parent_id /
	// spawned_from the same way the stdio and in-process transports do.
	// Empty for runs with no owning ticket (plain studio/CLI launches).
	SourceIssueID string
	// Trust is the run document's own RunTrust (#1802): the connector facade
	// resolves credentials through it, so a token minted for a run over code
	// the tenant does not control resolves READ operations only. Stated is
	// false when the minter could not read the run — which the facade must
	// read as "unknown", never as "trusted" (connection.TrustUnknown).
	Trust       string
	TrustStated bool
	// WorkDir is the run's working directory: the facade's PROJECT catalog
	// tier is rooted at THE RUN'S workspace, not at whatever directory the
	// serving process was started in — the two agree only when a studio is
	// launched inside the one project it serves. Empty on grants minted
	// before this field existed (legacy payloads), which the per-run catalog
	// reads as "no workspace of its own" and the project tier as absent.
	WorkDir   string
	ExpiresAt time.Time
}

// TokenStore is the per-run token registry the host MCP handlers authorize
// against. Register is called by the host's per-node token-minter closure —
// a Register failure means the minted token would never authorize, so the
// minter must not hand it out; Lookup is called by the HTTP handlers.
//
// The in-memory [Registry] is single-process: right wherever the token's
// listener lives in the process that minted it (the studio, the cloud
// runner pod). A token whose LOOKUP happens on another replica — the
// server's load-balanced mux — needs the Valkey implementation
// (pkg/server), which shares grants across replicas.
//
// The trust argument is the run's store.RunTrust: the connector facade's
// credential resolution (#2758) fails CLOSED on an unstated trust, so the
// minter must say when it could not read the run rather than letting the
// zero value read as the trusted one. workDir is the run's working
// directory, which the facade's project tier is rooted at for that run's
// calls alone.
type TokenStore interface {
	Register(token string, caps []string, sourceIssueID, tenantID string, trust store.RunTrust, trustStated bool, workDir string) error
	Revoke(token string)
	Lookup(token string) (Grant, bool)
}

// Registry is the ephemeral in-memory TokenStore. The runtime MUST call
// Register at the start of every node grant and Revoke at the end. Even
// with a missed Revoke, entries are reaped after DefaultGrantTTL and the
// registry is capped at MaxTokens.
type Registry struct {
	mu     sync.RWMutex
	tokens map[string]Grant
	now    func() time.Time // injectable for tests
}

// NewRegistry returns an empty in-memory registry.
func NewRegistry() *Registry {
	return &Registry{
		tokens: map[string]Grant{},
		now:    time.Now,
	}
}

// MintToken mints a random opaque X-Iterion-Run token (32 bytes hex) for a
// per-node grant. Returns "" on the (effectively impossible) crypto/rand
// failure so callers degrade to capability-disabled rather than panicking.
func MintToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// Register stores a token with its grant. A subsequent call with the same
// token replaces the grant. A full registry is an error: the token would
// never authorize, so the caller must not hand it out.
func (r *Registry) Register(token string, caps []string, sourceIssueID, tenantID string, trust store.RunTrust, trustStated bool, workDir string) error {
	grant := Grant{
		Capabilities:  boardops.Capabilities{},
		SourceIssueID: strings.TrimSpace(sourceIssueID),
		TenantID:      strings.TrimSpace(tenantID),
		Trust:         string(trust),
		TrustStated:   trustStated,
		WorkDir:       workDir,
		ExpiresAt:     r.now().Add(DefaultGrantTTL),
	}
	for _, c := range caps {
		grant.Capabilities[c] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Sweep expired entries on every write so a long-running daemon
	// doesn't accumulate dead tokens between explicit Revokes.
	r.sweepLocked()
	if len(r.tokens) >= MaxTokens {
		// Hard cap: refuse to grow the registry beyond the limit. An
		// unregistered token's CallTool would 401 (lookup miss), so
		// surface the refusal instead of no-opping.
		return fmt.Errorf("host MCP token registry full (%d tokens)", MaxTokens)
	}
	r.tokens[token] = grant
	return nil
}

// Revoke removes the token. A revoked token's subsequent calls fail with 401.
func (r *Registry) Revoke(token string) {
	r.mu.Lock()
	delete(r.tokens, token)
	r.mu.Unlock()
}

// Lookup returns the grant a token carries, false when the token is unknown
// or expired (an expired entry is evicted lazily on first failed Lookup).
func (r *Registry) Lookup(token string) (Grant, bool) {
	r.mu.RLock()
	g, ok := r.tokens[token]
	r.mu.RUnlock()
	if !ok {
		return Grant{}, false
	}
	if !g.ExpiresAt.IsZero() && r.now().After(g.ExpiresAt) {
		// TTL elapsed — drop lazily on first failed lookup.
		r.mu.Lock()
		// Re-check under write lock to avoid evicting a freshly-renewed
		// token that another goroutine just Registered.
		if cur, stillThere := r.tokens[token]; stillThere && !cur.ExpiresAt.IsZero() && r.now().After(cur.ExpiresAt) {
			delete(r.tokens, token)
		}
		r.mu.Unlock()
		return Grant{}, false
	}
	return g, true
}

// sweepLocked removes every expired entry. Caller must hold r.mu.
func (r *Registry) sweepLocked() {
	now := r.now()
	for k, v := range r.tokens {
		if !v.ExpiresAt.IsZero() && now.After(v.ExpiresAt) {
			delete(r.tokens, k)
		}
	}
}
