package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
			if iteration != "none" {
				w.Entry = "pick"
				if !w.CanReachVia(open, func(n Node) bool { return n.NodeID() == "work" }) {
					t.Error("the iteration body is reachable before exhaustion, even when no exit survives")
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
			if iteration != "none" {
				w.Entry = "pick"
				if !w.CanReachVia(open, func(n Node) bool { return n.NodeID() == "work" }) {
					t.Error("the iteration body is reachable before exhaustion, even when no exit survives")
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
			if iteration != "none" {
				w.Entry = "pick"
				if !w.CanReachVia(open, func(n Node) bool { return n.NodeID() == "work" }) {
					t.Error("the iteration body is reachable before exhaustion, even when no exit survives")
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
package ir

import "testing"

// Both walks share the node's own end, including exhaustion of either kind
// of bounded iteration. A target before that end (or on the node itself)
// still counts; a target behind its false guards does not.
func TestLaunchWalkStopsAtTheNodesOwnEnd(t *testing.T) {
	for _, iteration := range []string{"none", "loop", "foreach"} {
		t.Run(iteration, func(t *testing.T) {
			w := pickGraph(RouterCondition, map[string]*Var{"f": varDecl("f", VarString, "gpt")},
				whenEdge("pick", "a", "vars.f == 'claude'"),
				whenEdge("pick", "b", "vars.f == 'other'"))
			w.Entry = "work"
			w.Nodes["work"] = &AgentNode{BaseNode: BaseNode{ID: "work"}}
			w.Edges = append(w.Edges, &Edge{From: "work", To: "pick"})
			switch iteration {
			case "loop":
				w.Loops = map[string]*Loop{"retry": {Name: "retry", MaxIterations: 2}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", LoopName: "retry"})
			case "foreach":
				w.Foreaches = map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}}
				w.Edges = append(w.Edges, &Edge{From: "pick", To: "work", ForeachName: "items"})
			}
			open := w.LaunchVarEdgeFilter(nil)
			if open == nil {
				t.Fatal("the node's end must keep the prune even without a terminal")
			}
			for _, tc := range []struct {
				id   string
				want bool
			}{{"a", false}, {"b", false}, {"pick", true}, {"work", true}} {
				target := func(n Node) bool { return n.NodeID() == tc.id }
				if got := w.CanReachVia(open, target); got != tc.want {
					t.Errorf("CanReachVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
				if got := w.AlwaysReachesVia(open, target); got != tc.want {
					t.Errorf("AlwaysReachesVia(%s)=%v, want %v", tc.id, got, tc.want)
				}
			}
			if iteration != "none" {
				w.Entry = "pick"
				if !w.CanReachVia(open, func(n Node) bool { return n.NodeID() == "work" }) {
					t.Error("the iteration body is reachable before exhaustion, even when no exit survives")
				}
			}
		})
	}
}

func TestLaunchWalkStopsWhenItExcludesTheIterationItself(t *testing.T) {
	w := iterationGraph(map[string]*Var{"f": varDecl("f", VarString, "gpt")},
		iterationEdge("loop", "vars.f == 'claude'"), nil)
	open := w.LaunchVarEdgeFilter(nil)
	if open == nil || w.AlwaysReachesVia(open, noWall) {
		t.Fatal("a false iteration guard leaves a node's own end, not a wall")
	}
}
