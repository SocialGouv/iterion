package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/recordcodec"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type slot struct{ Fields []string }
type Plan struct {
	recordcodec.Codec
	Type  reflect.Type
	Slots []slot
}
type preparedRecord struct{ j, b []byte }

func (p preparedRecord) JSON() ([]byte, error) { return p.j, nil }
func (p preparedRecord) BSON() ([]byte, error) { return p.b, nil }
func (p Plan) Prepare(ctx context.Context, src any) (preparedRecord, error) {
	j, e := p.EncodeJSON(ctx, src)
	if e != nil {
		return preparedRecord{}, e
	}
	b, e := p.EncodeBSON(ctx, src)
	return preparedRecord{j, b}, e
}
func NewPlan(src any, paths ...string) Plan {
	var c recordcodec.Codec
	var err error
	switch src.(type) {
	case store.Run:
		c, err = store.ExactRecordCodec(store.ExactRun)
	case store.Checkpoint:
		c, err = store.ExactRecordCodec(store.ExactCheckpoint)
	case store.Interaction:
		c, err = store.ExactRecordCodec(store.ExactInteraction)
	case store.Event:
		c, err = store.ExactRecordCodec(store.ExactEvent)
	case store.Artifact:
		c, err = store.ExactRecordCodec(store.ExactArtifact)
	case queue.RunMessage:
		c, err = queue.ExactRunMessageCodec()
	}
	if err != nil {
		panic(err)
	}
	p := Plan{Codec: c, Type: reflect.TypeOf(src)}
	for _, path := range paths {
		p.Slots = append(p.Slots, slot{strings.Split(path, "/")})
	}
	return p
}
func leafType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}
func appendPath(path []string, s string) []string { return append(append([]string(nil), path...), s) }

var cpPaths = []string{"Outputs", "LoopPreviousOutput", "LoopCurrentOutput", "Artifacts", "Vars", "InteractionQuestions", "FiredEvents", "Parallel/PendingInteractionQuestions", "Parallel/Branches/*/Outputs", "Parallel/Branches/*/Artifacts", "Parallel/Branches/*/LoopPreviousOutput", "Parallel/Branches/*/LoopCurrentOutput", "Parallel/Branches/*/ResumeAnswers"}

func prefixPaths(prefix string, paths []string) []string {
	a := make([]string, len(paths))
	for i, p := range paths {
		a[i] = prefix + p
	}
	return a
}

var runPlan = NewPlan(store.Run{}, append([]string{"Inputs"}, prefixPaths("Checkpoint/", cpPaths)...)...)
var cpPlan = NewPlan(store.Checkpoint{}, cpPaths...)
var interactionPlan = NewPlan(store.Interaction{}, "Questions", "Answers", "Turns/[]/Verdict")
var eventPlan = NewPlan(store.Event{}, "Data")
var artifactPlan = NewPlan(store.Artifact{}, "Data")
var queuePlan = NewPlan(queue.RunMessage{}, "Vars", "Resume/Answers", "Resume/HostInputs")

func payload() map[string]any {
	return map[string]any{"large": json.Number("9007199254740993.0"), "decimal": json.Number("0.123456789012345678901234567890"), "huge": json.Number("1e400"), "uint": uint64(18446744073709551615), "raw-looking": []any{valuecodec.Version, []any{[]any{"number", "7"}}}}
}
func outputs() map[string]map[string]any {
	return map[string]map[string]any{"a.$literal": payload(), "null": nil, "empty": {}}
}

var instant = time.Date(2026, 11, 3, 17, 29, 1, 123000000, time.UTC)

