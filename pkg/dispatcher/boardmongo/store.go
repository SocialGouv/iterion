// Package boardmongo is the Mongo-backed implementation of
// native.BoardStore — the cloud counterpart of the filesystem
// pkg/dispatcher/native.Store. It lets the shared boardops + the dispatcher
// run against a multi-replica cloud board with the same semantics as the
// local JSON store: same "native:"+uuid id scheme, same default board, same
// claim/state/transition rules, same event vocabulary.
//
// One store instance is bound to one tenant (the interface carries no tenant
// arg, mirroring the single-board filesystem store). The board domain types
// live in pkg/dispatcher/native; this package reuses them (types-only) so a
// board issue is byte-identical whether it came from the JSON store or Mongo.
package boardmongo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Collection names.
const (
	IssuesCollection = "board_issues"
	ConfigCollection = "board_config"
	EventsCollection = "board_events"
	// EffectsCollection holds the trigger-effect outbox rows (ADR-094):
	// one durable row per matched (board event, subscription) pair,
	// materialized BEFORE the trigger cursor advances.
	EffectsCollection = "trigger_effects"
)

// opTimeout bounds every Mongo call (the BoardStore interface carries no
// context, so each op uses a fresh bounded background context).
const opTimeout = 10 * time.Second

// Store is a tenant-scoped Mongo board.
type Store struct {
	tenant  string
	issues  *mongo.Collection
	config  *mongo.Collection
	events  *mongo.Collection
	effects *mongo.Collection
}

// New builds a tenant-scoped Mongo board store over db.
func New(db *mongo.Database, tenantID string) *Store {
	return &Store{
		tenant:  tenantID,
		issues:  db.Collection(IssuesCollection),
		config:  db.Collection(ConfigCollection),
		events:  db.Collection(EventsCollection),
		effects: db.Collection(EffectsCollection),
	}
}

// Compile-time assertion that *Store satisfies the board contract.
var _ native.BoardStore = (*Store)(nil)

// issueDoc wraps a native.Issue with a Mongo _id + tenant scope. The inner
// issue marshals via the bson default codec and round-trips back into a
// native.Issue unchanged.
type issueDoc struct {
	ID     string       `bson:"_id"`
	Tenant string       `bson:"tenant_id"`
	Issue  native.Issue `bson:"issue"`
}

type configDoc struct {
	Tenant string       `bson:"_id"`
	Board  native.Board `bson:"board"`
}

type eventDoc struct {
	Tenant string       `bson:"tenant_id"`
	Event  native.Event `bson:"event"`
}

func ctxWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), opTimeout)
}

// EnsureSchema creates the indexes the store relies on. Idempotent (index
// conflicts on re-run are absorbed).
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	issues := db.Collection(IssuesCollection)
	_, err := issues.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}}, Options: options.Index().SetName("tenant")},
		// Serves Coordinator.ListEligible — three cross-tenant queries per
		// 5s tick per replica, one of them over in_progress+blocked (the
		// states that only accumulate). Without it each is a COLLSCAN plus
		// a blocking in-memory sort that trips Mongo's non-indexed-sort
		// limit at scale. The trailing updatedat key serves BOTH sort
		// directions (an index walks backwards for free).
		{Keys: bson.D{
			{Key: "issue.state", Value: 1},
			{Key: "issue.claim", Value: 1},
			{Key: "issue.updatedat", Value: 1},
		}, Options: options.Index().SetName("eligible_by_updated")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("boardmongo: ensure issues index: %w", err)
	}
	events := db.Collection(EventsCollection)
	_, err = events.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "event.seq", Value: 1}}, Options: options.Index().SetName("tenant_seq")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("boardmongo: ensure events index: %w", err)
	}
	effects := db.Collection(EffectsCollection)
	_, err = effects.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Serves ClaimDue: eligible rows by tenant + state, ordered by their
		// next-eligibility instant.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "state", Value: 1}, {Key: "not_before", Value: 1}}, Options: options.Index().SetName("tenant_state_due")},
		// Rows embed the full normalized event (card body included) and a
		// board produces one per matched subscription forever — DONE rows
		// expire after a week. PARTIAL on state=done only: failed rows are
		// the dead-letter and must stay queryable until acted on.
		{Keys: bson.D{{Key: "updated_at", Value: 1}}, Options: options.Index().
			SetName("done_ttl").
			SetExpireAfterSeconds(7 * 24 * 3600).
			SetPartialFilterExpression(bson.D{{Key: "state", Value: "done"}})},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("boardmongo: ensure effects index: %w", err)
	}
	return nil
}

