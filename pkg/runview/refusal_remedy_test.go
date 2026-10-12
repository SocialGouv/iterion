package runview

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestBuildDiagnostic_projectsTheRemedyARefusalNames: the diagnostic every
// surface reads (CLI, Studio, Copi, watchers) keeps the remedy a refusal's
// record carries — the consent it names, and that --force is needed too.
func TestBuildDiagnostic_projectsTheRemedyARefusalNames(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "diagnostic-remedy"
	if _, err := st.CreateRun(ctx, runID, "workflow", nil); err != nil {
		t.Fatal(err)
	}
	const hint = "relaunch the run fresh; or resume it accepting the scratch's loss (--accept-scratch-loss) to continue as it stands"
	if err := st.UpdateRunStatusCoded(ctx, runID, store.RunStatusFailedResumable, "refused — hint: "+hint, store.FailureScratchNotPortable); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventRunRetrySkipped, RunID: runID, Data: map[string]any{
		"reason": "deterministic", "code": string(store.FailureScratchNotPortable), "error": "refused", "hint": hint, "also_needs_force": true,
	}}); err != nil {
		t.Fatal(err)
	}
	d, err := BuildDiagnostic(ctx, st, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Evidence) != 1 {
		t.Fatalf("evidence = %v, want the run_retry_skipped", d.Evidence)
	}
	details := d.Evidence[0].Details
	if details["hint"] != hint || details["also_needs_force"] != "true" {
		t.Fatalf("evidence details = %v, want the hint and also_needs_force", details)
	}
}

// TestLogRunOutcome_saysTheRemedyARefusalNames: the service's line for a run
// that ended on a refusal carries the remedy the refusal names, which the
// error's text leaves out.
func TestLogRunOutcome_saysTheRemedyARefusalNames(t *testing.T) {
	var buf bytes.Buffer
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.New(iterlog.LevelInfo, &buf)))
	if err != nil {
		t.Fatal(err)
	}
	refusal := &runtime.RuntimeError{Code: runtime.ErrCodeScratchNotPortable, Message: "run r1: the scratch banked at the last teardown is gone",
		Hint: "resume it accepting the scratch's loss (--accept-scratch-loss)"}
	svc.logRunOutcome("r1", fmt.Errorf("runtime: sandbox: %w", refusal))
	if out := buf.String(); !strings.Contains(out, "--accept-scratch-loss") {
		t.Fatalf("log line %q does not name the consent", out)
	}
}
