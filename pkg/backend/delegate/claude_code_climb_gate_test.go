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
// (pkg/backend/model), which feeds the same calls to the claw tool loop.
func TestTheClaudeCodeHookRefusesAClimbOutOfANarrowAllow(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	for _, tc := range []struct {
		name string
		path string
		deny bool
	}{
		{"absolute climb", "/work/docs/../../etc/passwd", true},
		{"relative climb", "docs/../../etc/passwd", true},
		{"trailing climb", "/work/docs/..", true},
		{"inside the allowed tree", "/work/docs/x.md", false},
		{"relative, inside the allowed tree", "docs/x.md", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{"file_path": tc.path})
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
				t.Fatalf("the hook answered no verdict for Read(%q) (found=%v subtype=%q): %v", tc.path, v.found, v.subtype, stdin[1])
			}
			if tc.deny && v.decision != "deny" {
				t.Errorf("Read(%q) under allow Read(/work/docs/**) and Read(docs/**) = %q, want deny (reason %q)", tc.path, v.decision, v.reason)
			}
			if !tc.deny && v.hasOutput {
				t.Errorf("Read(%q) inside the allowed tree was answered %q (reason %q), want no decision — the CLI then runs it", tc.path, v.decision, v.reason)
			}
		})
	}
}
