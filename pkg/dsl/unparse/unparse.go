// Package unparse converts an ast.File back into .bot DSL text.
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
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Unparse renders an ast.File back to .bot DSL source text.
func Unparse(f *ast.File) string {
	strict := hasStrictEscapeDirective(f.Comments)
	text, needsStrict := render(f, strict)
	if needsStrict && !strict {
		// A value no v1 form can hold (a backtick together with a quote,
		// a backslash, a newline, or any carriage return): the whole file
		// switches to strict-escape mode, where every value has a quoted
		// form.
		strict = true
		text, _ = render(f, true)
	}
	if strict {
		// The lexer reads the directive from the file's first 32 lines,
		// before the first line of code. It goes on line 1 whatever
		// comment it came from — render skipped its copies in the comment
		// list — or a directive at comment #35 would be written strict and
		// read v1.
		text = "## " + strictEscapeDirective + "\n" + text
	}
	return text
}

// strictEscapeDirective is the leading comment that opts a file into
// standard escape interpretation (pkg/dsl/parser detectStrictEscape).
const strictEscapeDirective = "strict-escape: on"

// hasStrictEscapeDirective mirrors the lexer's recognition of the directive
// among the file's comments. Unparse writes every comment at the top, so a
// directive anywhere in f.Comments is a leading one in the output — the mode
// the OUTPUT is read in is what the quoting has to match.
func hasStrictEscapeDirective(comments []*ast.Comment) bool {
	for _, c := range comments {
		if isStrictEscapeDirective(c.Text) {
			return true
		}
	}
	return false
}

// isStrictEscapeDirective accepts the forms the lexer accepts.
func isStrictEscapeDirective(text string) bool {
	switch strings.TrimSpace(text) {
	case "strict-escape: on", "strict-escape:on", "strict-escape = on":
		return true
	}
	return false
}

// render writes f in one quoting mode and reports whether a value needed the
// strict one. In strict mode the directive's own comment lines are skipped:
// Unparse writes the directive on line 1.
func render(f *ast.File, strict bool) (string, bool) {
	w := &fileWriter{b: buf{strict: strict}, skipDirective: strict}
	w.writeFile(f)
	return w.b.String(), w.b.needsStrict
}

func (w *fileWriter) writeFile(f *ast.File) {
	w.writeComments(f.Comments)
	w.writeVars(f.Vars)
	w.writePresets(f.Presets)
	w.writeAttachments(f.Attachments)
	w.writeSecrets(f.Secrets)
	w.writeMCPServers(f.MCPServers)
	w.writePrompts(f.Prompts)
	w.writeSchemas(f.Schemas)
	w.writeCursors(f.Cursors)
	w.writeSupervisors(f.Supervisors)
	w.writeAgents(f.Agents)
	w.writeJudges(f.Judges)
	w.writeRouters(f.Routers)
	w.writeHumans(f.Humans)
	w.writeTools(f.Tools)
	w.writeComputes(f.Computes)
	w.writeEmits(f.Emits)
	w.writeWaits(f.Waits)
	w.writeAwaitAnswers(f.AwaitAnswers)
	w.writeFails(f.Fails)
	w.writeSubbots(f.Subbots)
	w.writeGroups(f.Groups)
	w.writeUses(f.Uses)
	w.writeWorkflows(f.Workflows)
}

// buf is the output being written, with the quoting mode every string
// value is rendered for. In v1 mode (the default) a `"…"` literal keeps
// backslashes literally and cannot hold a quote or a newline, so such
// values go to a backtick raw string; a value that also holds a backtick
// has no v1 form at all and flips needsStrict, which makes Unparse render
// the file again in strict-escape mode.
type buf struct {
	strings.Builder
	strict      bool
	needsStrict bool
	// nested is set on the writer of a group body, whose text is indented
	// after the fact: a raw string spanning lines would have its
	// continuation lines indented too, changing the value, so a value with
	// a newline needs the strict form there.
	nested bool
}

// str renders v as a string literal the lexer reads back as exactly v.
func (b *buf) str(v string) string {
	if b.strict {
		return strictQuote(v)
	}
	if !strings.ContainsAny(v, "\"\\\n\r") {
		return "\"" + v + "\""
	}
	// The lexer folds CRLF to LF before it reads anything, so no v1 form
	// carries a carriage return; only the strict escape does.
	multiLine := strings.ContainsAny(v, "\n\r")
	if !strings.Contains(v, "`") && !strings.Contains(v, "\r") && (!b.nested || !multiLine) {
		return "`" + v + "`"
	}
	b.needsStrict = true
	return strconv.Quote(v) // discarded: the file is rendered again in strict mode
}

// strictQuote is the `"…"` form under `## strict-escape: on`: the escapes
// the lexer decodes are \\ \" \n \t \r \0; every other byte is copied
// as is, so only those need escaping.
func strictQuote(v string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString("\\\\")
		case '"':
			sb.WriteString("\\\"")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case 0:
			sb.WriteString("\\0")
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// writeGroups renders `group NAME(params):` declarations: the members
// through the same writers as top-level nodes, indented one level after
// the fact, then the group's internal edges.
func (w *fileWriter) writeGroups(groups []*ast.GroupDecl) {
	for _, g := range groups {
		w.blankLine()
		if len(g.Params) > 0 {
			fmt.Fprintf(&w.b, "group %s(%s):\n", g.Name, strings.Join(g.Params, ", "))
		} else {
			fmt.Fprintf(&w.b, "group %s:\n", g.Name)
		}
		sub := &fileWriter{b: buf{strict: w.b.strict, nested: true}}
		sub.writeAgents(g.Agents)
		sub.writeJudges(g.Judges)
		sub.writeRouters(g.Routers)
		sub.writeHumans(g.Humans)
		sub.writeTools(g.Tools)
		sub.writeComputes(g.Computes)
		if sub.b.needsStrict {
			w.b.needsStrict = true
		}
		w.b.WriteString(indentBlock(sub.b.String(), "  "))
		if len(g.Edges) > 0 {
			w.b.WriteByte('\n')
		}
		for _, e := range g.Edges {
			writeEdge(&w.b, e)
		}
	}
}

// writeUses renders `use GROUP as PREFIX [with { … }]` instantiations.
func (w *fileWriter) writeUses(uses []*ast.UseDecl) {
	for _, u := range uses {
		w.blankLine()
		fmt.Fprintf(&w.b, "use %s as %s", u.Group, u.Prefix)
		if len(u.With) > 0 {
			w.b.WriteString(" with {\n")
			for _, e := range u.With {
				fmt.Fprintf(&w.b, "  %s: %s,\n", e.Key, w.b.str(e.Value))
			}
			w.b.WriteString("}")
		}
		w.b.WriteByte('\n')
	}
}

// indentBlock prefixes every non-blank line of text with indent.
func indentBlock(text, indent string) string {
	var sb strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.TrimSpace(line) != "" {
			sb.WriteString(indent)
		}
		sb.WriteString(line)
	}
	return sb.String()
}

