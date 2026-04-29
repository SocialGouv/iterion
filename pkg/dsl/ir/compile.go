package ir

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// ---------------------------------------------------------------------------
// Compiler diagnostics
// ---------------------------------------------------------------------------

// DiagCode identifies the kind of compilation diagnostic.
type DiagCode string

const (
	DiagUnknownNode           DiagCode = "C001" // edge references unknown node
	DiagUnknownSchema         DiagCode = "C002" // node references unknown schema
	DiagUnknownPrompt         DiagCode = "C003" // node references unknown prompt
	DiagBadTemplateRef        DiagCode = "C004" // malformed template reference
	DiagDuplicateLoop         DiagCode = "C005" // conflicting loop definitions
	DiagNoWorkflow            DiagCode = "C006" // no workflow found in file
	DiagMultipleWorkflow      DiagCode = "C007" // multiple workflows (unsupported in V1)
	DiagMissingEntry          DiagCode = "C008" // entry node not found
	DiagMissingModelOrBackend DiagCode = "C018" // agent/judge has neither model nor backend
	DiagDuplicateMCPServer    DiagCode = "C024" // duplicate top-level mcp_server name
	DiagInvalidMCPServer      DiagCode = "C025" // invalid MCP server config
	DiagCodexDiscouraged      DiagCode = "C030" // codex backend is supported but discouraged
	DiagComputeNoExpr         DiagCode = "C039" // compute node has no expressions
	DiagBadExpr               DiagCode = "C040" // expression failed to parse
)

// codexBackendName is the literal value of the discouraged backend.
// Hardcoded here (rather than imported from delegate/) to avoid an ir → delegate
// dependency, which the package layout intentionally forbids.
const codexBackendName = "codex"

// Severity indicates the severity of a diagnostic.
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// Diagnostic represents a compilation error or warning.
//
// NodeID and EdgeID are best-effort attribution fields used by tooling (the
// editor renders them as inline badges). They may be empty when the diagnostic
// is global (e.g. "no workflow"). EdgeID follows the canonical "<from>-><to>"
// format the editor uses; when multiple edges share endpoints the first
// matching one wins.
//
// Hint is a one-line, user-facing fix suggestion when one is known. The
// authoritative documentation still lives in `docs/diagnostics.md`; Hint is
// for UIs that want a quick tooltip without round-tripping to docs.
type Diagnostic struct {
	Code     DiagCode
	Severity Severity
	Message  string
	NodeID   string
	EdgeID   string
	Hint     string
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s [%s]: %s", d.Severity, d.Code, d.Message)
}

// ---------------------------------------------------------------------------
// CompileResult
// ---------------------------------------------------------------------------

// CompileResult holds the compiled IR workflow and any diagnostics.
type CompileResult struct {
	Workflow    *Workflow
	Diagnostics []Diagnostic
}

// HasErrors returns true if any diagnostic is an error.
func (r *CompileResult) HasErrors() bool {
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Compiler
// ---------------------------------------------------------------------------

// compiler holds state during compilation.
type compiler struct {
	file    *ast.File
	diags   []Diagnostic
	nodes   map[string]Node
	schemas map[string]*Schema
	prompts map[string]*Prompt
	mcp     map[string]*MCPServer
}

// workflowInteractionDefault returns the workflow-level interaction default,
// or InteractionNone if none is set.
func (c *compiler) workflowInteractionDefault() InteractionMode {
	if len(c.file.Workflows) > 0 {
		wf := c.file.Workflows[0]
		if wf.Interaction != nil {
			return *wf.Interaction
		}
	}
	return InteractionNone
}

func (c *compiler) errorf(code DiagCode, format string, args ...interface{}) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityError,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (c *compiler) warnf(code DiagCode, format string, args ...interface{}) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityWarning,
		Message:  fmt.Sprintf(format, args...),
	})
}

