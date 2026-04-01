// Package unparse converts an ast.File back into .iter DSL text.
package unparse

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/ast"
)

// Unparse renders an ast.File back to .iter DSL source text.
func Unparse(f *ast.File) string {
	var b strings.Builder
	needBlank := false

	blankLine := func() {
		if needBlank {
			b.WriteByte('\n')
		}
		needBlank = true
	}

	// --- Comments ---
	for _, c := range f.Comments {
		blankLine()
		needBlank = false // comments don't need blank line between them
		b.WriteString("## ")
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}

	// --- Vars ---
	if f.Vars != nil && len(f.Vars.Fields) > 0 {
		blankLine()
		writeVarsBlock(&b, f.Vars, "")
	}

	// --- Prompts ---
	for _, p := range f.Prompts {
		blankLine()
		fmt.Fprintf(&b, "prompt %s:\n", p.Name)
		for _, line := range strings.Split(p.Body, "\n") {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	// --- Schemas ---
	for _, s := range f.Schemas {
		blankLine()
		fmt.Fprintf(&b, "schema %s:\n", s.Name)
		for _, field := range s.Fields {
			b.WriteString("  ")
			b.WriteString(field.Name)
			b.WriteString(": ")
			b.WriteString(field.Type.String())
			if len(field.EnumValues) > 0 {
				b.WriteString(" [enum: ")
				for i, v := range field.EnumValues {
					if i > 0 {
						b.WriteString(", ")
					}
					fmt.Fprintf(&b, "%q", v)
				}
				b.WriteByte(']')
			}
			b.WriteByte('\n')
		}
	}

	// --- Agents ---
	for _, a := range f.Agents {
		blankLine()
		fmt.Fprintf(&b, "agent %s:\n", a.Name)
		writeAgentFields(&b, a.Model, a.Delegate, a.Input, a.Output, a.Publish,
			a.System, a.User, a.Session, a.Tools, a.ToolMaxSteps)
	}

	// --- Judges ---
	for _, j := range f.Judges {
		blankLine()
		fmt.Fprintf(&b, "judge %s:\n", j.Name)
		writeAgentFields(&b, j.Model, j.Delegate, j.Input, j.Output, j.Publish,
			j.System, j.User, j.Session, j.Tools, j.ToolMaxSteps)
	}

	// --- Routers ---
	for _, r := range f.Routers {
		blankLine()
		fmt.Fprintf(&b, "router %s:\n", r.Name)
		writeProp(&b, "mode", r.Mode.String())
	}

	// --- Joins ---
	for _, j := range f.Joins {
		blankLine()
		fmt.Fprintf(&b, "join %s:\n", j.Name)
		writeProp(&b, "strategy", j.Strategy.String())
		if len(j.Require) > 0 {
			fmt.Fprintf(&b, "  require: [%s]\n", strings.Join(j.Require, ", "))
		}
		if j.Output != "" {
			writeProp(&b, "output", j.Output)
		}
	}

	// --- Humans ---
	for _, h := range f.Humans {
		blankLine()
		fmt.Fprintf(&b, "human %s:\n", h.Name)
		if h.Input != "" {
			writeProp(&b, "input", h.Input)
		}
		if h.Output != "" {
			writeProp(&b, "output", h.Output)
		}
		if h.Publish != "" {
			writeProp(&b, "publish", h.Publish)
		}
		writeProp(&b, "mode", h.Mode.String())
		if h.Instructions != "" {
			writeProp(&b, "instructions", h.Instructions)
		}
		if h.MinAnswers > 0 {
			fmt.Fprintf(&b, "  min_answers: %d\n", h.MinAnswers)
		}
		if h.Model != "" {
			writeQuotedProp(&b, "model", h.Model)
		}
		if h.System != "" {
			writeProp(&b, "system", h.System)
		}
	}

	// --- Tools ---
	for _, t := range f.Tools {
		blankLine()
		fmt.Fprintf(&b, "tool %s:\n", t.Name)
		if t.Command != "" {
			writeQuotedProp(&b, "command", t.Command)
		}
		if t.Output != "" {
			writeProp(&b, "output", t.Output)
		}
	}

	// --- Workflows ---
	for _, w := range f.Workflows {
		blankLine()
		fmt.Fprintf(&b, "workflow %s:\n", w.Name)

		if w.Vars != nil && len(w.Vars.Fields) > 0 {
			writeVarsBlock(&b, w.Vars, "  ")
		}

		if w.Entry != "" {
			b.WriteString("\n")
			fmt.Fprintf(&b, "  entry: %s\n", w.Entry)
		}

		if w.Budget != nil {
			writeBudget(&b, w.Budget)
		}

		for _, e := range w.Edges {
			b.WriteByte('\n')
			writeEdge(&b, e)
		}
	}

	return b.String()
}

func writeProp(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "  %s: %s\n", key, value)
}

func writeQuotedProp(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "  %s: %q\n", key, value)
}

