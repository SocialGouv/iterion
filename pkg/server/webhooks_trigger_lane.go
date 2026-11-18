package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The trigger lane: an issue trigger (a label applied to an issue, or the
// zero-touch open) is a REQUEST a person can make again, not an event that
// happens once. Its claims form a sequence — the delivery rows of the
// launch tail on the direct lane, the cards on the board lane — and the
// lane holds four promises:
//
//  1. a redelivery of an event a claim was made for is a duplicate for as
//     long as that claim exists. An event is known by the forge's own
//     delivery id, which every redelivery carries again (GitHub
//     "Redeliver", a GitLab retry or "Resend request", whose bytes GitLab
//     does not promise), and by its payload hash, which an exact
//     retransmission repeats and a fresh application of the label never
//     does (the issue's updated_at moves) — see laneEvent;
//  2. a fresh event launches once the lane's latest claim has ended — its
//     run terminal, its card in a terminal column;
//  3. a fresh event that arrives while the latest claim is still serving is
//     ABSORBED by it: a duplicate that starts nothing, recorded under its own
//     identity so a later redelivery of it stays a duplicate too
//     (overlap: supersede on the direct lane replaces the serving run
//     instead, the policy the launch tail already applies);
//  4. nothing about a comment command changes: a comment is its own unique
//     subject, so the command lanes never build a trigger lane.
//
// Both lanes take their next claim atomically: the direct lane through the
// unique idempotency key of the next generation's row, the board lane
// through a card id derived from the lane and the cards it already holds.
// Two fresh events racing for the same free lane therefore collide on the
// same key and one of them answers as a duplicate.

// triggerLane names one re-armable trigger on one forge subject.
type triggerLane struct {
	Project string // the payload's project path ("owner/repo")
	Subject string // "issue:<n>"
	Trigger string // the label applied, or "opened" for the zero-touch lane
}

// laneEvent is one forge event as its deliveries present it.
type laneEvent struct {
	PayloadHash string // sha256 of the body
	DeliveryID  string // the forge's own id of the event (forgeDeliveryID), "" when it sends none
}

// key is the event's identity in the records the lane writes for it — the
// absorbed-delivery row, the card marker: the forge's delivery id when it
// sends one (its redeliveries carry it whatever bytes they send), the
// payload hash otherwise.
func (e laneEvent) key() string {
	if e.DeliveryID != "" {
		return "delivery:" + e.DeliveryID
	}
	return "sha256:" + e.PayloadHash
}

// keys are the identities a record of this event may have been written
// under, by the rule claimedBy applies: its own key, and — when it carries a
// delivery id — its payload's, the key a record written without an id took.
func (e laneEvent) keys() []string {
	if e.DeliveryID != "" {
		return []string{e.key(), "sha256:" + e.PayloadHash}
	}
	return []string{e.key()}
}

// claimedBy reports whether a direct-lane claim row was made for this event.
// Two forge delivery ids decide alone — two events whose bytes coincide are
// still two events. With an id missing on either side (a forge sending none,
// a row written before ids were recorded) the payload decides.
func (e laneEvent) claimedBy(row webhooks.Delivery) bool {
	if e.DeliveryID != "" && row.ForgeDeliveryID != "" {
		return row.ForgeDeliveryID == e.DeliveryID
	}
	return row.PayloadHash == e.PayloadHash
}

// eventOf is the event a claim row was made for.
func eventOf(row webhooks.Delivery) laneEvent {
	return laneEvent{PayloadHash: row.PayloadHash, DeliveryID: row.ForgeDeliveryID}
}

// forgeDeliveryID is the forge's own id of the event a request delivers, the
// one its redeliveries carry again: GitHub's X-GitHub-Delivery, GitLab's
// webhook-id or its legacy twin Idempotency-Key (GitLab 17.4+ — its
// X-Gitlab-Event-UUID is per request, not per event). "" when the forge sends
// none.
func forgeDeliveryID(r *http.Request, provider webhooks.Provider) string {
	switch provider {
	case webhooks.ProviderGitHub:
		return strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	case webhooks.ProviderGitLab:
		if v := strings.TrimSpace(r.Header.Get("webhook-id")); v != "" {
			return v
		}
		return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	return ""
}

// newIssueTriggerLane is the lane of a trigger on an issue, or nil when the
// payload names no issue (the handler's own key then stands alone, exactly
// like a lane-less delivery).
func newIssueTriggerLane(project, subject, trigger string) *triggerLane {
	if subject == "" || trigger == "" {
		return nil
	}
	return &triggerLane{Project: project, Subject: subject, Trigger: trigger}
}

// boardLaneLabelPrefix and boardDeliveryLabelPrefix live under the board-local
// "cmd:" namespace the forge label refresh preserves (boardLocalLabelPrefixes).
const (
	boardLaneLabelPrefix     = "cmd:lane:"
	boardDeliveryLabelPrefix = "cmd:delivery:"
)

// boardLabel is the label every card of this lane carries. A project path
// holds no '#' on any forge and a subject is "issue:<n>", so the rendering
// is unambiguous whatever the trigger label contains.
func (l triggerLane) boardLabel() string {
	return boardLaneLabelPrefix + l.Project + "#" + l.Subject + "#" + l.Trigger
}

// boardDeliveryLabel marks the card carded for the event of one identity key.
func boardDeliveryLabel(key string) string { return boardDeliveryLabelPrefix + key }

// cardedFor reports whether a card was carded for this event.
func (e laneEvent) cardedFor(c *native.Issue) bool {
	for _, k := range e.keys() {
		if slices.Contains(c.Labels, boardDeliveryLabel(k)) {
			return true
		}
	}
	return false
}

// ownsCard reports whether a card listed under the subject's "cmd:" label
// belongs to this lane. A card carded before lanes existed carries only that
// subject label: it belongs to every lane of its subject, scoped to its
// project when its stamped launch context names one — conservative, so a
// card still serving blocks the lanes it might be serving.
func (l triggerLane) ownsCard(c *native.Issue) bool {
	label := l.boardLabel()
	laned := false
	for _, x := range c.Labels {
		if x == label {
			return true
		}
		if strings.HasPrefix(x, boardLaneLabelPrefix) {
			laned = true
		}
	}
	if laned {
		return false
	}
	if p := c.BotArgs[boardProjectPathKey]; p != "" && p != l.Project {
		return false
	}
	return true
}

// laneGenerationKey is the idempotency key of the lane's n-th claim on the
// direct lane. Generation zero is the handler's own key, so the rows written
// before lanes existed are the lane's first claims.
func laneGenerationKey(base string, n int) string {
	if n == 0 {
		return base
	}
	return knowledge.ChecksumHex([]byte(fmt.Sprintf("%s|gen|%d", base, n)))
}

// laneAbsorbedKey is the idempotency key of the row recording that an event
// was absorbed by a serving claim (promise 3). Keyed on the event's own
// identity, so it never collides with a claim or with another event.
func laneAbsorbedKey(base, key string) string {
	return knowledge.ChecksumHex([]byte(base + "|absorbed|" + key))
}

// laneAbsorbedRow reads the absorption recorded for this event, under any of
// its identity keys.
func (s *Server) laneAbsorbedRow(ctx context.Context, base string, ev laneEvent) (*webhooks.Delivery, error) {
	for _, k := range ev.keys() {
		d, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, laneAbsorbedKey(base, k))
		switch {
		case err == nil:
			return &d, nil
		case !errors.Is(err, webhooks.ErrNotFound):
			return nil, fmt.Errorf("read the absorbed-delivery row: %w", err)
		}
	}
	return nil, nil
}

// laneCardNamespace turns a lane and the cards it already holds into the
// deterministic "native:<uuid>" id of its next card.
var laneCardNamespace = uuid.MustParse("6f1d2c8e-3b4a-5e7f-9a0b-c1d2e3f4a5b6")

func laneCardID(tenantID string, lane triggerLane, held []string) string {
	key := tenantID + "\x00" + lane.boardLabel() + "\x00" + strings.Join(held, "\x00")
	return "native:" + uuid.NewSHA1(laneCardNamespace, []byte(key)).String()
}

// laneClaim is the direct lane's answer for one delivery, decided before
// anything is metered.
type laneClaim struct {
	// key is the claim the launch tail takes or replays, when the lane leaves
	// the decision to the tail.
	key string
	// answered is the row that already answers this delivery: the absorption
	// recorded for it earlier, or the claim still serving the lane.
	answered *webhooks.Delivery
	// absorb asks the caller to record this delivery as absorbed by answered.
	absorb bool
	// displaced is the event of a failed claim this delivery takes over: the
	// tail rewrites that row with this delivery, so once the takeover holds
	// the caller records the displaced event as absorbed — its own
	// redelivery stays a duplicate.
	displaced *laneEvent
}

// laneClaimState classifies one claim row of the direct lane.
type laneClaimState int

const (
	// claimEnded: nothing runs for the claim any more — its run is terminal
	// or gone, or its launch gave up.
	claimEnded laneClaimState = iota
	// claimRetryable: the claim's launch failed before any run existed; the
	// launch tail retries such a row for the next event that reaches it.
	claimRetryable
	// claimServing: the claim's run is live, its launch is in flight, or a
	// run may exist that no row names (a launch stranded by a crash, an
	// uncertain launch).
	claimServing
)

// claimTriggerLane walks the direct lane's claims from generation zero and
// returns the one this delivery belongs to (see the promises above).
func (s *Server) claimTriggerLane(ctx context.Context, cfg webhooks.Config, base string, ev laneEvent) (laneClaim, error) {
	if s.webhookDeliveries == nil {
		// The launch tail answers "webhooks unavailable" itself.
		return laneClaim{key: base}, nil
	}
	absorbed, err := s.laneAbsorbedRow(ctx, base, ev)
	if err != nil {
		return laneClaim{}, err
	}
	if absorbed != nil {
		return laneClaim{answered: absorbed}, nil
	}
	for gen := 0; ; gen++ {
		key := laneGenerationKey(base, gen)
		row, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, key)
		if errors.Is(err, webhooks.ErrNotFound) {
			return laneClaim{key: key}, nil
		}
		if err != nil {
			return laneClaim{}, fmt.Errorf("read claim %d of the trigger lane: %w", gen, err)
		}
		if ev.claimedBy(row) {
			// The event this claim was made for, delivered again: the tail
			// replays it, or retries it when its launch failed with no run.
			return laneClaim{key: key}, nil
		}
		state, err := s.laneClaimStateOf(ctx, row)
		if err != nil {
			return laneClaim{}, fmt.Errorf("judge claim %d (delivery %s) of the trigger lane: %w", gen, row.ID, err)
		}
		switch state {
		case claimEnded:
			continue
		case claimRetryable:
			displaced := eventOf(row)
			return laneClaim{key: key, displaced: &displaced}, nil
		}
		// Serving. Under overlap: supersede the next claim replaces the
		// serving run: the tail's supersede pass cancels the launched run
		// it selects (supersededRunIDs reads launched rows only, so a claim
		// still launching is absorbed instead).
		if row.Status == webhooks.StatusLaunched && overlapSupersedes(cfg) {
			continue
		}
		return laneClaim{answered: &row, absorb: true}, nil
	}
}

