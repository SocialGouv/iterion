package connection

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store persists connections.
//
// Every read takes the tenant as its FIRST argument, and that is the whole
// design. `forge.ConnectionStore.Get(ctx, id)` filters on `_id` alone, so any
// caller holding an id — a run, a webhook, an API path parameter — reads any
// tenant's connection, and nothing at the call site looks wrong. Making the
// tenant positional means a caller cannot forget it: they must produce one,
// and producing the wrong one is a mistake a reviewer can see.
//
// A connection belonging to another tenant answers ErrNotFound, never a
// distinguishable "forbidden": to an untrusted caller the two must read the
// same, or the error itself enumerates which ids exist.
type Store interface {
	Create(ctx context.Context, c Connection) error
	// Get returns one connection of tenantID.
	Get(ctx context.Context, tenantID, id string) (Connection, error)
	// ByAlias resolves the name a `.bot` writes in `connection:`. Scoped to
	// one connector as well as one tenant, so two packages may each have a
	// connection called "main".
	ByAlias(ctx context.Context, tenantID, connector, alias string) (Connection, error)
	// List returns a tenant's connections, oldest first. Pass an empty
	// connector for all of them.
	List(ctx context.Context, tenantID, connector string) ([]Connection, error)
	Update(ctx context.Context, c Connection) error
	Delete(ctx context.Context, tenantID, id string) error
}

// MemoryStore is the in-process Store: the local CLI and desktop tier, and
// every test's. Its cloud twin is the Mongo implementation, which lands with
// the same conformance suite — a durable seam that ships filesystem-only is a
// cloud hole rather than a known limitation.
type MemoryStore struct {
	mu    sync.RWMutex
	byID  map[string]Connection
	clock func() time.Time
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[string]Connection{}, clock: time.Now}
}

// WithClock pins the clock, so a test can assert on timestamps.
func (s *MemoryStore) WithClock(now func() time.Time) *MemoryStore {
	s.clock = now
	return s
}

// detach returns a copy whose slice fields share no backing array with c.
//
// MemoryStore hands out and takes in Connection VALUES, and a value carries
// slice headers: without this, a caller's `Capabilities` becomes the store's
// backing array on Create, and the store's becomes the caller's on every read.
// Either side mutating its own copy then rewrites the other's record with no
// Update and no lock — a capability grant changing underneath the check that
// reads it.
//
// FileStore is immune for a reason that is an accident of its implementation
// rather than a shared rule: it re-decodes the file on every access, so every
// value it returns is freshly allocated. Stating the rule here is what keeps
// the two twins answering identically, which is the axis the conformance suite
// exists for — and what the Mongo twin (also decoding per read) must not
// quietly diverge from either.
func detach(c Connection) Connection {
	c.Capabilities = append([]Capability(nil), c.Capabilities...)
	c.GrantedScopes = append([]string(nil), c.GrantedScopes...)
	c.SealedPayload = append([]byte(nil), c.SealedPayload...)
	return c
}

func (s *MemoryStore) Create(_ context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c = detach(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[c.ID]; exists {
		return ErrExists
	}
	if err := checkAliasFree(s.byID, c, ""); err != nil {
		return err
	}
	now := s.clock()
	c.CreatedAt, c.UpdatedAt = now, now
	s.byID[c.ID] = c
	return nil
}

func (s *MemoryStore) Get(_ context.Context, tenantID, id string) (Connection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.byID[id]
	// The tenant check is the point of the signature. A record that exists but
	// belongs elsewhere is NOT FOUND, not forbidden.
	if !ok || c.TenantID != tenantID {
		return Connection{}, ErrNotFound
	}
	return detach(c), nil
}

func (s *MemoryStore) ByAlias(_ context.Context, tenantID, connector, alias string) (Connection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.byID {
		if c.TenantID == tenantID && c.Connector == connector && aliasEq(c.Alias, alias) {
			return detach(c), nil
		}
	}
	return Connection{}, ErrNotFound
}

func (s *MemoryStore) List(_ context.Context, tenantID, connector string) ([]Connection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Connection
	for _, c := range s.byID {
		if c.TenantID != tenantID {
			continue
		}
		if connector != "" && c.Connector != connector {
			continue
		}
		out = append(out, detach(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryStore) Update(_ context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c = detach(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.byID[c.ID]
	if !ok {
		return ErrNotFound
	}
	// An update may not move a connection between tenants: that would be a
	// silent grant of one tenant's credential to another, written as an
	// ordinary edit.
	if prev.TenantID != c.TenantID {
		return ErrNotFound
	}
	// The alias is checked on UPDATE as well as on create, which it was not.
	// Renaming one connection onto another's alias left two answering the same
	// name, and `ByAlias` returns the first map match — so identical workflow
	// input selected a different credential, or a different instance, between
	// two runs. A uniqueness rule enforced only at creation is not one.
	if err := checkAliasFree(s.byID, c, c.ID); err != nil {
		return err
	}
	c.SealedPayload = keptCredential(prev, c)
	c.CreatedAt = prev.CreatedAt
	c.UpdatedAt = s.clock()
	s.byID[c.ID] = c
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok || c.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// aliasEq compares aliases case-insensitively and ignoring surrounding space.
// An operator who created "Main" and a `.bot` that writes "main" mean the same
// connection, and the failure of the alternative is a run that cannot find a
// credential that is plainly there.
//
// The normalisation is what makes the uniqueness rule meaningful: without it
// "main", "MAIN" and " main " are three aliases that all answer to one lookup.
func aliasEq(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// checkAliasFree refuses an alias another connection of the same tenant and
// connector already answers to. `exceptID` is the record being updated, which
// may of course keep its own alias.
//
// Shared by create and update because it was enforced on create only, and a
// rename onto an existing alias left two records answering one name — with
// `ByAlias` returning whichever the map iteration reached first.
// keptCredential answers what an update leaves sealed on the record.
//
// An update is a full replace, and `Connection.SealedPayload` is `json:"-"`
// — deliberately, since a credential has no business in an API response. So
// any caller that rebuilds the record from the TRANSPORT shape (the studio's
// PATCH, which is what this exported interface exists for) hands back a
// record with no payload at all, and a replace would write the credential
// away: the connection survives, `connections list` still shows it active,
// and it 401s at its next call with an error that reads like a bad token.
// That is the exact failure fileRecord was introduced to prevent, on the
// update path instead of the write path.
//
// So an absent payload means "unchanged", and rotating one means SENDING one.
// A credential is never removed by omission, which no caller can mean:
// unbinding a connection is Delete, and suspending it is Status.
func keptCredential(prev, next Connection) []byte {
	if len(next.SealedPayload) == 0 {
		return prev.SealedPayload
	}
	return next.SealedPayload
}

func checkAliasFree(all map[string]Connection, c Connection, exceptID string) error {
	for id, other := range all {
		if id == exceptID {
			continue
		}
		if other.TenantID == c.TenantID && other.Connector == c.Connector && aliasEq(other.Alias, c.Alias) {
			return ErrExists
		}
	}
	return nil
}
