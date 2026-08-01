package runview

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// pipelineSchedulerInterval is the backstop poll cadence for the pipeline
// scheduler. The primary wakeup is the queue's signal (a slot freed or an
// item enqueued); the ticker only catches a missed signal or a restart
// backlog, so it can be lazy.
const pipelineSchedulerInterval = 5 * time.Second

// enqueuePipeline parks an over-limit root launch. The in-memory FIFO
// entry was already added by admitOrEnqueue; here we persist a queued run
// doc so the board renders the waiting pipeline in TODO and it survives a
// restart. Best-effort: if the store can't create a queued doc, the FIFO
// entry still starts the run later (the engine creates the doc on pickup).
func (s *Service) enqueuePipeline(parent context.Context, runID string, spec LaunchSpec, position int) (*LaunchResult, error) {
	if qc := store.AsQueuedRunCreator(s.store); qc != nil {
		wfName := ""
		if wf, _, cErr := compileForLaunch(spec.FilePath, spec.Source); cErr == nil {
			wfName = wf.Name
		}
		created, err := qc.CreateQueuedRun(parent, runID, wfName, spec.FilePath, spec.BotID, varsToInputs(spec.Vars))
		if err != nil {
			// The FIFO entry is then memory-only: rebuildPipelineQueue reads
			// the store on restart, so this queued launch is LOST if the
			// server restarts before a slot frees.
			s.logger.Warn("runview: persist queued pipeline %s: %v (queue entry is memory-only — lost on server restart)", runID, err)
		} else {
			created.Name = store.GenerateRunName(spec.FilePath + ":" + runID)
			if err := s.store.SaveRun(parent, created); err != nil {
				s.logger.Warn("runview: name queued pipeline %s: %v", runID, err)
			}
		}
	}
	s.logger.Info("runview: pipeline %s queued at position %d (concurrency cap %d reached)", runID, position, s.maxConcurrentPipelines)
	closed := make(chan struct{})
	close(closed)
	return &LaunchResult{RunID: runID, Done: closed, QueuePosition: position}, nil
}

// startPipelineScheduler runs the goroutine that admits queued roots as
// slots free. No-op when the concurrency cap is disabled.
func (s *Service) startPipelineScheduler() {
	if s.pipelineQueue == nil {
		return
	}
	s.pipelineStop = make(chan struct{})
	go func() {
		t := time.NewTicker(pipelineSchedulerInterval)
		defer t.Stop()
		for {
			select {
			case <-s.pipelineStop:
				return
			case <-s.pipelineQueue.wake:
			case <-t.C:
			}
			if s.draining.Load() {
				return
			}
			for _, it := range s.pipelineQueue.dequeueReady() {
				// Contain a per-item launch panic so one poisoned queue
				// entry cannot kill the scheduler and strand every
				// waiting pipeline behind it.
				func() {
					defer func() {
						if r := recover(); r != nil && s.logger != nil {
							s.logger.Error("runview: PANIC starting queued pipeline %s: %v\n%s", it.runID, r, debug.Stack())
						}
					}()
					s.startQueuedRun(it)
				}()
			}
		}
	}()
}

// stopPipelineScheduler ends the scheduler goroutine. Idempotent (Stop
// and Drain may both run in one teardown). Queued pipelines stay
// persisted as queued docs and are recovered on the next boot, so
// stopping the scheduler never strands them.
func (s *Service) stopPipelineScheduler() {
	s.pipelineStopOnce.Do(func() {
		if s.pipelineStop != nil {
			close(s.pipelineStop)
		}
	})
}

// startQueuedRun starts a root that had been deferred by the concurrency
// gate. Its queued doc already exists, so it starts with precreate=false
// (the engine claims the queued doc via runResolveDoc). On a start error
// the reserved slot is released and the doc is failed so it leaves the
// board's TODO lane.
func (s *Service) startQueuedRun(it queuedItem) {
	if _, err := s.startInProcess(context.Background(), it.runID, it.spec, false); err != nil {
		s.logger.Warn("runview: start queued pipeline %s: %v", it.runID, err)
		s.pipelineQueue.slotFreed(it.runID)
		ctx := store.WithoutTenantFilter(context.Background())
		if uErr := s.store.UpdateRunStatus(ctx, it.runID, store.RunStatusFailed, "queued pipeline failed to start: "+err.Error()); uErr != nil {
			s.logger.Warn("runview: mark queued pipeline %s failed: %v", it.runID, uErr)
		}
	}
}

// rebuildPipelineQueue recovers pipelines left waiting (persisted as
// queued docs) by a previous process lifetime, re-enqueuing them oldest-
// first so FIFO order survives a restart. Only ROOT queued docs occupy a
// slot. The in-memory launch spec is reconstructed minimally from the doc
// (file path + inputs); non-persisted launch overrides (backend,
// merge-into, branch-name, compress, permission) are not recovered — a
// documented V1 limitation. No-op when the cap is disabled.
func (s *Service) rebuildPipelineQueue() {
	if s.pipelineQueue == nil {
		return
	}
	ctx := store.WithoutTenantFilter(context.Background())
	ids, err := s.store.ListRuns(ctx)
	if err != nil {
		s.logger.Warn("runview: rebuild pipeline queue: list runs: %v", err)
		return
	}
	type queuedDoc struct {
		id   string
		at   time.Time
		spec LaunchSpec
	}
	var recovered []queuedDoc
	for _, id := range ids {
		r, err := s.store.LoadRun(ctx, id)
		if err != nil || r.Status != store.RunStatusQueued || r.ParentRunID != "" {
			continue
		}
		at := r.CreatedAt
		if r.QueuedAt != nil {
			at = *r.QueuedAt
		}
		recovered = append(recovered, queuedDoc{
			id: id,
			at: at,
			spec: LaunchSpec{
				RunID:    id,
				FilePath: r.FilePath,
				BotID:    r.BotID,
				Vars:     inputsToVars(r.Inputs),
				// Carry the ticket through the restart so a recovered launch
				// can still spend its own needs-attention reservation in
				// dequeueReady. The board stamps Source.IssueID at launch, so
				// it is available here; leaving it empty made every recovered
				// pipeline anonymous to the reservation gate.
				PipelineTicketID: pipelineTicketIDOf(r),
			},
		})
	}
	sort.Slice(recovered, func(i, j int) bool { return recovered[i].at.Before(recovered[j].at) })
	for _, d := range recovered {
		s.pipelineQueue.enqueueRebuilt(d.id, d.spec)
	}
	if len(recovered) > 0 {
		s.logger.Info("runview: recovered %d queued pipeline(s) from a previous session", len(recovered))
	}
}

// varsToInputs converts launch --var overrides (strings) into the run's
// inputs map, used to persist a queued doc so the board can render its
// entry input.
func varsToInputs(vars map[string]string) map[string]any {
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	return out
}

// inputsToVars best-effort reconstructs launch --var strings from a
// persisted run's inputs when recovering a queued pipeline across a
// restart. Non-string inputs are stringified (launch vars are strings in
// practice), which is lossy only for complex values — acceptable for V1.
func inputsToVars(inputs map[string]any) map[string]string {
	if len(inputs) == 0 {
		return nil
	}
	out := make(map[string]string, len(inputs))
	for k, v := range inputs {
		switch t := v.(type) {
		case string:
			out[k] = t
		default:
			out[k] = fmt.Sprintf("%v", t)
		}
	}
	return out
}