// laneClaimStateOf classifies one claim row. A row naming a run is judged by
// the run itself; a row naming none by its launch verdict, with the launch
// tail's own retry predicates.
func (s *Server) laneClaimStateOf(ctx context.Context, row webhooks.Delivery) (laneClaimState, error) {
	if row.RunID != "" {
		active, err := s.webhookRunActive(ctx, row.RunID)
		if err != nil {
			return claimServing, err
		}
		if active {
			return claimServing, nil
		}
		return claimEnded, nil
	}
	switch row.Status {
	case webhooks.StatusLaunchError:
		// The tail exhausts a durable retry at its attempt budget instead of
		// launching it (prepareWebhookTarget): such a claim has ended.
		if row.RetryTarget != nil && row.Attempts >= webhookMaxAttempts() {
			return claimEnded, nil
		}
		return claimRetryable, nil
	case webhooks.StatusLaunchExhausted:
		return claimEnded, nil
	default:
		return claimServing, nil
	}
}

// webhookRunActive reports whether a claim's run still serves its trigger:
// any status but a terminal one. A run the store no longer holds serves
// nothing; a store that cannot answer is an error, never a guess — a guess
// either way is a duplicate run or a dropped request.
func (s *Server) webhookRunActive(ctx context.Context, runID string) (bool, error) {
	if s.webhookRunIsLive != nil {
		return s.webhookRunIsLive(ctx, runID), nil
	}
	if s.runs == nil {
		return false, fmt.Errorf("no run store to read run %s", runID)
	}
	run, err := s.runs.LoadRunCtx(store.WithoutTenantFilter(ctx), runID)
	if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load run %s: %w", runID, err)
	}
	if run == nil {
		return false, nil
	}
	return !run.Status.IsTerminal(), nil
}

