// Package unparse converts an ast.File back into .iter DSL text.
package unparse

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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

	// --- Presets ---
	if f.Presets != nil && len(f.Presets.Entries) > 0 {
		blankLine()
		writePresetsBlock(&b, f.Presets, "")
	}

	// --- Attachments ---
	if f.Attachments != nil && len(f.Attachments.Fields) > 0 {
		blankLine()
		writeAttachmentsBlock(&b, f.Attachments, "")
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
		// Trim trailing newlines so a body ending in "\n" (the standard
		// text-block shape) doesn't unparse into a trailing indented
		// blank line that the lexer would re-read as an extra prompt
		// line, breaking parse → unparse → re-parse round-trip stability.
		body := strings.TrimRight(p.Body, "\n")
		for _, line := range strings.Split(body, "\n") {
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

	// --- Cursors (top-level declarations) ---
	for _, c := range f.Cursors {
		blankLine()
		writeCursorDecl(&b, c)
	}

	// --- Agents ---
	for _, a := range f.Agents {
		blankLine()
		fmt.Fprintf(&b, "agent %s:\n", a.Name)
		if a.MCP != nil {
			writeMCPConfigBlock(&b, a.MCP, "  ")
		}
		writeAgentFields(&b, a.Model, a.Backend, a.Provider, a.Input, a.Output, a.Publish,
			a.System, a.User, a.Session, a.Tools, a.ToolPolicy, a.Capabilities, a.ToolMaxSteps, a.MaxTokens, a.ReasoningEffort, a.Readonly,
			a.Interaction, a.InteractionPrompt, a.InteractionModel, a.Await)
		if a.Compaction != nil {
			writeCompaction(&b, a.Compaction, "  ", false)
		}
		if a.Memory != nil {
			writeMemory(&b, a.Memory, "  ", false)
		}
		writeSandboxBlock(&b, a.Sandbox, "  ")
		if a.Cursors != nil {
			writeCursorsBlock(&b, a.Cursors, "  ")
		}
	}

	// --- Judges ---
	for _, j := range f.Judges {
		blankLine()
		fmt.Fprintf(&b, "judge %s:\n", j.Name)
		if j.MCP != nil {
			writeMCPConfigBlock(&b, j.MCP, "  ")
		}
		writeAgentFields(&b, j.Model, j.Backend, j.Provider, j.Input, j.Output, j.Publish,
			j.System, j.User, j.Session, j.Tools, j.ToolPolicy, j.Capabilities, j.ToolMaxSteps, j.MaxTokens, j.ReasoningEffort, j.Readonly,
			j.Interaction, j.InteractionPrompt, j.InteractionModel, j.Await)
		if j.Compaction != nil {
			writeCompaction(&b, j.Compaction, "  ", false)
		}
		if j.Memory != nil {
			writeMemory(&b, j.Memory, "  ", false)
		}
		writeSandboxBlock(&b, j.Sandbox, "  ")
		if j.Cursors != nil {
			writeCursorsBlock(&b, j.Cursors, "  ")
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
			if r.Provider != "" {
				writeQuotedProp(&b, "provider", r.Provider)
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
				writeReasoningEffortProp(&b, r.ReasoningEffort)
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
		// Skip when it matches the implicit Human default. Emitting it
		// unconditionally introduced parse → unparse → re-parse noise
		// (every authored human node gained a synthetic
		// `interaction: human` line), and mirrors the same skip-if-default
		// guard already applied to `session:` further down.
		if h.Interaction != ast.InteractionHuman {
			writeProp(&b, "interaction", h.Interaction.String())
		}
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
		if t.Script != "" {
			writeQuotedProp(&b, "script", t.Script)
		}
		if t.Language != "" {
			writeProp(&b, "language", t.Language)
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
		writeSandboxBlock(&b, t.Sandbox, "  ")
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
		if w.Attachments != nil && len(w.Attachments.Fields) > 0 {
			writeAttachmentsBlock(&b, w.Attachments, "  ")
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

		if len(w.Capabilities) > 0 {
			fmt.Fprintf(&b, "  capabilities: [%s]\n", strings.Join(w.Capabilities, ", "))
		}

		if w.Worktree != "" {
			writeProp(&b, "worktree", w.Worktree)
		}

		writeSandboxBlock(&b, w.Sandbox, "  ")

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

// writeIdentProp emits an identifier-shaped property (input, output,
// publish, system, user, …). Iterion's grammar requires these to be
// bare identifiers — but the AST is also constructed programmatically
// (JSON round-trip, refactoring tools) where nothing forbids stuffing
// a space or punctuation into the field. If we wrote those values
// unquoted via writeProp, the round-trip Unparse → Parse would fail
// with a cryptic lexer error far away from the offending field. Quote
// the fallback so the malformed value at least round-trips into a
// TokenString the parser can complain about precisely.
func writeIdentProp(b *strings.Builder, key, value string) {
	if isBareIdent(value) {
		writeProp(b, key, value)
		return
	}
	writeQuotedProp(b, key, value)
}

func isBareIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// writeReasoningEffortProp emits a reasoning_effort field. Bare enum
// values are written unquoted; env-substituted forms ("${VAR:-max}")
// are quoted so the parser routes them through the TokenString branch
// on a re-parse.
func writeReasoningEffortProp(b *strings.Builder, value string) {
	if ir.IsEnvSubstitutedEffort(value) {
		writeQuotedProp(b, "reasoning_effort", value)
		return
	}
	writeProp(b, "reasoning_effort", value)
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

func writePresetsBlock(b *strings.Builder, pb *ast.PresetsBlock, indent string) {
	fmt.Fprintf(b, "%spresets:\n", indent)
	// Sort preset names alphabetically for deterministic output.
	names := make([]string, 0, len(pb.Entries))
	byName := make(map[string]*ast.Preset, len(pb.Entries))
	for _, e := range pb.Entries {
		names = append(names, e.Name)
		byName[e.Name] = e
	}
	sort.Strings(names)
	for _, name := range names {
		e := byName[name]
		fmt.Fprintf(b, "%s  %s:\n", indent, e.Name)
		for _, pv := range e.Values {
			fmt.Fprintf(b, "%s    %s: ", indent, pv.Key)
			if pv.Value != nil {
				writeLiteral(b, pv.Value)
			}
			b.WriteByte('\n')
		}
	}
}

func writeAttachmentsBlock(b *strings.Builder, ab *ast.AttachmentsBlock, indent string) {
	fmt.Fprintf(b, "%sattachments:\n", indent)
	for _, f := range ab.Fields {
		// Short form when no extra props are set.
		hasProps := f.Description != "" || len(f.AcceptMIME) > 0 || f.Required != nil
		b.WriteString(indent)
		b.WriteString("  ")
		b.WriteString(f.Name)
		b.WriteString(": ")
		b.WriteString(f.Type.String())
		b.WriteByte('\n')
		if !hasProps {
			continue
		}
		// Block form sub-properties (4-space indent under the field).
		if f.Description != "" {
			fmt.Fprintf(b, "%s    description: %q\n", indent, f.Description)
		}
		if len(f.AcceptMIME) > 0 {
			fmt.Fprintf(b, "%s    accept_mime: [%s]\n", indent, quoteList(f.AcceptMIME))
		}
		if f.Required != nil {
			fmt.Fprintf(b, "%s    required: %t\n", indent, *f.Required)
		}
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

func writeAgentFields(b *strings.Builder, model, backend, provider, input, output, publish, system, user string, session ast.SessionMode, tools []string, toolPolicy []string, capabilities []string, toolMaxSteps int, maxTokens int, reasoningEffort string, readonly bool, interaction ast.InteractionMode, interactionPrompt, interactionModel string, await ast.AwaitMode) {
	if model != "" {
		writeQuotedProp(b, "model", model)
	}
	if backend != "" {
		writeQuotedProp(b, "backend", backend)
	}
	if provider != "" {
		writeQuotedProp(b, "provider", provider)
	}
	if input != "" {
		writeIdentProp(b, "input", input)
	}
	if output != "" {
		writeIdentProp(b, "output", output)
	}
	if publish != "" {
		writeIdentProp(b, "publish", publish)
	}
	if system != "" {
		writeIdentProp(b, "system", system)
	}
	if user != "" {
		writeIdentProp(b, "user", user)
	}
	// Only emit session: when it's non-default. The previous if/else
	// emitted it unconditionally — both branches called the same
	// writeProp — which broke parse → unparse → re-parse round-trip
	// stability (every agent/judge would gain a synthetic
	// `session: fresh` line that wasn't in the source).
	if session != ast.SessionFresh {
		writeProp(b, "session", session.String())
	}
	if len(tools) > 0 {
		fmt.Fprintf(b, "  tools: [%s]\n", strings.Join(tools, ", "))
	}
	if len(toolPolicy) > 0 {
		fmt.Fprintf(b, "  tool_policy: [%s]\n", strings.Join(toolPolicy, ", "))
	}
	if len(capabilities) > 0 {
		fmt.Fprintf(b, "  capabilities: [%s]\n", strings.Join(capabilities, ", "))
	}
	if toolMaxSteps > 0 {
		fmt.Fprintf(b, "  tool_max_steps: %d\n", toolMaxSteps)
	}
	if maxTokens > 0 {
		fmt.Fprintf(b, "  max_tokens: %d\n", maxTokens)
	}
	if reasoningEffort != "" {
		writeReasoningEffortProp(b, reasoningEffort)
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

// writeSandboxBlock serializes an [ast.SandboxBlock] back to its
// canonical .iter source. Empty / nil blocks emit nothing. The short
// form (`sandbox: ident`) is used when only Mode is set; otherwise
// the full block form is rendered with each populated field on its
// own line.
//
// Round-trip stability: parser → IR → unparse → parser must produce
// the same AST. Tests in pkg/dsl/unparse/unparse_test.go and
// pkg/dsl/ir/sandbox_test.go pin the contract.
func writeSandboxBlock(b *strings.Builder, sb *ast.SandboxBlock, indent string) {
	if sb == nil {
		return
	}
	if sandboxBlockIsShort(sb) {
		// Short form — Mode-only.
		if sb.Mode != "" {
			fmt.Fprintf(b, "%ssandbox: %s\n", indent, sb.Mode)
		}
		return
	}
	fmt.Fprintf(b, "%ssandbox:\n", indent)
	inner := indent + "  "
	if sb.Mode != "" && sb.Mode != "inline" {
		fmt.Fprintf(b, "%smode: %s\n", inner, sb.Mode)
	}
	if sb.Image != "" {
		fmt.Fprintf(b, "%simage: %q\n", inner, sb.Image)
	}
	if sb.User != "" {
		fmt.Fprintf(b, "%suser: %q\n", inner, sb.User)
	}
	if sb.WorkspaceFolder != "" {
		fmt.Fprintf(b, "%sworkspace_folder: %q\n", inner, sb.WorkspaceFolder)
	}
	if sb.HostState != "" {
		fmt.Fprintf(b, "%shost_state: %s\n", inner, sb.HostState)
	}
	if sb.PostCreate != "" {
		fmt.Fprintf(b, "%spost_create: %q\n", inner, sb.PostCreate)
	}
	if len(sb.Env) > 0 {
		fmt.Fprintf(b, "%senv:\n", inner)
		for _, k := range slices.Sorted(maps.Keys(sb.Env)) {
			fmt.Fprintf(b, "%s  %s: %q\n", inner, k, sb.Env[k])
		}
	}
	if len(sb.Mounts) > 0 {
		fmt.Fprintf(b, "%smounts: [", inner)
		for i, m := range sb.Mounts {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(b, "%q", m)
		}
		b.WriteString("]\n")
	}
	if sb.Build != nil {
		writeSandboxBuildBlock(b, sb.Build, inner)
	}
	if sb.Network != nil {
		writeSandboxNetworkBlock(b, sb.Network, inner)
	}
}

// sandboxBlockIsShort reports whether the block can be unparsed as
// the single-line `sandbox: <mode>` form. True when Mode is set and
// no body fields are populated.
func sandboxBlockIsShort(sb *ast.SandboxBlock) bool {
	if sb == nil {
		return false
	}
	if sb.Image != "" || sb.User != "" || sb.WorkspaceFolder != "" || sb.PostCreate != "" {
		return false
	}
	if len(sb.Env) > 0 || len(sb.Mounts) > 0 {
		return false
	}
	if sb.Network != nil || sb.Build != nil {
		return false
	}
	return true
}

func writeSandboxBuildBlock(b *strings.Builder, bb *ast.SandboxBuildBlock, indent string) {
	fmt.Fprintf(b, "%sbuild:\n", indent)
	inner := indent + "  "
	if bb.Dockerfile != "" {
		fmt.Fprintf(b, "%sdockerfile: %q\n", inner, bb.Dockerfile)
	}
	if bb.Context != "" {
		fmt.Fprintf(b, "%scontext: %q\n", inner, bb.Context)
	}
	if len(bb.Args) > 0 {
		fmt.Fprintf(b, "%sargs:\n", inner)
		for _, k := range slices.Sorted(maps.Keys(bb.Args)) {
			fmt.Fprintf(b, "%s  %s: %q\n", inner, k, bb.Args[k])
		}
	}
}

func writeSandboxNetworkBlock(b *strings.Builder, n *ast.SandboxNetworkBlock, indent string) {
	fmt.Fprintf(b, "%snetwork:\n", indent)
	inner := indent + "  "
	if n.Mode != "" {
		fmt.Fprintf(b, "%smode: %s\n", inner, n.Mode)
	}
	if n.Preset != "" {
		fmt.Fprintf(b, "%spreset: %s\n", inner, n.Preset)
	}
	if n.Inherit != "" {
		fmt.Fprintf(b, "%sinherit: %s\n", inner, n.Inherit)
	}
	if len(n.Rules) > 0 {
		fmt.Fprintf(b, "%srules: [", inner)
		for i, r := range n.Rules {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(b, "%q", r)
		}
		b.WriteString("]\n")
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

func writeMemory(b *strings.Builder, m *ast.MemoryBlock, indent string, leadingBlank bool) {
	if leadingBlank {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "%smemory:\n", indent)
	if m.Enabled != nil {
		fmt.Fprintf(b, "%s  enabled: %t\n", indent, *m.Enabled)
	}
	if m.Scope != nil {
		fmt.Fprintf(b, "%s  scope: %q\n", indent, *m.Scope)
	}
	if len(m.Autoload) > 0 {
		quoted := make([]string, len(m.Autoload))
		for i, s := range m.Autoload {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(b, "%s  autoload: [%s]\n", indent, strings.Join(quoted, ", "))
	}
	if m.Read != nil {
		fmt.Fprintf(b, "%s  read: %t\n", indent, *m.Read)
	}
	if m.Write != nil {
		fmt.Fprintf(b, "%s  write: %t\n", indent, *m.Write)
	}
	if m.PreCompactInject != nil {
		fmt.Fprintf(b, "%s  pre_compact_inject: %t\n", indent, *m.PreCompactInject)
	}
}

// writeCursorDecl renders a top-level `cursor NAME:` declaration.
// Values and Bands are serialized in declaration order so that
// parse → unparse → parse stays stable; the IR compiler is the place
// where reorderings happen.
func writeCursorDecl(b *strings.Builder, c *ast.CursorDecl) {
	fmt.Fprintf(b, "cursor %s:\n", c.Name)
	if c.Description != "" {
		fmt.Fprintf(b, "  description: %q\n", c.Description)
	}
	if len(c.Values) > 0 {
		b.WriteString("  values:\n")
		for _, v := range c.Values {
			fmt.Fprintf(b, "    %s: %q\n", v.Name, v.Prompt)
		}
	}
	if len(c.Bands) > 0 {
		b.WriteString("  bands:\n")
		for _, band := range c.Bands {
			fmt.Fprintf(b, "    %q: %q\n", band.Range, band.Prompt)
		}
	}
}

// writeCursorsBlock renders an agent/judge `cursors:` activation
// block. Settings preserve declaration order; only the explicit
// `enabled: false` form needs emission — the default true is the
// implicit shape the parser assumes.
func writeCursorsBlock(b *strings.Builder, cb *ast.CursorBlock, indent string) {
	fmt.Fprintf(b, "%scursors:\n", indent)
	if !cb.Enabled {
		fmt.Fprintf(b, "%s  enabled: false\n", indent)
	}
	for _, s := range cb.Settings {
		if isCursorValueBareIdent(s.Value) {
			fmt.Fprintf(b, "%s  %s: %s\n", indent, s.Key, s.Value)
		} else {
			fmt.Fprintf(b, "%s  %s: %q\n", indent, s.Key, s.Value)
		}
	}
}

// isCursorValueBareIdent decides whether a setting value is safe to
// emit unquoted. Bare identifiers (enum names) and numeric forms
// (0.7, 1, .25) qualify; anything containing `${`, whitespace, or
// other punctuation goes through quoting.
func isCursorValueBareIdent(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, " \t\"${},[]:") {
		return false
	}
	// numeric forms always lex back to a number → safe unquoted
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return isBareIdent(s)
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
		if e.Loop.MaxIterationsExpr != "" {
			fmt.Fprintf(b, " as %s(%q)", e.Loop.Name, e.Loop.MaxIterationsExpr)
		} else {
			fmt.Fprintf(b, " as %s(%d)", e.Loop.Name, e.Loop.MaxIterations)
		}
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
