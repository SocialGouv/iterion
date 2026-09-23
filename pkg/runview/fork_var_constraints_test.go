package runview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `fork` is the one operator surface that writes var values into a run without
// crossing Engine.Run: the child is parked `cancelled` and executed by Resume,
// and Resume deliberately never re-judges stored values. So a var's declared
// `[enum: …]` / `[matching: …]` was inert on `--new-inputs`, on
// POST /api/runs/{id}/fork and in the studio's ForkDialog — three surfaces, one
// implementation, and the gate is on that implementation.
const forkGateMain = `vars:
  mode: string [enum: "fast", "slow"] = "fast"
  target: string [matching: "^[a-z0-9-]+$"] = "repo"
  free: string = "anything"

tool noop:
  command: ` + "`printf '{}'`" + `

workflow forkgate:
  worktree: none
  entry: noop
  noop -> done
`

// The same declaration, reached through an IMPORT. A recorded source that
// imports is what a multi-file bot records, and it is the shape a
// single-file-only compile refuses outright — which would have turned this
// gate into a hard refusal for those bots instead of a check.
const forkGateImporter = `import "lib/vars.bot"

tool noop:
  command: ` + "`printf '{}'`" + `

workflow forkgate:
  worktree: none
  entry: noop
  noop -> done
`

const forkGateLib = `vars:
  mode: string [enum: "fast", "slow"] = "fast"
`

