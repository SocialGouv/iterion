package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// toolCallEnvelopeFile writes one tool_call envelope a scripted runner can
// `cat` to drive the launcher's forwarded-call handler.
func toolCallEnvelopeFile(t *testing.T, dir, id, name string) string {
	t.Helper()
	env, err := delegate.NewToolCallEnvelope(id, name, json.RawMessage(`{"title":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "call.ndjson")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The stderr report reads a buffer exec's copier goroutine owns until the
// runner is JOINED. Every return of the dispatch joins it; a PANIC does not
// — a launcher-side tool (an MCP tool, a host tool) blowing up unwinds
// through the multiplexer and through this defer with no Wait at all, while
// the container is still writing. Reading the buffer there is a data race on
// a bytes.Buffer (`-race` names it), and what it would hand the run's secret
// pass and the operator's log is a torn body.
//
// The control arm drives the SAME runner with a tool that answers, so the
// absence in the panic arm is the gate and not a scenario that reports
// nothing anyway.
func TestSandboxDispatch_AnUnwoundCrossingReportsNoStderr(t *testing.T) {
	const toolName = "mcp_github_create_issue"
	const notice = "iterion-claw-runner: warn: settings hooks: 1 entry ignored"

	for _, tc := range []struct {
		name       string
		execute    func(context.Context, json.RawMessage) (string, error)
		wantReport bool
	}{
		{
			name:    "a launcher-side tool that panics",
			execute: func(context.Context, json.RawMessage) (string, error) { panic("a launcher-side tool blew up") },
		},
		{
			name:       "a launcher-side tool that answers",
			execute:    func(context.Context, json.RawMessage) (string, error) { return "done", nil },
			wantReport: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			callFile := toolCallEnvelopeFile(t, dir, "tc-1", toolName)
			resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
			var logs strings.Builder
			b := loggedTestClawBackend(&logs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// The notice goes out BEFORE the tool call, so the buffer holds
			// the container's words whichever arm runs; the bulk after it
			// keeps the copier writing while the panic unwinds, and the
			// runner exits on its own either way.
			script := awaitTaskEnvelope +
				"printf '%s\\n' \"$1\" >&2\n" +
				"cat \"$2\"\n" +
				"yes d | head -n 200000 >&2\n" +
				"cat \"$3\"\nexit 0\n"
			run := &scriptedSandbox{script: script, args: []string{notice, callFile, resultFile}}

			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				_, _ = b.executeViaSandboxRunner(ctx, delegate.Task{
					NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
					Sandbox:  run,
					ToolDefs: []delegate.ToolDef{{Name: toolName, Execute: tc.execute}},
				})
			}()

			var panicked any
			select {
			case panicked = <-done:
			case <-time.After(sandboxDrainBound):
				t.Fatalf("the dispatch neither returned nor panicked in %s", sandboxDrainBound)
			}
			if tc.wantReport && panicked != nil {
				t.Fatalf("the control arm panicked (%v) — it must drive the same crossing to a normal return", panicked)
			}
			if !tc.wantReport && panicked == nil {
				t.Fatal("the tool's panic did not unwind through the dispatch: this witness no longer reaches the defer it watches")
			}

			reported := strings.Contains(logs.String(), "the sandbox runner's stderr:")
			if tc.wantReport && !reported {
				t.Errorf("the crossing reported nothing of the container's log (%q) — the panic arm's silence proves nothing", logs.String())
			}
			if !tc.wantReport && reported {
				t.Errorf("a dispatch that unwound without joining the runner read its stderr anyway: %.200q", logs.String())
			}
		})
	}
}
