package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The tool-step bound across a sandboxed node's crossings (#3283). The
// launcher accounts a crossing's turns from what crossed — the captures the
// runner mirrored to the host before it ended — and hands every crossing
// the seed its node execution started from, so the in-container loop is
// charged what it replays.

// crossingScript drives two crossings from the files in $1: each records
// what it received up to the task envelope in $1/in.<n>; crossing 1 writes
// $1/first.ndjson then ends as $2 says (kill: SIGKILL; exit: exit 1), and
// crossing 2 answers with $1/second.ndjson.
const crossingScript = `n=$(cat "$1/count" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$1/count"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$1/in.$n"
  case "$line" in *'"type":"task"'*) break;; esac
done
if [ $n -eq 1 ]; then
  cat "$1/first.ndjson"
  if [ "$2" = kill ]; then sleep 0.2; kill -9 $$; fi
  exit 1
fi
cat "$1/second.ndjson"; exit 0
`

// writeEnvelopes writes envelopes, one per line, to path.
func writeEnvelopes(t *testing.T, path string, envs ...delegate.Envelope) {
	t.Helper()
	var b strings.Builder
	for _, env := range envs {
		line, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func captureOf(t *testing.T, rounds int) delegate.Envelope {
	t.Helper()
	snapshot, err := json.Marshal(storedRounds(rounds))
	if err != nil {
		t.Fatal(err)
	}
	return delegate.NewSessionCaptureEnvelope(snapshot)
}

func resultOf(t *testing.T, r delegate.IOResult) delegate.Envelope {
	t.Helper()
	env, err := delegate.NewResultEnvelope(r)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// crossingInput decodes what a crossing received: its task and the replay
// it was seeded with.
func crossingInput(t *testing.T, path string) (delegate.IOTask, json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the crossing never ran: %v", err)
	}
	var task delegate.IOTask
	var replay json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var env delegate.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		switch env.Type {
		case delegate.EnvelopeSessionReplay:
			replay = env.Data
		case delegate.EnvelopeTask:
			if err := json.Unmarshal(env.Data, &task); err != nil {
				t.Fatal(err)
			}
		}
	}
	return task, replay
}

// runInContainer runs what the runner of a crossing runs with what crossed:
// the real Execute, its store seeded with the replay, on a scripted model.
// It returns the tool turns the in-container loop ran and the tool results
// its answering request carried.
func runInContainer(t *testing.T, ioTask delegate.IOTask, replay json.RawMessage) (int, int) {
	t.Helper()
	probe, ran := budgetProbe()
	task := delegate.FromIOTask(ioTask)
	task.ToolDefs, task.HasTools, task.Sandbox, task.Model = []delegate.ToolDef{probe}, true, nil, "test/test-model"
	store := newNodeSessionStore()
	if len(replay) > 0 {
		if err := store.SaveSnapshot("run-crossing", "judge", replay); err != nil {
			t.Fatal(err)
		}
	}
	client := budgetModel(nil, nil)
	res, err := budgetBackend(client, 1).Execute(withRuntimeContext(context.Background(), "run-crossing", store), task)
	requireVerdict(t, res, err)
	return int(ran.Load()), toolResultsIn(client.calls[len(client.calls)-1])
}

// crossingDispatch runs a sandboxed claw node with the given bound through
// the executor's dispatch, whose in-place retry takes one more crossing.
func crossingDispatch(t *testing.T, dir, ending string, bound int) (chainOutcome, error) {
	t.Helper()
	box := &scriptedSandbox{script: crossingScript, args: []string{dir, ending}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, newTestClawBackend())
	e := newFallbackExecutor(reg, EventHooks{})
	build := e.newElementBuilder("judge", delegate.BackendClaw, nil,
		func(context.Context, string) (*delegate.Task, error) {
			return &delegate.Task{
				NodeID: "judge", Model: "openai/gpt-5", UserPrompt: "Judge the diff.",
				HasTools: true, ToolMaxSteps: bound, OutputSchema: json.RawMessage(budgetSchema),
				WorkDir: dir, Sandbox: box,
			}, nil
		})
	ctx := withRuntimeContext(context.Background(), "run-crossing", newNodeSessionStore())
	return e.dispatchChain(ctx, "judge", []chainElement{{Label: "primary"}}, "openai/gpt-5", build)
}

// Crossing 1 completes 4 of 6 turns, its capture reaches the host, then the
// runner dies before its result envelope (an OOM kill). The executor's
// in-place retry takes crossing 2, which is replayed those 4 turns AND
// handed the seed the node execution started from (0), so its loop is left
// the 2 turns the bound allows. The control arm ends crossing 1 on an error
// envelope instead: the same account either way.
//
// Mutants: return a death's result without the crossing's account (the
// seed lost, crossing 2 fixes its own from the replay and runs 6 more); or
// count no turn from the captures (the dispatch reports 0 for the dead
// crossing's 4).
func TestClawStepBudget_ACrossingThatDiesKeepsItsTurnsCharged(t *testing.T) {
	t.Parallel()
	const bound = 6
	for _, arm := range []string{"error-envelope", "kill"} {
		t.Run(arm, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			first := []delegate.Envelope{captureOf(t, 4)}
			if arm == "error-envelope" {
				first = append(first, resultOf(t, delegate.IOResult{Error: "read tcp: connection reset by peer", ToolTurns: 4}))
			}
			writeEnvelopes(t, filepath.Join(dir, "first.ndjson"), first...)
			writeEnvelopes(t, filepath.Join(dir, "second.ndjson"), resultOf(t, delegate.IOResult{Output: map[string]any{"verdict": "done"}}))

			out, err := crossingDispatch(t, dir, arm, bound)
			requireVerdict(t, out.Result, err)
			if out.Result.ToolTurns != 4 {
				t.Errorf("the dispatch reports %d tool turns, want the dead crossing's 4", out.Result.ToolTurns)
			}

			ioTask, replay := crossingInput(t, filepath.Join(dir, "in.2"))
			if ioTask.ToolTurnsSeed == nil || *ioTask.ToolTurnsSeed != 0 {
				t.Fatalf("crossing 2 carries ToolTurnsSeed %v, want the node execution's 0", ioTask.ToolTurnsSeed)
			}
			ran, results := runInContainer(t, ioTask, replay)
			if total := 4 + ran; total != bound {
				t.Fatalf("crossing 2 ran %d turns on a replay of 4: %d for tool_max_steps %d", ran, total, bound)
			}
			if results != bound {
				t.Errorf("crossing 2 answers with %d tool results in view, want %d", results, bound)
			}
		})
	}
}

