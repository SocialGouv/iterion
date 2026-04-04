// Package astjson provides JSON serialization and deserialization for ast.File
// types, converting Go iota-based enums to human-readable string representations.
package astjson

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/ast"
)

// ---------------------------------------------------------------------------
// Enum string mappings
// ---------------------------------------------------------------------------

var fieldTypeToStr = map[ast.FieldType]string{
	ast.FieldTypeString:      "string",
	ast.FieldTypeBool:        "bool",
	ast.FieldTypeInt:         "int",
	ast.FieldTypeFloat:       "float",
	ast.FieldTypeJSON:        "json",
	ast.FieldTypeStringArray: "string[]",
}

var strToFieldType = reverseMap(fieldTypeToStr)

var sessionModeToStr = map[ast.SessionMode]string{
	ast.SessionFresh:         "fresh",
	ast.SessionInherit:       "inherit",
	ast.SessionArtifactsOnly: "artifacts_only",
}

var strToSessionMode = reverseMap(sessionModeToStr)

var routerModeToStr = map[ast.RouterMode]string{
	ast.RouterFanOutAll:  "fan_out_all",
	ast.RouterCondition:  "condition",
	ast.RouterRoundRobin: "round_robin",
	ast.RouterLLM:        "llm",
}

var strToRouterMode = reverseMap(routerModeToStr)

var awaitModeToStr = map[ast.AwaitMode]string{
	ast.AwaitWaitAll:    "wait_all",
	ast.AwaitBestEffort: "best_effort",
}

var strToAwaitMode = func() map[string]ast.AwaitMode {
	m := reverseMap(awaitModeToStr)
	m["none"] = ast.AwaitNone // accept "none" on input, but never emit it
	return m
}()

var humanModeToStr = map[ast.HumanMode]string{
	ast.HumanPauseUntilAnswers: "pause_until_answers",
	ast.HumanAutoAnswer:        "auto_answer",
	ast.HumanAutoOrPause:       "auto_or_pause",
}

var strToHumanMode = reverseMap(humanModeToStr)

var typeExprToStr = map[ast.TypeExpr]string{
	ast.TypeString:      "string",
	ast.TypeBool:        "bool",
	ast.TypeInt:         "int",
	ast.TypeFloat:       "float",
	ast.TypeJSON:        "json",
	ast.TypeStringArray: "string[]",
}

var strToTypeExpr = reverseMap(typeExprToStr)

var literalKindToStr = map[ast.LiteralKind]string{
	ast.LitString: "string",
	ast.LitInt:    "int",
	ast.LitFloat:  "float",
	ast.LitBool:   "bool",
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
	Vars      *jsonVarsBlock      `json:"vars,omitempty"`
	Prompts   []*jsonPromptDecl   `json:"prompts,omitempty"`
	Schemas   []*jsonSchemaDecl   `json:"schemas,omitempty"`
	Agents    []*jsonAgentDecl    `json:"agents,omitempty"`
	Judges    []*jsonJudgeDecl    `json:"judges,omitempty"`
	Routers   []*jsonRouterDecl   `json:"routers,omitempty"`
	Humans    []*jsonHumanDecl    `json:"humans,omitempty"`
	Tools     []*jsonToolNodeDecl `json:"tools,omitempty"`
	Workflows []*jsonWorkflowDecl `json:"workflows,omitempty"`
	Comments  []*jsonComment      `json:"comments,omitempty"`
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
	Name            string   `json:"name,omitempty"`
	Model           string   `json:"model,omitempty"`
	Delegate        string   `json:"delegate,omitempty"`
	Input           string   `json:"input,omitempty"`
	Output          string   `json:"output,omitempty"`
	Publish         string   `json:"publish,omitempty"`
	System          string   `json:"system,omitempty"`
	User            string   `json:"user,omitempty"`
	Session         string   `json:"session,omitempty"`
	Tools           []string `json:"tools,omitempty"`
	ToolMaxSteps    int      `json:"tool_max_steps,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Await           string   `json:"await,omitempty"`
}

type jsonJudgeDecl struct {
	Name            string   `json:"name,omitempty"`
	Model           string   `json:"model,omitempty"`
	Delegate        string   `json:"delegate,omitempty"`
	Input           string   `json:"input,omitempty"`
	Output          string   `json:"output,omitempty"`
	Publish         string   `json:"publish,omitempty"`
	System          string   `json:"system,omitempty"`
	User            string   `json:"user,omitempty"`
	Session         string   `json:"session,omitempty"`
	Tools           []string `json:"tools,omitempty"`
	ToolMaxSteps    int      `json:"tool_max_steps,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Await           string   `json:"await,omitempty"`
}

