package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// dispatchSecretPauseBot declares a secret with an inline value and pauses
// at once on a human entry node — no LLM, no shell — so Dispatch reaches
// BuildExecutor (where the run's guard is built and installed on the run's
// logger) and returns, with no backend credentials in play.
const dispatchSecretPauseBot = `vars:
  planted: string = ""

secrets:
  db: "{{vars.planted}}"

schema gate_out:
  approve: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow dispatch_redactor:
  entry: gate
  gate -> done when approve
  gate -> fail when not approve
`

// The dispatcher surface builds its per-run logger at store.TeeRunLog
// (engine_runner.go), and the handle it returns is the one this surface
// hands BuildExecutor and the engine. The slot is per-handle, so the
// handle must never be the runner's own process logger: BuildExecutor
// would install THIS run's matcher on the logger every dispatched run of
// this process shares, and the next run would replace it mid-flight
// (iterion#2822). The witness drives the real Dispatch of a secret-declaring
// bot — pausing at the human entry, so no backend runs — and then logs on
// the process logger itself: the line must come back whole.
//
// The value-carrying text a dispatch logs THROUGH the run's handle is the
// supervisor's steer (pkg/supervise, witnessed there) and the engine's own
// lines (pkg/runtime, witnessed there); this witness is the surface's
// wiring: which handle the run's matcher landed on.
func TestTheDispatchersRunNeverInstallsItsMatcherOnTheProcessLogger(t *testing.T) {
	const value = "hunter2-9f8e7d6c5b4a"
	const runID = "run-dispatch-redactor"
	storeDir := t.TempDir()

	var procBuf bytes.Buffer
	runner, err := NewEngineRunner(writeDispatchSecretPauseBot(t), iterlog.New(iterlog.LevelInfo, &procBuf))
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = runner.Close() }()

	derr := runner.Dispatch(context.Background(), DispatchSpec{
		RunID:         runID,
		WorkspacePath: t.TempDir(),
		StoreDir:      storeDir,
		Vars:          map[string]any{"planted": value},
	})
	// The premise of this witness: the dispatch ran to its human gate, so
	// it crossed BuildExecutor — where the run's guard is built and the
	// matcher is installed on the handle this surface handed it.
	if !errors.Is(derr, runtime.ErrRunPaused) {
		t.Fatalf("Dispatch of a human-entry bot returned %v, want ErrRunPaused", derr)
	}

	// The run got as far as its executor — the tee exists for this run, so
	// the surface really built a per-run handle and drove it to BuildExecutor.
	if _, err := os.Stat(filepath.Join(storeDir, "runs", runID, "run.log")); err != nil {
		t.Fatalf("run.log for the dispatched run: %v", err)
	}

	procBuf.Reset()
	runner.logger.Warn("dispatcher pod line: %s", value)
	if got := procBuf.String(); !strings.Contains(got, value) {
		t.Errorf("the dispatched run installed its matcher on the runner's process logger, which every dispatched run shares: %q", got)
	}
	if placeholder := secretguard.PlaceholderForName("db"); strings.Contains(procBuf.String(), placeholder) {
		t.Errorf("the process logger shows the run's placeholder where its own line should stay whole: %q", procBuf.String())
	}
}

func writeDispatchSecretPauseBot(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dispatch-redactor.bot")
	if err := os.WriteFile(p, []byte(dispatchSecretPauseBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	return p
}
