package dryrun

import (
	"fmt"
	"strings"
)

// died says a pass ended neither finished, nor at a fail node the bot
// declared, nor at the bot's own ceiling — the one reading of a death, for
// the parent's passes and the children's alike. A pass out of time is a
// death here: what it would have met is unknown. A pass that finished
// only because a `best_effort` fan-out let a branch's death through the
// collector is a death too: DeadBranches names them (dry-run #1325).
func (p Pass) died() bool {
	if p.Status != "finished" && !p.Deliberate && !p.Ceiling {
		return true
	}
	return len(p.DeadBranches) > 0
}

// Failing reports whether the passes met a defect: a pass — of the program
// or of a child it simulated — died, or a reference kept as written, shell
// text refused, a fixture off its schema or naming nothing stands. The
// verdict `--strict` exits on. An expression the dry run could not decide
// on a shape (KindInconclusive) is not a defect and is not read here.
func (r *Report) Failing() bool {
	for _, p := range r.Passes {
		if p.died() {
			return true
		}
	}
	for _, c := range r.Children {
		if c.died() {
			return true
		}
	}
	for _, f := range r.Findings {
		if f.Kind == KindUnresolvedRef || f.Kind == KindShellSyntax || f.Kind == KindFixture {
			return true
		}
	}
	return false
}

// Inconclusive lists the expressions no pass could decide: each failed
// while reading a value the dry run invented, and says which and what
// would decide it.
func (r *Report) Inconclusive() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Kind == KindInconclusive {
			out = append(out, f)
		}
	}
	return out
}

// Clean reports whether the passes met nothing to fix and left nothing
// undecided: no defect (Failing), and no expression that rested on a
// shape it could not digest (Inconclusive). Unchecked things, shapes and
// ceilings are said, not held against the bot.
func (r *Report) Clean() bool {
	return !r.Failing() && len(r.Inconclusive()) == 0
}

// timedOut says a pass — the program's or a child's — ran out of time.
func (r *Report) timedOut() bool {
	for _, p := range r.Passes {
		if p.TimedOut {
			return true
		}
	}
	for _, c := range r.Children {
		if c.TimedOut {
			return true
		}
	}
	return false
}

// writePass is the one line of a pass, the program's or a child's. A dead
// branch reads its own line beneath, one per branch, with the classifier's
// code (when the error carried one), the message the branch's event
// carried and the crossings that saw it die — a fan-out whose branches all
// die reads as `finished` in the pass status, and the branches beneath are
// where the deaths are named.
func writePass(b *strings.Builder, label string, p Pass) {
	fmt.Fprintf(b, "  %s %-5v %s — %d nodes, %d edges", label, p.Bias, p.Status, len(p.Nodes), len(p.Edges))
	if p.Deliberate {
		b.WriteString(" (a fail node the bot declares)")
	}
	if p.Ceiling {
		b.WriteString(" (ran to the bot's own ceiling: an exit rode a value the dry run shapes — not a death, not a proof)")
	}
	if p.TimedOut {
		b.WriteString(" (ran out of time — the dry run's bound, not the program: raise it with --exec-timeout)")
	}
	if p.Failure != "" {
		fmt.Fprintf(b, " — %s", p.Failure)
	}
	b.WriteString("\n")
	for _, d := range p.DeadBranches {
		fmt.Fprintf(b, "    dead branch %s", d.Branch)
		if d.Node != "" {
			fmt.Fprintf(b, " (from %s)", d.Node)
		}
		if d.Crossings > 1 {
			fmt.Fprintf(b, " (died on %d crossings)", d.Crossings)
		}
		if d.Code != "" {
			fmt.Fprintf(b, " [%s]", d.Code)
		}
		if d.Error != "" {
			fmt.Fprintf(b, ": %s", d.Error)
		}
		b.WriteString("\n")
	}
}

// Render is the human reading of the report.
func (r *Report) Render() string {
	var b strings.Builder
	b.WriteString("Dry run — two passes, every condition true then false; no model, no shell, no workspace\n")
	for _, p := range r.Passes {
		writePass(&b, "pass", p)
	}
	for _, c := range r.Children {
		label := fmt.Sprintf("child %s (%s) pass", c.Node, c.Source)
		if c.Crossings > 1 {
			label = fmt.Sprintf("child %s (%s, crossed %d times) pass", c.Node, c.Source, c.Crossings)
		}
		writePass(&b, label, c.Pass)
	}
	var findings []Finding
	for _, f := range r.Findings {
		if f.Kind != KindInconclusive {
			findings = append(findings, f)
		}
	}
	if len(findings) == 0 {
		b.WriteString("  findings: none\n")
	} else {
		fmt.Fprintf(&b, "  findings (%d):\n", len(findings))
		for _, f := range findings {
			fmt.Fprintf(&b, "    %s\n", f)
		}
	}
	if undecided := r.Inconclusive(); len(undecided) > 0 {
		fmt.Fprintf(&b, "  inconclusive (%d) — expressions that failed on a value the dry run shaped; they decide nothing about the bot until the value is given:\n", len(undecided))
		for _, f := range undecided {
			fmt.Fprintf(&b, "    %s\n", f)
		}
	}
	if len(r.Pinned) > 0 {
		fmt.Fprintf(&b, "  pinned by fixtures: %s — a condition read from them read the recording on both passes\n", strings.Join(r.Pinned, ", "))
	}
	if len(r.Shaped) > 0 {
		fmt.Fprintf(&b, "  shapes: the outputs of %s were schema-shaped — a condition read from them decided nothing about the real bot\n", strings.Join(r.Shaped, ", "))
	}
	if len(r.UnvisitedNodes) > 0 {
		fmt.Fprintf(&b, "  unvisited nodes: %s\n", strings.Join(r.UnvisitedNodes, ", "))
	}
	if len(r.UnvisitedEdges) > 0 {
		parts := make([]string, len(r.UnvisitedEdges))
		for i, e := range r.UnvisitedEdges {
			parts[i] = e.From + " -> " + e.To
		}
		fmt.Fprintf(&b, "  unvisited edges: %s\n", strings.Join(parts, ", "))
	}
	switch {
	case r.Clean():
		b.WriteString("  verdict: clean — every pass finished or refused as declared, nothing left as written, no shell text refused, every expression decided\n")
	case r.timedOut():
		b.WriteString("  verdict: not clean — a pass ran out of time before the run ended (the dry run's bound, --exec-timeout; children share it), and what it met past that point is unknown\n")
	case !r.Failing():
		b.WriteString("  verdict: inconclusive — nothing died and no finding stands, but an expression rested on a value the dry run shaped and could not be decided; give the value a sample (--var, --fixtures) to decide it (--strict does not fail on this)\n")
	default:
		b.WriteString("  verdict: not clean — a pass died, or a reference, shell or fixture finding stands\n")
	}
	return b.String()
}
