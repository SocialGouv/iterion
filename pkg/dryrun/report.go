package dryrun

import (
	"fmt"
	"strings"
)

// Clean reports whether the passes met nothing to fix: every pass finished
// or ended at a fail node the bot declared, no reference kept as written,
// no shell text refused, no fixture off its schema. Unchecked things and
// shapes are said, not held against the bot.
func (r *Report) Clean() bool {
	for _, p := range r.Passes {
		if p.Status != "finished" && !p.Deliberate {
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

// Render is the human reading of the report.
func (r *Report) Render() string {
	var b strings.Builder
	b.WriteString("Dry run — two passes, every condition true then false; no model, no shell, no workspace\n")
	for _, p := range r.Passes {
		fmt.Fprintf(&b, "  pass %-5v %s — %d nodes, %d edges", p.Bias, p.Status, len(p.Nodes), len(p.Edges))
		if p.Deliberate {
			b.WriteString(" (a fail node the bot declares)")
		}
		if p.Failure != "" {
			fmt.Fprintf(&b, " — %s", p.Failure)
		}
		b.WriteString("\n")
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
	return b.String()
}