func seedForkGateParent(t *testing.T, files []store.WorkflowSourceFile, inputs map[string]any) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	const parentID = "run-forkgate-parent"
	if _, err := st.CreateRun(context.Background(), parentID, "forkgate", inputs); err != nil {
		t.Fatal(err)
	}
	parent, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	parent.Checkpoint = &store.Checkpoint{NodeID: "noop", Outputs: map[string]map[string]any{}}
	parent.Status = store.RunStatusCancelled
	parent.FilePath = "/somewhere/main.bot"
	if len(files) > 0 {
		parent.WorkflowSource = files[0].Text
		parent.WorkflowSources = files
	}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTurn(context.Background(), &store.TurnCheckpoint{
		RunID: parentID, NodeID: "noop", LoopIter: 0, TurnIndex: 0,
		Backend: "claw", WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	return svc, parentID
}

func singleFile() []store.WorkflowSourceFile {
	return []store.WorkflowSourceFile{{Path: "main.bot", Text: forkGateMain}}
}

func forkWith(t *testing.T, svc *Service, parentID string, newInputs map[string]any) error {
	t.Helper()
	_, err := svc.Fork(context.Background(), ForkSpec{RunID: parentID, NodeID: "noop", TurnIndex: 0, NewInputs: newInputs})
	return err
}

func TestForkNewInputsCrossTheSameVarConstraintGateALaunchCrosses(t *testing.T) {
	t.Run("a value outside the enum is refused, naming var, value and constraint", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "fast"})
		err := forkWith(t, svc, id, map[string]any{"mode": "yolo"})
		if err == nil {
			t.Fatal("the fork was accepted — a declared [enum:] is inert on the one surface that writes operator var values without entering Engine.Run")
		}
		for _, want := range []string{`"mode"`, `"yolo"`, "fast", "slow"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not name %q — the operator typed the value and is still at the keyboard", err, want)
			}
		}
	})

	t.Run("a value off the declared pattern is refused", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"target": "repo"})
		err := forkWith(t, svc, id, map[string]any{"target": " codeX --some-flag"})
		if err == nil {
			t.Fatal("the fork was accepted — [matching:] exists because that value is interpolated as a bare shell word")
		}
		if !strings.Contains(err.Error(), "does not match the declared pattern") {
			t.Errorf("refusal = %q, want the pattern violation", err)
		}
		// Typed, so the HTTP surface answers 400 for what the operator typed
		// instead of the 500 its default arm gives a genuine fault.
		if !errors.Is(err, ErrForkInputsRefused) {
			t.Errorf("refusal is untyped (%v) — POST /api/runs/{id}/fork would answer 500", err)
		}
	})

	t.Run("a value inside the declaration is accepted", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "fast"})
		if err := forkWith(t, svc, id, map[string]any{"mode": "slow", "free": "whatever"}); err != nil {
			t.Fatalf("a legitimate fork was refused: %v", err)
		}
	})

	// The ForkDialog pre-fills its editor with the parent's whole input map, so
	// an unmodified submit re-sends values the parent was ALREADY admitted with
	// — here one the declaration no longer allows. Judging those would refuse a
	// recovery fork because the declaration was tightened after the parent ran,
	// which is the thing Resume is forbidden from doing.
	t.Run("a value re-sent unchanged is never re-judged", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "legacy", "target": "repo"})
		if err := forkWith(t, svc, id, map[string]any{"mode": "legacy", "target": "repo"}); err != nil {
			t.Fatalf("an unmodified ForkDialog submit was refused: %v", err)
		}
	})

	// The expansion of ${…} is a property of the PROCESS: this gate runs in the
	// server, the child runs elsewhere, and ${PROJECT_DIR} names a worktree that
	// does not exist yet. A verdict on such a value would be a guess.
	t.Run("an environment-dependent value on a constrained var is refused, not guessed", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "fast"})
		err := forkWith(t, svc, id, map[string]any{"mode": "${FORK_GATE_MODE}"})
		if err == nil {
			t.Fatal("an environment-dependent value was admitted — the process that runs the child is not this one")
		}
		if !strings.Contains(err.Error(), "resolved from the environment") {
			t.Errorf("refusal = %q, want the environment-dependence refusal", err)
		}
	})

	// …and the same value on an UNCONSTRAINED var stays legal: the refusal is a
	// consequence of the constraint, not a new rule about templates.
	t.Run("an environment-dependent value on an unconstrained var is still accepted", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "fast"})
		if err := forkWith(t, svc, id, map[string]any{"free": "${FORK_GATE_ANYTHING}"}); err != nil {
			t.Fatalf("an unconstrained var lost the right to a template: %v", err)
		}
	})

	// An UNDECLARED key is admitted, exactly as the launch gate admits it:
	// `{{input.X}}` resolves from run inputs the workflow never declared, and
	// the forge gate relaunch writes such a key on purpose. A fork stricter
	// than the launch it recovers from would kill every valid change in the
	// same all-or-nothing submit.
	t.Run("a key that is not a var of the workflow is admitted, like a launch", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "fast"})
		if err := forkWith(t, svc, id, map[string]any{"ticket": "ABC-123"}); err != nil {
			t.Fatalf("an undeclared run input was refused, although the launch surface accepts it: %v", err)
		}
	})

	// …and a literal that merely LOOKS environment-dependent is not refused.
	t.Run("the empty reference is a literal on every machine", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"target": "repo"})
		if err := forkWith(t, svc, id, map[string]any{"target": "a${}b"}); err != nil {
			t.Fatalf("`a${}b` reads the same in every process and matches the pattern, but was refused: %v", err)
		}
	})

	// A recorded source that IMPORTS is what a multi-file bot records. Reading
	// it as a single file would refuse it outright, turning the gate into a
	// hard refusal for those bots.
	t.Run("a recorded source that imports is read as its unit", func(t *testing.T) {
		// The IMPORTED file first on purpose: the recorded list happens to be
		// written main-first, but a position in a slice is not a fact. With
		// the main picked by index this unit merges the wrong file and the
		// gate reports "unverifiable" instead of checking.
		files := []store.WorkflowSourceFile{
			{Path: "lib/vars.bot", Text: forkGateLib},
			{Path: "main.bot", Text: forkGateImporter},
		}
		svc, id := seedForkGateParent(t, files, map[string]any{"mode": "fast"})
		err := forkWith(t, svc, id, map[string]any{"mode": "yolo"})
		if err == nil {
			t.Fatal("the enum declared in an IMPORTED file did not bound the fork")
		}
		if errors.Is(err, ErrForkInputsUnverifiable) {
			t.Fatalf("the multi-file unit was read as a single file and refused instead of checked: %v", err)
		}
		if !strings.Contains(err.Error(), `"yolo"`) {
			t.Errorf("refusal = %q, want the enum violation", err)
		}
	})

	// A source-less parent (over the 1 MiB record cap, or a forced cloud resume
	// that cleared the pair) cannot be checked. The fork WITHOUT new inputs —
	// the recovery path an operator reaches for by default — must still work.
	t.Run("a source-less parent forks freely without new inputs", func(t *testing.T) {
		svc, id := seedForkGateParent(t, nil, map[string]any{"mode": "fast"})
		if err := forkWith(t, svc, id, nil); err != nil {
			t.Fatalf("the recovery path was refused on a run that recorded no source: %v", err)
		}
	})

	t.Run("a source-less parent refuses new inputs explicitly", func(t *testing.T) {
		svc, id := seedForkGateParent(t, nil, map[string]any{"mode": "fast"})
		err := forkWith(t, svc, id, map[string]any{"mode": "slow"})
		if !errors.Is(err, ErrForkInputsUnverifiable) {
			t.Fatalf("err = %v, want the typed unverifiable refusal naming the escape", err)
		}
		// A refusal an operator cannot act on is a dead end. It has to say
		// since WHEN sources are recorded — so the reader can tell an old run
		// from a broken one — and name both ways on.
		for _, want := range []string{"2026-08-04", "fork without --new-inputs", "launch the workflow afresh"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal does not carry %q: %v", want, err)
			}
		}
	})
}