// fileWriter accumulates Unparse output and tracks blank-line state so
// each top-level section is separated by a single blank line — matching
// the legacy inline `needBlank`/`blankLine` mechanic byte-for-byte.
type fileWriter struct {
	b         buf
	needBlank bool
	// skipDirective drops the strict-escape directive from the comment
	// list: Unparse writes it on line 1 itself.
	skipDirective bool
}

// ensureBody writes a no-op property under a declaration header that got
// no properties (a node added on the canvas and saved before it is filled
// in): a header with no indented body does not parse, and an empty
// description is what an absent one reads as, so the next save drops it.
func (w *fileWriter) ensureBody(mark int) {
	if w.b.Len() == mark {
		w.b.WriteString("  description: \"\"\n")
	}
}

// endBlock closes a block whose header was written at mark: when no
// property followed, a blank line separates the bare header from what
// comes next, which is how the parser tells an empty block from a body at
// the wrong indentation. An empty block is written, never omitted: the
// text carries the document as the author declared it, and whether an
// empty block changes the program is the compiler's call, not the
// writer's — today a `recovery:` block's presence is read by the
// verified-action checks, the others compile as their absence does.
func endBlock(b *buf, mark int) {
	if b.Len() == mark {
		b.WriteByte('\n')
	}
}

// blankLine emits a separator newline before the next section, unless
// this is the first section to write anything. Mirrors the closure that
// used to live inside Unparse — preserve the contract exactly so
// round-trip output stays byte-identical.
func (w *fileWriter) blankLine() {
	if w.needBlank {
		w.b.WriteByte('\n')
	}
	w.needBlank = true
}

func (w *fileWriter) writeComments(comments []*ast.Comment) {
	for _, c := range comments {
		if w.skipDirective && isStrictEscapeDirective(c.Text) {
			continue
		}
		w.blankLine()
		w.needBlank = false // comments don't need blank line between them
		w.b.WriteString("## ")
		w.b.WriteString(c.Text)
		w.b.WriteByte('\n')
	}
}

func (w *fileWriter) writeVars(vars *ast.VarsBlock) {
	if vars == nil {
		return
	}
	w.blankLine()
	writeVarsBlock(&w.b, vars, "")
}

func (w *fileWriter) writePresets(presets *ast.PresetsBlock) {
	if presets == nil {
		return
	}
	w.blankLine()
	writePresetsBlock(&w.b, presets, "")
}

func (w *fileWriter) writeAttachments(att *ast.AttachmentsBlock) {
	if att == nil {
		return
	}
	w.blankLine()
	writeAttachmentsBlock(&w.b, att, "")
}

func (w *fileWriter) writeSecrets(secrets *ast.SecretsBlock) {
	if secrets == nil {
		return
	}
	w.blankLine()
	writeSecretsBlock(&w.b, secrets, "")
}

func (w *fileWriter) writeMCPServers(servers []*ast.MCPServerDecl) {
	for _, s := range servers {
		w.blankLine()
		fmt.Fprintf(&w.b, "mcp_server %s:\n", s.Name)
		if s.Transport != ast.MCPTransportUnknown {
			writeProp(&w.b, "transport", s.Transport.String())
		}
		if s.Command != "" {
			writeQuotedProp(&w.b, "command", s.Command)
		}
		if len(s.Args) > 0 {
			fmt.Fprintf(&w.b, "  args: [%s]\n", quoteList(&w.b, s.Args))
		}
		if s.URL != "" {
			writeQuotedProp(&w.b, "url", s.URL)
		}
		if s.Auth != nil {
			writeMCPAuthBlock(&w.b, s.Auth)
		}
	}
}

func (w *fileWriter) writePrompts(prompts []*ast.PromptDecl) {
	for _, p := range prompts {
		w.blankLine()
		fmt.Fprintf(&w.b, "prompt %s:\n", p.Name)
		// The body is written in the lexer's canonical form — the only
		// form the text can carry: a blank line under a prompt header is
		// skipped when read, so writing one (or a trailing indented blank)
		// would put on disk a line that says nothing and re-reads as
		// something else than the document. A canvas body with a paragraph
		// break or an Enter after the last line lands as its canonical form,
		// which is what any reader of the file gets. A body the syntax cannot
		// carry at all (parser.CheckPromptBody) lands as its nearest form,
		// de-indented; Verify, which every production caller runs on this
		// text, refuses it by name.
		body := parser.CanonicalPromptBody(p.Body)
		if body == "" {
			// A bare header IS the empty prompt; an indented blank line
			// would be neither a body nor a valid empty form.
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			w.b.WriteString("  ")
			w.b.WriteString(line)
			w.b.WriteByte('\n')
		}
	}
}

func (w *fileWriter) writeSchemas(schemas []*ast.SchemaDecl) {
	for _, s := range schemas {
		w.blankLine()
		fmt.Fprintf(&w.b, "schema %s:\n", s.Name)
		for _, field := range s.Fields {
			w.b.WriteString("  ")
			w.b.WriteString(field.Name)
			w.b.WriteString(": ")
			w.b.WriteString(field.Type.String())
			writeEnumConstraint(&w.b, field.EnumValues)
			w.b.WriteByte('\n')
		}
	}
}

func (w *fileWriter) writeCursors(cursors []*ast.CursorDecl) {
	for _, c := range cursors {
		w.blankLine()
		writeCursorDecl(&w.b, c)
	}
}

// writeSupervisors renders top-level `supervisor NAME:` declarations.
func (w *fileWriter) writeSupervisors(supervisors []*ast.SupervisorDecl) {
	for _, s := range supervisors {
		w.blankLine()
		fmt.Fprintf(&w.b, "supervisor %s:\n", s.Name)
		if len(s.Watches) > 0 {
			fmt.Fprintf(&w.b, "  watches: [%s]\n", strings.Join(s.Watches, ", "))
		}
		if s.Model != "" {
			fmt.Fprintf(&w.b, "  model: %s\n", w.b.str(s.Model))
		}
		if s.System != "" {
			fmt.Fprintf(&w.b, "  system: %s\n", s.System)
		}
		if s.Cooldown != "" {
			fmt.Fprintf(&w.b, "  cooldown: %s\n", w.b.str(s.Cooldown))
		}
		if s.MaxEvals != 0 {
			fmt.Fprintf(&w.b, "  max_evals: %d\n", s.MaxEvals)
		}
		if len(s.Monitors) > 0 {
			quoted := make([]string, len(s.Monitors))
			for i, m := range s.Monitors {
				quoted[i] = w.b.str(m)
			}
			fmt.Fprintf(&w.b, "  monitors: [%s]\n", strings.Join(quoted, ", "))
		}
	}
}