func writeVarsBlock(b *strings.Builder, vars *ast.VarsBlock, indent string) {
	fmt.Fprintf(b, "%svars:\n", indent)
	for _, v := range vars.Fields {
		b.WriteString(indent)
		b.WriteString("  ")
		b.WriteString(v.Name)
		b.WriteString(": ")
		b.WriteString(v.Type.String())
		if v.Default != nil {
			b.WriteString(" = ")
			writeLiteral(b, v.Default)
		}
		b.WriteByte('\n')
	}
}

func writeLiteral(b *strings.Builder, lit *ast.Literal) {
	switch lit.Kind {
	case ast.LitString:
		fmt.Fprintf(b, "%q", lit.StrVal)
	case ast.LitInt:
		fmt.Fprintf(b, "%d", lit.IntVal)
	case ast.LitFloat:
		fmt.Fprintf(b, "%g", lit.FloatVal)
	case ast.LitBool:
		if lit.BoolVal {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	default:
		b.WriteString(lit.Raw)
	}
}

func writeAgentFields(b *strings.Builder, model, delegate, input, output, publish, system, user string, session ast.SessionMode, tools []string, toolMaxSteps int) {
	if model != "" {
		writeQuotedProp(b, "model", model)
	}
	if delegate != "" {
		writeQuotedProp(b, "delegate", delegate)
	}
	if input != "" {
		writeProp(b, "input", input)
	}
	if output != "" {
		writeProp(b, "output", output)
	}
	if publish != "" {
		writeProp(b, "publish", publish)
	}
	if system != "" {
		writeProp(b, "system", system)
	}
	if user != "" {
		writeProp(b, "user", user)
	}
	// Only emit session if non-default (fresh is the default/zero value)
	if session != ast.SessionFresh {
		writeProp(b, "session", session.String())
	} else {
		// Emit it always since the reference shows it explicitly
		writeProp(b, "session", session.String())
	}
	if len(tools) > 0 {
		fmt.Fprintf(b, "  tools: [%s]\n", strings.Join(tools, ", "))
	}
	if toolMaxSteps > 0 {
		fmt.Fprintf(b, "  tool_max_steps: %d\n", toolMaxSteps)
	}
}

func writeBudget(b *strings.Builder, budget *ast.BudgetBlock) {
	b.WriteString("\n  budget:\n")
	if budget.MaxParallelBranches > 0 {
		fmt.Fprintf(b, "    max_parallel_branches: %d\n", budget.MaxParallelBranches)
	}
	if budget.MaxDuration != "" {
		fmt.Fprintf(b, "    max_duration: %q\n", budget.MaxDuration)
	}
	if budget.MaxCostUSD > 0 {
		fmt.Fprintf(b, "    max_cost_usd: %g\n", budget.MaxCostUSD)
	}
	if budget.MaxTokens > 0 {
		fmt.Fprintf(b, "    max_tokens: %d\n", budget.MaxTokens)
	}
	if budget.MaxIterations > 0 {
		fmt.Fprintf(b, "    max_iterations: %d\n", budget.MaxIterations)
	}
}

func writeEdge(b *strings.Builder, e *ast.Edge) {
	fmt.Fprintf(b, "  %s -> %s", e.From, e.To)
	if e.When != nil {
		b.WriteString(" when ")
		if e.When.Negated {
			b.WriteString("not ")
		}
		b.WriteString(e.When.Condition)
	}
	if e.Loop != nil {
		fmt.Fprintf(b, " as %s(%d)", e.Loop.Name, e.Loop.MaxIterations)
	}
	if len(e.With) > 0 {
		if len(e.With) == 1 {
			fmt.Fprintf(b, " with {\n")
			fmt.Fprintf(b, "    %s: %q\n", e.With[0].Key, e.With[0].Value)
			b.WriteString("  }")
		} else {
			b.WriteString(" with {\n")
			for _, w := range e.With {
				fmt.Fprintf(b, "    %s: %q", w.Key, w.Value)
				b.WriteByte(',')
				b.WriteByte('\n')
			}
			b.WriteString("  }")
		}
	}
	b.WriteByte('\n')
}
