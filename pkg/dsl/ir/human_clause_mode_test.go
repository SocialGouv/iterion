package ir

import (
	"strings"
	"testing"
)

// humanClauseSrc builds a human gate with the given interaction mode and
// extra clauses, reachable from an agent so nothing else complains.
func humanClauseSrc(mode, clauses string) string {
	src := `
schema verdict:
  ok: bool

prompt gate_sys:
  Judge it.

prompt gate_instructions:
  Please look.

agent worker:
  model: "m"
  output: verdict

human gate:
  output: verdict
  instructions: gate_instructions
`
	if mode != "" {
		src += "  interaction: " + mode + "\n"
	}
	src += clauses + `
workflow w:
  worktree: none
  entry: worker
  worker -> gate
  gate -> done
`
	return src
}

// A clause the compiler keeps only inside its own interaction-mode branch
// is DROPPED everywhere else — measured: with `interaction: human` the IR
// carried SystemPrompt="" and Model="" for a gate that declared both, with
// errors=0 and warnings=0. That is plumbing that reads as done and delivers
// nothing, one `interaction:` word away from the gate that works, and a
// `system:` naming no declared prompt was not even checked (the
// validatePromptRef call lives in the branch too). C313 names the clause and
// the modes that read it instead of deleting it in silence.
func TestAHumanClauseItsModeIgnoresIsWarned(t *testing.T) {
	for _, mode := range []string{"human", "human_or_host", "none", ""} {
		r := compileFile(t, humanClauseSrc(mode, "  system: gate_sys\n  model: \"anthropic/claude-sonnet-4-6\"\n"))
		expectDiag(t, r, DiagHumanClauseIgnoredInMode)
		hits := 0
		for _, d := range r.Diagnostics {
			if d.Code != DiagHumanClauseIgnoredInMode {
				continue
			}
			hits++
			if d.Severity != SeverityWarning {
				t.Errorf("mode %q: C313 fired as %v, want a warning", mode, d.Severity)
			}
			if d.NodeID != "gate" {
				t.Errorf("mode %q: C313 attributed to %q, want the gate", mode, d.NodeID)
			}
			for _, want := range []string{"system: gate_sys", "model: \"anthropic/claude-sonnet-4-6\"", "runs no companion model", "interaction: llm, llm_or_human or review"} {
				if !strings.Contains(d.Message, want) {
					t.Errorf("mode %q: the C313 message does not say %q: %s", mode, want, d.Message)
				}
			}
			if d.Hint == "" {
				t.Errorf("mode %q: C313 arrived with no catalogue hint", mode)
			}
		}
		if hits != 1 {
			t.Errorf("mode %q: %d C313 warnings, want exactly one for the two clauses", mode, hits)
		}
	}
}

// `interaction: async` on a human node is refused outright (C240) and the
// mode is RESET to human, so the clause really is dropped: C313 fires beside
// the error rather than pretending the system prompt survived. Two
// diagnostics, two true statements — the refusal of the mode, and the fate of
// the clause under the mode the rest of the compile reads.
func TestAnAsyncHumanGetsBothTheRefusalAndTheClauseWarning(t *testing.T) {
	r := compileFile(t, humanClauseSrc("async", "  system: gate_sys\n"))
	expectDiag(t, r, DiagAsyncOnHuman)
	expectDiag(t, r, DiagHumanClauseIgnoredInMode)
	for _, d := range r.Diagnostics {
		if d.Code == DiagHumanClauseIgnoredInMode && !strings.Contains(d.Message, "interaction human runs no companion model") {
			t.Errorf("C313 on an async human names the wrong mode: %s", d.Message)
		}
	}
}

// The modes that DO run a companion model keep both values and stay silent.
func TestAHumanClauseAModeReadsIsNotWarned(t *testing.T) {
	for _, mode := range []string{"llm", "llm_or_human", "review"} {
		r := compileFile(t, humanClauseSrc(mode, "  system: gate_sys\n  model: \"anthropic/claude-sonnet-4-6\"\n"))
		expectNoDiag(t, r, DiagHumanClauseIgnoredInMode)
		if r.Workflow == nil {
			t.Fatalf("mode %q: no workflow: %v", mode, r.Diagnostics)
		}
		hn, ok := r.Workflow.Nodes["gate"].(*HumanNode)
		if !ok {
			t.Fatalf("mode %q: gate is not a human node", mode)
		}
		if hn.SystemPrompt != "gate_sys" || hn.Model == "" {
			t.Errorf("mode %q: the IR dropped the clause it reads (system=%q model=%q)", mode, hn.SystemPrompt, hn.Model)
		}
	}
}

// A gate that declares neither clause is silent in every mode: the warning
// is about an authored value the compile deletes, never about a default.
func TestAHumanGateWithNoIgnoredClauseIsSilent(t *testing.T) {
	for _, mode := range []string{"human", "human_or_host", "llm_or_human"} {
		clauses := ""
		if mode == "llm_or_human" {
			clauses = "  model: \"anthropic/claude-sonnet-4-6\"\n"
		}
		expectNoDiag(t, compileFile(t, humanClauseSrc(mode, clauses)), DiagHumanClauseIgnoredInMode)
	}
}

// The review-gate configuration is the same class on the other branch: only
// `interaction: review` reads posture / merge_strategy / merge_into /
// max_turns / review_url, and the compiler drops them elsewhere — including
// the `review_url:` template, whose references are parsed inside the branch
// too (an unparseable one is not even reported).
func TestReviewGateClausesOutsideReviewAreWarned(t *testing.T) {
	const clauses = `  posture: agent_verdict_ok
  merge_strategy: merge
  merge_into: main
  max_turns: 4
  review_url: "https://example.test/{{outputs.worker.ok}}"
`
	r := compileFile(t, humanClauseSrc("human", clauses))
	expectDiag(t, r, DiagHumanClauseIgnoredInMode)
	found := false
	for _, d := range r.Diagnostics {
		if d.Code != DiagHumanClauseIgnoredInMode || !strings.Contains(d.Message, "posture") {
			continue
		}
		found = true
		for _, want := range []string{"posture: agent_verdict_ok", "merge_strategy: merge", "merge_into: main", "max_turns: 4", "review_url:", "only interaction: review", "set interaction: review"} {
			if !strings.Contains(d.Message, want) {
				t.Errorf("the review-clause C313 message does not say %q: %s", want, d.Message)
			}
		}
	}
	if !found {
		t.Errorf("no C313 named the review-gate clauses: %v", r.Diagnostics)
	}
	// Under `interaction: review` the same clauses are read, so nothing is
	// said — and the gate keeps them.
	rr := compileFile(t, humanClauseSrc("review", clauses+"  model: \"anthropic/claude-sonnet-4-6\"\n  system: gate_sys\n"))
	expectNoDiag(t, rr, DiagHumanClauseIgnoredInMode)
	if rr.Workflow == nil {
		t.Fatalf("review mode: no workflow: %v", rr.Diagnostics)
	}
	hn := rr.Workflow.Nodes["gate"].(*HumanNode)
	if hn.Posture != PostureAgentVerdictOK || hn.MergeStrategy != "merge" || hn.MergeInto != "main" || hn.MaxTurns != 4 || hn.ReviewURL == "" {
		t.Errorf("review mode dropped a clause it reads: posture=%q merge=%q into=%q turns=%d url=%q",
			hn.Posture, hn.MergeStrategy, hn.MergeInto, hn.MaxTurns, hn.ReviewURL)
	}
}
