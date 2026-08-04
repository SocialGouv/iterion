package ast_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// buildTestFile creates a comprehensive ast.File with at least one of each node type.
func buildTestFile() *ast.File {
	return &ast.File{
		Vars: &ast.VarsBlock{
			Fields: []*ast.VarField{
				{
					Name: "project_name",
					Type: ast.TypeString,
					Default: &ast.Literal{
						Kind:   ast.LitString,
						Raw:    `"my-project"`,
						StrVal: "my-project",
					},
				},
				{
					Name: "max_retries",
					Type: ast.TypeInt,
					Default: &ast.Literal{
						Kind:   ast.LitInt,
						Raw:    "3",
						IntVal: 3,
					},
				},
				{
					Name: "threshold",
					Type: ast.TypeFloat,
				},
				{
					Name: "verbose",
					Type: ast.TypeBool,
					Default: &ast.Literal{
						Kind:    ast.LitBool,
						Raw:     "true",
						BoolVal: true,
					},
				},
				{
					Name: "config",
					Type: ast.TypeJSON,
				},
				{
					Name: "tags",
					Type: ast.TypeStringArray,
				},
			},
		},
		Prompts: []*ast.PromptDecl{
			{Name: "system_prompt", Body: "You are a helpful assistant. Project: {{project_name}}"},
		},
		Schemas: []*ast.SchemaDecl{
			{
				Name: "review_output",
				Fields: []*ast.SchemaField{
					{Name: "verdict", Type: ast.FieldTypeString, EnumValues: []string{"approved", "rejected", "needs_work"}},
					{Name: "score", Type: ast.FieldTypeInt},
					{Name: "details", Type: ast.FieldTypeJSON},
					{Name: "tags", Type: ast.FieldTypeStringArray},
					{Name: "confidence", Type: ast.FieldTypeFloat},
					{Name: "passed", Type: ast.FieldTypeBool},
				},
			},
		},
		Agents: []*ast.AgentDecl{
			{
				Name: "coder",
				LLMDecl: ast.LLMDecl{
					Model:           "claude-sonnet-4-20250514",
					Input:           "task_input",
					Output:          "code_output",
					Publish:         "code_artifact",
					System:          "system_prompt",
					User:            "user_prompt",
					Session:         ast.SessionInherit,
					Tools:           []string{"read_file", "write_file"},
					ToolMaxSteps:    10,
					ReasoningEffort: "high",
				},
			},
		},
		Judges: []*ast.JudgeDecl{
			{
				Name: "reviewer",
				LLMDecl: ast.LLMDecl{
					Model:           "claude-sonnet-4-20250514",
					Input:           "code_output",
					Output:          "review_output",
					Session:         ast.SessionArtifactsOnly,
					ReasoningEffort: "low",
				},
			},
		},
		Routers: []*ast.RouterDecl{
			{Name: "dispatch", Mode: ast.RouterFanOutAll},
			{Name: "check_result", Mode: ast.RouterCondition},
		},
		Humans: []*ast.HumanDecl{
			{
				Name:         "approval",
				Input:        "review_output",
				Output:       "human_output",
				Publish:      "human_decision",
				Instructions: "approval_prompt",
				Interaction:  ast.InteractionHuman,
				MinAnswers:   1,
			},
			{
				Name:        "auto_check",
				Interaction: ast.InteractionLLM,
				Model:       "claude-sonnet-4-20250514",
				System:      "auto_system",
			},
			{
				Name:        "hybrid",
				Interaction: ast.InteractionLLMOrHuman,
			},
		},
		Tools: []*ast.ToolNodeDecl{
			{
				Name:    "run_tests",
				Command: "go test ./...",
				Input:   "task_input",
				Output:  "test_output",
			},
		},
		Workflows: []*ast.WorkflowDecl{
			{
				Name:  "main",
				Entry: "coder",
				Vars: &ast.VarsBlock{
					Fields: []*ast.VarField{
						{Name: "wf_var", Type: ast.TypeString},
					},
				},
				Budget: &ast.BudgetBlock{
					MaxParallelBranches: 4,
					MaxDuration:         "60m",
					MaxCostUSD:          10.50,
					MaxTokens:           100000,
					MaxIterations:       20,
				},
				Edges: []*ast.Edge{
					{From: "coder", To: "reviewer"},
					{
						From: "reviewer",
						To:   "coder",
						When: &ast.WhenClause{Condition: "needs_work", Negated: false},
						Loop: &ast.LoopClause{Name: "refine_loop", MaxIterations: 3},
						With: []*ast.WithEntry{
							{Key: "feedback", Value: "{{outputs.reviewer.comments}}"},
						},
					},
					{
						From: "reviewer",
						To:   "done",
						When: &ast.WhenClause{Condition: "needs_work", Negated: true},
					},
					{From: "reviewer", To: "fail"},
				},
			},
		},
		Comments: []*ast.Comment{
			{Text: "## Main workflow for code review"},
		},
	}
}

