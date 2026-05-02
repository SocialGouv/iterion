// MarshalFile / UnmarshalFile provide JSON serialization and deserialization for File
// types, converting Go iota-based enums to human-readable string representations.
package ast

import (
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// Enum string mappings
// ---------------------------------------------------------------------------

var fieldTypeToStr = map[FieldType]string{
	FieldTypeString:      "string",
	FieldTypeBool:        "bool",
	FieldTypeInt:         "int",
	FieldTypeFloat:       "float",
	FieldTypeJSON:        "json",
	FieldTypeStringArray: "string[]",
}

var strToFieldType = reverseMap(fieldTypeToStr)

var sessionModeToStr = map[SessionMode]string{
	SessionFresh:         "fresh",
	SessionInherit:       "inherit",
	SessionArtifactsOnly: "artifacts_only",
	SessionFork:          "fork",
}

var strToSessionMode = reverseMap(sessionModeToStr)

var mcpTransportToStr = map[MCPTransport]string{
	MCPTransportUnknown: "unknown",
	MCPTransportStdio:   "stdio",
	MCPTransportHTTP:    "http",
	MCPTransportSSE:     "sse",
}

var strToMCPTransport = func() map[string]MCPTransport {
	m := reverseMap(mcpTransportToStr)
	m[""] = MCPTransportUnknown
	return m
}()

var routerModeToStr = map[RouterMode]string{
	RouterFanOutAll:  "fan_out_all",
	RouterCondition:  "condition",
	RouterRoundRobin: "round_robin",
	RouterLLM:        "llm",
}

var strToRouterMode = reverseMap(routerModeToStr)

var awaitModeToStr = map[AwaitMode]string{
	AwaitWaitAll:    "wait_all",
	AwaitBestEffort: "best_effort",
}

var strToAwaitMode = func() map[string]AwaitMode {
	m := reverseMap(awaitModeToStr)
	m["none"] = AwaitNone // accept "none" on input, but never emit it
	return m
}()

var interactionModeToStr = map[InteractionMode]string{
	InteractionNone:       "none",
	InteractionHuman:      "human",
	InteractionLLM:        "llm",
	InteractionLLMOrHuman: "llm_or_human",
}

var strToInteractionMode = reverseMap(interactionModeToStr)

var typeExprToStr = map[TypeExpr]string{
	TypeString:      "string",
	TypeBool:        "bool",
	TypeInt:         "int",
	TypeFloat:       "float",
	TypeJSON:        "json",
	TypeStringArray: "string[]",
}

var strToTypeExpr = reverseMap(typeExprToStr)

var literalKindToStr = map[LiteralKind]string{
	LitString: "string",
	LitInt:    "int",
	LitFloat:  "float",
	LitBool:   "bool",
}

var strToLiteralKind = reverseMap(literalKindToStr)

func reverseMap[K comparable, V comparable](m map[K]V) map[V]K {
	r := make(map[V]K, len(m))
	for k, v := range m {
		r[v] = k
	}
	return r
}

// ---------------------------------------------------------------------------
// JSON mirror structs
// ---------------------------------------------------------------------------

type jsonFile struct {
	Vars       *jsonVarsBlock       `json:"vars,omitempty"`
	MCPServers []*jsonMCPServerDecl `json:"mcp_servers,omitempty"`
	Prompts    []*jsonPromptDecl    `json:"prompts,omitempty"`
	Schemas    []*jsonSchemaDecl    `json:"schemas,omitempty"`
	Agents     []*jsonAgentDecl     `json:"agents,omitempty"`
	Judges     []*jsonJudgeDecl     `json:"judges,omitempty"`
	Routers    []*jsonRouterDecl    `json:"routers,omitempty"`
	Humans     []*jsonHumanDecl     `json:"humans,omitempty"`
	Tools      []*jsonToolNodeDecl  `json:"tools,omitempty"`
	Computes   []*jsonComputeDecl   `json:"computes,omitempty"`
	Workflows  []*jsonWorkflowDecl  `json:"workflows,omitempty"`
	Comments   []*jsonComment       `json:"comments,omitempty"`
}

type jsonComment struct {
	Text string `json:"text,omitempty"`
}

type jsonVarsBlock struct {
	Fields []*jsonVarField `json:"fields,omitempty"`
}

type jsonVarField struct {
	Name    string       `json:"name,omitempty"`
	Type    string       `json:"type,omitempty"`
	Default *jsonLiteral `json:"default,omitempty"`
}

type jsonLiteral struct {
	Kind     string  `json:"kind,omitempty"`
	Raw      string  `json:"raw,omitempty"`
	StrVal   string  `json:"str_val,omitempty"`
	IntVal   int64   `json:"int_val,omitempty"`
	FloatVal float64 `json:"float_val,omitempty"`
	BoolVal  bool    `json:"bool_val,omitempty"`
}

type jsonMCPServerDecl struct {
	Name      string           `json:"name,omitempty"`
	Transport string           `json:"transport,omitempty"`
	Command   string           `json:"command,omitempty"`
	Args      []string         `json:"args,omitempty"`
	URL       string           `json:"url,omitempty"`
	Auth      *jsonMCPAuthDecl `json:"auth,omitempty"`
}

type jsonMCPAuthDecl struct {
	Type      string   `json:"type,omitempty"`
	AuthURL   string   `json:"auth_url,omitempty"`
	TokenURL  string   `json:"token_url,omitempty"`
	RevokeURL string   `json:"revoke_url,omitempty"`
	ClientID  string   `json:"client_id,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

type jsonMCPConfigDecl struct {
	AutoloadProject *bool    `json:"autoload_project,omitempty"`
	Inherit         *bool    `json:"inherit,omitempty"`
	Servers         []string `json:"servers,omitempty"`
	Disable         []string `json:"disable,omitempty"`
}

type jsonCompactionBlock struct {
	Threshold      *float64 `json:"threshold,omitempty"`
	PreserveRecent *int     `json:"preserve_recent,omitempty"`
}

type jsonPromptDecl struct {
	Name string `json:"name,omitempty"`
	Body string `json:"body,omitempty"`
}

type jsonSchemaDecl struct {
	Name   string             `json:"name,omitempty"`
	Fields []*jsonSchemaField `json:"fields,omitempty"`
}

type jsonSchemaField struct {
	Name       string   `json:"name,omitempty"`
	Type       string   `json:"type,omitempty"`
	EnumValues []string `json:"enum_values,omitempty"`
}

type jsonAgentDecl struct {
	Name              string               `json:"name,omitempty"`
	Model             string               `json:"model,omitempty"`
	Backend           string               `json:"backend,omitempty"`
	MCP               *jsonMCPConfigDecl   `json:"mcp,omitempty"`
	Input             string               `json:"input,omitempty"`
	Output            string               `json:"output,omitempty"`
	Publish           string               `json:"publish,omitempty"`
	System            string               `json:"system,omitempty"`
	User              string               `json:"user,omitempty"`
	Session           string               `json:"session,omitempty"`
	Tools             []string             `json:"tools,omitempty"`
	ToolPolicy        []string             `json:"tool_policy,omitempty"`
	ToolMaxSteps      int                  `json:"tool_max_steps,omitempty"`
	MaxTokens         int                  `json:"max_tokens,omitempty"`
	ReasoningEffort   string               `json:"reasoning_effort,omitempty"`
	Readonly          bool                 `json:"readonly,omitempty"`
	Interaction       string               `json:"interaction,omitempty"`
	InteractionPrompt string               `json:"interaction_prompt,omitempty"`
	InteractionModel  string               `json:"interaction_model,omitempty"`
	Await             string               `json:"await,omitempty"`
	Compaction        *jsonCompactionBlock `json:"compaction,omitempty"`
}

type jsonJudgeDecl struct {
	Name              string               `json:"name,omitempty"`
	Model             string               `json:"model,omitempty"`
	Backend           string               `json:"backend,omitempty"`
	MCP               *jsonMCPConfigDecl   `json:"mcp,omitempty"`
	Input             string               `json:"input,omitempty"`
	Output            string               `json:"output,omitempty"`
	Publish           string               `json:"publish,omitempty"`
	System            string               `json:"system,omitempty"`
	User              string               `json:"user,omitempty"`
	Session           string               `json:"session,omitempty"`
	Tools             []string             `json:"tools,omitempty"`
	ToolPolicy        []string             `json:"tool_policy,omitempty"`
	ToolMaxSteps      int                  `json:"tool_max_steps,omitempty"`
	MaxTokens         int                  `json:"max_tokens,omitempty"`
	ReasoningEffort   string               `json:"reasoning_effort,omitempty"`
	Readonly          bool                 `json:"readonly,omitempty"`
	Interaction       string               `json:"interaction,omitempty"`
	InteractionPrompt string               `json:"interaction_prompt,omitempty"`
	InteractionModel  string               `json:"interaction_model,omitempty"`
	Await             string               `json:"await,omitempty"`
	Compaction        *jsonCompactionBlock `json:"compaction,omitempty"`
}

type jsonRouterDecl struct {
	Name            string `json:"name,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Model           string `json:"model,omitempty"`
	Backend         string `json:"backend,omitempty"`
	System          string `json:"system,omitempty"`
	User            string `json:"user,omitempty"`
	Multi           bool   `json:"multi,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type jsonHumanDecl struct {
	Name              string `json:"name,omitempty"`
	Input             string `json:"input,omitempty"`
	Output            string `json:"output,omitempty"`
	Publish           string `json:"publish,omitempty"`
	Instructions      string `json:"instructions,omitempty"`
	Interaction       string `json:"interaction,omitempty"`
	InteractionPrompt string `json:"interaction_prompt,omitempty"`
	InteractionModel  string `json:"interaction_model,omitempty"`
	MinAnswers        int    `json:"min_answers,omitempty"`
	Model             string `json:"model,omitempty"`
	System            string `json:"system,omitempty"`
	Await             string `json:"await,omitempty"`
}

type jsonToolNodeDecl struct {
	Name    string `json:"name,omitempty"`
	Command string `json:"command,omitempty"`
	Input   string `json:"input,omitempty"`
	Output  string `json:"output,omitempty"`
	Await   string `json:"await,omitempty"`
}

type jsonComputeDecl struct {
	Name   string             `json:"name,omitempty"`
	Input  string             `json:"input,omitempty"`
	Output string             `json:"output,omitempty"`
	Expr   []*jsonComputeExpr `json:"expr,omitempty"`
	Await  string             `json:"await,omitempty"`
}

type jsonComputeExpr struct {
	Key  string `json:"key,omitempty"`
	Expr string `json:"expr,omitempty"`
}

type jsonWorkflowDecl struct {
	Name           string               `json:"name,omitempty"`
	Vars           *jsonVarsBlock       `json:"vars,omitempty"`
	Entry          string               `json:"entry,omitempty"`
	DefaultBackend string               `json:"default_backend,omitempty"`
	ToolPolicy     []string             `json:"tool_policy,omitempty"`
	MCP            *jsonMCPConfigDecl   `json:"mcp,omitempty"`
	Budget         *jsonBudgetBlock     `json:"budget,omitempty"`
	Compaction     *jsonCompactionBlock `json:"compaction,omitempty"`
	Interaction    string               `json:"interaction,omitempty"`
	Worktree       string               `json:"worktree,omitempty"`
	Edges          []*jsonEdge          `json:"edges,omitempty"`
}

type jsonBudgetBlock struct {
	MaxParallelBranches int     `json:"max_parallel_branches,omitempty"`
	MaxDuration         string  `json:"max_duration,omitempty"`
	MaxCostUSD          float64 `json:"max_cost_usd,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	MaxIterations       int     `json:"max_iterations,omitempty"`
}

type jsonEdge struct {
	From string           `json:"from,omitempty"`
	To   string           `json:"to,omitempty"`
	When *jsonWhenClause  `json:"when,omitempty"`
	Loop *jsonLoopClause  `json:"loop,omitempty"`
	With []*jsonWithEntry `json:"with,omitempty"`
}

type jsonWhenClause struct {
	Condition string `json:"condition,omitempty"`
	Negated   bool   `json:"negated,omitempty"`
	Expr      string `json:"expr,omitempty"`
}

type jsonLoopClause struct {
	Name          string `json:"name,omitempty"`
	MaxIterations int    `json:"max_iterations,omitempty"`
}

type jsonWithEntry struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// ---------------------------------------------------------------------------
// Marshal
// ---------------------------------------------------------------------------

// Marshal converts an File to JSON with human-readable string enums.
// Span fields are omitted from the output.
func MarshalFile(f *File) ([]byte, error) {
	jf := toJSON(f)
	return json.MarshalIndent(jf, "", "  ")
}

func toJSON(f *File) *jsonFile {
	if f == nil {
		return nil
	}
	jf := &jsonFile{}

	if f.Vars != nil {
		jf.Vars = varsBlockToJSON(f.Vars)
	}

	for _, s := range f.MCPServers {
		jf.MCPServers = append(jf.MCPServers, mcpServerToJSON(s))
	}
	for _, p := range f.Prompts {
		jf.Prompts = append(jf.Prompts, &jsonPromptDecl{Name: p.Name, Body: p.Body})
	}
	for _, s := range f.Schemas {
		jf.Schemas = append(jf.Schemas, schemaToJSON(s))
	}
	for _, a := range f.Agents {
		jf.Agents = append(jf.Agents, agentToJSON(a))
	}
	for _, j := range f.Judges {
		jf.Judges = append(jf.Judges, judgeToJSON(j))
	}
	for _, r := range f.Routers {
		jf.Routers = append(jf.Routers, &jsonRouterDecl{
			Name:            r.Name,
			Mode:            routerModeToStr[r.Mode],
			Model:           r.Model,
			Backend:         r.Backend,
			System:          r.System,
			User:            r.User,
			Multi:           r.Multi,
			ReasoningEffort: r.ReasoningEffort,
		})
	}
	for _, h := range f.Humans {
		jf.Humans = append(jf.Humans, humanToJSON(h))
	}
	for _, t := range f.Tools {
		jf.Tools = append(jf.Tools, &jsonToolNodeDecl{
			Name:    t.Name,
			Command: t.Command,
			Input:   t.Input,
			Output:  t.Output,
			Await:   awaitModeToStr[t.Await],
		})
	}
	for _, c := range f.Computes {
		jc := &jsonComputeDecl{
			Name:   c.Name,
			Input:  c.Input,
			Output: c.Output,
			Await:  awaitModeToStr[c.Await],
		}
		for _, e := range c.Expr {
			jc.Expr = append(jc.Expr, &jsonComputeExpr{Key: e.Key, Expr: e.Expr})
		}
		jf.Computes = append(jf.Computes, jc)
	}
	for _, w := range f.Workflows {
		jf.Workflows = append(jf.Workflows, workflowToJSON(w))
	}
	for _, c := range f.Comments {
		jf.Comments = append(jf.Comments, &jsonComment{Text: c.Text})
	}

	return jf
}

func mcpServerToJSON(s *MCPServerDecl) *jsonMCPServerDecl {
	js := &jsonMCPServerDecl{
		Name:      s.Name,
		Transport: mcpTransportToStr[s.Transport],
		Command:   s.Command,
		Args:      s.Args,
		URL:       s.URL,
	}
	if s.Auth != nil {
		js.Auth = &jsonMCPAuthDecl{
			Type:      s.Auth.Type,
			AuthURL:   s.Auth.AuthURL,
			TokenURL:  s.Auth.TokenURL,
			RevokeURL: s.Auth.RevokeURL,
			ClientID:  s.Auth.ClientID,
			Scopes:    s.Auth.Scopes,
		}
	}
	return js
}

func mcpConfigToJSON(c *MCPConfigDecl) *jsonMCPConfigDecl {
	if c == nil {
		return nil
	}
	return &jsonMCPConfigDecl{
		AutoloadProject: c.AutoloadProject,
		Inherit:         c.Inherit,
		Servers:         c.Servers,
		Disable:         c.Disable,
	}
}

func compactionToJSON(c *CompactionBlock) *jsonCompactionBlock {
	if c == nil {
		return nil
	}
	return &jsonCompactionBlock{Threshold: c.Threshold, PreserveRecent: c.PreserveRecent}
}

func varsBlockToJSON(v *VarsBlock) *jsonVarsBlock {
	jv := &jsonVarsBlock{}
	for _, f := range v.Fields {
		jf := &jsonVarField{
			Name: f.Name,
			Type: typeExprToStr[f.Type],
		}
		if f.Default != nil {
			jf.Default = literalToJSON(f.Default)
		}
		jv.Fields = append(jv.Fields, jf)
	}
	return jv
}

func literalToJSON(l *Literal) *jsonLiteral {
	return &jsonLiteral{
		Kind:     literalKindToStr[l.Kind],
		Raw:      l.Raw,
		StrVal:   l.StrVal,
		IntVal:   l.IntVal,
		FloatVal: l.FloatVal,
		BoolVal:  l.BoolVal,
	}
}

func schemaToJSON(s *SchemaDecl) *jsonSchemaDecl {
	js := &jsonSchemaDecl{Name: s.Name}
	for _, f := range s.Fields {
		js.Fields = append(js.Fields, &jsonSchemaField{
			Name:       f.Name,
			Type:       fieldTypeToStr[f.Type],
			EnumValues: f.EnumValues,
		})
	}
	return js
}

func agentToJSON(a *AgentDecl) *jsonAgentDecl {
	return &jsonAgentDecl{
		Name:              a.Name,
		Model:             a.Model,
		Backend:           a.Backend,
		MCP:               mcpConfigToJSON(a.MCP),
		Input:             a.Input,
		Output:            a.Output,
		Publish:           a.Publish,
		System:            a.System,
		User:              a.User,
		Session:           sessionModeToStr[a.Session],
		Tools:             a.Tools,
		ToolPolicy:        a.ToolPolicy,
		ToolMaxSteps:      a.ToolMaxSteps,
		MaxTokens:         a.MaxTokens,
		ReasoningEffort:   a.ReasoningEffort,
		Readonly:          a.Readonly,
		Interaction:       interactionModeToStr[a.Interaction],
		InteractionPrompt: a.InteractionPrompt,
		InteractionModel:  a.InteractionModel,
		Await:             awaitModeToStr[a.Await],
		Compaction:        compactionToJSON(a.Compaction),
	}
}

func judgeToJSON(j *JudgeDecl) *jsonJudgeDecl {
	return &jsonJudgeDecl{
		Name:              j.Name,
		Model:             j.Model,
		Backend:           j.Backend,
		MCP:               mcpConfigToJSON(j.MCP),
		Input:             j.Input,
		Output:            j.Output,
		Publish:           j.Publish,
		System:            j.System,
		User:              j.User,
		Session:           sessionModeToStr[j.Session],
		Tools:             j.Tools,
		ToolPolicy:        j.ToolPolicy,
		ToolMaxSteps:      j.ToolMaxSteps,
		MaxTokens:         j.MaxTokens,
		ReasoningEffort:   j.ReasoningEffort,
		Readonly:          j.Readonly,
		Interaction:       interactionModeToStr[j.Interaction],
		InteractionPrompt: j.InteractionPrompt,
		InteractionModel:  j.InteractionModel,
		Await:             awaitModeToStr[j.Await],
		Compaction:        compactionToJSON(j.Compaction),
	}
}

func humanToJSON(h *HumanDecl) *jsonHumanDecl {
	return &jsonHumanDecl{
		Name:              h.Name,
		Input:             h.Input,
		Output:            h.Output,
		Publish:           h.Publish,
		Instructions:      h.Instructions,
		Interaction:       interactionModeToStr[h.Interaction],
		InteractionPrompt: h.InteractionPrompt,
		InteractionModel:  h.InteractionModel,
		MinAnswers:        h.MinAnswers,
		Model:             h.Model,
		System:            h.System,
		Await:             awaitModeToStr[h.Await],
	}
}

func workflowToJSON(w *WorkflowDecl) *jsonWorkflowDecl {
	jw := &jsonWorkflowDecl{
		Name:           w.Name,
		Entry:          w.Entry,
		DefaultBackend: w.DefaultBackend,
		ToolPolicy:     w.ToolPolicy,
		MCP:            mcpConfigToJSON(w.MCP),
		Compaction:     compactionToJSON(w.Compaction),
		Worktree:       w.Worktree,
	}
	if w.Vars != nil {
		jw.Vars = varsBlockToJSON(w.Vars)
	}
	if w.Interaction != nil {
		jw.Interaction = interactionModeToStr[*w.Interaction]
	}
	if w.Budget != nil {
		jw.Budget = &jsonBudgetBlock{
			MaxParallelBranches: w.Budget.MaxParallelBranches,
			MaxDuration:         w.Budget.MaxDuration,
			MaxCostUSD:          w.Budget.MaxCostUSD,
			MaxTokens:           w.Budget.MaxTokens,
			MaxIterations:       w.Budget.MaxIterations,
		}
	}
	for _, e := range w.Edges {
		jw.Edges = append(jw.Edges, edgeToJSON(e))
	}
	return jw
}

func edgeToJSON(e *Edge) *jsonEdge {
	je := &jsonEdge{
		From: e.From,
		To:   e.To,
	}
	if e.When != nil {
		je.When = &jsonWhenClause{
			Condition: e.When.Condition,
			Negated:   e.When.Negated,
			Expr:      e.When.Expr,
		}
	}
	if e.Loop != nil {
		je.Loop = &jsonLoopClause{
			Name:          e.Loop.Name,
			MaxIterations: e.Loop.MaxIterations,
		}
	}
	for _, w := range e.With {
		je.With = append(je.With, &jsonWithEntry{
			Key:   w.Key,
			Value: w.Value,
		})
	}
	return je
}

// ---------------------------------------------------------------------------
// Unmarshal
// ---------------------------------------------------------------------------

// Unmarshal converts JSON (produced by Marshal) back to an File.
func UnmarshalFile(data []byte) (*File, error) {
	var jf jsonFile
	if err := json.Unmarshal(data, &jf); err != nil {
		return nil, fmt.Errorf("astjson: %w", err)
	}
	return fromJSON(&jf)
}

func fromJSON(jf *jsonFile) (*File, error) {
	f := &File{}

	if jf.Vars != nil {
		v, err := varsBlockFromJSON(jf.Vars)
		if err != nil {
			return nil, err
		}
		f.Vars = v
	}

	for _, js := range jf.MCPServers {
		s, err := mcpServerFromJSON(js)
		if err != nil {
			return nil, err
		}
		f.MCPServers = append(f.MCPServers, s)
	}

	for _, jp := range jf.Prompts {
		f.Prompts = append(f.Prompts, &PromptDecl{Name: jp.Name, Body: jp.Body})
	}

	for _, js := range jf.Schemas {
		s, err := schemaFromJSON(js)
		if err != nil {
			return nil, err
		}
		f.Schemas = append(f.Schemas, s)
	}

	for _, ja := range jf.Agents {
		a, err := agentFromJSON(ja)
		if err != nil {
			return nil, err
		}
		f.Agents = append(f.Agents, a)
	}

	for _, jj := range jf.Judges {
		j, err := judgeFromJSON(jj)
		if err != nil {
			return nil, err
		}
		f.Judges = append(f.Judges, j)
	}

	for _, jr := range jf.Routers {
		mode, ok := strToRouterMode[jr.Mode]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown router mode %q", jr.Mode)
		}
		f.Routers = append(f.Routers, &RouterDecl{
			Name:            jr.Name,
			Mode:            mode,
			Model:           jr.Model,
			Backend:         jr.Backend,
			System:          jr.System,
			User:            jr.User,
			Multi:           jr.Multi,
			ReasoningEffort: jr.ReasoningEffort,
		})
	}

	for _, jh := range jf.Humans {
		h, err := humanFromJSON(jh)
		if err != nil {
			return nil, err
		}
		f.Humans = append(f.Humans, h)
	}

	for _, jt := range jf.Tools {
		aw, ok := strToAwaitMode[jt.Await]
		if jt.Await != "" && !ok {
			return nil, fmt.Errorf("astjson: unknown await mode %q", jt.Await)
		}
		f.Tools = append(f.Tools, &ToolNodeDecl{
			Name:    jt.Name,
			Command: jt.Command,
			Input:   jt.Input,
			Output:  jt.Output,
			Await:   aw,
		})
	}

	for _, jc := range jf.Computes {
		aw, ok := strToAwaitMode[jc.Await]
		if jc.Await != "" && !ok {
			return nil, fmt.Errorf("astjson: unknown await mode %q", jc.Await)
		}
		cd := &ComputeDecl{
			Name:   jc.Name,
			Input:  jc.Input,
			Output: jc.Output,
			Await:  aw,
		}
		for _, je := range jc.Expr {
			cd.Expr = append(cd.Expr, &ComputeExpr{Key: je.Key, Expr: je.Expr})
		}
		f.Computes = append(f.Computes, cd)
	}

	for _, jw := range jf.Workflows {
		w, err := workflowFromJSON(jw)
		if err != nil {
			return nil, err
		}
		f.Workflows = append(f.Workflows, w)
	}

	for _, jc := range jf.Comments {
		f.Comments = append(f.Comments, &Comment{Text: jc.Text})
	}

	return f, nil
}

