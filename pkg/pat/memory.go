package pat

import (
	"context"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// MemoryStore is the in-process PAT store for tests and local mode.
// Keep semantics in lock-step with MongoStore.
type MemoryStore struct {
	kit *storekit.Memory[Token]
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{kit: storekit.NewMemory[Token](ErrNotFound)}
}

func (s *MemoryStore) Create(_ context.Context, t Token) error {
	s.kit.Put(t.ID, t)
	return nil
}

func (s *MemoryStore) GetByTokenHash(_ context.Context, hash string) (Token, error) {
	return s.kit.Find(func(t Token) bool { return t.TokenHash == hash })
}

func (s *MemoryStore) Get(_ context.Context, id string) (Token, error) {
	return s.kit.Get(id)
}

func (s *MemoryStore) ListByUser(_ context.Context, userID string) ([]Token, error) {
	out := s.kit.List(func(t Token) bool { return t.UserID == userID })
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) Revoke(_ context.Context, id string, at time.Time) error {
	_, err := s.kit.Mutate(id, func(t *Token) bool {
		if t.RevokedAt != nil {
			return false // the first instant stands
		}
		t.RevokedAt = &at
		return true
	})
	return err
}

func (s *MemoryStore) RevokeIfLive(_ context.Context, id string, at time.Time) (bool, error) {
	return s.kit.Mutate(id, func(t *Token) bool {
		if t.RevokedAt != nil {
			return false
		}
		t.RevokedAt = &at
		return true
	})
}

func (s *MemoryStore) RevokeAllByUser(_ context.Context, userID string, at time.Time) (int64, error) {
	var n int64
	for _, t := range s.kit.List(func(t Token) bool { return t.UserID == userID && t.RevokedAt == nil }) {
		changed, err := s.kit.Mutate(t.ID, func(t *Token) bool {
			if t.RevokedAt != nil {
				return false
			}
			t.RevokedAt = &at
			return true
		})
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	return n, nil
}

func (s *MemoryStore) Disable(_ context.Context, id string, at time.Time) error {
	revoked := false
	_, err := s.kit.Mutate(id, func(t *Token) bool {
		if t.RevokedAt != nil {
			revoked = true
			return false
		}
		if t.DisabledAt != nil {
			return false // the first instant stands
		}
		t.DisabledAt = &at
		return true
	})
	if err == nil && revoked {
		return ErrRevoked
	}
	return err
}

func (s *MemoryStore) Enable(_ context.Context, id string) error {
	revoked := false
	_, err := s.kit.Mutate(id, func(t *Token) bool {
		if t.RevokedAt != nil {
			revoked = true
			return false
		}
		if t.DisabledAt == nil {
			return false
		}
		t.DisabledAt = nil
		return true
	})
	if err == nil && revoked {
		return ErrRevoked
	}
	return err
}

func (s *MemoryStore) Purge(_ context.Context, id string) error {
	purged := false
	s.kit.DeleteWhere(func(t Token) bool {
		if t.ID == id && t.RevokedAt != nil {
			purged = true
			return true
		}
		return false
	})
	if purged {
		return nil
	}
	if _, err := s.kit.Get(id); err != nil {
		return err
	}
	return ErrNotRevoked
}

func (s *MemoryStore) MarkUsed(_ context.Context, id string, at time.Time) error {
	_, err := s.kit.Mutate(id, func(t *Token) bool {
		t.LastUsedAt = &at
		return true
	})
	return err
}
