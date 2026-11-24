package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestReliabilityScenarios names the three invariants across the shipped seams:
// no replay of checkpointed effects, no destructive resume without source
// consent, and no semantic repair beyond its declared bound. Interruption here
// ends at a durable checkpoint; it does not promise external-effect atomicity
// for a process killed between its effect and its checkpoint write.
func TestReliabilityScenarios(t *testing.T) {
	for _, first := range []string{"left", "right"} {
		t.Run("checkpointed_interruption/"+first+"_first", func(t *testing.T) {
			reliabilityCheckpointedEffects(t, first, false)
		})
	}
	t.Run("usage_window_park", func(t *testing.T) { reliabilityCheckpointedEffects(t, "left", true) })
	t.Run("invalid_output_has_no_effect", reliabilityInvalidOutput)
	t.Run("source_consent_before_destructive_resume", reliabilitySourceConsent)
	t.Run("rewind_checkpoint", reliabilityRewindCheckpoint)
	t.Run("rewind_workspace", reliabilityRewindWorkspace)
	t.Run("rewind_refuses_running", reliabilityRewindRefusesRunning)
	t.Run("bounded_repair", reliabilityBoundedRepair)
	t.Run("single_schema_reask", reliabilitySchemaReask)
}

// The file is an external effect relative to the run store. Counting its lines
// after reopening the store catches replays that a new executor's counter hides.
type reliabilityEffects struct {
	mu   sync.Mutex
	path string
}

func (e *reliabilityEffects) append(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(f, id)
	return errors.Join(writeErr, f.Close())
}

func (e *reliabilityEffects) counts(t *testing.T) map[string]int {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	body, err := os.ReadFile(e.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]int{}
	}
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, id := range strings.Fields(string(body)) {
		counts[id]++
	}
	return counts
}

const reliabilityFanoutSource = `schema value:
  value: string
schema count:
  count: int
agent seed:
  model: "fixture"
  output: value
router fan:
  mode: fan_out_all
agent left:
  model: "fixture"
  output: value
agent right:
  model: "fixture"
  output: value
compute joined:
  await: wait_all
  output: count
  expr:
    count: "1"
agent finish:
  model: "fixture"
  output: value
workflow reliability:
  worktree: none
  sandbox: none
  entry: seed
  seed -> fan
  fan -> left
  fan -> right
  left -> joined
  right -> joined
  joined -> finish
  finish -> done
`

