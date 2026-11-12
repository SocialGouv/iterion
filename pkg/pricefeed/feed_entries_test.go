package pricefeed

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The env decoder stays the env decoder: OPENAI_COMPATIBLE_MODELS keeps
// refusing reasoning_efforts (accepted efforts have their own env name on
// the runner), while the feed parser reads them strictly — an unknown
// level, an empty list, a repeated level or a null refuse, naming the id.
func TestParseFeedEntriesReadsEffortsStrictly(t *testing.T) {
	withEfforts := `{"Scaleway/DeepSeek-X":{"input_usd_per_mtok":1,"output_usd_per_mtok":2,"reasoning_efforts":[" HIGH ","low",50]}}`
	if _, err := ParseEntries(withEfforts, "OPENAI_COMPATIBLE_MODELS"); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("the env decoder accepted reasoning_efforts: %v", err)
	}
	table, err := ParseFeedEntries(withEfforts, "feed stdout")
	if err != nil {
		t.Fatalf("the feed parser refused a well-formed effort list: %v", err)
	}
	got := table["scaleway/deepseek-x"]
	if !reflect.DeepEqual(got.ReasoningEfforts, []string{"high", "low", "50"}) {
		t.Fatalf("efforts decoded %#v, want [high low 50] in the declared order", got.ReasoningEfforts)
	}
	if got.InputCostPerM != 1 || got.OutputCostPerM != 2 {
		t.Fatalf("the prices beside the efforts decoded wrong: %+v", got)
	}
	effortsOnly, err := ParseFeedEntries(`{"glm-5.3":{"reasoning_efforts":["max"]}}`, "feed stdout")
	if err != nil || !reflect.DeepEqual(effortsOnly["glm-5.3"].ReasoningEfforts, []string{"max"}) {
		t.Fatalf("an efforts-only entry did not parse: %+v %v", effortsOnly, err)
	}
	if noEfforts, err := ParseFeedEntries(`{"glm-5.3":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`, "x"); err != nil || noEfforts["glm-5.3"].ReasoningEfforts != nil {
		t.Fatalf("an entry stating no efforts answered %+v %v, want nil efforts", noEfforts, err)
	}
	for _, bad := range []struct {
		name, raw, want string
	}{
		{"unknown level", `{"deepseek-x":{"reasoning_efforts":["hihg"]}}`, `"hihg"`},
		{"empty list", `{"deepseek-x":{"reasoning_efforts":[]}}`, "empty"},
		{"repeated level", `{"deepseek-x":{"reasoning_efforts":["high","HIGH"]}}`, `"high"`},
		{"repeated integer", `{"deepseek-x":{"reasoning_efforts":[7,"007"]}}`, `"7"`},
		{"null", `{"deepseek-x":{"reasoning_efforts":null}}`, "expected an array"},
	} {
		_, err := ParseFeedEntries(bad.raw, "feed stdout")
		if err == nil || !strings.Contains(err.Error(), bad.want) || !strings.Contains(err.Error(), `"deepseek-x"`) || !strings.Contains(err.Error(), "feed stdout") {
			t.Errorf("%s: refusal %v does not name the source, the id and %s", bad.name, err, bad.want)
		}
	}
}

// One body, two field sets: every rule of the env decoder holds on the
// feed parser too — unknown fields, unpaired and negative prices,
// colliding spellings and trailing content refuse in both.
func TestParseFeedEntriesKeepsEveryEnvRule(t *testing.T) {
	for _, bad := range []string{
		`{"m":{"input_usd_per_mtok":1}}`,
		`{"m":{"input_usd_per_mtok":-1,"output_usd_per_mtok":-1}}`,
		`{"m":{"inputt_usd_per_mtok":1}}`,
		`{"GLM":{"input_usd_per_mtok":1,"output_usd_per_mtok":2},"glm":{"input_usd_per_mtok":3,"output_usd_per_mtok":4}}`,
		`{"m":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}}`,
	} {
		_, envErr := ParseEntries(bad, "x")
		_, feedErr := ParseFeedEntries(bad, "x")
		if envErr == nil || feedErr == nil {
			t.Errorf("%s: env refusal %v, feed refusal %v — both must refuse", bad, envErr, feedErr)
		}
	}
}

// The exec source prints the feed format: a command stating accepted
// efforts beside its prices is read, not refused.
func TestExecSourceReadsEfforts(t *testing.T) {
	script := filepath.Join(t.TempDir(), "tariffs.sh")
	body := "#!/bin/sh\nprintf '%s' '{\"deepseek-x\":{\"input_usd_per_mtok\":0.6,\"output_usd_per_mtok\":1.8,\"reasoning_efforts\":[\"low\",\"high\"]}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := SourceFor(TypeExec)
	table, err := src.Fetch(context.Background(), map[string]string{"command": script})
	if err != nil {
		t.Fatalf("an exec feed stating efforts was refused: %v", err)
	}
	if got := table["deepseek-x"].ReasoningEfforts; !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Fatalf("exec efforts decoded %v, want [low high]", got)
	}
}

