package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// priceFeedExecAllowed reads the exec hatch as the process inherited it.
func priceFeedExecAllowed() bool {
	return envtrust.Inherited(priceFeedExecEnv) == "1"
}

// The gateway price feed's server side (ADR-163): the effective-table
// fold the publisher seals into every run bundle, and the refresh both
// the elected reconciler (pricefeed_sync.go) and the manual trigger
// (pricefeed_routes.go) run. Precedence everywhere: an answering feed
// entry (a fetched feed's while fresh, a declaration's always) answers
// over the operator table for the ids it carries, the more specific
// scope over the less (team over org over platform); a stale or failing
// feed steps down with a warning, never a refusal. A level the fold
// cannot read is no answer at all: the launch or resume that folds it is
// refused, retryably (PriceFeedUnreadError, ADR-166 §4).

// priceFeedReadTimeout bounds one level's reads in the launch-path fold
// (its config document and its fetched feeds' states): a slow database
// must not hold a launch. Each level reads under its own deadline, so a
// fold's worst case is one deadline per level, three levels; a level that
// misses it is unread.
const priceFeedReadTimeout = 2 * time.Second

// PriceFeedUnreadErrorCode is the stable API code of a launch or resume
// refused because the run's price-feed fold could not read a level.
const PriceFeedUnreadErrorCode = "PRICE_FEED_UNREAD"

// PriceFeedUnreadError is a fold that could not read every level it folds:
// a config document or a fetched feed's state that did not answer, deadline
// included. The run's table would lack the level's prices and accepted
// efforts — an answer the server does not know, where a missing effort
// list sends a level the gateway refuses — so the launch or resume that
// folds it is refused, and the same request succeeds once the level reads.
// The message names the levels and not the read errors, which name
// internal topology (a database's replica set, its hosts): it travels to
// whoever launched, and the fold's warnf carries the causes to the
// operator's log. Err joins them for errors.Is and errors.As.
type PriceFeedUnreadError struct {
	// Levels names the unread levels, least specific first: "platform",
	// "org <id>", "team <id>".
	Levels []string
	Err    error
}

func (e *PriceFeedUnreadError) Error() string {
	return fmt.Sprintf("price feed: %s unread — the run's prices and accepted efforts must be known, never guessed; retry once it reads (the server log names the cause)", strings.Join(e.Levels, ", "))
}

func (e *PriceFeedUnreadError) Unwrap() error { return e.Err }

// Code and Retryable let the launch and resume answers expose stable
// machine-readable semantics.
func (e *PriceFeedUnreadError) Code() string    { return PriceFeedUnreadErrorCode }
func (e *PriceFeedUnreadError) Retryable() bool { return true }

// EffectivePriceFeed folds one run's effective table — the platform's,
// the org's and the team's answering feeds, the more specific scope
// answering per id — and answers the wire bytes the bundle seals. nil
// when no feed answers: the operator table is the floor and the bundle
// serializes unchanged. The named ids are the run's own gateway models,
// the narrowing fallback when the folded table exceeds the channel's
// byte budget.
//
// A level whose read fails answers a *PriceFeedUnreadError naming it and
// no table; each read error reaches warnf — the operator's log, never a
// member.
func EffectivePriceFeed(ctx context.Context, configs pricefeed.ConfigStore, states pricefeed.StateStore, orgID, teamID string, named []string, warnf func(format string, args ...any)) (json.RawMessage, error) {
	return effectivePriceFeed(ctx, configs, states, orgID, teamID, named, warnf, nil)
}

