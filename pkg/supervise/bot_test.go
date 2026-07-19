package supervise

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecisionSchemaIsValidJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal([]byte(decisionSchema), &v); err != nil {
		t.Fatalf("decisionSchema is not valid JSON: %v", err)
	}
	req, _ := v["required"].([]any)
	if len(req) != 1 || req[0] != "intervene" {
		t.Errorf("required = %v; want [intervene]", req)
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	base := buildSystemPrompt(Spec{})
	if !strings.Contains(base, "SUPERVISOR") {
		t.Error("base framing missing")
	}
	if strings.Contains(base, "## Supervision policy") {
		t.Error("empty policy must not emit the policy header")
	}
	// Whitespace-only policy treated as absent.
	if strings.Contains(buildSystemPrompt(Spec{System: "  \n\t"}), "## Supervision policy") {
		t.Error("whitespace-only policy must not emit the policy header")
	}
	withPolicy := buildSystemPrompt(Spec{System: "Watch for flaky tests."})
	if !strings.Contains(withPolicy, "## Supervision policy") || !strings.Contains(withPolicy, "Watch for flaky tests.") {
		t.Errorf("policy not grafted: %q", withPolicy)
	}
	if !strings.HasPrefix(withPolicy, base[:50]) {
		t.Error("policy prompt does not share the base framing prefix")
	}
}

func TestBuildUserPrompt(t *testing.T) {
	t.Run("full input", func(t *testing.T) {
		in := EvalInput{
			ActiveNode:   "implement",
			WakeReason:   "monitor matched: #4 tool_error",
			RecentEvents: []string{"#1 node_started node=implement", "#2 tool_called"},
			Monitors:     []Monitor{{EventType: "tool_error", ToolName: "Bash"}},
			Last:         &Decision{Intervene: true, Message: "re-run the tests", Reason: "flaky"},
		}
		got := buildUserPrompt(in)
		for _, want := range []string{
			"Wake reason: monitor matched: #4 tool_error",
			"Supervised node: implement",
			`Currently watching: [{"event_type":"tool_error","tool_name":"Bash"}]`,
			`Your previous action: intervene=true message="re-run the tests" reason="flaky"`,
			"Do NOT repeat a steering message you already sent",
			"  #1 node_started node=implement\n",
			"  #2 tool_called\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("prompt missing %q\nprompt:\n%s", want, got)
			}
		}
		if strings.Contains(got, "(no events yet)") {
			t.Error("non-empty events must not render the placeholder")
		}
	})

	t.Run("minimal input", func(t *testing.T) {
		got := buildUserPrompt(EvalInput{WakeReason: "turn_boundary"})
		if !strings.Contains(got, "Wake reason: turn_boundary") {
			t.Error("wake reason missing")
		}
		if !strings.Contains(got, "(no events yet)") {
			t.Error("empty-events placeholder missing")
		}
		for _, absent := range []string{"Supervised node:", "Currently watching:", "Your previous action:"} {
			if strings.Contains(got, absent) {
				t.Errorf("minimal prompt should not contain %q", absent)
			}
		}
	})

	t.Run("last decision without message or reason is omitted", func(t *testing.T) {
		got := buildUserPrompt(EvalInput{WakeReason: "turn_boundary", Last: &Decision{Intervene: false}})
		if strings.Contains(got, "Your previous action:") {
			t.Error("empty previous decision should not be rendered")
		}
	})
}

func TestResolveModel(t *testing.T) {
	t.Run("spec pin wins over env", func(t *testing.T) {
		t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "openai/gpt-5.4-mini")
		got, err := resolveModel("anthropic/claude-sonnet-4-6")
		if err != nil || got != "anthropic/claude-sonnet-4-6" {
			t.Fatalf("resolveModel = (%q, %v); want spec pin", got, err)
		}
	})
	t.Run("env override when no pin", func(t *testing.T) {
		t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "openai/gpt-5.4-mini")
		got, err := resolveModel("")
		if err != nil || got != "openai/gpt-5.4-mini" {
			t.Fatalf("resolveModel = (%q, %v); want env override", got, err)
		}
	})
}
