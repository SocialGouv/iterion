package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestResume_theClaimHookFiresAtTheClaim: WithOnResumeClaimed fires once,
// when a resume claims the run and before its first node runs; a resume
// refused before its claim never fires it.
func TestResume_theClaimHookFiresAtTheClaim(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-claim-hook"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "facts.json"), []byte("{}"), 0o644)
	})
	reportRan := false
	x.on("report", func(map[string]any) (map[string]any, error) {
		reportRan = true
		return map[string]any{}, nil
	})
	first := scratchEngine(s, x, d)
	first.workflowHash = "sha256:launch"
	if err := first.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	fired, firedAfterReport := 0, false
	resumer := func(hash string) *Engine {
		e := New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithOnResumeClaimed(func() {
			fired++
			firedAfterReport = reportRan
		}), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		}))
		e.workflowHash = hash
		return e
	}
	if err := resumer("sha256:edited").Resume(ctx, runID, map[string]any{"ok": true}); !IsWorkflowSourceChanged(err) || fired != 0 {
		t.Fatalf("a resume refused before its claim: err=%v fired=%d, want the source refusal and no claim", err, fired)
	}
	if err := resumer("sha256:launch").Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if fired != 1 || firedAfterReport || !reportRan {
		t.Fatalf("the claim hook fired %d times (after the first node: %v; the node ran: %v), want once, before it", fired, firedAfterReport, reportRan)
	}
}
