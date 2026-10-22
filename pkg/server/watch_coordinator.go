package server

import (
	"context"
	"fmt"
	"slices"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// watchCoordinator fans native-kanban issue-state transitions out to the
// runs that subscribed to them (Run.WatchedIssueIDs, MVP3b).
//
// It tails the shared events.jsonl via native.Store.Subscribe and, for
// each issue_state_changed event, enqueues a user-message onto every
// non-terminal run watching that issue so the bot sees the transition
// between turns (claw drains each agent-loop iteration; claude_code at
// the next pause/resume). Delivery reuses runview.Service.QueueMessage
// so the message ID, terminal-state guard, and WS inbox event stay in
// lockstep with operator-typed messages.
//
// The events tail (the sole sender) feeds a buffered channel drained by
// a single worker goroutine, keeping the tailer goroutine snappy and
// serialising store I/O.
type watchCoordinator struct {
	runs   *runview.Service
	native *native.Store
	logger *iterlog.Logger
	events chan native.Event
	cancel func()
	done   chan struct{}
}

// startWatchCoordinator wires the coordinator and begins tailing. Returns
// nil (a no-op) when prerequisites are missing or the events tail can't
// start — fan-out is an enhancement, never a hard dependency.
func startWatchCoordinator(runs *runview.Service, ns *native.Store, logger *iterlog.Logger) *watchCoordinator {
	if runs == nil || ns == nil {
		return nil
	}
	wc := &watchCoordinator{
		runs:   runs,
		native: ns,
		logger: logger,
		events: make(chan native.Event, 128),
		done:   make(chan struct{}),
	}
	cancel, err := ns.Subscribe(wc.enqueue)
	if err != nil {
		if logger != nil {
			logger.Warn("server: watch fan-out disabled (events tail unavailable): %v", err)
		}
		return nil
	}
	wc.cancel = cancel
	go wc.worker()
	return wc
}

// enqueue runs on the tailer goroutine — keep it non-blocking so a slow
// fan-out can't stall the tailer. A full buffer drops the event; the
// next transition self-heals the view and a dropped notification only
// delays one update.
func (wc *watchCoordinator) enqueue(evt native.Event) {
	if evt.Type != native.EvtIssueState || evt.IssueID == "" {
		return
	}
	select {
	case wc.events <- evt:
	default:
		if wc.logger != nil {
			wc.logger.Warn("server: watch fan-out queue full, dropping %s for issue %s", evt.Type, evt.IssueID)
		}
	}
}

func (wc *watchCoordinator) worker() {
	defer close(wc.done)
	for evt := range wc.events {
		wc.fanOut(evt)
	}
}

func (wc *watchCoordinator) fanOut(evt native.Event) {
	ctx := context.Background()
	from, _ := evt.Payload["from"].(string)
	to, _ := evt.Payload["to"].(string)

	rs := wc.runs.RunStore()
	// The runs that watch SOMETHING, not every run in the store: the walk
	// below keeps only those watching this issue, and asking the store for
	// the candidates is what keeps a transition's cost at the number of
	// watchers rather than the size of the store (#2636).
	ids, err := rs.ListDeliverableWatchers(ctx)
	if err != nil {
		if wc.logger != nil {
			wc.logger.Warn("server: watch fan-out list watching runs: %v", err)
		}
		return
	}
	text := wc.message(evt.IssueID, from, to)
	for _, runID := range ids {
		run, err := rs.LoadRun(ctx, runID)
		if err != nil {
			continue
		}
		if !watchDeliverable(run.Status) {
			continue
		}
		if !slices.Contains(run.WatchedIssueIDs, evt.IssueID) {
			continue
		}
		if _, err := wc.runs.QueueMessage(ctx, runID, text); err != nil {
			if wc.logger != nil {
				wc.logger.Warn("server: watch fan-out queue message to run %s: %v", runID, err)
			}
		}
	}
}

// message renders the bot-facing notification. The issue title is a
// best-effort lookup; a deleted/unknown issue still produces a useful
// line from the ID + transition.
func (wc *watchCoordinator) message(issueID, from, to string) string {
	title := ""
	if iss, err := wc.native.Get(issueID); err == nil && iss != nil {
		title = iss.Title
	}
	transition := to
	if from != "" {
		transition = fmt.Sprintf("%s → %s", from, to)
	}
	if title != "" {
		return fmt.Sprintf("Watched ticket %s (%s) changed state: %s.", issueID, title, transition)
	}
	return fmt.Sprintf("Watched ticket %s changed state: %s.", issueID, transition)
}

// Close stops the events tail and drains the worker. cancel() blocks
// until the tailer goroutine (the sole sender) has exited, so closing
// the events channel afterwards can never race a send.
func (wc *watchCoordinator) Close() {
	if wc == nil {
		return
	}
	if wc.cancel != nil {
		wc.cancel()
	}
	close(wc.events)
	<-wc.done
}

// watchDeliverable reports whether a run in this status will consume a
// queued message soon (running drains mid-loop; paused drains on resume).
// Terminal runs are skipped — a queued message would never be read.
//
// The set lives in the store (store.WatchDeliverableStatuses): the store's
// ListDeliverableWatchers answers with exactly these statuses, so the check
// here and the query there can never disagree.
func watchDeliverable(status store.RunStatus) bool {
	return status.WatchDeliverable()
}
