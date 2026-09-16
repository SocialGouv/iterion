package dryrun

import (
	"fmt"
	"strings"
)

// died says a pass ended neither finished, nor at a fail node the bot
// declared, nor at the bot's own ceiling — the one reading of a death, for
// the parent's passes and the children's alike. A pass out of time is a
// death here: what it would have met is unknown.
func (p Pass) died() bool {
	return p.Status != "finished" && !p.Deliberate && !p.Ceiling
}

// Clean reports whether the passes met nothing to fix: every pass — of the
// program and of every child it simulated — finished, ended at a fail node
// the bot declared, or ran to the bot's own ceiling; no reference kept as
// written, no shell text refused, no fixture off its schema or naming
// nothing. Unchecked things, shapes and ceilings are said, not held
// against the bot.
func (r *Report) Clean() bool {
	for _, p := range r.Passes {
		if p.died() {
			return false
		}
	}
	for _, c := range r.Children {
		if c.died() {
			return false
		}
	}
	for _, f := range r.Findings {
		if f.Kind == KindUnresolvedRef || f.Kind == KindShellSyntax || f.Kind == KindFixture {
			return false
		}
	}
	return true
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

// writePass is the one line of a pass, the program's or a child's.
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
	if len(r.Findings) == 0 {
		b.WriteString("  findings: none\n")
	} else {
		fmt.Fprintf(&b, "  findings (%d):\n", len(r.Findings))
		for _, f := range r.Findings {
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
		b.WriteString("  verdict: clean — every pass finished or refused as declared, nothing left as written, no shell text refused\n")
	case r.timedOut():
		b.WriteString("  verdict: not clean — a pass ran out of time before the run ended (the dry run's bound, --exec-timeout; children share it), and what it met past that point is unknown\n")
	default:
		b.WriteString("  verdict: not clean — a pass died, or a reference, shell or fixture finding stands\n")
	}
	return b.String()
}
