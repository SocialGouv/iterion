//go:build live

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/liveledger"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestLive_Feat_ClaudeCodeBackgroundWork proves, against the
// REAL CLI, what only a real model can: a node whose agent launches a
// background subagent and ends its turn gets the subagent's result into its
// final structured output — and, with the lifecycle off, does not (the
// pre-fix behaviour, measured on a Doki run as 4 of 5 wasted passes). The
// second half is the witness: without it a green first half could mean the
// model happened to wait in-turn.
//
// ITERION_LIVE_CLAUDE_CLI pins the binary — point it at the version the
// runner image pins (docker/llm-clis/package.json); `claude` on PATH
// otherwise. ITERION_LIVE_CLAUDE_MODEL overrides the cheap default.
//
// Cost: two short haiku sessions, ~$0.4 a run.
func TestLive_Feat_ClaudeCodeBackgroundWork(t *testing.T) {
	tracker := liveledger.Track(t) // #1422: record last-green ledger row for this target on t.Cleanup
	// The test drives delegate.ClaudeCodeBackend directly — no executor, no
	// engine — so neither the re-target nor the ceiling can reach the two
	// sessions it pays for. Refused with the reason, as feat:schedule is,
	// rather than spent on the pinned model while the operator believes the
	// run moved; the funder knob still reaches the row below.
	refuseRetargetWithoutSeam(t, "delegate.ClaudeCodeBackend.Execute")
	cli := os.Getenv("ITERION_LIVE_CLAUDE_CLI")
	if cli == "" {
		p, err := exec.LookPath("claude")
		if err != nil {
			skipLive(t, "no claude CLI: set ITERION_LIVE_CLAUDE_CLI or put claude on PATH")
		}
		cli = p
	}
	model := os.Getenv("ITERION_LIVE_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}

	// What served and what it cost, read from the backend's own report: this
	// test drives the delegate directly, so there is no event stream to read
	// llm_request from. Guarded by a lock because the observer is fed from
	// inside the subtests.
	var provMu sync.Mutex
	var served []string
	var calls int
	var cost float64

	run := func(t *testing.T, lifecycle string) (answer string, phases []delegate.BackgroundPhase) {
		// Subagents run in the foreground unless background work is asked
		// for; this test is about background work.
		t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "on")
		t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", lifecycle)
		nonce := fmt.Sprintf("NONCE-%d", time.Now().UnixNano())
		var mu sync.Mutex
		task := delegate.Task{
			NodeID:  "live-bg",
			Command: cli,
			Model:   model,
			WorkDir: t.TempDir(),
			UserPrompt: "Step 1: call the Agent tool ONCE with subagent_type 'general-purpose', run_in_background true, " +
				"description 'fetcher', prompt: 'Run the bash command `sleep 5 && echo " + nonce + "` and reply with its exact output only.'. " +
				"Step 2: right after launching it, end your turn by replying only the word WAITING — do not wait for it and do not call TaskOutput. " +
				"Step 3: when you are later notified that the agent finished, report its exact output as `answer`.",
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		}
		task.Hooks.OnBackgroundWork = func(w delegate.BackgroundWork) {
			mu.Lock()
			defer mu.Unlock()
			phases = append(phases, w.Phase)
		}
		b := &delegate.ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelInfo, io.Discard)}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		provMu.Lock()
		calls++
		provMu.Unlock()
		res, err := b.Execute(ctx, task)
		if err != nil {
			// A failed call proves nothing either way — least of all the
			// witness, which an error would otherwise turn green.
			t.Fatalf("Execute (%s): %v", lifecycle, err)
		}
		provMu.Lock()
		if res.EffectiveModel != "" {
			served = append(served, res.EffectiveModel)
		}
		cost += res.CostUSD
		provMu.Unlock()
		a, _ := res.Output["answer"].(string)
		t.Logf("lifecycle=%s answer=%q phases=%v cost=%v terminated=%v",
			lifecycle, a, phases, res.Output["_cost_usd"], res.TerminatedBackgroundTasks)
		if !strings.Contains(a, nonce) {
			return "", phases
		}
		return a, phases
	}

	t.Run("lifecycle on: the final report carries the background agent's result", func(t *testing.T) {
		answer, phases := run(t, "on")
		if answer == "" {
			t.Fatalf("the final answer does not carry the background agent's output (phases %v)", phases)
		}
	})
	t.Run("lifecycle off: the witness — the result dies with the session", func(t *testing.T) {
		if answer, _ := run(t, "off"); answer != "" {
			t.Fatalf("with the lifecycle off the answer still carried the agent's output (%q): the model waited in-turn, and the first half proves nothing", answer)
		}
	})

	// Two sessions ran: a model the backend did not name is said as such
	// (unobserved:<asked>), never left blank — blank is the column's word
	// for a run with no LLM call at all.
	provMu.Lock()
	defer provMu.Unlock()
	recordLiveProvenance(tracker, liveOverride(t), liveledger.ServedModelFor(served, calls, model), served)
	tracker.SetCost(cost)
}