func TestAttachmentsRoundtrip(t *testing.T) {
	required := true
	original := &ast.File{
		Attachments: &ast.AttachmentsBlock{
			Fields: []*ast.AttachmentField{
				{Name: "logo", Type: ast.AttachmentTypeImage},
				{
					Name:        "spec",
					Type:        ast.AttachmentTypeFile,
					Required:    &required,
					AcceptMIME:  []string{"application/pdf"},
					Description: "Spec PDF",
				},
			},
		},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Errorf("roundtrip mismatch:\noriginal=%#v\nrestored=%#v", original, restored)
	}
	// Sanity: emitted JSON contains the expected keys.
	if !strings.Contains(string(data), `"attachments"`) ||
		!strings.Contains(string(data), `"image"`) ||
		!strings.Contains(string(data), `"file"`) {
		t.Errorf("unexpected JSON: %s", data)
	}
}

func TestToolParallelSafeRoundtrip(t *testing.T) {
	original := &ast.File{
		Tools: []*ast.ToolNodeDecl{
			{Name: "render_scene", Command: "render --scene x", ParallelSafe: true},
			{Name: "plain", Command: "echo ok"},
		},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if !strings.Contains(string(data), `"parallel_safe": true`) {
		t.Errorf("expected parallel_safe in JSON: %s", data)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Errorf("roundtrip mismatch:\noriginal=%#v\nrestored=%#v", original, restored)
	}
	// omitempty: the plain tool must not emit the key.
	plainData, _ := ast.MarshalFile(&ast.File{Tools: []*ast.ToolNodeDecl{{Name: "plain", Command: "echo ok"}}})
	if strings.Contains(string(plainData), "parallel_safe") {
		t.Errorf("parallel_safe should be omitted when false: %s", plainData)
	}
}

func TestLoopClauseRoundtrip(t *testing.T) {
	// The loop bound has three wire forms (literal / expression / unbounded
	// with optional fuel); each must survive marshal → unmarshal untouched,
	// or a studio open→save silently rewrites `as name(unbounded)` to
	// `as name(0)`.
	original := &ast.File{
		Workflows: []*ast.WorkflowDecl{
			{
				Name:  "wf",
				Entry: "a",
				Edges: []*ast.Edge{
					{From: "a", To: "b", Loop: &ast.LoopClause{Name: "lit", MaxIterations: 5}},
					{From: "b", To: "c", Loop: &ast.LoopClause{Name: "expr", MaxIterationsExpr: "{{vars.max}}"}},
					{From: "c", To: "d", Loop: &ast.LoopClause{Name: "free", Unbounded: true}},
					{From: "d", To: "e", Loop: &ast.LoopClause{Name: "fuelled", Unbounded: true, FuelCap: 40}},
				},
			},
		},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Errorf("roundtrip mismatch:\noriginal=%#v\nrestored=%#v", original, restored)
	}
	if !strings.Contains(string(data), `"unbounded"`) || !strings.Contains(string(data), `"fuel_cap"`) {
		t.Errorf("unexpected JSON (missing unbounded/fuel_cap): %s", data)
	}
}

func TestRoundtrip(t *testing.T) {
	original := buildTestFile()

	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if !reflect.DeepEqual(original, restored) {
		// Re-marshal both for readable diff
		origJSON, _ := ast.MarshalFile(original)
		restJSON, _ := ast.MarshalFile(restored)
		t.Errorf("roundtrip mismatch.\nOriginal JSON:\n%s\n\nRestored JSON:\n%s", origJSON, restJSON)
	}
}

func TestEnumsSerializeAsStrings(t *testing.T) {
	f := &ast.File{
		Vars: &ast.VarsBlock{
			Fields: []*ast.VarField{
				{Name: "x", Type: ast.TypeStringArray},
			},
		},
		Schemas: []*ast.SchemaDecl{
			{Name: "s", Fields: []*ast.SchemaField{
				{Name: "f", Type: ast.FieldTypeJSON},
			}},
		},
		Agents:  []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Session: ast.SessionArtifactsOnly, Await: ast.AwaitBestEffort}}},
		Routers: []*ast.RouterDecl{{Name: "r", Mode: ast.RouterCondition}},
		Humans:  []*ast.HumanDecl{{Name: "h", Interaction: ast.InteractionLLMOrHuman}},
	}

	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	jsonStr := string(data)

	// Verify string enum values appear in JSON
	expectations := []string{
		`"string[]"`,       // TypeStringArray
		`"json"`,           // FieldTypeJSON
		`"artifacts_only"`, // SessionArtifactsOnly
		`"condition"`,      // RouterCondition
		`"best_effort"`,    // AwaitBestEffort
		`"llm_or_human"`,   // InteractionLLMOrHuman
	}

	for _, exp := range expectations {
		if !strings.Contains(jsonStr, exp) {
			t.Errorf("expected JSON to contain %s, got:\n%s", exp, jsonStr)
		}
	}

	// Verify no raw integer enum values leak through (check that "type": 5 doesn't appear etc.)
	// We do this by unmarshalling to a generic map and checking key types.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to unmarshal to map: %v", err)
	}

	// Check the vars field type is a string, not a number
	vars := raw["vars"].(map[string]any)
	fields := vars["fields"].([]any)
	field0 := fields[0].(map[string]any)
	typeVal := field0["type"]
	if _, ok := typeVal.(string); !ok {
		t.Errorf("expected vars field type to be string, got %T: %v", typeVal, typeVal)
	}
}

