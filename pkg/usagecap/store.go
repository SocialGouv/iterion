package usagecap

import (
	"context"
	"strings"
	"sync"
)

// Store shares what one process learned about a credential's usage windows
// with every other process that might spend it.
//
// Without it the guard still works, but only from inside a run: each pod
// discovers the wall by hitting it once. With it, a pod that is about to
// start work asks first and parks for free.
//
// Implementations must be safe for concurrent use and must not let an
// out-of-order write regress a window to an older reading — several pods
// observe the same credential at once, and the network does not preserve
// order.
type Store interface {
	// Record stores a reading for a credential key, keeping the newest
	// per window.
	Record(ctx context.Context, key string, r Reading) error
	// Latest returns the newest reading per window for a credential key.
	// An unknown key is not an error: it means "nothing learned yet",
	// which must read as "not blocked".
	Latest(ctx context.Context, key string) ([]Reading, error)
}

// Key identifies the credential whose windows a reading describes.
//
// The scope is what keeps one tenant's BYOK subscription from blocking
// another's: readings only merge when they genuinely come from the same
// meter. Runs that fall back to the deployment's own credential share
// ScopePlatform, and merging THOSE is not a compromise but the point — they
// really are one subscription.
//
// credFP, when known, is the audit fingerprint of the credential itself
// (secrets.FingerprintSHA256). It is what makes the meter follow the
// CREDENTIAL and not the slot: a rotated token opens a fresh key, so a
// seven-day reading recorded against the old account — legitimately fresh
// until its own reset instant — cannot park runs that no longer draw on
// it. Mesure : une clé neuve posée sur une team est restée bloquée des
// jours par le reading à 95% de la clé qu'elle remplaçait. Readings
// recorded under a fingerprint-less key simply expire in place.
func Key(backend, scope, credFP string) string {
	backend = strings.TrimSpace(backend)
	scope = strings.TrimSpace(scope)
	if backend == "" {
		backend = "unknown"
	}
	if scope == "" {
		scope = ScopePlatform
	}
	k := backend + "|" + scope
	if fp := strings.TrimSpace(credFP); fp != "" {
		k += "|fp:" + fp
	}
	return k
}

const (
	// ScopePlatform is the deployment's own credential — the env-provided
	// subscription every run falls back to.
	ScopePlatform = "platform"
	// ScopeLocal is a single-process CLI run.
	ScopeLocal = "local"
	// ScopeTenantPrefix prefixes a tenant that brought its own credential.
	ScopeTenantPrefix = "tenant:"
)

// TenantScope builds the scope for a tenant's own credential.
func TenantScope(tenantID string) string {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return ScopePlatform
	}
	return ScopeTenantPrefix + tenantID
}

// MemStore is an in-process Store: the local CLI's whole world, and the
// tests' substitute for Mongo.
type MemStore struct {
	mu   sync.RWMutex
	data map[string]map[Window]Reading
}

// NewMemStore builds an empty in-process store.
func NewMemStore() *MemStore {
	return &MemStore{data: map[string]map[Window]Reading{}}
}

// Record keeps the newest reading per window.
func (s *MemStore) Record(_ context.Context, key string, r Reading) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	byWindow, ok := s.data[key]
	if !ok {
		byWindow = map[Window]Reading{}
		s.data[key] = byWindow
	}
	if prev, ok := byWindow[r.Window]; ok && prev.ObservedAt.After(r.ObservedAt) {
		return nil
	}
	byWindow[r.Window] = r
	return nil
}

// Latest returns the newest reading per window.
func (s *MemStore) Latest(_ context.Context, key string) ([]Reading, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byWindow := s.data[key]
	out := make([]Reading, 0, len(byWindow))
	for _, r := range byWindow {
		out = append(out, r)
	}
	return out, nil
}
