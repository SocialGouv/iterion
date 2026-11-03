package bundlelint_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// presetWorkflow declares one value secret `tok` with the given grants (nil =
// every sink) beside an agent that reads the preset's "## Focus" section.
func presetWorkflow(sinks []string) *ir.Workflow {
	w := wf("bot", nil, nil, nil, &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}})
	w.Secrets["tok"] = &ir.Secret{Name: "tok", As: "value", Sinks: sinks}
	return w
}

func diagsFor(t *testing.T, w *ir.Workflow, presets ...bundlelint.PresetDoc) []bundlelint.Diag {
	t.Helper()
	return bundlelint.CheckConsistency(bundlelint.Input{Workflow: w, Presets: presets})
}

func onlyCode(t *testing.T, diags []bundlelint.Diag, code string) bundlelint.Diag {
	t.Helper()
	var found []bundlelint.Diag
	for _, d := range diags {
		if string(d.Code) == code {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s, got %d (%v)", code, len(found), codesOf(diags))
	}
	return found[0]
}

// A bundle preset's prompt body is appended to the system prompt of every
// model task the run builds, rendered at the PROMPT sink — and it is merged into the
// workflow AFTER compilation, so no compile check ever read it. The lint
// holds it to the workflow's grants, with the compiler's own code, sentence
// and remedy, and names the preset file.
func TestABundlePresetPromptSecretRefIsRefusedAtLint(t *testing.T) {
	refused := diagsFor(t, presetWorkflow([]string{secretsink.Command}), bundlelint.PresetDoc{
		Path: "presets/sre.md", Name: "sre", Prompt: "Deploy with {{secrets.tok}} when asked.",
	})
	d := onlyCode(t, refused, string(ir.DiagSecretSinkUngranted))
	if d.Severity != bundlelint.SeverityError {
		t.Errorf("C323 on a preset body is %v, want an error: the resolver leaves the reference as written, so every model task the run builds reads literal DSL text and the declaration's refusal is silently void", d.Severity)
	}
	if d.Field != "presets/sre.md" {
		t.Errorf("the finding is on %q, not the preset file", d.Field)
	}
	// The sentence says what the reader gate implements: the body is
	// rendered by the model TASKS the run builds, not by every LLM node.
	for _, want := range []string{`preset "sre"`, "the system prompt of every model task the run builds", "{{secrets.tok}}", "reaches the prompt sink", `secret "tok" does not grant (sinks: [command])`, "add `prompt`"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("the message does not carry %q:\n%s", want, d.Message)
		}
	}
	if strings.Contains(d.Message, "every LLM node") {
		t.Errorf("the message promises every LLM node a body only a model task renders:\n%s", d.Message)
	}
	if strings.TrimSpace(d.Hint) == "" {
		t.Error("the finding carries no hint — the compiler's catalogue line is the remedy")
	}

	// The grant given, the same body is clean; so is a secret that names no
	// `sinks:` at all (the default is every sink). `prompt` alone resolves
	// nothing (C322 on the declaration), hence the second resolving grant.
	for _, sinks := range [][]string{{secretsink.Prompt, secretsink.Agent}, nil} {
		clean := diagsFor(t, presetWorkflow(sinks), bundlelint.PresetDoc{
			Path: "presets/sre.md", Name: "sre", Prompt: "Deploy with {{secrets.tok}} when asked.",
		})
		if len(clean) != 0 {
			t.Errorf("a preset body whose secret grants the prompt sink (sinks: %v) draws %v", sinks, codesOf(clean))
		}
	}

	// A var-only preset, and a bundle with no presets at all, ask nothing.
	for _, docs := range [][]bundlelint.PresetDoc{
		{{Path: "presets/fast.md", Name: "fast", Prompt: "Go fast. No templates here."}},
		nil,
	} {
		if got := diagsFor(t, presetWorkflow([]string{secretsink.Command}), docs...); len(got) != 0 {
			t.Errorf("presets %v draw %v", docs, codesOf(got))
		}
	}

	// The body is appended to a model TASK's system prompt: a workflow that
	// builds none renders it nowhere, so the same reference reaches no sink
	// and the lint fails no bundle over a body a run never reads — the
	// compiler's own per-reader rule.
	toolOnly := wf("bot", nil, nil, nil, &ir.ToolNode{BaseNode: ir.BaseNode{ID: "kick"}, Command: "make"})
	toolOnly.Secrets["tok"] = &ir.Secret{Name: "tok", As: "value", Sinks: []string{secretsink.Command}}
	if got := diagsFor(t, toolOnly, bundlelint.PresetDoc{Path: "presets/sre.md", Name: "sre", Prompt: "Deploy with {{secrets.tok}} when asked."}); len(got) != 0 {
		t.Errorf("a preset body in a workflow with no model task draws %v: no node renders it", codesOf(got))
	}
}

