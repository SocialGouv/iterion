package runtime

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// A tool node that prints a declared secret, with a node boundary behind it:
// the engine snapshots the workspace when the node finishes, and the
// diagnostic it logs when that capture fails is a NON-event line — the class
// iterion#2822 names.
const snapshotSecretBot = `tool readcfg:
  command: "cat creds.txt"

workflow w:
  worktree: none
  sandbox: none
  entry: readcfg
  readcfg -> done
`

// failingCaptureTracker is the real native tracker with ONE fault injected on
// the seam the engine declares (WithWorkspaceTracker): Capture fails, and its
// error quotes the workspace file the node wrote — the shape a capture error
// takes when it names what it could not read. The engine's own diagnostic,
// the logger and the run.log tee are production code; only the failure is
// injected, because a capture that fails on its own terms is a timing
// accident (an unreadable file, a MaxFiles overflow) and a witness owes a
// deterministic one.
type failingCaptureTracker struct {
	workspacetrack.Tracker
	file string
}

func (f *failingCaptureTracker) Capture(_, workspaceDir, _ string) (*workspacetrack.Snapshot, error) {
	body, err := os.ReadFile(filepath.Join(workspaceDir, f.file))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.file, err)
	}
	return nil, fmt.Errorf("capture %s: %s", f.file, strings.TrimSpace(string(body)))
}

// TestRunLogRedactsTheWorkspaceSnapshotDiagnostic is iterion#2822's
// acceptance: a run whose tool prints a declared secret and whose workspace
// snapshot fails leaves the PLACEHOLDER in run.log, never the value.
//
// The assertion is on run.log, not on an event: the event half is already
// scrubbed at Engine.emitBranch, so an event-only test would be green
// whatever the logger does. Measured before the fix, the same run wrote
// `workspace capture: node "readcfg" iter 0: capture creds.txt: db_password:
// hunter2-…` to run.log one line under a node_finished line that showed
// `result: db_password: __ITERION_SECRET_DB__`.
func TestRunLogRedactsTheWorkspaceSnapshotDiagnostic(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	const runID = "log-redact-snapshot"

	wf := compileBotText(t, snapshotSecretBot)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "creds.txt"), []byte("db_password: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig())
	exec := model.NewClawExecutor(nil, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard))
	t.Cleanup(func() { _ = exec.Close() })

	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	logger, closer := store.TeeRunLog(iterlog.Nop(), iterlog.LevelInfo, storeDir, runID)
	if closer == nil {
		t.Fatalf("scenario broken: no run.log tee for %s", runID)
	}
	t.Cleanup(func() { _ = closer.Close() })

	tracker := &failingCaptureTracker{Tracker: workspacetrack.NewNative(t.TempDir()), file: "creds.txt"}
	e := New(wf, s, exec,
		WithWorkDir(ws),
		WithSandboxOverride("none"),
		WithWorkspaceTracker(tracker),
		WithLogger(logger),
	)
	if err := e.Run(t.Context(), runID, nil); err != nil {
		t.Fatalf("the bot is a single tool node: %v", err)
	}

	log := runLogOf(t, storeDir, runID)
	diag := ""
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "workspace capture") {
			diag = line
		}
	}
	if diag == "" {
		t.Fatalf("scenario broken: the capture never failed into run.log:\n%s", log)
	}
	if strings.Contains(diag, secret) {
		t.Errorf("run.log's snapshot diagnostic carries the value: %q", diag)
	}
	ph := secretguard.PlaceholderForName("DB")
	if !strings.Contains(diag, ph) {
		t.Errorf("run.log's snapshot diagnostic = %q, want %s where the value was (an absence alone proves nothing)", diag, ph)
	}
	// The diagnosis an operator greps must survive the pass: a scrub that
	// eats the line is not one.
	if !strings.Contains(diag, `node "readcfg"`) || !strings.Contains(diag, "capture creds.txt") {
		t.Errorf("run.log's snapshot diagnostic lost what it diagnoses: %q", diag)
	}
}

// newScrubbingExecutor is the package's scrubbingExecutor (a stub carrying
// the SecretScrubber of a run that knows one secret) over one known value —
// the production shape (ClawExecutor.ScrubOutput) at stub cost.
func newScrubbingExecutor(secret string) scrubbingExecutor {
	return scrubbingExecutor{
		stubExecutor: newStubExecutor(),
		guard:        secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig()),
	}
}

