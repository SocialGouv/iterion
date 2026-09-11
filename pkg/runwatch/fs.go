package runwatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type fileState struct {
	Watches  map[string]Watch   `json:"watches"`
	Episodes map[string]Episode `json:"episodes"`
}

// FSStore is the local single-control-plane implementation. The whole index
// is replaced atomically; watches are deliberately small and bounded while
// this makes every read/claim crash-safe without another local database.
type FSStore struct {
	path string
	mu   sync.Mutex
}

func NewFSStore(root string) *FSStore {
	return &FSStore{path: filepath.Join(root, "assistant-run-watches.json")}
}

func (s *FSStore) EnsureSchema(context.Context) error {
	return os.MkdirAll(filepath.Dir(s.path), 0o700)
}

func (s *FSStore) load() (fileState, error) {
	st := fileState{Watches: map[string]Watch{}, Episodes: map[string]Episode{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Watches == nil {
		st.Watches = map[string]Watch{}
	}
	if st.Episodes == nil {
		st.Episodes = map[string]Episode{}
	}
	return st, nil
}

func (s *FSStore) save(st fileState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(s.path, append(b, '\n'), 0o600)
}

func (s *FSStore) CreateWatch(_ context.Context, w Watch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	for _, ex := range st.Watches {
		if ex.State == WatchActive && ex.TenantID == w.TenantID && ex.OwnerID == w.OwnerID && ex.TargetRunID == w.TargetRunID {
			return ErrAlreadyExists
		}
	}
	st.Watches[w.ID] = w
	return s.save(st)
}

// ReconfigureActiveWatch changes only the fields owned by the watch request.
// Runtime delivery bookkeeping belongs to the existing watch and must survive
// a policy expansion: in particular, retaining LastObservedEventSeq lets the
// coordinator reconcile an already-recorded health event under the new kinds.
func (s *FSStore) ReconfigureActiveWatch(_ context.Context, requested Watch) (Watch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Watch{}, false, err
	}
	for id, existing := range st.Watches {
		if existing.State != WatchActive ||
			existing.TenantID != requested.TenantID ||
			existing.OwnerID != requested.OwnerID ||
			existing.TargetRunID != requested.TargetRunID ||
			existing.AssistantRunID != requested.AssistantRunID {
			continue
		}
		existing.Mode = requested.Mode
		existing.Kinds = append([]string(nil), requested.Kinds...)
		// Legacy episode limits are deliberately cleared on reconfiguration.
		// Delivery no longer reads this field, so older persisted positive
		// values become harmless without a bulk migration.
		existing.MaxEpisodes = 0
		existing.CooldownSeconds = requested.CooldownSeconds
		existing.UpdatedAt = requested.UpdatedAt
		st.Watches[id] = existing
		if err := s.save(st); err != nil {
			return Watch{}, false, err
		}
		return existing, true, nil
	}
	return Watch{}, false, nil
}

// TransferActiveWatch preserves one durable watch row while changing the
// assistant that receives future outcomes. The expected outgoing assistant is
// a compare-and-swap guard: another handoff or a normal reconfiguration must
// never be overwritten by a stale caller.
func (s *FSStore) TransferActiveWatch(_ context.Context, fromAssistantID string, requested Watch) (Watch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Watch{}, false, err
	}
	for id, existing := range st.Watches {
		if existing.State != WatchActive ||
			existing.TenantID != requested.TenantID ||
			existing.OwnerID != requested.OwnerID ||
			existing.TargetRunID != requested.TargetRunID ||
			existing.AssistantRunID != fromAssistantID {
			continue
		}
		existing.AssistantRunID = requested.AssistantRunID
		existing.Mode = requested.Mode
		existing.Kinds = append([]string(nil), requested.Kinds...)
		existing.MaxEpisodes = 0
		existing.CooldownSeconds = requested.CooldownSeconds
		existing.UpdatedAt = requested.UpdatedAt
		st.Watches[id] = existing
		if err := s.save(st); err != nil {
			return Watch{}, false, err
		}
		return existing, true, nil
	}
	return Watch{}, false, nil
}

func (s *FSStore) GetWatch(_ context.Context, id string) (Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Watch{}, err
	}
	w, ok := st.Watches[id]
	if !ok {
		return Watch{}, ErrNotFound
	}
	return w, nil
}

func (s *FSStore) watches(match func(Watch) bool, limit int) ([]Watch, error) {
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Watch, 0)
	for _, w := range st.Watches {
		if match(w) {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *FSStore) ListActiveByTarget(_ context.Context, tenant, id string) ([]Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches(func(w Watch) bool { return w.State == WatchActive && w.TenantID == tenant && w.TargetRunID == id }, 0)
}
func (s *FSStore) ListActiveByAssistant(_ context.Context, tenant, id string) ([]Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches(func(w Watch) bool { return w.State == WatchActive && w.TenantID == tenant && w.AssistantRunID == id }, 0)
}
func (s *FSStore) ListActive(_ context.Context, limit int) ([]Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches(func(w Watch) bool { return w.State == WatchActive }, limit)
}

func (s *FSStore) StopWatch(_ context.Context, id, tenant string, state WatchState, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	w, ok := st.Watches[id]
	if !ok || w.TenantID != tenant {
		return ErrNotFound
	}
	w.State, w.StopReason, w.UpdatedAt = state, reason, now
	st.Watches[id] = w
	return s.save(st)
}

// AdvanceObservedEventSeq is monotonic so two coordinators reconciling the
// same watch cannot move its durable health cursor backwards.
func (s *FSStore) AdvanceObservedEventSeq(_ context.Context, id, tenant string, seq int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	w, ok := st.Watches[id]
	if !ok || w.TenantID != tenant {
		return ErrNotFound
	}
	if seq <= w.LastObservedEventSeq {
		return nil
	}
	w.LastObservedEventSeq, w.UpdatedAt = seq, now
	st.Watches[id] = w
	return s.save(st)
}

// InitializeTreeTracking stamps the migration boundary exactly once. Two
// coordinators may race during a rolling deploy; the file mutex makes both
// callers observe the same persisted boundary.
func (s *FSStore) InitializeTreeTracking(_ context.Context, id, tenant string, started, now time.Time) (Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Watch{}, err
	}
	w, ok := st.Watches[id]
	if !ok || w.TenantID != tenant {
		return Watch{}, ErrNotFound
	}
	if w.TreeTrackingStartedAt == nil {
		stamp := started
		w.TreeTrackingStartedAt = &stamp
		w.UpdatedAt = now
		st.Watches[id] = w
		if err := s.save(st); err != nil {
			return Watch{}, err
		}
	}
	return w, nil
}