// effectivePriceFeed is EffectivePriceFeed with its fold warnings passed
// through once (nil: every warning at every fold).
func effectivePriceFeed(ctx context.Context, configs pricefeed.ConfigStore, states pricefeed.StateStore, orgID, teamID string, named []string, warnf func(format string, args ...any), once *priceFeedWarnOnce) (json.RawMessage, error) {
	fold := foldPriceFeed(ctx, configs, states, orgID, teamID, pricefeed.FoldConfig{Named: named, Warnf: warnf}, once)
	if err := fold.unreadError(); err != nil {
		if warnf != nil {
			for _, read := range fold.scopes {
				if read.err != nil {
					warnf("price feed: %s unread — the launch or resume folding it is refused: %v", read.scope.what, read.err)
				}
			}
		}
		return nil, err
	}
	return fold.encode(warnf), nil
}

// PublisherPriceFeedEffective is the publisher's price-feed fold
// (cloudpublisher.Config.PriceFeedEffective) over the deployment's feed
// stores: EffectivePriceFeed for the org and the team the publisher
// passes, in that order, the run's named gateway ids narrowing an
// over-budget table, its warnings to warnf. The server binary wires it,
// and the pool-retirement E2E launches through it.
//
// It folds at every launch and resume, and a layout ADR-166 recommends
// warns at every fold (a declaration listed before a preset that states
// flags it leaves unstated): each fold warning reaches warnf once per
// distinct line and fold input on this replica (priceFeedWarnOnce), so a
// line the operator has read does not bury the next one. A level's read
// error reaches warnf at every refusal; the effective view shows every
// warning at every read.
func PublisherPriceFeedEffective(configs pricefeed.ConfigStore, states pricefeed.StateStore, warnf func(format string, args ...any)) func(ctx context.Context, orgID, teamID string, named []string) (json.RawMessage, error) {
	once := &priceFeedWarnOnce{}
	return func(ctx context.Context, orgID, teamID string, named []string) (json.RawMessage, error) {
		return effectivePriceFeed(ctx, configs, states, orgID, teamID, named, warnf, once)
	}
}

// priceFeedWarnOnceBound caps the lines priceFeedWarnOnce remembers; past
// it the memory starts over, and each line may be said once more.
const priceFeedWarnOnceBound = 4096

// priceFeedWarnOnce passes each fold warning through once per (line, fold
// input): the input is every level's config stamp and its fetched feeds'
// refresh stamps, so a configuration change or a refresh says the line
// again — the drift report's per-refresh bound (ADR-163) — and a fold over
// the same input says nothing new.
type priceFeedWarnOnce struct {
	mu   sync.Mutex
	said map[string]bool
}

// wrap answers warnf passing each line once for the fold input stamp.
func (o *priceFeedWarnOnce) wrap(stamp string, warnf func(format string, args ...any)) func(format string, args ...any) {
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		key := stamp + "\x00" + line
		o.mu.Lock()
		said := o.said[key]
		if !said {
			if o.said == nil || len(o.said) >= priceFeedWarnOnceBound {
				o.said = map[string]bool{}
			}
			o.said[key] = true
		}
		o.mu.Unlock()
		if !said {
			warnf("%s", line)
		}
	}
}

// foldInputStamp names what one fold read: each level's config document
// and its update stamp, and each fetched feed state's refresh stamp.
func foldInputStamp(scopes []priceFeedScopeRead) string {
	var b strings.Builder
	for _, read := range scopes {
		b.WriteString(read.scope.docID)
		if read.state == nil {
			b.WriteString("@none;")
			continue
		}
		b.WriteString("@" + read.state.Config.UpdatedAt.UTC().Format(time.RFC3339Nano))
		names := make([]string, 0, len(read.state.States))
		for name := range read.state.States {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			b.WriteString("," + name + "@" + read.state.States[name].RefreshedAt.UTC().Format(time.RFC3339Nano))
		}
		b.WriteString(";")
	}
	return b.String()
}

// priceFeedFold is one fold's read and answer: the levels it read, least
// specific first, the instant it judged freshness at, the table — nil
// when no feed answers — and the level each id's entry and efforts answer
// from (by the level's warning name, priceFeedScope.what).
type priceFeedFold struct {
	now      time.Time
	scopes   []priceFeedScopeRead
	table    *pricefeed.Table
	answered pricefeed.Provenance
}

