package delegate

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// TestTheClaudeCodeHookRefusesAClimbOutOfANarrowAllow is claude_code's half
// of #2918's backend parity: the REAL Execute registers its PreToolUse gate on
// the spawn's stdin, the stand-in CLI calls it back with a native `Read`, and
// the verdict it writes is what the CLI obeys. A spelling whose text sits under
// the node's narrow allow and whose file does not is refused; the allowed tree
// still reads. claw's half is TestTheClawToolLoopRefusesAClimbOutOfANarrowAllow
// (pkg/backend/model), which feeds the same calls to the claw tool loop. A
// call carrying a second path key is refused here too, though claude_code's
// Read honours `file_path`: claw's read_file honours `path`, and the gate
// opens a call only where the rule opens every path it names.
func TestTheClaudeCodeHookRefusesAClimbOutOfANarrowAllow(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	for _, tc := range []struct {
		name  string
		input map[string]any
		deny  bool
	}{
		{"absolute climb", map[string]any{"file_path": "/work/docs/../../etc/passwd"}, true},
		{"relative climb", map[string]any{"file_path": "docs/../../etc/passwd"}, true},
		{"trailing climb", map[string]any{"file_path": "/work/docs/.."}, true},
		{"a second path key beside the allowed one", map[string]any{"file_path": "/work/docs/x.md", "path": "/work/docs/../private/key"}, true},
		{"inside the allowed tree", map[string]any{"file_path": "/work/docs/x.md"}, false},
		{"relative, inside the allowed tree", map[string]any{"file_path": "docs/x.md"}, false},
		{"both keys inside the allowed tree", map[string]any{"file_path": "/work/docs/x.md", "path": "docs/y.md"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			t.Setenv("HOOK_PROBE_CALL", `"tool_name":"Read","tool_input":`+string(input))

			// The same tree twice: the absolute rule is met by the absolute
			// spellings, the relative one by the relative spellings.
			pol, err := permission.NewPolicy(permission.ModeDeny, []string{"Read(/work/docs/**)", "Read(docs/**)"}, nil, nil)
			if err != nil {
				t.Fatalf("NewPolicy: %v", err)
			}
			pol.SetPathRoot("/work")
			rec, stdin := spawnRecordedWithStdin(t, Task{
				NodeID:        "n",
				Permission:    pol,
				AllowedTools:  []string{"read_file"},
				ToolsDeclared: true,
				OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
			})
			if len(stdin) < 2 {
				t.Fatalf("expected the work pass and the structured-output pass the probe answers on, got %d spawn(s): %v", len(stdin), rec.argv)
			}
			v := readHookVerdict(t, stdin[1])
			if !v.found || v.subtype != "success" {
				t.Fatalf("the hook answered no verdict for Read(%v) (found=%v subtype=%q): %v", tc.input, v.found, v.subtype, stdin[1])
			}
			if tc.deny && v.decision != "deny" {
				t.Errorf("Read(%v) under allow Read(/work/docs/**) and Read(docs/**) = %q, want deny (reason %q)", tc.input, v.decision, v.reason)
			}
			if !tc.deny && v.hasOutput {
				t.Errorf("Read(%v) inside the allowed tree was answered %q (reason %q), want no decision — the CLI then runs it", tc.input, v.decision, v.reason)
			}
		})
	}
}
