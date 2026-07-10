package delegate

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ParseAskUserToolInput mirrors claw-code-go's ParseAskUserInput
// semantics but never errors (the PreToolUse hook must not fail the
// interception): malformed entries are skipped, labels capped, and
// free text defaults to allowed only when no options are given.
func TestParseAskUserToolInput(t *testing.T) {
	tests := []struct {
		name        string
		in          map[string]any
		wantOptions []AskUserOption
		wantFree    bool
	}{
		{
			name:        "no options defaults to free text",
			in:          map[string]any{"question": "why?"},
			wantOptions: nil,
			wantFree:    true,
		},
		{
			name: "options without flag disable free text",
			in: map[string]any{
				"question": "pick",
				"options": []any{
					map[string]any{"id": "a", "label": "Option A"},
					map[string]any{"id": "b", "label": "Option B"},
				},
			},
			wantOptions: []AskUserOption{{ID: "a", Label: "Option A"}, {ID: "b", Label: "Option B"}},
			wantFree:    false,
		},
		{
			name: "explicit allow_free_text with options",
			in: map[string]any{
				"question":        "pick or type",
				"options":         []any{map[string]any{"id": "a", "label": "A"}},
				"allow_free_text": true,
			},
			wantOptions: []AskUserOption{{ID: "a", Label: "A"}},
			wantFree:    true,
		},
		{
			name: "malformed entries are skipped not fatal",
			in: map[string]any{
				"question": "pick",
				"options": []any{
					"not-an-object",
					map[string]any{"id": "", "label": "missing id"},
					map[string]any{"id": "ok", "label": "  Trimmed  "},
				},
			},
			wantOptions: []AskUserOption{{ID: "ok", Label: "Trimmed"}},
			wantFree:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, free := ParseAskUserToolInput(tt.in)
			if !reflect.DeepEqual(opts, tt.wantOptions) {
				t.Errorf("options = %#v, want %#v", opts, tt.wantOptions)
			}
			if free != tt.wantFree {
				t.Errorf("allowFreeText = %v, want %v", free, tt.wantFree)
			}
		})
	}
}

// The stamped questions map must round-trip through JSON (it is
// persisted verbatim in the checkpoint's InteractionQuestions) and the
// no-options call must leave the historical single-key shape untouched.
func TestAddAskUserOptionKeys(t *testing.T) {
	questions := map[string]any{AskUserQuestionKey: "pick one"}
	AddAskUserOptionKeys(questions, nil, true)
	if _, ok := questions[AskUserOptionsKey]; ok {
		t.Fatalf("no-options call must not stamp %s", AskUserOptionsKey)
	}

	AddAskUserOptionKeys(questions, []AskUserOption{{ID: "a", Label: "A"}}, false)
	raw, err := json.Marshal(questions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	list, ok := back[AskUserOptionsKey].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("options round-trip = %#v", back[AskUserOptionsKey])
	}
	opt, _ := list[0].(map[string]any)
	if opt["id"] != "a" || opt["label"] != "A" {
		t.Errorf("option round-trip = %#v", opt)
	}
	if back[AskUserAllowFreeTextKey] != false {
		t.Errorf("allow_free_text round-trip = %#v", back[AskUserAllowFreeTextKey])
	}
}