// boardLaneAnswer is the board lane's answer for one delivery.
type boardLaneAnswer struct {
	// card is the lane card that already answers this delivery: the one
	// carded for it, or the one still serving the lane.
	card *native.Issue
	// absorb is set when card serves the lane but was carded for another
	// delivery.
	absorb bool
	// nextID is the id the lane's next card takes, when card is nil.
	nextID string
}

// boardLaneVerdict reads the lane's cards on the tenant board. A card in a
// terminal column has ended; any other column — inbox to review, or one the
// board no longer declares — still serves.
func boardLaneVerdict(bs native.BoardStore, tenantID string, lane triggerLane, ev laneEvent) (boardLaneAnswer, error) {
	board, err := bs.Board()
	if err != nil {
		return boardLaneAnswer{}, fmt.Errorf("read the board: %w", err)
	}
	cards, err := bs.List(native.ListFilter{Labels: []string{"cmd:" + lane.Subject}})
	if err != nil {
		return boardLaneAnswer{}, fmt.Errorf("list the cards of %s: %w", lane.Subject, err)
	}
	var held []*native.Issue
	for _, c := range cards {
		if lane.ownsCard(c) {
			held = append(held, c)
		}
	}
	sort.Slice(held, func(i, j int) bool { return held[i].ID < held[j].ID })
	for _, c := range held {
		if ev.cardedFor(c) {
			return boardLaneAnswer{card: c}, nil
		}
	}
	ids := make([]string, 0, len(held))
	for _, c := range held {
		if st := board.StateByName(c.State); st == nil || !st.Terminal {
			return boardLaneAnswer{card: c, absorb: true}, nil
		}
		ids = append(ids, c.ID)
	}
	return boardLaneAnswer{nextID: laneCardID(tenantID, lane, ids)}, nil
}

