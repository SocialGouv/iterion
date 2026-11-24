package model

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// wildcardOrderToolCount is high enough that map order agreeing with sorted
// order by chance is not a thing that happens: 1/20! for a single call, and
// the test takes two.
const wildcardOrderToolCount = 20

// TestAWildcardExpandsInAStableOrder pins the order a third-party MCP wildcard
// splices into `tools:`, because that order is inside the prompt-cached prefix.
//
// `buildRequest` marks the LAST element of the tools array with an ephemeral
// cache_control breakpoint. A prefix is cached up to and including the marked
// block, so if the array's tail changes between two turns, the second turn
// writes a new entry instead of reading the first — the whole tools prefix
// re-billed, silently, on an unchanged tree. Measured on the system prefix in
// #3132: the corpus behind the marker was 96% of the billed bytes, and the
// same arithmetic applies here.
//
// `Registry.ListByServer` ranges the registry's map and documents that its
// order is not guaranteed; Go randomises map iteration per iteration. So the
// defect was not "unsorted", it was "a different order per CALL".
//
// Two assertions, because they fail for different reasons: the order must be
// the sorted one (a specific, reviewable order rather than whatever the map
// gave), and two resolutions of an unchanged registry must agree (the property
// the cache actually depends on). An implementation that sorts satisfies both;
// one that sorts by anything unstable satisfies the first and fails the second.
//
// Mutation seen red: dropping `sort.Strings(qualified)` in expandWildcards
// fails this test on both assertions and nothing else in the package — the
// pre-existing wildcard tests normalise with `slices.Sort(got)` before
// comparing, which is exactly why this went unseen.
func TestAWildcardExpandsInAStableOrder(t *testing.T) {
	t.Parallel()
	e := internalMCPExecutor(t)
	// No manager, which is the branch expandWildcards guards for: the subject
	// here is the ORDER of the splice, and a manager would make the test about
	// booting a third-party server instead — it would have to either spawn a
	// process or refuse "slack" as unknown, and neither reaches the sort. The
	// tools are in the registry already, which is the state the ensure exists
	// to reach.
	e.mcpManager = nil

	// Registered in an order that is neither sorted nor reverse-sorted, so a
	// pass cannot come from the registry happening to preserve insertion.
	for _, i := range []int{7, 19, 3, 11, 0, 15, 8, 1, 17, 5, 13, 2, 9, 18, 6, 14, 4, 12, 10, 16} {
		name := "op_" + strconv.Itoa(i)
		if err := e.toolRegistry.RegisterMCP("slack", name, "unpinned", nil, jsonExec(`{"ok":true}`)); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "triage"},
		Tools:            []string{"skill"},
		Capabilities:     boardops.AllCapabilities(),
		ActiveMCPServers: []string{"slack"},
	}

	resolve := func(t *testing.T) []string {
		t.Helper()
		defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.slack.*"})
		if err != nil {
			t.Fatalf("resolve mcp.slack.*: %v", err)
		}
		// NOT sorted here: the order under test is the one resolution
		// produced. Normalising it is how the existing wildcard tests hid
		// this.
		return qualifiedToolNames(defs)
	}

	first := resolve(t)
	if len(first) < wildcardOrderToolCount {
		t.Fatalf("the wildcard expanded to %d tools, want at least %d: the fixture is not exercising the order", len(first), wildcardOrderToolCount)
	}

	wildcarded := make([]string, 0, len(first))
	for _, n := range first {
		if len(n) > len("mcp.slack.") && n[:len("mcp.slack.")] == "mcp.slack." {
			wildcarded = append(wildcarded, n)
		}
	}
	// A floor, not an equality: the shared fixture already registers one tool
	// on this server, and a fixture that grows must not fail an order test.
	// What the floor buys is that the order is actually exercised.
	if len(wildcarded) < wildcardOrderToolCount {
		t.Fatalf("the wildcard spliced %d names of its server, want at least %d: the fixture is not exercising the order", len(wildcarded), wildcardOrderToolCount)
	}

	want := slices.Clone(wildcarded)
	slices.Sort(want)
	if !slices.Equal(wildcarded, want) {
		t.Errorf("a wildcard spliced its server's tools in a non-sorted order.\n got: %v\nwant: %v\nThat order lands in the tools array whose last element carries the cache breakpoint, so it must be a reviewable order and not the registry's map order", wildcarded, want)
	}

	// The property the prompt cache depends on: an unchanged registry resolves
	// to the same bytes twice. This is the half a sort by an unstable key
	// would still fail.
	second := resolve(t)
	if !slices.Equal(first, second) {
		t.Errorf("two resolutions of an unchanged registry disagreed, so the tools prefix is not byte-stable between turns.\nfirst:  %v\nsecond: %v", first, second)
	}
}
