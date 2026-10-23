package trigger

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type scheduleState struct {
	NextFire map[string]time.Time       `json:"next_fire"`
	Pending  map[string]*scheduleIntent `json:"pending"`
}

func (s *Scheduler) loadScheduleState() error {
	if s.stateFile == "" || s.stateLoaded {
		return nil
	}
	raw, err := os.ReadFile(s.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		s.stateLoaded = true
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
	s.stateLoaded = true
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