func mcpServerFromJSON(js *jsonMCPServerDecl) (*MCPServerDecl, error) {
	transport, ok := strToMCPTransport[js.Transport]
	if js.Transport != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown mcp transport %q", js.Transport)
	}
	s := &MCPServerDecl{
		Name:      js.Name,
		Transport: transport,
		Command:   js.Command,
		Args:      js.Args,
		URL:       js.URL,
	}
	if js.Auth != nil {
		s.Auth = &MCPAuthDecl{
			Type:      js.Auth.Type,
			AuthURL:   js.Auth.AuthURL,
			TokenURL:  js.Auth.TokenURL,
			RevokeURL: js.Auth.RevokeURL,
			ClientID:  js.Auth.ClientID,
			Scopes:    js.Auth.Scopes,
		}
	}
	return s, nil
}

func mcpConfigFromJSON(jc *jsonMCPConfigDecl) *MCPConfigDecl {
	if jc == nil {
		return nil
	}
	return &MCPConfigDecl{
		AutoloadProject: jc.AutoloadProject,
		Inherit:         jc.Inherit,
		Servers:         jc.Servers,
		Disable:         jc.Disable,
	}
}

func compactionFromJSON(jc *jsonCompactionBlock) *CompactionBlock {
	if jc == nil {
		return nil
	}
	return &CompactionBlock{Threshold: jc.Threshold, PreserveRecent: jc.PreserveRecent}
}

