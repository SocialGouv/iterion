package usernotify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// bodyPreviewLen bounds the human-node instructions excerpt included in a
// notification body — enough to recognise the ask, small enough for the
// ~4KB push payload budget and for OS notification truncation.
const bodyPreviewLen = 120

// deliverTimeout bounds one sink delivery (a sink may push to many
// subscriptions of many users).
const deliverTimeout = 30 * time.Second

// Dispatcher turns run-outcome trigger.Events into user Notifications and
// fans them out to its sinks. It subscribes on the eventbus spine (queue
// group "usernotify" on NATS ⇒ exactly one replica handles each event) and
// is also driven by the reconciliation sweep for episodes the lossy bus
// dropped. The SentStore's first-writer-wins claim dedups the two paths.
type Dispatcher struct {
	runs    store.RunStore
	prefs   PrefsStore // nil ⇒ owner-only recipients
	sent    SentStore  // nil ⇒ no dedup (tests)
	baseURL string
	logger  *iterlog.Logger
	sinks   []Sink
}

// SubscriberName is the eventbus subscriber (and NATS queue group) name.
const SubscriberName = "usernotify"

func NewDispatcher(runs store.RunStore, prefs PrefsStore, sent SentStore, baseURL string, logger *iterlog.Logger, sinks ...Sink) *Dispatcher {
	if logger == nil {
		logger = iterlog.Nop()
	}
	return &Dispatcher{
		runs:    runs,
		prefs:   prefs,
		sent:    sent,
		baseURL: strings.TrimRight(baseURL, "/"),
		logger:  logger,
		sinks:   sinks,
	}
}

// Attach subscribes the dispatcher to bus for run-lifecycle events.
func (d *Dispatcher) Attach(bus eventbus.Bus) (func(), error) {
	return bus.Subscribe(SubscriberName, trigger.Matcher{
		Sources: []trigger.Source{trigger.SourceRun},
		Kinds: []string{
			trigger.KindRunPaused,
			trigger.KindRunFinished,
			trigger.KindRunFailed,
			trigger.KindRunCancelled,
		},
	}, d.Handle)
}

// Handle processes one run-outcome event. It is the eventbus.Handler and
// the sweep's replay entry point.
func (d *Dispatcher) Handle(ctx context.Context, ev trigger.Event) error {
	kind, ok := kindFor(ev)
	if !ok {
		return nil
	}
	runID := ev.Subject.ID
	if runID == "" {
		return nil
	}

	// Claim the episode before delivering so the live bus path, the sweep,
	// and other replicas cannot double-send. A delivery that fails on every
	// sink releases the claim so the sweep retries.
	if d.sent != nil {
		won, err := d.sent.TryMark(ctx, ev.ID)
		if err != nil {
			return fmt.Errorf("usernotify: claim episode %s: %w", ev.ID, err)
		}
		if !won {
			return nil
		}
	}

	n := d.build(ctx, ev, kind, runID)
	if len(n.UserIDs) == 0 || len(d.sinks) == 0 {
		// Nothing addressable (local single-user run with no owner) or no
		// channel wired — confirm the claim so neither the sweep nor the
		// stale-claim takeover ever retries a deliberate no-op.
		d.markDelivered(ctx, ev.ID)
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, len(d.sinks))
	for i, sink := range d.sinks {
		wg.Add(1)
		go func(i int, sink Sink) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
			defer cancel()
			if err := sink.Deliver(sctx, n); err != nil {
				errs[i] = err
				d.logger.Warn("usernotify: sink %s: deliver %s for run %s: %v", sink.Name(), n.Kind, n.RunID, err)
			}
		}(i, sink)
	}
	wg.Wait()

	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	if failed == len(d.sinks) {
		if d.sent != nil {
			if err := d.sent.Unmark(context.WithoutCancel(ctx), ev.ID); err != nil {
				d.logger.Warn("usernotify: release episode %s after failed delivery: %v", ev.ID, err)
			}
		}
		return fmt.Errorf("usernotify: every sink failed for episode %s", ev.ID)
	}
	// At least one sink delivered: confirm the pending claim so the episode
	// is settled (a crash BEFORE this line leaves a pending claim that the
	// ClaimGrace takeover retries — better a rare duplicate than a lost
	// "your run is waiting on you").
	d.markDelivered(ctx, ev.ID)
	return nil
}

