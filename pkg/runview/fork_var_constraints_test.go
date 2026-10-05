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

// seedTurnFor writes the anchor turn checkpoint a fork of `id` needs — the
// seed helper writes it for the parent only, and a fork of a FORKED child
// reads the child's own turn.
func seedTurnFor(t *testing.T, svc *Service, id string) {
	t.Helper()
	turnStore := store.AsTurnStore(svc.store)
	if turnStore == nil {
		t.Fatal("this store cannot hold turn checkpoints")
	}
	if err := turnStore.WriteTurn(context.Background(), &store.TurnCheckpoint{
		RunID: id, NodeID: "noop", LoopIter: 0, TurnIndex: 0,
		Backend: "claw", WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
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
		if !errors.Is(err, ErrForkInputsRefused) {
			t.Fatalf("the multi-file unit was read as a single file and admitted instead of checked: %v", err)
		}
		if !strings.Contains(err.Error(), `"yolo"`) {
			t.Errorf("refusal = %q, want the enum violation", err)
		}
	})

	// A source-less parent (over the 1 MiB record cap, a forced cloud resume
	// that cleared the pair, or a pre-2026-08-04 run) cannot be pre-checked.
	// The fork WITHOUT new inputs — the recovery path an operator reaches for
	// by default — must still work, and record nothing.
	t.Run("a source-less parent forks freely without new inputs", func(t *testing.T) {
		svc, id := seedForkGateParent(t, nil, map[string]any{"mode": "fast"})
		if err := forkWith(t, svc, id, nil); err != nil {
			t.Fatalf("the recovery path was refused on a run that recorded no source: %v", err)
		}
	})

	// The unverifiable case is ADMITTED with a note since the first-resume
	// floor exists (#1743): the 1 MiB cap stops costing forkability, and the
	// recorded delta is what makes that admission safe — the child's first
	// resume judges exactly these keys and refuses the child if they violate
	// the constraints. The note says so, loudly.
	t.Run("a source-less parent admits new inputs with a note and records the delta", func(t *testing.T) {
		svc, id := seedForkGateParent(t, nil, map[string]any{"mode": "fast"})
		result, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "noop", TurnIndex: 0, NewInputs: map[string]any{"mode": "slow"}})
		if err != nil {
			t.Fatalf("an unverifiable fork was refused although the first-resume floor judges it: %v", err)
		}
		if len(result.Notes) == 0 {
			t.Fatal("the fork was admitted with no note — the operator was not told the values were admitted unchecked")
		}
		for _, want := range []string{"first resume", "admitted without a pre-check"} {
			if !strings.Contains(strings.Join(result.Notes, "; "), want) {
				t.Errorf("note %q does not carry %q", result.Notes, want)
			}
		}
		child, err := svc.store.LoadRun(context.Background(), result.NewRunID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(child.ForkSuppliedInputs, ",") != "mode" {
			t.Fatalf("child.ForkSuppliedInputs = %v, want [mode] — the first-resume gate is blind without the record", child.ForkSuppliedInputs)
		}
		if child.Inputs["mode"] != "slow" {
			t.Fatalf("child.Inputs[mode] = %v, want slow", child.Inputs["mode"])
		}
	})

	// A fork of a forked child carries the parent's UNJUDGED keys forward:
	// an unchanged re-send is not a verdict, and without the carry the value
	// would ride an unjudged chain into a run that never judges it.
	t.Run("a fork of a forked child carries the unjudged keys forward", func(t *testing.T) {
		svc, id := seedForkGateParent(t, nil, map[string]any{"mode": "fast"})
		first, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "noop", TurnIndex: 0, NewInputs: map[string]any{"mode": "slow"}})
		if err != nil {
			t.Fatal(err)
		}
		seedTurnFor(t, svc, first.NewRunID)
		second, err := svc.Fork(context.Background(), ForkSpec{RunID: first.NewRunID, NodeID: "noop", TurnIndex: 0})
		if err != nil {
			t.Fatalf("a plain recovery fork of a forked child was refused: %v", err)
		}
		grandchild, err := svc.store.LoadRun(context.Background(), second.NewRunID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(grandchild.ForkSuppliedInputs, ",") != "mode" {
			t.Fatalf("grandchild.ForkSuppliedInputs = %v, want [mode] — an unjudged value must not ride an unjudged chain into a run that never judges it", grandchild.ForkSuppliedInputs)
		}
	})

	// The union is the point: a fork that changes ANOTHER key on top of an
	// unjudged chain arms BOTH — the new delta for its own sake, the carried
	// keys because an unchanged re-send is not a verdict.
	t.Run("a fork over an unjudged chain arms the union of both", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "fast"})
		first, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "noop", TurnIndex: 0, NewInputs: map[string]any{"mode": "slow"}})
		if err != nil {
			t.Fatal(err)
		}
		seedTurnFor(t, svc, first.NewRunID)
		second, err := svc.Fork(context.Background(), ForkSpec{RunID: first.NewRunID, NodeID: "noop", TurnIndex: 0, NewInputs: map[string]any{"target": "other-repo"}})
		if err != nil {
			t.Fatal(err)
		}
		grandchild, err := svc.store.LoadRun(context.Background(), second.NewRunID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(grandchild.ForkSuppliedInputs, ",") != "mode,target" {
			t.Fatalf("grandchild.ForkSuppliedInputs = %v, want [mode target] — the carried key and the new delta arm together", grandchild.ForkSuppliedInputs)
		}
	})

	// A plain recovery fork — no new inputs, no inherited record — records
	// NOTHING: an empty delta never meets a gate, at fork time or after.
	t.Run("a plain recovery fork records nothing", func(t *testing.T) {
		svc, id := seedForkGateParent(t, singleFile(), map[string]any{"mode": "fast"})
		result, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "noop", TurnIndex: 0})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Notes) != 0 {
			t.Fatalf("a plain recovery fork produced a note: %v", result.Notes)
		}
		child, err := svc.store.LoadRun(context.Background(), result.NewRunID)
		if err != nil {
			t.Fatal(err)
		}
		if len(child.ForkSuppliedInputs) != 0 {
			t.Fatalf("child.ForkSuppliedInputs = %v, want empty — an empty delta never meets a gate", child.ForkSuppliedInputs)
		}
	})
}
