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
// started with and the flag settings object its --settings names, and
// answers the minimum stream-json the SDK needs to end a turn once it has
// read the user message.
const effortStandInCLI = `#!/bin/sh
printf '%s\n' "${CLAUDE_CODE_EFFORT_LEVEL-<unset>}" >> "$EFFORT_LOG"
settings=; prev=
for a in "$@"; do if [ "$prev" = --settings ]; then settings=$a; fi; prev=$a; done
if [ -n "$settings" ] && [ -f "$settings" ]; then tr '\n' ' ' < "$settings" >> "$SETTINGS_FILE_LOG"; fi
printf '\n' >> "$SETTINGS_FILE_LOG"
case "$*" in *--input-format*)
	while read -r line; do case "$line" in *'"type":"user"'*) break ;; esac; done ;;
esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
`

// runEffortStandIn runs the real backend on task against the stand-in and
// answers the Result, every spawn's effort env, and every spawn's settings.
func runEffortStandIn(t *testing.T, task Task) (Result, []string, []string) {
	t.Helper()
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(effortStandInCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	effortLog, settingsLog := filepath.Join(dir, "effort.log"), filepath.Join(dir, "settings.log")
	t.Setenv("EFFORT_LOG", effortLog)
	t.Setenv("SETTINGS_FILE_LOG", settingsLog)
	t.Setenv("TMPDIR", t.TempDir())
	task.Command, task.WorkDir, task.UserPrompt = cli, dir, "x"
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
	res, err := b.Execute(context.Background(), task)
	t.Logf("Execute: err=%v", err)
	efforts, rerr := os.ReadFile(effortLog)
	if rerr != nil {
		t.Fatalf("the stand-in was never spawned: %v", rerr)
	}
	settings, _ := os.ReadFile(settingsLog)
	return res, strings.Fields(string(efforts)), strings.Split(strings.TrimSpace(string(settings)), "\n")
}

// TestClaudeCodeReportsTheEffortItHandsTheCLI runs the real backend against a
// stand-in CLI and compares the effort the Result reports with the one every
// spawn was started with: the node's level, `none` raised to `low`, the
// backend's default when the task names none. The level is PINNED: a task's
// ExtraEnv entry for the variable is dropped, and the --settings env block —
// which outranks a user or project settings `env` on the CLI (measured,
// TestCLILiveClaudeCodeEffort) — carries the same level.
//
// Mutation that reddens it: drop the reportClaudeCodeEffort defer from
// Execute; drop the claudeEffortLevelEnv pin from claudeEnvPinsAndDropped.
func TestClaudeCodeReportsTheEffortItHandsTheCLI(t *testing.T) {
	for _, tc := range []struct {
		name, handed, extra, want string
	}{
		{"high", "high", "", "high"},
		{"none", "none", "", "low"},
		{"no level", "", "", defaultClaudeCodeEffort},
		{"an ExtraEnv entry for the variable", "high", "low", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{ReasoningEffort: tc.handed, Model: "claude-opus-5-5"}
			if tc.extra != "" {
				task.ExtraEnv = []string{claudeEffortLevelEnv + "=" + tc.extra}
			}
			res, efforts, settings := runEffortStandIn(t, task)
			for _, got := range efforts {
				if got != tc.want {
					t.Errorf("a spawn got %s=%q, want %q", claudeEffortLevelEnv, got, tc.want)
				}
			}
			for _, s := range settings {
				if !strings.Contains(s, `"`+claudeEffortLevelEnv+`":"`+tc.want+`"`) {
					t.Errorf("a spawn's --settings env does not pin %s=%q: %s", claudeEffortLevelEnv, tc.want, s)
				}
			}
			if !res.WireReasoningEffortReported || res.WireReasoningEffort != tc.want || res.WireReasoningEffortLast != tc.want {
				t.Errorf("reported %+v, want %q on both ends, reported", [3]any{res.WireReasoningEffort, res.WireReasoningEffortLast, res.WireReasoningEffortReported}, tc.want)
			}
		})
	}
}