func TestNilAndEmptyFieldsOmitted(t *testing.T) {
	// Minimal file: just one agent with mostly zero-value fields
	f := &ast.File{
		Agents: []*ast.AgentDecl{
			{Name: "minimal"},
		},
	}

	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	jsonStr := string(data)

	// These fields should NOT appear because they are zero/nil/empty
	absent := []string{
		"vars",
		"prompts",
		"schemas",
		"judges",
		"routers",
		"humans",
		"tools",
		"workflows",
		"comments",
		"model",
		"backend",
		"input",
		"output",
		"publish",
		"system",
		"user",
		"tool_max_steps",
		"await",
	}

	for _, key := range absent {
		// Check that the key doesn't appear as a JSON key
		search := `"` + key + `"`
		if strings.Contains(jsonStr, search) {
			t.Errorf("expected %q to be omitted from JSON, got:\n%s", key, jsonStr)
		}
	}

	// "name" and "session" should be present (session has "fresh" as zero value)
	if !strings.Contains(jsonStr, `"name"`) {
		t.Errorf("expected 'name' in JSON output")
	}
}

func TestLiteralKinds(t *testing.T) {
	f := &ast.File{
		Vars: &ast.VarsBlock{
			Fields: []*ast.VarField{
				{
					Name: "s",
					Type: ast.TypeString,
					Default: &ast.Literal{
						Kind:   ast.LitString,
						Raw:    `"hello"`,
						StrVal: "hello",
					},
				},
				{
					Name: "f",
					Type: ast.TypeFloat,
					Default: &ast.Literal{
						Kind:     ast.LitFloat,
						Raw:      "3.14",
						FloatVal: 3.14,
					},
				},
			},
		},
	}

	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"kind": "string"`) {
		t.Errorf("expected literal kind 'string' in JSON:\n%s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"kind": "float"`) {
		t.Errorf("expected literal kind 'float' in JSON:\n%s", jsonStr)
	}

	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if !reflect.DeepEqual(f, restored) {
		t.Error("literal roundtrip mismatch")
	}
}

