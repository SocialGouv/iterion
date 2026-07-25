package usernotify

import (
	"context"
	"sync"
)

// Scope selects which runs a user is notified about.
type Scope string

const (
	// ScopeOwn (the default when no prefs row exists): only runs the user
	// launched.
	ScopeOwn Scope = "own"
	// ScopeTeam: every run of the user's active team.
	ScopeTeam Scope = "team"
)

// ValidScope reports whether s is an accepted preference value.
func ValidScope(s Scope) bool { return s == ScopeOwn || s == ScopeTeam }

// Prefs is one user's notification preference within a team. Rows are only
// created when a user changes the default, so absence means ScopeOwn.
type Prefs struct {
	TenantID string `json:"tenant_id" bson:"tenant_id"`
	UserID   string `json:"user_id" bson:"user_id"`
	Scope    Scope  `json:"scope" bson:"scope"`
}

// PrefsStore persists per-user notification preferences.
type PrefsStore interface {
	Get(ctx context.Context, tenantID, userID string) (*Prefs, error) // nil, nil when absent
	Set(ctx context.Context, p *Prefs) error
	// ListTeamWide returns the user IDs in tenantID that opted into
	// ScopeTeam.
	ListTeamWide(ctx context.Context, tenantID string) ([]string, error)
}

// MemPrefsStore is the in-memory PrefsStore for tests and local mode.
type MemPrefsStore struct {
	mu   sync.RWMutex
	rows map[string]Prefs // key tenant\x00user
}

func NewMemPrefsStore() *MemPrefsStore {
	return &MemPrefsStore{rows: make(map[string]Prefs)}
}

func prefsKey(tenantID, userID string) string { return tenantID + "\x00" + userID }

func (m *MemPrefsStore) Get(_ context.Context, tenantID, userID string) (*Prefs, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.rows[prefsKey(tenantID, userID)]; ok {
		cp := p
		return &cp, nil
	}
	return nil, nil
}

func (m *MemPrefsStore) Set(_ context.Context, p *Prefs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[prefsKey(p.TenantID, p.UserID)] = *p
	return nil
}

func (m *MemPrefsStore) ListTeamWide(_ context.Context, tenantID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var users []string
	for _, p := range m.rows {
		if p.TenantID == tenantID && p.Scope == ScopeTeam {
			users = append(users, p.UserID)
		}
	}
	return users, nil
}