func fixtureRun() *store.Run {
	return &store.Run{ID: "run", FormatVersion: 1, CreatedAt: instant, Inputs: payload(), PublicContract: json.RawMessage(`{"z":9007199254740993.0,"a":2}`), Checkpoint: &store.Checkpoint{NodeID: "gate", Outputs: outputs(), LoopPreviousOutput: outputs(), LoopCurrentOutput: outputs(), Artifacts: outputs(), Vars: payload(), InteractionQuestions: payload(), FiredEvents: outputs(), BackendConversation: json.RawMessage(`[{"x":9007199254740993.0}]`), Parallel: &store.ParallelCheckpoint{RouterNodeID: "router", PendingInteractionQuestions: payload(), Branches: map[string]*store.BranchCheckpoint{"b.$literal": {BranchID: "b", Outputs: outputs(), Artifacts: outputs(), LoopPreviousOutput: outputs(), LoopCurrentOutput: outputs(), ResumeAnswers: payload()}, "nil": nil}}}}
}
func TestExactRecordAll22Payloads(t *testing.T) {
	if len(runPlan.Slots)+len(interactionPlan.Slots)+len(eventPlan.Slots)+len(artifactPlan.Slots)+len(queuePlan.Slots) != 22 {
		t.Fatal("slot count")
	}
	cases := []struct {
		name string
		p    Plan
		src  any
	}{{"run", runPlan, fixtureRun()}, {"checkpoint", cpPlan, fixtureRun().Checkpoint}, {"interaction", interactionPlan, &store.Interaction{ID: "i", RunID: "r", RequestedAt: instant, Questions: payload(), Answers: payload(), Turns: []store.InteractionTurn{{Role: "companion", At: instant, Verdict: payload()}, {Role: "human", At: instant, Verdict: nil}, {Role: "companion", Verdict: map[string]any{}}}}}, {"event", eventPlan, &store.Event{Timestamp: instant, Type: store.EventNodeFinished, Data: payload()}}, {"artifact", artifactPlan, &store.Artifact{WrittenAt: instant, Data: payload()}}, {"queue", queuePlan, &queue.RunMessage{Vars: payload(), Resume: &queue.ResumeSpec{Answers: payload(), HostInputs: payload()}, IRCompiled: json.RawMessage(`{"z":9007199254740993.0,"a":2}`)}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, _ := json.Marshal(tc.src)
			prepared, err := tc.p.Prepare(t.Context(), tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, transport := range []string{"json", "bson"} {
				dst := reflect.New(tc.p.Type)
				var wire []byte
				if transport == "json" {
					wire, err = prepared.JSON()
					if err == nil {
						err = tc.p.DecodeJSON(t.Context(), wire, dst.Interface())
					}
				} else {
					wire, err = prepared.BSON()
					if err == nil {
						err = tc.p.DecodeBSON(t.Context(), wire, dst.Interface())
					}
				}
				if err != nil {
					t.Fatal(transport, err)
				}
				if !reflect.DeepEqual(reflect.ValueOf(tc.src).Elem().Interface(), dst.Elem().Interface()) {
					t.Fatalf("%s record not identical\nsrc=%#v\ndst=%#v", transport, tc.src, dst.Interface())
				}
				t.Logf("%s actual type=%s payload occurrences=%d wire bytes=%d exact=true", transport, tc.p.Type, len(tc.p.Slots), len(wire))
			}
			after, _ := json.Marshal(tc.src)
			if !bytes.Equal(snapshot, after) {
				t.Fatal("source mutated")
			}
		})
	}
}
func TestExactRecordNilEmptyAndOmission(t *testing.T) {
	for _, inputs := range []map[string]any{nil, {}, payload()} {
		src := &store.Run{Inputs: inputs}
		p, err := runPlan.Prepare(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"json", "bson"} {
			var got store.Run
			var wire []byte
			if kind == "json" {
				wire, err = p.JSON()
				if err == nil {
					err = runPlan.DecodeJSON(t.Context(), wire, &got)
				}
			} else {
				wire, err = p.BSON()
				if err == nil {
					err = runPlan.DecodeBSON(t.Context(), wire, &got)
				}
			}
			if err != nil || !reflect.DeepEqual(got.Inputs, inputs) || got.Checkpoint != nil {
				t.Fatalf("nil/empty %s in=%v got=%v err=%v", kind, inputs, got.Inputs, err)
			}
			if kind == "json" {
				var doc map[string]json.RawMessage
				json.Unmarshal(wire, &doc)
				if len(doc["inputs"]) == 0 || doc["inputs"][0] != '[' {
					t.Fatal("omitempty removed shape barrier")
				}
				if _, present := doc["checkpoint"]; present {
					t.Fatal("synthesized nil parent")
				}
			}
		}
	}
	t.Log("nil input -> explicit null frame; empty input -> object frame; nil checkpoint stays omitted")
}
func TestExactRecordGranularMatchesFull(t *testing.T) {
	r := fixtureRun()
	full, _ := runPlan.Prepare(t.Context(), r)
	part, _ := cpPlan.Prepare(t.Context(), r.Checkpoint)
	fj, _ := full.JSON()
	pj, _ := part.JSON()
	var jd map[string]json.RawMessage
	json.Unmarshal(fj, &jd)
	if !bytes.Equal(jd["checkpoint"], pj) {
		t.Fatal("checkpoint JSON differs")
	}
	fb, _ := full.BSON()
	pb, _ := part.BSON()
	var fullCP store.Checkpoint
	if err := cpPlan.DecodeBSON(t.Context(), bson.Raw(fb).Lookup("checkpoint").Document(), &fullCP); err != nil {
		t.Fatal(err)
	}
	var partial store.Checkpoint
	if err := cpPlan.DecodeBSON(t.Context(), pb, &partial); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fullCP, partial) {
		t.Fatal("checkpoint BSON differs")
	}
	frame, err := valuecodec.EncodeMap(t.Context(), r.Checkpoint.Parallel.Branches["b.$literal"].ResumeAnswers)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := bson.Marshal(bson.M{"$set": bson.M{"checkpoint": bson.Raw(pb), "answers": frame}})
	if err != nil {
		t.Fatal(err)
	}
	if bson.Raw(wire).Lookup("$set", "answers").Type != bson.TypeArray {
		t.Fatal("granular answers unframed")
	}
	literal, err := bson.Marshal(bson.M{"$set": bson.M{"checkpoint": bson.M{"$literal": bson.Raw(pb)}}})
	if err != nil || bson.Raw(literal).Lookup("$set", "checkpoint", "$literal").Type != bson.TypeEmbeddedDocument {
		t.Fatal("literal corrupted")
	}
	t.Log("full/checkpoint JSON equal; BSON equivalent; granular answers same leaf; $literal preserved (no server execution)")
}

