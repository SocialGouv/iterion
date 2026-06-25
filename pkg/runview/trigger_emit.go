package runview

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// emitRunCompletion publishes a run-completion trigger.Event when a run reaches
// a terminal state — the "runned by iterion" source that lets a finished or
// failed run fire downstream runs (pipelines, on-failure escalation). It is a
// best-effort no-op unless an event publisher is wired, and never blocks the
// run goroutine on a slow consumer (the bus fan-out is itself lossy).
//
// The kind is derived from bodyErr (the engine already wrote the authoritative
// status to the run record, re-read here to enrich the event with repo/bot for
// matching and templating). A load failure still emits the bare event so a
// downstream subscription keyed only on source/kind/bot still fires.
func (s *Service) emitRunCompletion(runID string, bodyErr error) {
	if s.eventPublisher == nil {
		return
	}
	kind := trigger.KindRunFinished
	switch {
	case errors.Is(bodyErr, runtime.ErrRunCancelled):
		kind = trigger.KindRunCancelled
	case bodyErr != nil:
		kind = trigger.KindRunFailed
	}

	fctx := store.WithoutTenantFilter(context.Background())
	var repo, botID, status, name string
	if r, err := s.store.LoadRun(fctx, runID); err == nil && r != nil {
		repo = r.ProjectPath
		botID = r.BotID
		status = string(r.Status)
		name = r.Name
		if name == "" {
			name = r.WorkflowName
		}
		// Trust the persisted terminal status over the derived kind when it
		// is unambiguous (the engine is the source of truth).
		switch r.Status {
		case store.RunStatusFinished:
			kind = trigger.KindRunFinished
		case store.RunStatusFailed, store.RunStatusFailedResumable:
			kind = trigger.KindRunFailed
		case store.RunStatusCancelled:
			kind = trigger.KindRunCancelled
		}
	}

	ev := trigger.Event{
		ID:         "run:" + runID,
		Source:     trigger.SourceRun,
		Kind:       kind,
		Repo:       repo,
		Subject:    trigger.Subject{Type: "run", ID: runID, Title: name, State: status},
		Actor:      botID,
		Payload:    map[string]any{"bot_id": botID, "status": status, "run_id": runID},
		OccurredAt: time.Now().UTC(),
	}
	_ = s.eventPublisher.Publish(fctx, ev)
}