// errorfAt is a variant of errorf that attaches authoritative attribution
// (nodeID and/or edgeID) so downstream tooling can render the diagnostic on
// the precise node or edge instead of guessing from the message text.
func (c *compiler) errorfAt(code DiagCode, nodeID, edgeID string, format string, args ...interface{}) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityError,
		Message:  fmt.Sprintf(format, args...),
		NodeID:   nodeID,
		EdgeID:   edgeID,
	})
}

// warnfAt is the warning counterpart to errorfAt.
func (c *compiler) warnfAt(code DiagCode, nodeID, edgeID string, format string, args ...interface{}) {
	c.diags = append(c.diags, Diagnostic{
		Code:     code,
		Severity: SeverityWarning,
		Message:  fmt.Sprintf(format, args...),
		NodeID:   nodeID,
		EdgeID:   edgeID,
	})
}

// edgeID builds the canonical "<from>-><to>" identifier the editor uses so
// inline diagnostic badges can match attributed diagnostics to the right edge.
func edgeID(from, to string) string {
	return from + "->" + to
}

// warnCodexDiscouraged emits a C030 warning when a node uses the codex backend.
// Codex is still supported but has known limitations (cannot configure tool set,
// tends to fill its own context window, weaker iterion integration). New
// workflows should prefer 'claude_code' for tool-using agents or 'claw' with an
// OpenAI model (e.g. model: "openai/gpt-5.4-mini") for judges/reviewers.
func (c *compiler) warnCodexDiscouraged(kind, name, backend string) {
	if backend != codexBackendName {
		return
	}
	c.warnfAt(DiagCodexDiscouraged, name, "",
		"%s %q uses 'codex' backend which is supported but discouraged: codex cannot configure its tool set, tends to fill its context window, and has weaker integration; prefer 'claude_code' for tool-using agents or 'claw' with an OpenAI model (e.g. model: \"openai/gpt-5.4-mini\") for judges/reviewers",
		kind, name)
}

// Compile transforms an AST File into a canonical IR Workflow.
// In V1, exactly one workflow per file is supported.
func Compile(file *ast.File) *CompileResult {
	c := &compiler{
		file:    file,
		nodes:   make(map[string]Node),
		schemas: make(map[string]*Schema),
		prompts: make(map[string]*Prompt),
		mcp:     make(map[string]*MCPServer),
	}
	w := c.compile()
	return &CompileResult{
		Workflow:    w,
		Diagnostics: c.diags,
	}
}

func (c *compiler) compile() *Workflow {
	// Validate workflow count.
	if len(c.file.Workflows) == 0 {
		c.errorf(DiagNoWorkflow, "no workflow declaration found")
		return nil
	}
	if len(c.file.Workflows) > 1 {
		c.errorf(DiagMultipleWorkflow, "multiple workflows not supported in V1; found %d", len(c.file.Workflows))
	}

	// Compile shared declarations.
	c.compileMCPServers()
	c.compileSchemas()
	c.compilePrompts()

	// Compile nodes from all node declarations.
	c.compileAgents()
	c.compileJudges()
	c.compileRouters()
	c.compileHumans()
	c.compileTools()
	c.compileComputes()

	// Add terminal nodes.
	c.nodes["done"] = &DoneNode{BaseNode: BaseNode{ID: "done"}}
	c.nodes["fail"] = &FailNode{BaseNode: BaseNode{ID: "fail"}}

	wf := c.file.Workflows[0]

	// Validate entry node.
	if _, ok := c.nodes[wf.Entry]; !ok {
		c.errorf(DiagMissingEntry, "entry node %q not found", wf.Entry)
	}

	// Compile vars (merge top-level + workflow-level).
	vars := c.compileVars(c.file.Vars, wf.Vars)

	// Compile edges.
	edges, loops := c.compileEdges(wf.Edges)

	// Compile budget.
	var budget *Budget
	if wf.Budget != nil {
		budget = c.compileBudget(wf.Budget)
	}

	// Compile workflow-level interaction default.
	var interaction *InteractionMode
	if wf.Interaction != nil {
		im := *wf.Interaction
		interaction = &im
	}

	w := &Workflow{
		Name:           wf.Name,
		Entry:          wf.Entry,
		DefaultBackend: wf.DefaultBackend,
		ToolPolicy:     wf.ToolPolicy,
		Nodes:          c.nodes,
		Edges:          edges,
		Schemas:        c.schemas,
		Prompts:        c.prompts,
		Vars:           vars,
		Loops:          loops,
		Budget:         budget,
		MCP:            convertMCPConfig(wf.MCP),
		MCPServers:     c.mcp,
		Interaction:    interaction,
	}

	// Static validation pass (P2-02).
	c.validate(w)

	return w
}

