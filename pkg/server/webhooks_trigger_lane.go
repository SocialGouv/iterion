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

// triggerLane names one re-armable trigger on one forge subject. Bot names
// the bot the lane serves once its integration routes by subscription
// (ADR-172); "" is the legacy lane the elected winner serves. A lane carries
// no per-delivery alias list: what a lane owns derives from (Project,
// Subject, Trigger, Bot) alone, so two concurrent events on one bot lane
// hold the same set and compute the same next card id.
type triggerLane struct {
	Project string // the payload's project path ("owner/repo")
	Subject string // "issue:<n>"
	Trigger string // the label applied, or "opened" for the zero-touch lane
	Bot     string // the bot this lane serves; "" on the legacy lane
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
//
// A bot lane renders the bot between the prefix and the project
// ("cmd:lane:@nestor/acme/widgets#issue:42#nestor"). That rendering can
// never equal a legacy one, whatever the trigger label holds: no project
// path starts with '@' on GitHub, GitLab or Forgejo, and a bot id is a
// slug — so an old replica's ownsCard reads a bot card as laned and not its
// own, and a bot card can never be mistaken for a legacy card.
func (l triggerLane) boardLabel() string {
	if l.Bot != "" {
		return boardLaneLabelPrefix + "@" + l.Bot + "/" + l.Project + "#" + l.Subject + "#" + l.Trigger
	}
	return l.legacyBoardLabel()
}

// legacyBoardLabel always renders the bot-less form: the label a lane card
// carried before lanes carried bots, and the one the legacy writer keeps
// stamping. A bot lane owns the legacy rendering of its OWN trigger only
// when the card's bot is its own (ownsCard) — the card a legacy era left
// beside this bot's claims, never a card of another trigger of the subject.
func (l triggerLane) legacyBoardLabel() string {
	return boardLaneLabelPrefix + l.Project + "#" + l.Subject + "#" + l.Trigger
}

// botLaneBase is the bot-scoped idempotency base of a bot lane: the lane's
// legacy base string extended with the bot, so a bot's claims chain under
// their own key — never beside another bot's claims, and never beside the
// legacy election's. Every writer and reader of a bot lane derives the base
// through this one function, so the chains they write and walk agree.
func botLaneBase(legacyBase, bot string) string {
	return knowledge.ChecksumHex([]byte(legacyBase + "|bot:" + bot))
}

// laneSpineHalf is the spine half a LEGACY lane read covers once its
// integration has ever routed by subscription (cfg.SpineSince set): the
// elected bot's bot-scoped Delivery chains, one per added label that passes
// the repository gate, and — on a board read — the same labels' bot lanes,
// whose cards join the owned set. A never-flipped config passes none, and
// its reads stay byte-identical.
type laneSpineHalf struct {
	// Bot is the bot whose claims the read covers: the legacy election's
	// winner at read time.
	Bot string
	// Bases are the bot-scoped chains (botLaneBase of each gated added
	// label), read read-only and filtered on BotID == Bot.
	Bases []string
	// Triggers are the labels the bases were derived from, in the same
	// order: the bot lanes whose cards join a board read's owned set.
	Triggers []string
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
// belongs to this lane.
//
// A bot lane owns, of its (bot, trigger): its own label; the legacy lane
// card of the SAME project, subject and trigger, when the card's bot is this
// lane's — the card the legacy election left beside this bot's claims; no
// other cmd:lane: card, so a lane of another bot or another trigger is its
// own lane and a stale card of bot A never absorbs bot B; and a card from
// before lanes existed whose bot is this lane's or unnamed, within its
// project — conservative, so a card still serving blocks the lanes it might
// be serving.
//
// A legacy lane (Bot == "") keeps the pre-bot rule: its own label, and every
// pre-lane card of its project. An '@'-card is laned and never its own — an
// old replica reading a bot card reaches the same verdict.
// botLaneCard reports whether c carries a bot lane label — a card the
// spine carded for one bot, as opposed to a lane card of the bot-less era.
func botLaneCard(c *native.Issue) bool {
	return slices.ContainsFunc(c.Labels, func(x string) bool {
		return strings.HasPrefix(x, boardLaneLabelPrefix+"@")
	})
}

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
	if l.Bot == "" {
		if laned {
			return false
		}
		if p := c.BotArgs[boardProjectPathKey]; p != "" && p != l.Project {
			return false
		}
		return true
	}
	// The legacy rendering of this lane's own trigger: the card's bot
	// decides — another bot's legacy card is that bot's lane, never ours.
	for _, x := range c.Labels {
		if x == l.legacyBoardLabel() {
			return c.Bot == l.Bot
		}
	}
	if laned {
		// Another lane's label: another bot's lane label, or this bot's lane
		// under another trigger. Either way it is not ours.
		return false
	}
	// A pre-lane card: the bot's own, or one the bot-less era left — the
	// card may be serving this bot, so it must block its lane.
	if c.Bot != "" && c.Bot != l.Bot {
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

// laneRead is one lane read: what the one reader walks, and whose claims
// count.
type laneRead struct {
	// Lane is the lane the read serves. nil reads Delivery chains only —
	// the board half needs a lane's subject and label to read cards.
	Lane *triggerLane
	// Bases are the Delivery chains this read may claim under; Bases[0] is
	// the primary, the chain the claimer writes. Its rows are read
	// unfiltered; a further base behaves like Legacy.
	Bases []string
	// Legacy are other families' chains, read read-only: a row of another
	// bot is skipped, a serving claim absorbs, nothing is ever claimed
	// under them. A legacy lane's spine half rides here.
	Legacy []string
	// EventOnly are chains read only for THIS event's identity: a claim
	// made for it answers (a duplicate), nothing else in the chain counts.
	// The base legacy used for this event (GitLab payload order) rides here
	// on a spine read.
	EventOnly []string
	// Bot is the bot the read serves. Rows of the non-primary families and
	// the board's event scan answer only it. "" with no families is the
	// never-flipped legacy read, which reads nothing new.
	Bot string
	// BotTriggers are the labels whose bot lanes join the owned set of a
	// LEGACY lane's board read (the read's spine half). Empty on a bot
	// lane's own read: the lane already owns its own and its legacy cards.
	BotTriggers []string
	// Board says the target claims a card (a board mode with a
	// coordinator): a retryable row on the primary chain then counts as
	// serving — the sweeper owns it — instead of being taken over.
	Board bool
}

// laneVerdict is the answer every lane reader consumes, decided before
// anything is claimed or metered. answered names the Delivery row that
// already answers the event — a duplicate of it, or a serving claim that
// absorbs it (absorb says the caller must record the absorption); card
// names the board card that answers; nextKey is the primary chain's claim
// key when nothing answers; nextCardID is the deterministic id of the
// lane's next card; displaced is the event of a failed claim this delivery
// takes over. The first answer wins in walk order, so a claim in either
// store blocks a claim in the other.
type laneVerdict struct {
	answered *webhooks.Delivery
	card     *native.Issue
	absorb   bool
	// legacyEra says the answering record is not the read's own bot lane:
	// a row of a side family (Legacy, EventOnly) or a card carrying no bot
	// lane label. Such a record was made by a router that decided the whole
	// delivery, not one bot of it.
	legacyEra  bool
	nextKey    string
	nextCardID string
	displaced  *laneEvent
}

// readTriggerLane is the ONE lane reader, used by every lane reader: the
// spine's direct and board targets, and legacy's claimTriggerLane and
// cardTriggerLane. It reads, in this order, and writes nothing:
//
//  1. the absorbed-delivery rows under every base — the primary's
//     unfiltered, the side families' filtered on BotID == rd.Bot;
//  2. the generation chains under Legacy (read-only, rows of rd.Bot):
//     claimed by this event answers (a duplicate of that row), serving or
//     retryable absorbs (the sweeper owns a retryable secondary row), ended
//     continues; under EventOnly only claimed-by is checked;
//  3. when a board coordinator is wired — a card is then a claim — the
//     lane's cards: this event's delivery marker on any card of the subject
//     whose bot is the read's bot (or empty), owned or not, answers (a
//     duplicate); a non-terminal owned card absorbs; otherwise the lane's
//     next deterministic card id;
//  4. the primary chain: claimed by this event replays its key; serving
//     absorbs (skipped under overlap: supersede); retryable is taken over —
//     or, on a board target, counts as serving; ended continues to the next
//     generation, which yields nextKey.
//
// A claim found in either store answers, so the target then claims only in
// its own mode's store.
func (s *Server) readTriggerLane(ctx context.Context, cfg webhooks.Config, rd laneRead, ev laneEvent) (laneVerdict, error) {
	primary := ""
	if len(rd.Bases) > 0 {
		primary = rd.Bases[0]
	}
	if s.webhookDeliveries == nil {
		// The launch tail answers "webhooks unavailable" itself.
		return laneVerdict{nextKey: primary}, nil
	}
	// 1. the absorption recorded for this event, under any base that may
	// hold it and any identity the record may have been written under — the
	// primary's unfiltered, the side families' filtered on this bot.
	for _, base := range slices.Concat(rd.Bases, rd.Legacy, rd.EventOnly) {
		d, err := s.laneAbsorbedRow(ctx, base, ev)
		if err != nil {
			return laneVerdict{}, err
		}
		if d == nil {
			continue
		}
		side := !slices.Contains(rd.Bases, base)
		if side && rd.Bot != "" && d.BotID != rd.Bot {
			continue
		}
		return laneVerdict{answered: d, legacyEra: side}, nil
	}
	// 2. the side generation chains, read-only.
	for _, base := range rd.Legacy {
		d, absorb, err := s.walkSideChain(ctx, base, rd.Bot, false, ev)
		if err != nil {
			return laneVerdict{}, err
		}
		if d != nil {
			return laneVerdict{answered: d, absorb: absorb, legacyEra: true}, nil
		}
	}
	for _, base := range rd.EventOnly {
		d, _, err := s.walkSideChain(ctx, base, rd.Bot, true, ev)
		if err != nil {
			return laneVerdict{}, err
		}
		if d != nil {
			return laneVerdict{answered: d, legacyEra: true}, nil
		}
	}
	// 3. the board half, when a wired coordinator makes a card a claim.
	card, absorb, nextCardID, err := s.readLaneBoard(cfg, rd, ev)
	if err != nil {
		return laneVerdict{}, err
	}
	if card != nil {
		return laneVerdict{card: card, absorb: absorb, legacyEra: !botLaneCard(card)}, nil
	}
	// 4. the primary chain, which yields the claim key.
	v, err := s.walkPrimaryChain(ctx, cfg, primary, rd.Board, ev)
	if err != nil {
		return laneVerdict{}, err
	}
	v.nextCardID = nextCardID
	return v, nil
}

// walkSideChain walks one non-primary generation chain read-only: a row of
// another bot is skipped; this event's own claim answers (a duplicate of
// that row); a serving or retryable claim absorbs — the sweeper owns a
// retryable secondary row, so nobody takes it over; an ended claim
// continues. Under eventOnly only claimed-by is checked: the chain belongs
// to another trigger's lane, and its verdicts must not absorb a fresh
// event.
func (s *Server) walkSideChain(ctx context.Context, base, bot string, eventOnly bool, ev laneEvent) (*webhooks.Delivery, bool, error) {
	for gen := 0; ; gen++ {
		row, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, laneGenerationKey(base, gen))
		if errors.Is(err, webhooks.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read claim %d of the lane's side chain: %w", gen, err)
		}
		if bot != "" && row.BotID != bot {
			continue
		}
		if ev.claimedBy(row) {
			return &row, false, nil
		}
		if eventOnly {
			continue
		}
		state, err := s.laneClaimStateOf(ctx, row)
		if err != nil {
			return nil, false, fmt.Errorf("judge claim %d (delivery %s) of the lane's side chain: %w", gen, row.ID, err)
		}
		if state == claimEnded {
			continue
		}
		return &row, true, nil
	}
}

// walkPrimaryChain walks the primary chain, the one this read claims under:
// this event's claim replays its key (the tail replays or retries it); a
// serving claim absorbs — skipped under overlap: supersede, the policy the
// tail applies (supersededRunIDs reads launched rows only, so a claim still
// launching is absorbed instead); a retryable claim is taken over by this
// event, unless the target claims a card, in which case the row is a claim
// from a direct-mode period and the sweeper owns it — it counts as serving;
// an ended claim continues to the next generation, whose key the caller
// claims.
func (s *Server) walkPrimaryChain(ctx context.Context, cfg webhooks.Config, base string, boardTarget bool, ev laneEvent) (laneVerdict, error) {
	for gen := 0; ; gen++ {
		key := laneGenerationKey(base, gen)
		row, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, key)
		if errors.Is(err, webhooks.ErrNotFound) {
			return laneVerdict{nextKey: key}, nil
		}
		if err != nil {
			return laneVerdict{}, fmt.Errorf("read claim %d of the trigger lane: %w", gen, err)
		}
		if ev.claimedBy(row) {
			return laneVerdict{nextKey: key}, nil
		}
		state, err := s.laneClaimStateOf(ctx, row)
		if err != nil {
			return laneVerdict{}, fmt.Errorf("judge claim %d (delivery %s) of the trigger lane: %w", gen, row.ID, err)
		}
		switch state {
		case claimEnded:
			continue
		case claimRetryable:
			if boardTarget {
				return laneVerdict{answered: &row, absorb: true}, nil
			}
			displaced := eventOf(row)
			return laneVerdict{nextKey: key, displaced: &displaced}, nil
		}
		if row.Status == webhooks.StatusLaunched && overlapSupersedes(cfg) {
			continue
		}
		return laneVerdict{answered: &row, absorb: true}, nil
	}
}

// readLaneBoard is the board half of a lane read: the cards a wired
// coordinator turns into claims. It returns the answering card (a duplicate
// when absorb is false, a serving claim that absorbs when true), else the
// lane's next deterministic card id. Without a coordinator a card is a
// tracking card, not a claim, and is read by nobody.
func (s *Server) readLaneBoard(cfg webhooks.Config, rd laneRead, ev laneEvent) (*native.Issue, bool, string, error) {
	if rd.Lane == nil || s.cfg.CloudBoardCoordinator == nil || s.cfg.CloudBoardFor == nil {
		return nil, false, "", nil
	}
	bs := s.cfg.CloudBoardFor(cfg.TenantID)
	if bs == nil {
		return nil, false, "", nil
	}
	board, err := bs.Board()
	if err != nil {
		return nil, false, "", fmt.Errorf("read the board: %w", err)
	}
	cards, err := bs.List(native.ListFilter{Labels: []string{"cmd:" + rd.Lane.Subject}})
	if err != nil {
		return nil, false, "", fmt.Errorf("list the cards of %s: %w", rd.Lane.Subject, err)
	}
	bot := rd.Bot
	if bot == "" {
		bot = rd.Lane.Bot
	}
	var owned []*native.Issue
	for _, c := range cards {
		if rd.Lane.ownsCard(c) {
			owned = append(owned, c)
		}
	}
	if bot != "" && rd.Lane.Bot == "" {
		// The spine half of a legacy read: the elected bot's bot lanes hold
		// cards beside the legacy lane's own.
		for _, trig := range rd.BotTriggers {
			ln := triggerLane{Project: rd.Lane.Project, Subject: rd.Lane.Subject, Trigger: trig, Bot: bot}
			for _, c := range cards {
				if !ln.ownsCard(c) {
					continue
				}
				duplicate := false
				for _, o := range owned {
					if o.ID == c.ID {
						duplicate = true
						break
					}
				}
				if !duplicate {
					owned = append(owned, c)
				}
			}
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })
	// Event identity may read wider than ownership: the delivery marker on
	// any card of the subject whose bot is the read's bot (or the bot-less
	// era's) answers this event, owned or not — a card legacy carded for it
	// under another trigger (GitLab payload order) is a duplicate, and it
	// never joins the owned set that decides serving and the next id.
	scan := owned
	if bot != "" {
		scan = nil
		for _, c := range cards {
			if c.Bot == bot || c.Bot == "" {
				scan = append(scan, c)
			}
		}
		sort.Slice(scan, func(i, j int) bool { return scan[i].ID < scan[j].ID })
	}
	for _, c := range scan {
		if ev.cardedFor(c) {
			return c, false, "", nil
		}
	}
	ids := make([]string, 0, len(owned))
	for _, c := range owned {
		if st := board.StateByName(c.State); st == nil || !st.Terminal {
			return c, true, "", nil
		}
		ids = append(ids, c.ID)
	}
	return nil, false, laneCardID(cfg.TenantID, *rd.Lane, ids), nil
}

// claimTriggerLane is the direct lane's claim-side projection of
// readTriggerLane: the walk of ONE chain, with no lane and no spine half —
// the shape every lane-less caller before the spine consumed. Kept for its
// legacy callers.
func (s *Server) claimTriggerLane(ctx context.Context, cfg webhooks.Config, base string, ev laneEvent) (laneClaim, error) {
	v, err := s.readTriggerLane(ctx, cfg, laneRead{Bases: []string{base}}, ev)
	if err != nil {
		return laneClaim{}, err
	}
	if v.answered != nil {
		return laneClaim{answered: v.answered, absorb: v.absorb}, nil
	}
	return laneClaim{key: v.nextKey, displaced: v.displaced}, nil
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

// boardLaneVerdict is the board lane's narrow claim-side projection: the
// owned cards of ONE lane on one board store, no Delivery chains, no spine
// half, no bot filter on the event scan — the shape the board writer
// consumed before the one reader existed. Kept for its legacy callers; the
// carded path reads through readTriggerLane.
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
