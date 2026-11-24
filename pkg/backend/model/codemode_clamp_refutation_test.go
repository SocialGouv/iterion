package model

// The run-level codemode override (#3345): guarantee witnesses produced by
// the adversarial round. Each pins a property the rung must keep — an
// override cannot bypass Clamp, a negative field cannot smuggle itself past
// the authored rungs, a garbage mode cannot arm anything, and a schema
// re-ask cannot revive the rung.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// Probe A: the launch bounds override, resolved through codemodePolicy, runs
// a REAL program under Clamp — above-ceiling fields land at the ceiling, the
// below-floor memory lands at the floor, and OnClamp names both. This is the
// executed refutation of "an override can bypass Clamp".
func TestTheOverrideRunsUnderClampLikeAnyAuthor(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{
		codemodeOverride: "on",
		codemodeBoundsOverride: &ir.CodemodeBounds{
			ToolCalls: 1 << 40, // far above Ceiling 4096
			Wall:      time.Hour,
			Memory:    1 << 20, // below the 64 MiB floor
		},
	}
	_, bounds, err := e.codemodePolicy(backendFields{id: "n", codemode: "on"}, backendcaps.Claw)
	if err != nil {
		t.Fatalf("codemodePolicy: %v", err)
	}
	if bounds == nil {
		t.Fatal("bounds nil with an override set")
	}
	if bounds.ToolCalls != 1<<40 || bounds.Memory != 1<<20 {
		t.Fatalf("resolver did not carry the override verbatim: %+v", bounds)
	}

	var requested, applied scripthost.Limits
	clamped := false
	host := scripthost.Host{
		Binary:                       os.Args[0],
		AllowSingleThreadConfinement: true,
		OnClamp:                      func(r, a scripthost.Limits) { requested, applied, clamped = r, a, true },
	}
	_, ctx := newToolDispatcher(context.Background(), map[string]*GenerationTool{}, nil, nil, nil, nil, nil)
	in, err := json.Marshal(map[string]string{"source": "1+1"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := codemodeTool(host, *bounds, codemodeDescription).Execute(ctx, in)
	if err != nil {
		t.Fatalf("program run: %v", err)
	}
	if strings.TrimSpace(out) != "2" {
		t.Fatalf("program returned %q, want 2", out)
	}
	if !clamped {
		t.Fatal("OnClamp never fired — an override above the ceiling was served verbatim (bypass)")
	}
	ceil := scripthost.Ceiling()
	if applied.ToolCalls != ceil.ToolCalls || applied.Wall != ceil.Wall {
		t.Errorf("applied = %+v, want ToolCalls %d / Wall %s (the ceiling)", applied, ceil.ToolCalls, ceil.Wall)
	}
	if applied.Memory != scripthost.MemoryFloor().Memory {
		t.Errorf("applied.Memory = %d, want the %d floor", applied.Memory, scripthost.MemoryFloor().Memory)
	}
	if requested.ToolCalls != 1<<40 {
		t.Errorf("OnClamp's requested.ToolCalls = %d, want the override's %d", requested.ToolCalls, int64(1)<<40)
	}
}

// A NEGATIVE field on the bounds override is dropped by the ladder (r > 0
// fails) and the authored value stands. The ENTRY surfaces refuse a negative
// outright (the API's toBounds, the CLI flags) — this witness pins the
// resolver's half of the same decision, so a future surface that forgets to
// refuse still cannot turn a negative into an elevation.
func TestANegativeOverrideFieldDropsToAuthored(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{
		codemodeOverride:       "on",
		codemodeBoundsOverride: &ir.CodemodeBounds{ToolCalls: -5},
	}
	_, bounds, err := e.codemodePolicy(backendFields{
		id:             "n",
		codemode:       "on",
		codemodeBounds: &ir.CodemodeBounds{ToolCalls: 64},
	}, backendcaps.Claw)
	if err != nil {
		t.Fatalf("codemodePolicy: %v", err)
	}
	if bounds == nil || bounds.ToolCalls != 64 {
		t.Errorf("a negative override field resolved %+v, want the authored 64 standing", bounds)
	}
}

// A garbage mode string on the run rung resolves OFF from the launch — and
// this executor-level witness documents the resolver's half; the runner's
// spec builder refuses an invalid message value outright, so the garbage
// cannot reach here from the wire.
func TestAGarbageModeOnTheExecutorResolvesOff(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{codemodeOverride: "maybe"}
	policy, _, err := e.codemodePolicy(backendFields{id: "n", codemode: "on"}, backendcaps.Claw)
	if err != nil {
		t.Fatalf("codemodePolicy: %v", err)
	}
	if policy.On {
		t.Errorf("garbage mode %q resolved On — worse than expected", "maybe")
	}
	t.Logf("garbage run mode resolves On=%v Source=%q — silently disabled, no error", policy.On, policy.Source)
	if ir.ValidCodemode("maybe") {
		t.Error("ValidCodemode accepted garbage — the surfaces would not even refuse it")
	}
}

// The schema re-ask must not revive the launch rung. The executor carries
// BOTH overrides; the re-ask of a codemode task must come back with
// Codemode=false AND nil bounds even so.
func TestAReaskDoesNotReviveTheLaunchRung(t *testing.T) {
	t.Parallel()
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithCodemodeOverride("on"),
		WithCodemodeBoundsOverride(&ir.CodemodeBounds{ToolCalls: 4096}))
	task := &delegate.Task{
		NodeID:                 "n",
		Model:                  "test/test-model",
		UserPrompt:             "go",
		OutputSchema:           json.RawMessage(`{"type":"object"}`),
		Codemode:               true,
		CodemodeBounds:         &ir.CodemodeBounds{ToolCalls: 4096},
		HasTools:               true,
		AllowedTools:           []string{"Bash"},
		ResumeConversation:     []byte(`[{"role":"user","content":[{"type":"text","text":"hi"}]}]`),
		ResumePendingToolUseID: "tu_1",
		ResumeAnswer:           "yes",
	}
	r := exec.planSchemaReask(context.Background(), delegate.BackendClaw, task, delegate.Result{}, "schema-feedback")
	if r.task.Codemode {
		t.Error("the re-ask revived the capability despite the tool-less arm")
	}
	if r.task.CodemodeBounds != nil {
		t.Errorf("the re-ask carried bounds %+v — a launch rung came back to life on a tool-less turn", r.task.CodemodeBounds)
	}
}