// --- board config ---

// Board returns the tenant's board config, defaulting to native.DefaultBoard
// when none is stored yet. A persisted board from an older iterion is
// schema-upgraded on READ (inbox / awaiting_input states) — the same upgrade
// the filesystem store persists in loadOrInitBoard. Normalizing here (not
// writing back) keeps reads race-free; the next SetBoard from the column
// editor persists the upgraded shape naturally, and SetState validation
// (which reads through this method) accepts the upgraded states either way.
func (s *Store) Board() *native.Board {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	var doc configDoc
	err := s.config.FindOne(ctx, bson.M{"_id": s.tenant}).Decode(&doc)
	if err != nil {
		return native.DefaultBoard()
	}
	b := doc.Board
	native.UpgradeBoardSchema(&b)
	return &b
}

// SetBoard persists the tenant's board config after validating it.
func (s *Store) SetBoard(b *native.Board) error {
	if b == nil {
		return errors.New("boardmongo: nil board")
	}
	if err := b.Validate(); err != nil {
		return err
	}
	b.UpdatedAt = time.Now().UTC()
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	_, err := s.config.ReplaceOne(ctx, bson.M{"_id": s.tenant}, configDoc{Tenant: s.tenant, Board: *b}, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("boardmongo: set board: %w", err)
	}
	return s.emit(native.Event{Type: native.EvtBoardUpdated})
}

// --- issues ---

func (s *Store) Create(in native.Issue) (*native.Issue, error) {
	if in.Title == "" {
		return nil, errors.New("issue: title required")
	}
	board := s.Board()
	if in.State == "" {
		in.State = board.States[0].Name
	}
	if board.StateByName(in.State) == nil {
		return nil, fmt.Errorf("issue: unknown state %q", in.State)
	}
	if err := board.ValidateFieldValues(in.Fields); err != nil {
		return nil, err
	}
	in.Blockers = native.NormalizeBlockers(in.Blockers)
	if in.ID == "" {
		in.ID = "native:" + uuid.NewString()
	} else if err := validateIssueID(in.ID); err != nil {
		return nil, err
	}
	if err := native.ValidateBlockers(s, in.ID, in.Blockers); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	in.CreatedAt = now
	in.UpdatedAt = now
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	if _, err := s.issues.InsertOne(ctx, issueDoc{ID: in.ID, Tenant: s.tenant, Issue: in}); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return nil, fmt.Errorf("issue: id %q already exists", in.ID)
		}
		return nil, fmt.Errorf("boardmongo: insert issue: %w", err)
	}
	if err := s.emit(native.Event{Type: native.EvtIssueCreated, IssueID: in.ID, Payload: map[string]any{"state": in.State, "title": in.Title}}); err != nil {
		return nil, err
	}
	if len(in.Blockers) > 0 {
		if err := s.emit(native.Event{
			Type: native.EvtIssueBlockersUpdated, IssueID: in.ID,
			Payload: map[string]any{"blockers": append([]string(nil), in.Blockers...)},
		}); err != nil {
			return nil, err
		}
	}
	clone := in
	return &clone, nil
}

func (s *Store) Get(id string) (*native.Issue, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return iss, nil
}

func (s *Store) get(ctx context.Context, id string) (*native.Issue, error) {
	doc, err := mongoutil.FindOne[issueDoc](ctx, s.issues, bson.M{"_id": id, "tenant_id": s.tenant}, tracker.ErrNotFound, "boardmongo: get issue")
	if err != nil {
		return nil, err
	}
	iss := doc.Issue
	return &iss, nil
}

