package ir

import "testing"

func TestValidateMemory_WarnsOnNonClawBackend(t *testing.T) {
	cases := []struct {
		name        string
		backend     string
		wantWarning bool
	}{
		{"claw is supported", "claw", false},
		{"empty backend (auto-resolved) accepted", "", false},
		{"claude_code is ignored", "claude_code", true},
		{"codex is ignored", "codex", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &Workflow{
				Name: "t",
				Nodes: map[string]Node{
					"a1": &AgentNode{
						BaseNode:  BaseNode{ID: "a1"},
						LLMFields: LLMFields{Backend: tc.backend},
						Memory: &Memory{
							Enabled: true,
							Scope:   "session-continuity",
							Read:    true, Write: true,
						},
					},
				},
			}
			c := &compiler{}
			c.validateMemory(w)
			gotWarning := false
			for _, d := range c.diags {
				if d.Code == DiagMemoryNotSupported {
					gotWarning = true
				}
			}
			if gotWarning != tc.wantWarning {
				t.Fatalf("backend=%q: got warning=%v want %v; diags=%+v", tc.backend, gotWarning, tc.wantWarning, c.diags)
			}
		})
	}
}

func TestValidateMemory_DisabledNeverWarns(t *testing.T) {
	w := &Workflow{
		Name: "t",
		Nodes: map[string]Node{
			"a1": &AgentNode{
				BaseNode:  BaseNode{ID: "a1"},
				LLMFields: LLMFields{Backend: "claude_code"},
				Memory:    &Memory{Enabled: false},
			},
		},
	}
	c := &compiler{}
	c.validateMemory(w)
	for _, d := range c.diags {
		if d.Code == DiagMemoryNotSupported {
			t.Fatalf("disabled block should not warn, got %+v", d)
		}
	}
}

func TestValidateMemory_RequiresScopeWhenEnabled(t *testing.T) {
	w := &Workflow{
		Name: "t",
		Nodes: map[string]Node{
			"a1": &AgentNode{
				BaseNode:  BaseNode{ID: "a1"},
				LLMFields: LLMFields{Backend: "claw"},
				Memory:    &Memory{Enabled: true, Scope: ""},
			},
		},
	}
	c := &compiler{}
	c.validateMemory(w)
	if !hasDiag(c.diags, DiagMemoryMissingScope) {
		t.Fatalf("expected DiagMemoryMissingScope, got %+v", c.diags)
	}
}
