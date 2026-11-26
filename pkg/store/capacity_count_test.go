package store

import (
	"maps"
	"slices"
	"testing"
	"time"
)

func holder(id string, subjects ...string) Run {
	return Run{ID: id, Status: RunStatusRunning, CapacitySubjects: subjects}
}

// The per-subject count reads each holder once and each subbot tree once,
// selects on the prefix alone, and answers the same whatever the order of the
// listing: the tree of a child is its parent's, never "the first holder seen".
func TestCountCapacityHolders(t *testing.T) {
	gwA := GatewayCapacitySubject("a.example")
	gwB := GatewayCapacitySubject("b.example")
	child := holder("child", gwA)
	child.ParentRunID, child.ParentNodeID = "one", "review"
	runs := []Run{
		holder("one", gwA),
		holder("two", gwA, gwB),
		holder("three", gwB, gwB),
		holder("four", "x:"+gwA),
		child,
	}
	want := map[string]CapacityCount{
		gwA: {Runs: 3, Trees: 2},
		gwB: {Runs: 2, Trees: 2},
	}
	for _, order := range []string{"listed", "reversed"} {
		in := slices.Clone(runs)
		if order == "reversed" {
			slices.Reverse(in)
		}
		got := CountCapacityHolders(in, GatewayCapacitySubjectPrefix)
		if !maps.Equal(got, want) {
			t.Fatalf("%s order: CountCapacityHolders = %v, want %v — a holder counted once per run, a child under its parent's tree, `x:gw:` no gateway subject", order, got, want)
		}
	}
}

// The tree of a run follows subbot links alone, and only to an alive parent:
// a fork or a shard carries a ParentRunID with no ParentNodeID and is a tree
// of its own; a child whose parent ended is its own root; a cycle, which no
// writer produces, ends the climb.
func TestAliveSubbotTreeRoots(t *testing.T) {
	sub := func(id, parent string) Run {
		return Run{ID: id, Status: RunStatusRunning, ParentRunID: parent, ParentNodeID: "node-" + id}
	}
	fork := Run{ID: "fork", Status: RunStatusRunning, ParentRunID: "root"}
	runs := []Run{
		{ID: "root", Status: RunStatusRunning},
		sub("mid", "root"),
		sub("leaf", "mid"),
		fork,
		sub("orphan", "ended"),
		sub("cyc-a", "cyc-b"),
		sub("cyc-b", "cyc-a"),
	}
	want := map[string]string{
		"root": "root", "mid": "root", "leaf": "root",
		"fork":   "fork",
		"orphan": "orphan",
		"cyc-a":  "cyc-a", "cyc-b": "cyc-a",
	}
	for _, order := range []string{"listed", "reversed"} {
		in := slices.Clone(runs)
		if order == "reversed" {
			slices.Reverse(in)
		}
		done := make(chan map[string]string, 1)
		go func() { done <- AliveSubbotTreeRoots(in) }()
		select {
		case got := <-done:
			if !maps.Equal(got, want) {
				t.Fatalf("%s order: AliveSubbotTreeRoots = %v, want %v", order, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s order: AliveSubbotTreeRoots did not return — a cycle of parent links must end the climb", order)
		}
	}
}

// GatewayHostOfSubject is GatewayCapacitySubject's inverse, and names no host
// for anything that is not a gateway subject.
func TestGatewayHostOfSubject(t *testing.T) {
	for _, host := range []string{"a.example", "10.0.0.7", "xn--bcher-kva.example"} {
		subject := GatewayCapacitySubject(host)
		if got, ok := GatewayHostOfSubject(subject); !ok || got != host {
			t.Fatalf("GatewayHostOfSubject(%q) = %q, %v; want %q, true", subject, got, ok, host)
		}
	}
	for _, other := range []string{"fp", "account:x", "x:gw:a", "gw:", ""} {
		if got, ok := GatewayHostOfSubject(other); ok || got != "" {
			t.Fatalf("GatewayHostOfSubject(%q) = %q, %v; want no host", other, got, ok)
		}
	}
}
