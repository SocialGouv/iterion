package pricefeed

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// olderEntry freezes the feed-entry field set of a release before
// image_input (efforts aside): what an older server parses a declared or
// exec feed's entries with — strictly. Frozen here, never derived from the
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

// A feed's entries stating image_input are refused WHOLE by an older
// server's strict parser — it folds nothing of that declared feed and
// refuses its write, and an older lease holder's exec refresh fails — while
// this release reads them. The reason image_input is written into a
// declared or exec feed only once no older server replica can fold or
// refresh it (docs/agents/backends/image-acceptance.md, mixed versions).
func TestAnOlderServerRefusesFeedEntriesStatingImageInput(t *testing.T) {
	const stated = `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15,"image_input":false}}`
	const unstated = `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`
	olderParse := func(raw string) error {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var entries map[string]olderEntry
		return dec.Decode(&entries)
	}
	if err := olderParse(stated); err == nil || !strings.Contains(err.Error(), `unknown field "image_input"`) {
		t.Fatalf("the older parser read entries stating image_input: err = %v", err)
	}
	if err := olderParse(unstated); err != nil {
		t.Fatalf("fixture: the older parser refuses entries without image_input: %v", err)
	}
	table, err := ParseFeedEntries(stated, "feed stdout")
	if err != nil || table["scaleway/glm-5.3"].ImageInput == nil || *table["scaleway/glm-5.3"].ImageInput {
		t.Fatalf("this release reads %+v (err %v), want image_input false", table, err)
	}
}

// The operator table is not where image_input goes, as it is not where
// reasoning_efforts goes (ADR-166): a runner parses OPENAI_COMPATIBLE_MODELS,
// and a runner older than the field would refuse the whole table — every
// gateway launch on its pool refused. This release refuses the field there
// the same way it refuses reasoning_efforts, naming the table, while the
// same row without it parses.
func TestTheOperatorTableRefusesImageInput(t *testing.T) {
	const stated = `{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15,"image_input":false}}`
	_, err := ParseEntries(stated, "OPENAI_COMPATIBLE_MODELS")
	if err == nil || !strings.Contains(err.Error(), "OPENAI_COMPATIBLE_MODELS does not parse") || !strings.Contains(err.Error(), `unknown field "image_input"`) {
		t.Fatalf("the operator table accepted image_input: err = %v", err)
	}
	if _, err := ParseEntries(`{"scaleway/glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`, "OPENAI_COMPATIBLE_MODELS"); err != nil {
		t.Fatalf("fixture: the row without image_input is refused: %v", err)
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

// A runner reads the folded run table leniently (ParseTable is a plain
// json.Unmarshal, as it was before image_input): an older runner, whose spec
// has no image_input, still reads a table whose entry states it — the price
// answers, the flag is ignored.
func TestAnOlderRunnerReadsARunTableStatingImageInput(t *testing.T) {
	now := time.Now()
	text := false
	table := newTable(map[string]Entry{"scaleway/glm-5.3": {Feed: "pliage", RefreshedAt: now, FreshFor: D(time.Hour),
		Spec: modelspecs.Spec{InputCostPerM: 2.01, OutputCostPerM: 6.15, ImageInput: &text}}}, nil)
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"image_input":false`) {
		t.Fatalf("fixture: the table does not carry image_input: %s", raw)
	}
	type olderRunSpec struct {
		InputCostPerM  float64 `json:"input_cost_per_m,omitempty"`
		OutputCostPerM float64 `json:"output_cost_per_m,omitempty"`
	}
	var older struct {
		Entries map[string]struct {
			Spec olderRunSpec `json:"spec"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &older); err != nil || older.Entries["scaleway/glm-5.3"].Spec.InputCostPerM != 2.01 {
		t.Fatalf("an older runner's read: %+v (err %v), want the price", older, err)
	}
}