type evil int

var evilCalls int

func (e evil) MarshalJSON() ([]byte, error) { evilCalls++; return []byte("0"), nil }
func TestExactRecordRefusalBeforeOrdinaryMarshal(t *testing.T) {
	for _, v := range []any{evil(1), json.RawMessage(`{"x":1}`), time.Now()} {
		r := fixtureRun()
		r.Checkpoint.Parallel.Branches["b.$literal"].ResumeAnswers["bad"] = v
		_, err := runPlan.Prepare(t.Context(), r)
		if !errors.Is(err, valuecodec.ErrUnsupportedValue) {
			t.Fatalf("bad type %T not refused: %v", v, err)
		}
	}
	if evilCalls != 0 {
		t.Fatal("payload callback ran")
	}
	r := fixtureRun()
	if _, err := bson.Marshal(r); err == nil {
		t.Fatal("baseline BSON should refuse 1e400 before mask")
	}
	if _, err := runPlan.Prepare(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	t.Log("native BSON rejects huge Number; adapter masks first; custom/raw/time payloads refused without callbacks")
}
func TestExactRecordMissingFutureWrongShape(t *testing.T) {
	p, _ := runPlan.Prepare(t.Context(), fixtureRun())
	raw, _ := p.JSON()
	var original map[string]json.RawMessage
	json.Unmarshal(raw, &original)
	for _, tc := range []struct {
		name   string
		value  json.RawMessage
		remove bool
		future bool
	}{{"missing", nil, true, false}, {"future", json.RawMessage(`["iterion.exact-values/999"]`), false, true}, {"legacy-map", json.RawMessage(`{"n":1}`), false, false}, {"root-array", json.RawMessage(`["iterion.exact-values/1",[["array",[]]]]`), false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]json.RawMessage{}
			for k, v := range original {
				m[k] = v
			}
			if tc.remove {
				delete(m, "inputs")
			} else {
				m["inputs"] = tc.value
			}
			bad, _ := json.Marshal(m)
			dst := store.Run{ID: "untouched", Inputs: map[string]any{"sentinel": true}}
			err := runPlan.DecodeJSON(t.Context(), bad, &dst)
			if err == nil || dst.ID != "untouched" {
				t.Fatal("masked failure into metadata or partial destination", err)
			}
			if tc.future && !errors.Is(err, valuecodec.ErrUnsupportedVersion) {
				t.Fatal(err)
			}
			t.Logf("%s rejected before assignment: %v", tc.name, err)
		})
	}
	// A present checkpoint with its required payload omitted is also not legacy.
	var cp map[string]json.RawMessage
	json.Unmarshal(original["checkpoint"], &cp)
	delete(cp, "vars")
	original["checkpoint"], _ = json.Marshal(cp)
	bad, _ := json.Marshal(original)
	if err := runPlan.DecodeJSON(t.Context(), bad, &store.Run{}); err == nil {
		t.Fatal("missing nested payload accepted")
	}
}
func TestExactRecordUnknownMetadata(t *testing.T) {
	p, _ := runPlan.Prepare(t.Context(), fixtureRun())
	raw, _ := p.JSON()
	var d map[string]json.RawMessage
	json.Unmarshal(raw, &d)
	d["future_meta"] = json.RawMessage(`{"n":9007199254740993.0,"looks_like_frame":["iterion.exact-values/999"]}`)
	raw, _ = json.Marshal(d)
	var dst store.Run
	if err := runPlan.DecodeJSON(t.Context(), raw, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Inputs["large"] != json.Number("9007199254740993.0") {
		t.Fatal(dst.Inputs)
	}
	t.Log("unknown metadata ignored by typed decoder without inspecting its future-looking frame; preservation needs raw preimage owner")
}
func TestExactRecordCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runPlan.Prepare(ctx, fixtureRun()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestExactRecordBSONMissingFuture(t *testing.T) {
	p, _ := runPlan.Prepare(t.Context(), fixtureRun())
	raw, _ := p.BSON()
	var d bson.D
	bson.Unmarshal(raw, &d)
	for _, tc := range []struct {
		name   string
		value  any
		remove bool
		future bool
	}{{"missing", nil, true, false}, {"future", bson.A{"iterion.exact-values/999"}, false, true}, {"legacy-map", bson.M{"n": int64(1)}, false, false}} {
		copy := append(bson.D{}, d...)
		for i, e := range copy {
			if e.Key == "inputs" {
				if tc.remove {
					copy = append(copy[:i], copy[i+1:]...)
				} else {
					copy[i].Value = tc.value
				}
				break
			}
		}
		wire, _ := bson.Marshal(copy)
		dst := store.Run{ID: "untouched"}
		err := runPlan.DecodeBSON(t.Context(), wire, &dst)
		if err == nil || dst.ID != "untouched" {
			t.Fatal(tc.name, err)
		}
		if tc.future && !errors.Is(err, valuecodec.ErrUnsupportedVersion) {
			t.Fatal(err)
		}
		t.Logf("BSON %s refused: %v", tc.name, err)
	}
}

// Test-only inventory guards drift; the runtime never discovers payload maps.
func TestExactRecordExplicitRegistryExhaustive(t *testing.T) {
	plans := []Plan{runPlan, interactionPlan, eventPlan, artifactPlan, queuePlan}
	total := 0
	for _, p := range plans {
		var discovered []string
		var walk func(reflect.Type, []string)
		walk = func(typ reflect.Type, path []string) {
			typ = leafType(typ)
			if typ == reflect.TypeOf(json.RawMessage{}) || typ == reflect.TypeOf(time.Time{}) {
				return
			}
			if typ.Kind() == reflect.Map {
				if typ.Key().Kind() != reflect.String {
					return
				}
				elem := typ.Elem()
				if elem.Kind() == reflect.Interface || elem.Kind() == reflect.Map && elem.Elem().Kind() == reflect.Interface {
					discovered = append(discovered, strings.Join(path, "/"))
					return
				}
				walk(elem, appendPath(path, "*"))
				return
			}
			if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
				walk(typ.Elem(), appendPath(path, "[]"))
				return
			}
			if typ.Kind() == reflect.Struct {
				for i := 0; i < typ.NumField(); i++ {
					f := typ.Field(i)
					if f.PkgPath == "" {
						walk(f.Type, appendPath(path, f.Name))
					}
				}
			}
		}
		walk(p.Type, nil)
		var explicit []string
		for _, s := range p.Slots {
			explicit = append(explicit, strings.Join(s.Fields, "/"))
		}
		sort.Strings(discovered)
		sort.Strings(explicit)
		if !reflect.DeepEqual(discovered, explicit) {
			t.Fatalf("%s registry mismatch discovered=%v explicit=%v", p.Type, discovered, explicit)
		}
		total += len(explicit)
	}
	if total != 22 {
		t.Fatal(total)
	}
	t.Log("22 explicit paths equal mechanical inventory of actual store+queue types")
}