func varsBlockFromJSON(jv *jsonVarsBlock) (*VarsBlock, error) {
	v := &VarsBlock{}
	for _, jf := range jv.Fields {
		te, ok := strToTypeExpr[jf.Type]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown type %q", jf.Type)
		}
		vf := &VarField{Name: jf.Name, Type: te}
		if jf.Default != nil {
			l, err := literalFromJSON(jf.Default)
			if err != nil {
				return nil, err
			}
			vf.Default = l
		}
		v.Fields = append(v.Fields, vf)
	}
	return v, nil
}

func literalFromJSON(jl *jsonLiteral) (*Literal, error) {
	kind, ok := strToLiteralKind[jl.Kind]
	if !ok {
		return nil, fmt.Errorf("astjson: unknown literal kind %q", jl.Kind)
	}
	return &Literal{
		Kind:     kind,
		Raw:      jl.Raw,
		StrVal:   jl.StrVal,
		IntVal:   jl.IntVal,
		FloatVal: jl.FloatVal,
		BoolVal:  jl.BoolVal,
	}, nil
}

func schemaFromJSON(js *jsonSchemaDecl) (*SchemaDecl, error) {
	s := &SchemaDecl{Name: js.Name}
	for _, jf := range js.Fields {
		ft, ok := strToFieldType[jf.Type]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown field type %q", jf.Type)
		}
		s.Fields = append(s.Fields, &SchemaField{
			Name:       jf.Name,
			Type:       ft,
			EnumValues: jf.EnumValues,
		})
	}
	return s, nil
}