// claudeCLIEffortMeasurements is what claude CLI 2.1.282 put in a request's
// output_config.effort when started with CLAUDE_CODE_EFFORT_LEVEL=level for
// model, against a recording Anthropic endpoint (rva-s12-c, 2026-11-16;
// TestCLILiveClaudeCodeEffort re-measures it): "" is no effort sent.
var claudeCLIEffortMeasurements = []struct{ model, level, sent string }{
	{"claude-3-5-haiku-20241022", "high", ""},
	{"claude-3-5-sonnet-20241022", "high", ""},
	{"claude-3-5-sonnet-20241022", "max", ""},
	{"claude-haiku-4-5", "high", ""},
	{"claude-haiku-4-5", "low", ""},
	{"claude-sonnet-4-5", "high", ""},
	{"claude-sonnet-4-6", "xhigh", "high"},
	{"claude-opus-4-7", "high", "high"},
	{"claude-opus-5-5", "high", "high"},
	{"claude-sonnet-5", "high", "high"},
	{"claude-opus-5-5", "max", "max"},
	{"glm-5.3", "high", "high"},
	// The closing review's shapes: a registry row whose matrix was not yet
	// documented, a dated snapshot of a known model, a second level for the
	// no-effort models.
	{"claude-sonnet-4-7", "high", "high"},
	{"claude-sonnet-4-7", "xhigh", "xhigh"},
	{"claude-sonnet-4-7", "low", "low"},
	{"claude-sonnet-4-7", "medium", "medium"},
	{"claude-sonnet-4-7", "max", "max"},
	{"claude-opus-5-5-20260101", "high", "high"},
	{"claude-opus-5-5-20260101", "xhigh", "xhigh"},
	{"claude-sonnet-4-5", "medium", ""},
	{"claude-haiku-4-5-20251001", "high", ""},
	// Recorded on CLI 2.1.282: bare aliases with a date are sent as handed.
	// Canonical registry lookup would wrongly report high/none for these.
	{"sonnet-4-6-20260101", "xhigh", "xhigh"},
	{"haiku-20260101", "xhigh", "xhigh"},
}

// claudeCLIEffortPrefixedMeasurements are the id spellings the tree writes
// that the bare table cannot carry: a slot-prefixed spec the spawn strips
// before the CLI sees it (its sent value is the stripped id's measurement,
// proven end to end by the closing review's probe against the real CLI), and
// a Claude id with no registry matrix and no measurement — the CLI's
// behaviour for it is not known (a fabricated id would not run), so the
// report degrades to the level as handed rather than claim none.
var claudeCLIEffortPrefixedMeasurements = []struct{ model, level, sent string }{
	{"anthropic/claude-sonnet-4-6", "xhigh", "high"},
	{"anthropic/claude-haiku-4-5-20251001", "high", ""},
	{"claude-nova-9", "high", "high"},
}

// TestClaudeCodeReportsWhatTheCLISendsForItsModel pins the CLI's own rule:
// the backend reports, model by model, the level the CLI was measured
// sending — none for the measured no-effort ids, a clamp onto the model's
// accepted set, any other id as handed — through the real backend and its
// stand-in, on the bare ids and on the prefixed spellings the tree writes.
// A call that never reaches the model reports none.
//
// Mutations that redden it: report claudeCodeSpawnEffort instead of
// claudeCodeWireEffort; report a level when the call reached no model;
// resolve the task's model without the spawn's prefix strip (the prefixed
// rows); drop the registry's date normalization (the dated rows); report ""
// for an id with no matrix instead of the handed level (the unmeasured row);
// drop the measured no-effort set (the sonnet-4-5 and haiku rows).
func TestClaudeCodeReportsWhatTheCLISendsForItsModel(t *testing.T) {
	for _, m := range claudeCLIEffortMeasurements {
		t.Run(m.model+"/"+m.level, func(t *testing.T) {
			res, _, _ := runEffortStandIn(t, Task{Model: m.model, ReasoningEffort: m.level})
			if !res.WireReasoningEffortReported || res.WireReasoningEffort != m.sent {
				t.Errorf("reported %q (reported=%v), the CLI sends %q", res.WireReasoningEffort, res.WireReasoningEffortReported, m.sent)
			}
		})
	}
	for _, m := range claudeCLIEffortPrefixedMeasurements {
		t.Run("prefixed "+m.model+"/"+m.level, func(t *testing.T) {
			res, _, _ := runEffortStandIn(t, Task{Model: m.model, ReasoningEffort: m.level})
			if !res.WireReasoningEffortReported || res.WireReasoningEffort != m.sent {
				t.Errorf("reported %q (reported=%v), the CLI sends %q for the stripped id", res.WireReasoningEffort, res.WireReasoningEffortReported, m.sent)
			}
		})
	}
	t.Run("a call that reaches no model", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "no-such-claude")
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
		res, err := b.Execute(context.Background(), Task{Command: missing, WorkDir: dir, UserPrompt: "x", Model: "claude-opus-5-5", ReasoningEffort: "high"})
		if err == nil {
			t.Fatal("premise: a missing CLI should fail the call")
		}
		if !res.WireReasoningEffortReported || res.WireReasoningEffort != "" {
			t.Errorf("reported %q (reported=%v) for a call that never reached the model", res.WireReasoningEffort, res.WireReasoningEffortReported)
		}
	})
}
