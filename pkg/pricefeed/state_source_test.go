package pricefeed

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// A state document is keyed by (scope, name): a feed retyped under its
// name finds its predecessor's document. The stamp decides whose it is —
// a record written for another source type is not the feed's answer,
// while an unstamped record (written before the stamp) still is.
func TestFoldReadsOnlyAStateOfTheFeedsOwnType(t *testing.T) {
	now := time.Now()
	cfg := &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{fetchedFeed("pliage")}} // type exec
	for _, tc := range []struct {
		stamp   string
		answers bool
	}{
		{TypeLiteLLM, false},
		{TypeDeclared, false},
		{"", true},
		{TypeExec, true},
	} {
		state := freshState("pliage", now, map[string]modelspecs.Spec{"glm-5.3": {InputCostPerM: 2.01, OutputCostPerM: 6.15, ReasoningEfforts: []string{"max"}}})
		state.Type = tc.stamp
		table := Fold(FoldConfig{Now: now}, ScopeState{Config: cfg, States: map[string]*StateRecord{"pliage": state}})
		if got := table != nil; got != tc.answers {
			t.Fatalf("a fresh state stamped %q under an exec feed answered=%v, want %v (%+v)", tc.stamp, got, tc.answers, table)
		}
	}
}

// Refresh stamps the record with the feed's source type, and reads a prev
// written for another type as no prev: a failing refresh of a retyped feed
// retains nothing of its predecessor, a successful one reports every
// entry as new. An unstamped prev is retained, as before the stamp.
func TestRefreshKeepsOnlyAStateOfTheFeedsOwnType(t *testing.T) {
	now := time.Now()
	dead := FeedConfig{Name: "pliage", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(5),
		Params: map[string]string{"command": "/nonexistent/iterion-feed"}}
	prev := func(stamp string) *StateRecord {
		state := freshState("pliage", now.Add(-time.Minute), map[string]modelspecs.Spec{"glm-5.3": {InputCostPerM: 2.01, OutputCostPerM: 6.15}})
		state.Type = stamp
		state.FreshFor = d(60 * 86400)
		return state
	}
	state, outcome := Refresh(context.Background(), dead, prev(TypeDeclared), nil, now)
	if outcome.Status != StateFailing {
		t.Fatalf("the dead exec feed refreshed %+v", outcome)
	}
	if state.Type != TypeExec {
		t.Fatalf("the record is stamped %q, want the feed's type %q", state.Type, TypeExec)
	}
	if len(state.Entries) != 0 || state.FreshFor != 0 || !state.RefreshedAt.IsZero() {
		t.Fatalf("a failing refresh retained a declaration's record: %d entries, window %v, refreshed %v", len(state.Entries), time.Duration(state.FreshFor), state.RefreshedAt)
	}
	if state.Fresh(now) {
		t.Fatal("the retyped feed's record reads fresh")
	}
	state, _ = Refresh(context.Background(), dead, prev(""), nil, now)
	if len(state.Entries) != 1 || state.FreshFor != d(60*86400) {
		t.Fatalf("an unstamped prev was not retained: %+v", state)
	}
}

// Due reads only a state of the feed's own type: a retyped feed whose
// predecessor just attempted is due at once; the same attempt by the feed
// itself is not.
func TestDueReadsOnlyAStateOfTheFeedsOwnType(t *testing.T) {
	now := time.Now()
	feed := fetchedFeed("pliage")
	for _, tc := range []struct {
		stamp string
		due   bool
	}{
		{TypeLiteLLM, true},
		{TypeExec, false},
		{"", false},
	} {
		state := &StateRecord{Feed: "pliage", Type: tc.stamp, LastAttempt: now.Add(-time.Minute)}
		if got := Due(feed, state, now); got != tc.due {
			t.Fatalf("a state stamped %q attempted a minute ago: due=%v, want %v", tc.stamp, got, tc.due)
		}
	}
}
