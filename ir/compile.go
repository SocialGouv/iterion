package ir

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/ast"
)

// ---------------------------------------------------------------------------
// Compiler diagnostics
// ---------------------------------------------------------------------------

// DiagCode identifies the kind of compilation diagnostic.
type DiagCode string

const (
	DiagUnknownNode            DiagCode = "C001" // edge references unknown node
	DiagUnknownSchema          DiagCode = "C002" // node references unknown schema
	DiagUnknownPrompt          DiagCode = "C003" // node references unknown prompt
	DiagBadTemplateRef         DiagCode = "C004" // malformed template reference
	DiagDuplicateLoop          DiagCode = "C005" // conflicting loop definitions
	DiagNoWorkflow             DiagCode = "C006" // no workflow found in file
	DiagMultipleWorkflow       DiagCode = "C007" // multiple workflows (unsupported in V1)
	DiagMissingEntry           DiagCode = "C008" // entry node not found
	DiagMissingModelOrDelegate DiagCode = "C018" // agent/judge has neither model nor delegate
	DiagDuplicateMCPServer     DiagCode = "C024" // duplicate top-level mcp_server name
	DiagInvalidMCPServer       DiagCode = "C025" // invalid MCP server config
)

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
type Diagnostic struct {
	Code     DiagCode
	Severity Severity
	Message  string
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
	nodes   map[string]*Node
	schemas map[string]*Schema
	prompts map[string]*Prompt
	mcp     map[string]*MCPServer
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

// Compile transforms an AST File into a canonical IR Workflow.
// In V1, exactly one workflow per file is supported.
func Compile(file *ast.File) *CompileResult {
	c := &compiler{
		file:    file,
		nodes:   make(map[string]*Node),
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
	c.compileJoins()
	c.compileHumans()
	c.compileTools()

	// Add terminal nodes.
	c.nodes["done"] = &Node{ID: "done", Kind: NodeDone}
	c.nodes["fail"] = &Node{ID: "fail", Kind: NodeFail}

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

	w := &Workflow{
		Name:       wf.Name,
		Entry:      wf.Entry,
		Nodes:      c.nodes,
		Edges:      edges,
		Schemas:    c.schemas,
		Prompts:    c.prompts,
		Vars:       vars,
		Loops:      loops,
		Budget:     budget,
		MCP:        convertMCPConfig(wf.MCP),
		MCPServers: c.mcp,
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
			Transport: convertMCPTransport(s.Transport),
			Command:   s.Command,
			Args:      append([]string(nil), s.Args...),
			URL:       s.URL,
		}
		c.validateMCPServer(server)
		c.mcp[s.Name] = server
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
	case MCPTransportHTTP:
		if s.URL == "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport http must set 'url'", s.Name)
		}
		if s.Command != "" {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport http cannot set 'command'", s.Name)
		}
		if len(s.Args) > 0 {
			c.errorf(DiagInvalidMCPServer, "mcp_server %q with transport http cannot set 'args'", s.Name)
		}
	case MCPTransportSSE:
		c.errorf(DiagInvalidMCPServer, "mcp_server %q uses transport sse, which is not supported in v1", s.Name)
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
				Type:       convertFieldType(f.Type),
				EnumValues: f.EnumValues,
			}
		}
		c.schemas[s.Name] = &Schema{
			Name:   s.Name,
			Fields: fields,
		}
	}
}

func convertFieldType(ft ast.FieldType) FieldType {
	switch ft {
	case ast.FieldTypeString:
		return FieldTypeString
	case ast.FieldTypeBool:
		return FieldTypeBool
	case ast.FieldTypeInt:
		return FieldTypeInt
	case ast.FieldTypeFloat:
		return FieldTypeFloat
	case ast.FieldTypeJSON:
		return FieldTypeJSON
	case ast.FieldTypeStringArray:
		return FieldTypeStringArray
	default:
		return FieldTypeString
	}
}