func TestUnmarshalErrors(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"invalid json", `{bad`},
		{"unknown field type", `{"schemas":[{"name":"s","fields":[{"name":"f","type":"unknown_type"}]}]}`},
		{"unknown session mode", `{"agents":[{"name":"a","session":"bad_mode"}]}`},
		{"unknown router mode", `{"routers":[{"name":"r","mode":"bad_mode"}]}`},
		{"unknown await mode", `{"agents":[{"name":"a","await":"bad_mode"}]}`},
		{"unknown human mode", `{"humans":[{"name":"h","interaction":"bad_mode"}]}`},
		{"unknown type expr", `{"vars":{"fields":[{"name":"v","type":"bad_type"}]}}`},
		{"unknown literal kind", `{"vars":{"fields":[{"name":"v","type":"string","default":{"kind":"bad_kind"}}]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ast.UnmarshalFile([]byte(tt.json))
			if err == nil {
				t.Errorf("expected error for %s, got nil", tt.name)
			}
		})
	}
}

func TestSpansOmitted(t *testing.T) {
	f := &ast.File{
		Agents: []*ast.AgentDecl{
			{
				Name: "with_span",
				Span: ast.Span{
					Start: ast.Pos{File: "test.bot", Line: 1, Column: 1},
					End:   ast.Pos{File: "test.bot", Line: 5, Column: 1},
				},
			},
		},
	}

	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	jsonStr := string(data)
	for _, key := range []string{"span", "start", "end", "file", "line", "column"} {
		if strings.Contains(jsonStr, `"`+key+`"`) {
			t.Errorf("expected span field %q to be omitted from JSON, got:\n%s", key, jsonStr)
		}
	}
}

func TestEdgeWithAllClauses(t *testing.T) {
	f := &ast.File{
		Workflows: []*ast.WorkflowDecl{
			{
				Name:  "wf",
				Entry: "a",
				Edges: []*ast.Edge{
					{
						From: "a",
						To:   "b",
						When: &ast.WhenClause{Condition: "approved", Negated: true},
						Loop: &ast.LoopClause{Name: "retry", MaxIterations: 5},
						With: []*ast.WithEntry{
							{Key: "input", Value: "{{outputs.a.result}}"},
							{Key: "context", Value: "{{vars.ctx}}"},
						},
					},
				},
			},
		},
	}

	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if !reflect.DeepEqual(f, restored) {
		t.Error("edge roundtrip mismatch")
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"negated": true`) {
		t.Errorf("expected negated=true in JSON:\n%s", jsonStr)
	}
}

