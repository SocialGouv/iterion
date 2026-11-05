package model

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestTheClawToolLoopRefusesAClimbOutOfANarrowAllow is claw's half of
// #2918's backend parity, driven through the path an agent reaches: the
// policy the REAL task build arms for a node (resolvePermissionPolicy, its
// path root and its secret trees included) judging a read_file call in the
// claw tool loop. A spelling whose text sits under the node's narrow allow
// and whose file does not must never reach the tool; claude_code's half is
// TestTheClaudeCodeHookRefusesAClimbOutOfANarrowAllow (pkg/backend/delegate),
// which feeds the same calls to the PreToolUse hook a spawn registers.
func TestTheClawToolLoopRefusesAClimbOutOfANarrowAllow(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	wf := &ir.Workflow{
		Permission: "deny",
		// The same tree twice: the absolute rule is met by the absolute
		// spellings, the relative one by the relative spellings.
		PermissionAllow: []string{"Read(" + filepath.Join(work, "docs") + "/**)", "Read(docs/**)"},
	}
	task := buildNodeTask(t, nodeRulesExecutor(t, wf, WithWorkDir(work)), nodeRulesAgent(nil))

	for _, tc := range []struct {
		name string
		path string
		runs bool
	}{
		{"absolute climb", filepath.Join(work, "docs") + "/../../../etc/passwd", false},
		{"relative climb", "docs/../../../etc/passwd", false},
		{"trailing climb", filepath.Join(work, "docs") + "/..", false},
		{"inside the allowed tree", filepath.Join(work, "docs", "x.md"), true},
		{"relative, inside the allowed tree", "docs/x.md", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var ran []string
			tools := map[string]*GenerationTool{
				"read_file": {
					Name: "read_file",
					Execute: func(_ context.Context, raw json.RawMessage) (string, error) {
						mu.Lock()
						defer mu.Unlock()
						ran = append(ran, string(raw))
						return "contents", nil
					},
				},
			}
			in, err := json.Marshal(map[string]any{"path": tc.path})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			results, gErr := executeToolsDirect(context.Background(),
				[]toolUseBlock{{ID: "tu_1", Name: "read_file", PartialJSON: string(in)}},
				tools, nil, nil, nil, nil, task.Permission)
			if gErr != nil {
				t.Fatalf("executeToolsDirect: %v", gErr)
			}
			mu.Lock()
			executed := len(ran) > 0
			mu.Unlock()
			if executed != tc.runs {
				t.Fatalf("read_file(%q) executed = %v, want %v", tc.path, executed, tc.runs)
			}
			if tc.runs {
				return
			}
			raw, err := json.Marshal(results)
			if err != nil {
				t.Fatalf("marshal results: %v", err)
			}
			if !strings.Contains(string(raw), "Permission denied") {
				t.Errorf("the refused read must hand the model the gate's refusal: %s", raw)
			}
		})
	}

	// The gate a sandboxed claw runner rebuilds from the serialised policy
	// reaches the same verdict.
	rebuilt, err := permission.NewPolicyFromConfig(task.Permission.Config())
	if err != nil {
		t.Fatalf("NewPolicyFromConfig: %v", err)
	}
	assertDecision(t, rebuilt, "read_file", map[string]any{"path": "docs/../../../etc/passwd"},
		permission.Deny, "the runner-side gate refuses the climb the engine refuses")
}