// EnsureRunObservation inserts one run cursor without replacing a cursor a
// competing coordinator already established.
func (s *FSStore) EnsureRunObservation(_ context.Context, id, tenant string, observation RunObservation, now time.Time) (RunObservation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return RunObservation{}, false, err
	}
	w, ok := st.Watches[id]
	if !ok || w.TenantID != tenant {
		return RunObservation{}, false, ErrNotFound
	}
	for _, existing := range w.Observations {
		if existing.RunID == observation.RunID {
			return existing, false, nil
		}
	}
	w.Observations = append(w.Observations, observation)
	w.UpdatedAt = now
	st.Watches[id] = w
	if err := s.save(st); err != nil {
		return RunObservation{}, false, err
	}
	return observation, true, nil
}

// AdvanceObservedRunEventSeq is monotonic independently for every run in the
// tree. The legacy root cursor advances with its observation for old readers.
func (s *FSStore) AdvanceObservedRunEventSeq(_ context.Context, id, tenant, runID string, seq int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	w, ok := st.Watches[id]
	if !ok || w.TenantID != tenant {
		return ErrNotFound
	}
	found := false
	for i := range w.Observations {
		if w.Observations[i].RunID != runID {
			continue
		}
		found = true
		if seq > w.Observations[i].EventSeq {
			w.Observations[i].EventSeq = seq
		}
		break
	}
	if !found {
		return ErrNotFound
	}
	if runID == w.TargetRunID && seq > w.LastObservedEventSeq {
		w.LastObservedEventSeq = seq
	}
	w.UpdatedAt = now
	st.Watches[id] = w
	return s.save(st)
}

func (s *FSStore) CreateEpisode(_ context.Context, ep Episode) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return false, err
	}
	if _, ok := st.Episodes[ep.ID]; ok {
		return false, nil
	}
	st.Episodes[ep.ID] = ep
	return true, s.save(st)
}

func (s *FSStore) ListEpisodesByWatch(_ context.Context, watchID, tenant string, limit int) ([]Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxEpisodeReadLimit {
		limit = maxEpisodeReadLimit
	}
	out := make([]Episode, 0)
	for _, ep := range st.Episodes {
		if ep.WatchID == watchID && ep.TenantID == tenant {
			out = append(out, ep)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *FSStore) ListDueEpisodes(_ context.Context, now time.Time, limit int) ([]Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Episode, 0)
	for _, ep := range st.Episodes {
		leaseExpired := ep.LeaseUntil == nil || !ep.LeaseUntil.After(now)
		if (ep.State == EpisodePending || ep.State == EpisodeProcessing && leaseExpired) && !ep.NextAttemptAt.After(now) {
			out = append(out, ep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextAttemptAt.Before(out[j].NextAttemptAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *FSStore) ClaimEpisode(_ context.Context, id, owner string, now time.Time, lease time.Duration) (Episode, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Episode{}, false, err
	}
	ep, ok := st.Episodes[id]
	if !ok {
		return Episode{}, false, ErrNotFound
	}
	if ep.State != EpisodePending && (ep.State != EpisodeProcessing || (ep.LeaseUntil != nil && ep.LeaseUntil.After(now))) {
		return ep, false, nil
	}
	until := now.Add(lease)
	ep.State, ep.LeaseOwner, ep.LeaseUntil, ep.UpdatedAt = EpisodeProcessing, owner, &until, now
	ep.Attempts++
	st.Episodes[id] = ep
	return ep, true, s.save(st)
}

func (s *FSStore) ReleaseEpisode(_ context.Context, id, owner string, next time.Time, msg string) error {
	return s.finish(id, owner, EpisodePending, next, msg, false)
}
func (s *FSStore) BlockEpisode(_ context.Context, id, owner string, now time.Time, msg string) error {
	return s.finish(id, owner, EpisodeBlocked, now, msg, false)
}
func (s *FSStore) CompleteEpisode(_ context.Context, id, owner string, now time.Time) error {
	return s.finish(id, owner, EpisodeDone, now, "", true)
}

func (s *FSStore) finish(id, owner string, state EpisodeState, at time.Time, msg string, delivered bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	ep, ok := st.Episodes[id]
	if !ok {
		return ErrNotFound
	}
	if ep.State != EpisodeProcessing || ep.LeaseOwner != owner {
		return ErrNotFound
	}
	ep.State, ep.LeaseOwner, ep.LeaseUntil, ep.NextAttemptAt, ep.LastError, ep.UpdatedAt = state, "", nil, at, msg, at
	if delivered {
		ep.DeliveredAt = &at
		w := st.Watches[ep.WatchID]
		w.DeliveredEpisodes++
		w.LastDeliveredAt = &at
		w.UpdatedAt = at
		st.Watches[w.ID] = w
	}
	st.Episodes[id] = ep
	return s.save(st)
}

var _ Store = (*FSStore)(nil)