type jsonRouterDecl struct {
	Name   string `json:"name,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Model  string `json:"model,omitempty"`
	System string `json:"system,omitempty"`
	User   string `json:"user,omitempty"`
	Multi  bool   `json:"multi,omitempty"`
}

type jsonHumanDecl struct {
	Name         string `json:"name,omitempty"`
	Input        string `json:"input,omitempty"`
	Output       string `json:"output,omitempty"`
	Publish      string `json:"publish,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Mode         string `json:"mode,omitempty"`
	MinAnswers   int    `json:"min_answers,omitempty"`
	Model        string `json:"model,omitempty"`
	System       string `json:"system,omitempty"`
	Await        string `json:"await,omitempty"`
}

type jsonToolNodeDecl struct {
	Name    string `json:"name,omitempty"`
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	Await   string `json:"await,omitempty"`
}

type jsonWorkflowDecl struct {
	Name   string           `json:"name,omitempty"`
	Vars   *jsonVarsBlock   `json:"vars,omitempty"`
	Entry  string           `json:"entry,omitempty"`
	Budget *jsonBudgetBlock `json:"budget,omitempty"`
	Edges  []*jsonEdge      `json:"edges,omitempty"`
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

// Marshal converts an ast.File to JSON with human-readable string enums.
// Span fields are omitted from the output.
func Marshal(f *ast.File) ([]byte, error) {
	jf := toJSON(f)
	return json.MarshalIndent(jf, "", "  ")
}

func toJSON(f *ast.File) *jsonFile {
	if f == nil {
		return nil
	}
	jf := &jsonFile{}

	if f.Vars != nil {
		jf.Vars = varsBlockToJSON(f.Vars)
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
			Name:   r.Name,
			Mode:   routerModeToStr[r.Mode],
			Model:  r.Model,
			System: r.System,
			User:   r.User,
			Multi:  r.Multi,
		})
	}
	for _, h := range f.Humans {
		jf.Humans = append(jf.Humans, humanToJSON(h))
	}
	for _, t := range f.Tools {
		jf.Tools = append(jf.Tools, &jsonToolNodeDecl{
			Name:    t.Name,
			Command: t.Command,
			Output:  t.Output,
			Await:   awaitModeToStr[t.Await],
		})
	}
	for _, w := range f.Workflows {
		jf.Workflows = append(jf.Workflows, workflowToJSON(w))
	}
	for _, c := range f.Comments {
		jf.Comments = append(jf.Comments, &jsonComment{Text: c.Text})
	}

	return jf
}

