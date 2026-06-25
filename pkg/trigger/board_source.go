package trigger

import (
	"context"
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Publisher is the minimal sink the board source needs. eventbus.Bus
// satisfies it structurally; declaring it here (instead of importing
// eventbus) keeps the trigger package free of an eventbus import, so
// eventbus can depend on trigger without a cycle.
type Publisher interface {
	Publish(ctx context.Context, ev Event) error
}

// BoardSource turns native-board transitions into trigger.Events on the bus.
// It tails the shared events.jsonl via native.Store.Subscribe (the only
// writer-agnostic seam — every Store instance appends to the same log), so it
// observes transitions made by the server, the dispatcher, or a per-run
// executor alike. Lifecycle mirrors watch_coordinator: a buffered channel fed
// by the tailer goroutine, drained by a single worker that does the store
// read + publish.
type BoardSource struct {
	store     *native.Store
	bus       Publisher
	logger    *iterlog.Logger
	tenantID  string
	repo      string
	boardName string

	events chan native.Event
	cancel func()
	done   chan struct{}
}

// BoardSourceOption configures a BoardSource.
type BoardSourceOption func(*BoardSource)

// WithBoardTenant stamps a tenant id onto emitted events (cloud mode).
func WithBoardTenant(id string) BoardSourceOption { return func(b *BoardSource) { b.tenantID = id } }

// WithBoardRepo stamps a repo slug onto emitted events so repo-scoped
// subscriptions match.
func WithBoardRepo(repo string) BoardSourceOption { return func(b *BoardSource) { b.repo = repo } }

// WithBoardName records the board's name (informational, carried in payload).
func WithBoardName(name string) BoardSourceOption {
	return func(b *BoardSource) { b.boardName = name }
}

// StartBoardSource subscribes to the native store's event tail and begins
// publishing board events to the bus. It returns nil (a no-op) when a
// prerequisite is missing or the tail can't start — board triggering is an
// enhancement layered on the dispatcher poll, never a hard dependency, so a
// host without fsnotify simply falls back to the poll.
func StartBoardSource(store *native.Store, bus Publisher, logger *iterlog.Logger, opts ...BoardSourceOption) *BoardSource {
	if store == nil || bus == nil {
		return nil
	}
	bs := &BoardSource{
		store:  store,
		bus:    bus,
		logger: logger,
		events: make(chan native.Event, 128),
		done:   make(chan struct{}),
	}
	for _, o := range opts {
		o(bs)
	}
	cancel, err := store.Subscribe(bs.enqueue)
	if err != nil {
		if logger != nil {
			logger.Warn("trigger: board source disabled (events tail unavailable): %v", err)
		}
		return nil
	}
	bs.cancel = cancel
	go bs.worker()
	return bs
}

// Stop tears down the tail and worker (idempotent-safe via the once-guarded
// cancel returned by Subscribe).
func (b *BoardSource) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	close(b.events)
	<-b.done
}

// enqueue runs on the tailer goroutine — non-blocking so a slow publish can't
// stall the tail. A full buffer drops the event; the dispatcher poll is the
// backstop for a dropped board notification.
func (b *BoardSource) enqueue(evt native.Event) {
	switch evt.Type {
	case native.EvtIssueCreated, native.EvtIssueState, native.EvtIssueUpdated:
	default:
		return
	}
	if evt.IssueID == "" {
		return
	}
	select {
	case b.events <- evt:
	default:
		if b.logger != nil {
			b.logger.Warn("trigger: board source queue full, dropping %s for issue %s", evt.Type, evt.IssueID)
		}
	}
}

func (b *BoardSource) worker() {
	defer close(b.done)
	ctx := context.Background()
	for evt := range b.events {
		te, ok := b.normalize(evt)
		if !ok {
			continue
		}
		if err := b.bus.Publish(ctx, te); err != nil && b.logger != nil {
			b.logger.Warn("trigger: publish board event for issue %s failed: %v", evt.IssueID, err)
		}
	}
}

