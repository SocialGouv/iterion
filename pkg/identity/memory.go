package identity

import (
	"context"
	"sort"
	"sync"
)

// MemoryStore is an in-process Store backed by maps. It is the
// reference implementation used by tests in pkg/auth and pkg/server
// — keep its semantics in lock-step with mongo.go.
type MemoryStore struct {
	mu          sync.RWMutex
	users       map[string]User            // id → user
	emails      map[string]string          // normalized email → user id
	teams       map[string]Team            // id → team
	teamSlugs   map[string]string          // slug → team id
	memberships map[string]Membership      // user_id|team_id → membership
	invitations map[string]Invitation      // id → invitation
	invHash     map[string]string          // token_hash → invitation id
	oidc        map[string]OIDCLink        // provider|provider_user_id → link
	oidcByUser  map[string]map[string]bool // user_id → set of provider|provider_user_id keys
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:       make(map[string]User),
		emails:      make(map[string]string),
		teams:       make(map[string]Team),
		teamSlugs:   make(map[string]string),
		memberships: make(map[string]Membership),
		invitations: make(map[string]Invitation),
		invHash:     make(map[string]string),
		oidc:        make(map[string]OIDCLink),
		oidcByUser:  make(map[string]map[string]bool),
	}
}

func memberKey(userID, teamID string) string { return userID + "|" + teamID }
func oidcKey(provider, sub string) string    { return provider + "|" + sub }

func (m *MemoryStore) CreateUser(_ context.Context, u User) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	email := NormalizeEmail(u.Email)
	if _, ok := m.emails[email]; ok {
		return User{}, ErrEmailAlreadyTaken
	}
	u.Email = email
	m.users[u.ID] = u
	m.emails[email] = u.ID
	return u, nil
}

func (m *MemoryStore) GetUser(_ context.Context, id string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (m *MemoryStore) GetUserByEmail(_ context.Context, email string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.emails[NormalizeEmail(email)]
	if !ok {
		return User{}, ErrNotFound
	}
	return m.users[id], nil
}

func (m *MemoryStore) UpdateUser(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.users[u.ID]
	if !ok {
		return ErrNotFound
	}
	newEmail := NormalizeEmail(u.Email)
	if newEmail != cur.Email {
		if _, taken := m.emails[newEmail]; taken {
			return ErrEmailAlreadyTaken
		}
		delete(m.emails, cur.Email)
		m.emails[newEmail] = u.ID
	}
	u.Email = newEmail
	m.users[u.ID] = u
	return nil
}

func (m *MemoryStore) ListUsers(_ context.Context, page Page) ([]User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	users := make([]User, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].CreatedAt.Before(users[j].CreatedAt) })
	limit := page.Limit
	if limit <= 0 {
		limit = 50
	}
	if page.Offset >= len(users) {
		return nil, nil
	}
	end := page.Offset + limit
	if end > len(users) {
		end = len(users)
	}
	return users[page.Offset:end], nil
}

func (m *MemoryStore) UserCount(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.users)), nil
}

func (m *MemoryStore) CreateTeam(_ context.Context, t Team) (Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.teamSlugs[t.Slug]; ok {
		return Team{}, ErrSlugAlreadyTaken
	}
	m.teams[t.ID] = t
	m.teamSlugs[t.Slug] = t.ID
	return t, nil
}

func (m *MemoryStore) GetTeam(_ context.Context, id string) (Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.teams[id]
	if !ok {
		return Team{}, ErrNotFound
	}
	return t, nil
}

func (m *MemoryStore) GetTeamBySlug(_ context.Context, slug string) (Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.teamSlugs[slug]
	if !ok {
		return Team{}, ErrNotFound
	}
	return m.teams[id], nil
}

func (m *MemoryStore) UpdateTeam(_ context.Context, t Team) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.teams[t.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Slug != t.Slug {
		if _, taken := m.teamSlugs[t.Slug]; taken {
			return ErrSlugAlreadyTaken
		}
		delete(m.teamSlugs, cur.Slug)
		m.teamSlugs[t.Slug] = t.ID
	}
	m.teams[t.ID] = t
	return nil
}