// listAll fetches every issue for the tenant (the board is small; we filter +
// sort in Go to match native.Store's in-memory semantics exactly).
func (s *Store) listAll(ctx context.Context) ([]native.Issue, error) {
	cur, err := s.issues.Find(ctx, bson.M{"tenant_id": s.tenant})
	if err != nil {
		return nil, fmt.Errorf("boardmongo: list issues: %w", err)
	}
	var docs []issueDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("boardmongo: decode issues: %w", err)
	}
	out := make([]native.Issue, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Issue)
	}
	return out, nil
}

func (s *Store) List(filter native.ListFilter) ([]*native.Issue, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	all, err := s.listAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*native.Issue, 0, len(all))
	for i := range all {
		if !matchFilter(filter, all[i]) {
			continue
		}
		iss := all[i]
		out = append(out, &iss)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// replace persists the issue and stamps UpdatedAt.
func (s *Store) replace(ctx context.Context, iss *native.Issue) error {
	expireGiveUp(iss)
	_, err := s.issues.ReplaceOne(ctx, bson.M{"_id": iss.ID, "tenant_id": s.tenant}, issueDoc{ID: iss.ID, Tenant: s.tenant, Issue: *iss})
	if err != nil {
		return fmt.Errorf("boardmongo: replace issue: %w", err)
	}
	return nil
}

func (s *Store) Update(id string, p native.Patch) (*native.Issue, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Blockers != nil {
		next := native.NormalizeBlockers(*p.Blockers)
		if err := native.ValidateBlockers(s, id, next); err != nil {
			return nil, err
		}
		// Normalize before applyPatch so the stored list is clean.
		p.Blockers = &next
	}
	changed := applyPatch(iss, p, s.Board())
	if len(changed.fields) == 0 {
		return iss, changed.err
	}
	if changed.err != nil {
		return nil, changed.err
	}
	iss.UpdatedAt = time.Now().UTC()
	if err := s.replace(ctx, iss); err != nil {
		return nil, err
	}
	if err := s.emit(native.Event{Type: native.EvtIssueUpdated, IssueID: iss.ID, Payload: map[string]any{"changed": changed.fields}}); err != nil {
		return nil, err
	}
	for _, f := range changed.fields {
		if f == "blockers" {
			if err := s.emit(native.Event{
				Type: native.EvtIssueBlockersUpdated, IssueID: iss.ID,
				Payload: map[string]any{"blockers": append([]string(nil), iss.Blockers...)},
			}); err != nil {
				return nil, err
			}
			break
		}
	}
	return iss, nil
}

func (s *Store) SetState(id, newState string) (*native.Issue, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.Board().StateByName(newState) == nil {
		return nil, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, newState)
	}
	if iss.State == newState {
		return iss, nil
	}
	old := iss.State
	iss.State = newState
	iss.UpdatedAt = time.Now().UTC()
	if err := s.replace(ctx, iss); err != nil {
		return nil, err
	}
	if err := s.emit(native.Event{Type: native.EvtIssueState, IssueID: iss.ID, Payload: map[string]any{"from": old, "to": newState}}); err != nil {
		return nil, err
	}
	if newState == native.StateDone {
		// Best-effort: do not roll back a successful done transition.
		_ = native.PromoteUnblockedDependents(s, id)
	}
	return iss, nil
}

func (s *Store) Delete(id string) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	if err := mongoutil.DeleteOneChecked(ctx, s.issues, bson.M{"_id": id, "tenant_id": s.tenant}, tracker.ErrNotFound, "boardmongo: delete issue"); err != nil {
		return err
	}
	return s.emit(native.Event{Type: native.EvtIssueDeleted, IssueID: id})
}

// Claim sets the claim marker via a conditional update (CAS): the update only
// matches when the issue is unclaimed OR already held by this marker, so two
// replicas racing to claim cannot both win. Idempotent for the same marker.
func (s *Store) Claim(id, marker string) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	now := time.Now().UTC()
	res, err := s.issues.UpdateOne(ctx,
		bson.M{"_id": id, "tenant_id": s.tenant, "$or": bson.A{bson.M{"issue.claim": ""}, bson.M{"issue.claim": marker}}},
		bson.M{"$set": bson.M{"issue.claim": marker, "issue.updatedat": now}},
	)
	if err != nil {
		return fmt.Errorf("boardmongo: claim: %w", err)
	}
	if res.MatchedCount == 0 {
		// Either the issue doesn't exist, or it's held by another marker.
		iss, gerr := s.get(ctx, id)
		if gerr != nil {
			return gerr
		}
		return fmt.Errorf("%w: held by %s", tracker.ErrClaimConflict, iss.Claim)
	}
	if res.ModifiedCount == 0 {
		return nil // already held by this marker (idempotent)
	}
	return s.emit(native.Event{Type: native.EvtIssueClaimed, IssueID: id, Payload: map[string]any{"marker": marker}})
}

