package delegate

import (
	"encoding/json"
	"strings"
	"testing"
)

// #2782: since #1672.2 the structured-output spawn is bounded by the task's
// policy rather than by withholding the roster, so a tool the policy allows
// can run on that pass — and it must meet the same secret hooks as the work
// pass: PreToolUse swaps a __ITERION_SECRET_<name>__ placeholder for the
// value immediately before the tool runs, PostToolUse turns a known value a
// tool's output quotes back into the placeholder. The witness drives the
// real second spawn (the transport TestTheGatedFormattingSpawnCarriesTheGateOnItsStdin
// uses) and asserts both the registration and the verdict reach it.

const (
	probeGrantPlaceholder = "__ITERION_SECRET_probe_grant__"
	probeGrantValue       = "probe-grant-value-3f9a7c2e5b1d4806"
)

// secretSchemaTask is a two-pass node carrying a secret boundary and no
// permission policy: on its structured-output spawn the materialise hook is
// hook_PreToolUse_0 and the redaction hook hook_PostToolUse_0.
func secretSchemaTask() Task {
	return Task{
		NodeID:        "n",
		AllowedTools:  []string{"web_fetch"},
		ToolsDeclared: true,
		OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
		MaterializeSecrets: func(s string) string {
			return strings.ReplaceAll(s, probeGrantPlaceholder, probeGrantValue)
		},
		UnmaterializeSecrets: func(s string) string {
			return strings.ReplaceAll(s, probeGrantValue, probeGrantPlaceholder)
		},
		NamesSecret: func(s string) bool {
			return strings.Contains(s, probeGrantPlaceholder)
		},
	}
}

// probeSecretAnswer reads the spawn's answer to one secret probe, decoded far
// enough to see what the hook handed the CLI.
func probeSecretAnswer(t *testing.T, stdin []string, requestID string) map[string]any {
	t.Helper()
	for _, line := range stdin {
		var resp struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Response  struct {
					HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
				} `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		if resp.Type != "control_response" || resp.Response.RequestID != requestID {
			continue
		}
		if resp.Response.Subtype != "success" {
			t.Fatalf("the %s probe came back as %q, not success — the callback resolved to no handler (stdin: %v)", requestID, resp.Response.Subtype, stdin)
		}
		return resp.Response.Response.HookSpecificOutput
	}
	t.Fatalf("the CLI asked the structured-output spawn for %s and got nothing back (stdin: %v)", requestID, stdin)
	return nil
}

func TestTheFormattingSpawnCarriesTheSecretHooks(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	t.Setenv("HOOK_PROBE_SECRET", "1")
	rec, stdin := spawnRecordedWithStdin(t, secretSchemaTask())
	if len(rec.argv) < 2 {
		t.Fatalf("expected the work pass and the structured-output pass, got %d spawn(s): %v", len(rec.argv), rec.argv)
	}
	if !strings.Contains(rec.argv[1], "--resume") {
		t.Fatalf("spawn #2 is not the structured-output pass: %s", rec.argv[1])
	}

	// Registration: both hooks reached the process.
	hooks := initializeHooks(t, stdin[1])
	if len(hooks["PreToolUse"]) != 1 {
		t.Fatalf("the structured-output spawn registered %d PreToolUse matcher(s), want 1 (the materialise hook) — it never reached the process (stdin: %v)",
			len(hooks["PreToolUse"]), stdin[1])
	}
	if len(hooks["PostToolUse"]) != 1 {
		t.Fatalf("the structured-output spawn registered %d PostToolUse matcher(s), want 1 (the redaction hook) — it never reached the process (stdin: %v)",
			len(hooks["PostToolUse"]), stdin[1])
	}

	// Substitution: a call whose input carries the placeholder is answered
	// with the value materialised — not the literal placeholder the tool
	// would fail on, and never a leak the other way.
	answer := probeSecretAnswer(t, stdin[1], "probeS1")
	updated, _ := answer["updatedInput"].(map[string]any)
	url, _ := updated["url"].(string)
	if !strings.Contains(url, probeGrantValue) {
		t.Errorf("the PreToolUse answer carries no materialised value (updatedInput=%v) — a placeholder reaches the tool unsubstituted", answer["updatedInput"])
	}
	if strings.Contains(url, probeGrantPlaceholder) {
		t.Errorf("the PreToolUse answer keeps the placeholder: %q", url)
	}

	// Redaction: a tool output quoting the value is answered with the
	// placeholder back in its place.
	answer = probeSecretAnswer(t, stdin[1], "probeS2")
	out, _ := answer["updatedToolOutput"].(map[string]any)
	text, _ := out["text"].(string)
	if strings.Contains(text, probeGrantValue) {
		t.Errorf("the PostToolUse answer leaves the value in the tool's output: %q", text)
	}
	if !strings.Contains(text, probeGrantPlaceholder) {
		t.Errorf("the PostToolUse answer carries no redaction (updatedToolOutput=%v)", answer["updatedToolOutput"])
	}
}

// The no-secrets shape is unchanged: a two-pass node without a secret
// boundary registers nothing on its second spawn — the hooks are no-ops a
// task carrying no secrets never pays for.
func TestTheSecretlessFormattingSpawnStaysUnhooked(t *testing.T) {
	rec, stdin := spawnRecordedWithStdin(t, Task{
		NodeID:        "n",
		AllowedTools:  []string{"read_file"},
		ToolsDeclared: true,
		OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(rec.argv) < 2 {
		t.Fatalf("expected two spawns, got %d: %v", len(rec.argv), rec.argv)
	}
	if hooks := initializeHooks(t, stdin[1]); len(hooks) != 0 {
		t.Errorf("a secretless structured-output spawn registered hooks %v", hooks)
	}
}
