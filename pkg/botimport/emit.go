package botimport

import (
	"fmt"
	"strings"
)

// emit renders the lowered model as .bot source, IMPORT REPORT first.
func emit(m *model) string {
	var b strings.Builder

	b.WriteString(m.Report.header())
	b.WriteString("\n")
	if m.Description != "" {
		for _, l := range strings.Split(m.Description, "\n") {
			b.WriteString("## " + l + "\n")
		}
	}
	for _, c := range m.HeaderComments {
		b.WriteString(c + "\n")
	}
	b.WriteString("\n")

	if len(m.Vars) > 0 {
		b.WriteString("vars:\n")
		for _, v := range m.Vars {
			fmt.Fprintf(&b, "  %s: string = \"\"   ## %s\n", v.name, v.comment)
		}
		b.WriteString("\n")
	}

	for _, sc := range m.Schemas {
		fmt.Fprintf(&b, "schema %s:\n", sc.name)
		if len(sc.fields) == 0 {
			b.WriteString("  result: json   ## IMPORT: source schema had no properties\n")
		}
		for _, f := range sc.fields {
			fmt.Fprintf(&b, "  %s: %s", f.name, f.typ)
			if len(f.enum) > 0 {
				b.WriteString(" [enum: " + quoteList(f.enum) + "]")
			}
			if f.desc != "" {
				b.WriteString("   ## " + oneLine(f.desc))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	for _, p := range m.Prompts {
		fmt.Fprintf(&b, "prompt %s:\n", p.name)
		text := p.text
		if strings.TrimSpace(text) == "" {
			text = "TODO: the source prompt was fully dynamic — see the IMPORT REPORT above."
		}
		for _, l := range strings.Split(text, "\n") {
			if strings.TrimSpace(l) == "" {
				b.WriteString("\n")
			} else {
				b.WriteString("  " + strings.TrimRight(l, " \t") + "\n")
			}
		}
		b.WriteString("\n")
	}

	for _, n := range m.Nodes {
		for _, c := range n.comments {
			b.WriteString(c + "\n")
		}
		switch n.kind {
		case "agent":
			fmt.Fprintf(&b, "agent %s:\n", n.id)
			if n.model != "" {
				fmt.Fprintf(&b, "  model: %q\n", n.model)
			}
			if n.effort != "" {
				fmt.Fprintf(&b, "  reasoning_effort: %s\n", n.effort)
			}
			fmt.Fprintf(&b, "  user: %s\n", n.userPrompt)
			if n.output != "" {
				fmt.Fprintf(&b, "  output: %s\n", n.output)
			}
			if n.awaitAll {
				b.WriteString("  await: wait_all\n")
			}
		case "router":
			fmt.Fprintf(&b, "router %s:\n", n.id)
			fmt.Fprintf(&b, "  mode: %s\n", n.routerMode)
			if n.over != "" {
				fmt.Fprintf(&b, "  over: %q\n", n.over)
			}
			if n.alias != "" {
				fmt.Fprintf(&b, "  as: %s\n", n.alias)
			}
		case "tool":
			fmt.Fprintf(&b, "tool %s:\n", n.id)
			fmt.Fprintf(&b, "  command: %q\n", n.command)
			if n.awaitAll {
				b.WriteString("  await: wait_all\n")
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "workflow %s:\n", m.WorkflowName)
	if m.Entry != "" {
		fmt.Fprintf(&b, "  entry: %s\n", m.Entry)
	}
	for _, e := range m.Edges {
		for _, c := range e.comments {
			b.WriteString("  " + c + "\n")
		}
		fmt.Fprintf(&b, "  %s -> %s", e.src, e.dst)
		if e.when != "" {
			if e.whenBare {
				b.WriteString(" when " + e.when)
			} else {
				fmt.Fprintf(&b, " when %q", e.when)
			}
		}
		if e.isElse {
			b.WriteString(" else")
		}
		if e.loopName != "" {
			if e.loopUnbounded {
				fmt.Fprintf(&b, " as %s(unbounded %d)", e.loopName, e.loopFuel)
			} else {
				fmt.Fprintf(&b, " as %s(%d)", e.loopName, e.loopN)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func quoteList(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