func agentFromJSON(ja *jsonAgentDecl) (*AgentDecl, error) {
	sess, ok := strToSessionMode[ja.Session]
	if ja.Session != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown session mode %q", ja.Session)
	}
	aw, ok := strToAwaitMode[ja.Await]
	if ja.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", ja.Await)
	}
	interaction, ok := strToInteractionMode[ja.Interaction]
	if ja.Interaction != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown interaction mode %q", ja.Interaction)
	}
	return &AgentDecl{
		Name:              ja.Name,
		Model:             ja.Model,
		Backend:           ja.Backend,
		MCP:               mcpConfigFromJSON(ja.MCP),
		Input:             ja.Input,
		Output:            ja.Output,
		Publish:           ja.Publish,
		System:            ja.System,
		User:              ja.User,
		Session:           sess,
		Tools:             ja.Tools,
		ToolPolicy:        ja.ToolPolicy,
		ToolMaxSteps:      ja.ToolMaxSteps,
		MaxTokens:         ja.MaxTokens,
		ReasoningEffort:   ja.ReasoningEffort,
		Readonly:          ja.Readonly,
		Interaction:       interaction,
		InteractionPrompt: ja.InteractionPrompt,
		InteractionModel:  ja.InteractionModel,
		Await:             aw,
		Compaction:        compactionFromJSON(ja.Compaction),
	}, nil
}

