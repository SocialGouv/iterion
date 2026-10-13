package tool

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// orchestrationSection cuts the built prompt down to the ultracode grant, so
// a failure message is the section under test and not the whole agentic
// posture. Its absence is itself a failure: the grant is what these tests are
// about.
func orchestrationSection(t *testing.T, prompt string) string {
	t.Helper()
	const header = "## Workflow Orchestration"
	i := strings.Index(prompt, header)
	if i < 0 {
		t.Fatalf("no %q section in the built prompt: %q", header, prompt)
	}
	return prompt[i:]
}

// TestTheUltracodeGrantNamesWhatARefusalMeans runs the two producers that
// reach an ultracode claw node in ONE process: the system prompt the backend
// sends (delegate.Task.BuildSystemPrompt with claw's own mode) and the `agent`
// tool the production registry installs.
//
// The prompt grants standing consent to dispatch subagents, and it outranks a
// tool description: a prompt that orders the delegation while the engine
// refuses it sends the model into a retry loop, N errors per stage, with
// nothing anywhere telling it the capability is gone. So the grant has to name
// what the refusal means and what to do instead — and it has to point at the
// phrase the tool actually says, which is why both read the same constant.
func TestTheUltracodeGrantNamesWhatARefusalMeans(t *testing.T) {
	task := delegate.Task{
		SystemPrompt:     "do the thing",
		SystemPromptMode: delegate.SystemPromptModeForBackend(delegate.BackendClaw),
		Ultracode:        true,
	}
	prompt := orchestrationSection(t, task.BuildSystemPrompt())
	if !strings.Contains(prompt, "dispatch parallel subagents") {
		t.Fatalf("the ultracode grant no longer orders a dispatch; this test guards the pair, not the wording: %q", prompt)
	}

	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}

	headline := toolcatalog.ClawRefusedToolHeadline
	if !strings.Contains(td.Description, headline) {
		t.Errorf("the agent tool's description does not carry %q, so the prompt points at words no tool says: %q", headline, td.Description)
	}
	if !strings.Contains(prompt, headline) {
		t.Errorf("the ultracode grant orders a dispatch without naming the refusal (%q) the engine answers with: %q", headline, prompt)
	}
	// The grant conditions "final, do not retry" on what the tool ANSWERS, and
	// at refusal time the answer is the tool error — not the description it
	// read a turn earlier. The condition the grant states has to be matchable
	// against that error, so both surfaces open on this one sentence.
	const condition = "Spawning a sub-agent is " + toolcatalog.ClawRefusedToolHeadline
	if !strings.Contains(strings.ToLower(prompt), strings.ToLower(condition)) {
		t.Errorf("the grant's condition is no longer %q, so the error cannot be written to match it: %q", condition, prompt)
	}
	out, execErr := executeAgentTool(t, reg)
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(execErr, &unsupported) {
		t.Fatalf("agent tool returned (%q, %v); want the typed capability refusal", out, execErr)
	}
	if !strings.Contains(strings.ToLower(unsupported.Error()), strings.ToLower(condition)) {
		t.Errorf("the refusal the model reads at retry-decision time does not say %q, so the grant's condition matches nothing it received: %s",
			condition, unsupported.Error())
	}
	// The way out, not just the fact: a model told only that the tool fails
	// retries it. The grant must send it back to its own thread.
	for _, want := range []string{"do not retry", "in this thread"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the ultracode grant does not tell the model what to do after a refusal (missing %q): %q", want, prompt)
		}
	}
}

// TestTheUltracodeRefusalClauseIsBackendNeutral: the instruction is one
// constant every ultracode node reads, claude_code included, where the
// delegation really works. The clause must therefore be conditional on what
// the tool answers — never a claim that this backend cannot serve subagents.
func TestTheUltracodeRefusalClauseIsBackendNeutral(t *testing.T) {
	task := delegate.Task{
		SystemPrompt:     "do the thing",
		SystemPromptMode: delegate.SystemPromptModeForBackend(delegate.BackendClaudeCode),
		Ultracode:        true,
	}
	prompt := orchestrationSection(t, task.BuildSystemPrompt())
	for _, forbidden := range []string{"claw", "cannot serve"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("the shared ultracode grant names %q, so a claude_code node reads a refusal that does not apply to it: %q", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, toolcatalog.ClawRefusedToolHeadline) {
		t.Errorf("the conditional clause vanished from the shared grant: %q", prompt)
	}
}
