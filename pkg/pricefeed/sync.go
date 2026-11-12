package pricefeed

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// EntryChange is one id's move in a refresh: old → new, either side nil
// for added/removed. The manual sync reports what changed, per id
// (ADR-163 §5).
type EntryChange struct {
	ID  string           `json:"id"`
	Old *modelspecs.Spec `json:"old,omitempty"`
	New *modelspecs.Spec `json:"new,omitempty"`
}

// SyncOutcome is one feed's refresh result — what the manual trigger
// answers and what the reconciler records.
type SyncOutcome struct {
	Feed        string        `json:"feed"`
	Status      string        `json:"status"` // ok | failing | declared (staleness derives on read)
	RefreshedAt time.Time     `json:"refreshed_at,omitempty"`
	FreshFor    Duration      `json:"fresh_for,omitempty"`
	Error       string        `json:"error,omitempty"`
	Changes     []EntryChange `json:"changes,omitempty"`
}

// Drift is one id where a fresh feed and the operator table disagree —
// surfaced, never resolved: the feed answers while fresh, and the
// operator's explicit table is never silently overridden by anything,
// live or dead (ADR-163 §3).
type Drift struct {
	ID      string
	Feed    string
	FeedIn  float64
	FeedOut float64
	OpIn    float64
	OpOut   float64
}

// Refresh runs one feed's sync: fetch through its own adapter under its
// own timeout, normalize, diff against the previous state, and derive
// the durable state. It is idempotent — the elected reconciler and the
// manual trigger call the same function; a concurrent double refresh
// converges on the same source answer. The record it answers is stamped
// with the feed's source type, and a prev stamped with another type is
// read as no prev (StateRecord.Of). A fetched table holding a price that
// is not a finite number fails the refresh, naming the id
// (finiteEntries).
//
// The operator table (the SERVER's own reading of
// OPENAI_COMPATIBLE_MODELS, passed by the caller — this package never
// reads an environment) feeds the drift report: every id both tables
// carry with differing prices names the id, both values and the source.
// One line per drifting id per refresh — the "not per dispatch" bound
// the ADR sets.
func Refresh(ctx context.Context, feed FeedConfig, prev *StateRecord, operatorTable map[string]modelspecs.Spec, now time.Time) (StateRecord, SyncOutcome) {
	if now.IsZero() {
		now = time.Now()
	}
	// A record written for another source type (the feed retyped under
	// its name) is not this feed's: nothing of it is retained or diffed.
	prev = prev.Of(feed.Type)
	state := StateRecord{
		Feed:        feed.Name,
		Type:        feed.Type,
		FreshFor:    feed.FreshFor(),
		LastAttempt: now,
	}
	outcome := SyncOutcome{Feed: feed.Name, FreshFor: feed.FreshFor()}
	// A failed refresh keeps the previous entries: staleness — not
	// amnesia — is the failure mode. And they must be judged by the
	// window they were FETCHED under, never the CURRENT config's —
	// widening the interval while the source is dead would resurrect
	// already-stale entries as fresh through a refresh that FAILED
	// (round 1). The success path overwrites both fields below.
	if prev != nil {
		state.Entries = prev.Entries
		state.RefreshedAt = prev.RefreshedAt
		state.FreshFor = prev.FreshFor
	} else {
		state.FreshFor = 0
	}
	src, ok := SourceFor(feed.Type)
	if !ok {
		state.Status = StateFailing
		state.LastError = fmt.Sprintf("unknown source type %q", feed.Type)
		outcome.Status = StateFailing
		outcome.Error = state.LastError
		return state, outcome
	}
	fetchCtx, cancel := context.WithTimeout(ctx, time.Duration(feed.Timeout))
	defer cancel()
	fetched, err := src.Fetch(fetchCtx, feed.Params)
	if err == nil {
		err = finiteEntries(fetched)
	}
	if err != nil {
		state.Status = DeriveStatus(state.Entries, state.RefreshedAt, state.FreshFor, err.Error(), now)
		state.LastError = err.Error()
		outcome.Status = StateFailing
		outcome.Error = err.Error()
		return state, outcome
	}
	state.LastError = ""
	state.Entries = fetched
	state.RefreshedAt = now
	// Fetched under the CURRENT cadence: the window is the config's, the
	// one the pre-fetch block may have overwritten with the previous
	// fetch's (the failure-path preservation).
	state.FreshFor = feed.FreshFor()
	state.Status = StateOK
	outcome.Status = StateOK
	outcome.RefreshedAt = now
	outcome.Changes = DiffEntries(prevEntries(prev), fetched)
	return state, outcome
}

// finiteEntries refuses a fetched table holding a price no wire can
// carry — NaN or ±Inf, as a LiteLLM per-token cost scaled to per-mtok past
// float64's range answers: stored, it would leave every fold naming the id
// with a table that does not encode, its accepted efforts with it. It names
// the first such id, sorted, so the refusal reads the same on every
// refresh.
func finiteEntries(entries map[string]modelspecs.Spec) error {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if field, value, bad := nonFinitePrice(entries[id]); bad {
			return fmt.Errorf("pricefeed: entry %q: %s is %v, not a finite price — the source answered a number no table can carry", id, field, value)
		}
	}
	return nil
}

