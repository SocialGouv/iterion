package ir

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A chain of V nodes used to O(V²) memory: validateLoopCapExpressions
// materialised every node's transitive predecessor set whether or not any
// loop carried an expression cap (#2776 — 2.0 GB at V=8 000, OOM at 16 000).
// The sets are now computed on demand, per consumer that actually queries
// reachability, so a chain with no expression cap compiles in O(V).
func chainSource(v int) string {
	var b strings.Builder
	b.WriteString("schema s:\n  v: int\n\n")
	for i := 0; i < v; i++ {
		fmt.Fprintf(&b, "compute n%d:\n  output: s\n  expr:\n    v: \"1\"\n\n", i)
	}
	b.WriteString("workflow w:\n  entry: n0\n")
	for i := 0; i+1 < v; i++ {
		fmt.Fprintf(&b, "  n%d -> n%d\n", i, i+1)
	}
	fmt.Fprintf(&b, "  n%d -> done\n", v-1)
	return b.String()
}

func TestNodeChainCompilesLinearly(t *testing.T) {
	const v = 4000
	src := chainSource(v)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	r := compileFile(t, src)
	el := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc

	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("compile error: %s", d.Error())
		}
	}
	t.Logf("chain of %d nodes: %s, %d MiB allocated", v, el.Round(time.Millisecond), alloc>>20)
	if limit := uint64(256 << 20); alloc > limit {
		t.Errorf("compiling a chain of %d nodes allocated %d MiB, want under %d MiB: predecessor sets are materialised for nodes nobody queries (#2776)", v, alloc>>20, limit>>20)
	}
	if limit := 10 * time.Second; el > limit {
		t.Errorf("compiling a chain of %d nodes took %s, want under %s (#2776)", v, el, limit)
	}
}

// A loop whose cap is an expression referencing outputs still gets its
// reachability checked: the on-demand predecessor index answers the same
// sets buildPredecessors materialised.
func TestLoopCapExpressionReachabilityOnDemand(t *testing.T) {
	src := `
schema s:
  v: int

compute a:
  output: s
  expr:
    v: "1"

compute b:
  output: s
  expr:
    v: "1"

compute c:
  output: s
  expr:
    v: "1"

workflow w:
  entry: a
  a -> b as l("outputs.c.v + 1")
  b -> a as l("outputs.c.v + 1")
  b -> c
  c -> done
`
	r := compileFile(t, src)
	var found bool
	for _, d := range r.Diagnostics {
		if d.Code == DiagRefNodeNotReachable && strings.Contains(d.Message, "not reachable before") {
			found = true
		}
	}
	if !found {
		t.Errorf("a cap referencing a node that cannot precede the loop source must still be refused; diagnostics: %v", r.Diagnostics)
	}
}
