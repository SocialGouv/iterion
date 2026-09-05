package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func ultracodeWorkflow(model string) string {
	var b strings.Builder
	b.WriteString("agent x:\n")
	if model != "" {
		b.WriteString("  model: \"" + model + "\"\n")
	}
	b.WriteString("  system: p\n")
	b.WriteString("  reasoning_effort: ultracode\n\n")
	b.WriteString("prompt p:\n  hi\n\n")
	b.WriteString("workflow w:\n  entry: x\n  x -> done\n")
	return b.String()
}

func hasDiag(diags []Diagnostic, code DiagCode) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestUltracodeAcceptedAsEffort(t *testing.T) {
	pr := parser.Parse("t.bot", ultracodeWorkflow("anthropic/claude-opus-4-8"))
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser must accept reasoning_effort: ultracode, got %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("ultracode on opus-4-8 must compile without errors, got %+v", cr.Diagnostics)
	}
	if hasDiag(cr.Diagnostics, DiagUltracodeModelGate) {
		t.Errorf("C089 must NOT fire on claude-opus-4-8, got %+v", cr.Diagnostics)
	}
}

func TestUltracodeGateWarnsOffOpus48(t *testing.T) {
	pr := parser.Parse("t.bot", ultracodeWorkflow("anthropic/claude-sonnet-4-6"))
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if !hasDiag(cr.Diagnostics, DiagUltracodeModelGate) {
		t.Fatalf("expected C089 ultracode-model-gate warning on sonnet-4-6, got %+v", cr.Diagnostics)
	}
	// The gate is advisory — ultracode degrades to xhigh, it must not error.
	if cr.HasErrors() {
		t.Errorf("C089 must be a warning, not an error: %+v", cr.Diagnostics)
	}
}

func TestUltracodeNoWarnOnOpusAlias(t *testing.T) {
	// The bare "opus" alias resolves to the newest Opus (4.8) in claw's
	// registry, so it must not trip the gate.
	pr := parser.Parse("t.bot", ultracodeWorkflow("opus"))
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if hasDiag(cr.Diagnostics, DiagUltracodeModelGate) {
		t.Errorf("C089 must NOT fire on the opus alias, got %+v", cr.Diagnostics)
	}
}

// TestUltracodeNoWarnOnClaude5: the Claude 5 family carries ultracode's
// orchestration half — C089 must stay silent on Opus 5 and Fable 5.1.
func TestUltracodeNoWarnOnClaude5(t *testing.T) {
	for _, model := range []string{"anthropic/claude-opus-5", "anthropic/claude-fable-5-1", "fable"} {
		pr := parser.Parse("t.bot", ultracodeWorkflow(model))
		if len(pr.Diagnostics) > 0 {
			t.Fatalf("%s: parse errors: %+v", model, pr.Diagnostics)
		}
		cr := Compile(pr.File)
		if cr.HasErrors() {
			t.Fatalf("%s: ultracode must compile without errors, got %+v", model, cr.Diagnostics)
		}
		if hasDiag(cr.Diagnostics, DiagUltracodeModelGate) {
			t.Errorf("C089 must NOT fire on %s, got %+v", model, cr.Diagnostics)
		}
	}
}