func (s *Store) Release(id, marker string) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if iss.Claim == "" {
		return nil
	}
	if iss.Claim != marker {
		return fmt.Errorf("%w: held by %s", tracker.ErrClaimConflict, iss.Claim)
	}
	iss.Claim = ""
	iss.UpdatedAt = time.Now().UTC()
	if err := s.replace(ctx, iss); err != nil {
		return err
	}
	return s.emit(native.Event{Type: native.EvtIssueReleased, IssueID: id, Payload: map[string]any{"marker": marker}})
}

func (s *Store) SetLastRun(id, runID, workdir string) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if iss.LastRunID == runID && iss.LastWorkdir == workdir {
		return nil
	}
	now := time.Now().UTC()
	iss.LastRunID = runID
	iss.LastWorkdir = workdir
	iss.Runs = native.AppendRunRef(iss.Runs, runID, workdir, now)
	iss.UpdatedAt = now
	if err := s.replace(ctx, iss); err != nil {
		return err
	}
	return s.emit(native.Event{Type: native.EvtIssueLastRun, IssueID: id, Payload: map[string]any{"run_id": runID, "workdir": workdir}})
}

// SetAwaitingInput denormalizes onto the issue whether its most recent
// run parked awaiting human/operator input (see native.Issue.AwaitingInput).
// Idempotent — setting the flag to its current value is a no-op. Mirrors
// SetLastRun: read → set → replace → bump UpdatedAt → emit EvtIssueUpdated.
func (s *Store) SetAwaitingInput(id string, v bool) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if iss.AwaitingInput == v {
		return nil
	}
	iss.AwaitingInput = v
	iss.UpdatedAt = time.Now().UTC()
	if err := s.replace(ctx, iss); err != nil {
		return err
	}
	return s.emit(native.Event{Type: native.EvtIssueUpdated, IssueID: id, Payload: map[string]any{"awaiting_input": v}})
}

// SetGaveUp stamps (nil clears) the dispatcher's give-up on an issue — the
// record that its current state was written by an exhausted retry budget and
// not by a human (see native.Issue.GaveUp). Idempotent on the fields that
// decide behaviour; mirrors SetAwaitingInput: read → set → replace → bump
// UpdatedAt → emit, here EvtIssueGaveUp.
func (s *Store) SetGaveUp(id string, g *native.GiveUp) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	want, ok := giveUpToRecord(iss, g)
	if !ok {
		return nil
	}
	// Compared against what would ACTUALLY be written, so a repeat call is a
	// real no-op rather than a re-write that churns UpdatedAt.
	if sameGiveUp(iss.GaveUp, want) {
		return nil
	}
	// stamped is what actually landed on the issue (nil for a clear); the
	// event reports IT, never the caller's g, which may name a state the
	// store overrode.
	var stamped *native.GiveUp
	if want == nil {
		iss.GaveUp = nil
	} else {
		stamp := *want
		if stamp.At.IsZero() {
			stamp.At = time.Now().UTC()
		}
		iss.GaveUp = &stamp
		stamped = &stamp
	}
	iss.UpdatedAt = time.Now().UTC()
	if err := s.replace(ctx, iss); err != nil {
		return err
	}
	payload := map[string]any{"gave_up": stamped != nil}
	if stamped != nil {
		payload["run_id"] = stamped.RunID
		// The state that was STAMPED, not the one the caller believed — the
		// two differ when a give-up raced an operator move, and the audit
		// record exists to reconstruct what actually happened.
		payload["state"] = stamped.State
		payload["attempts"] = stamped.Attempts
	}
	return s.emit(native.Event{Type: native.EvtIssueGaveUp, IssueID: id, Payload: payload})
}

