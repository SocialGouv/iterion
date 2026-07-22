package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// authorTrust classifies whether an issue author holds real rights on a repo
// — the gate that decides if an inbound issue may spend LLM budget (auto
// triage / auto implement) or must be parked for operator approval.
//
// Unlike the org quota gates (operator policy, fail-open), this is a
// SECURITY boundary against budget-drain by drive-by issue authors: any
// unknown (API error, no client, unknown vocab) classifies as UNTRUSTED.
//
// Order: static allowlist → payload author_association fast path (no API
// call) → TTL-cached CollaboratorPermission rank >= min role. Dependency
// bots are never trusted (their issues must not spend triage runs either).
type authorTrust struct {
	ttl time.Duration
	mu  sync.Mutex
	// cache key: provider|repo|login (lowercased) → cached permission rank.
	cache map[string]authorTrustEntry
}

type authorTrustEntry struct {
	rank    int
	expires time.Time
}

const authorTrustDefaultTTL = 10 * time.Minute

func newAuthorTrust() *authorTrust {
	return &authorTrust{ttl: authorTrustDefaultTTL, cache: map[string]authorTrustEntry{}}
}

// authorTrustGate returns the server's shared author-trust cache, built on
// first use (Server has several construction paths; lazy init covers all).
func (s *Server) authorTrustGate() *authorTrust {
	s.authorTrustOnce.Do(func() { s.authorTrustG = newAuthorTrust() })
	return s.authorTrustG
}

// trustedAssociations are the GitHub author_association values that prove
// write-side rights without an API round trip. CONTRIBUTOR ("has previously
// committed") and NONE/FIRST_TIME_* are deliberately excluded — a merged
// typo fix must not grant auto-triage. COLLABORATOR can, on some repos,
// include read-only collaborators; the API fallback is the precise check,
// this fast path trades that edge for zero latency (threshold stays
// operator-configurable via minRole, which the API path honours exactly).
var trustedAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
}

// trusted classifies login for repo. assoc is the webhook payload's
// author_association ("" when the provider doesn't send one). pc is the
// provider admin client's optional permission capability (nil = no API
// fallback available → only the allowlist and assoc fast path can trust).
// minRole uses the gitlab vocabulary (empty → developer ≡ write), ranked by
// replierMinRoleRank — the same cross-forge scale as the command gate.
func (t *authorTrust) trusted(ctx context.Context, pc forge.PermissionClient, provider, repo, login, assoc, minRole string, allowlist []string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	if isDependencyBotAuthor(login) {
		return false
	}
	if prforgeInAllowlist(allowlist, login) {
		return true
	}
	if trustedAssociations[strings.ToUpper(strings.TrimSpace(assoc))] {
		return true
	}
	if pc == nil {
		return false
	}
	rank, ok := t.cachedRank(provider, repo, login)
	if !ok {
		perm, err := pc.CollaboratorPermission(ctx, repo, login)
		if err != nil {
			return false // unknown ⇒ untrusted (fail-closed)
		}
		rank = prforgePermRank(perm)
		t.storeRank(provider, repo, login, rank)
	}
	return rank >= replierMinRoleRank(minRole)
}

func (t *authorTrust) key(provider, repo, login string) string {
	return strings.ToLower(provider + "|" + repo + "|" + login)
}

func (t *authorTrust) cachedRank(provider, repo, login string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.cache[t.key(provider, repo, login)]
	if !ok || time.Now().After(e.expires) {
		return 0, false
	}
	return e.rank, true
}

func (t *authorTrust) storeRank(provider, repo, login string, rank int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cache[t.key(provider, repo, login)] = authorTrustEntry{rank: rank, expires: time.Now().Add(t.ttl)}
}