// ---------------------------------------------------------------------------
// MCP servers
// ---------------------------------------------------------------------------

func (c *compiler) compileMCPServers() {
	for _, s := range c.file.MCPServers {
		if _, exists := c.mcp[s.Name]; exists {
			c.errorf(DiagDuplicateMCPServer, "mcp_server %q declared more than once", s.Name)
			continue
		}
		server := &MCPServer{
			Name:      s.Name,
			Transport: s.Transport,
			Command:   s.Command,
			Args:      append([]string(nil), s.Args...),
			URL:       s.URL,
			Auth:      compileMCPAuth(s.Auth),
		}
		c.validateMCPServer(server)
		c.mcp[s.Name] = server
	}
}

// compileMCPAuth converts an AST auth declaration to its IR form.
// Returns nil when the AST node is nil so a missing block stays absent.
func compileMCPAuth(decl *ast.MCPAuthDecl) *MCPAuth {
	if decl == nil {
		return nil
	}
	return &MCPAuth{
		Type:      decl.Type,
		AuthURL:   decl.AuthURL,
		TokenURL:  decl.TokenURL,
		RevokeURL: decl.RevokeURL,
		ClientID:  decl.ClientID,
		Scopes:    append([]string(nil), decl.Scopes...),
	}
}

func (c *compiler) validateMCPServer(s *MCPServer) {
	switch s.Transport {
	case MCPTransportStdio:
		if s.Command == "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport stdio must set 'command'", s.Name)
		}
		if s.URL != "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport stdio cannot set 'url'", s.Name)
		}
	case MCPTransportHTTP, MCPTransportSSE:
		// HTTP and SSE share the same StreamableClientTransport at
		// runtime: both require a URL and forbid Command/Args.
		if s.URL == "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport %s must set 'url'", s.Name, s.Transport)
		}
		if s.Command != "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport %s cannot set 'command'", s.Name, s.Transport)
		}
		if len(s.Args) > 0 {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport %s cannot set 'args'", s.Name, s.Transport)
		}
	case MCPTransportUnknown:
		c.errorf(DiagInvalidMCPServer, "mcp_server %q must set a supported 'transport'", s.Name)
	}
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

func (c *compiler) compileSchemas() {
	for _, s := range c.file.Schemas {
		fields := make([]*SchemaField, len(s.Fields))
		for i, f := range s.Fields {
			fields[i] = &SchemaField{
				Name:       f.Name,
				Type:       f.Type,
				EnumValues: f.EnumValues,
			}
		}
		c.schemas[s.Name] = &Schema{
			Name:   s.Name,
			Fields: fields,
		}
	}
}

func convertMCPConfig(cfg *ast.MCPConfigDecl) *MCPConfig {
	if cfg == nil {
		return nil
	}
	return &MCPConfig{
		AutoloadProject: cloneBool(cfg.AutoloadProject),
		Inherit:         cloneBool(cfg.Inherit),
		Servers:         append([]string(nil), cfg.Servers...),
		Disable:         append([]string(nil), cfg.Disable...),
	}
}