// normalize converts a native.Event into a trigger.Event by reading the
// current issue (the audit event payload is sparse — labels/title/body live on
// the issue). Returns false when the issue can't be read (deleted between the
// transition and the read).
func (b *BoardSource) normalize(evt native.Event) (Event, bool) {
	iss, err := b.store.Get(evt.IssueID)
	if err != nil || iss == nil {
		return Event{}, false
	}
	kind := KindCardUpdated
	switch evt.Type {
	case native.EvtIssueCreated:
		kind = KindCardCreated
	case native.EvtIssueState:
		kind = KindCardMoved
	case native.EvtIssueUpdated:
		kind = KindCardUpdated
	}
	payload := map[string]any{}
	if b.boardName != "" {
		payload["board"] = b.boardName
	}
	if from, ok := evt.Payload["from"].(string); ok {
		payload["from_state"] = from
	}
	return Event{
		ID:       fmt.Sprintf("board:%s:%s:%s", b.boardName, evt.IssueID, strconv.FormatInt(evt.Seq, 10)),
		Source:   SourceBoard,
		Kind:     kind,
		TenantID: b.tenantID,
		Repo:     b.repo,
		Subject: Subject{
			Type:  "card",
			ID:    iss.ID,
			Title: iss.Title,
			Body:  iss.Body,
			State: iss.State,
		},
		Actor:      iss.Assignee,
		Labels:     append([]string(nil), iss.Labels...),
		Payload:    payload,
		OccurredAt: evt.Timestamp,
	}, true
}

// NativeBoardEffect is the BoardEffect for the native board: it promotes a
// card by stamping the subscription's bot + bot-args (and, when the
// subscription names one via vars["promote_state"], an eligible state) so the
// dispatcher's existing Claim picks it up. After a real change it nudges an
// optional Nudger (the dispatcher's Refresh) to dispatch now instead of at the
// next poll. Stamping is idempotent — a card already pinned to the same bot is
// left untouched, so a board event storm converges.
type NativeBoardEffect struct {
	store  *native.Store
	nudger Nudger
	logger *iterlog.Logger
}

// NewNativeBoardEffect builds the native board promote effect. nudger and
// logger may be nil.
func NewNativeBoardEffect(store *native.Store, nudger Nudger, logger *iterlog.Logger) *NativeBoardEffect {
	return &NativeBoardEffect{store: store, nudger: nudger, logger: logger}
}

// PromoteStateVar is the reserved subscription var that, when set, tells the
// promote effect to also move the card into that (eligible) state. Absent =
// leave the state as-is and only pin the bot.
const PromoteStateVar = "promote_state"

func (n *NativeBoardEffect) Promote(_ context.Context, plan LaunchPlan) (string, error) {
	if n.store == nil {
		return "", fmt.Errorf("trigger: native board effect has no store")
	}
	id := plan.Event.Subject.ID
	if id == "" {
		return "", fmt.Errorf("trigger: board plan has no card id")
	}
	iss, err := n.store.Get(id)
	if err != nil {
		return "", fmt.Errorf("trigger: get card %s: %w", id, err)
	}
	// Idempotent: a card already pinned to this bot needs no churn.
	promoteState := plan.Vars[PromoteStateVar]
	alreadyBot := iss.Bot == plan.BotID
	alreadyState := promoteState == "" || iss.State == promoteState
	if alreadyBot && alreadyState {
		return id, nil
	}

	changed := false
	if !alreadyBot {
		bot := plan.BotID
		args := botArgsFromVars(plan.Vars)
		if _, err := n.store.Update(id, native.Patch{Bot: &bot, BotArgs: &args}); err != nil {
			return "", fmt.Errorf("trigger: stamp bot on card %s: %w", id, err)
		}
		changed = true
	}
	if !alreadyState {
		if _, err := n.store.SetState(id, promoteState); err != nil {
			return "", fmt.Errorf("trigger: promote card %s to %s: %w", id, promoteState, err)
		}
		changed = true
	}
	if changed && n.nudger != nil {
		n.nudger.Refresh()
	}
	return id, nil
}

// botArgsFromVars strips the reserved promote_state key from the resolved vars
// before they are stamped as the card's BotArgs.
func botArgsFromVars(vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		if k == PromoteStateVar {
			continue
		}
		out[k] = v
	}
	return out
}

var _ BoardEffect = (*NativeBoardEffect)(nil)
