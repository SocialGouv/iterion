package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/gofrs/flock"
)

type scheduleState struct {
	NextFire map[string]time.Time       `json:"next_fire"`
	Pending  map[string]*scheduleIntent `json:"pending"`
}

func (s *Scheduler) loadScheduleState() error {
	if s.stateFile == "" {
		return nil
	}
	raw, err := os.ReadFile(s.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state scheduleState
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if state.NextFire != nil {
		s.nextFire = state.NextFire
	}
	if state.Pending != nil {
		s.pending = state.Pending
	}
	return nil
}
func (s *Scheduler) saveScheduleState() error {
	if s.stateFile == "" {
		return nil
	}
	s.mu.Lock()
	raw, err := json.Marshal(scheduleState{NextFire: s.nextFire, Pending: s.pending})
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(s.stateFile, raw, 0600)
}

// The journal is one atomic file: lock before reloading it and retain exclusion
// until every launch and outcome write finishes. OS locks disappear on crash.
func (s *Scheduler) lockScheduleState(ctx context.Context) (func(), error) {
	if s.stateFile == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.stateFile), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(s.stateFile + ".lock")
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	return func() {
		if err := lock.Unlock(); err != nil {
			s.warn("scheduler: unlock intentions: %v", err)
		}
	}, nil
}
