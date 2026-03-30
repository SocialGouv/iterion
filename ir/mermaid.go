package ir

import (
	"fmt"
	"sort"
	"strings"
)

// MermaidView controls the level of detail in the generated diagram.
type MermaidView int

const (
	// MermaidCompact shows nodes with kind icons and simple edge labels.
	MermaidCompact MermaidView = iota
	// MermaidDetailed shows nodes with full metadata and annotated edges.
	MermaidDetailed
)

// ToMermaid renders the workflow IR as a Mermaid flowchart string.
func (w *Workflow) ToMermaid(view MermaidView) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")

	// Collect and sort node IDs for deterministic output.
	ids := make([]string, 0, len(w.Nodes))
	for id := range w.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Emit nodes.
	for _, id := range ids {
		node := w.Nodes[id]
		b.WriteString("    ")
		b.WriteString(nodeDecl(node, view))
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Emit edges.
	for _, edge := range w.Edges {
		b.WriteString("    ")
		b.WriteString(edgeDecl(edge, w, view))
		b.WriteString("\n")
	}

	// Emit style classes for node kinds.
	b.WriteString("\n")
	b.WriteString(styleClasses(w))

	return b.String()
}

// nodeDecl returns the Mermaid node declaration for a given node.
func nodeDecl(n *Node, view MermaidView) string {
	id := sanitizeID(n.ID)

	switch view {
	case MermaidDetailed:
		return id + detailedShape(n)
	default:
		return id + compactShape(n)
	}
}

// compactShape returns a compact Mermaid shape with a kind icon prefix.
func compactShape(n *Node) string {
	icon := kindIcon(n.Kind)
	label := icon + " " + n.ID

	switch n.Kind {
	case NodeDone:
		return fmt.Sprintf(`(["%s"])`, label)
	case NodeFail:
		return fmt.Sprintf(`(["%s"])`, label)
	case NodeRouter:
		return fmt.Sprintf(`{"%s"}`, label)
	case NodeJoin:
		return fmt.Sprintf(`[["%s"]]`, label)
	case NodeHuman:
		return fmt.Sprintf(`>"%s"]`, label)
	default:
		return fmt.Sprintf(`["%s"]`, label)
	}
}

// detailedShape returns a detailed Mermaid shape with metadata.
func detailedShape(n *Node) string {
	icon := kindIcon(n.Kind)

	var lines []string
	lines = append(lines, icon+" "+n.ID)

	switch n.Kind {
	case NodeAgent, NodeJudge:
		if n.Model != "" {
			lines = append(lines, "model: "+n.Model)
		}
		if n.InputSchema != "" {
			lines = append(lines, "in: "+n.InputSchema)
		}
		if n.OutputSchema != "" {
			lines = append(lines, "out: "+n.OutputSchema)
		}
		if n.Publish != "" {
			lines = append(lines, "publish: "+n.Publish)
		}
		if n.Session != SessionFresh {
			lines = append(lines, "session: "+n.Session.String())
		}
	case NodeRouter:
		lines = append(lines, "mode: "+n.RouterMode.String())
	case NodeJoin:
		lines = append(lines, "strategy: "+n.JoinStrategy.String())
		if len(n.Require) > 0 {
			lines = append(lines, "require: "+strings.Join(n.Require, ", "))
		}
	case NodeHuman:
		lines = append(lines, "mode: "+n.HumanMode.String())
		if n.MinAnswers > 0 {
			lines = append(lines, fmt.Sprintf("min_answers: %d", n.MinAnswers))
		}
	case NodeTool:
		if n.Command != "" {
			lines = append(lines, "cmd: "+n.Command)
		}
	}

	label := strings.Join(lines, "<br/>")

	switch n.Kind {
	case NodeDone, NodeFail:
		return fmt.Sprintf(`(["%s"])`, label)
	case NodeRouter:
		return fmt.Sprintf(`{"%s"}`, label)
	case NodeJoin:
		return fmt.Sprintf(`[["%s"]]`, label)
	case NodeHuman:
		return fmt.Sprintf(`>"%s"]`, label)
	default:
		return fmt.Sprintf(`["%s"]`, label)
	}
}

// edgeDecl returns the Mermaid edge declaration.
func edgeDecl(e *Edge, w *Workflow, view MermaidView) string {
	from := sanitizeID(e.From)
	to := sanitizeID(e.To)

	label := edgeLabel(e, w, view)
	if label == "" {
		return fmt.Sprintf("%s --> %s", from, to)
	}
	return fmt.Sprintf(`%s -->|"%s"| %s`, from, label, to)
}

// edgeLabel builds the label for an edge.
func edgeLabel(e *Edge, w *Workflow, view MermaidView) string {
	var parts []string

	if e.Condition != "" {
		cond := e.Condition
		if e.Negated {
			cond = "NOT " + cond
		}
		parts = append(parts, cond)
	}

	if e.LoopName != "" {
		loop, ok := w.Loops[e.LoopName]
		if ok {
			parts = append(parts, fmt.Sprintf("loop:%s(%d)", loop.Name, loop.MaxIterations))
		} else {
			parts = append(parts, "loop:"+e.LoopName)
		}
	}

	if view == MermaidDetailed && len(e.With) > 0 {
		var mappings []string
		for _, dm := range e.With {
			mappings = append(mappings, dm.Key+"="+dm.Raw)
		}
		parts = append(parts, "with: "+strings.Join(mappings, ", "))
	}

	return strings.Join(parts, " / ")
}

// styleClasses emits Mermaid classDef and class assignments for node kinds.
func styleClasses(w *Workflow) string {
	var b strings.Builder

	b.WriteString("    classDef agent fill:#4A90D9,stroke:#2C5F8A,color:#fff\n")
	b.WriteString("    classDef judge fill:#7B68EE,stroke:#5A4CB5,color:#fff\n")
	b.WriteString("    classDef router fill:#F5A623,stroke:#C47D0E,color:#fff\n")
	b.WriteString("    classDef join fill:#50C878,stroke:#2D8B4A,color:#fff\n")
	b.WriteString("    classDef human fill:#FF6B6B,stroke:#CC4444,color:#fff\n")
	b.WriteString("    classDef tool fill:#A0522D,stroke:#6E3720,color:#fff\n")
	b.WriteString("    classDef done fill:#2ECC71,stroke:#1A8B4C,color:#fff\n")
	b.WriteString("    classDef fail fill:#E74C3C,stroke:#A93226,color:#fff\n")

	// Group nodes by kind.
	groups := map[NodeKind][]string{}
	for id, node := range w.Nodes {
		groups[node.Kind] = append(groups[node.Kind], sanitizeID(id))
	}

	for kind, nodeIDs := range groups {
		sort.Strings(nodeIDs)
		b.WriteString(fmt.Sprintf("    class %s %s\n", strings.Join(nodeIDs, ","), kind.String()))
	}

	return b.String()
}

// kindIcon returns a text icon prefix for each node kind.
func kindIcon(k NodeKind) string {
	switch k {
	case NodeAgent:
		return "🤖"
	case NodeJudge:
		return "⚖️"
	case NodeRouter:
		return "🔀"
	case NodeJoin:
		return "🔗"
	case NodeHuman:
		return "👤"
	case NodeTool:
		return "🔧"
	case NodeDone:
		return "✅"
	case NodeFail:
		return "❌"
	default:
		return "?"
	}
}

// sanitizeID replaces characters that Mermaid cannot handle in node IDs.
func sanitizeID(id string) string {
	return strings.ReplaceAll(id, "-", "_")
}