func convertMCPTransport(mt ast.MCPTransport) MCPTransport {
	switch mt {
	case ast.MCPTransportStdio:
		return MCPTransportStdio
	case ast.MCPTransportHTTP:
		return MCPTransportHTTP
	case ast.MCPTransportSSE:
		return MCPTransportSSE
	default:
		return MCPTransportUnknown
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
		if model == "" && a.Delegate == "" {
			c.errorf(DiagMissingModelOrDelegate, "agent %q must set 'model' or 'delegate', or define ITERION_DEFAULT_SUPERVISOR_MODEL", a.Name)
		}

		c.nodes[a.Name] = &Node{
			ID:           a.Name,
			Kind:         NodeAgent,
			Model:        model,
			Delegate:     a.Delegate,
			MCP:          convertMCPConfig(a.MCP),
			InputSchema:  a.Input,
			OutputSchema: a.Output,
			Publish:      a.Publish,
			SystemPrompt: a.System,
			UserPrompt:   a.User,
			Session:      convertSessionMode(a.Session),
			Tools:        a.Tools,
			ToolMaxSteps: a.ToolMaxSteps,
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
		if model == "" && j.Delegate == "" {
			c.errorf(DiagMissingModelOrDelegate, "judge %q must set 'model' or 'delegate', or define ITERION_DEFAULT_SUPERVISOR_MODEL", j.Name)
		}

		c.nodes[j.Name] = &Node{
			ID:           j.Name,
			Kind:         NodeJudge,
			Model:        model,
			Delegate:     j.Delegate,
			MCP:          convertMCPConfig(j.MCP),
			InputSchema:  j.Input,
			OutputSchema: j.Output,
			Publish:      j.Publish,
			SystemPrompt: j.System,
			UserPrompt:   j.User,
			Session:      convertSessionMode(j.Session),
			Tools:        j.Tools,
			ToolMaxSteps: j.ToolMaxSteps,
		}
	}
}

// ---------------------------------------------------------------------------
// Nodes — Router
// ---------------------------------------------------------------------------

func (c *compiler) compileRouters() {
	for _, r := range c.file.Routers {
		mode := convertRouterMode(r.Mode)
		node := &Node{
			ID:         r.Name,
			Kind:       NodeRouter,
			RouterMode: mode,
		}
		if mode != RouterLLM {
			if r.Model != "" {
				c.errorf(DiagRouterLLMOnlyProperty, "router %q property 'model' is only valid with mode: llm", r.Name)
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
			if r.Model == "" {
				c.errorf(DiagMissingModelOrDelegate, "router %q with mode llm must set 'model'", r.Name)
			}
			node.Model = r.Model
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
// Nodes — Join
// ---------------------------------------------------------------------------

func (c *compiler) compileJoins() {
	for _, j := range c.file.Joins {
		c.validateSchemaRef(j.Name, "output", j.Output)

		c.nodes[j.Name] = &Node{
			ID:           j.Name,
			Kind:         NodeJoin,
			JoinStrategy: convertJoinStrategy(j.Strategy),
			Require:      j.Require,
			JoinOutput:   j.Output,
		}
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

		mode := convertHumanMode(h.Mode)
		node := &Node{
			ID:           h.Name,
			Kind:         NodeHuman,
			InputSchema:  h.Input,
			OutputSchema: h.Output,
			Publish:      h.Publish,
			HumanMode:    mode,
			MinAnswers:   h.MinAnswers,
			Instructions: h.Instructions,
		}

		// Auto modes require a model and output schema for LLM execution.
		if mode == HumanAutoAnswer || mode == HumanAutoOrPause {
			if h.Model == "" {
				c.errorf(DiagMissingModelOrDelegate, "human %q with mode %s must set 'model'", h.Name, mode)
			}
			if h.Output == "" {
				c.errorf(DiagMissingModelOrDelegate, "human %q with mode %s must set 'output'", h.Name, mode)
			}
			node.Model = h.Model
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

		c.nodes[t.Name] = &Node{
			ID:           t.Name,
			Kind:         NodeTool,
			OutputSchema: t.Output,
			Command:      t.Command,
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

		// Condition.
		if ae.When != nil {
			e.Condition = ae.When.Condition
			e.Negated = ae.When.Negated
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
// Type converters (AST → IR, keeping IR independent from AST enums)
// ---------------------------------------------------------------------------

func convertSessionMode(sm ast.SessionMode) SessionMode {
	switch sm {
	case ast.SessionInherit:
		return SessionInherit
	case ast.SessionArtifactsOnly:
		return SessionArtifactsOnly
	default:
		return SessionFresh
	}
}

func convertRouterMode(rm ast.RouterMode) RouterMode {
	switch rm {
	case ast.RouterCondition:
		return RouterCondition
	case ast.RouterRoundRobin:
		return RouterRoundRobin
	case ast.RouterLLM:
		return RouterLLM
	default:
		return RouterFanOutAll
	}
}

func convertJoinStrategy(js ast.JoinStrategy) JoinStrategy {
	switch js {
	case ast.JoinBestEffort:
		return JoinBestEffort
	default:
		return JoinWaitAll
	}
}

func convertHumanMode(hm ast.HumanMode) HumanMode {
	switch hm {
	case ast.HumanAutoAnswer:
		return HumanAutoAnswer
	case ast.HumanAutoOrPause:
		return HumanAutoOrPause
	default:
		return HumanPauseUntilAnswers
	}
}

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