// recordLaneAbsorbed writes the row that keeps an absorbed event a duplicate
// for good (promise 3): the delivery at hand, or the event of a failed claim
// it displaces. It carries no run id: the absorbing claim owns the run, and
// the readers that count launches read the run id.
func (s *Server) recordLaneAbsorbed(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta, base, botID string, ev laneEvent, srcIP, servedBy string) {
	d := newWebhookDelivery(cfg, meta, webhooks.StatusDuplicate, ev.PayloadHash, srcIP)
	d.ForgeDeliveryID = ev.DeliveryID
	d.IdempotencyKey = laneAbsorbedKey(base, ev.key())
	d.BotID = botID
	d.Error = "absorbed: " + servedBy + " still serves this trigger"
	if s.webhookDeliveries == nil {
		return
	}
	if err := s.webhookDeliveries.Insert(ctx, d); err != nil && !errors.Is(err, webhooks.ErrDuplicate) {
		s.warnf("webhooks: %s/%s %s: the absorbed delivery could not be recorded (%v) — a redelivery of it once %s ends would launch",
			cfg.Provider, meta.ProjectPath, meta.SubjectID, err, servedBy)
	}
}

// laneUnavailable answers a delivery whose lane could not be read: nothing
// was metered or launched, the audit row says why under a random key (it
// claims nothing), and the forge sees a failure it can redeliver.
func (s *Server) laneUnavailable(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, payloadHash, srcIP string, err error) {
	reason := "trigger lane unavailable: " + err.Error()
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, reason)
	s.warnf("webhooks: %s/%s %s: %s", cfg.Provider, meta.ProjectPath, meta.SubjectID, reason)
	httpError(w, http.StatusServiceUnavailable, "the trigger's previous claim could not be read; redeliver to retry")
}
