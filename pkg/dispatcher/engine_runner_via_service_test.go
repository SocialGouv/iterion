package dispatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pauseBot pauses immediately on a human entry node — no LLM, no shell —
// so a Dispatch test observes the persisted run doc + emitted events with
// no backend credentials.
const pauseBot = `
schema gate_out:
  approve: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow via_service_demo:
  entry: gate
  gate -> done when approve
  gate -> fail when not approve
`

func writePauseBot(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pause.bot")
	if err := os.WriteFile(p, []byte(pauseBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	return p
}

type dispatchOutcome struct {
	err       error
	source    *store.RunSource
	status    store.RunStatus
	eventKind []string
}

type recordingRunLauncher struct {
	spec runview.LaunchSpec
}

func (l *recordingRunLauncher) LaunchAndWait(_ context.Context, spec runview.LaunchSpec) error {
	l.spec = spec
	return nil
}

// runOneDispatch drives a single EngineRunner.Dispatch of the pause bot and
// captures the terminal error, the persisted run metadata, and the SET of
// event types the OnEvent hook observed.
func runOneDispatch(t *testing.T, botPath, storeDir, runID string, launcher RunLauncher) dispatchOutcome {
	t.Helper()
	opts := []EngineRunnerOption(nil)
	if launcher != nil {
		opts = append(opts, WithRunLauncher(launcher))
	}
	runner, err := NewEngineRunner(botPath, iterlog.Nop(), opts...)
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = runner.Close() }()

	var (
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	spec := DispatchSpec{
		RunID:         runID,
		WorkspacePath: t.TempDir(),
		StoreDir:      storeDir,
		Vars:          map[string]any{"ignored": "x"},
		Issue:         &IssueRef{ID: "native:" + runID, Identifier: runID, Title: "Converge ADR-046"},
		OnEvent: func(name string) {
			mu.Lock()
			seen[name] = true
			mu.Unlock()
		},
	}
	derr := runner.Dispatch(context.Background(), spec)

	s, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	mu.Lock()
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	mu.Unlock()
	sort.Strings(kinds)
	return dispatchOutcome{err: derr, source: r.Source, status: r.Status, eventKind: kinds}
}

// TestEngineRunner_ViaServiceMatchesDirect is the ADR-046 diff-run: the
// same fresh dispatch, once through the direct engine and once through the
// shared launch authority, must produce byte-identical dispatcher-observable
// metadata — terminal error (pause), persisted Source, run status, and the
// SET of stall-heartbeat event types.
func TestEngineRunner_ViaServiceMatchesDirect(t *testing.T) {
	botPath := writePauseBot(t)

	// Both dispatches share ONE store dir (the convergence invariant: the
	// via-service Service writes the same records the dispatcher reads back).
	storeDir := t.TempDir()

	// --- Direct path (flag off) ---
	direct := runOneDispatch(t, botPath, storeDir, "run-direct-0001", nil)

	// --- Via-service path (flag on) ---
	t.Setenv("ITERION_DISPATCH_VIA_SERVICE", "1")
	svc, err := runview.NewService(storeDir, runview.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	viaSvc := runOneDispatch(t, botPath, storeDir, "run-viasvc-0002", ServiceRunLauncher{Svc: svc})

	// Terminal error: both suspend on the human gate.
	if !errors.Is(direct.err, runtime.ErrRunPaused) {
		t.Fatalf("direct path err = %v, want ErrRunPaused", direct.err)
	}
	if !errors.Is(viaSvc.err, runtime.ErrRunPaused) {
		t.Fatalf("via-service path err = %v, want ErrRunPaused (byte-identical retry/park contract)", viaSvc.err)
	}

	// Persisted status: paused_waiting_human on both.
	if direct.status != viaSvc.status {
		t.Errorf("status mismatch: direct=%q via-service=%q", direct.status, viaSvc.status)
	}
	if direct.status != store.RunStatusPausedWaitingHuman {
		t.Errorf("status = %q, want paused_waiting_human", direct.status)
	}

	// Source stamping: same shape (Kind + the issue back-reference) on both.
	if direct.source == nil || viaSvc.source == nil {
		t.Fatalf("Source not stamped: direct=%+v via-service=%+v", direct.source, viaSvc.source)
	}
	if direct.source.Kind != viaSvc.source.Kind || direct.source.Kind != store.RunSourceKindDispatcher {
		t.Errorf("Source.Kind mismatch: direct=%q via-service=%q", direct.source.Kind, viaSvc.source.Kind)
	}
	if direct.source.IssueID != "native:run-direct-0001" || viaSvc.source.IssueID != "native:run-viasvc-0002" {
		t.Errorf("Source.IssueID not stamped from the issue: direct=%q via-service=%q", direct.source.IssueID, viaSvc.source.IssueID)
	}

	// Stall heartbeat: the SET of event types OnEvent saw must be identical
	// (the store-level observer is a superset of engine-level events on both
	// paths; only multiplicity differs, which stall detection is immune to).
	if !equalStringSets(direct.eventKind, viaSvc.eventKind) {
		t.Errorf("OnEvent type set mismatch:\n direct     = %v\n via-service = %v", direct.eventKind, viaSvc.eventKind)
	}
	if len(direct.eventKind) == 0 {
		t.Error("direct path OnEvent never fired — stall detection would be blind")
	}
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDispatchViaServicePropagatesPreHookWorktreeAuthority(t *testing.T) {
	launcher := &recordingRunLauncher{}
	runner := &EngineRunner{
		workflowPath: "/tmp/authority.bot",
		launcher:     launcher,
		logger:       iterlog.Nop(),
	}
	authoritySince := time.Now().Add(-time.Second).UTC()
	err := runner.dispatchViaService(context.Background(), DispatchSpec{
		RunID:                  "run-authority",
		WorkspacePath:          "/tmp/workspace",
		WorktreeAuthoritySince: authoritySince,
	})
	if err != nil {
		t.Fatalf("dispatchViaService: %v", err)
	}
	if !launcher.spec.WorktreeAuthoritySince.Equal(authoritySince) {
		t.Fatalf(
			"LaunchSpec authority=%s, want dispatcher boundary %s",
			launcher.spec.WorktreeAuthoritySince.Format(time.RFC3339Nano),
			authoritySince.Format(time.RFC3339Nano),
		)
	}
}
