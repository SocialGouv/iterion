package runview

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run doc is the only carrier a resume surface that cannot re-state an
// override has (the studio's answers, the retry sweeper): the launch stamped
// its codemode choice on the doc, and resumeExecutorSpec replays it into the
// executor spec. A replay gone deaf here re-arms a capability the operator
// killed at launch — the fail-open direction (CM-5e, #3345; the hole the
// adversarial round graded high).
func TestTheRunDocReplaysTheCodemodeOverrideOnResume(t *testing.T) {
	t.Parallel()
	svc := &Service{workDir: "/service-root"}
	wf := &ir.Workflow{Name: "resume-codemode"}
	bounds := &scripthost.Limits{ToolCalls: 4096, Wall: 90 * time.Second}
	spec := svc.resumeExecutorSpec(wf, &store.Run{
		CodemodeOverride:       "off",
		CodemodeBoundsOverride: bounds,
	}, nil, "")
	if spec.Codemode != "off" {
		t.Errorf("replayed Codemode = %q, want the doc's %q — the resume would run the authored value", spec.Codemode, "off")
	}
	if spec.CodemodeBounds == nil || spec.CodemodeBounds.ToolCalls != 4096 || spec.CodemodeBounds.Wall != 90*time.Second {
		t.Errorf("replayed CodemodeBounds = %+v, want the doc's raise", spec.CodemodeBounds)
	}

	// A run launched without an override resumes without one — the replay
	// must not conjure a choice nobody made.
	plain := svc.resumeExecutorSpec(wf, &store.Run{}, nil, "")
	if plain.Codemode != "" || plain.CodemodeBounds != nil {
		t.Errorf("a doc with no override replayed Codemode=%q bounds=%+v — inherit must read absent", plain.Codemode, plain.CodemodeBounds)
	}
}
