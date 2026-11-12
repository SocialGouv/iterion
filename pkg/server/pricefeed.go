package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
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
// feed steps down with a warning, never a refusal.

// priceFeedReadTimeout bounds one scope's reads in the launch-path fold:
// a slow database must not hold a launch, and a feed that cannot be read
// steps down to the operator floor with a warning — never a refusal.
const priceFeedReadTimeout = 2 * time.Second

// EffectivePriceFeed folds one run's effective table — the platform's,
// the org's and the team's answering feeds, the more specific scope
// answering per id — and answers the wire bytes the bundle seals. nil
// when no feed answers: the operator table is the floor and the bundle
// serializes unchanged. The named ids are the run's own gateway models,
// the narrowing fallback when the folded table exceeds the channel's
// byte budget.
//
// A level whose read fails warns with the read error: this warning
// reaches the operator's log, never a member.
func EffectivePriceFeed(ctx context.Context, configs pricefeed.ConfigStore, states pricefeed.StateStore, orgID, teamID string, named []string, warnf func(format string, args ...any)) json.RawMessage {
	unread := func(what string, err error) {
		if warnf != nil {
			warnf("price feed: %s unread — the operator table answers: %v", what, err)
		}
	}
	return foldPriceFeed(ctx, configs, states, orgID, teamID, pricefeed.FoldConfig{Named: named, Warnf: warnf}, unread).encode(warnf)
}

// priceFeedFold is one fold's read and answer: the levels it read, least
// specific first, the instant it judged freshness at, and the table —
// nil when no feed answers.
type priceFeedFold struct {
	now    time.Time
	scopes []priceFeedScopeRead
	table  *pricefeed.Table
}

// priceFeedScopeRead is one level as the fold read it: its config and
// fetched states (nil when the level holds no feed), or the read error
// that stepped it down.
type priceFeedScopeRead struct {
	scope priceFeedScope
	state *pricefeed.ScopeState
	err   error
}

// foldPriceFeed reads every level of one run's fold and folds them at
// one instant by cfg, its Now set here — the one body the bundle's table
// (EffectivePriceFeed) and the effective view (pricefeed_effective.go)
// both answer from, so the view cannot show a table the run would not
// seal. A level whose read fails steps down, and unread hears its read
// error: each caller says it where its reader belongs.
func foldPriceFeed(ctx context.Context, configs pricefeed.ConfigStore, states pricefeed.StateStore, orgID, teamID string, cfg pricefeed.FoldConfig, unread func(what string, err error)) priceFeedFold {
	fold := priceFeedFold{now: time.Now()}
	if configs == nil || states == nil {
		return fold
	}
	readCtx, cancel := context.WithTimeout(ctx, priceFeedReadTimeout)
	defer cancel()
	var answering []pricefeed.ScopeState
	for _, scope := range priceFeedFoldScopes(orgID, teamID) {
		state, err := priceFeedScopeState(readCtx, configs, states, scope.docID)
		fold.scopes = append(fold.scopes, priceFeedScopeRead{scope: scope, state: state, err: err})
		if err != nil {
			// A feed that cannot be read is a feed that does not answer:
			// the operator table prices the run and the caller's warning
			// names the scope — the ADR's step-down, not a launch refusal.
			if unread != nil {
				unread(scope.what, err)
			}
			continue
		}
		if state != nil {
			state.Label = scope.what
			answering = append(answering, *state)
		}
	}
	if len(answering) > 0 {
		cfg.Now = fold.now
		fold.table = pricefeed.Fold(cfg, answering...)
	}
	return fold
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
// of its feeds. A scope with no config document contributes nothing; a
// config whose state read fails fails the scope (the caller warns and
// steps the whole scope down), so partial outages degrade per scope
// rather than half-folding.
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
		state, err := states.Get(ctx, pricefeed.StateDocID(docID, feed.Name))
		if err != nil {
			return nil, err
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