func (w *fileWriter) writeAgents(agents []*ast.AgentDecl) {
	for _, a := range agents {
		w.blankLine()
		fmt.Fprintf(&w.b, "agent %s:\n", a.Name)
		mark := w.b.Len()
		if a.Description != "" {
			writeQuotedProp(&w.b, "description", a.Description)
		}
		if a.MCP != nil {
			writeMCPConfigBlock(&w.b, a.MCP, "  ")
		}
		writeAgentFields(&w.b, llmFields{
			Model: a.Model, Backend: a.Backend, Provider: a.Provider, Command: a.Command,
			Input: a.Input, Output: a.Output, Publish: a.Publish, ArtifactLabels: a.ArtifactLabels,
			System: a.System, User: a.User, Session: a.Session,
			Tools: a.Tools, ToolPolicy: a.ToolPolicy, Capabilities: a.Capabilities, Skills: a.Skills,
			ToolMaxSteps: a.ToolMaxSteps, MaxTokens: a.MaxTokens, ReasoningEffort: a.ReasoningEffort,
			Timeout:  a.Timeout,
			Readonly: a.Readonly, FullAccess: a.FullAccess, Images: a.Images, Interaction: a.Interaction, InteractionPrompt: a.InteractionPrompt,
			InteractionModel: a.InteractionModel, Await: a.Await,
			Compress: a.Compress, AutoMemory: a.AutoMemory, Permission: a.Permission, Needs: a.Needs,
		})
		if a.Compaction != nil {
			writeCompaction(&w.b, a.Compaction, "  ", false)
		}
		if a.Memory != nil {
			writeMemory(&w.b, a.Memory, "  ", false)
		}
		writeSandboxBlock(&w.b, a.Sandbox, "  ")
		if a.Cursors != nil {
			writeCursorsBlock(&w.b, a.Cursors, "  ")
		}
		writeFallbacksBlock(&w.b, a.Fallbacks, "  ")
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeJudges(judges []*ast.JudgeDecl) {
	for _, j := range judges {
		w.blankLine()
		fmt.Fprintf(&w.b, "judge %s:\n", j.Name)
		mark := w.b.Len()
		if j.Description != "" {
			writeQuotedProp(&w.b, "description", j.Description)
		}
		if j.MCP != nil {
			writeMCPConfigBlock(&w.b, j.MCP, "  ")
		}
		writeAgentFields(&w.b, llmFields{
			Model: j.Model, Backend: j.Backend, Provider: j.Provider, Command: j.Command,
			Input: j.Input, Output: j.Output, Publish: j.Publish, ArtifactLabels: j.ArtifactLabels,
			System: j.System, User: j.User, Session: j.Session,
			Tools: j.Tools, ToolPolicy: j.ToolPolicy, Capabilities: j.Capabilities, Skills: j.Skills,
			ToolMaxSteps: j.ToolMaxSteps, MaxTokens: j.MaxTokens, ReasoningEffort: j.ReasoningEffort,
			Timeout:  j.Timeout,
			Readonly: j.Readonly, FullAccess: j.FullAccess, Images: j.Images, Interaction: j.Interaction, InteractionPrompt: j.InteractionPrompt,
			InteractionModel: j.InteractionModel, Await: j.Await,
			Compress: j.Compress, AutoMemory: j.AutoMemory, Permission: j.Permission, Needs: j.Needs,
		})
		if j.Compaction != nil {
			writeCompaction(&w.b, j.Compaction, "  ", false)
		}
		if j.Memory != nil {
			writeMemory(&w.b, j.Memory, "  ", false)
		}
		writeSandboxBlock(&w.b, j.Sandbox, "  ")
		if j.Cursors != nil {
			writeCursorsBlock(&w.b, j.Cursors, "  ")
		}
		writeFallbacksBlock(&w.b, j.Fallbacks, "  ")
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeRouters(routers []*ast.RouterDecl) {
	for _, r := range routers {
		w.blankLine()
		fmt.Fprintf(&w.b, "router %s:\n", r.Name)
		if r.Description != "" {
			writeQuotedProp(&w.b, "description", r.Description)
		}
		writeProp(&w.b, "mode", r.Mode.String())
		if r.Mode == ast.RouterLLM {
			if r.Model != "" {
				writeQuotedProp(&w.b, "model", r.Model)
			}
			if r.Backend != "" {
				writeQuotedProp(&w.b, "backend", r.Backend)
			}
			if r.Provider != "" {
				writeQuotedProp(&w.b, "provider", r.Provider)
			}
			if r.System != "" {
				writeProp(&w.b, "system", r.System)
			}
			if r.User != "" {
				writeProp(&w.b, "user", r.User)
			}
			if r.Multi {
				writeProp(&w.b, "multi", "true")
			}
			if r.ReasoningEffort != "" {
				writeReasoningEffortProp(&w.b, r.ReasoningEffort)
			}
		}
		if r.Mode == ast.RouterFanOutEach {
			if r.Over != "" {
				writeQuotedProp(&w.b, "over", r.Over)
			}
			if r.As != "" {
				writeIdentProp(&w.b, "as", r.As)
			}
			if r.Key != "" {
				writeIdentProp(&w.b, "key", r.Key)
			}
			if r.DependsOn != "" {
				writeIdentProp(&w.b, "depends_on", r.DependsOn)
			}
		}
		if len(r.Needs) > 0 {
			fmt.Fprintf(&w.b, "  needs: [%s]\n", strings.Join(r.Needs, ", "))
		}
	}
}

func (w *fileWriter) writeHumans(humans []*ast.HumanDecl) {
	for _, h := range humans {
		w.blankLine()
		fmt.Fprintf(&w.b, "human %s:\n", h.Name)
		mark := w.b.Len()
		if h.Description != "" {
			writeQuotedProp(&w.b, "description", h.Description)
		}
		if h.Input != "" {
			writeProp(&w.b, "input", h.Input)
		}
		if h.Output != "" {
			writeProp(&w.b, "output", h.Output)
		}
		if h.Publish != "" {
			writeProp(&w.b, "publish", h.Publish)
		}
		writeArtifactLabels(&w.b, h.ArtifactLabels, "  ")
		// Skip when it matches the implicit Human default. Emitting it
		// unconditionally introduced parse → unparse → re-parse noise
		// (every authored human node gained a synthetic
		// `interaction: human` line), and mirrors the same skip-if-default
		// guard already applied to `session:` further down.
		if h.Interaction != ast.InteractionHuman {
			writeProp(&w.b, "interaction", h.Interaction.String())
		}
		if h.InteractionPrompt != "" {
			writeProp(&w.b, "interaction_prompt", h.InteractionPrompt)
		}
		if h.InteractionModel != "" {
			writeQuotedProp(&w.b, "interaction_model", h.InteractionModel)
		}
		if h.Instructions != "" {
			writeProp(&w.b, "instructions", h.Instructions)
		}
		if h.MinAnswers > 0 {
			fmt.Fprintf(&w.b, "  min_answers: %d\n", h.MinAnswers)
		}
		if h.Model != "" {
			writeQuotedProp(&w.b, "model", h.Model)
		}
		if h.System != "" {
			writeProp(&w.b, "system", h.System)
		}
		if h.ReviewURL != "" {
			writeQuotedProp(&w.b, "review_url", h.ReviewURL)
		}
		if h.Posture != "" {
			writeProp(&w.b, "posture", h.Posture)
		}
		if h.MergeStrategy != "" {
			writeProp(&w.b, "merge_strategy", h.MergeStrategy)
		}
		if h.MergeInto != "" {
			writeQuotedProp(&w.b, "merge_into", h.MergeInto)
		}
		if h.MaxTurns > 0 {
			fmt.Fprintf(&w.b, "  max_turns: %d\n", h.MaxTurns)
		}
		if h.Await != ast.AwaitNone {
			writeProp(&w.b, "await", h.Await.String())
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeTools(tools []*ast.ToolNodeDecl) {
	for _, t := range tools {
		w.blankLine()
		fmt.Fprintf(&w.b, "tool %s:\n", t.Name)
		mark := w.b.Len()
		if t.Description != "" {
			writeQuotedProp(&w.b, "description", t.Description)
		}
		if t.Command != "" {
			writeQuotedProp(&w.b, "command", t.Command)
		}
		if t.Script != "" {
			writeQuotedProp(&w.b, "script", t.Script)
		}
		if t.Language != "" {
			writeProp(&w.b, "language", t.Language)
		}
		if t.Input != "" {
			writeProp(&w.b, "input", t.Input)
		}
		if t.Output != "" {
			writeProp(&w.b, "output", t.Output)
		}
		if t.Publish != "" {
			writeProp(&w.b, "publish", t.Publish)
		}
		writeArtifactLabels(&w.b, t.ArtifactLabels, "  ")
		if t.Await != ast.AwaitNone {
			writeProp(&w.b, "await", t.Await.String())
		}
		if t.Compress != "" {
			writeProp(&w.b, "compress", t.Compress)
		}
		if t.Permission != "" {
			writeProp(&w.b, "permission", t.Permission)
		}
		if len(t.Needs) > 0 {
			fmt.Fprintf(&w.b, "  needs: [%s]\n", strings.Join(t.Needs, ", "))
		}
		// Verified Action quad (ADR-044).
		if t.Goal != "" {
			writeQuotedProp(&w.b, "goal", t.Goal)
		}
		if t.Postcondition != "" {
			writeQuotedProp(&w.b, "postcondition", t.Postcondition)
		}
		if t.Policy != "" {
			writeProp(&w.b, "policy", t.Policy)
		}
		if t.ParallelSafe {
			writeProp(&w.b, "parallel_safe", "true")
		}
		if t.Recovery != nil {
			writeRecoveryBlock(&w.b, t.Recovery, "  ")
		}
		writeSandboxBlock(&w.b, t.Sandbox, "  ")
		w.ensureBody(mark)
	}
}

// writeRecoveryBlock serialises a tool node's recovery: block (ADR-044).
func writeRecoveryBlock(b *buf, r *ast.RecoveryBlock, indent string) {
	if r == nil {
		return
	}
	fmt.Fprintf(b, "%srecovery:\n", indent)
	defer endBlock(b, b.Len())
	inner := indent + "  "
	if r.MaxRepairAttempts > 0 {
		fmt.Fprintf(b, "%smax_repair_attempts: %d\n", inner, r.MaxRepairAttempts)
	}
	if r.MaxAgentAttempts > 0 {
		fmt.Fprintf(b, "%smax_agent_attempts: %d\n", inner, r.MaxAgentAttempts)
	}
	if r.Model != "" {
		fmt.Fprintf(b, "%smodel: %s\n", inner, b.str(r.Model))
	}
	if len(r.AgentTools) > 0 {
		fmt.Fprintf(b, "%sagent_tools: [%s]\n", inner, strings.Join(r.AgentTools, ", "))
	}
}

func (w *fileWriter) writeSubbots(subbots []*ast.SubbotDecl) {
	for _, s := range subbots {
		w.blankLine()
		fmt.Fprintf(&w.b, "subbot %s:\n", s.Name)
		mark := w.b.Len()
		if s.Description != "" {
			writeQuotedProp(&w.b, "description", s.Description)
		}
		if s.Source != "" {
			writeQuotedProp(&w.b, "source", s.Source)
		}
		if len(s.With) > 0 {
			w.b.WriteString("  with {\n")
			for _, e := range s.With {
				fmt.Fprintf(&w.b, "    %s: %s,\n", e.Key, w.b.str(e.Value))
			}
			w.b.WriteString("  }\n")
		}
		if s.Output != "" {
			writeProp(&w.b, "output", s.Output)
		}
		if len(s.Needs) > 0 {
			fmt.Fprintf(&w.b, "  needs: [%s]\n", strings.Join(s.Needs, ", "))
		}
		if s.Isolated {
			writeProp(&w.b, "isolated", "true")
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeComputes(computes []*ast.ComputeDecl) {
	for _, c := range computes {
		w.blankLine()
		fmt.Fprintf(&w.b, "compute %s:\n", c.Name)
		mark := w.b.Len()
		if c.Description != "" {
			writeQuotedProp(&w.b, "description", c.Description)
		}
		if c.Input != "" {
			writeProp(&w.b, "input", c.Input)
		}
		if c.Output != "" {
			writeProp(&w.b, "output", c.Output)
		}
		if c.Publish != "" {
			writeProp(&w.b, "publish", c.Publish)
		}
		writeArtifactLabels(&w.b, c.ArtifactLabels, "  ")
		if c.Await != ast.AwaitNone {
			writeProp(&w.b, "await", c.Await.String())
		}
		if len(c.Expr) > 0 {
			w.b.WriteString("  expr:\n")
			for _, e := range c.Expr {
				fmt.Fprintf(&w.b, "    %s: %s\n", e.Key, w.b.str(e.Expr))
			}
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeEmits(emits []*ast.EmitDecl) {
	for _, e := range emits {
		w.blankLine()
		fmt.Fprintf(&w.b, "emit %s:\n", e.Name)
		mark := w.b.Len()
		if e.Description != "" {
			writeQuotedProp(&w.b, "description", e.Description)
		}
		if e.Event != "" {
			writeQuotedProp(&w.b, "event", e.Event)
		}
		if len(e.With) > 0 {
			w.b.WriteString("  with {\n")
			for _, entry := range e.With {
				fmt.Fprintf(&w.b, "    %s: %s,\n", entry.Key, w.b.str(entry.Value))
			}
			w.b.WriteString("  }\n")
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeWaits(waits []*ast.WaitDecl) {
	for _, wt := range waits {
		w.blankLine()
		fmt.Fprintf(&w.b, "wait %s:\n", wt.Name)
		mark := w.b.Len()
		if wt.Description != "" {
			writeQuotedProp(&w.b, "description", wt.Description)
		}
		if wt.Event != "" {
			writeQuotedProp(&w.b, "event", wt.Event)
		}
		if wt.Timeout != "" {
			writeQuotedProp(&w.b, "timeout", wt.Timeout)
		}
		if wt.Output != "" {
			writeProp(&w.b, "output", wt.Output)
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeAwaitAnswers(decls []*ast.AwaitAnswersDecl) {
	for _, aa := range decls {
		w.blankLine()
		fmt.Fprintf(&w.b, "await_answers %s:\n", aa.Name)
		mark := w.b.Len()
		if aa.Description != "" {
			writeQuotedProp(&w.b, "description", aa.Description)
		}
		if aa.From != "" {
			writeProp(&w.b, "from", aa.From)
		}
		if aa.Timeout != "" {
			writeQuotedProp(&w.b, "timeout", aa.Timeout)
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeFails(decls []*ast.FailDecl) {
	for _, fd := range decls {
		w.blankLine()
		fmt.Fprintf(&w.b, "fail %s:\n", fd.Name)
		mark := w.b.Len()
		if fd.Description != "" {
			writeQuotedProp(&w.b, "description", fd.Description)
		}
		if fd.Code != "" {
			writeProp(&w.b, "code", fd.Code)
		}
		if fd.Message != "" {
			writeQuotedProp(&w.b, "message", fd.Message)
		}
		if fd.Resumable {
			writeProp(&w.b, "resumable", "true")
		}
		w.ensureBody(mark)
	}
}

func (w *fileWriter) writeWorkflows(workflows []*ast.WorkflowDecl) {
	for _, wf := range workflows {
		w.blankLine()
		fmt.Fprintf(&w.b, "workflow %s:\n", wf.Name)

		if wf.Vars != nil && len(wf.Vars.Fields) > 0 {
			writeVarsBlock(&w.b, wf.Vars, "  ")
		}
		if wf.Attachments != nil && len(wf.Attachments.Fields) > 0 {
			writeAttachmentsBlock(&w.b, wf.Attachments, "  ")
		}
		if wf.MCP != nil {
			writeMCPConfigBlock(&w.b, wf.MCP, "  ")
		}

		if wf.DefaultBackend != "" {
			writeQuotedProp(&w.b, "default_backend", wf.DefaultBackend)
		}

		if wf.Interaction != nil {
			writeProp(&w.b, "interaction", wf.Interaction.String())
		}

		if len(wf.ToolPolicy) > 0 {
			fmt.Fprintf(&w.b, "  tool_policy: [%s]\n", strings.Join(wf.ToolPolicy, ", "))
		}

		if len(wf.Capabilities) > 0 {
			fmt.Fprintf(&w.b, "  capabilities: [%s]\n", strings.Join(wf.Capabilities, ", "))
		}
		if len(wf.Skills) > 0 {
			fmt.Fprintf(&w.b, "  skills: [%s]\n", quoteList(&w.b, wf.Skills))
		}

		if wf.Worktree != "" {
			writeProp(&w.b, "worktree", wf.Worktree)
		}

		if wf.Compress != "" {
			writeProp(&w.b, "compress", wf.Compress)
		}

		if wf.AutoMemory != "" {
			writeProp(&w.b, "auto_memory", wf.AutoMemory)
		}

		if wf.LoopBudgetGuard != "" {
			writeProp(&w.b, "loop_budget_guard", wf.LoopBudgetGuard)
		}

		if wf.RepoDevbox != "" {
			writeProp(&w.b, "repo_devbox", wf.RepoDevbox)
		}

		if wf.WorkspaceCheckpoint != "" {
			writeProp(&w.b, "workspace_checkpoint", wf.WorkspaceCheckpoint)
		}

		if wf.Permission != "" {
			writeProp(&w.b, "permission", wf.Permission)
		}
		if len(wf.Allow) > 0 {
			fmt.Fprintf(&w.b, "  allow: [%s]\n", quoteList(&w.b, wf.Allow))
		}
		if len(wf.Ask) > 0 {
			fmt.Fprintf(&w.b, "  ask: [%s]\n", quoteList(&w.b, wf.Ask))
		}
		if len(wf.Deny) > 0 {
			fmt.Fprintf(&w.b, "  deny: [%s]\n", quoteList(&w.b, wf.Deny))
		}

		writeSandboxBlock(&w.b, wf.Sandbox, "  ")

		if wf.Entry != "" {
			w.b.WriteString("\n")
			fmt.Fprintf(&w.b, "  entry: %s\n", wf.Entry)
		}

		if wf.Budget != nil {
			writeBudget(&w.b, wf.Budget)
		}

		if wf.Resources != nil {
			writeResources(&w.b, wf.Resources)
		}

		if wf.Compaction != nil {
			writeCompaction(&w.b, wf.Compaction, "  ", true)
		}

		for _, e := range wf.Edges {
			w.b.WriteByte('\n')
			writeEdge(&w.b, e)
		}
	}
}

func writeProp(b *buf, key, value string) {
	fmt.Fprintf(b, "  %s: %s\n", key, value)
}

func writeQuotedProp(b *buf, key, value string) {
	fmt.Fprintf(b, "  %s: %s\n", key, b.str(value))
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
func writeIdentProp(b *buf, key, value string) {
	if isBareIdent(value) {
		writeProp(b, key, value)
		return
	}
	writeQuotedProp(b, key, value)
}

// writeArtifactLabels renders `artifact_labels: [a, b]` — the labels a
// published artifact is tagged with (ADR on artifact labels); a label is
// written bare when it is an identifier and quoted otherwise, and the tool
// list parser reads a quoted element back as the literal name.
func writeArtifactLabels(b *buf, labels []string, indent string) {
	if len(labels) == 0 {
		return
	}
	items := make([]string, len(labels))
	for i, l := range labels {
		items[i] = identOrStr(b, l)
	}
	fmt.Fprintf(b, "%sartifact_labels: [%s]\n", indent, strings.Join(items, ", "))
}

// identOrStr renders v bare when it is an identifier and as a string
// literal otherwise — for properties the parser reads as either.
func identOrStr(b *buf, v string) string {
	if isBareIdent(v) {
		return v
	}
	return b.str(v)
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
func writeReasoningEffortProp(b *buf, value string) {
	if ir.IsEnvSubstitutedEffort(value) {
		writeQuotedProp(b, "reasoning_effort", value)
		return
	}
	writeProp(b, "reasoning_effort", value)
}

func writeVarsBlock(b *buf, vars *ast.VarsBlock, indent string) {
	fmt.Fprintf(b, "%svars:\n", indent)
	defer endBlock(b, b.Len())
	for _, v := range vars.Fields {
		b.WriteString(indent)
		b.WriteString("  ")
		b.WriteString(v.Name)
		b.WriteString(": ")
		b.WriteString(v.Type.String())
		writeEnumConstraint(b, v.EnumValues)
		if v.Default != nil {
			b.WriteString(" = ")
			writeLiteral(b, v.Default)
		}
		b.WriteByte('\n')
	}
}

// writeEnumConstraint emits ` [enum: "a", "b"]` after a type for schema
// fields and var declarations alike. No-op on an empty value set.
func writeEnumConstraint(b *buf, vals []string) {
	if len(vals) == 0 {
		return
	}
	b.WriteString(" [enum: ")
	for i, v := range vals {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "%s", b.str(v))
	}
	b.WriteByte(']')
}

func writeSecretsBlock(b *buf, sb *ast.SecretsBlock, indent string) {
	fmt.Fprintf(b, "%ssecrets:\n", indent)
	defer endBlock(b, b.Len())
	for _, s := range sb.Fields {
		// Short form when only a value is set; block form when egress
		// hosts, file materialisation, env wiring, or a description
		// accompany it.
		hasProps := s.As != "" || s.MountPath != "" || s.Env != "" || len(s.Hosts) > 0 || s.Description != ""
		b.WriteString(indent)
		b.WriteString("  ")
		b.WriteString(s.Name)
		if !hasProps {
			fmt.Fprintf(b, ": %s\n", b.str(s.Value))
			continue
		}
		b.WriteString(":\n")
		if s.Value != "" {
			fmt.Fprintf(b, "%s    value: %s\n", indent, b.str(s.Value))
		}
		if s.As != "" {
			fmt.Fprintf(b, "%s    as: %s\n", indent, s.As)
		}
		if s.MountPath != "" {
			fmt.Fprintf(b, "%s    mount_path: %s\n", indent, b.str(s.MountPath))
		}
		if s.Env != "" {
			fmt.Fprintf(b, "%s    env: %s\n", indent, b.str(s.Env))
		}
		if s.Optional {
			fmt.Fprintf(b, "%s    optional: true\n", indent)
		}
		if len(s.Hosts) > 0 {
			fmt.Fprintf(b, "%s    hosts: [%s]\n", indent, quoteList(b, s.Hosts))
		}
		if s.Description != "" {
			fmt.Fprintf(b, "%s    description: %s\n", indent, b.str(s.Description))
		}
	}
}

func writePresetsBlock(b *buf, pb *ast.PresetsBlock, indent string) {
	fmt.Fprintf(b, "%spresets:\n", indent)
	defer endBlock(b, b.Len())
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

func writeAttachmentsBlock(b *buf, ab *ast.AttachmentsBlock, indent string) {
	fmt.Fprintf(b, "%sattachments:\n", indent)
	defer endBlock(b, b.Len())
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
			fmt.Fprintf(b, "%s    description: %s\n", indent, b.str(f.Description))
		}
		if len(f.AcceptMIME) > 0 {
			fmt.Fprintf(b, "%s    accept_mime: [%s]\n", indent, quoteList(b, f.AcceptMIME))
		}
		if f.Required != nil {
			fmt.Fprintf(b, "%s    required: %t\n", indent, *f.Required)
		}
	}
}

func writeLiteral(b *buf, lit *ast.Literal) {
	switch lit.Kind {
	case ast.LitString:
		fmt.Fprintf(b, "%s", b.str(lit.StrVal))
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

func writeMCPAuthBlock(b *buf, auth *ast.MCPAuthDecl) {
	b.WriteString("  auth:\n")
	defer endBlock(b, b.Len())
	if auth.Type != "" {
		fmt.Fprintf(b, "    type: %s\n", b.str(auth.Type))
	}
	if auth.AuthURL != "" {
		fmt.Fprintf(b, "    auth_url: %s\n", b.str(auth.AuthURL))
	}
	if auth.TokenURL != "" {
		fmt.Fprintf(b, "    token_url: %s\n", b.str(auth.TokenURL))
	}
	if auth.RevokeURL != "" {
		fmt.Fprintf(b, "    revoke_url: %s\n", b.str(auth.RevokeURL))
	}
	if auth.ClientID != "" {
		fmt.Fprintf(b, "    client_id: %s\n", b.str(auth.ClientID))
	}
	if len(auth.Scopes) > 0 {
		fmt.Fprintf(b, "    scopes: [%s]\n", quoteList(b, auth.Scopes))
	}
}

func writeMCPConfigBlock(b *buf, cfg *ast.MCPConfigDecl, indent string) {
	fmt.Fprintf(b, "%smcp:\n", indent)
	defer endBlock(b, b.Len())
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

func quoteList(b *buf, vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = b.str(v)
	}
	return strings.Join(quoted, ", ")
}

// llmFields bundles the agent/judge node properties shared by both
// declaration kinds. It exists so writeAgentFields takes one named
// argument instead of 22 positional ones — eight of which are
// consecutive strings, where a transposition would compile cleanly but
// silently corrupt the emitted source. Field names mirror ast.AgentDecl
// / ast.JudgeDecl so the call-site literals read as a direct projection.
type llmFields struct {
	Model, Backend, Provider, Command   string
	Input, Output, Publish              string
	ArtifactLabels                      []string
	System, User                        string
	Session                             ast.SessionMode
	Tools, ToolPolicy                   []string
	Capabilities                        []string
	Skills                              []string
	ToolMaxSteps, MaxTokens             int
	ReasoningEffort                     string
	Timeout                             string
	Readonly                            bool
	FullAccess                          bool
	Images                              []string
	Interaction                         ast.InteractionMode
	InteractionPrompt, InteractionModel string
	Await                               ast.AwaitMode
	Compress                            string
	AutoMemory                          string
	Permission                          string
	Needs                               []string
}

func writeAgentFields(b *buf, f llmFields) {
	if f.Model != "" {
		writeQuotedProp(b, "model", f.Model)
	}
	if f.Backend != "" {
		writeQuotedProp(b, "backend", f.Backend)
	}
	if f.Provider != "" {
		writeQuotedProp(b, "provider", f.Provider)
	}
	if f.Command != "" {
		writeQuotedProp(b, "command", f.Command)
	}
	if f.Input != "" {
		writeIdentProp(b, "input", f.Input)
	}
	if f.Output != "" {
		writeIdentProp(b, "output", f.Output)
	}
	if f.Publish != "" {
		writeIdentProp(b, "publish", f.Publish)
	}
	writeArtifactLabels(b, f.ArtifactLabels, "  ")
	if f.System != "" {
		writeIdentProp(b, "system", f.System)
	}
	if f.User != "" {
		writeIdentProp(b, "user", f.User)
	}
	// Only emit session: when it's non-default. The previous if/else
	// emitted it unconditionally — both branches called the same
	// writeProp — which broke parse → unparse → re-parse round-trip
	// stability (every agent/judge would gain a synthetic
	// `session: fresh` line that wasn't in the source).
	if f.Session != ast.SessionFresh {
		writeProp(b, "session", f.Session.String())
	}
	if len(f.Tools) > 0 {
		fmt.Fprintf(b, "  tools: [%s]\n", strings.Join(f.Tools, ", "))
	}
	if len(f.ToolPolicy) > 0 {
		fmt.Fprintf(b, "  tool_policy: [%s]\n", strings.Join(f.ToolPolicy, ", "))
	}
	if len(f.Capabilities) > 0 {
		fmt.Fprintf(b, "  capabilities: [%s]\n", strings.Join(f.Capabilities, ", "))
	}
	if len(f.Skills) > 0 {
		fmt.Fprintf(b, "  skills: [%s]\n", quoteList(b, f.Skills))
	}
	if f.ToolMaxSteps > 0 {
		fmt.Fprintf(b, "  tool_max_steps: %d\n", f.ToolMaxSteps)
	}
	if f.MaxTokens > 0 {
		fmt.Fprintf(b, "  max_tokens: %d\n", f.MaxTokens)
	}
	if f.ReasoningEffort != "" {
		writeReasoningEffortProp(b, f.ReasoningEffort)
	}
	if f.Timeout != "" {
		writeQuotedProp(b, "timeout", f.Timeout)
	}
	if f.Readonly {
		writeProp(b, "readonly", "true")
	}
	if f.FullAccess {
		writeProp(b, "full_access", "true")
	}
	if len(f.Images) > 0 {
		fmt.Fprintf(b, "  images: [%s]\n", quoteList(b, f.Images))
	}
	if f.Interaction != ast.InteractionNone {
		writeProp(b, "interaction", f.Interaction.String())
	}
	if f.InteractionPrompt != "" {
		writeProp(b, "interaction_prompt", f.InteractionPrompt)
	}
	if f.InteractionModel != "" {
		writeQuotedProp(b, "interaction_model", f.InteractionModel)
	}
	if f.Await != ast.AwaitNone {
		writeProp(b, "await", f.Await.String())
	}
	if f.Compress != "" {
		writeProp(b, "compress", f.Compress)
	}
	if f.AutoMemory != "" {
		writeProp(b, "auto_memory", f.AutoMemory)
	}
	if f.Permission != "" {
		writeProp(b, "permission", f.Permission)
	}
	if len(f.Needs) > 0 {
		fmt.Fprintf(b, "  needs: [%s]\n", strings.Join(f.Needs, ", "))
	}
}

// writeSandboxBlock serializes an [ast.SandboxBlock] back to its
// canonical .bot source. Empty / nil blocks emit nothing. The short
// form (`sandbox: ident`) is used when only Mode is set; otherwise
// the full block form is rendered with each populated field on its
// own line.
//
// Round-trip stability: parser → IR → unparse → parser must produce
// the same AST. Tests in pkg/dsl/unparse/unparse_test.go and
// pkg/dsl/ir/sandbox_test.go pin the contract.
func writeSandboxBlock(b *buf, sb *ast.SandboxBlock, indent string) {
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
		fmt.Fprintf(b, "%simage: %s\n", inner, b.str(sb.Image))
	}
	if sb.User != "" {
		fmt.Fprintf(b, "%suser: %s\n", inner, b.str(sb.User))
	}
	if sb.WorkspaceFolder != "" {
		fmt.Fprintf(b, "%sworkspace_folder: %s\n", inner, b.str(sb.WorkspaceFolder))
	}
	if sb.HostState != "" {
		fmt.Fprintf(b, "%shost_state: %s\n", inner, sb.HostState)
	}
	if sb.PostCreate != "" {
		fmt.Fprintf(b, "%spost_create: %s\n", inner, b.str(sb.PostCreate))
	}
	if len(sb.Env) > 0 {
		fmt.Fprintf(b, "%senv:\n", inner)
		for _, k := range slices.Sorted(maps.Keys(sb.Env)) {
			fmt.Fprintf(b, "%s  %s: %s\n", inner, k, b.str(sb.Env[k]))
		}
	}
	if len(sb.Mounts) > 0 {
		fmt.Fprintf(b, "%smounts: [", inner)
		for i, m := range sb.Mounts {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(b, "%s", b.str(m))
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

func writeSandboxBuildBlock(b *buf, bb *ast.SandboxBuildBlock, indent string) {
	fmt.Fprintf(b, "%sbuild:\n", indent)
	defer endBlock(b, b.Len())
	inner := indent + "  "
	if bb.Dockerfile != "" {
		fmt.Fprintf(b, "%sdockerfile: %s\n", inner, b.str(bb.Dockerfile))
	}
	if bb.Context != "" {
		fmt.Fprintf(b, "%scontext: %s\n", inner, b.str(bb.Context))
	}
	if len(bb.Args) > 0 {
		fmt.Fprintf(b, "%sargs:\n", inner)
		for _, k := range slices.Sorted(maps.Keys(bb.Args)) {
			fmt.Fprintf(b, "%s  %s: %s\n", inner, k, b.str(bb.Args[k]))
		}
	}
}

func writeSandboxNetworkBlock(b *buf, n *ast.SandboxNetworkBlock, indent string) {
	fmt.Fprintf(b, "%snetwork:\n", indent)
	defer endBlock(b, b.Len())
	inner := indent + "  "
	if n.Mode != "" {
		fmt.Fprintf(b, "%smode: %s\n", inner, n.Mode)
	}
	if n.Preset != "" {
		// Preset names are kebab-case ("iterion-default"), which the lexer
		// reads as ident/-/ident: the parser takes a string or an ident.
		fmt.Fprintf(b, "%spreset: %s\n", inner, identOrStr(b, n.Preset))
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
			fmt.Fprintf(b, "%s", b.str(r))
		}
		b.WriteString("]\n")
	}
}

func writeCompaction(b *buf, compaction *ast.CompactionBlock, indent string, leadingBlank bool) {
	if leadingBlank {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "%scompaction:\n", indent)
	defer endBlock(b, b.Len())
	if compaction.Threshold != nil {
		fmt.Fprintf(b, "%s  threshold: %g\n", indent, *compaction.Threshold)
	}
	if compaction.PreserveRecent != nil {
		fmt.Fprintf(b, "%s  preserve_recent: %d\n", indent, *compaction.PreserveRecent)
	}
}

func writeMemory(b *buf, m *ast.MemoryBlock, indent string, leadingBlank bool) {
	if leadingBlank {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "%smemory:\n", indent)
	defer endBlock(b, b.Len())
	if m.Enabled != nil {
		fmt.Fprintf(b, "%s  enabled: %t\n", indent, *m.Enabled)
	}
	if m.Scope != nil {
		fmt.Fprintf(b, "%s  scope: %s\n", indent, b.str(*m.Scope))
	}
	if len(m.Autoload) > 0 {
		quoted := make([]string, len(m.Autoload))
		for i, s := range m.Autoload {
			quoted[i] = b.str(s)
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
	if m.ProjectRoot != nil {
		fmt.Fprintf(b, "%s  project_root: %t\n", indent, *m.ProjectRoot)
	}
	if m.Visibility != nil {
		fmt.Fprintf(b, "%s  visibility: %s\n", indent, b.str(*m.Visibility))
	}
}

// writeCursorDecl renders a top-level `cursor NAME:` declaration.
// Values and Bands are serialized in declaration order so that
// parse → unparse → parse stays stable; the IR compiler is the place
// where reorderings happen.
func writeCursorDecl(b *buf, c *ast.CursorDecl) {
	fmt.Fprintf(b, "cursor %s:\n", c.Name)
	if c.Description != "" {
		fmt.Fprintf(b, "  description: %s\n", b.str(c.Description))
	}
	if len(c.Values) > 0 {
		b.WriteString("  values:\n")
		for _, v := range c.Values {
			fmt.Fprintf(b, "    %s: %s\n", v.Name, b.str(v.Prompt))
		}
	}
	if len(c.Bands) > 0 {
		b.WriteString("  bands:\n")
		for _, band := range c.Bands {
			fmt.Fprintf(b, "    %s: %s\n", b.str(band.Range), b.str(band.Prompt))
		}
	}
}

// writeCursorsBlock renders an agent/judge `cursors:` activation
// block. Settings preserve declaration order; only the explicit
// `enabled: false` form needs emission — the default true is the
// implicit shape the parser assumes.
func writeCursorsBlock(b *buf, cb *ast.CursorBlock, indent string) {
	fmt.Fprintf(b, "%scursors:\n", indent)
	defer endBlock(b, b.Len())
	if !cb.Enabled {
		fmt.Fprintf(b, "%s  enabled: false\n", indent)
	}
	for _, s := range cb.Settings {
		if isCursorValueBareIdent(s.Value) {
			fmt.Fprintf(b, "%s  %s: %s\n", indent, s.Key, s.Value)
		} else {
			fmt.Fprintf(b, "%s  %s: %s\n", indent, s.Key, b.str(s.Value))
		}
	}
}

// writeFallbacksBlock renders an agent/judge `fallbacks:` block
// (ADR-087) as named entries, preserving declaration order — which IS
// the try order.
//
// Omitting this would not merely lose formatting: the studio saves every
// edit through parse → unparse, so an unserialised block is DELETED from
// the .bot the next time anyone touches an unrelated field.
func writeFallbacksBlock(b *buf, fbs []*ast.FallbackDecl, indent string) {
	if len(fbs) == 0 {
		return
	}
	fmt.Fprintf(b, "%sfallbacks:\n", indent)
	for _, fb := range fbs {
		if fb == nil || strings.TrimSpace(fb.Name) == "" {
			// A route with no name has no `<name>:` header to emit: it
			// would serialise as a bare `  :` the parser rejects
			// (R4a40d3). Skip rather than produce a .bot that cannot
			// re-parse — the studio saves every edit through unparse.
			continue
		}
		fmt.Fprintf(b, "%s  %s:\n", indent, fb.Name)
		if fb.Backend != "" {
			fmt.Fprintf(b, "%s    backend: %s\n", indent, b.str(fb.Backend))
		}
		if fb.Model != "" {
			fmt.Fprintf(b, "%s    model: %s\n", indent, b.str(fb.Model))
		}
		if fb.Provider != "" {
			fmt.Fprintf(b, "%s    provider: %s\n", indent, b.str(fb.Provider))
		}
		if len(fb.On) > 0 {
			fmt.Fprintf(b, "%s    on: [%s]\n", indent, strings.Join(fb.On, ", "))
		}
		if fb.Metered {
			fmt.Fprintf(b, "%s    metered: true\n", indent)
		}
		if fb.Action != "" {
			fmt.Fprintf(b, "%s    action: %s\n", indent, fb.Action)
		}
		if fb.When != "" {
			fmt.Fprintf(b, "%s    when: %s\n", indent, b.str(fb.When))
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

func writeBudget(b *buf, budget *ast.BudgetBlock) {
	b.WriteString("\n  budget:\n")
	defer endBlock(b, b.Len())
	if budget.MaxParallelBranches > 0 {
		fmt.Fprintf(b, "    max_parallel_branches: %d\n", budget.MaxParallelBranches)
	}
	if budget.MaxDuration != "" {
		fmt.Fprintf(b, "    max_duration: %s\n", b.str(budget.MaxDuration))
	}
	if budget.MaxCostUSD > 0 {
		fmt.Fprintf(b, "    max_cost_usd: %g\n", budget.MaxCostUSD)
	}
	if budget.MaxTokens > 0 {
		fmt.Fprintf(b, "    max_tokens: %d\n", budget.MaxTokens)
	}
	if budget.WarnTokens > 0 {
		fmt.Fprintf(b, "    warn_tokens: %d\n", budget.WarnTokens)
	}
	if budget.MaxIterations > 0 {
		fmt.Fprintf(b, "    max_iterations: %d\n", budget.MaxIterations)
	}
}

// writeResources serializes the workflow `resources:` block. Names are
// emitted in sorted order for deterministic, round-trip-stable output.
func writeResources(b *buf, res *ast.ResourcesBlock) {
	if res == nil {
		return
	}
	b.WriteString("\n  resources:\n")
	defer endBlock(b, b.Len())
	names := make([]string, 0, len(res.Capacities))
	for name := range res.Capacities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Lease form round-trips as the quoted string-list it was declared
		// with; counting form as the bare capacity.
		if members := res.Members[name]; len(members) > 0 {
			quoted := make([]string, len(members))
			for i, m := range members {
				quoted[i] = b.str(m)
			}
			fmt.Fprintf(b, "    %s: [%s]\n", name, strings.Join(quoted, ", "))
		} else {
			fmt.Fprintf(b, "    %s: %d\n", name, res.Capacities[name])
		}
	}
}

func writeEdge(b *buf, e *ast.Edge) {
	fmt.Fprintf(b, "  %s -> %s", e.From, e.To)
	if e.IsElse {
		b.WriteString(" else")
	}
	if e.When != nil {
		if e.When.Expr != "" {
			fmt.Fprintf(b, " when %s", b.str(e.When.Expr))
		} else {
			b.WriteString(" when ")
			if e.When.Negated {
				b.WriteString("not ")
			}
			b.WriteString(e.When.Condition)
		}
	}
	if e.Loop != nil {
		switch {
		case e.Loop.Unbounded:
			if e.Loop.FuelCap > 0 {
				fmt.Fprintf(b, " as %s(unbounded %d)", e.Loop.Name, e.Loop.FuelCap)
			} else {
				fmt.Fprintf(b, " as %s(unbounded)", e.Loop.Name)
			}
		case e.Loop.MaxIterationsExpr != "":
			fmt.Fprintf(b, " as %s(%s)", e.Loop.Name, b.str(e.Loop.MaxIterationsExpr))
		default:
			fmt.Fprintf(b, " as %s(%d)", e.Loop.Name, e.Loop.MaxIterations)
		}
	}
	if e.Foreach != nil {
		fmt.Fprintf(b, " as foreach %s(%s in %s)", e.Foreach.Name, e.Foreach.Item, b.str(e.Foreach.Collection))
	}
	if len(e.With) > 0 {
		if len(e.With) == 1 {
			fmt.Fprintf(b, " with {\n")
			fmt.Fprintf(b, "    %s: %s\n", e.With[0].Key, b.str(e.With[0].Value))
			b.WriteString("  }")
		} else {
			b.WriteString(" with {\n")
			for _, w := range e.With {
				fmt.Fprintf(b, "    %s: %s", w.Key, b.str(w.Value))
				b.WriteByte(',')
				b.WriteByte('\n')
			}
			b.WriteString("  }")
		}
	}
	b.WriteByte('\n')
}