// expireGiveUp drops a stamp that no longer describes the state the issue is
// being written in, on the ONE write path — the Mongo twin of the filesystem
// store's rule, which is what makes give-up staleness permanent instead of
// reversible. See native.Store.expireGiveUp for the full argument.
func expireGiveUp(iss *native.Issue) {
	if iss.GaveUp != nil && iss.GaveUp.State != iss.State {
		iss.GaveUp = nil
	}
}

// giveUpToRecord resolves a caller's stamp against the issue as it stands,
// returning the value to write and whether to write at all.
//
// A give-up describes a ticket that is still where the give-up PUT it. When
// the ticket has already moved — an operator got there between the terminal
// move and the stamp — the give-up is superseded, and recording it would put
// the operator's own choice under a "the dispatcher gave up and filed this
// ticket as …" banner. Nothing is written; the state change already stands in
// the audit log.
//
// A stamp arriving without a state is filled in from the issue, so the value
// compared for idempotence and the value written are always the same thing.
func giveUpToRecord(iss *native.Issue, g *native.GiveUp) (*native.GiveUp, bool) {
	if g == nil {
		return nil, true
	}
	out := *g
	if out.State == "" {
		out.State = iss.State
	}
	if out.State != iss.State {
		return nil, false
	}
	return &out, true
}

// sameGiveUp compares two stamps on the fields that decide behaviour (the
// timestamp is provenance, not identity), so a re-stamp of the same give-up
// writes nothing. Mirrors the filesystem store's predicate.
func sameGiveUp(a, b *native.GiveUp) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.RunID == b.RunID && a.State == b.State && a.Attempts == b.Attempts
}

