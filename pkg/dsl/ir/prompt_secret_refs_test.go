package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// promptSinkWorkflow declares a value secret `tok` granting the command sink
// alone, beside the given nodes.
func promptSinkWorkflow(nodes ...Node) *Workflow {
	w := &Workflow{
		Secrets: map[string]*Secret{"tok": {Name: "tok", As: "value", Sinks: []string{secretsink.Command}}},
		Nodes:   map[string]Node{},
	}
	for _, n := range nodes {
		w.Nodes[n.NodeID()] = n
	}
	return w
}

// A prompt-sink text no node owns — a bundle preset's body — is rendered only
// where a model TASK is built: its references are judged per reader, as the
// compiler judges a prompt's (a prompt no node names draws no C323), so a
// workflow that builds no task says nothing about a body it never renders,
// and one that does says the grant. Each node kind is counted by whether
// pkg/backend/model's buildTask runs for it.
func TestAPromptSinkTextIsJudgedOnlyWhereAModelTaskRendersIt(t *testing.T) {
	const body = "Deploy with {{secrets.tok}} when asked."
	cases := []struct {
		name    string
		node    Node
		renders bool
	}{
		{"agent", &AgentNode{BaseNode: BaseNode{ID: "a"}}, true},
		{"judge", &JudgeNode{BaseNode: BaseNode{ID: "j"}}, true},
		{"tool whose recovery ladder hands the goal to an agent", &ToolNode{BaseNode: BaseNode{ID: "t"}, Policy: PolicyRecover, Recovery: &RecoverySpec{MaxAgentAttempts: 1}}, true},
		{"tool", &ToolNode{BaseNode: BaseNode{ID: "t"}}, false},
		{"tool whose recovery stops at self-repair", &ToolNode{BaseNode: BaseNode{ID: "t"}, Policy: PolicyRecover, Recovery: &RecoverySpec{MaxRepairAttempts: 1}}, false},
		{"llm router", &RouterNode{BaseNode: BaseNode{ID: "r"}, RouterMode: RouterLLM}, false},
		{"human answered by a model", &HumanNode{BaseNode: BaseNode{ID: "h"}, InteractionFields: InteractionFields{Interaction: InteractionLLM}}, false},
		{"subbot", &SubbotNode{BaseNode: BaseNode{ID: "s"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := promptSinkWorkflow(tc.node)
			if got := w.RendersPresetPrompt(); got != tc.renders {
				t.Fatalf("RendersPresetPrompt() = %v, want %v", got, tc.renders)
			}
			issues := PromptSecretRefIssues(w, body)
			switch {
			case tc.renders && (len(issues) != 1 || issues[0].Code != DiagSecretSinkUngranted):
				t.Errorf("a body rendered into this node's task draws %+v, want one C323", issues)
			case !tc.renders && len(issues) != 0:
				t.Errorf("a body no task of this workflow renders draws %+v, want nothing: the reference reaches no sink", issues)
			}
		})
	}
	// An empty graph renders nothing; a nil workflow is nothing to judge.
	if got := PromptSecretRefIssues(promptSinkWorkflow(), body); len(got) != 0 {
		t.Errorf("a workflow with no node draws %+v", got)
	}
	if (*Workflow)(nil).RendersPresetPrompt() {
		t.Error("a nil workflow renders a preset prompt")
	}

	// The template verdict is the declaration's, not a reader's: a body the
	// parser cannot read is said whether or not a task renders it, as the
	// compiler says C004 on a prompt no node names.
	malformed := PromptSecretRefIssues(promptSinkWorkflow(&ToolNode{BaseNode: BaseNode{ID: "t"}}), "Write {{secrets.tok and stop.")
	if len(malformed) != 1 || malformed[0].Code != DiagBadTemplateRef {
		t.Errorf("a malformed body in a task-free workflow draws %+v, want C004", malformed)
	}
}

// The parser's error quotes the rest of the text, so a malformed body of any
// size would ship its whole tail into a lint message and a launch's log line:
// the reason is held to the compiler's own bound at the one point both
// readers cross.
func TestAMalformedPromptSinkTextsReasonIsBounded(t *testing.T) {
	body := "Focus on {{secrets.tok and then " + strings.Repeat("the rest of the page, which goes on. ", 3000)
	if len(body) < 4*expr.MaxDiagnosticText {
		t.Fatalf("the fixture is %d bytes, not large enough to exceed the bound", len(body))
	}
	issues := PromptSecretRefIssues(promptSinkWorkflow(&AgentNode{BaseNode: BaseNode{ID: "a"}}), body)
	if len(issues) != 1 || issues[0].Code != DiagBadTemplateRef {
		t.Fatalf("got %+v, want one C004", issues)
	}
	// Clip cuts at the bound and marks the cut with one rune.
	if max := expr.MaxDiagnosticText + len("…"); len(issues[0].Reason) > max {
		t.Errorf("the C004 reason is %d bytes, past the %d the compiler holds every diagnostic to", len(issues[0].Reason), max)
	}
	if !strings.HasSuffix(issues[0].Reason, "…") {
		t.Errorf("the clipped reason does not mark its cut:\n%.120s", issues[0].Reason)
	}
}