// A runner that ends without its result envelope reports the turns its
// captures show, on that failure too.
func TestClawStepBudget_ACrossingWithoutAResultStillReportsItsTurns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeEnvelopes(t, filepath.Join(dir, "first.ndjson"), captureOf(t, 3))
	box := &scriptedSandbox{script: crossingScript, args: []string{dir, "exit"}}
	res, err := newTestClawBackend().executeViaSandboxRunner(budgetCtx(), delegate.Task{
		NodeID: "judge", Model: "openai/gpt-5", UserPrompt: "Judge the diff.",
		HasTools: true, ToolMaxSteps: 6, WorkDir: dir, Sandbox: box,
	})
	if err == nil {
		t.Fatal("a runner that exits 1 without a result must fail the crossing")
	}
	if res.ToolTurns != 3 || res.ToolTurnsSeed == nil || *res.ToolTurnsSeed != 0 {
		t.Fatalf("result ToolTurns=%d ToolTurnsSeed=%v, want the captures' 3 and the seed 0 (err: %v)", res.ToolTurns, res.ToolTurnsSeed, err)
	}
}

// Crossing 1 reports the whole bound done but none of its conversation
// reached the host (no capture). Crossing 2 replays nothing, so it is
// charged nothing: its loop runs the bound again and answers from turns it
// sees, instead of formatting a verdict from the bare prompt.
//
// Mutant: carry the turns the result envelope reports as spent and charge
// them unread (the launcher stamps the envelope's count into the seed, the
// loop charges the seed): crossing 2 goes straight to the recovery pass
// with no tool result in view.
func TestClawStepBudget_ACrossingWhoseTurnsNeverCrossedRunsTheBound(t *testing.T) {
	t.Parallel()
	const bound = 4
	dir := t.TempDir()
	writeEnvelopes(t, filepath.Join(dir, "first.ndjson"), resultOf(t, delegate.IOResult{Error: "read tcp: connection reset by peer", ToolTurns: bound}))
	writeEnvelopes(t, filepath.Join(dir, "second.ndjson"), resultOf(t, delegate.IOResult{Output: map[string]any{"verdict": "done"}}))

	out, err := crossingDispatch(t, dir, "exit", bound)
	requireVerdict(t, out.Result, err)
	ioTask, replay := crossingInput(t, filepath.Join(dir, "in.2"))
	if len(replay) != 0 {
		t.Fatalf("crossing 2 was replayed %d bytes, want none (nothing was captured)", len(replay))
	}
	ran, results := runInContainer(t, ioTask, replay)
	if ran != bound || results != bound {
		t.Fatalf("crossing 2 ran %d turns and answers with %d tool results in view, want %d and %d", ran, results, bound, bound)
	}
}