func TestEditorCriticalFieldsRoundtrip(t *testing.T) {
	threshold := 0.82
	preserve := 7
	workflowInteraction := ast.InteractionLLMOrHuman
	inherit := false
	autoload := true
	original := &ast.File{
		MCPServers: []*ast.MCPServerDecl{{
			Name:      "github",
			Transport: ast.MCPTransportHTTP,
			URL:       "https://api.githubcopilot.com/mcp",
			Auth: &ast.MCPAuthDecl{
				Type:      "oauth2",
				AuthURL:   "https://github.com/login/oauth/authorize",
				TokenURL:  "https://github.com/login/oauth/access_token",
				RevokeURL: "https://github.com/login/oauth/revoke",
				ClientID:  "Iv1.iterion-demo",
				Scopes:    []string{"repo", "read:org"},
			},
		}},
		Agents: []*ast.AgentDecl{{
			Name: "implement",
			LLMDecl: ast.LLMDecl{
				Backend:   "claude_code",
				MCP:       &ast.MCPConfigDecl{Inherit: &inherit, Servers: []string{"github"}, Disable: []string{"local"}},
				System:    "sys",
				User:      "usr",
				Session:   ast.SessionFork,
				MaxTokens: 2048,
				Readonly:  true,
				Compaction: &ast.CompactionBlock{
					Threshold:      &threshold,
					PreserveRecent: &preserve,
				},
			},
		}},
		Judges: []*ast.JudgeDecl{{
			Name: "review",
			LLMDecl: ast.LLMDecl{
				Backend:     "claude_code",
				MaxTokens:   1024,
				Readonly:    true,
				Compaction:  &ast.CompactionBlock{Threshold: &threshold},
				Interaction: ast.InteractionLLM,
			},
		}},
		Workflows: []*ast.WorkflowDecl{{
			Name:           "flow",
			Entry:          "implement",
			DefaultBackend: "claude_code",
			MCP:            &ast.MCPConfigDecl{AutoloadProject: &autoload, Servers: []string{"github"}},
			Compaction:     &ast.CompactionBlock{Threshold: &threshold, PreserveRecent: &preserve},
			Interaction:    &workflowInteraction,
			Edges:          []*ast.Edge{{From: "implement", To: "review"}, {From: "review", To: "done"}},
		}},
	}

	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(original, restored) {
		origJSON, _ := ast.MarshalFile(original)
		restJSON, _ := ast.MarshalFile(restored)
		t.Fatalf("critical field roundtrip mismatch.\nOriginal JSON:\n%s\n\nRestored JSON:\n%s", origJSON, restJSON)
	}
}