// Both constructors join the two halves — the launch surface's per-run logger
// and the executor's secret matcher — so neither can be the one that forgets.
// Ten engine-building call sites pass a logger they built themselves (the
// CLI, the dispatcher, the runner loop and its subbots, the studio service);
// the engine is where they all arrive.
func TestBothEngineConstructorsRedactTheirLogger(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	ph := secretguard.PlaceholderForName("DB")

	for _, tc := range []struct {
		name string
		make func(t *testing.T, logger *iterlog.Logger) *Engine
	}{
		{"New", func(t *testing.T, logger *iterlog.Logger) *Engine {
			return New(minimalReviewWorkflow(), tmpStore(t), newScrubbingExecutor(secret), WithLogger(logger))
		}},
		{"NewFromRecipe", func(t *testing.T, logger *iterlog.Logger) *Engine {
			spec := &recipe.RecipeSpec{Name: "r", WorkflowRef: recipe.WorkflowRef{Name: minimalReviewWorkflow().Name}}
			e, err := NewFromRecipe(spec, minimalReviewWorkflow(), tmpStore(t), newScrubbingExecutor(secret), WithLogger(logger))
			if err != nil {
				t.Fatalf("NewFromRecipe: %v", err)
			}
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := iterlog.New(iterlog.LevelInfo, &buf)
			e := tc.make(t, logger)

			e.logger.Warn("snapshot: node %q iter %d: %v\noutput: %s", "implement", 0, "exit status 128", "fatal: token "+secret)
			out := buf.String()
			if strings.Contains(out, secret) {
				t.Errorf("the engine's logger wrote the value: %q", out)
			}
			if !strings.Contains(out, ph) {
				t.Errorf("the engine's logger wrote %q, want %s where the value was", out, ph)
			}

			// The fork does not reach back: the logger the caller handed in
			// keeps its own reading, so a run's matcher cannot replace
			// another run's on a process-wide logger (the studio service
			// runs many runs in one process).
			buf.Reset()
			logger.Warn("caller line with %s", secret)
			if !strings.Contains(buf.String(), secret) {
				t.Error("the caller's own logger was mutated — WithRedactor must fork")
			}
		})
	}
}

// The chokepoint is only one while every constructor goes through it. A second
// &Engine{} literal that applied its options and returned would hand the
// engine a logger with no pass, silently — the defect iterion#2822 names.
func TestEngineSweep_EveryEngineLiteralInstallsTheLogRedactor(t *testing.T) {
	literal := regexp.MustCompile(`&Engine\{`)
	install := regexp.MustCompile(`e\.installLogRedactor\(\)`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	literals, installs := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		n := len(literal.FindAllString(string(body), -1))
		if n > 0 && name != "engine.go" {
			t.Errorf("%s builds an Engine literal — a constructor lives in engine.go and installs the log redactor there (iterion#2822)", name)
		}
		literals += n
		installs += len(install.FindAllString(string(body), -1))
	}
	if literals != installs {
		t.Errorf("engine.go holds %d Engine literals and %d installLogRedactor calls — a constructor that skips the install writes raw third-party text to run.log", literals, installs)
	}
	if literals != 1 {
		t.Errorf("engine.go holds %d Engine literals, want the 1 constructor (New; NewFromRecipe delegates to it) — update this sweep with the second", literals)
	}
}

// keyDroppingLogScrubber is an executor whose scrubber does not return the key
// it was given — the one shape the pass cannot trust.
type keyDroppingLogScrubber struct{ *stubExecutor }

func (keyDroppingLogScrubber) ScrubOutput(map[string]any) map[string]any { return map[string]any{} }

// Fail closed, not open: a scrubber that drops the line gets a marker in
// run.log, never the raw text. The marker IS the report — the engine cannot
// log this through the logger whose redactor just failed.
func TestTheLogRedactorFailsClosedOnAScrubberThatDropsTheLine(t *testing.T) {
	var buf bytes.Buffer
	e := New(minimalReviewWorkflow(), tmpStore(t), keyDroppingLogScrubber{newStubExecutor()},
		WithLogger(iterlog.New(iterlog.LevelInfo, &buf)))
	e.logger.Warn("snapshot: token hunter2-9f8e7d6c5b4a")
	out := buf.String()
	if strings.Contains(out, "hunter2-9f8e7d6c5b4a") {
		t.Errorf("a dropped line was written raw: %q", out)
	}
	if !strings.Contains(out, unscrubbableLogLine) {
		t.Errorf("wrote %q, want the withheld marker", out)
	}
}

// An executor with no scrubber is a run with no known values — and
// ITERION_SECRETS_REDACT=off builds exactly such a guard, which is the kill
// switch. It wires no redactor and pays nothing per line.
func TestAnExecutorWithoutAScrubberWiresNoLogRedactor(t *testing.T) {
	var buf bytes.Buffer
	e := New(minimalReviewWorkflow(), tmpStore(t), newStubExecutor(),
		WithLogger(iterlog.New(iterlog.LevelInfo, &buf)))
	const raw = "snapshot: token hunter2-9f8e7d6c5b4a"
	e.logger.Warn("%s", raw)
	if !strings.Contains(buf.String(), raw) {
		t.Errorf("wrote %q, want the line unchanged", buf.String())
	}
	// A nil logger is the dryrun/benchmark shape: the install is a no-op,
	// not a panic.
	New(minimalReviewWorkflow(), tmpStore(t), newScrubbingExecutor("hunter2-9f8e7d6c5b4a"))
}