func judgeFromJSON(jj *jsonJudgeDecl) (*JudgeDecl, error) {
	sess, ok := strToSessionMode[jj.Session]
	if jj.Session != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown session mode %q", jj.Session)
	}
	aw, ok := strToAwaitMode[jj.Await]
	if jj.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", jj.Await)
	}
	interaction, ok := strToInteractionMode[jj.Interaction]
	if jj.Interaction != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown interaction mode %q", jj.Interaction)
	}
	return &JudgeDecl{
		Name:              jj.Name,
		Model:             jj.Model,
		Backend:           jj.Backend,
		MCP:               mcpConfigFromJSON(jj.MCP),
		Input:             jj.Input,
		Output:            jj.Output,
		Publish:           jj.Publish,
		System:            jj.System,
		User:              jj.User,
		Session:           sess,
		Tools:             jj.Tools,
		ToolPolicy:        jj.ToolPolicy,
		ToolMaxSteps:      jj.ToolMaxSteps,
		MaxTokens:         jj.MaxTokens,
		ReasoningEffort:   jj.ReasoningEffort,
		Readonly:          jj.Readonly,
		Interaction:       interaction,
		InteractionPrompt: jj.InteractionPrompt,
		InteractionModel:  jj.InteractionModel,
		Await:             aw,
		Compaction:        compactionFromJSON(jj.Compaction),
	}, nil
}

