package pricefeed

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// olderEntry freezes the operator-table and feed-entry field set of a
// release before image_input: what an older runner parses
// OPENAI_COMPATIBLE_MODELS with, and an older server a declared or exec
// feed's entries with — strictly. Frozen here, never derived from the
// current types.
type olderEntry struct {
	ContextWindow       int     `json:"context_window"`
	MaxOutputTokens     int     `json:"max_output_tokens"`
	InputUSDPerMTok     float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok    float64 `json:"output_usd_per_mtok"`
	CacheReadUSDPerMTok float64 `json:"cache_read_usd_per_mtok"`
	Reasoning           *bool   `json:"reasoning"`
	ToolCall            *bool   `json:"tool_call"`
	Temperature         *bool   `json:"temperature"`
}

// A table stating image_input is refused WHOLE by an older binary's strict
// parser — an older runner then resolves every gateway id of
// OPENAI_COMPATIBLE_MODELS to unknown, an older server folds nothing of that
// declared feed and refuses its write, an older lease holder's exec refresh
// fails — while this release reads it. The reason image_input is written
// into the operator table only once every runner reading it runs this
// release, and into a declared or exec feed only once no older server
// replica can fold or refresh it (docs/agents/backends/image-acceptance.md, mixed
// versions).
func TestAnOlderBinaryRefusesATableStatingImageInput(t *testing.T) {
	const stated = `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15,"image_input":false}}`
	const unstated = `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`
	olderParse := func(raw string) error {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var entries map[string]olderEntry
		return dec.Decode(&entries)
	}
	if err := olderParse(stated); err == nil || !strings.Contains(err.Error(), `unknown field "image_input"`) {
		t.Fatalf("the older parser read a table stating image_input: err = %v", err)
	}
	if err := olderParse(unstated); err != nil {
		t.Fatalf("fixture: the older parser refuses the table without image_input: %v", err)
	}
	for name, parse := range map[string]func(string, string) (map[string]modelspecs.Spec, error){"ParseEntries": ParseEntries, "ParseFeedEntries": ParseFeedEntries} {
		table, err := parse(stated, "test")
		if err != nil || table["scaleway/glm-5.3"].ImageInput == nil || *table["scaleway/glm-5.3"].ImageInput {
			t.Fatalf("%s: this release reads %+v (err %v), want image_input false", name, table, err)
		}
	}
}

// An older replica's failing exec refresh writes the entries back as it
// decoded them: the price stays, image_input is gone, so runs send images
// to that id until a replica of this release refreshes the feed.
func TestAnOlderReplicasFailingRefreshDropsAnExecFeedsImageInput(t *testing.T) {
	const id = "scaleway/glm-5.3"
	now := time.Now().UTC().Truncate(time.Millisecond)
	text := false
	fresh := StateRecord{Feed: "pliage-exec", Type: TypeExec, Status: StateOK, Entries: map[string]modelspecs.Spec{
		id: {InputCostPerM: 2.01, OutputCostPerM: 6.15, ImageInput: &text},
	}, RefreshedAt: now, FreshFor: D(2 * time.Hour), LastAttempt: now}
	doc, err := stateToDoc("platform/pliage-exec", fresh)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture states the flag: the new binary round-trips it.
	var same StateRecord
	if err := bson.Unmarshal(raw, &same); err != nil || same.Entries[id].ImageInput == nil {
		t.Fatalf("fixture: this release does not round-trip image_input (%v): %+v", err, same.Entries[id])
	}
	var older olderState
	if err := bson.Unmarshal(raw, &older); err != nil {
		t.Fatalf("an older replica cannot decode the state: %v", err)
	}
	older.Status, older.LastError = StateFailing, `pliage-exec stdout does not parse: json: unknown field "image_input"`
	rewritten, err := bson.Marshal(older)
	if err != nil {
		t.Fatal(err)
	}
	var back StateRecord
	if err := bson.Unmarshal(rewritten, &back); err != nil {
		t.Fatal(err)
	}
	if back.Entries[id].InputCostPerM != 2.01 || back.Entries[id].ImageInput != nil {
		t.Fatalf("the rewritten entry reads %+v, want the price kept and image_input gone", back.Entries[id])
	}
}