func prevEntries(prev *StateRecord) map[string]modelspecs.Spec {
	if prev == nil {
		return nil
	}
	return prev.Entries
}

// DiffEntries lists the ids whose spec moved, added and removed — sorted
// by id, deterministic whatever map order ranged.
func DiffEntries(old, new map[string]modelspecs.Spec) []EntryChange {
	if len(old) == 0 && len(new) == 0 {
		return nil
	}
	ids := map[string]bool{}
	for id := range old {
		ids[id] = true
	}
	for id := range new {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	var changes []EntryChange
	for _, id := range sorted {
		before, had := old[id]
		after, have := new[id]
		switch {
		case had && !have:
			b := before
			changes = append(changes, EntryChange{ID: id, Old: &b})
		case !had && have:
			a := after
			changes = append(changes, EntryChange{ID: id, New: &a})
		case had && have && !SpecEqual(before, after):
			b, a := before, after
			changes = append(changes, EntryChange{ID: id, Old: &b, New: &a})
		}
	}
	return changes
}

// SpecEqual compares two specs BY VALUE, field by field: the capability
// flags dereferenced (a plain != on the *bool fields compares POINTER
// addresses, and every refresh allocates new ones for the same verdicts)
// and the accepted efforts element by element, in order.
func SpecEqual(a, b modelspecs.Spec) bool {
	if a.ContextWindow != b.ContextWindow || a.MaxOutputTokens != b.MaxOutputTokens ||
		a.InputCostPerM != b.InputCostPerM || a.OutputCostPerM != b.OutputCostPerM || a.CacheReadCostPerM != b.CacheReadCostPerM {
		return false
	}
	return boolPtrEqual(a.Reasoning, b.Reasoning) && boolPtrEqual(a.ToolCall, b.ToolCall) && boolPtrEqual(a.Temperature, b.Temperature) &&
		slices.Equal(a.ReasoningEfforts, b.ReasoningEfforts)
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// DriftReport names every id where the feed's fresh entries and the
// operator table disagree on price. Window and context windows can
// legitimately differ (the feed's business may be the price alone); the
// PRICE disagreement is the one that changes what a run is billed, so
// that is the one surfaced. An entry without the price pair (a window or
// an effort list alone) states no price and never answers one (Fold), so
// it never drifts.
func DriftReport(feed string, entries, operatorTable map[string]modelspecs.Spec) []Drift {
	if len(entries) == 0 || len(operatorTable) == 0 {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for id, e := range entries {
		if e.InputCostPerM <= 0 || e.OutputCostPerM <= 0 {
			continue
		}
		if _, op := operatorTable[NormalizeID(id)]; op {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var out []Drift
	for _, id := range ids {
		e := entries[id]
		op := operatorTable[NormalizeID(id)]
		if e.InputCostPerM == op.InputCostPerM && e.OutputCostPerM == op.OutputCostPerM {
			continue
		}
		out = append(out, Drift{
			ID: id, Feed: feed,
			FeedIn: e.InputCostPerM, FeedOut: e.OutputCostPerM,
			OpIn: op.InputCostPerM, OpOut: op.OutputCostPerM,
		})
	}
	return out
}

// FormatDrift renders one drift line for the reconciler's and the manual
// sync's warning channel.
func FormatDrift(d Drift) string {
	return fmt.Sprintf("price feed %s: id %q disagrees with the operator table — feed $%.4f/$%.4f per mtok (in/out) vs operator $%.4f/$%.4f; the feed answers while fresh",
		d.Feed, d.ID, d.FeedIn, d.FeedOut, d.OpIn, d.OpOut)
}

// Due reports whether a feed's next refresh attempt is due at now: a
// feed refreshes every interval; one whose last attempt FAILED retries
// on the short backoff, not the full period — a dead source behind a
// 30-day interval would otherwise stay dead for a month. A static feed
// (a declaration) is never due: its answer is its config, which the fold
// reads directly. A state written for another source type counts as no
// attempt (StateRecord.Of).
func Due(feed FeedConfig, state *StateRecord, now time.Time) bool {
	if IsStatic(feed.Type) {
		return false
	}
	// A record written for another source type says nothing about this
	// feed's attempts: a retyped feed fetches on the next pass.
	state = state.Of(feed.Type)
	if state == nil || state.LastAttempt.IsZero() {
		return true
	}
	// A LastAttempt in the future is a clock-skewed replica's manual sync,
	// not patience: treat it as due so the elected replica re-fetches
	// until real time catches up, instead of stalling the feed for the
	// skew's whole width.
	if state.LastAttempt.After(now) {
		return true
	}
	delay := time.Duration(feed.Interval)
	if strings.TrimSpace(state.LastError) != "" {
		if backoff := 2 * time.Duration(feed.Timeout); backoff < delay {
			delay = backoff
		}
		if delay < MinInterval {
			delay = MinInterval
		}
	}
	return now.Sub(state.LastAttempt) >= delay
}
