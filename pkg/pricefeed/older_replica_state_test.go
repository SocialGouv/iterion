package pricefeed

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// olderSpec and olderState freeze the shapes a release before accepted
// efforts decodes a feed state into: no reasoning efforts on a spec, no
// source-type stamp on a state. Frozen here, never derived from the current
// types, so they keep answering what an older replica reads.
type olderSpec struct {
	ContextWindow     int
	MaxOutputTokens   int
	InputCostPerM     float64
	OutputCostPerM    float64
	CacheReadCostPerM float64
	Reasoning         *bool
	ToolCall          *bool
	Temperature       *bool
}

type olderState struct {
	Feed        string               `bson:"feed"`
	Status      string               `bson:"status"`
	Entries     map[string]olderSpec `bson:"entries,omitempty"`
	RefreshedAt time.Time            `bson:"refreshed_at,omitempty"`
	FreshFor    Duration             `bson:"fresh_for,omitempty"`
	LastAttempt time.Time            `bson:"last_attempt,omitempty"`
	LastError   string               `bson:"last_error,omitempty"`
	UpdatedAt   time.Time            `bson:"updated_at"`
}

// An older replica holding the reconciler's lease refreshes an exec feed
// that prints reasoning_efforts: its strict parser refuses the field, so the
// refresh fails, and its failing path writes back the entries AS IT DECODED
// THEM, with their fetch time — the prices stay fresh, the accepted efforts
// and the type stamp are gone, and the fold seals no efforts from the feed
// until a release that reads them refreshes it. The reason an exec feed
// prints efforts only once no older replica can hold the lease (ADR-166,
// mixed versions).
func TestAnOlderReplicasFailingRefreshDropsAnExecFeedsEfforts(t *testing.T) {
	const id = "scaleway/deepseek-v4.1-flash"
	now := time.Now().UTC().Truncate(time.Millisecond)
	fresh := StateRecord{Feed: "pliage-exec", Type: TypeExec, Status: StateOK, Entries: map[string]modelspecs.Spec{
		id: {InputCostPerM: 0.672, OutputCostPerM: 2.016, ReasoningEfforts: []string{"low", "high", "xhigh", "max"}},
	}, RefreshedAt: now, FreshFor: D(2 * time.Hour), LastAttempt: now}
	doc, err := stateToDoc("platform/pliage-exec", fresh)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var older olderState
	if err := bson.Unmarshal(raw, &older); err != nil {
		t.Fatalf("an older replica cannot decode the state: %v", err)
	}
	// The older Refresh's failing path: entries, fetch time and window kept.
	older.Status, older.LastError, older.LastAttempt = StateFailing, `pliage-exec stdout does not parse: json: unknown field "reasoning_efforts"`, now.Add(time.Minute)
	rewritten, err := bson.Marshal(older)
	if err != nil {
		t.Fatal(err)
	}
	var back StateRecord
	if err := bson.Unmarshal(rewritten, &back); err != nil {
		t.Fatal(err)
	}
	if back.Type != "" || !back.Fresh(now.Add(time.Minute)) || back.Entries[id].InputCostPerM != 0.672 || len(back.Entries[id].ReasoningEfforts) != 0 {
		t.Fatalf("the rewritten state reads type=%q fresh=%v entry=%+v, want unstamped, fresh, priced, without efforts", back.Type, back.Fresh(now.Add(time.Minute)), back.Entries[id])
	}
	feed := FeedConfig{Name: "pliage-exec", Type: TypeExec, Enabled: true, Interval: D(time.Hour), Timeout: D(10 * time.Second), Params: map[string]string{"command": "/x"}}
	scope := func(state *StateRecord) ScopeState {
		return ScopeState{Config: &ConfigRecord{Feeds: []FeedConfig{feed}}, States: map[string]*StateRecord{"pliage-exec": state}}
	}
	if before := Fold(FoldConfig{Now: now.Add(time.Minute)}, scope(&fresh)); before.Efforts[id].Feed != "pliage-exec" {
		t.Fatalf("fixture: the fresh state folds no efforts: %+v", before)
	}
	after := Fold(FoldConfig{Now: now.Add(time.Minute)}, scope(&back))
	if _, here := after.Efforts[id]; here || after.Entries[id].Spec.InputCostPerM != 0.672 {
		t.Fatalf("after the older rewrite the fold answers efforts %+v and entry %+v, want the price and no efforts", after.Efforts, after.Entries[id])
	}
}
