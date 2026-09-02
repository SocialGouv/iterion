package trigger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
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

	events       chan native.Event
	cancel       func()
	workerCtx    context.Context
	workerCancel context.CancelFunc
	done         chan struct{}
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
	ctx, cancel := context.WithCancel(context.Background())
	bs := &BoardSource{
		store:        store,
		bus:          bus,
		logger:       logger,
		events:       make(chan native.Event, 128),
		workerCtx:    ctx,
		workerCancel: cancel,
		done:         make(chan struct{}),
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
	// Cancel the worker context first so an in-flight Publish observes the
	// teardown, then close the event channel to end the worker's range.
	if b.workerCancel != nil {
		b.workerCancel()
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
	for evt := range b.events {
		te, ok := b.normalize(evt)
		if !ok {
			continue
		}
		// b.workerCtx is cancelled on Stop so a context-aware Publish
		// unwinds promptly. (The store.Get inside normalize is still
		// synchronous and context-free — a hung store read would not be
		// interrupted here; that would require a context-aware Store.Get.)
		if err := b.bus.Publish(b.workerCtx, te); err != nil && b.logger != nil {
			b.logger.Warn("trigger: publish board event for issue %s failed: %v", evt.IssueID, err)
		}
	}
}

// normalize converts a native.Event into a trigger.Event by reading the
// current issue (the audit event payload is sparse — labels/title/body live on
// the issue). Returns false when the issue can't be read. The local tail has
// no cursor to protect, so a transient read error degrades to a skip like a
// deletion (the dispatcher poll is this path's net) — the distinction only
// buys something where an advanced cursor would make the loss permanent.
func (b *BoardSource) normalize(evt native.Event) (Event, bool) {
	ev, ok, err := NormalizeBoardEvent(b.store.Get, evt, b.tenantID, b.repo, b.boardName)
	if err != nil {
		b.logger.Warn("trigger: normalize board event for issue %s: %v", evt.IssueID, err)
		return Event{}, false
	}
	return ev, ok
}

// IsCardEvent reports whether a native board event is one of the card
// transitions the trigger spine reacts to — the shared filter between the
// local tail (enqueue) and the cloud poll-tail.
func IsCardEvent(t native.EventType) bool {
	switch t {
	case native.EvtIssueCreated, native.EvtIssueState, native.EvtIssueUpdated:
		return true
	}
	return false
}

// NormalizeBoardEvent converts a native board event into a trigger.Event by
// reading the CURRENT issue through get (the audit event payload is sparse —
// labels/title/body live on the issue). Shared by the local BoardSource and
// the cloud board source. ok=false with a nil error means the card is GONE
// (deleted between the transition and the read — a definitive skip); a
// non-nil error is a TRANSIENT store failure the caller must not treat as a
// deletion: the cloud tail aborts its batch before advancing the cursor,
// because an advanced cursor turns one Mongo blip into a permanently lost
// trigger. When the card links an external forge issue, its repo slug is
// stamped on the event (falling back to the source-wide repo) so repo-scoped
// subscriptions match.
func NormalizeBoardEvent(get func(id string) (*native.Issue, error), evt native.Event, tenantID, repo, boardName string) (Event, bool, error) {
	iss, err := get(evt.IssueID)
	switch {
	case errors.Is(err, tracker.ErrNotFound):
		return Event{}, false, nil
	case err != nil:
		return Event{}, false, err
	case iss == nil:
		return Event{}, false, nil
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
	if boardName != "" {
		payload["board"] = boardName
	}
	if from, ok := evt.Payload["from"].(string); ok {
		payload["from_state"] = from
	}
	// Provenance travels verbatim; whether it is MACHINE provenance is the
	// enumerated tracker.IsMachineReason — the same contract machineCaused
	// reads on the effect side. A descriptive reason (unblocked) keeps its
	// actor and its triggers.
	machine := false
	if reason, ok := evt.Payload["reason"].(string); ok && reason != "" {
		payload["reason"] = reason
		machine = tracker.IsMachineReason(reason)
	}
	if iss.External != nil && iss.External.Repo != "" {
		repo = iss.External.Repo
	}
	return Event{
		ID:       fmt.Sprintf("board:%s:%s:%s", boardName, evt.IssueID, strconv.FormatInt(evt.Seq, 10)),
		Source:   SourceBoard,
		Kind:     kind,
		TenantID: tenantID,
		Repo:     repo,
		Subject: Subject{
			Type:  "card",
			ID:    iss.ID,
			Title: iss.Title,
			Body:  iss.Body,
			State: iss.State,
		},
		// A machine repair is not authored by the card's assignee. Leaving
		// their name on it misreports who acted, to every Actor-reading
		// subscription and audit surface.
		Actor:      actorFor(iss.Assignee, machine),
		Labels:     append([]string(nil), iss.Labels...),
		Payload:    payload,
		OccurredAt: evt.Timestamp,
	}, true, nil
}

// NativeBoardEffect is the BoardEffect for the native board: it promotes a
// card by stamping the subscription's bot + bot-args so the dispatcher's
// existing Claim picks it up. After a real change it nudges an optional Nudger
// (the dispatcher's Refresh) to dispatch now instead of at the next poll.
// Stamping is idempotent — a card already pinned to the same bot is left
// untouched, so a board event storm converges.
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

// Promote stamps the matched card's bot + bot-args so the dispatcher's Claim
// picks it up, then nudges it to dispatch now. A board trigger fires when a
// card ENTERS an eligible state (the matcher's subject_states gate), so the
// card is already dispatchable — promoting only needs to pin which bot runs.
// Idempotent: a card already pinned to this bot is left untouched, so a board
// event storm converges.
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
	if iss.Bot == plan.BotID {
		return id, nil // already pinned — no churn
	}
	bot := plan.BotID
	args := plan.Vars
	if _, err := n.store.Update(id, native.Patch{Bot: &bot, BotArgs: &args}); err != nil {
		return "", fmt.Errorf("trigger: stamp bot on card %s: %w", id, err)
	}
	if n.nudger != nil {
		n.nudger.Refresh()
	}
	return id, nil
}

// ConsumeMatchLabels strips the given labels from the card, reporting whether
// any were actually present. The evaluator runs on a single serial worker
// (InProcBus), so read-strip-launch is race-free locally; consumed=false means
// a previous evaluation already stripped them and the caller must skip. The
// tenant is ignored — the local store IS one tenant.
func (n *NativeBoardEffect) ConsumeMatchLabels(_ context.Context, _ string, issueID string, labels []string) (bool, error) {
	if n.store == nil {
		return false, fmt.Errorf("trigger: native board effect has no store")
	}
	if issueID == "" || len(labels) == 0 {
		return false, nil
	}
	iss, err := n.store.Get(issueID)
	if err != nil {
		return false, fmt.Errorf("trigger: get card %s: %w", issueID, err)
	}
	strip := make(map[string]bool, len(labels))
	for _, l := range labels {
		strip[strings.ToLower(l)] = true
	}
	remaining := make([]string, 0, len(iss.Labels))
	found := false
	for _, l := range iss.Labels {
		if strip[strings.ToLower(l)] {
			found = true
			continue
		}
		remaining = append(remaining, l)
	}
	if !found {
		return false, nil
	}
	if _, err := n.store.Update(issueID, native.Patch{Labels: &remaining}); err != nil {
		return false, fmt.Errorf("trigger: consume labels on card %s: %w", issueID, err)
	}
	return true, nil
}

var _ BoardEffect = (*NativeBoardEffect)(nil)
var _ LabelConsumer = (*NativeBoardEffect)(nil)

// actorFor blanks the actor on a machine-caused event: attributing a
// watchdog's repair to whoever the card is assigned to is a lie the
// audit trail keeps.
func actorFor(assignee string, machine bool) string {
	if machine {
		return ""
	}
	return assignee
}