// priceFeedScopeRead is one level as the fold read it: its config and
// fetched states (nil when the level holds no feed), or the read error
// that left it unread.
type priceFeedScopeRead struct {
	scope priceFeedScope
	state *pricefeed.ScopeState
	err   error
}

// foldPriceFeed reads every level of one run's fold, each under its own
// deadline, and folds the levels it read at one instant by cfg, its Now
// set here — the one body the bundle's table (EffectivePriceFeed) and the
// effective view (pricefeed_effective.go) both answer from, so the view
// cannot show a table the run would not seal. A level whose read fails
// carries its error in scopes and folds nothing: the run refuses it
// (unreadError), the view reports it. A non-nil once passes cfg.Warnf's
// lines once per fold input.
func foldPriceFeed(ctx context.Context, configs pricefeed.ConfigStore, states pricefeed.StateStore, orgID, teamID string, cfg pricefeed.FoldConfig, once *priceFeedWarnOnce) priceFeedFold {
	fold := priceFeedFold{now: time.Now()}
	if configs == nil || states == nil {
		return fold
	}
	var answering []pricefeed.ScopeState
	for _, scope := range priceFeedFoldScopes(orgID, teamID) {
		readCtx, cancel := context.WithTimeout(ctx, priceFeedReadTimeout)
		state, err := priceFeedScopeState(readCtx, configs, states, scope.docID)
		cancel()
		fold.scopes = append(fold.scopes, priceFeedScopeRead{scope: scope, state: state, err: err})
		if err != nil {
			continue
		}
		if state != nil {
			state.Label = scope.what
			answering = append(answering, *state)
		}
	}
	if len(answering) > 0 {
		cfg.Now = fold.now
		if once != nil && cfg.Warnf != nil {
			cfg.Warnf = once.wrap(foldInputStamp(fold.scopes), cfg.Warnf)
		}
		fold.table, fold.answered = pricefeed.FoldProvenance(cfg, answering...)
	}
	return fold
}

// unreadError answers the fold's unread levels as one
// *PriceFeedUnreadError, nil when every level read.
func (f priceFeedFold) unreadError() error {
	var levels []string
	var errs []error
	for _, read := range f.scopes {
		if read.err != nil {
			levels = append(levels, read.scope.what)
			errs = append(errs, fmt.Errorf("%s: %w", read.scope.what, read.err))
		}
	}
	if len(levels) == 0 {
		return nil
	}
	return &PriceFeedUnreadError{Levels: levels, Err: errors.Join(errs...)}
}

// encode answers the folded table's wire bytes; nil when no feed answers
// or the table does not encode (warned).
func (f priceFeedFold) encode(warnf func(format string, args ...any)) json.RawMessage {
	if f.table == nil {
		return nil
	}
	raw, err := json.Marshal(f.table)
	if err != nil {
		if warnf != nil {
			warnf("price feed: the folded table does not encode: %v", err)
		}
		return nil
	}
	return raw
}

// priceFeedScope is one level of the fold: its config document id and
// the name warnings and views give it.
type priceFeedScope struct {
	level string // "platform" | "org" | "team"
	id    string // the org or team id; "" for the platform
	docID string
	what  string
}

// priceFeedFoldScopes lists the levels one run's fold reads, least
// specific first — the order pricefeed.Fold takes them in. The platform
// is always read; an org or a team the run has none of is no level.
func priceFeedFoldScopes(orgID, teamID string) []priceFeedScope {
	scopes := []priceFeedScope{{level: "platform", docID: pricefeed.PlatformConfigDocID, what: "platform"}}
	if orgID != "" {
		scopes = append(scopes, priceFeedScope{level: "org", id: orgID, docID: pricefeed.OrgConfigDocID(orgID), what: "org " + orgID})
	}
	if teamID != "" {
		scopes = append(scopes, priceFeedScope{level: "team", id: teamID, docID: pricefeed.TeamConfigDocID(teamID), what: "team " + teamID})
	}
	return scopes
}

