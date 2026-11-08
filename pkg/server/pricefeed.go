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
// (pricefeed_routes.go) run. Precedence everywhere: a FRESH feed entry
// answers over the operator table for the ids it carries, the more
// specific scope (team) over the less (org); a stale or failing feed
// steps down with a warning, never a refusal.

// priceFeedReadTimeout bounds one scope's reads in the launch-path fold:
// a slow database must not hold a launch, and a feed that cannot be read
// steps down to the operator floor with a warning — never a refusal.
const priceFeedReadTimeout = 2 * time.Second

// EffectivePriceFeed folds one run's effective table — the org and team
// feeds' fresh entries, team answering over org per id — and answers the
// wire bytes the bundle seals. nil when no fresh feed answers: the
// operator table is the floor and the bundle serializes unchanged. The
// named ids are the run's own gateway models, the narrowing fallback
// when the folded table exceeds the channel's byte budget.
func EffectivePriceFeed(ctx context.Context, configs pricefeed.ConfigStore, states pricefeed.StateStore, orgID, teamID string, named []string, warnf func(format string, args ...any)) json.RawMessage {
	if configs == nil || states == nil {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, priceFeedReadTimeout)
	defer cancel()
	var scopes []pricefeed.ScopeState
	for _, scope := range []struct {
		docID string
		what  string
	}{
		{pricefeed.OrgConfigDocID(orgID), "org " + orgID},
		{pricefeed.TeamConfigDocID(teamID), "team " + teamID},
	} {
		if scope.docID == "org:" || scope.docID == "team:" {
			continue
		}
		contrib, err := priceFeedScopeState(readCtx, configs, states, scope.docID)
		if err != nil {
			// A feed that cannot be read is a feed that does not answer:
			// the operator table prices the run and the warning names the
			// scope — the ADR's step-down, not a launch refusal.
			if warnf != nil {
				warnf("price feed: %s unread — the operator table answers: %v", scope.what, err)
			}
			continue
		}
		if contrib != nil {
			scopes = append(scopes, *contrib)
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	table := pricefeed.Fold(pricefeed.FoldConfig{Named: named, Warnf: warnf}, scopes...)
	if table == nil {
		return nil
	}
	raw, err := json.Marshal(table)
	if err != nil {
		if warnf != nil {
			warnf("price feed: the folded table does not encode: %v", err)
		}
		return nil
	}
	return raw
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
func (s *Server) priceFeedRefresh(ctx context.Context, scopeDocID string, feed pricefeed.FeedConfig) (pricefeed.SyncOutcome, error) {
	if feed.Type == pricefeed.TypeExec && !priceFeedExecAllowed() {
		err := fmt.Errorf("exec price feeds are the platform operator's: export %s=1 in the deployment's environment to arm them (super-admin configures them)", priceFeedExecEnv)
		stateID := pricefeed.StateDocID(scopeDocID, feed.Name)
		// Staleness, not amnesia — the same rule Refresh's own failure
		// path holds: a deployment restarted without the hatch keeps the
		// last good entries, aging out visibly, instead of wiping them.
		state := pricefeed.StateRecord{Feed: feed.Name, Status: pricefeed.StateFailing, LastAttempt: time.Now(), LastError: err.Error()}
		if prev, getErr := s.priceFeedStates.Get(ctx, stateID); getErr == nil && prev != nil {
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