func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func resolveSupervisorModel(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return os.Getenv("ITERION_DEFAULT_SUPERVISOR_MODEL")
}

// ---------------------------------------------------------------------------
// Prompts
// ---------------------------------------------------------------------------

func (c *compiler) compilePrompts() {
	for _, p := range c.file.Prompts {
		refs, err := ParseRefs(p.Body)
		if err != nil {
			c.errorf(DiagBadTemplateRef, "prompt %q: %v", p.Name, err)
		}
		c.prompts[p.Name] = &Prompt{
			Name:         p.Name,
			Body:         p.Body,
			TemplateRefs: refs,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Agent
// ---------------------------------------------------------------------------

func (c *compiler) compileAgents() {
	for _, a := range c.file.Agents {
		c.validateSchemaRef(a.Name, "input", a.Input)
		c.validateSchemaRef(a.Name, "output", a.Output)
		c.validatePromptRef(a.Name, "system", a.System)
		c.validatePromptRef(a.Name, "user", a.User)
		model := resolveSupervisorModel(a.Model)
		if model == "" && a.Backend == "" {
			c.errorfAt(DiagMissingModelOrBackend, a.Name, "", "agent %q must set 'model' or 'backend', or define ITERION_DEFAULT_SUPERVISOR_MODEL", a.Name)
		}
		c.warnCodexDiscouraged("agent", a.Name, a.Backend)

		// Apply workflow-level interaction default when node doesn't set one.
		interaction := a.Interaction
		if interaction == InteractionNone {
			interaction = c.workflowInteractionDefault()
		}

		c.nodes[a.Name] = &AgentNode{
			BaseNode: BaseNode{ID: a.Name},
			LLMFields: LLMFields{
				Model:           model,
				Backend:         a.Backend,
				SystemPrompt:    a.System,
				UserPrompt:      a.User,
				MaxTokens:       a.MaxTokens,
				ReasoningEffort: a.ReasoningEffort,
				Readonly:        a.Readonly,
			},
			SchemaFields: SchemaFields{
				InputSchema:  a.Input,
				OutputSchema: a.Output,
			},
			InteractionFields: InteractionFields{
				Interaction:       interaction,
				InteractionPrompt: a.InteractionPrompt,
				InteractionModel:  a.InteractionModel,
			},
			MCP:          convertMCPConfig(a.MCP),
			Publish:      a.Publish,
			Session:      a.Session,
			Tools:        a.Tools,
			ToolPolicy:   a.ToolPolicy,
			ToolMaxSteps: a.ToolMaxSteps,
			AwaitMode:    a.Await,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Judge
// ---------------------------------------------------------------------------

func (c *compiler) compileJudges() {
	for _, j := range c.file.Judges {
		c.validateSchemaRef(j.Name, "input", j.Input)
		c.validateSchemaRef(j.Name, "output", j.Output)
		c.validatePromptRef(j.Name, "system", j.System)
		c.validatePromptRef(j.Name, "user", j.User)
		model := resolveSupervisorModel(j.Model)
		if model == "" && j.Backend == "" {
			c.errorfAt(DiagMissingModelOrBackend, j.Name, "", "judge %q must set 'model' or 'backend', or define ITERION_DEFAULT_SUPERVISOR_MODEL", j.Name)
		}
		c.warnCodexDiscouraged("judge", j.Name, j.Backend)

		// Apply workflow-level interaction default when node doesn't set one.
		interaction := j.Interaction
		if interaction == InteractionNone {
			interaction = c.workflowInteractionDefault()
		}

		c.nodes[j.Name] = &JudgeNode{
			BaseNode: BaseNode{ID: j.Name},
			LLMFields: LLMFields{
				Model:           model,
				Backend:         j.Backend,
				SystemPrompt:    j.System,
				UserPrompt:      j.User,
				MaxTokens:       j.MaxTokens,
				ReasoningEffort: j.ReasoningEffort,
				Readonly:        j.Readonly,
			},
			SchemaFields: SchemaFields{
				InputSchema:  j.Input,
				OutputSchema: j.Output,
			},
			InteractionFields: InteractionFields{
				Interaction:       interaction,
				InteractionPrompt: j.InteractionPrompt,
				InteractionModel:  j.InteractionModel,
			},
			MCP:          convertMCPConfig(j.MCP),
			Publish:      j.Publish,
			Session:      j.Session,
			Tools:        j.Tools,
			ToolPolicy:   j.ToolPolicy,
			ToolMaxSteps: j.ToolMaxSteps,
			AwaitMode:    j.Await,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Router
// ---------------------------------------------------------------------------

func (c *compiler) compileRouters() {
	for _, r := range c.file.Routers {
		mode := r.Mode
		node := &RouterNode{
			BaseNode:   BaseNode{ID: r.Name},
			RouterMode: mode,
		}
		if mode != RouterLLM {
			if r.Model != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'model' is only valid with mode: llm", r.Name)
			}
			if r.Backend != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'backend' is only valid with mode: llm", r.Name)
			}
			if r.System != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'system' is only valid with mode: llm", r.Name)
			}
			if r.User != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'user' is only valid with mode: llm", r.Name)
			}
			if r.Multi {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'multi' is only valid with mode: llm", r.Name)
			}
		}
		if mode == RouterLLM {
			model := resolveSupervisorModel(r.Model)
			if model == "" && r.Backend == "" {
				c.warnf(DiagMissingModelOrBackend, "router %q with mode llm has no model or backend; will use built-in default at runtime", r.Name)
			}
			node.Model = model
			node.Backend = r.Backend
			c.warnCodexDiscouraged("router", r.Name, r.Backend)
			if r.System != "" {
				c.validatePromptRef(r.Name, "system", r.System)
				node.SystemPrompt = r.System
			}
			if r.User != "" {
				c.validatePromptRef(r.Name, "user", r.User)
				node.UserPrompt = r.User
			}
			node.RouterMulti = r.Multi
		}
		c.nodes[r.Name] = node
	}
}

// ---------------------------------------------------------------------------
// Nodes — Human
// ---------------------------------------------------------------------------

func (c *compiler) compileHumans() {
	for _, h := range c.file.Humans {
		c.validateSchemaRef(h.Name, "input", h.Input)
		c.validateSchemaRef(h.Name, "output", h.Output)
		c.validatePromptRef(h.Name, "instructions", h.Instructions)

		interaction := h.Interaction
		// Human nodes default to InteractionHuman; workflow-level default
		// can override when the node doesn't set interaction explicitly.
		if h.Interaction == 0 {
			wfDefault := c.workflowInteractionDefault()
			if wfDefault != InteractionNone {
				interaction = wfDefault
			} else {
				interaction = InteractionHuman
			}
		}
		node := &HumanNode{
			BaseNode: BaseNode{ID: h.Name},
			SchemaFields: SchemaFields{
				InputSchema:  h.Input,
				OutputSchema: h.Output,
			},
			InteractionFields: InteractionFields{
				Interaction:       interaction,
				InteractionPrompt: h.InteractionPrompt,
				InteractionModel:  h.InteractionModel,
			},
			Publish:      h.Publish,
			MinAnswers:   h.MinAnswers,
			Instructions: h.Instructions,
			AwaitMode:    h.Await,
		}

		// LLM-based interaction modes require a model and output schema.
		if interaction == InteractionLLM || interaction == InteractionLLMOrHuman {
			model := h.InteractionModel
			if model == "" {
				model = h.Model
			}
			if model == "" {
				c.errorf(DiagMissingModelOrBackend, "human %q with interaction %s must set 'model' or 'interaction_model'", h.Name, interaction)
			}
			if h.Output == "" {
				c.errorf(DiagMissingModelOrBackend, "human %q with interaction %s must set 'output'", h.Name, interaction)
			}
			node.Model = h.Model
			if h.InteractionModel != "" {
				node.InteractionModel = h.InteractionModel
			}
			if h.System != "" {
				c.validatePromptRef(h.Name, "system", h.System)
				node.SystemPrompt = h.System
			}
		}

		c.nodes[h.Name] = node
	}
}

// ---------------------------------------------------------------------------
// Nodes — Tool
// ---------------------------------------------------------------------------

func (c *compiler) compileTools() {
	for _, t := range c.file.Tools {
		c.validateSchemaRef(t.Name, "output", t.Output)
		if t.Input != "" {
			c.validateSchemaRef(t.Name, "input", t.Input)
		}

		var cmdRefs []*Ref
		if refs, err := ParseRefs(t.Command); err != nil {
			c.errorf(DiagBadTemplateRef, "tool %q command: %v", t.Name, err)
		} else {
			cmdRefs = refs
		}

		c.nodes[t.Name] = &ToolNode{
			BaseNode: BaseNode{ID: t.Name},
			SchemaFields: SchemaFields{
				InputSchema:  t.Input,
				OutputSchema: t.Output,
			},
			Command:     t.Command,
			CommandRefs: cmdRefs,
			AwaitMode:   t.Await,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Compute
// ---------------------------------------------------------------------------

func (c *compiler) compileComputes() {
	for _, cd := range c.file.Computes {
		c.validateSchemaRef(cd.Name, "output", cd.Output)
		if cd.Input != "" {
			c.validateSchemaRef(cd.Name, "input", cd.Input)
		}
		if len(cd.Expr) == 0 {
			c.errorfAt(DiagComputeNoExpr, cd.Name, "",
				"compute %q has no `expr:` block — at least one expression is required", cd.Name)
		}
		exprs := make([]*ComputeExpr, 0, len(cd.Expr))
		for _, e := range cd.Expr {
			ast, err := expr.Parse(e.Expr)
			if err != nil {
				c.errorfAt(DiagBadExpr, cd.Name, "",
					"compute %q field %q: invalid expression %q: %v", cd.Name, e.Key, e.Expr, err)
				continue
			}
			exprs = append(exprs, &ComputeExpr{
				Key: e.Key,
				AST: ast,
				Raw: e.Expr,
			})
		}
		c.nodes[cd.Name] = &ComputeNode{
			BaseNode: BaseNode{ID: cd.Name},
			SchemaFields: SchemaFields{
				InputSchema:  cd.Input,
				OutputSchema: cd.Output,
			},
			Exprs:     exprs,
			AwaitMode: cd.Await,
		}
	}
}

// ---------------------------------------------------------------------------
// Edges
// ---------------------------------------------------------------------------

func (c *compiler) compileEdges(astEdges []*ast.Edge) ([]*Edge, map[string]*Loop) {
	loops := make(map[string]*Loop)
	edges := make([]*Edge, 0, len(astEdges))

	for _, ae := range astEdges {
		// Validate node references.
		if _, ok := c.nodes[ae.From]; !ok {
			c.errorf(DiagUnknownNode, "edge source %q not found", ae.From)
		}
		if _, ok := c.nodes[ae.To]; !ok {
			c.errorf(DiagUnknownNode, "edge target %q not found", ae.To)
		}

		e := &Edge{
			From: ae.From,
			To:   ae.To,
		}

		// Condition: either a simple field name (legacy) or a parsed expression.
		if ae.When != nil {
			if ae.When.Expr != "" {
				ast, err := expr.Parse(ae.When.Expr)
				if err != nil {
					c.errorfAt(DiagBadExpr, "", edgeID(ae.From, ae.To),
						"edge %s -> %s: invalid `when` expression %q: %v",
						ae.From, ae.To, ae.When.Expr, err)
				} else {
					e.Expression = ast
					e.ExpressionSrc = ae.When.Expr
				}
			} else {
				e.Condition = ae.When.Condition
				e.Negated = ae.When.Negated
			}
		}

		// Loop.
		if ae.Loop != nil {
			e.LoopName = ae.Loop.Name
			if existing, ok := loops[ae.Loop.Name]; ok {
				// Multiple edges can share a loop, but max_iterations must agree.
				if existing.MaxIterations != ae.Loop.MaxIterations {
					c.errorf(DiagDuplicateLoop,
						"loop %q has conflicting max_iterations: %d vs %d",
						ae.Loop.Name, existing.MaxIterations, ae.Loop.MaxIterations)
				}
			} else {
				loops[ae.Loop.Name] = &Loop{
					Name:          ae.Loop.Name,
					MaxIterations: ae.Loop.MaxIterations,
				}
			}
		}

		// Data mappings.
		if len(ae.With) > 0 {
			e.With = make([]*DataMapping, len(ae.With))
			for i, w := range ae.With {
				refs, err := ParseRefs(w.Value)
				if err != nil {
					c.errorf(DiagBadTemplateRef, "edge %s -> %s, with key %q: %v",
						ae.From, ae.To, w.Key, err)
				}
				e.With[i] = &DataMapping{
					Key:  w.Key,
					Refs: refs,
					Raw:  w.Value,
				}
			}
		}

		edges = append(edges, e)
	}

	return edges, loops
}

// ---------------------------------------------------------------------------
// Vars
// ---------------------------------------------------------------------------

func (c *compiler) compileVars(topLevel *ast.VarsBlock, workflowLevel *ast.VarsBlock) map[string]*Var {
	vars := make(map[string]*Var)

	addVars := func(vb *ast.VarsBlock) {
		if vb == nil {
			return
		}
		for _, f := range vb.Fields {
			v := &Var{
				Name: f.Name,
				Type: convertVarType(f.Type),
			}
			if f.Default != nil {
				v.HasDefault = true
				switch f.Default.Kind {
				case ast.LitString:
					v.Default = f.Default.StrVal
				case ast.LitInt:
					v.Default = f.Default.IntVal
				case ast.LitFloat:
					v.Default = f.Default.FloatVal
				case ast.LitBool:
					v.Default = f.Default.BoolVal
				}
			}
			vars[f.Name] = v
		}
	}

	// Top-level vars first, then workflow-level (workflow overrides).
	addVars(topLevel)
	addVars(workflowLevel)

	return vars
}

// ---------------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------------

func (c *compiler) compileBudget(b *ast.BudgetBlock) *Budget {
	return &Budget{
		MaxParallelBranches: b.MaxParallelBranches,
		MaxDuration:         b.MaxDuration,
		MaxCostUSD:          b.MaxCostUSD,
		MaxTokens:           b.MaxTokens,
		MaxIterations:       b.MaxIterations,
	}
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

func (c *compiler) validateSchemaRef(node, prop, ref string) {
	if ref == "" {
		return
	}
	if _, ok := c.schemas[ref]; !ok {
		c.errorf(DiagUnknownSchema, "node %q property %q references unknown schema %q", node, prop, ref)
	}
}

func (c *compiler) validatePromptRef(node, prop, ref string) {
	if ref == "" {
		return
	}
	if _, ok := c.prompts[ref]; !ok {
		c.errorf(DiagUnknownPrompt, "node %q property %q references unknown prompt %q", node, prop, ref)
	}
}

// ---------------------------------------------------------------------------
// Type converters (AST → IR)
// ---------------------------------------------------------------------------

func convertVarType(te ast.TypeExpr) VarType {
	switch te {
	case ast.TypeBool:
		return VarBool
	case ast.TypeInt:
		return VarInt
	case ast.TypeFloat:
		return VarFloat
	case ast.TypeJSON:
		return VarJSON
	case ast.TypeStringArray:
		return VarStringArray
	default:
		return VarString
	}
}
