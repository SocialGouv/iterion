package assistantmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type fsState struct {
	Missions map[string]Mission `json:"missions"`
}

// FSStore is process-safe as well as goroutine-safe. Every read/modify/write
// is protected by the same OS lock and persisted with an atomic rename.
type FSStore struct {
	path string
	mu   sync.Mutex
}

func NewFSStore(root string) *FSStore {
	return &FSStore{path: filepath.Join(root, "assistant-missions.json")}
}

func (s *FSStore) EnsureSchema(context.Context) error {
	return os.MkdirAll(filepath.Dir(s.path), 0o700)
}

func (s *FSStore) withState(ctx context.Context, write bool, fn func(*fsState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	var lock store.RunLock
	for {
		var err error
		lock, err = store.AcquireFileLock(s.path+".lock", "assistant missions")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("assistantmission: acquire file lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer func() { _ = lock.Unlock() }()
	st := fsState{Missions: map[string]Mission{}}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if st.Missions == nil {
		st.Missions = map[string]Mission{}
	}
	if err := fn(&st); err != nil || !write {
		return err
	}
	b, err = json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(s.path, append(b, '\n'), 0o600)
}

func (s *FSStore) CreateOrGet(ctx context.Context, requested Mission) (Mission, bool, error) {
	var out Mission
	created := false
	err := s.withState(ctx, true, func(st *fsState) error {
		for _, existing := range st.Missions {
			if existing.TenantID == requested.TenantID && existing.OperatorID == requested.OperatorID && existing.InvocationKey == requested.InvocationKey {
				if !existing.SameRequest(requested) {
					return ErrConflict
				}
				out = existing
				return nil
			}
			if !existing.State.Terminal() && existing.TenantID == requested.TenantID && existing.TargetRunID == requested.TargetRunID {
				return ErrConflict
			}
		}
		requested.Policy = requested.Policy.Canonical()
		requested.ActiveTargetKey = requested.TenantID + "\x00" + requested.TargetRunID
		st.Missions[requested.ID] = requested
		out, created = requested, true
		return nil
	})
	return out, created, err
}

func scopeMatches(m Mission, scope Scope) bool {
	return m.TenantID == scope.TenantID && m.OperatorID == scope.OperatorID && (scope.TargetRunID == "" || m.TargetRunID == scope.TargetRunID)
}

func (s *FSStore) Get(ctx context.Context, scope Scope, id string) (Mission, error) {
	var out Mission
	err := s.withState(ctx, false, func(st *fsState) error {
		m, ok := st.Missions[id]
		if !ok || !scopeMatches(m, scope) {
			return ErrNotFound
		}
		out = m
		return nil
	})
	return out, err
}

func (s *FSStore) GetByInvocation(ctx context.Context, scope Scope, key string) (Mission, error) {
	var out Mission
	err := s.withState(ctx, false, func(st *fsState) error {
		for _, m := range st.Missions {
			if scopeMatches(m, scope) && m.InvocationKey == key {
				out = m
				return nil
			}
		}
		return ErrNotFound
	})
	return out, err
}

func (s *FSStore) List(ctx context.Context, scope Scope, limit int) ([]Mission, error) {
	var out []Mission
	err := s.withState(ctx, false, func(st *fsState) error {
		for _, m := range st.Missions {
			if scopeMatches(m, scope) {
				out = append(out, m)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

func (s *FSStore) ListReconcileCandidates(ctx context.Context, _ time.Time, limit int) ([]Mission, error) {
	var out []Mission
	err := s.withState(ctx, false, func(st *fsState) error {
		for _, m := range st.Missions {
			if !m.State.Terminal() {
				out = append(out, m)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

func (s *FSStore) Claim(ctx context.Context, id, owner string, now time.Time, lease time.Duration) (Mission, bool, error) {
	var out Mission
	claimed := false
	err := s.withState(ctx, true, func(st *fsState) error {
		m, ok := st.Missions[id]
		if !ok {
			return ErrNotFound
		}
		if m.State.Terminal() || (m.LeaseUntil != nil && m.LeaseUntil.After(now) && m.LeaseOwner != owner) {
			out = m
			return nil
		}
		until := now.Add(lease)
		m.LeaseOwner, m.LeaseUntil, m.UpdatedAt = owner, &until, now
		m.LeaseEpoch++
		m.Revision++
		st.Missions[id] = m
		out, claimed = m, true
		return nil
	})
	return out, claimed, err
}

func (s *FSStore) UpdateClaimed(ctx context.Context, next Mission, owner string) (Mission, error) {
	var out Mission
	err := s.withState(ctx, true, func(st *fsState) error {
		current, ok := st.Missions[next.ID]
		if !ok {
			return ErrNotFound
		}
		if current.LeaseOwner != owner || current.LeaseEpoch != next.LeaseEpoch || current.Revision != next.Revision {
			return ErrClaimLost
		}
		if !current.SameRequest(next) || current.CreatedAt != next.CreatedAt || current.InvocationKey != next.InvocationKey {
			return ErrConflict
		}
		next.Revision++
		if next.State.Terminal() {
			next.ActiveTargetKey, next.LeaseOwner, next.LeaseUntil = "", "", nil
		}
		st.Missions[next.ID] = next
		out = next
		return nil
	})
	return out, err
}

func (s *FSStore) RequestStop(ctx context.Context, scope Scope, id, reason string, now time.Time) (Mission, error) {
	var out Mission
	err := s.withState(ctx, true, func(st *fsState) error {
		m, ok := st.Missions[id]
		if !ok || !scopeMatches(m, scope) {
			return ErrNotFound
		}
		if !m.State.Terminal() {
			m.State, m.Reason, m.UpdatedAt, m.ActiveTargetKey = StateStopped, reason, now, ""
			m.TerminalAt, m.LeaseOwner, m.LeaseUntil = &now, "", nil
			m.Revision++
			st.Missions[id] = m
		}
		out = m
		return nil
	})
	return out, err
}

var _ Store = (*FSStore)(nil)
