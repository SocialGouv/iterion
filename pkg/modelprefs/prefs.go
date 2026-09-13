// Package modelprefs persists an operator's chosen model/backend/effort for a
// long-lived surface, so the choice survives the surface rather than being
// re-made every time it starts.
//
// The motivating case is the studio assistant: its model was pinned in the
// .bot and overridable only by a server-start environment variable — not per
// user, not per session, not visible anywhere. But nothing here knows about an
// assistant, or about any particular bot. A preference is keyed by an OPAQUE
// scope string the caller chooses (the studio passes a bot id), so a second
// conversational bot needs no engine change — which is exactly the coupling
// the engine is supposed to refuse.
package modelprefs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Pref is one user's model choice for one scope key within a tenant.
//
// An absent row means "no preference" — the caller falls back to the bot's own
// DSL defaults. That is deliberately different from a row with empty fields,
// which is how an operator says "go back to the default" without the record of
// having chosen disappearing.
type Pref struct {
	TenantID string `json:"-" bson:"tenant_id"`
	UserID   string `json:"-" bson:"user_id"`
	// Key is an opaque scope identifier owned by the caller. The engine never
	// interprets it.
	Key string `json:"key" bson:"key"`

	Model   string `json:"model,omitempty" bson:"model,omitempty"`
	Backend string `json:"backend,omitempty" bson:"backend,omitempty"`
	Effort  string `json:"effort,omitempty" bson:"effort,omitempty"`

	UpdatedAt time.Time `json:"updated_at,omitempty" bson:"updated_at,omitempty"`
}

// Empty reports whether the preference expresses no choice at all.
func (p Pref) Empty() bool {
	return p.Model == "" && p.Backend == "" && p.Effort == ""
}

const (
	// MaxKeyLength bounds one caller-controlled storage/index dimension.
	MaxKeyLength = 128
	// MaxPreferencesPerScope bounds distinct keys for one (tenant,user).
	// Existing keys remain freely updateable once the scope reaches the cap.
	MaxPreferencesPerScope = 64
)

var (
	// ErrInvalidKey is returned for a blank, oversized, or malformed scope key.
	ErrInvalidKey = errors.New("modelprefs: invalid preference key")
	// ErrTooManyPreferences is returned only when a NEW key would exceed the
	// per-(tenant,user) cardinality cap. Updating an existing key still works.
	ErrTooManyPreferences = errors.New("modelprefs: preference key limit reached")
)

// Keys are intentionally generic but storage-safe: bot slugs, catalog ids,
// and namespaced ids such as "catalog/copilot" fit; whitespace/control bytes
// and arbitrary JSON-like text do not become index keys.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// Store persists preferences per (tenant, user, key).
type Store interface {
	// Get returns nil, nil when no preference has been recorded.
	Get(ctx context.Context, tenantID, userID, key string) (*Pref, error)
	Set(ctx context.Context, p *Pref) error
	Delete(ctx context.Context, tenantID, userID, key string) error
}

// NormalizeKey trims and bounds a caller-supplied scope key.
func NormalizeKey(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return "", fmt.Errorf("%w: key is required", ErrInvalidKey)
	}
	if len(k) > MaxKeyLength {
		return "", fmt.Errorf("%w: key is %d bytes (maximum %d)", ErrInvalidKey, len(k), MaxKeyLength)
	}
	if !keyPattern.MatchString(k) {
		return "", fmt.Errorf("%w: %q must start with an alphanumeric character and contain only letters, digits, '.', '_', ':', '/', or '-'", ErrInvalidKey, k)
	}
	return k, nil
}

// nowUTC is the single timestamp source, so the stores stamp UpdatedAt alike.
func nowUTC() time.Time { return time.Now().UTC() }

func rowKey(tenantID, userID, key string) string {
	return tenantID + "\x00" + userID + "\x00" + key
}

func scopeAtLimit(rows map[string]Pref, tenantID, userID, key string) bool {
	if _, exists := rows[rowKey(tenantID, userID, key)]; exists {
		return false
	}
	n := 0
	for _, p := range rows {
		if p.TenantID == tenantID && p.UserID == userID {
			n++
		}
	}
	return n >= MaxPreferencesPerScope
}

// MemStore is the in-memory Store, used by tests and by any surface that
// deliberately wants the preference to die with the process.
type MemStore struct {
	mu   sync.RWMutex
	rows map[string]Pref
}

func NewMemStore() *MemStore { return &MemStore{rows: make(map[string]Pref)} }

func (m *MemStore) Get(_ context.Context, tenantID, userID, key string) (*Pref, error) {
	k, err := NormalizeKey(key)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.rows[rowKey(tenantID, userID, k)]; ok {
		cp := p
		return &cp, nil
	}
	return nil, nil
}

func (m *MemStore) Set(_ context.Context, p *Pref) error {
	k, err := NormalizeKey(p.Key)
	if err != nil {
		return err
	}
	row := *p
	row.Key = k
	row.UpdatedAt = nowUTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if scopeAtLimit(m.rows, p.TenantID, p.UserID, k) {
		return fmt.Errorf("%w: maximum %d keys per tenant/user", ErrTooManyPreferences, MaxPreferencesPerScope)
	}
	m.rows[rowKey(p.TenantID, p.UserID, k)] = row
	return nil
}

func (m *MemStore) Delete(_ context.Context, tenantID, userID, key string) error {
	k, err := NormalizeKey(key)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, rowKey(tenantID, userID, k))
	return nil
}
