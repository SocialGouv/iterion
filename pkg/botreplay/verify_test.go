package botreplay

import (
	"reflect"
	"sort"
	"testing"
)

func TestCollectAssignees_NestedShapes(t *testing.T) {
	// Mirrors emit_output.created_issues[] and roadmap_item arrays.
	output := map[string]interface{}{
		"created_issues": []interface{}{
			map[string]interface{}{"id": "iss-1", "assignee": "feature_dev"},
			map[string]interface{}{"id": "iss-2", "assignee": ""},
		},
		"next_action": []interface{}{
			map[string]interface{}{"title": "x", "assignee": "docs-refresh"},
		},
		"nested": map[string]interface{}{
			"deeper": map[string]interface{}{"bot": "whats-next"},
		},
	}
	got := collectAssignees(output)
	sort.Strings(got)
	want := []string{"", "docs-refresh", "feature_dev", "whats-next"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collectAssignees = %v, want %v", got, want)
	}
}

func TestVerifyNoHallucinatedAssignees(t *testing.T) {
	valid := map[string]bool{"feature-dev": true, "docs-refresh": true}

	cases := []struct {
		name    string
		output  map[string]interface{}
		wantErr bool
	}{
		{
			name:    "snake_case normalizes to known bot",
			output:  map[string]interface{}{"assignee": "feature_dev"},
			wantErr: false,
		},
		{
			name:    "empty assignee is allowed",
			output:  map[string]interface{}{"assignee": ""},
			wantErr: false,
		},
		{
			name:    "whitespace assignee is allowed",
			output:  map[string]interface{}{"assignee": "   "},
			wantErr: false,
		},
		{
			name: "hallucinated assignee fails",
			output: map[string]interface{}{
				"created_issues": []interface{}{
					map[string]interface{}{"assignee": "super-coder-3000"},
				},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyNoHallucinatedAssignees(&Fixture{Output: tc.output}, valid)
			if (err != nil) != tc.wantErr {
				t.Fatalf("VerifyNoHallucinatedAssignees err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyRequiredNonEmpty(t *testing.T) {
	cases := []struct {
		name    string
		output  map[string]interface{}
		fields  []string
		wantErr bool
	}{
		{
			name:    "present non-empty array passes",
			output:  map[string]interface{}{"created_issues": []interface{}{"x"}},
			fields:  []string{"created_issues"},
			wantErr: false,
		},
		{
			name:    "empty array fails",
			output:  map[string]interface{}{"created_issues": []interface{}{}},
			fields:  []string{"created_issues"},
			wantErr: true,
		},
		{
			name:    "absent field fails",
			output:  map[string]interface{}{},
			fields:  []string{"created_issues"},
			wantErr: true,
		},
		{
			name:    "empty string fails",
			output:  map[string]interface{}{"summary": "  "},
			fields:  []string{"summary"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyRequiredNonEmpty(&Fixture{Output: tc.output}, tc.fields)
			if (err != nil) != tc.wantErr {
				t.Fatalf("VerifyRequiredNonEmpty err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestVerifySchema_RealBot exercises the schema lookup + validation path
// against a real compiled bot (feature_dev's v2 campaign →
// campaign_output), so a schema change in the .bot file that breaks the
// golden contract is caught here too — not just in TestGoldens.
func TestVerifySchema_RealBot(t *testing.T) {
	wf, err := CompileBot("feature-dev")
	if err != nil {
		t.Fatalf("CompileBot: %v", err)
	}

	good := &Fixture{
		Bot:  "feature-dev",
		Node: "campaign",
		Output: map[string]interface{}{
			"feature_complete": true,
			// JSON-decoded numbers arrive as float64 — mirror that here
			// (a Go int literal is rejected by the integer check).
			"commits_this_pass": float64(2),
			"work_remaining":    "",
			"needs_human":       false,
			"human_note":        "",
			"summary":           "shipped the feature slice by slice",
		},
	}
	if err := VerifySchema(good, wf); err != nil {
		t.Errorf("valid campaign_output rejected: %v", err)
	}

	// Missing the required `feature_complete` field (the done-oracle).
	bad := &Fixture{
		Bot:  "feature-dev",
		Node: "campaign",
		Output: map[string]interface{}{
			"commits_this_pass": float64(2),
			"work_remaining":    "",
			"needs_human":       false,
			"human_note":        "",
			"summary":           "shipped",
		},
	}
	if err := VerifySchema(bad, wf); err == nil {
		t.Error("campaign_output missing `feature_complete` should fail schema validation")
	}

	// Wrong type on `commits_this_pass` (int field carrying a string).
	wrongType := &Fixture{
		Bot:  "feature-dev",
		Node: "campaign",
		Output: map[string]interface{}{
			"feature_complete":  true,
			"commits_this_pass": "two",
			"work_remaining":    "",
			"needs_human":       false,
			"human_note":        "",
			"summary":           "shipped",
		},
	}
	if err := VerifySchema(wrongType, wf); err == nil {
		t.Error("campaign_output with string commits_this_pass should fail schema validation")
	}
}
