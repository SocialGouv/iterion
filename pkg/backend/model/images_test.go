package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// buildTask resolves node-level `images:` templates against the run input and
// forwards the survivors on delegate.Task.Images for the codex backend. Two
// classes of entry are dropped as "no image this run": an empty/whitespace
// result, and a still-unresolved template (an optional ref whose input key was
// absent, which resolveTemplate leaves as the verbatim "{{...}}" marker —
// forwarding that to codex as `-i {{...}}` would be a bogus path).
func TestBuildTaskResolvesAndDropsImages(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf)

	node := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "kf"},
		LLMFields: ir.LLMFields{
			Model:   "test/test-model",
			Backend: delegate.BackendCodex,
			Images: []string{
				"{{input.prev_frame}}",      // resolves → kept
				"seed.png",                  // literal → kept
				"{{input.empty}}",           // present but empty → dropped
				"{{input.identity_anchor}}", // absent key → unresolved marker → dropped
				"   ",                       // whitespace → dropped
			},
		},
	}

	f, err := extractBackendFields(node)
	if err != nil {
		t.Fatalf("extractBackendFields: %v", err)
	}

	input := map[string]any{
		"prev_frame": "/tmp/frame_003.png",
		"empty":      "",
	}
	task, err := exec.buildTask(context.Background(), node, f, input, delegate.BackendCodex)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}

	want := []string{"/tmp/frame_003.png", "seed.png"}
	if len(task.Images) != len(want) {
		t.Fatalf("task.Images = %v, want %v", task.Images, want)
	}
	for i, w := range want {
		if task.Images[i] != w {
			t.Errorf("task.Images[%d] = %q, want %q", i, task.Images[i], w)
		}
	}
}
