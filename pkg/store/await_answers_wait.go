package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AwaitAnswersWait is an active, bounded sync point, not an outstanding
// question. Its execution token separates parallel and repeated invocations.
type AwaitAnswersWait struct {
	NodeID string    `json:"node_id" bson:"node_id"`
	Until  time.Time `json:"until" bson:"until"`
}

// ValidateAwaitAnswersWait keeps execution tokens safe as map keys on every
// backend. A past expiry is valid data, but it never exempts a run from stalls.
func ValidateAwaitAnswersWait(token string, wait *AwaitAnswersWait) error {
	if token == "" || strings.ContainsAny(token, ".$\x00") {
		return fmt.Errorf("invalid await_answers execution token %q", token)
	}
	if wait != nil && (wait.NodeID == "" || wait.Until.IsZero()) {
		return fmt.Errorf("await_answers wait requires a node and expiry")
	}
	return nil
}

func (s *FilesystemRunStore) SetAwaitAnswersWait(_ context.Context, runID, token string, wait *AwaitAnswersWait) error {
	if err := ValidateAwaitAnswersWait(token, wait); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.loadRunRaw(runID)
	if err != nil {
		return err
	}
	if wait != nil {
		if run.Status != RunStatusRunning {
			return fmt.Errorf("cannot park await_answers on run %s in state %s", runID, run.Status)
		}
		if run.AwaitAnswersWaits == nil {
			run.AwaitAnswersWaits = make(map[string]AwaitAnswersWait)
		}
		run.AwaitAnswersWaits[token] = *wait
	} else {
		delete(run.AwaitAnswersWaits, token)
	}
	run.UpdatedAt = time.Now().UTC()
	return s.writeRun(run)
}
