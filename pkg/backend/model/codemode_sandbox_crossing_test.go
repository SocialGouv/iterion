package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// TestCodemodeChangesNothingOnTheWireToASandboxRunner holds what the
// predicate buys over the mutation it replaced, and it drives the REAL
// crossing to do it.
//
// An earlier version called delegate.ToIOTask on a task the test built, which
// is not where the defect lived: the mutation sat at the top of
// ClawBackend.Execute, and the envelope is produced further down, by
// executeViaSandboxRunner → runnerIOTask. A sixth adversarial round put the
// mutation back verbatim and watched thirty packages stay green.
//
// So this spawns a scripted sandbox that captures the one line the launcher
// writes to the runner's stdin, and reads the envelope the runner would have
// received. What it must not contain is `has_tools` — a runner image older
// than codemode keeps that flag, drops the `codemode` that justified it, and
// runs the node with ZERO tools while telling it to answer "after completing
// all tool operations".
func TestCodemodeChangesNothingOnTheWireToASandboxRunner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())

	dir := t.TempDir()
	captured := filepath.Join(dir, "envelope.json")
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	_, _ = b.Execute(context.Background(), delegate.Task{
		NodeID:       "n",
		Model:        "test/test-model",
		UserPrompt:   "go",
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		HasTools:     false,
		Codemode:     true,
		WorkDir:      dir,
		Sandbox: &scriptedSandbox{
			script: "IFS= read -r line\nprintf '%s' \"$line\" > \"$1\"\nexit 1\n",
			args:   []string{captured},
		},
	})

	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("the launcher never wrote a task envelope, so this test crossed nothing: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("the captured envelope is not JSON (%v): %s", err, raw)
	}
	// The envelope wraps the task; find it wherever it sits.
	var walk func(any) map[string]any
	walk = func(v any) map[string]any {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if _, hasNode := m["node_id"]; hasNode {
			return m
		}
		for _, child := range m {
			if found := walk(child); found != nil {
				return found
			}
		}
		return nil
	}
	task := walk(envelope)
	if task == nil {
		t.Fatalf("no task found in the captured envelope: %s", raw)
	}
	if got, present := task["codemode"]; !present || got != true {
		t.Errorf("the envelope does not carry codemode=true (got %v, present=%v)", got, present)
	}
	if _, present := task["has_tools"]; present {
		t.Errorf("has_tools crossed for a node that declared no tools: a runner that cannot read `codemode` gets a tool loop with no tools.\nenvelope task: %v", task)
	}
}