// The bodies are held to the program a LAUNCH merges them into: for a sibling
// entry validated by path the manifest-bound checks read main.bot's program,
// while `iterion run bots/x/extend.bot` folds the presets into extend.bot's
// own — so a secret the sibling grants and main withholds is no finding on
// the sibling, and one main grants and the sibling withholds is.
func TestABundlePresetIsJudgedAgainstTheWorkflowALaunchMergesItInto(t *testing.T) {
	withheld, granted := presetWorkflow([]string{secretsink.Command}), presetWorkflow(nil)
	doc := bundlelint.PresetDoc{Path: "presets/sre.md", Name: "sre", Prompt: "Deploy with {{secrets.tok}} when asked."}

	clean := bundlelint.CheckConsistency(bundlelint.Input{Workflow: withheld, Launched: granted, Presets: []bundlelint.PresetDoc{doc}})
	if len(clean) != 0 {
		t.Errorf("the preset is judged against the manifest's main, which withholds the sink, not the launched entry, which grants it: %v", codesOf(clean))
	}
	refused := bundlelint.CheckConsistency(bundlelint.Input{Workflow: granted, Launched: withheld, Presets: []bundlelint.PresetDoc{doc}})
	onlyCode(t, refused, string(ir.DiagSecretSinkUngranted))
}

// The declaration half: a preset naming a secret the workflow does not
// declare renders to nothing at run time, under the compiler's own C093.
func TestABundlePresetNamingAnUndeclaredSecretIsSaidAtLint(t *testing.T) {
	got := diagsFor(t, presetWorkflow(nil), bundlelint.PresetDoc{
		Path: "presets/sre.md", Name: "sre", Prompt: "Use {{secrets.nope}}.",
	})
	d := onlyCode(t, got, string(ir.DiagUnknownSecret))
	if d.Severity != bundlelint.SeverityError || !strings.Contains(d.Message, `undeclared secret "nope"`) {
		t.Errorf("C093 on a preset body = %v %q", d.Severity, d.Message)
	}
}

// A body the template parser cannot read WARNS: the run's resolver leaves
// such a text as written, so prose that mentions braces keeps working — and
// the warning says the references behind it went unchecked, instead of
// reporting "no reference".
func TestAMalformedPresetTemplateWarnsThatTheRestWentUnchecked(t *testing.T) {
	got := diagsFor(t, presetWorkflow([]string{secretsink.Command}), bundlelint.PresetDoc{
		Path: "presets/sre.md", Name: "sre", Prompt: "Write {{secrets.tok and stop.",
	})
	d := onlyCode(t, got, string(ir.DiagBadTemplateRef))
	if d.Severity != bundlelint.SeverityWarning {
		t.Errorf("C004 on a preset body is %v, want a warning: the run tolerates the text", d.Severity)
	}
	if !strings.Contains(d.Message, "were not checked") {
		t.Errorf("the warning does not say what went unchecked:\n%s", d.Message)
	}
}