func reliabilityCheckpointedEffects(t *testing.T, first string, quota bool) {
	synctest.Test(t, func(t *testing.T) {
		wf := compileSource(t, "reliability.bot", reliabilityFanoutSource)
		s := tmpStore(t)
		effects := &reliabilityEffects{path: filepath.Join(t.TempDir(), "effects")}
		firstFinished := make(chan struct{})
		var once sync.Once
		var orderMu sync.Mutex
		var order []string
		observer := func(event store.Event) {
			if event.Type == store.EventNodeFinished && (event.NodeID == "left" || event.NodeID == "right") {
				orderMu.Lock()
				order = append(order, event.NodeID)
				orderMu.Unlock()
				if event.NodeID == first {
					once.Do(func() { close(firstFinished) })
				}
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		exec := newScenarioExecutor()
		for _, id := range []string{"seed", "left", "right"} {
			exec.on(id, func(_ map[string]any) (map[string]any, error) {
				if id != "seed" && id != first {
					select {
					case <-firstFinished:
						// Wait until the first branch has returned and the collector blocks
						// on the second result; NodeFinished alone precedes the channel send.
						synctest.Wait()
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return map[string]any{"value": id}, effects.append(id)
			})
		}
		exec.on("finish", func(_ map[string]any) (map[string]any, error) {
			if quota {
				return nil, &delegate.ErrRateLimited{Provider: "fixture", Kind: delegate.RateLimitKindUsageWindow, Detail: "fixture window"}
			}
			return nil, errors.New("interrupted before the final effect")
		})
		opts := []runtime.EngineOption{runtime.WithLogger(iterlog.Nop()), runtime.WithEventObserver(observer)}
		if quota {
			opts = append(opts, runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())))
		}
		const id = "reliability-checkpoint"
		if err := newEngine(t, wf, s, exec, opts...).Run(ctx, id, nil); err == nil {
			t.Fatal("expected checkpointed interruption")
		}
		row, err := s.LoadRun(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != store.RunStatusFailedResumable || row.Checkpoint == nil || row.Checkpoint.NodeID != "finish" {
			t.Fatalf("interruption lost its resumable checkpoint: %+v", row)
		}
		for _, node := range []string{"left", "right"} {
			if got := row.Checkpoint.Outputs[node]["value"]; got != node {
				t.Fatalf("fan-out result set lost %s: %v", node, row.Checkpoint.Outputs)
			}
		}
		if exec.callCount("finish") != 1 {
			t.Fatalf("park retried the blocked node %d times", exec.callCount("finish"))
		}
		if quota && row.FailureCode != store.FailureUsageLimitBlocked {
			t.Fatalf("quota park lost its typed cause: %s", row.FailureCode)
		}
		second := "right"
		if first == "right" {
			second = "left"
		}
		orderMu.Lock()
		gotOrder := append([]string(nil), order...)
		orderMu.Unlock()
		if !reflect.DeepEqual(gotOrder, []string{first, second}) {
			t.Fatalf("fixture did not exercise requested completion order: %v", gotOrder)
		}
		if got := effects.counts(t); !reflect.DeepEqual(got, map[string]int{"seed": 1, "left": 1, "right": 1}) {
			t.Fatalf("duplicated or missing effect before resume: %v", got)
		}
		reopened, err := store.New(s.Root())
		if err != nil {
			t.Fatal(err)
		}
		resumed := newScenarioExecutor()
		for _, node := range []string{"seed", "left", "right", "finish"} {
			resumed.on(node, func(_ map[string]any) (map[string]any, error) {
				return map[string]any{"value": node}, effects.append(node)
			})
		}
		if err := newEngine(t, wf, reopened, resumed, runtime.WithLogger(iterlog.Nop())).Resume(t.Context(), id, nil); err != nil {
			t.Fatal(err)
		}
		if got := effects.counts(t); !reflect.DeepEqual(got, map[string]int{"seed": 1, "left": 1, "right": 1, "finish": 1}) {
			t.Fatalf("duplicated external effect across checkpoint resume: %v", got)
		}
		row, err = reopened.LoadRun(t.Context(), id)
		if err != nil || row.Status != store.RunStatusFinished {
			t.Fatalf("resume did not finish: %+v, %v", row, err)
		}

	})
}

const reliabilityOutputSource = `schema answer:
  value: string
agent answer:
  model: "fixture"
  output: answer
  publish: checked
agent effect:
  model: "fixture"
workflow reliability_output:
  worktree: none
  sandbox: none
  entry: answer
  answer -> effect
  effect -> done
`

func reliabilityInvalidOutput(t *testing.T) {
	wf := compileSource(t, "output.bot", reliabilityOutputSource)
	s := tmpStore(t)
	exec := newScenarioExecutor()
	effects := &reliabilityEffects{path: filepath.Join(t.TempDir(), "effects")}
	exec.on("answer", func(map[string]any) (map[string]any, error) { return map[string]any{"value": false}, nil })
	exec.on("effect", func(map[string]any) (map[string]any, error) { return nil, effects.append("effect") })
	const id = "invalid-output"
	for attempt := 0; attempt < 2; attempt++ {
		eng := newEngine(t, wf, s, exec, runtime.WithOutputValidation(true), runtime.WithLogger(iterlog.Nop()))
		var err error
		if attempt == 0 {
			err = eng.Run(t.Context(), id, nil)
		} else {
			err = eng.Resume(t.Context(), id, nil)
		}
		if got := effects.counts(t); len(got) != 0 {
			t.Fatalf("invalid output reached an external effect: %v", got)
		}
		if err == nil {
			t.Fatal("invalid output unexpectedly accepted")
		}
		if art, err := s.LoadArtifact(t.Context(), id, "answer", 0); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid output published: %+v, %v", art, err)
		}
	}
	exec.on("answer", func(map[string]any) (map[string]any, error) { return map[string]any{"value": "valid"}, nil })
	if err := newEngine(t, wf, s, exec, runtime.WithOutputValidation(true), runtime.WithLogger(iterlog.Nop())).Resume(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	if got := effects.counts(t); got["effect"] != 1 {
		t.Fatalf("valid control did not reach its effect exactly once: %v", got)
	}
	art, err := s.LoadArtifact(t.Context(), id, "answer", 0)
	if err != nil || art.Data["value"] != "valid" {
		t.Fatalf("valid control did not publish: %+v, %v", art, err)
	}
}

func reliabilitySourceConsent(t *testing.T) {
	wf := compileFixtureStubSafe(t, "rewind_mini.bot")
	wf.Worktree = "none"
	s := tmpStore(t)
	exec := newScenarioExecutor()
	exec.on("implement", func(map[string]any) (map[string]any, error) { return nil, errors.New("checkpoint before write") })
	const id = "source-consent"
	if err := newEngine(t, wf, s, exec, runtime.WithWorkflowHash("source-one"), runtime.WithLogger(iterlog.Nop())).Run(t.Context(), id, nil); err == nil {
		t.Fatal("expected checkpoint")
	}
	path := filepath.Join(t.TempDir(), "human.txt")
	if err := os.WriteFile(path, []byte("operator work"), 0600); err != nil {
		t.Fatal(err)
	}
	resumed := newScenarioExecutor()
	resumed.on("implement", func(map[string]any) (map[string]any, error) {
		return map[string]any{"value": "changed"}, os.WriteFile(path, []byte("resumed write"), 0600)
	})
	resumed.on("verify", func(map[string]any) (map[string]any, error) { return map[string]any{"value": "ok", "ok": true}, nil })
	reopened, err := store.New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	resumeErr := newEngine(t, wf, reopened, resumed, runtime.WithWorkflowHash("source-two"), runtime.WithLogger(iterlog.Nop())).Resume(t.Context(), id, nil)
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "operator work" || resumed.callCount("implement") != 0 {
		t.Fatalf("unauthorised destructive resume: file=%q calls=%d err=%v", body, resumed.callCount("implement"), err)
	}
	if !errors.Is(resumeErr, runtime.ErrWorkflowSourceChanged) {
		t.Fatalf("changed source resumed without consent: %v", resumeErr)
	}
	row, err := reopened.LoadRun(t.Context(), id)
	if err != nil || row.Status != store.RunStatusFailedResumable || row.Checkpoint == nil || row.Checkpoint.NodeID != "implement" {
		t.Fatalf("refusal consumed the resumable checkpoint: %+v, %v", row, err)
	}
	if err := newEngine(t, wf, reopened, resumed, runtime.WithWorkflowHash("source-two"), runtime.WithForceResume(true), runtime.WithLogger(iterlog.Nop())).Resume(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil || string(body) != "resumed write" || resumed.callCount("implement") != 1 {
		t.Fatalf("authorized control did not execute: %q calls=%d err=%v", body, resumed.callCount("implement"), err)
	}
}