// TestCapabilitiesRoundtrip guards the data-integrity fix for the
// JSON serialiser silently dropping `capabilities` on AgentDecl,
// JudgeDecl and WorkflowDecl. An editor save / bundle load that
// stripped board.* caps would turn a privileged agent into one
// without any board access.
func TestCapabilitiesRoundtrip(t *testing.T) {
	original := &ast.File{
		Agents: []*ast.AgentDecl{
			{
				Name: "writer",
				LLMDecl: ast.LLMDecl{
					Model:        "anthropic/claude-sonnet-4-6",
					Capabilities: []string{"board.create", "board.move"},
				},
			},
		},
		Judges: []*ast.JudgeDecl{
			{
				Name: "reviewer",
				LLMDecl: ast.LLMDecl{
					Model:        "anthropic/claude-sonnet-4-6",
					Capabilities: []string{"board.read"},
				},
			},
		},
		Workflows: []*ast.WorkflowDecl{
			{
				Name:         "wf",
				Entry:        "writer",
				Capabilities: []string{"board.read", "board.label"},
			},
		},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	if !reflect.DeepEqual(restored.Agents[0].Capabilities, original.Agents[0].Capabilities) {
		t.Errorf("agent capabilities lost: got %v, want %v",
			restored.Agents[0].Capabilities, original.Agents[0].Capabilities)
	}
	if !reflect.DeepEqual(restored.Judges[0].Capabilities, original.Judges[0].Capabilities) {
		t.Errorf("judge capabilities lost: got %v, want %v",
			restored.Judges[0].Capabilities, original.Judges[0].Capabilities)
	}
	if !reflect.DeepEqual(restored.Workflows[0].Capabilities, original.Workflows[0].Capabilities) {
		t.Errorf("workflow capabilities lost: got %v, want %v",
			restored.Workflows[0].Capabilities, original.Workflows[0].Capabilities)
	}
}

// TestSubbotRoundtrip covers the subbot declaration on the editor-document
// wire (contract C1): every field must survive MarshalFile → UnmarshalFile,
// or the studio save path silently deletes subbots from the .bot file.
func TestSubbotRoundtrip(t *testing.T) {
	original := &ast.File{
		Subbots: []*ast.SubbotDecl{
			{
				Name:   "produce_episode",
				Source: "episode.bot",
				With: []*ast.WithEntry{
					{Key: "episode", Value: "{{outputs.dispatch.ep.id}}"},
					{Key: "tone", Value: "dry"},
				},
				Output:   "episode_out",
				Needs:    []string{"worktree_slot"},
				Isolated: true,
			},
		},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Errorf("roundtrip mismatch:\noriginal=%#v\nrestored=%#v", original, restored)
	}
	// Sanity: emitted JSON carries the contract keys.
	for _, key := range []string{`"subbots"`, `"produce_episode"`, `"episode.bot"`, `"isolated"`, `"needs"`, `"episode_out"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("emitted JSON missing %s:\n%s", key, data)
		}
	}
}

// TestSubbotStudioSavePath proves the full studio save path
// (parse → MarshalFile → UnmarshalFile → unparse) preserves subbot
// declarations end to end.
func TestSubbotStudioSavePath(t *testing.T) {
	src := `subbot produce_episode:
  source: "episode.bot"
  with {
    episode: "{{outputs.dispatch.ep.id}}",
    tone: "dry",
  }
  output: episode_out
  needs: [worktree_slot]
  isolated: true

workflow w:
  entry: produce_episode

  produce_episode -> done
`
	res := parser.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}
	data, err := ast.MarshalFile(res.File)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	out := unparse.Unparse(restored)
	res2 := parser.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	if len(res2.File.Subbots) != 1 {
		t.Fatalf("subbot lost on save path, unparsed source:\n%s", out)
	}
	got, want := res2.File.Subbots[0], res.File.Subbots[0]
	if got.Name != want.Name || got.Source != want.Source ||
		got.Output != want.Output || got.Isolated != want.Isolated ||
		!reflect.DeepEqual(got.Needs, want.Needs) {
		t.Errorf("subbot fields diverged:\ngot=%#v\nwant=%#v", got, want)
	}
	if len(got.With) != len(want.With) {
		t.Fatalf("with entries diverged: got %d, want %d\n%s", len(got.With), len(want.With), out)
	}
	for i := range want.With {
		if got.With[i].Key != want.With[i].Key || got.With[i].Value != want.With[i].Value {
			t.Errorf("with[%d] diverged: got %q=%q, want %q=%q",
				i, got.With[i].Key, got.With[i].Value, want.With[i].Key, want.With[i].Value)
		}
	}
}

// TestFallbacksRoundtrip guards the exact bug TestCapabilitiesRoundtrip
// was written for, on the newest block. ast.UnmarshalFile is a plain
// typed json.Unmarshal — a key the wire struct does not know is silently
// discarded — and the studio round-trips every save through
// /api/parse → /api/unparse. Miss the DECODE half and a `fallbacks:`
// block disappears from the .bot the next time anyone edits an unrelated
// field, with no diagnostic anywhere.
func TestFallbacksRoundtrip(t *testing.T) {
	original := &ast.File{
		Agents: []*ast.AgentDecl{
			{
				Name: "implement",
				LLMDecl: ast.LLMDecl{
					Backend: "claude_code",
					Model:   "claude-opus-5",
					Tools:   []string{"read_file"},
					Fallbacks: []*ast.FallbackDecl{
						{Name: "api", Backend: "claw", Model: "anthropic/claude-opus-5", Provider: "anthropic", On: []string{"usage_window"}},
						{Name: "gpt", Backend: "claw", Model: "openai/gpt-5.5", Metered: true},
					},
				},
			},
		},
		Judges: []*ast.JudgeDecl{
			{
				Name: "gate",
				LLMDecl: ast.LLMDecl{
					Model:     "anthropic/claude-opus-5",
					Fallbacks: []*ast.FallbackDecl{{Name: "second", Model: "openai/gpt-5.5"}},
				},
			},
		},
		Workflows: []*ast.WorkflowDecl{{Name: "wf", Entry: "implement"}},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Errorf("fallbacks did not survive the JSON round-trip\n--- original ---\n%#v\n--- restored ---\n%#v",
			original.Agents[0].Fallbacks, restored.Agents[0].Fallbacks)
	}
	if len(restored.Judges[0].Fallbacks) != 1 {
		t.Error("judge routes lost — the judge encode/decode site is the easy one to forget")
	}
}