// priceFeedScopeState reads one scope's config document and the states
// of the feeds the fold reads them for: enabled fetched feeds. A
// declaration answers from its config and writes no state (a state
// document under its name is never its word), and a disabled feed does
// not answer; neither costs a read that could leave the level unread. A
// scope with no config document contributes nothing; any read that fails
// fails the scope.
func priceFeedScopeState(ctx context.Context, configs pricefeed.ConfigStore, states pricefeed.StateStore, docID string) (*pricefeed.ScopeState, error) {
	rec, err := configs.Get(ctx, docID)
	if err != nil {
		return nil, err
	}
	if rec == nil || len(rec.Feeds) == 0 {
		return nil, nil
	}
	out := &pricefeed.ScopeState{Config: rec, States: map[string]*pricefeed.StateRecord{}}
	for _, feed := range rec.Feeds {
		if !feed.Enabled || pricefeed.IsStatic(feed.Type) {
			continue
		}
		state, err := states.Get(ctx, pricefeed.StateDocID(docID, feed.Name))
		if err != nil {
			return nil, fmt.Errorf("feed %s state: %w", feed.Name, err)
		}
		if state != nil {
			out.States[feed.Name] = state
		}
	}
	return out, nil
}

// priceFeedExecEnv is the deployment operator's hatch over the exec
// source type: an exec feed runs an operator-supplied command ON THE
// SERVER, so it is the platform operator's capability — a tenant admin
// must not buy command execution with a feed list. The write route gates
// the type at super-admin; THIS gate is the second layer, at the one
// body every execution path share (the reconciler, the manual sync), so
// a document no route should have been able to write still does not
// execute. Read inherited (envtrust): a project `.env` plants nothing.
const priceFeedExecEnv = "ITERION_PRICE_FEED_EXEC"