// An effort-list change is a change: the manual sync reports it per id,
// like a price move.
func TestRefreshReportsAnEffortsChange(t *testing.T) {
	prev := map[string]modelspecs.Spec{"deepseek-x": {InputCostPerM: 0.6, OutputCostPerM: 1.8, ReasoningEfforts: []string{"low"}}}
	next := map[string]modelspecs.Spec{"deepseek-x": {InputCostPerM: 0.6, OutputCostPerM: 1.8, ReasoningEfforts: []string{"low", "high"}}}
	if changes := DiffEntries(prev, next); len(changes) != 1 || changes[0].ID != "deepseek-x" {
		t.Fatalf("an effort-list change did not report: %+v", changes)
	}
	same := map[string]modelspecs.Spec{"deepseek-x": {InputCostPerM: 0.6, OutputCostPerM: 1.8, ReasoningEfforts: []string{"low"}}}
	if changes := DiffEntries(prev, same); len(changes) != 0 {
		t.Fatalf("an unchanged effort list reported as changed: %+v", changes)
	}

	// The same through the exec source's real refresh.
	script := filepath.Join(t.TempDir(), "tariffs.sh")
	body := "#!/bin/sh\nprintf '%s' '{\"deepseek-x\":{\"input_usd_per_mtok\":0.6,\"output_usd_per_mtok\":1.8,\"reasoning_efforts\":[\"low\",\"high\"]}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	feed := FeedConfig{Name: "f", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(10), Params: map[string]string{"command": script}}
	_, outcome := Refresh(context.Background(), feed, &StateRecord{Feed: "f", Entries: prev, RefreshedAt: time.Now(), FreshFor: d(7200)}, nil, time.Now())
	if outcome.Status != StateOK || len(outcome.Changes) != 1 || outcome.Changes[0].ID != "deepseek-x" {
		t.Fatalf("the exec refresh did not report the effort-list change: %+v", outcome)
	}
}

// The memory twin hands out and stores deep copies of the effort lists:
// a caller mutating a returned (or a written) list never reaches the
// stored state.
func TestMemoryStateStoreDoesNotAliasEfforts(t *testing.T) {
	ctx := context.Background()
	states := NewMemoryStateStore()
	written := &StateRecord{Entries: map[string]modelspecs.Spec{"m": {ReasoningEfforts: []string{"low", "high"}}}}
	if err := states.Put(ctx, "doc", written); err != nil {
		t.Fatal(err)
	}
	written.Entries["m"].ReasoningEfforts[0] = "WRITER-MUTATED"
	got, _ := states.Get(ctx, "doc")
	got.Entries["m"].ReasoningEfforts[1] = "READER-MUTATED"
	again, _ := states.Get(ctx, "doc")
	if want := []string{"low", "high"}; !reflect.DeepEqual(again.Entries["m"].ReasoningEfforts, want) {
		t.Fatalf("a mutated effort list reached the store: %v, want %v", again.Entries["m"].ReasoningEfforts, want)
	}
	listed, _ := states.List(ctx)
	listed["doc"].Entries["m"].ReasoningEfforts[0] = "LIST-MUTATED"
	if again, _ := states.Get(ctx, "doc"); again.Entries["m"].ReasoningEfforts[0] != "low" {
		t.Fatalf("a listed record's effort list aliases the store: %v", again.Entries["m"].ReasoningEfforts)
	}
}

// The Mongo twin's encoding round-trips the effort lists under the
// driver's default key — the document the state store writes is the
// document it reads back.
func TestMongoStateDocRoundTripsEfforts(t *testing.T) {
	rec := StateRecord{Feed: "f", Status: StateOK, Entries: map[string]modelspecs.Spec{
		"deepseek-x": {InputCostPerM: 0.6, OutputCostPerM: 1.8, ReasoningEfforts: []string{"low", "high"}},
		"glm-5.3":    {InputCostPerM: 2, OutputCostPerM: 6},
	}}
	doc, err := stateToDoc("org:o/f", rec)
	if err != nil {
		t.Fatal(err)
	}
	body, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bson.Raw(body).LookupErr("entries", "deepseek-x", "reasoningefforts"); err != nil {
		t.Fatalf("the state document carries no entries.deepseek-x.reasoningefforts key: %v (%s)", err, bson.Raw(body))
	}
	var back stateDoc
	if err := bson.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != "org:o/f" {
		t.Fatalf("the document id did not round-trip: %q", back.ID)
	}
	if got := back.Entries["deepseek-x"].ReasoningEfforts; !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Fatalf("efforts round-tripped as %v", got)
	}
	if got := back.Entries["glm-5.3"].ReasoningEfforts; len(got) != 0 {
		t.Fatalf("an entry stating no efforts round-tripped as %v", got)
	}
}

// Drift is a PRICE disagreement: an entry with no price pair (an
// efforts-only declaration) states nothing about price, so it never
// drifts against the operator table.
func TestDriftReportSkipsPricelessEntries(t *testing.T) {
	feed := map[string]modelspecs.Spec{
		"efforts-only": {ReasoningEfforts: []string{"high"}},
		"windowed":     {ContextWindow: 131072},
		"priced":       {InputCostPerM: 3, OutputCostPerM: 4},
	}
	operator := map[string]modelspecs.Spec{
		"efforts-only": {InputCostPerM: 1, OutputCostPerM: 2},
		"windowed":     {InputCostPerM: 1, OutputCostPerM: 2},
		"priced":       {InputCostPerM: 3.5, OutputCostPerM: 4},
	}
	drift := DriftReport("pliage", feed, operator)
	if len(drift) != 1 || drift[0].ID != "priced" {
		t.Fatalf("only the priced disagreement is drift, got %+v", drift)
	}
}
