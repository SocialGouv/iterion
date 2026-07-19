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
		t.RevokedAt = &at
		return true
	})
	return err
}

func (s *MemoryStore) MarkUsed(_ context.Context, id string, at time.Time) error {
	_, err := s.kit.Mutate(id, func(t *Token) bool {
		t.LastUsedAt = &at
		return true
	})
	return err
}