// The kill switch, on the REAL executor, in BOTH guard shapes a run
// reaches it in. ClawExecutor implements SecretScrubber whatever its
// guard, so the pass IS wired and must hand the line straight back:
//
//   - no guard at all — a run with no known value, no file hint and the
//     heuristic off: BuildSecretGuard returns nil and the pass has
//     nothing to match;
//   - a guard that HOLDS this run's declared value with the sink flags
//     cleared, which is what ITERION_SECRETS_REDACT=off actually builds
//     for a run that declares a secret (secretGuardConfigFromEnv clears
//     RedactKnown and Heuristic, it does not drop the guard).
//
// A marker in either shape would turn the operator's opt-out into a log
// that says nothing.
func TestTheKillSwitchLeavesTheLineWhole(t *testing.T) {
	const raw = "snapshot: token hunter2-9f8e7d6c5b4a"
	off := secretguard.DefaultConfig()
	off.RedactKnown = false
	off.Heuristic = false
	withTheSwitchOff := secretguard.New([]secretguard.Secret{
		{Name: "db", Value: "hunter2-9f8e7d6c5b4a", Placeholder: secretguard.PlaceholderForName("db")},
	}, off)
	if !withTheSwitchOff.HasKnownSecrets() {
		t.Fatal("scenario broken: the switch-off guard must still HOLD the run's declared value")
	}

	for _, tc := range []struct {
		name  string
		guard *secretguard.Guard
	}{
		{name: "no guard", guard: nil},
		{name: "guard with the sink flags cleared", guard: withTheSwitchOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := minimalReviewWorkflow()
			exec := model.NewClawExecutor(nil, wf, model.WithSecretGuard(tc.guard))
			t.Cleanup(func() { _ = exec.Close() })
			if _, ok := any(exec).(SecretScrubber); !ok {
				t.Fatal("scenario broken: the production executor does not implement SecretScrubber")
			}
			var buf bytes.Buffer
			e := New(wf, tmpStore(t), exec, WithLogger(iterlog.New(iterlog.LevelInfo, &buf)))
			e.logger.Warn("%s", raw)
			if !strings.Contains(buf.String(), raw) {
				t.Errorf("the kill switch rewrote the line: %q", buf.String())
			}
		})
	}
}

// The pass runs on EVERY kept line, so what it eats is as load-bearing as
// what it hides: the heuristic detector is on by default, and an engine
// diagnostic is mostly ids, refs, paths and git output. These are the real
// shapes of the lines iterion#2822 names — a commit SHA, a ref name, a run
// id, a workspace path — and they must survive whole, while a credential in
// the same line does not.
func TestTheLogRedactorLeavesTheEngineDiagnosticVocabularyAlone(t *testing.T) {
	exec := newScrubbingExecutor("hunter2-9f8e7d6c5b4a")
	keep := []string{
		`snapshot: node "implement" iter 0: anchor clean tree at HEAD: exit status 128` +
			"\noutput: fatal: update-ref failed for ref 'refs/iterion/runs/run-20261101-abc123/turns/implement/0/post': " +
			"cannot update ref: trying to write ref with nonexistent object 4f2c8a9b1d3e5f70a1b2c3d4e5f60718293a4b5c",
		`workspace capture: node "implement" iter 2: capture /tmp/iterion-ws-7781/notes.md: permission denied`,
		`runtime: failed to record run run-20261101-143022-swift-otter as failed during setup after 3 attempts: context deadline exceeded`,
	}
	for _, line := range keep {
		if got := scrubbedLogLine(exec, line); got != line {
			t.Errorf("the pass rewrote a diagnostic that carries no secret:\n in : %q\n out: %q", line, got)
		}
	}
	// …and it is not blind: a token-shaped span in the same class of line is
	// still replaced, so the two assertions together price the pass.
	withToken := `pre-snapshot: node "push" iter 0: exit status 128` +
		"\noutput: fatal: could not read Username for 'https://github.com': Bearer sk-live-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	got := scrubbedLogLine(exec, withToken)
	if strings.Contains(got, "sk-live-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789") {
		t.Errorf("a bearer token survived the pass: %q", got)
	}
	if !strings.Contains(got, `node "push"`) {
		t.Errorf("the pass ate the diagnosis around the token: %q", got)
	}
}
