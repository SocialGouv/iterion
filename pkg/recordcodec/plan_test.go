package recordcodec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type testLeaf struct {
	Data map[string]any `json:"data,omitempty" bson:"data,omitempty"`
}
type testRecord struct {
	Data     map[string]any       `json:"data,omitempty" bson:"data,omitempty"`
	Child    *testLeaf            `json:"child,omitempty" bson:"child,omitempty"`
	Branches map[string]*testLeaf `json:"branches,omitempty" bson:"branches,omitempty"`
	Turns    []testLeaf           `json:"turns,omitempty" bson:"turns,omitempty"`
	Meta     json.RawMessage      `json:"meta,omitempty" bson:"meta,omitempty"`
}

func testPlan(t *testing.T) *Plan {
	t.Helper()
	p, e := Compile(reflect.TypeFor[testRecord](), "Data", "Child/Data", "Branches/*/Data", "Turns/[]/Data")
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestExactRecordPlanValidation(t *testing.T) {
	for _, paths := range [][]string{{}, {""}, {"Missing"}, {"Meta"}, {"Data", "Data"}, {"Data/x"}, {"Child"}, {"Branches/Data"}, {"Turns/*/Data"}, {"Data", "Data/x"}} {
		if _, err := Compile(reflect.TypeFor[testRecord](), paths...); err == nil {
			t.Fatalf("accepted %q", paths)
		}
	}
	for _, typ := range []reflect.Type{nil, reflect.TypeFor[*testRecord](), reflect.TypeFor[map[string]any](), reflect.TypeFor[struct{ Data map[int]any }](), reflect.TypeFor[struct{ Data map[string]int }](), reflect.TypeFor[struct {
		Data map[string]any `json:"-"`
	}](), reflect.TypeFor[struct {
		Data map[string]any `bson:",inline"`
	}](), reflect.TypeFor[struct {
		Data map[string]any `json:",string"` //nolint:staticcheck // Intentionally invalid tag: Compile must reject it.
	}](), reflect.TypeFor[struct {
		Data map[string]any
		DATA int
	}]()} {
		if _, err := Compile(typ, "Data"); err == nil {
			t.Fatalf("accepted type %v", typ)
		}
	}
	p := testPlan(t)
	for _, v := range []any{nil, (*testRecord)(nil), testLeaf{}, &testLeaf{}} {
		if _, e := p.EncodeJSON(t.Context(), v); e == nil {
			t.Fatalf("source %T accepted", v)
		}
		if e := p.DecodeJSON(t.Context(), []byte(`{}`), v); e == nil {
			t.Fatalf("destination %T accepted", v)
		}
	}
	paths := []string{"Data"}
	p, e := Compile(reflect.TypeFor[testRecord](), paths...)
	if e != nil {
		t.Fatal(e)
	}
	paths[0] = "Meta"
	if _, e = p.EncodeJSON(t.Context(), testRecord{Data: map[string]any{"n": json.Number("1e400")}}); e != nil {
		t.Fatal("plan borrowed path input", e)
	}
}
func TestExactRecordCyclesCancellationAndSharing(t *testing.T) {
	p := testPlan(t)
	cycle := map[string]any{}
	cycle["self"] = cycle
	if _, e := p.EncodeBSON(t.Context(), testRecord{Data: cycle}); e == nil {
		t.Fatal("payload cycle accepted")
	}
	type metadata struct {
		Data  map[string]any
		Other any
	}
	mp, e := Compile(reflect.TypeFor[metadata](), "Data")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = mp.EncodeBSON(t.Context(), metadata{Other: cycle}); e == nil {
		t.Fatal("metadata cycle accepted")
	}
	shared := &testLeaf{Data: map[string]any{"n": json.Number("1e400")}}
	src := testRecord{Child: shared, Branches: map[string]*testLeaf{"a": shared, "b": shared}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, enc := range []func(context.Context, any) ([]byte, error){p.EncodeJSON, p.EncodeBSON} {
		if _, e = enc(ctx, src); !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
		if _, e = enc(t.Context(), src); e != nil {
			t.Fatal("DAG refused", e)
		}
	}
	for _, pair := range []struct {
		enc func(context.Context, any) ([]byte, error)
		dec func(context.Context, []byte, any) error
	}{{p.EncodeJSON, p.DecodeJSON}, {p.EncodeBSON, p.DecodeBSON}} {
		raw, e := pair.enc(t.Context(), src)
		if e != nil {
			t.Fatal(e)
		}
		dst := testRecord{Data: map[string]any{"sentinel": true}}
		if e = pair.dec(ctx, raw, &dst); !errors.Is(e, context.Canceled) || dst.Data["sentinel"] != true {
			t.Fatal("cancellation published", e)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 10; j++ {
				if _, err := p.EncodeBSON(t.Context(), src); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}
func TestExactRecordStructuralJSONRefusal(t *testing.T) {
	p := testPlan(t)
	raw, e := p.EncodeJSON(t.Context(), testRecord{Data: map[string]any{}, Child: &testLeaf{}})
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{`null`, `[]`, `{} {}`, `{"data":null}`, strings.Replace(string(raw), `"data":`, `"DATA":`, 1), strings.Replace(string(raw), `"data":`, `"data":null,"data":`, 1), strings.Replace(string(raw), `"child":{`, `"child":{"data":null,`, 1), strings.Replace(string(raw), `"child":{`, `"child":{"d\uD800":null,`, 1), strings.Replace(string(raw), `"child":{`, `"child":{"d`+string([]byte{255})+`":null,`, 1)} {
		dst := testRecord{Data: map[string]any{"sentinel": true}}
		if err := p.DecodeJSON(t.Context(), []byte(bad), &dst); err == nil || dst.Data["sentinel"] != true {
			t.Fatalf("accepted or published %q: %v", bad, err)
		}
	}
	// Branch identifiers are data keys, so case-distinct entries stay distinct.
	src := testRecord{Branches: map[string]*testLeaf{"A": nil, "a": {}}}
	raw, e = p.EncodeJSON(t.Context(), src)
	if e != nil {
		t.Fatal(e)
	}
	var dst testRecord
	if e = p.DecodeJSON(t.Context(), raw, &dst); e != nil || !reflect.DeepEqual(src, dst) {
		t.Fatal(e, dst)
	}
	var doc bson.D
	raw, e = p.EncodeBSON(t.Context(), testRecord{})
	if e != nil {
		t.Fatal(e)
	}
	if e = bson.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	doc = append(doc, bson.E{Key: "data", Value: nil})
	raw, e = bson.Marshal(doc)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.DecodeBSON(t.Context(), raw, &dst); e == nil {
		t.Fatal("duplicate BSON accepted")
	}
}

type structuralHook struct{ Data map[string]any }

func (structuralHook) MarshalJSON() ([]byte, error) { panic("must not invoke structural callback") }
func TestExactRecordStructuralHookAndTagValidation(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[structuralHook](), reflect.TypeFor[struct {
		Data map[string]any `json:"🙂"`
	}](), reflect.TypeFor[struct {
		Data  map[string]any `json:"a" bson:"B"`
		Other int            `json:"b" bson:"b"`
	}]()} {
		if _, e := Compile(typ, "Data"); e == nil {
			t.Fatal("unmaskable structural type accepted", typ)
		}
	}
	p := testPlan(t)
	raw, e := p.EncodeBSON(t.Context(), testRecord{})
	if e != nil {
		t.Fatal(e)
	}
	var d bson.D
	if e = bson.Unmarshal(raw, &d); e != nil {
		t.Fatal(e)
	}
	d = append(d, bson.E{Key: "DATA", Value: nil})
	raw, e = bson.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.DecodeBSON(t.Context(), raw, &testRecord{}); e == nil {
		t.Fatal("BSON alias bypassed structural mask")
	}
}

func TestExactRecordAliasesAndDuplicatesDiscriminant(t *testing.T) {
	p := testPlan(t)
	raw, err := p.EncodeJSON(t.Context(), testRecord{})
	if err != nil {
		t.Fatal(err)
	}
	// Both metadata values are legal for the ordinary decoder. Only the
	// structural alias guard rejects this input; no missing payload can do so.
	bad := append([]byte(`{"meta":null,"META":{"other":true},`), raw[1:]...)
	if err = p.DecodeJSON(t.Context(), bad, &testRecord{}); err == nil {
		t.Fatal("JSON metadata alias accepted")
	}
	raw, err = p.EncodeBSON(t.Context(), testRecord{})
	if err != nil {
		t.Fatal(err)
	}
	var d bson.D
	if err = bson.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	d = append(d, bson.E{Key: "meta", Value: nil}, bson.E{Key: "meta", Value: nil})
	raw, err = bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.DecodeBSON(t.Context(), raw, &testRecord{}); err == nil {
		t.Fatal("duplicate valid BSON metadata accepted")
	}
}

type recursivePlanMap map[string]recursivePlanMap
type recursivePlanPointer *recursivePlanPointer
type mutualPlanMapA map[string]mutualPlanMapB
type mutualPlanMapB map[string]mutualPlanMapA
type mutualPlanPointerA *mutualPlanPointerB
type mutualPlanPointerB *mutualPlanPointerA

func TestExactRecordCompileRecursiveTypes(t *testing.T) {
	const probeEnv = "ITERION_RECORDCODEC_RECURSIVE_PROBE"
	if mode := os.Getenv(probeEnv); mode != "" {
		// A broken recursive guard must fail this child, never exhaust the parent.
		debug.SetMaxStack(1 << 20)
		var typ reflect.Type
		path := "Data"
		switch mode {
		case "map":
			typ = reflect.TypeFor[struct{ Data recursivePlanMap }]()
		case "mutual-map":
			typ = reflect.TypeFor[struct{ Data mutualPlanMapA }]()
		case "pointer":
			typ = reflect.TypeFor[struct{ Data recursivePlanPointer }]()
			path = "Data/Unknown"
		case "mutual-pointer":
			typ = reflect.TypeFor[struct{ Data mutualPlanPointerA }]()
			path = "Data/Unknown"
		default:
			t.Fatal("unknown recursion probe", mode)
		}
		if _, err := Compile(typ, path); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("recursive type must return ErrInvalidRecord: %v", err)
		}
		return
	}
	for _, mode := range []string{"map", "mutual-map", "pointer", "mutual-pointer"} {
		t.Run(mode, func(t *testing.T) {
			// Test-process safety only: production has no time or depth ceiling.
			deadline := time.Now().Add(5 * time.Second)
			if outer, ok := t.Deadline(); ok && outer.Before(deadline) {
				deadline = outer
			}
			ctx, cancel := context.WithDeadline(t.Context(), deadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExactRecordCompileRecursiveTypes$")
			cmd.Env = append(os.Environ(), probeEnv+"="+mode)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Compile must terminate with ErrInvalidRecord for %s: %v (context %v)\n%s", mode, err, ctx.Err(), output)
			}
		})
	}
}

func TestExactRecordFiniteRecursivePaths(t *testing.T) {
	type recursiveNode struct {
		Data map[string]any
		Next *recursiveNode
	}
	p, err := Compile(reflect.TypeFor[recursiveNode](), "Data", "Next/Data", "Next/Next/Data")
	if err != nil {
		t.Fatal("finite recursive paths refused", err)
	}
	src := recursiveNode{Data: map[string]any{}, Next: &recursiveNode{Next: &recursiveNode{Data: map[string]any{"n": json.Number("1e400")}}}}
	for _, pair := range []struct {
		enc func(context.Context, any) ([]byte, error)
		dec func(context.Context, []byte, any) error
	}{{p.EncodeJSON, p.DecodeJSON}, {p.EncodeBSON, p.DecodeBSON}} {
		raw, err := pair.enc(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		var dst recursiveNode
		if err = pair.dec(t.Context(), raw, &dst); err != nil || !reflect.DeepEqual(src, dst) {
			t.Fatalf("finite recursive path changed: %#v %v", dst, err)
		}
	}
}

func TestExactRecordMetadataSliceIdentity(t *testing.T) {
	type record struct {
		Data map[string]any
		Meta any
	}
	p, err := Compile(reflect.TypeFor[record](), "Data")
	if err != nil {
		t.Fatal(err)
	}
	for _, length := range []int{0, 1} {
		a := make([]any, 2)
		a[1] = a[:length]
		src := record{Meta: a}
		for _, baseline := range []func(any) ([]byte, error){json.Marshal, bson.Marshal} {
			if _, err = baseline(src); err != nil {
				t.Fatal("ordinary codec must admit acyclic overlap", err)
			}
		}
		for _, enc := range []func(context.Context, any) ([]byte, error){p.EncodeJSON, p.EncodeBSON} {
			if _, err = enc(t.Context(), src); err != nil {
				t.Fatalf("acyclic subslice length %d refused: %v", length, err)
			}
		}
	}
	cycle := make([]any, 1)
	cycle[0] = cycle
	for _, enc := range []func(context.Context, any) ([]byte, error){p.EncodeJSON, p.EncodeBSON} {
		if _, err = enc(t.Context(), record{Meta: cycle}); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("true slice cycle must fail in adapter: %v", err)
		}
	}
	shared := map[string]any{"value": 1}
	pointer := &shared
	for _, meta := range []any{[]any{shared, shared}, []any{pointer, pointer}, map[string]any{"a": pointer, "b": pointer}} {
		for _, enc := range []func(context.Context, any) ([]byte, error){p.EncodeJSON, p.EncodeBSON} {
			if _, err = enc(t.Context(), record{Meta: meta}); err != nil {
				t.Fatal("acyclic metadata DAG refused", err)
			}
		}
	}
	self := map[string]any{}
	self["self"] = &self
	for _, enc := range []func(context.Context, any) ([]byte, error){p.EncodeJSON, p.EncodeBSON} {
		if _, err = enc(t.Context(), record{Meta: self}); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("pointer/map cycle must fail in adapter: %v", err)
		}
	}
}

func TestExactRecordMetadataTransportExclusions(t *testing.T) {
	type both struct {
		Data map[string]any
		Meta any `json:"-" bson:"-"`
	}
	type onlyJSON struct {
		Data map[string]any
		Meta any `json:"-" bson:"kept"`
	}
	type onlyBSON struct {
		Data map[string]any
		Meta any `json:"kept" bson:"-"`
	}
	type dashName struct {
		Data map[string]any
		Meta any `json:"-,omitempty" bson:"-,omitempty"` //nolint:staticcheck // Exercise the literal dash name, distinct from an ignored field.
	}
	type private struct {
		Data map[string]any
		meta any
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, tc := range []struct {
		name           string
		src            any
		jsonOK, bsonOK bool
	}{
		{"both", both{Meta: cycle}, true, true}, {"json-only", onlyJSON{Meta: cycle}, true, false},
		{"bson-only", onlyBSON{Meta: cycle}, false, true}, {"dash-is-name", dashName{Meta: cycle}, false, false},
		{"unexported", private{meta: cycle}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(reflect.TypeOf(tc.src), "Data")
			if err != nil {
				t.Fatal(err)
			}
			for _, pair := range []struct {
				name     string
				baseline func(any) ([]byte, error)
				enc      func(context.Context, any) ([]byte, error)
				ok       bool
			}{
				{"json", json.Marshal, p.EncodeJSON, tc.jsonOK}, {"bson", bson.Marshal, p.EncodeBSON, tc.bsonOK},
			} {
				t.Run(pair.name, func(t *testing.T) {
					if pair.ok {
						if _, err = pair.baseline(tc.src); err != nil {
							t.Fatal("ordinary excluded field failed", err)
						}
					}
					_, err = pair.enc(t.Context(), tc.src)
					if pair.ok && err != nil {
						t.Fatal("excluded metadata visited", err)
					}
					if !pair.ok && !errors.Is(err, ErrInvalidRecord) {
						t.Fatalf("visible cycle must fail before ordinary serializer: %v", err)
					}
				})
			}
		})
	}
}
