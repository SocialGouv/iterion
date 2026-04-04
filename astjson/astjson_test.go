package astjson

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/ast"
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
				Name:            "coder",
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
		Judges: []*ast.JudgeDecl{
			{
				Name:            "reviewer",
				Model:           "claude-sonnet-4-20250514",
				Input:           "code_output",
				Output:          "review_output",
				Session:         ast.SessionArtifactsOnly,
				ReasoningEffort: "low",
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
				Mode:         ast.HumanPauseUntilAnswers,
				MinAnswers:   1,
			},
			{
				Name:   "auto_check",
				Mode:   ast.HumanAutoAnswer,
				Model:  "claude-sonnet-4-20250514",
				System: "auto_system",
			},
			{
				Name: "hybrid",
				Mode: ast.HumanAutoOrPause,
			},
		},
		Tools: []*ast.ToolNodeDecl{
			{
				Name:    "run_tests",
				Command: "go test ./...",
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

func TestRoundtrip(t *testing.T) {
	original := buildTestFile()

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	restored, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if !reflect.DeepEqual(original, restored) {
		// Re-marshal both for readable diff
		origJSON, _ := Marshal(original)
		restJSON, _ := Marshal(restored)
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
		Agents:  []*ast.AgentDecl{{Name: "a", Session: ast.SessionArtifactsOnly, Await: ast.AwaitBestEffort}},
		Routers: []*ast.RouterDecl{{Name: "r", Mode: ast.RouterCondition}},
		Humans:  []*ast.HumanDecl{{Name: "h", Mode: ast.HumanAutoOrPause}},
	}

	data, err := Marshal(f)
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
		`"auto_or_pause"`,  // HumanAutoOrPause
	}

	for _, exp := range expectations {
		if !strings.Contains(jsonStr, exp) {
			t.Errorf("expected JSON to contain %s, got:\n%s", exp, jsonStr)
		}
	}

	// Verify no raw integer enum values leak through (check that "type": 5 doesn't appear etc.)
	// We do this by unmarshalling to a generic map and checking key types.
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to unmarshal to map: %v", err)
	}

	// Check the vars field type is a string, not a number
	vars := raw["vars"].(map[string]interface{})
	fields := vars["fields"].([]interface{})
	field0 := fields[0].(map[string]interface{})
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

	data, err := Marshal(f)
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
		"delegate",
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

	data, err := Marshal(f)
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

	restored, err := Unmarshal(data)
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
		{"unknown human mode", `{"humans":[{"name":"h","mode":"bad_mode"}]}`},
		{"unknown type expr", `{"vars":{"fields":[{"name":"v","type":"bad_type"}]}}`},
		{"unknown literal kind", `{"vars":{"fields":[{"name":"v","type":"string","default":{"kind":"bad_kind"}}]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Unmarshal([]byte(tt.json))
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
					Start: ast.Pos{File: "test.iter", Line: 1, Column: 1},
					End:   ast.Pos{File: "test.iter", Line: 5, Column: 1},
				},
			},
		},
	}

	data, err := Marshal(f)
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

	data, err := Marshal(f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	restored, err := Unmarshal(data)
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