func (m *MemoryStore) UpsertMembership(_ context.Context, mb Membership) error {
	if !mb.Role.Valid() {
		return ErrInvalidRole
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memberships[memberKey(mb.UserID, mb.TeamID)] = mb
	return nil
}

func (m *MemoryStore) GetMembership(_ context.Context, userID, teamID string) (Membership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mb, ok := m.memberships[memberKey(userID, teamID)]
	if !ok {
		return Membership{}, ErrNotFound
	}
	return mb, nil
}

func (m *MemoryStore) DeleteMembership(_ context.Context, userID, teamID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.memberships, memberKey(userID, teamID))
	return nil
}

func (m *MemoryStore) ListMembershipsByUser(_ context.Context, userID string) ([]Membership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Membership
	for _, mb := range m.memberships {
		if mb.UserID == userID {
			out = append(out, mb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JoinedAt.Before(out[j].JoinedAt) })
	return out, nil
}

func (m *MemoryStore) ListMembershipsByTeam(_ context.Context, teamID string) ([]Membership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Membership
	for _, mb := range m.memberships {
		if mb.TeamID == teamID {
			out = append(out, mb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JoinedAt.Before(out[j].JoinedAt) })
	return out, nil
}

func (m *MemoryStore) CreateInvitation(_ context.Context, inv Invitation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invitations[inv.ID] = inv
	if inv.TokenHash != "" {
		m.invHash[inv.TokenHash] = inv.ID
	}
	return nil
}

func (m *MemoryStore) GetInvitation(_ context.Context, id string) (Invitation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inv, ok := m.invitations[id]
	if !ok {
		return Invitation{}, ErrNotFound
	}
	return inv, nil
}

func (m *MemoryStore) GetInvitationByTokenHash(_ context.Context, tokenHash string) (Invitation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.invHash[tokenHash]
	if !ok {
		return Invitation{}, ErrNotFound
	}
	return m.invitations[id], nil
}

func (m *MemoryStore) UpdateInvitation(_ context.Context, inv Invitation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.invitations[inv.ID]; !ok {
		return ErrNotFound
	}
	m.invitations[inv.ID] = inv
	return nil
}

func (m *MemoryStore) DeleteInvitation(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invitations[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.invitations, id)
	if inv.TokenHash != "" {
		delete(m.invHash, inv.TokenHash)
	}
	return nil
}

func (m *MemoryStore) ListInvitationsByTeam(_ context.Context, teamID string) ([]Invitation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Invitation
	for _, inv := range m.invitations {
		if inv.TeamID == teamID {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryStore) UpsertOIDCLink(_ context.Context, link OIDCLink) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := oidcKey(link.Provider, link.ProviderUserID)
	m.oidc[k] = link
	if m.oidcByUser[link.UserID] == nil {
		m.oidcByUser[link.UserID] = make(map[string]bool)
	}
	m.oidcByUser[link.UserID][k] = true
	return nil
}

func (m *MemoryStore) GetOIDCLink(_ context.Context, provider, providerUserID string) (OIDCLink, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	link, ok := m.oidc[oidcKey(provider, providerUserID)]
	if !ok {
		return OIDCLink{}, ErrNotFound
	}
	return link, nil
}

func (m *MemoryStore) ListOIDCLinksByUser(_ context.Context, userID string) ([]OIDCLink, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []OIDCLink
	for k := range m.oidcByUser[userID] {
		out = append(out, m.oidc[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].ProviderUserID < out[j].ProviderUserID
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

func (m *MemoryStore) DeleteOIDCLink(_ context.Context, provider, providerUserID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := oidcKey(provider, providerUserID)
	link, ok := m.oidc[k]
	if !ok {
		return ErrNotFound
	}
	delete(m.oidc, k)
	if set := m.oidcByUser[link.UserID]; set != nil {
		delete(set, k)
		if len(set) == 0 {
			delete(m.oidcByUser, link.UserID)
		}
	}
	return nil
}
