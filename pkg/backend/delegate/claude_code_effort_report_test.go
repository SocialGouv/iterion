package delegate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// effortStandInCLI records, per spawn, the CLAUDE_CODE_EFFORT_LEVEL it was
// started with, and answers the minimum stream-json the SDK needs to end a
// turn once it has read the user message.
const effortStandInCLI = `#!/bin/sh
printf '%s\n' "${CLAUDE_CODE_EFFORT_LEVEL-<unset>}" >> "$EFFORT_LOG"
case "$*" in *--input-format*)
	while read -r line; do case "$line" in *'"type":"user"'*) break ;; esac; done ;;
esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
`

// TestClaudeCodeReportsTheEffortItHandsTheCLI runs the real backend against a
// stand-in CLI and compares the effort the Result reports with the one every
// spawn was started with: the node's level, `none` raised to `low`, and the
// backend's default when the task names none. The report is what a node's
// record says the model was asked to spend.
//
// Mutation that reddens it: drop the reportClaudeCodeEffort defer from
// Execute, or report task.ReasoningEffort instead of claudeCodeSpawnEffort.
func TestClaudeCodeReportsTheEffortItHandsTheCLI(t *testing.T) {
	for _, tc := range []struct{ handed, want string }{
		{"high", "high"},
		{"none", "low"},
		{"", defaultClaudeCodeEffort},
	} {
		t.Run("handed "+tc.handed, func(t *testing.T) {
			dir := t.TempDir()
			cli := filepath.Join(dir, "claude")
			if err := os.WriteFile(cli, []byte(effortStandInCLI), 0o755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "effort.log")
			t.Setenv("EFFORT_LOG", logPath)
			t.Setenv("TMPDIR", t.TempDir())
			b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
			res, err := b.Execute(context.Background(), Task{Command: cli, WorkDir: dir, UserPrompt: "x", ReasoningEffort: tc.handed})
			t.Logf("Execute: err=%v", err)
			raw, rerr := os.ReadFile(logPath)
			if rerr != nil {
				t.Fatalf("the stand-in was never spawned: %v", rerr)
			}
			spawned := strings.Fields(string(raw))
			for _, got := range spawned {
				if got != tc.want {
					t.Errorf("a spawn got CLAUDE_CODE_EFFORT_LEVEL=%q, want %q", got, tc.want)
				}
			}
			if !res.WireReasoningEffortReported || res.WireReasoningEffort != tc.want || res.WireReasoningEffortLast != tc.want {
				t.Errorf("reported %+v, want %q on both ends, reported", [3]any{res.WireReasoningEffort, res.WireReasoningEffortLast, res.WireReasoningEffortReported}, tc.want)
			}
		})
	}
}
