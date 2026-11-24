package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/recordcodec"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func decodeExactRecordM(t *testing.T, raw []byte) bson.M {
	t.Helper()
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	dec.DefaultDocumentM()
	var d bson.M
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}
func TestExactRecordExistingUnknownPreserver(t *testing.T) {
	r := exactRecordFixtureRun()
	p, err := exactRecordRunPlan.Prepare(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.BSON()
	if err != nil {
		t.Fatal(err)
	}
	old := decodeExactRecordM(t, raw)
	future := bson.M{"integer": int64(9007199254740993), "date": bson.NewDateTimeFromTime(exactRecordInstant), "binary": bson.Binary{Subtype: 0x80, Data: []byte{0, 255}}, "frame-like": bson.A{"iterion.exact-values/999"}}
	old["future_meta"] = future
	cp := old["checkpoint"].(bson.M)
	cp["future_cp"] = future
	branches := cp["parallel"].(bson.M)["branches"].(bson.M)
	branches["b.$literal"].(bson.M)["future_branch"] = future
	branches["removed"] = bson.M{"future_removed": future}
	r.Inputs = map[string]any{"replacement": json.Number("1e400")}
	r.Checkpoint.Parallel.Branches["b.$literal"].Outputs = map[string]map[string]any{"new": exactRecordPayload()}
	p, err = exactRecordRunPlan.Prepare(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = p.BSON()
	if err != nil {
		t.Fatal(err)
	}
	next := decodeExactRecordM(t, raw)
	merged, err := preserveUnknownBSON(old, next, reflect.TypeFor[store.Run]())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := bson.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeExactRecordM(t, wire)
	if !reflect.DeepEqual(got["future_meta"], future) || !reflect.DeepEqual(got["checkpoint"].(bson.M)["future_cp"], future) {
		t.Fatal("unknown metadata lost")
	}
	b := got["checkpoint"].(bson.M)["parallel"].(bson.M)["branches"].(bson.M)
	if !reflect.DeepEqual(b["b.$literal"].(bson.M)["future_branch"], future) {
		t.Fatal("branch metadata lost")
	}
	if _, exists := b["removed"]; exists {
		t.Fatal("deleted branch resurrected")
	}
	var decoded store.Run
	if err := exactRecordRunPlan.DecodeBSON(t.Context(), wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Inputs) != 1 || decoded.Inputs["replacement"] != json.Number("1e400") {
		t.Fatal("old exactRecordPayload merged", decoded.Inputs)
	}
	t.Log("real preserveUnknownBSON retained root/checkpoint/branch metadata (date/binary/int64), kept payload replacement authoritative, deleted removed branch")
}
func TestExactRecordInteractionInlineWrapper(t *testing.T) {
	src := &store.Interaction{ID: "i", RunID: "r", Questions: exactRecordPayload(), Answers: nil, Turns: []store.InteractionTurn{{Role: "companion", Verdict: exactRecordPayload()}}}
	p, err := exactRecordInteractionPlan.Prepare(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.BSON()
	if err != nil {
		t.Fatal(err)
	}
	var d bson.D
	bson.Unmarshal(raw, &d)
	id := interactionID{RunID: "r", InteractionID: "i"}
	d = append(bson.D{{Key: "_id", Value: id}}, d...)
	wire, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got store.Interaction
	if err := exactRecordInteractionPlan.DecodeBSON(t.Context(), wire, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src, &got) {
		t.Fatal("inline interaction mismatch")
	}
	var metadoc struct {
		ID interactionID `bson:"_id"`
	}
	if err = bson.Unmarshal(wire, &metadoc); err != nil || metadoc.ID != id {
		t.Fatal("composite id lost", err)
	}
	t.Log("real interactionID inline wrapper preserved; payloads reconstructed without calling its legacy map decoder")
}
func TestExactRecordStructuralListPreserverBoundary(t *testing.T) {
	src := &store.Interaction{ID: "i", RunID: "r", Questions: nil, Answers: nil, Turns: []store.InteractionTurn{{Role: "companion", Verdict: exactRecordPayload()}}}
	p, err := exactRecordInteractionPlan.Prepare(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.BSON()
	if err != nil {
		t.Fatal(err)
	}
	old := decodeExactRecordM(t, raw)
	next := decodeExactRecordM(t, raw)
	_, err = preserveUnknownBSON(old, next, reflect.TypeFor[store.Interaction]())
	if err == nil {
		t.Fatal("expected current generic structural list comparator to reject exact Verdict frame")
	}
	t.Logf("existing comparator cannot be reused for arbitrary record lists: %v (not currently Interaction writer path)", err)
}
func TestExactRecordPreserveMetadataReorderAndNilBranch(t *testing.T) {
	r := exactRecordFixtureRun()
	r.Checkpoint.SelectedIncoming = map[string][]store.IncomingEdge{"join": {{From: "a", To: "join"}, {From: "b", To: "join"}}}
	p, e := exactRecordRunPlan.Prepare(t.Context(), r)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := p.BSON()
	if e != nil {
		t.Fatal(e)
	}
	old := decodeExactRecordM(t, raw)
	cp := old["checkpoint"].(bson.M)
	edges := cp["selected_incoming"].(bson.M)["join"].(bson.A)
	edges[0].(bson.M)["future"] = int64(9007199254740993)
	edges[1].(bson.M)["future"] = "B"
	b := cp["parallel"].(bson.M)["branches"].(bson.M)
	b["b.$literal"].(bson.M)["future"] = "must disappear with nil parent"
	r.Checkpoint.SelectedIncoming["join"] = []store.IncomingEdge{{From: "b", To: "join"}, {From: "a", To: "join"}}
	r.Checkpoint.Parallel.Branches["b.$literal"] = nil
	p, e = exactRecordRunPlan.Prepare(t.Context(), r)
	if e != nil {
		t.Fatal(e)
	}
	raw, e = p.BSON()
	if e != nil {
		t.Fatal(e)
	}
	next := decodeExactRecordM(t, raw)
	merged, e := preserveUnknownBSON(old, next, reflect.TypeFor[store.Run]())
	if e != nil {
		t.Fatal(e)
	}
	wire, e := bson.Marshal(merged)
	if e != nil {
		t.Fatal(e)
	}
	got := decodeExactRecordM(t, wire)
	c := got["checkpoint"].(bson.M)
	a := c["selected_incoming"].(bson.M)["join"].(bson.A)
	if a[0].(bson.M)["future"] != "B" || a[1].(bson.M)["future"] != int64(9007199254740993) {
		t.Fatalf("metadata identity lost on reorder %#v", a)
	}
	if c["parallel"].(bson.M)["branches"].(bson.M)["b.$literal"] != nil {
		t.Fatal("nil branch resurrected")
	}
	var result store.Run
	if e = exactRecordRunPlan.DecodeBSON(t.Context(), wire, &result); e != nil {
		t.Fatal(e)
	}
	t.Log("real preserveUnknownBSON: reorder keeps metadata by identity; nil branch clears extensions; exact payload rehydrates")
}

type exactRecordTestPlan struct{ recordcodec.Codec }
type exactRecordTestPrepared struct{ b []byte }

func (p exactRecordTestPrepared) BSON() ([]byte, error) { return p.b, nil }
func (p exactRecordTestPlan) Prepare(ctx context.Context, src any) (exactRecordTestPrepared, error) {
	b, e := p.EncodeBSON(ctx, src)
	return exactRecordTestPrepared{b}, e
}
func exactRecordTestCodec(k store.ExactRecordKind) exactRecordTestPlan {
	p, e := store.ExactRecordCodec(k)
	if e != nil {
		panic(e)
	}
	return exactRecordTestPlan{p}
}

var exactRecordRunPlan = exactRecordTestCodec(store.ExactRun)
var exactRecordInteractionPlan = exactRecordTestCodec(store.ExactInteraction)

func exactRecordPayload() map[string]any {
	return map[string]any{"large": json.Number("9007199254740993.0"), "decimal": json.Number("0.123456789012345678901234567890"), "huge": json.Number("1e400"), "uint": uint64(18446744073709551615), "raw-looking": []any{"iterion.exact-values/1", []any{[]any{"number", "7"}}}}
}
func exactRecordOutputs() map[string]map[string]any {
	return map[string]map[string]any{"a.$literal": exactRecordPayload(), "null": nil, "empty": {}}
}

var exactRecordInstant = time.Date(2026, 11, 3, 17, 29, 1, 123000000, time.UTC)

func exactRecordFixtureRun() *store.Run {
	return &store.Run{ID: "run", FormatVersion: 1, CreatedAt: exactRecordInstant, Inputs: exactRecordPayload(), PublicContract: json.RawMessage(`{"z":9007199254740993.0,"a":2}`), Checkpoint: &store.Checkpoint{NodeID: "gate", Outputs: exactRecordOutputs(), LoopPreviousOutput: exactRecordOutputs(), LoopCurrentOutput: exactRecordOutputs(), Artifacts: exactRecordOutputs(), Vars: exactRecordPayload(), InteractionQuestions: exactRecordPayload(), FiredEvents: exactRecordOutputs(), BackendConversation: json.RawMessage(`[{"x":9007199254740993.0}]`), Parallel: &store.ParallelCheckpoint{RouterNodeID: "router", PendingInteractionQuestions: exactRecordPayload(), Branches: map[string]*store.BranchCheckpoint{"b.$literal": {BranchID: "b", Outputs: exactRecordOutputs(), Artifacts: exactRecordOutputs(), LoopPreviousOutput: exactRecordOutputs(), LoopCurrentOutput: exactRecordOutputs(), ResumeAnswers: exactRecordPayload()}, "nil": nil}}}}
}
