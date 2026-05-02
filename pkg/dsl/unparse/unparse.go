// Package unparse converts an ast.File back into .iter DSL text.
package unparse

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
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

	// --- MCP servers ---
	for _, s := range f.MCPServers {
		blankLine()
		fmt.Fprintf(&b, "mcp_server %s:\n", s.Name)
		if s.Transport != ast.MCPTransportUnknown {
			writeProp(&b, "transport", s.Transport.String())
		}
		if s.Command != "" {
			writeQuotedProp(&b, "command", s.Command)
		}
		if len(s.Args) > 0 {
			fmt.Fprintf(&b, "  args: [%s]\n", quoteList(s.Args))
		}
		if s.URL != "" {
			writeQuotedProp(&b, "url", s.URL)
		}
		if s.Auth != nil {
			writeMCPAuthBlock(&b, s.Auth)
		}
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
		if a.MCP != nil {
			writeMCPConfigBlock(&b, a.MCP, "  ")
		}
		writeAgentFields(&b, a.Model, a.Backend, a.Input, a.Output, a.Publish,
			a.System, a.User, a.Session, a.Tools, a.ToolPolicy, a.ToolMaxSteps, a.MaxTokens, a.ReasoningEffort, a.Readonly,
			a.Interaction, a.InteractionPrompt, a.InteractionModel, a.Await)
		if a.Compaction != nil {
			writeCompaction(&b, a.Compaction, "  ", false)
		}
	}

	// --- Judges ---
	for _, j := range f.Judges {
		blankLine()
		fmt.Fprintf(&b, "judge %s:\n", j.Name)
		if j.MCP != nil {
			writeMCPConfigBlock(&b, j.MCP, "  ")
		}
		writeAgentFields(&b, j.Model, j.Backend, j.Input, j.Output, j.Publish,
			j.System, j.User, j.Session, j.Tools, j.ToolPolicy, j.ToolMaxSteps, j.MaxTokens, j.ReasoningEffort, j.Readonly,
			j.Interaction, j.InteractionPrompt, j.InteractionModel, j.Await)
		if j.Compaction != nil {
			writeCompaction(&b, j.Compaction, "  ", false)
		}
	}

	// --- Routers ---
	for _, r := range f.Routers {
		blankLine()
		fmt.Fprintf(&b, "router %s:\n", r.Name)
		writeProp(&b, "mode", r.Mode.String())
		if r.Mode == ast.RouterLLM {
			if r.Model != "" {
				writeQuotedProp(&b, "model", r.Model)
			}
			if r.Backend != "" {
				writeQuotedProp(&b, "backend", r.Backend)
			}
			if r.System != "" {
				writeProp(&b, "system", r.System)
			}
			if r.User != "" {
				writeProp(&b, "user", r.User)
			}
			if r.Multi {
				writeProp(&b, "multi", "true")
			}
			if r.ReasoningEffort != "" {
				writeProp(&b, "reasoning_effort", r.ReasoningEffort)
			}
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
		writeProp(&b, "interaction", h.Interaction.String())
		if h.InteractionPrompt != "" {
			writeProp(&b, "interaction_prompt", h.InteractionPrompt)
		}
		if h.InteractionModel != "" {
			writeQuotedProp(&b, "interaction_model", h.InteractionModel)
		}
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
		if h.Await != ast.AwaitNone {
			writeProp(&b, "await", h.Await.String())
		}
	}

	// --- Tools ---
	for _, t := range f.Tools {
		blankLine()
		fmt.Fprintf(&b, "tool %s:\n", t.Name)
		if t.Command != "" {
			writeQuotedProp(&b, "command", t.Command)
		}
		if t.Input != "" {
			writeProp(&b, "input", t.Input)
		}
		if t.Output != "" {
			writeProp(&b, "output", t.Output)
		}
		if t.Await != ast.AwaitNone {
			writeProp(&b, "await", t.Await.String())
		}
	}

	// --- Computes ---
	for _, c := range f.Computes {
		blankLine()
		fmt.Fprintf(&b, "compute %s:\n", c.Name)
		if c.Input != "" {
			writeProp(&b, "input", c.Input)
		}
		if c.Output != "" {
			writeProp(&b, "output", c.Output)
		}
		if c.Await != ast.AwaitNone {
			writeProp(&b, "await", c.Await.String())
		}
		if len(c.Expr) > 0 {
			b.WriteString("  expr:\n")
			for _, e := range c.Expr {
				fmt.Fprintf(&b, "    %s: %q\n", e.Key, e.Expr)
			}
		}
	}

	// --- Workflows ---
	for _, w := range f.Workflows {
		blankLine()
		fmt.Fprintf(&b, "workflow %s:\n", w.Name)

		if w.Vars != nil && len(w.Vars.Fields) > 0 {
			writeVarsBlock(&b, w.Vars, "  ")
		}
		if w.MCP != nil {
			writeMCPConfigBlock(&b, w.MCP, "  ")
		}

		if w.DefaultBackend != "" {
			writeQuotedProp(&b, "default_backend", w.DefaultBackend)
		}

		if w.Interaction != nil {
			writeProp(&b, "interaction", w.Interaction.String())
		}

		if len(w.ToolPolicy) > 0 {
			fmt.Fprintf(&b, "  tool_policy: [%s]\n", strings.Join(w.ToolPolicy, ", "))
		}

		if w.Worktree != "" {
			writeProp(&b, "worktree", w.Worktree)
		}

		if w.Entry != "" {
			b.WriteString("\n")
			fmt.Fprintf(&b, "  entry: %s\n", w.Entry)
		}

		if w.Budget != nil {
			writeBudget(&b, w.Budget)
		}

		if w.Compaction != nil {
			writeCompaction(&b, w.Compaction, "  ", true)
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

func writeMCPAuthBlock(b *strings.Builder, auth *ast.MCPAuthDecl) {
	b.WriteString("  auth:\n")
	if auth.Type != "" {
		fmt.Fprintf(b, "    type: %q\n", auth.Type)
	}
	if auth.AuthURL != "" {
		fmt.Fprintf(b, "    auth_url: %q\n", auth.AuthURL)
	}
	if auth.TokenURL != "" {
		fmt.Fprintf(b, "    token_url: %q\n", auth.TokenURL)
	}
	if auth.RevokeURL != "" {
		fmt.Fprintf(b, "    revoke_url: %q\n", auth.RevokeURL)
	}
	if auth.ClientID != "" {
		fmt.Fprintf(b, "    client_id: %q\n", auth.ClientID)
	}
	if len(auth.Scopes) > 0 {
		fmt.Fprintf(b, "    scopes: [%s]\n", quoteList(auth.Scopes))
	}
}

func writeMCPConfigBlock(b *strings.Builder, cfg *ast.MCPConfigDecl, indent string) {
	fmt.Fprintf(b, "%smcp:\n", indent)
	if cfg.AutoloadProject != nil {
		fmt.Fprintf(b, "%s  autoload_project: %t\n", indent, *cfg.AutoloadProject)
	}
	if cfg.Inherit != nil {
		fmt.Fprintf(b, "%s  inherit: %t\n", indent, *cfg.Inherit)
	}
	if len(cfg.Servers) > 0 {
		fmt.Fprintf(b, "%s  servers: [%s]\n", indent, strings.Join(cfg.Servers, ", "))
	}
	if len(cfg.Disable) > 0 {
		fmt.Fprintf(b, "%s  disable: [%s]\n", indent, strings.Join(cfg.Disable, ", "))
	}
}

func quoteList(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}

func writeAgentFields(b *strings.Builder, model, backend, input, output, publish, system, user string, session ast.SessionMode, tools []string, toolPolicy []string, toolMaxSteps int, maxTokens int, reasoningEffort string, readonly bool, interaction ast.InteractionMode, interactionPrompt, interactionModel string, await ast.AwaitMode) {
	if model != "" {
		writeQuotedProp(b, "model", model)
	}
	if backend != "" {
		writeQuotedProp(b, "backend", backend)
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
	if len(toolPolicy) > 0 {
		fmt.Fprintf(b, "  tool_policy: [%s]\n", strings.Join(toolPolicy, ", "))
	}
	if toolMaxSteps > 0 {
		fmt.Fprintf(b, "  tool_max_steps: %d\n", toolMaxSteps)
	}
	if maxTokens > 0 {
		fmt.Fprintf(b, "  max_tokens: %d\n", maxTokens)
	}
	if reasoningEffort != "" {
		writeProp(b, "reasoning_effort", reasoningEffort)
	}
	if readonly {
		writeProp(b, "readonly", "true")
	}
	if interaction != ast.InteractionNone {
		writeProp(b, "interaction", interaction.String())
	}
	if interactionPrompt != "" {
		writeProp(b, "interaction_prompt", interactionPrompt)
	}
	if interactionModel != "" {
		writeQuotedProp(b, "interaction_model", interactionModel)
	}
	if await != ast.AwaitNone {
		writeProp(b, "await", await.String())
	}
}

func writeCompaction(b *strings.Builder, compaction *ast.CompactionBlock, indent string, leadingBlank bool) {
	if leadingBlank {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "%scompaction:\n", indent)
	if compaction.Threshold != nil {
		fmt.Fprintf(b, "%s  threshold: %g\n", indent, *compaction.Threshold)
	}
	if compaction.PreserveRecent != nil {
		fmt.Fprintf(b, "%s  preserve_recent: %d\n", indent, *compaction.PreserveRecent)
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
		if e.When.Expr != "" {
			fmt.Fprintf(b, " when %q", e.When.Expr)
		} else {
			b.WriteString(" when ")
			if e.When.Negated {
				b.WriteString("not ")
			}
			b.WriteString(e.When.Condition)
		}
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