func varsBlockToJSON(v *ast.VarsBlock) *jsonVarsBlock {
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

func literalToJSON(l *ast.Literal) *jsonLiteral {
	return &jsonLiteral{
		Kind:     literalKindToStr[l.Kind],
		Raw:      l.Raw,
		StrVal:   l.StrVal,
		IntVal:   l.IntVal,
		FloatVal: l.FloatVal,
		BoolVal:  l.BoolVal,
	}
}

func schemaToJSON(s *ast.SchemaDecl) *jsonSchemaDecl {
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

func agentToJSON(a *ast.AgentDecl) *jsonAgentDecl {
	return &jsonAgentDecl{
		Name:            a.Name,
		Model:           a.Model,
		Delegate:        a.Delegate,
		Input:           a.Input,
		Output:          a.Output,
		Publish:         a.Publish,
		System:          a.System,
		User:            a.User,
		Session:         sessionModeToStr[a.Session],
		Tools:           a.Tools,
		ToolMaxSteps:    a.ToolMaxSteps,
		ReasoningEffort: a.ReasoningEffort,
		Await:           awaitModeToStr[a.Await],
	}
}

func judgeToJSON(j *ast.JudgeDecl) *jsonJudgeDecl {
	return &jsonJudgeDecl{
		Name:            j.Name,
		Model:           j.Model,
		Delegate:        j.Delegate,
		Input:           j.Input,
		Output:          j.Output,
		Publish:         j.Publish,
		System:          j.System,
		User:            j.User,
		Session:         sessionModeToStr[j.Session],
		Tools:           j.Tools,
		ToolMaxSteps:    j.ToolMaxSteps,
		ReasoningEffort: j.ReasoningEffort,
		Await:           awaitModeToStr[j.Await],
	}
}

func humanToJSON(h *ast.HumanDecl) *jsonHumanDecl {
	return &jsonHumanDecl{
		Name:         h.Name,
		Input:        h.Input,
		Output:       h.Output,
		Publish:      h.Publish,
		Instructions: h.Instructions,
		Mode:         humanModeToStr[h.Mode],
		MinAnswers:   h.MinAnswers,
		Model:        h.Model,
		System:       h.System,
		Await:        awaitModeToStr[h.Await],
	}
}

func workflowToJSON(w *ast.WorkflowDecl) *jsonWorkflowDecl {
	jw := &jsonWorkflowDecl{
		Name:  w.Name,
		Entry: w.Entry,
	}
	if w.Vars != nil {
		jw.Vars = varsBlockToJSON(w.Vars)
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

func edgeToJSON(e *ast.Edge) *jsonEdge {
	je := &jsonEdge{
		From: e.From,
		To:   e.To,
	}
	if e.When != nil {
		je.When = &jsonWhenClause{
			Condition: e.When.Condition,
			Negated:   e.When.Negated,
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

// Unmarshal converts JSON (produced by Marshal) back to an ast.File.
func Unmarshal(data []byte) (*ast.File, error) {
	var jf jsonFile
	if err := json.Unmarshal(data, &jf); err != nil {
		return nil, fmt.Errorf("astjson: %w", err)
	}
	return fromJSON(&jf)
}

func fromJSON(jf *jsonFile) (*ast.File, error) {
	f := &ast.File{}

	if jf.Vars != nil {
		v, err := varsBlockFromJSON(jf.Vars)
		if err != nil {
			return nil, err
		}
		f.Vars = v
	}

	for _, jp := range jf.Prompts {
		f.Prompts = append(f.Prompts, &ast.PromptDecl{Name: jp.Name, Body: jp.Body})
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
		f.Routers = append(f.Routers, &ast.RouterDecl{
			Name:   jr.Name,
			Mode:   mode,
			Model:  jr.Model,
			System: jr.System,
			User:   jr.User,
			Multi:  jr.Multi,
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
		f.Tools = append(f.Tools, &ast.ToolNodeDecl{
			Name:    jt.Name,
			Command: jt.Command,
			Output:  jt.Output,
			Await:   aw,
		})
	}

	for _, jw := range jf.Workflows {
		w, err := workflowFromJSON(jw)
		if err != nil {
			return nil, err
		}
		f.Workflows = append(f.Workflows, w)
	}

	for _, jc := range jf.Comments {
		f.Comments = append(f.Comments, &ast.Comment{Text: jc.Text})
	}

	return f, nil
}

func varsBlockFromJSON(jv *jsonVarsBlock) (*ast.VarsBlock, error) {
	v := &ast.VarsBlock{}
	for _, jf := range jv.Fields {
		te, ok := strToTypeExpr[jf.Type]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown type %q", jf.Type)
		}
		vf := &ast.VarField{Name: jf.Name, Type: te}
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

func literalFromJSON(jl *jsonLiteral) (*ast.Literal, error) {
	kind, ok := strToLiteralKind[jl.Kind]
	if !ok {
		return nil, fmt.Errorf("astjson: unknown literal kind %q", jl.Kind)
	}
	return &ast.Literal{
		Kind:     kind,
		Raw:      jl.Raw,
		StrVal:   jl.StrVal,
		IntVal:   jl.IntVal,
		FloatVal: jl.FloatVal,
		BoolVal:  jl.BoolVal,
	}, nil
}

func schemaFromJSON(js *jsonSchemaDecl) (*ast.SchemaDecl, error) {
	s := &ast.SchemaDecl{Name: js.Name}
	for _, jf := range js.Fields {
		ft, ok := strToFieldType[jf.Type]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown field type %q", jf.Type)
		}
		s.Fields = append(s.Fields, &ast.SchemaField{
			Name:       jf.Name,
			Type:       ft,
			EnumValues: jf.EnumValues,
		})
	}
	return s, nil
}

func agentFromJSON(ja *jsonAgentDecl) (*ast.AgentDecl, error) {
	sess, ok := strToSessionMode[ja.Session]
	if ja.Session != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown session mode %q", ja.Session)
	}
	aw, ok := strToAwaitMode[ja.Await]
	if ja.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", ja.Await)
	}
	return &ast.AgentDecl{
		Name:            ja.Name,
		Model:           ja.Model,
		Delegate:        ja.Delegate,
		Input:           ja.Input,
		Output:          ja.Output,
		Publish:         ja.Publish,
		System:          ja.System,
		User:            ja.User,
		Session:         sess,
		Tools:           ja.Tools,
		ToolMaxSteps:    ja.ToolMaxSteps,
		ReasoningEffort: ja.ReasoningEffort,
		Await:           aw,
	}, nil
}

func judgeFromJSON(jj *jsonJudgeDecl) (*ast.JudgeDecl, error) {
	sess, ok := strToSessionMode[jj.Session]
	if jj.Session != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown session mode %q", jj.Session)
	}
	aw, ok := strToAwaitMode[jj.Await]
	if jj.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", jj.Await)
	}
	return &ast.JudgeDecl{
		Name:            jj.Name,
		Model:           jj.Model,
		Delegate:        jj.Delegate,
		Input:           jj.Input,
		Output:          jj.Output,
		Publish:         jj.Publish,
		System:          jj.System,
		User:            jj.User,
		Session:         sess,
		Tools:           jj.Tools,
		ToolMaxSteps:    jj.ToolMaxSteps,
		ReasoningEffort: jj.ReasoningEffort,
		Await:           aw,
	}, nil
}

func humanFromJSON(jh *jsonHumanDecl) (*ast.HumanDecl, error) {
	mode, ok := strToHumanMode[jh.Mode]
	if jh.Mode != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown human mode %q", jh.Mode)
	}
	aw, ok := strToAwaitMode[jh.Await]
	if jh.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", jh.Await)
	}
	return &ast.HumanDecl{
		Name:         jh.Name,
		Input:        jh.Input,
		Output:       jh.Output,
		Publish:      jh.Publish,
		Instructions: jh.Instructions,
		Mode:         mode,
		MinAnswers:   jh.MinAnswers,
		Model:        jh.Model,
		System:       jh.System,
		Await:        aw,
	}, nil
}

func workflowFromJSON(jw *jsonWorkflowDecl) (*ast.WorkflowDecl, error) {
	w := &ast.WorkflowDecl{
		Name:  jw.Name,
		Entry: jw.Entry,
	}
	if jw.Vars != nil {
		v, err := varsBlockFromJSON(jw.Vars)
		if err != nil {
			return nil, err
		}
		w.Vars = v
	}
	if jw.Budget != nil {
		w.Budget = &ast.BudgetBlock{
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

func edgeFromJSON(je *jsonEdge) (*ast.Edge, error) {
	e := &ast.Edge{
		From: je.From,
		To:   je.To,
	}
	if je.When != nil {
		e.When = &ast.WhenClause{
			Condition: je.When.Condition,
			Negated:   je.When.Negated,
		}
	}
	if je.Loop != nil {
		e.Loop = &ast.LoopClause{
			Name:          je.Loop.Name,
			MaxIterations: je.Loop.MaxIterations,
		}
	}
	for _, jw := range je.With {
		e.With = append(e.With, &ast.WithEntry{
			Key:   jw.Key,
			Value: jw.Value,
		})
	}
	return e, nil
}