func (d *Dispatcher) markDelivered(ctx context.Context, key string) {
	if d.sent == nil {
		return
	}
	if err := d.sent.MarkDelivered(context.WithoutCancel(ctx), key); err != nil {
		d.logger.Warn("usernotify: confirm episode %s: %v", key, err)
	}
}

// kindFor maps the trigger event onto a notification Kind. A run.paused
// without a pending interaction is an operator soft-pause — the operator
// initiated it, so there is nobody to notify.
func kindFor(ev trigger.Event) (Kind, bool) {
	switch ev.Kind {
	case trigger.KindRunPaused:
		if s, _ := ev.Payload["interaction_id"].(string); s == "" {
			return "", false
		}
		return KindHumanInputRequested, true
	case trigger.KindRunFinished:
		return KindRunFinished, true
	case trigger.KindRunFailed:
		return KindRunFailed, true
	case trigger.KindRunCancelled:
		return KindRunCancelled, true
	}
	return "", false
}

func (d *Dispatcher) build(ctx context.Context, ev trigger.Event, kind Kind, runID string) Notification {
	fctx := store.WithoutTenantFilter(ctx)

	tenantID := ev.TenantID
	ownerID, _ := ev.Payload["owner_id"].(string)
	nodeID, _ := ev.Payload["node_id"].(string)
	interactionID, _ := ev.Payload["interaction_id"].(string)
	name := ev.Subject.Title

	// The event usually carries everything; re-read the run only to fill
	// gaps (events built before the enrichment existed, sweep replays).
	if tenantID == "" || ownerID == "" || name == "" {
		r, err := d.runs.LoadRun(fctx, runID)
		if err != nil {
			// Visible, not fatal: with no owner resolvable the episode ends
			// as an addressed-to-nobody no-op, so surface why.
			d.logger.Warn("usernotify: load run %s for notification: %v", runID, err)
		} else if r != nil {
			if tenantID == "" {
				tenantID = r.TenantID
			}
			if ownerID == "" {
				ownerID = r.OwnerID
			}
			if name == "" {
				name = r.Name
				if name == "" {
					name = r.WorkflowName
				}
			}
		}
	}
	if name == "" {
		name = runID
	}

	recipients := make([]string, 0, 2)
	seen := map[string]struct{}{}
	if ownerID != "" {
		recipients = append(recipients, ownerID)
		seen[ownerID] = struct{}{}
	}
	if d.prefs != nil && tenantID != "" {
		teamWide, err := d.prefs.ListTeamWide(fctx, tenantID)
		if err != nil {
			d.logger.Warn("usernotify: list team-wide opt-ins for %s: %v", tenantID, err)
		}
		for _, uid := range teamWide {
			if _, dup := seen[uid]; !dup {
				recipients = append(recipients, uid)
				seen[uid] = struct{}{}
			}
		}
	}

	title, body := d.render(fctx, kind, name, runID, nodeID, interactionID)

	data := map[string]string{}
	if nodeID != "" {
		data["node_id"] = nodeID
	}
	if interactionID != "" {
		data["interaction_id"] = interactionID
	}

	return Notification{
		Kind:     kind,
		TenantID: tenantID,
		UserIDs:  recipients,
		Title:    title,
		Body:     body,
		Link:     d.baseURL + "/runs/" + runID,
		RunID:    runID,
		Tag:      runID,
		Data:     data,
	}
}

// render produces the display strings. The human-input body includes a
// bounded excerpt of the node's authored `instructions:` text — never
// resolved answer data — so the push payload cannot leak run content.
func (d *Dispatcher) render(ctx context.Context, kind Kind, name, runID, nodeID, interactionID string) (title, body string) {
	switch kind {
	case KindHumanInputRequested:
		title = "Input needed: " + name
		body = "A run is waiting for your answer"
		if nodeID != "" {
			body = "Node “" + nodeID + "” is waiting for your answer"
		}
		if interactionID != "" {
			if in, err := d.runs.LoadInteraction(ctx, runID, interactionID); err == nil && in != nil {
				if instr, _ := in.Questions["instructions"].(string); instr != "" {
					body = truncate(strings.TrimSpace(instr), bodyPreviewLen)
				}
			}
		}
	case KindRunFinished:
		title = "Run finished: " + name
		body = "The run completed successfully."
	case KindRunFailed:
		title = "Run failed: " + name
		body = "The run failed."
		if nodeID != "" {
			body = "The run failed at node “" + nodeID + "”."
		}
	case KindRunCancelled:
		title = "Run cancelled: " + name
		body = "The run was cancelled."
	}
	return title, body
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
