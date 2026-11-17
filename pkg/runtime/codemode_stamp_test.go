package runtime

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The run-doc stamp of the launch-time overrides (CM-5e, #3345; tour 2
// finding L2): the stamp's outer save guard must treat an override choice as
// reason enough to write the doc. An engine carrying ONLY an override is
// what a resume re-statement produces — the re-statement persists, or the
// next silent resume replays the previous decision and re-arms a capability
// the operator killed. Executed by the tour-2 reviewer against the guard
// without the terms: the doc came back unchanged.
func TestAnOverrideOnlyEngineStampsTheRunDoc(t *testing.T) {
	t.Parallel()
	s := tmpStore(t)
	bounds := &scripthost.Limits{ToolCalls: 4096, Wall: 90 * time.Second}

	// The codemode override alone — no workflow hash, no source, none of the
	// guard's other reasons to save.
	e := New(pinWorkflow("override-only", nil), s, newStubExecutor(),
		WithLogger(iterlog.Nop()),
		WithCodemodeOverride("off", bounds),
	)
	run, err := e.runResolveDoc(context.Background(), "run-override-only", nil)
	if err != nil {
		t.Fatalf("runResolveDoc: %v", err)
	}
	if run.CodemodeOverride != "off" {
		t.Errorf("doc CodemodeOverride = %q, want the engine's %q — the stamp never reached the doc", run.CodemodeOverride, "off")
	}
	if run.CodemodeBoundsOverride == nil || run.CodemodeBoundsOverride.ToolCalls != 4096 {
		t.Errorf("doc CodemodeBoundsOverride = %+v, want the engine's raise", run.CodemodeBoundsOverride)
	}

	// And the persisted value survives a SECOND pass that carries no
	// override (the silent resume): the absence must not clobber it.
	e2 := New(pinWorkflow("override-only", nil), s, newStubExecutor(), WithLogger(iterlog.Nop()))
	run2, err := e2.runResolveDoc(context.Background(), "run-override-only", nil)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if run2.CodemodeOverride != "off" {
		t.Errorf("silent resume read CodemodeOverride = %q, want the persisted %q — the replay would re-arm the capability", run2.CodemodeOverride, "off")
	}
	if run2.CodemodeBoundsOverride == nil || run2.CodemodeBoundsOverride.ToolCalls != 4096 {
		t.Errorf("silent resume read bounds %+v, want the persisted raise", run2.CodemodeBoundsOverride)
	}
}
