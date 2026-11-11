package tool

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
)

// listingOrderToolCount is high enough that a map happening to iterate in
// sorted order is not something that occurs: 1/24! per call, and each
// assertion below takes two calls.
const listingOrderToolCount = 24

// TestEveryRegistryListingIsDeterministic pins the order of all three listing
// methods, because that order reaches a model request's tools array — whose
// LAST element is where buildRequest puts the ephemeral cache breakpoint.
//
// A prefix is cached up to and including the marked element, so a tail that
// differs between two turns makes the second turn write a new cache entry
// instead of reading the first: the whole tools prefix re-billed on an
// unchanged tree. Go randomises map iteration per iteration, so before this
// the listings disagreed with THEMSELVES across calls — not merely "unsorted".
//
// Two non-test consumers took the unsorted order: the subagent tool set
// (pkg/backend/model/subagent.go) and tool_search's haystack
// (claw_builtins.go). Sorting in the three methods rather than at each caller
// is why this test lives here: they are the only places the map is ranged, so
// a fourth consumer inherits the guarantee instead of the defect.
//
// Each method gets both assertions, and they fail for different reasons:
// equality with the sorted sequence pins a reviewable order, while agreement
// between two calls pins the property the cache depends on — the half that a
// sort by an unstable key would still fail.
//
// Mutation seen red: deleting the `sort.Slice` inside sortByQualifiedName
// reddens this test on all three methods and nothing else in the package.
func TestEveryRegistryListingIsDeterministic(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	// Registered in an order that is neither sorted nor reverse-sorted, so a
	// pass cannot come from the map preserving insertion.
	order := []int{7, 19, 3, 23, 11, 0, 15, 8, 1, 17, 5, 13, 2, 21, 9, 18, 6, 22, 14, 4, 12, 20, 10, 16}
	if len(order) != listingOrderToolCount {
		t.Fatalf("the fixture declares %d names for a count of %d", len(order), listingOrderToolCount)
	}
	exec := func(_ context.Context, _ json.RawMessage) (string, error) { return "{}", nil }
	for _, i := range order {
		name := "op_" + strconv.Itoa(i)
		// Half on one MCP server, half on another, so ListByServer is
		// exercised over a strict subset rather than the whole registry.
		server := "alpha"
		if i%2 == 1 {
			server = "beta"
		}
		if err := r.RegisterMCP(server, name, "unpinned", nil, exec); err != nil {
			t.Fatalf("register %s/%s: %v", server, name, err)
		}
	}

	names := func(defs []*ToolDef) []string {
		out := make([]string, 0, len(defs))
		for _, d := range defs {
			out = append(out, d.QualifiedName)
		}
		return out
	}

	for _, tc := range []struct {
		what  string
		list  func() []*ToolDef
		floor int
	}{
		{"List", func() []*ToolDef { return r.List() }, listingOrderToolCount},
		{"ListByOrigin(OriginMCP)", func() []*ToolDef { return r.ListByOrigin(OriginMCP) }, listingOrderToolCount},
		{"ListByServer(alpha)", func() []*ToolDef { return r.ListByServer("alpha") }, listingOrderToolCount / 2},
	} {
		t.Run(tc.what, func(t *testing.T) {
			first := names(tc.list())
			if len(first) < tc.floor {
				t.Fatalf("%s returned %d tools, want at least %d: the fixture is not exercising the order", tc.what, len(first), tc.floor)
			}

			want := slices.Clone(first)
			slices.Sort(want)
			if !slices.Equal(first, want) {
				t.Errorf("%s returned a non-sorted order, so the tools array it feeds is the registry's map order rather than a reviewable one.\n got: %v\nwant: %v", tc.what, first, want)
			}

			// The property the prompt cache depends on.
			second := names(tc.list())
			if !slices.Equal(first, second) {
				t.Errorf("%s disagreed with itself across two calls on an unchanged registry, so the tools prefix is not byte-stable between turns.\nfirst:  %v\nsecond: %v", tc.what, first, second)
			}
		})
	}
}
