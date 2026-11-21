package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestBusinessStoresMergeValidatesEachSourceAndPreservesInputs(t *testing.T) {
	a := ContextRef{ID: "a", Kind: "postgres", Namespace: "team", Required: true}
	b := ContextRef{ID: "b", Kind: "postgres", Namespace: "team"}
	source := []ContextRef{b, a}
	got, err := MergeBusinessStores(source, []ContextRef{a})
	if err != nil || !reflect.DeepEqual(got, []ContextRef{a, b}) {
		t.Fatalf("merge=%+v err=%v", got, err)
	}
	if !reflect.DeepEqual(source, []ContextRef{b, a}) {
		t.Fatal("merge mutates input")
	}
	for _, tc := range []struct {
		name    string
		sources [][]ContextRef
		want    string
	}{
		{"duplicate-in-source", [][]ContextRef{{a, a}, {a}}, "duplicate"},
		{"missing-id", [][]ContextRef{{{Kind: "postgres"}}}, ".id is required"},
		{"missing-kind", [][]ContextRef{{{ID: "a"}}}, ".kind is required"},
		{"required-conflict", [][]ContextRef{{a}, {{ID: a.ID, Kind: a.Kind, Namespace: a.Namespace}}}, "conflicting"},
		{"revision-conflict", [][]ContextRef{{a}, {{ID: a.ID, Kind: a.Kind, Namespace: a.Namespace, Required: true, Revision: "changed"}}}, "conflicting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MergeBusinessStores(tc.sources...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("merge err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestBusinessStoresKeepCompositeIdentityAndStableOrdering(t *testing.T) {
	first := ContextRef{Kind: "a", Namespace: "b\x00c", ID: "d"}
	second := ContextRef{Kind: "a\x00b", Namespace: "c", ID: "d"}
	ctx := &ExecutionContext{RunStore: ContextRef{ID: "runs", Kind: "fs"}, BusinessStores: []ContextRef{second, first}}
	if err := ctx.Normalize(); err != nil {
		t.Fatal(err)
	}
	want := []ContextRef{first, second}
	if !reflect.DeepEqual(ctx.BusinessStores, want) {
		t.Fatalf("normalization=%+v", ctx.BusinessStores)
	}
	got, err := MergeBusinessStores([]ContextRef{second}, []ContextRef{first})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("merge=%+v err=%v", got, err)
	}
}