// priceFeedRefresh runs one feed's sync through the shared Refresh and
// writes the durable state — the ONE body the elected reconciler and the
// manual trigger execute, so a forced refresh and the next pass converge
// on the same answer. The drift report runs on every successful refresh:
// one line per drifting id per refresh, the "not per dispatch" bound.
//
// A static feed (a declaration) is judged and reported, never persisted:
// its answer is its config, and a state document under its name would
// outlive it — a fetched feed later written under the same name would
// inherit the declaration's entries as its own retained, fresh word.
func (s *Server) priceFeedRefresh(ctx context.Context, scopeDocID string, feed pricefeed.FeedConfig) (pricefeed.SyncOutcome, error) {
	if feed.Type == pricefeed.TypeExec && !priceFeedExecAllowed() {
		err := fmt.Errorf("exec price feeds are the platform operator's: export %s=1 in the deployment's environment to arm them (super-admin configures them)", priceFeedExecEnv)
		stateID := pricefeed.StateDocID(scopeDocID, feed.Name)
		// Staleness, not amnesia — the same rule Refresh's own failure
		// path holds: a deployment restarted without the hatch keeps the
		// last good entries, aging out visibly, instead of wiping them;
		// a record another source type wrote is not this feed's to keep.
		state := pricefeed.StateRecord{Feed: feed.Name, Type: feed.Type, Status: pricefeed.StateFailing, LastAttempt: time.Now(), LastError: err.Error()}
		if prev, getErr := s.priceFeedStates.Get(ctx, stateID); getErr == nil && prev.Of(feed.Type) != nil {
			state.Entries = prev.Entries
			state.RefreshedAt = prev.RefreshedAt
			state.FreshFor = prev.FreshFor
			state.Status = pricefeed.DeriveStatus(prev.Entries, prev.RefreshedAt, prev.FreshFor, err.Error(), time.Now())
		}
		if putErr := s.priceFeedStates.Put(ctx, stateID, &state); putErr != nil {
			return pricefeed.SyncOutcome{}, putErr
		}
		return pricefeed.SyncOutcome{Feed: feed.Name, Status: pricefeed.StateFailing, Error: err.Error()}, nil
	}
	operatorTable, err := compatgw.OperatorTable(os.Getenv)
	if err != nil {
		// Malformed: the launch seam already refuses it loudly; drift
		// compares against nothing rather than refusing the refresh.
		operatorTable = nil
	}
	if src, ok := pricefeed.SourceFor(feed.Type); ok {
		if static, ok := src.(pricefeed.StaticSource); ok {
			entries, err := static.Entries(feed.Params)
			if err != nil {
				return pricefeed.SyncOutcome{Feed: feed.Name, Status: pricefeed.StateFailing, Error: sanitizeFeedError(err.Error())}, nil
			}
			for _, d := range pricefeed.DriftReport(feed.Name, entries, operatorTable) {
				s.warnf("%s", pricefeed.FormatDrift(d))
			}
			return pricefeed.SyncOutcome{Feed: feed.Name, Status: pricefeed.StateDeclared}, nil
		}
	}
	stateID := pricefeed.StateDocID(scopeDocID, feed.Name)
	prev, err := s.priceFeedStates.Get(ctx, stateID)
	if err != nil {
		return pricefeed.SyncOutcome{}, err
	}
	// The stamp this refresh BASED its retained state on: a failing
	// refresh writes the PREVIOUS entries, and a slower failing refresh
	// overlapping a successful one must not revert the newer tariffs
	// (the gate's finding). The stamp check below refuses that write.
	var basedOn time.Time
	if prev != nil {
		basedOn = prev.UpdatedAt
	}
	state, outcome := pricefeed.Refresh(ctx, feed, prev, operatorTable, time.Now())
	// A fetch error embeds the configured URL — credentials in its query
	// or userinfo must not reach the durable state and the member view.
	state.LastError = sanitizeFeedError(state.LastError)
	outcome.Error = sanitizeFeedError(outcome.Error)
	for _, d := range pricefeed.DriftReport(feed.Name, state.Entries, operatorTable) {
		s.warnf("%s", pricefeed.FormatDrift(d))
	}
	if outcome.Status != pricefeed.StateOK {
		// The failing path writes RETAINED entries: if another writer
		// landed while this fetch was in flight, ours is the older
		// word — keep theirs (a successful fetch's data is real
		// whichever writer wins; the retained snapshot is not).
		if current, getErr := s.priceFeedStates.Get(ctx, stateID); getErr == nil && current != nil && !current.UpdatedAt.Equal(basedOn) {
			return outcome, nil
		}
	}
	if err := s.priceFeedStates.Put(ctx, stateID, &state); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// urlInError matches the URL an error message embeds, so its query and
// userinfo can be stripped wherever the message travels.
var urlInError = regexp.MustCompile(`(?i)https?://[^\s"']+`)

// sanitizeFeedError rewrites every URL in an error message to its
// credential-free form (userinfo and query dropped).
func sanitizeFeedError(msg string) string {
	if msg == "" || !strings.Contains(msg, "://") {
		return msg
	}
	return urlInError.ReplaceAllStringFunc(msg, func(m string) string {
		return strings.TrimSuffix(sanitizeFeedURL(m), "%!(NOVERB)")
	})
}

// priceFeedDerivedStatus recomputes a state's first-class status at READ
// time — the stored word was true when the reconciler wrote it, and
// staleness arrives with the clock, not with a write.
func priceFeedDerivedStatus(state *pricefeed.StateRecord, now time.Time) string {
	if state == nil {
		return pricefeed.StateNever
	}
	return pricefeed.DeriveStatus(state.Entries, state.RefreshedAt, state.FreshFor, state.LastError, now)
}