// A runner that predates the shared bound reads no seed and reports no tool
// turn: the launcher names the skew when its captures show turns, and stays
// silent when the runner reports its own.
//
// Mutant: drop the warning.
func TestClawStepBudget_AnOlderRunnerIsNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		reported  int
		wantNamed bool
	}{
		{name: "an older runner", reported: 0, wantNamed: true},
		{name: "a current runner", reported: 3, wantNamed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			script := `while IFS= read -r line; do case "$line" in *'"type":"task"'*) break;; esac; done; cat "$1"; exit 0`
			out := filepath.Join(dir, "out.ndjson")
			writeEnvelopes(t, out, captureOf(t, 3), resultOf(t, delegate.IOResult{Output: map[string]any{"verdict": "done"}, ToolTurns: tc.reported}))
			var logs strings.Builder
			res, err := loggedTestClawBackend(&logs).executeViaSandboxRunner(budgetCtx(), delegate.Task{
				NodeID: "judge", Iteration: 1, Model: "openai/gpt-5", UserPrompt: "Judge the diff.",
				HasTools: true, ToolMaxSteps: 6, WorkDir: dir,
				Sandbox: &scriptedSandbox{script: script, args: []string{out}},
			})
			if err != nil {
				t.Fatalf("crossing: %v", err)
			}
			if res.ToolTurns != 3 {
				t.Errorf("ToolTurns = %d, want 3", res.ToolTurns)
			}
			named := strings.Contains(logs.String(), "older iterion that does not share the node's tool_max_steps")
			if named != tc.wantNamed {
				t.Errorf("skew named = %v, want %v; logs:\n%s", named, tc.wantNamed, logs.String())
			}
		})
	}
}

// A route change in the fallback chain restarts from the last-good
// conversation: the failed route's turns are rolled back out of it, and the
// next route's loop gets the whole bound.
//
// Mutant: keep the failed route's conversation (no rollback) and the next
// route is charged its 2 turns.
func TestClawStepBudget_ARouteChangeRestartsWithTheWholeBound(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{3: true}, nil)
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, budgetBackend(client, 1))
	e := newFallbackExecutor(reg, EventHooks{})
	e.retry.MaxAttempts = 1
	build := e.newElementBuilder("judge", delegate.BackendClaw, nil,
		func(context.Context, string) (*delegate.Task, error) {
			task := budgetTask(probe, 5)
			return &task, nil
		})
	out, err := e.dispatchChain(budgetCtx(), "judge",
		[]chainElement{{Label: "primary"}, {Label: "rescue", Model: "test/rescue-model"}}, "test/test-model", build)
	requireVerdict(t, out.Result, err)
	if got := ran.Load(); got != 2+5 {
		t.Fatalf("%d tool turns ran, want the failed route's 2 and the rescue's whole 5 (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLLLLR" {
		t.Errorf("requests = %s, want 2 turns and the cut, then 5 turns and one recovery pass", got)
	}
	if got := toolResultsIn(client.calls[len(client.calls)-1]); got != 5 {
		t.Errorf("the rescue answers with %d tool results in view, want its own 5", got)
	}
}