func humanFromJSON(jh *jsonHumanDecl) (*HumanDecl, error) {
	interactionStr := jh.Interaction
	interaction, ok := strToInteractionMode[interactionStr]
	if interactionStr != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown interaction mode %q", interactionStr)
	}
	if interactionStr == "" {
		interaction = InteractionHuman // default for human nodes
	}
	return humanFromJSONWithInteraction(jh, interaction)
}

func humanFromJSONWithInteraction(jh *jsonHumanDecl, interaction InteractionMode) (*HumanDecl, error) {
	aw, ok := strToAwaitMode[jh.Await]
	if jh.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", jh.Await)
	}
	return &HumanDecl{
		Name:              jh.Name,
		Input:             jh.Input,
		Output:            jh.Output,
		Publish:           jh.Publish,
		Instructions:      jh.Instructions,
		Interaction:       interaction,
		InteractionPrompt: jh.InteractionPrompt,
		InteractionModel:  jh.InteractionModel,
		MinAnswers:        jh.MinAnswers,
		Model:             jh.Model,
		System:            jh.System,
		Await:             aw,
	}, nil
}

func workflowFromJSON(jw *jsonWorkflowDecl) (*WorkflowDecl, error) {
	w := &WorkflowDecl{
		Name:           jw.Name,
		Entry:          jw.Entry,
		DefaultBackend: jw.DefaultBackend,
		ToolPolicy:     jw.ToolPolicy,
		MCP:            mcpConfigFromJSON(jw.MCP),
		Compaction:     compactionFromJSON(jw.Compaction),
		Worktree:       jw.Worktree,
	}
	if jw.Vars != nil {
		v, err := varsBlockFromJSON(jw.Vars)
		if err != nil {
			return nil, err
		}
		w.Vars = v
	}
	if jw.Interaction != "" {
		interaction, ok := strToInteractionMode[jw.Interaction]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown interaction mode %q", jw.Interaction)
		}
		w.Interaction = &interaction
	}
	if jw.Budget != nil {
		w.Budget = &BudgetBlock{
			MaxParallelBranches: jw.Budget.MaxParallelBranches,
			MaxDuration:         jw.Budget.MaxDuration,
			MaxCostUSD:          jw.Budget.MaxCostUSD,
			MaxTokens:           jw.Budget.MaxTokens,
			MaxIterations:       jw.Budget.MaxIterations,
		}
	}
	for _, je := range jw.Edges {
		e, err := edgeFromJSON(je)
		if err != nil {
			return nil, err
		}
		w.Edges = append(w.Edges, e)
	}
	return w, nil
}

func edgeFromJSON(je *jsonEdge) (*Edge, error) {
	e := &Edge{
		From: je.From,
		To:   je.To,
	}
	if je.When != nil {
		e.When = &WhenClause{
			Condition: je.When.Condition,
			Negated:   je.When.Negated,
			Expr:      je.When.Expr,
		}
	}
	if je.Loop != nil {
		e.Loop = &LoopClause{
			Name:          je.Loop.Name,
			MaxIterations: je.Loop.MaxIterations,
		}
	}
	for _, jw := range je.With {
		e.With = append(e.With, &WithEntry{
			Key:   jw.Key,
			Value: jw.Value,
		})
	}
	return e, nil
}