// AddComment appends a note to the issue's discussion thread and returns
// the updated issue plus the created comment.
func (s *Store) AddComment(id, author, body string) (*native.Issue, *native.Comment, error) {
	if strings.TrimSpace(body) == "" {
		return nil, nil, errors.New("comment: body required")
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	c := native.Comment{
		ID:        uuid.NewString(),
		Author:    author,
		Body:      body,
		CreatedAt: time.Now().UTC(),
	}
	iss.Comments = append(iss.Comments, c)
	iss.UpdatedAt = c.CreatedAt
	if err := s.replace(ctx, iss); err != nil {
		return nil, nil, err
	}
	if err := s.emit(native.Event{Type: native.EvtIssueComment, IssueID: id, Payload: map[string]any{"comment_id": c.ID, "author": author}}); err != nil {
		return nil, nil, err
	}
	return iss, &c, nil
}

func (s *Store) Resolve(prefix string) (string, error) {
	want := prefix
	if !strings.HasPrefix(prefix, "native:") {
		want = "native:" + prefix
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	all, err := s.listAll(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, iss := range all {
		if iss.ID == want || strings.HasPrefix(iss.ID, want) {
			matches = append(matches, iss.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", tracker.ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("boardmongo: ambiguous prefix %q matches %d issues", prefix, len(matches))
	}
}

func (s *Store) ScanEvents(visit func(*native.Event) bool) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	cur, err := s.events.Find(ctx, bson.M{"tenant_id": s.tenant}, options.Find().SetSort(bson.D{{Key: "event.seq", Value: 1}}))
	if err != nil {
		return fmt.Errorf("boardmongo: scan events: %w", err)
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var doc eventDoc
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		e := doc.Event
		if !visit(&e) {
			break
		}
	}
	return cur.Err()
}

func (s *Store) AggregateLabels() []native.LabelUsage {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	all, err := s.listAll(ctx)
	if err != nil {
		return nil
	}
	type acc struct {
		count int
		last  string
	}
	agg := map[string]*acc{}
	for _, iss := range all {
		stamp := iss.UpdatedAt.UTC().Format(time.RFC3339)
		for _, lbl := range iss.Labels {
			if lbl == "" {
				continue
			}
			if cur, ok := agg[lbl]; ok {
				cur.count++
				if stamp > cur.last {
					cur.last = stamp
				}
			} else {
				agg[lbl] = &acc{count: 1, last: stamp}
			}
		}
	}
	out := make([]native.LabelUsage, 0, len(agg))
	for lbl, a := range agg {
		out = append(out, native.LabelUsage{Label: lbl, Count: a.count, LastUsedAt: a.last})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// --- helpers ---

// emit appends an event with a monotonic per-tenant seq.
func (s *Store) emit(evt native.Event) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	seq, err := s.nextSeq(ctx)
	if err != nil {
		return err
	}
	evt.Seq = seq
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	if _, err := s.events.InsertOne(ctx, eventDoc{Tenant: s.tenant, Event: evt}); err != nil {
		return fmt.Errorf("boardmongo: emit event: %w", err)
	}
	return nil
}

// nextSeq returns a monotonic per-tenant event sequence via an atomic $inc on
// a counter doc in the config collection (id "seq:<tenant>").
func (s *Store) nextSeq(ctx context.Context) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := s.config.FindOneAndUpdate(ctx,
		bson.M{"_id": "seq:" + s.tenant},
		bson.M{"$inc": bson.M{"seq": int64(1)}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return 0, fmt.Errorf("boardmongo: next seq: %w", err)
	}
	return doc.Seq, nil
}

func matchFilter(f native.ListFilter, iss native.Issue) bool {
	if len(f.States) > 0 && !containsStr(f.States, iss.State) {
		return false
	}
	for _, want := range f.Labels {
		if !containsStr(iss.Labels, want) {
			return false
		}
	}
	if f.Assignee != "" && iss.Assignee != f.Assignee {
		return false
	}
	if f.Claimed != nil {
		if *f.Claimed != (iss.Claim != "") {
			return false
		}
	}
	return true
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

type patchResult struct {
	fields []string
	err    error
}

// applyPatch mutates iss per p, returning the changed field names. Mirrors
// native.Store.Update field-by-field, including field-value validation.
func applyPatch(iss *native.Issue, p native.Patch, board *native.Board) patchResult {
	var changed []string
	if p.Title != nil && *p.Title != iss.Title {
		iss.Title = *p.Title
		changed = append(changed, "title")
	}
	if p.Body != nil && *p.Body != iss.Body {
		iss.Body = *p.Body
		changed = append(changed, "body")
	}
	if p.Labels != nil {
		iss.Labels = append([]string(nil), (*p.Labels)...)
		changed = append(changed, "labels")
	}
	if p.Priority != nil && *p.Priority != iss.Priority {
		iss.Priority = *p.Priority
		changed = append(changed, "priority")
	}
	if p.Assignee != nil && *p.Assignee != iss.Assignee {
		iss.Assignee = *p.Assignee
		changed = append(changed, "assignee")
	}
	if p.Blockers != nil {
		iss.Blockers = append([]string(nil), (*p.Blockers)...)
		changed = append(changed, "blockers")
	}
	if len(p.Fields) > 0 {
		merged := map[string]any{}
		for k, v := range iss.Fields {
			merged[k] = v
		}
		for k, v := range p.Fields {
			if v == nil {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if err := board.ValidateFieldValues(merged); err != nil {
			return patchResult{err: err}
		}
		iss.Fields = merged
		changed = append(changed, "fields")
	}
	if p.Bot != nil && *p.Bot != iss.Bot {
		iss.Bot = *p.Bot
		changed = append(changed, "bot")
	}
	if p.BotArgs != nil {
		var next map[string]string
		if len(*p.BotArgs) > 0 {
			next = make(map[string]string, len(*p.BotArgs))
			for k, v := range *p.BotArgs {
				next[k] = v
			}
		}
		iss.BotArgs = next
		changed = append(changed, "bot_args")
	}
	if p.External != nil {
		ext := *p.External
		iss.External = &ext
		changed = append(changed, "external")
	}
	return patchResult{fields: changed}
}

// validateIssueID mirrors native's id rule: "native:"+uuid.
func validateIssueID(id string) error {
	raw, ok := strings.CutPrefix(id, "native:")
	if !ok || raw == "" {
		return fmt.Errorf("boardmongo: invalid issue id %q", id)
	}
	if parsed, err := uuid.Parse(raw); err != nil || parsed.String() != raw {
		return fmt.Errorf("boardmongo: invalid issue id %q", id)
	}
	return nil
}
